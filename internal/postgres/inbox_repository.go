package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/junglegaming/backend-challenge-go/internal/application"
)

type inboxRepository struct{ tx pgx.Tx }

func (r *inboxRepository) Claim(ctx context.Context, record application.InboxRecord) (application.InboxRecord, bool, error) {
	inserted, err := r.tx.Exec(ctx, `INSERT INTO inbox_messages(consumer_name,message_id,provider_id,payload_hash,received_at)
  VALUES ($1,$2,$3,$4,$5) ON CONFLICT (consumer_name,message_id) DO NOTHING`,
		record.ConsumerName, record.MessageID, record.ProviderID, record.PayloadHash, record.ReceivedAt)
	if err != nil {
		return application.InboxRecord{}, false, databaseError(err)
	}
	if inserted.RowsAffected() == 1 {
		return record, true, nil
	}
	var existing application.InboxRecord
	var completed pgtype.Timestamptz
	err = r.tx.QueryRow(ctx, `SELECT consumer_name,message_id,provider_id,payload_hash,received_at,
  COALESCE(transaction_id,''),completed_at FROM inbox_messages WHERE consumer_name=$1 AND message_id=$2 FOR UPDATE`,
		record.ConsumerName, record.MessageID).Scan(&existing.ConsumerName, &existing.MessageID, &existing.ProviderID,
		&existing.PayloadHash, &existing.ReceivedAt, &existing.TransactionID, &completed)
	if completed.Valid {
		existing.CompletedAt = completed.Time
	}
	return existing, false, databaseError(err)
}
func (r *inboxRepository) Complete(ctx context.Context, consumerName, messageID, transactionID string, now time.Time) error {
	result, err := r.tx.Exec(ctx, `UPDATE inbox_messages SET transaction_id=$3,completed_at=$4
  WHERE consumer_name=$1 AND message_id=$2 AND completed_at IS NULL`, consumerName, messageID, transactionID, now)
	if err != nil {
		return databaseError(err)
	}
	if result.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return nil
}
