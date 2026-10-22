package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/junglegaming/backend-challenge-go/internal/workers"
)

// Cada claim é uma instrução SQL atômica curta. Nenhum lock SQL é mantido
// durante a chamada ao broker ou durante a retomada do caso de uso.
type WorkerStore struct{ pool *pgxpool.Pool }

func NewWorkerStore(pool *pgxpool.Pool) *WorkerStore { return &WorkerStore{pool: pool} }
func (s *WorkerStore) ClaimOutbox(ctx context.Context, maxAttempts int) (workers.OutboxJob, bool, error) {
	token := rand.Text()
	var job workers.OutboxJob
	err := s.pool.QueryRow(ctx, `WITH candidate AS (
  SELECT event_id FROM outbox_events
  WHERE published_at IS NULL AND dead_lettered_at IS NULL
    AND next_attempt_at <= clock_timestamp() AND (lease_until IS NULL OR lease_until <= clock_timestamp())
  ORDER BY next_attempt_at,event_id LIMIT 1 FOR UPDATE SKIP LOCKED
 ) UPDATE outbox_events AS e SET attempts=e.attempts+1, lease_token=$1,
   lease_until=clock_timestamp()+INTERVAL '60 seconds',route_to_dlq=e.route_to_dlq OR e.attempts >= $2
 FROM candidate WHERE e.event_id=candidate.event_id
 RETURNING e.event_id,e.aggregate_id,e.payload,e.lease_token,e.attempts,e.route_to_dlq`, token, maxAttempts).
		Scan(&job.EventID, &job.AggregateID, &job.Payload, &job.Token, &job.Attempts, &job.ToDLQ)
	if errors.Is(err, pgx.ErrNoRows) {
		return workers.OutboxJob{}, false, nil
	}
	return job, err == nil, err
}
func (s *WorkerStore) OutboxDelivered(ctx context.Context, job workers.OutboxJob) error {
	result, err := s.pool.Exec(ctx, `UPDATE outbox_events SET
  published_at=CASE WHEN $3 THEN NULL ELSE clock_timestamp() END,
  dead_lettered_at=CASE WHEN $3 THEN clock_timestamp() ELSE NULL END,
  lease_token=NULL,lease_until=NULL WHERE event_id=$1 AND lease_token=$2`, job.EventID, job.Token, job.ToDLQ)
	return workerUpdate(result, err)
}
func (s *WorkerStore) RetryOutbox(ctx context.Context, job workers.OutboxJob, delay time.Duration, toDLQ bool) error {
	result, err := s.pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at=clock_timestamp()+$3::bigint*INTERVAL '1 millisecond',
  route_to_dlq=route_to_dlq OR $4,lease_token=NULL,lease_until=NULL WHERE event_id=$1 AND lease_token=$2`,
		job.EventID, job.Token, delay.Milliseconds(), toDLQ)
	return workerUpdate(result, err)
}
func (s *WorkerStore) ClaimReference(ctx context.Context) (workers.ReferenceJob, bool, error) {
	token := rand.Text()
	var job workers.ReferenceJob
	err := s.pool.QueryRow(ctx, `WITH candidate AS (
  SELECT transaction_id FROM reference_jobs WHERE completed_at IS NULL
    AND next_attempt_at <= clock_timestamp() AND (lease_until IS NULL OR lease_until <= clock_timestamp())
  ORDER BY next_attempt_at,transaction_id LIMIT 1 FOR UPDATE SKIP LOCKED
 ) UPDATE reference_jobs AS j SET attempts=j.attempts+1,lease_token=$1,lease_until=clock_timestamp()+INTERVAL '60 seconds'
 FROM candidate WHERE j.transaction_id=candidate.transaction_id
 RETURNING j.transaction_id,j.lease_token,j.attempts`, token).Scan(&job.TransactionID, &job.Token, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return workers.ReferenceJob{}, false, nil
	}
	return job, err == nil, err
}
func (s *WorkerStore) ReferenceCompleted(ctx context.Context, job workers.ReferenceJob) error {
	result, err := s.pool.Exec(ctx, `UPDATE reference_jobs SET completed_at=clock_timestamp(),lease_token=NULL,lease_until=NULL
  WHERE transaction_id=$1 AND lease_token=$2
 AND EXISTS(SELECT 1 FROM wager_transactions WHERE id=$1 AND status IN ('PROCESSED','REJECTED','FAILED'))`, job.TransactionID, job.Token)
	return workerUpdate(result, err)
}
func (s *WorkerStore) RetryReference(ctx context.Context, job workers.ReferenceJob, delay time.Duration) error {
	result, err := s.pool.Exec(ctx, `UPDATE reference_jobs SET next_attempt_at=clock_timestamp()+$3::bigint*INTERVAL '1 millisecond',
  lease_token=NULL,lease_until=NULL WHERE transaction_id=$1 AND lease_token=$2`, job.TransactionID, job.Token, delay.Milliseconds())
	return workerUpdate(result, err)
}
func workerUpdate(result pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return workers.ErrLeaseLost
	}
	return nil
}
