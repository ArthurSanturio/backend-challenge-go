//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	neturl "net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
	storage "github.com/junglegaming/backend-challenge-go/internal/postgres"
)

type databaseFixture struct {
	pool    *pgxpool.Pool
	url     string
	now     time.Time
	wallets *application.WalletService
	wagers  *application.WageringService
	actor   application.Actor
	manager *storage.Transactor
}

// Cada teste cria seu próprio banco e o remove no cleanup. Nunca altera o
// schema nem os dados do banco informado em POSTGRES_TEST_URL.
func newDatabase(t *testing.T) *databaseFixture {
	t.Helper()
	url := os.Getenv("POSTGRES_TEST_URL")
	if url == "" {
		t.Fatal("integration tests require POSTGRES_TEST_URL with permission to create temporary databases")
	}
	parsedURL, err := neturl.Parse(url)
	if err != nil || (parsedURL.Scheme != "postgres" && parsedURL.Scheme != "postgresql") {
		t.Fatal("POSTGRES_TEST_URL must be a postgres:// or postgresql:// URL")
	}
	ctx := t.Context()
	admin, err := storage.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("wallet_test_%x", suffix)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, `DROP DATABASE `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Errorf("remove temporary database: %v", err)
		}
	})
	parsedURL.Path, parsedURL.RawPath = "/"+name, ""
	f := &databaseFixture{
		url: parsedURL.String(),
		now: time.Date(2026, time.October, 20, 12, 0, 0, 987654321, time.UTC),
	}
	f.pool, err = storage.Open(ctx, f.url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.pool.Close() })
	var connectedDatabase string
	if err := f.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&connectedDatabase); err != nil || connectedDatabase != name {
		t.Fatal("refusing to migrate a database other than this test's temporary database")
	}
	applyMigration(t, f.pool, "up")
	f.connectServices(t, nil)
	f.actor, err = application.NewProviderActor("provider-a")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func applyMigration(t *testing.T, pool *pgxpool.Pool, direction string) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000001_initial."+direction+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), string(payload)); err != nil {
		t.Fatalf("migration %s: %v", direction, err)
	}
}

func (f *databaseFixture) connectServices(t *testing.T, override application.Transactor) {
	t.Helper()
	var err error
	f.manager, err = storage.NewTransactor(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	var manager application.Transactor = f.manager
	if override != nil {
		manager = override
	}
	dependencies := application.Dependencies{Transactions: manager, Now: func() time.Time { return f.now }}
	f.wallets, err = application.NewWalletService(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	f.wagers, err = application.NewWageringService(dependencies)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *databaseFixture) reopen(t *testing.T) {
	t.Helper()
	f.pool.Close()
	var err error
	f.pool, err = storage.Open(t.Context(), f.url)
	if err != nil {
		t.Fatal(err)
	}
	f.connectServices(t, nil)
}

func money(t *testing.T, units int64) domain.Money {
	t.Helper()
	value, err := domain.NewMoneyFromMinorUnits(units, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (f *databaseFixture) create(t *testing.T, initial int64) domain.WalletState {
	t.Helper()
	state, err := f.wallets.Create(t.Context(), application.InternalActor(), application.CreateWalletInput{
		PlayerID: "player-1", InitialBalance: money(t, initial),
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func request(t *testing.T, wallet domain.WalletState, kind domain.TransactionKind, units int64, externalID string) application.ProcessWagerInput {
	t.Helper()
	return application.ProcessWagerInput{
		ProviderID: "provider-a", ExternalTransactionID: externalID, IdempotencyKey: "key:" + externalID,
		WalletID: wallet.ID, PlayerID: wallet.PlayerID, RoundID: "round-1", GameID: "game-1",
		Kind: kind, Money: money(t, units),
	}
}

func (f *databaseFixture) process(t *testing.T, input application.ProcessWagerInput) application.ProcessWagerResult {
	t.Helper()
	result, err := f.wagers.Process(t.Context(), f.actor, input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *databaseFixture) counts(t *testing.T) [4]int64 {
	t.Helper()
	var counts [4]int64
	err := f.pool.QueryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM wallets),
		(SELECT COUNT(*) FROM wager_transactions),(SELECT COUNT(*) FROM wallet_ledger_entries),
		(SELECT COUNT(*) FROM outbox_events)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3])
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestPostgresOpeningAndPersistentReplay(t *testing.T) {
	f := newDatabase(t)
	wallet := f.create(t, 10000)
	if wallet.CreatedAt != f.now.Truncate(time.Microsecond) || f.counts(t) != [4]int64{1, 1, 1, 2} {
		t.Fatalf("opening or timestamps do not match: %+v, %v", wallet, f.counts(t))
	}
	loaded, err := f.wallets.Get(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || loaded != wallet {
		t.Fatalf("rehydrated wallet differs: %+v, %v", loaded, err)
	}
	input := request(t, wallet, domain.KindBet, 2500, "bet-1")
	first := f.process(t, input)
	f.process(t, request(t, wallet, domain.KindWin, 5000, "win-1"))
	before := f.counts(t)
	f.reopen(t)
	replay := f.process(t, input)
	if !replay.IdempotentReplay || replay.Transaction != first.Transaction || f.counts(t) != before ||
		replay.Transaction.ResultBalance != money(t, 7500) {
		t.Fatalf("persistent replay changed result: %+v", replay)
	}
	input.Money = money(t, 2600)
	if _, err := f.wagers.Process(t.Context(), f.actor, input); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("changed replay was accepted: %v", err)
	}
	result, err := f.wallets.Reconcile(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || !result.Consistent || result.StoredBalance != money(t, 12500) || result.CheckedEntries != 3 {
		t.Fatalf("unexpected reconciliation: %+v, %v", result, err)
	}
}

func TestPostgresZeroLossAndAuditRejection(t *testing.T) {
	f := newDatabase(t)
	wallet := f.create(t, 0)
	if f.counts(t) != [4]int64{1, 0, 0, 0} {
		t.Fatal("zero opening generated financial records")
	}
	loss := f.process(t, request(t, wallet, domain.KindLoss, 0, "loss-1"))
	bet := f.process(t, request(t, wallet, domain.KindBet, 100, "bet-1"))
	if loss.Transaction.Status != domain.StatusProcessed || bet.Transaction.FailureCode != domain.FailureInsufficientFunds ||
		f.counts(t) != [4]int64{1, 2, 0, 2} {
		t.Fatalf("unexpected LOSS or rejection: %+v, %+v", loss, bet)
	}
	input := request(t, wallet, domain.KindLoss, 0, "wrong-currency")
	input.Money, _ = domain.ZeroMoney("USD")
	wrongCurrency := f.process(t, input)
	if wrongCurrency.Transaction.FailureCode != domain.FailureInvalidOperation {
		t.Fatal("currency mismatch was not audited")
	}
	loaded, err := f.wallets.Get(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || loaded != wallet {
		t.Fatalf("LOSS or rejection changed wallet: %+v, %v", loaded, err)
	}
}

func TestPostgresPendingReferenceSurvivesReconnection(t *testing.T) {
	f := newDatabase(t)
	wallet := f.create(t, 10000)
	refund := request(t, wallet, domain.KindRefund, 2500, "refund-1")
	refund.ReferenceExternalTransactionID = "bet-later"
	pending := f.process(t, refund)
	if pending.Transaction.Status != domain.StatusPendingReference {
		t.Fatalf("unexpected pending state: %+v", pending)
	}
	f.reopen(t)
	original := f.process(t, request(t, wallet, domain.KindBet, 2500, "bet-later"))
	resolved, err := f.wagers.Resume(t.Context(), application.InternalActor(), pending.Transaction.ID, application.RequestMetadata{})
	if err != nil || resolved.Transaction.Status != domain.StatusProcessed || resolved.Transaction.ResultBalance != money(t, 10000) {
		t.Fatalf("pending reference was not recovered: %+v, %v", resolved, err)
	}
	rollback := request(t, wallet, domain.KindRollback, 2500, "another-return")
	rollback.ReferenceExternalTransactionID = "bet-later"
	if got := f.process(t, rollback); got.Transaction.FailureCode != domain.FailureAlreadyReversed {
		t.Fatalf("refund plus rollback duplicated the debit return: %+v", got)
	}
	// Exercita o índice no banco, ignorando deliberadamente a consulta preventiva.
	err = f.manager.WithinTransaction(t.Context(), func(repositories application.Repositories) error {
		loadedWallet, err := repositories.Wallets.Get(t.Context(), wallet.ID)
		if err != nil {
			return err
		}
		reference, err := repositories.Wagers.Get(t.Context(), original.Transaction.ID)
		if err != nil {
			return err
		}
		duplicate, err := domain.NewWagerTransaction(domain.WagerTransactionInput{
			ID: "duplicate-refund", ExternalTransactionID: "duplicate-refund", ProviderID: "provider-a",
			IdempotencyKey: "duplicate-key", PayloadHash: strings.Repeat("a", 64), WalletID: wallet.ID,
			PlayerID: wallet.PlayerID, RoundID: "round-1", GameID: "game-1", Kind: domain.KindRefund,
			Money: money(t, 2500), ReferenceExternalTransactionID: "bet-later",
		}, f.now.Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		if err := duplicate.Process(loadedWallet, reference, f.now.Truncate(time.Microsecond)); err != nil {
			return err
		}
		return repositories.Wagers.Insert(t.Context(), duplicate)
	})
	if !errors.Is(err, application.ErrConflict) {
		t.Fatalf("database accepted duplicate successful reversal: %v", err)
	}
	result, err := f.wallets.Reconcile(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || !result.Consistent || result.StoredBalance != money(t, 10000) {
		t.Fatalf("reversal ledger is inconsistent: %+v, %v", result, err)
	}
}

func TestPostgresConstraintsRejectDirectFinancialCorruption(t *testing.T) {
	f := newDatabase(t)
	wallet := f.create(t, 10000)
	bet := f.process(t, request(t, wallet, domain.KindBet, 2500, "bet-1"))
	before := f.counts(t)
	tests := []struct {
		name string
		sql  string
		args []any
		code string
	}{
		{"negative balance", `UPDATE wallets SET balance_minor=-1,version=version+1 WHERE id=$1`, []any{wallet.ID}, "23514"},
		{"unlogged balance", `UPDATE wallets SET balance_minor=balance_minor+1,version=version+1 WHERE id=$1`, []any{wallet.ID}, "23514"},
		{"version without movement", `UPDATE wallets SET version=version+1 WHERE id=$1`, []any{wallet.ID}, "23514"},
		{"wallet identity", `UPDATE wallets SET player_id='another-player' WHERE id=$1`, []any{wallet.ID}, "23514"},
		{"ledger update", `UPDATE wallet_ledger_entries SET amount_minor=amount_minor+1`, nil, "23514"},
		{"ledger delete", `DELETE FROM wallet_ledger_entries`, nil, "23514"},
		{"ledger truncate", `TRUNCATE wallet_ledger_entries`, nil, "23514"},
		{"terminal update", `UPDATE wager_transactions SET updated_at=updated_at WHERE id=$1`, []any{bet.Transaction.ID}, "23514"},
		{"transaction delete", `DELETE FROM wager_transactions WHERE id=$1`, []any{bet.Transaction.ID}, "23514"},
		{"event mutation", `UPDATE outbox_events SET payload=payload||' '`, nil, "23514"},
		{"numeric JSON balance", `INSERT INTO outbox_events(event_id,aggregate_id,event_type,version,occurred_at,transaction_id,payload)
			SELECT 'numeric-event',aggregate_id,event_type,version,occurred_at,transaction_id,
			jsonb_set(jsonb_set(payload::jsonb,'{eventId}','"numeric-event"'),'{data,balance,amount}','75.00')::text
			FROM outbox_events WHERE transaction_id=$1 AND event_type='WagerTransactionProcessed'`, []any{bet.Transaction.ID}, "23514"},
		{"event delete", `DELETE FROM outbox_events`, nil, "23514"},
		{"event truncate", `TRUNCATE outbox_events`, nil, "23514"},
		{"duplicate player and currency", `INSERT INTO wallets VALUES ('duplicate','player-1','BRL',0,1,NOW(),NOW())`, nil, "23505"},
		{"opening without its records", `INSERT INTO wallets VALUES ('unlogged-opening','another-player','BRL',100,1,NOW(),NOW())`, nil, "23514"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			_, err = tx.Exec(t.Context(), tt.sql, tt.args...)
			if err == nil {
				err = tx.Commit(t.Context())
			} else {
				_ = tx.Rollback(t.Context())
			}
			var databaseErr *pgconn.PgError
			if !errors.As(err, &databaseErr) || databaseErr.Code != tt.code {
				t.Fatalf("expected SQLSTATE %s, got %v", tt.code, err)
			}
			if f.counts(t) != before {
				t.Fatal("rejected SQL altered audit records")
			}
		})
	}
	loaded, err := f.wallets.Get(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || loaded.Balance != money(t, 7500) || loaded.Version != 2 {
		t.Fatalf("failed writes changed wallet: %+v, %v", loaded, err)
	}
}

type faultTransactor struct {
	base application.Transactor
	mode string
}

var errInjected = errors.New("injected write failure")

func (f faultTransactor) WithinTransaction(ctx context.Context, action func(application.Repositories) error) error {
	return f.base.WithinTransaction(ctx, func(repositories application.Repositories) error {
		switch f.mode {
		case "outbox error", "outbox omitted":
			repositories.Outbox = faultOutbox{OutboxRepository: repositories.Outbox, omit: f.mode == "outbox omitted"}
		case "ledger error":
			repositories.Ledger = faultLedger{LedgerRepository: repositories.Ledger}
		}
		return action(repositories)
	})
}

type faultOutbox struct {
	application.OutboxRepository
	omit bool
}

func (f faultOutbox) Insert(context.Context, application.OutboxRecord) error {
	if f.omit {
		return nil // O commit deve falhar mesmo quando o callback não acusa erro.
	}
	return errInjected
}

type faultLedger struct{ application.LedgerRepository }

func (faultLedger) Insert(context.Context, domain.WalletLedgerEntry) error { return errInjected }

func TestPostgresAtomicRollbackAndCommitValidation(t *testing.T) {
	f := newDatabase(t)
	wallet := f.create(t, 10000)
	for _, mode := range []string{"outbox error", "ledger error", "outbox omitted"} {
		t.Run(mode, func(t *testing.T) {
			before := f.counts(t)
			f.connectServices(t, faultTransactor{base: f.manager, mode: mode})
			result, err := f.wagers.Process(t.Context(), f.actor, request(t, wallet, domain.KindBet, 2500, mode))
			if err == nil || result != (application.ProcessWagerResult{}) {
				t.Fatalf("partial result returned: %+v, %v", result, err)
			}
			if mode == "outbox omitted" {
				var databaseErr *pgconn.PgError
				if !errors.As(err, &databaseErr) || databaseErr.Code != "23514" {
					t.Fatalf("missing outbox was not rejected at commit: %v", err)
				}
			} else if !errors.Is(err, errInjected) {
				t.Fatal(err)
			}
			if f.counts(t) != before {
				t.Fatal("rollback left partial rows")
			}
			loaded, err := f.wallets.Get(t.Context(), application.InternalActor(), wallet.ID)
			if err != nil || loaded != wallet {
				t.Fatalf("rollback changed wallet: %+v, %v", loaded, err)
			}
			f.connectServices(t, nil)
		})
	}
}

func TestPostgresLedgerPaginationAndLargeExactTotals(t *testing.T) {
	f := newDatabase(t)
	wallet := f.create(t, 0)
	for i := range 2 {
		f.process(t, request(t, wallet, domain.KindWin, math.MaxInt64, fmt.Sprintf("win-%d", i)))
		f.process(t, request(t, wallet, domain.KindBet, math.MaxInt64, fmt.Sprintf("bet-%d", i)))
	}
	result, err := f.wallets.Reconcile(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || !result.Consistent || result.CheckedEntries != 4 || result.CalculatedBalance != money(t, 0) {
		t.Fatalf("NUMERIC aggregate lost precision: %+v, %v", result, err)
	}
	seen := make(map[string]bool)
	cursor := ""
	for {
		page, err := f.wallets.ListLedger(t.Context(), application.InternalActor(), wallet.ID, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.Entries {
			if seen[entry.ID] {
				t.Fatal("cursor repeated a ledger entry")
			}
			seen[entry.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 4 {
		t.Fatalf("pagination returned %d entries, want 4", len(seen))
	}
}

func TestPostgresRollbackOnContextCancellation(t *testing.T) {
	f := newDatabase(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := f.manager.WithinTransaction(ctx, func(repositories application.Repositories) error {
		wallet, err := domain.NewWallet("cancel-wallet", "cancel-player", money(t, 0), f.now.Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		if err := repositories.Wallets.Insert(ctx, wallet); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || f.counts(t) != [4]int64{} {
		t.Fatalf("canceled transaction was confirmed: %v, %v", err, f.counts(t))
	}
}

func TestPostgresMigrationsCanBeRevertedAndReapplied(t *testing.T) {
	f := newDatabase(t)
	f.create(t, 10000)
	applyMigration(t, f.pool, "down")
	var exists bool
	if err := f.pool.QueryRow(t.Context(), `SELECT to_regclass('wallets') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("migration down left its schema: %v, %v", exists, err)
	}
	applyMigration(t, f.pool, "up")
	f.pool.Reset()
	before := f.create(t, 0)
	loaded, err := f.wallets.Get(t.Context(), application.InternalActor(), before.ID)
	if err != nil || !reflect.DeepEqual(before, loaded) {
		t.Fatalf("reapplied schema is unusable: %+v, %v", loaded, err)
	}
}
