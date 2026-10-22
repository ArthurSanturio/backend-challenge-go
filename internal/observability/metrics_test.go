package observability

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

func TestMetricsConcurrentCountersAndDurableSnapshot(t *testing.T) {
	metrics := New(func(context.Context) (Snapshot, error) {
		return Snapshot{OutboxPending: 2, OutboxLag: 3, OutboxDLQ: 4, ReferencesPending: 5}, nil
	})
	var group sync.WaitGroup
	for i := 0; i < 50; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			metrics.WagerResult(domain.StatusProcessed, true)
			metrics.Conflict()
			metrics.Event("outbox", "retry")
			metrics.Duration("outbox", time.Millisecond)
		}()
	}
	group.Wait()
	metrics.Reconciliation(false)
	metrics.QueueDepth("input", 2)
	metrics.HTTP("UNBOUNDED-USER-METHOD", "", 404, time.Millisecond)
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 {
		t.Fatal(recorder.Body.String())
	}
	for _, expected := range []string{
		`wallet_wager_results_total{replay="true",status="PROCESSED"} 50`,
		`wallet_conflicts_total 50`, `wallet_background_operations_total{component="outbox",outcome="retry"} 50`,
		`wallet_reconciliations_total{consistent="false"} 1`, `wallet_storage_metrics_up 1`,
		`wallet_outbox_pending 2`, `wallet_outbox_oldest_age_seconds 3`, `wallet_outbox_dead_lettered 4`, `wallet_reference_jobs_pending 5`,
		`wallet_sqs_dlq_depth{queue="input"} 2`, `method="OTHER"`,
	} {
		if !strings.Contains(recorder.Body.String(), expected) {
			t.Errorf("missing metric %s", expected)
		}
	}
	if strings.Contains(recorder.Body.String(), "UNBOUNDED-USER-METHOD") {
		t.Fatal("unbounded method label exposed")
	}
}
