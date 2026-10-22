package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/junglegaming/backend-challenge-go/internal/application"
)

type outboxRepository struct{ tx pgx.Tx }

var _ application.OutboxRepository = (*outboxRepository)(nil)

func (r *outboxRepository) Insert(ctx context.Context, record application.OutboxRecord) error {
	_, err := r.tx.Exec(ctx, `INSERT INTO outbox_events
		(event_id,aggregate_id,event_type,version,occurred_at,transaction_id,payload)
		VALUES ($1,$2,$3,$4,$5,($6::text::jsonb #>> '{data,transactionId}'),$6::text)`,
		record.EventID, record.AggregateID, record.EventType, record.Version,
		record.OccurredAt.UTC().Truncate(time.Microsecond), record.Payload)
	return databaseError(err)
}
