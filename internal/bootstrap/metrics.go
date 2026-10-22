package bootstrap

import (
	"context"
	"errors"

	"github.com/junglegaming/backend-challenge-go/internal/observability"
)

func newMetrics(database *database) *observability.Metrics {
	return observability.New(func(ctx context.Context) (observability.Snapshot, error) {
		var value observability.Snapshot
		if database.pool == nil {
			return value, errors.New("database is not started")
		}
		err := database.pool.QueryRow(ctx, `SELECT
   (SELECT COUNT(*)::float8 FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL),
   (SELECT COALESCE(GREATEST(EXTRACT(EPOCH FROM clock_timestamp()-MIN(occurred_at)),0),0)::float8 FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL),
   (SELECT COUNT(*)::float8 FROM outbox_events WHERE dead_lettered_at IS NOT NULL),
   (SELECT COUNT(*)::float8 FROM reference_jobs WHERE completed_at IS NULL)`).Scan(&value.OutboxPending, &value.OutboxLag, &value.OutboxDLQ, &value.ReferencesPending)
		return value, err
	})
}
