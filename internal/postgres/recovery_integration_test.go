//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
	storage "github.com/junglegaming/backend-challenge-go/internal/postgres"
	"github.com/junglegaming/backend-challenge-go/internal/workers"
)

func extraMigration(t *testing.T, f *databaseFixture, name string) {
	t.Helper()
	body, err := os.ReadFile("../../migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(t.Context(), string(body)); err != nil {
		t.Fatal(err)
	}
}
func recoveryDatabase(t *testing.T) *databaseFixture {
	f := newDatabase(t)
	extraMigration(t, f, "000002_inbox.up.sql")
	extraMigration(t, f, "000003_workers.up.sql")
	return f
}

func TestPostgresInboxAtomicityPersistentReplayAndHash(t *testing.T) {
	f := recoveryDatabase(t)
	wallet := f.create(t, 10000)
	input := request(t, wallet, domain.KindBet, 2500, "inbox-bet")
	message := application.IncomingMessage{ConsumerName: "test-consumer", MessageID: "envelope-1", OccurredAt: f.now}
	before := f.counts(t)
	f.connectServices(t, faultTransactor{base: f.manager, mode: "outbox error"})
	if _, err := f.wagers.ProcessMessage(t.Context(), f.actor, message, input); !errors.Is(err, errInjected) {
		t.Fatalf("expected rollback: %v", err)
	}
	var count int64
	if err := f.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM inbox_messages").Scan(&count); err != nil || count != 0 || f.counts(t) != before {
		t.Fatal("inbox or financial writes escaped rollback")
	}
	f.connectServices(t, nil)
	first, err := f.wagers.ProcessMessage(t.Context(), f.actor, message, input)
	if err != nil {
		t.Fatal(err)
	}
	before = f.counts(t)
	f.reopen(t)
	replay, err := f.wagers.ProcessMessage(t.Context(), f.actor, message, input)
	if err != nil || !replay.IdempotentReplay || replay.Transaction != first.Transaction || f.counts(t) != before {
		t.Fatalf("durable inbox replay differs: %+v %v", replay, err)
	}
	input.Money = money(t, 2600)
	if _, err := f.wagers.ProcessMessage(t.Context(), f.actor, message, input); !errors.Is(err, application.ErrInboxConflict) {
		t.Fatalf("changed envelope accepted: %v", err)
	}
	if f.counts(t) != before {
		t.Fatal("changed envelope altered financial records")
	}
	err = f.manager.WithinTransaction(t.Context(), func(repositories application.Repositories) error {
		_, _, err := repositories.Inbox.Claim(t.Context(), application.InboxRecord{ConsumerName: "test-consumer", MessageID: "incomplete", ProviderID: "provider-a", PayloadHash: first.Transaction.PayloadHash, ReceivedAt: f.now})
		return err
	})
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "23514" {
		t.Fatalf("incomplete inbox committed: %v", err)
	}
	if err := f.pool.QueryRow(t.Context(), "SELECT COUNT(*) FROM inbox_messages").Scan(&count); err != nil || count != 1 {
		t.Fatal("incomplete receipt was persisted")
	}
}
func TestPostgresWorkerLeasesFencePreviousOwners(t *testing.T) {
	f := recoveryDatabase(t)
	wallet := f.create(t, 10000)
	otherPool, err := storage.Open(t.Context(), f.url)
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	firstStore, secondStore := storage.NewWorkerStore(f.pool), storage.NewWorkerStore(otherPool)
	first, found, err := firstStore.ClaimOutbox(t.Context(), 8)
	if err != nil || !found {
		t.Fatal("outbox not claimed", err)
	}
	second, found, err := secondStore.ClaimOutbox(t.Context(), 8)
	if err != nil || !found || first.EventID == second.EventID {
		t.Fatal("competing workers reserved same event", err)
	}
	_, err = f.pool.Exec(t.Context(), `UPDATE outbox_events SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE event_id=$1`, first.EventID)
	if err != nil {
		t.Fatal(err)
	}
	replacement, found, err := secondStore.ClaimOutbox(t.Context(), 8)
	if err != nil || !found || replacement.EventID != first.EventID || replacement.Token == first.Token {
		t.Fatal("expired claim not recovered", err)
	}
	if err := firstStore.OutboxDelivered(t.Context(), first); !errors.Is(err, workers.ErrLeaseLost) {
		t.Fatalf("stale publisher acknowledged new lease: %v", err)
	}
	if replacement.Payload != first.Payload {
		t.Fatal("retry changed event snapshot")
	}
	if err := secondStore.OutboxDelivered(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := secondStore.OutboxDelivered(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	refund := request(t, wallet, domain.KindRefund, 1000, "missing-refund")
	refund.ReferenceExternalTransactionID = "not-arrived"
	pending := f.process(t, refund)
	job, found, err := firstStore.ClaimReference(t.Context())
	if err != nil || !found || job.TransactionID != pending.Transaction.ID {
		t.Fatal("pending operation not scheduled", err)
	}
	if _, found, err := secondStore.ClaimReference(t.Context()); err != nil || found {
		t.Fatal("live reference lease was stolen", err)
	}
	_, err = f.pool.Exec(t.Context(), `UPDATE reference_jobs SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE transaction_id=$1`, job.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	next, found, err := secondStore.ClaimReference(t.Context())
	if err != nil || !found || next.Token == job.Token {
		t.Fatal("reference lease not recovered", err)
	}
	if err := firstStore.ReferenceCompleted(t.Context(), job); !errors.Is(err, workers.ErrLeaseLost) {
		t.Fatalf("old owner completed new job lease: %v", err)
	}
	result, err := f.wagers.ResumeFinalReferenceAttempt(t.Context(), application.InternalActor(), job.TransactionID, application.RequestMetadata{})
	if err != nil || result.Transaction.FailureCode != domain.FailureReferenceNotFound {
		t.Fatal("reference budget not rejected", err)
	}
	if err := secondStore.ReferenceCompleted(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	reconciliation, err := f.wallets.Reconcile(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || !reconciliation.Consistent || reconciliation.StoredBalance != money(t, 10000) {
		t.Fatal("lease recovery changed money", err)
	}
}
func TestPostgresAllMigrationsCanBeRevertedAndReapplied(t *testing.T) {
	f := recoveryDatabase(t)
	f.create(t, 10000)
	extraMigration(t, f, "000003_workers.down.sql")
	extraMigration(t, f, "000002_inbox.down.sql")
	applyMigration(t, f.pool, "down")
	applyMigration(t, f.pool, "up")
	extraMigration(t, f, "000002_inbox.up.sql")
	extraMigration(t, f, "000003_workers.up.sql")
	f.pool.Reset()
	wallet := f.create(t, 0)
	input := request(t, wallet, domain.KindWin, 100, "after-migration")
	_, err := f.wagers.ProcessMessage(t.Context(), f.actor, application.IncomingMessage{ConsumerName: "migration", MessageID: "message", OccurredAt: time.Now()}, input)
	if err != nil {
		t.Fatal("reapplied schema does not support inbox and outbox", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, found, err := storage.NewWorkerStore(f.pool).ClaimOutbox(ctx, 8); err != nil || !found {
		t.Fatal("reapplied outbox cannot be claimed", err)
	}
}
