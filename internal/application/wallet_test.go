package application_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type fixture struct {
	store   *memoryStore
	wallets *application.WalletService
	wagers  *application.WageringService
	now     time.Time
	logs    bytes.Buffer
	actor   application.Actor
	ids     int
	idErr   error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{store: newMemoryStore(), now: time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)}
	dependencies := application.Dependencies{
		Transactions: f.store,
		Now:          func() time.Time { return f.now },
		NewID: func() (string, error) {
			if f.idErr != nil {
				return "", f.idErr
			}
			f.ids++
			return fmt.Sprintf("id-%04d", f.ids), nil
		},
		Logger: slog.New(slog.NewJSONHandler(&f.logs, nil)),
	}
	var err error
	f.wallets, err = application.NewWalletService(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	f.wagers, err = application.NewWageringService(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	f.actor, err = application.NewProviderActor("provider-a")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func units(t *testing.T, amount int64) domain.Money {
	t.Helper()
	money, err := domain.NewMoneyFromMinorUnits(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return money
}

func (f *fixture) createWallet(t *testing.T, initialUnits int64) domain.WalletState {
	t.Helper()
	wallet, err := f.wallets.Create(t.Context(), application.InternalActor(), application.CreateWalletInput{
		PlayerID: "player-1", InitialBalance: units(t, initialUnits),
	})
	if err != nil {
		t.Fatal(err)
	}
	return wallet
}

func wagerInput(t *testing.T, wallet domain.WalletState, kind domain.TransactionKind, amount int64, externalID string) application.ProcessWagerInput {
	t.Helper()
	return application.ProcessWagerInput{
		ProviderID: "provider-a", ExternalTransactionID: externalID, IdempotencyKey: "custom-key:" + externalID,
		WalletID: wallet.ID, PlayerID: wallet.PlayerID, RoundID: "round-1", GameID: "game-1",
		Kind: kind, Money: units(t, amount),
	}
}

func (f *fixture) process(t *testing.T, input application.ProcessWagerInput) application.ProcessWagerResult {
	t.Helper()
	result, err := f.wagers.Process(t.Context(), f.actor, input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCreateWalletOpeningAndZero(t *testing.T) {
	for _, initial := range []int64{0, 10000} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			f := newFixture(t)
			wallet := f.createWallet(t, initial)
			if wallet.Balance != units(t, initial) || wallet.Version != 1 {
				t.Fatalf("unexpected wallet: %+v", wallet)
			}
			if initial == 0 {
				if len(f.store.data.wagers) != 0 || len(f.store.data.ledger) != 0 || len(f.store.data.outbox) != 0 {
					t.Fatal("zero opening created financial records")
				}
			} else {
				opening := f.store.data.wagers["opening:"+wallet.ID]
				if opening.Status != domain.StatusProcessed || opening.Kind != domain.KindOpening ||
					opening.ResultBalance != wallet.Balance || opening.ProviderID != "" {
					t.Fatalf("unexpected opening: %+v", opening)
				}
				if len(f.store.data.ledger) != 1 || f.store.data.ledger[0].BalanceBefore != units(t, 0) ||
					f.store.data.ledger[0].BalanceAfter != wallet.Balance || len(f.store.data.outbox) != 2 {
					t.Fatal("opening, ledger and events do not agree")
				}
			}
			loaded, err := f.wallets.Get(t.Context(), application.InternalActor(), wallet.ID)
			if err != nil || loaded != wallet {
				t.Fatalf("Get = %+v, %v; want %+v", loaded, err, wallet)
			}
			_, err = f.wallets.Create(t.Context(), application.InternalActor(), application.CreateWalletInput{
				PlayerID: wallet.PlayerID, InitialBalance: units(t, initial),
			})
			if !errors.Is(err, application.ErrConflict) || len(f.store.data.wallets) != 1 {
				t.Fatalf("duplicate wallet: %v", err)
			}
		})
	}
}

func TestCreateWalletFailureRollsBackEveryRecord(t *testing.T) {
	for _, step := range []string{"wallet.find", "wallet.insert", "wager.insert", "ledger.insert", "outbox.insert", "outbox.insert.2", "commit"} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t)
			before := f.store.data.clone()
			f.store.failAt = step
			result, err := f.wallets.Create(t.Context(), application.InternalActor(), application.CreateWalletInput{
				PlayerID: "player-1", InitialBalance: units(t, 10000),
			})
			if !errors.Is(err, errStorage) || result != (domain.WalletState{}) {
				t.Fatalf("failed creation returned result %+v, error %v", result, err)
			}
			if !reflect.DeepEqual(before, f.store.data) {
				t.Fatal("failed creation left partial records")
			}
		})
	}
}

func TestWalletOperationsAreInternalOnly(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	before, calls := f.store.data.clone(), f.store.calls
	_, createErr := f.wallets.Create(t.Context(), f.actor, application.CreateWalletInput{PlayerID: "player-2", InitialBalance: units(t, 0)})
	_, getErr := f.wallets.Get(t.Context(), f.actor, wallet.ID)
	_, listErr := f.wallets.ListLedger(t.Context(), f.actor, wallet.ID, "", 50)
	_, reconcileErr := f.wallets.Reconcile(t.Context(), f.actor, wallet.ID)
	for _, err := range []error{createErr, getErr, listErr, reconcileErr} {
		if !errors.Is(err, application.ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
	}
	if f.store.calls != calls || !reflect.DeepEqual(before, f.store.data) {
		t.Fatal("unauthorized call reached storage")
	}
}

func TestLedgerPaginationWithEqualTimestamps(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	for i := range 4 {
		f.process(t, wagerInput(t, wallet, domain.KindBet, 100, fmt.Sprintf("bet-%d", i)))
	}
	seen := make(map[string]bool)
	cursor := ""
	for {
		page, err := f.wallets.ListLedger(t.Context(), application.InternalActor(), wallet.ID, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Entries) > 2 {
			t.Fatal("page exceeds requested limit")
		}
		for _, entry := range page.Entries {
			if seen[entry.ID] {
				t.Fatal("pagination repeated an entry")
			}
			seen[entry.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		if cursor == page.NextCursor {
			t.Fatal("pagination cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("received %d entries, want 5", len(seen))
	}
	page, err := f.wallets.ListLedger(t.Context(), application.InternalActor(), wallet.ID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		walletID string
		cursor   string
		limit    int
	}{
		{wallet.ID, "invalid!", 50}, {wallet.ID, "e30", 50},
		{"another-wallet", page.NextCursor, 50}, {wallet.ID, "", -1}, {wallet.ID, "", 201},
	} {
		_, err := f.wallets.ListLedger(t.Context(), application.InternalActor(), test.walletID, test.cursor, test.limit)
		if !errors.Is(err, application.ErrInvalidInput) {
			t.Errorf("invalid pagination accepted: %+v, %v", test, err)
		}
	}
}

func TestReconciliationReportsDifferenceWithoutChangingBalance(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	f.process(t, wagerInput(t, wallet, domain.KindBet, 2500, "bet-1"))
	result, err := f.wallets.Reconcile(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || !result.Consistent || result.StoredBalance != units(t, 7500) ||
		result.CalculatedBalance != units(t, 7500) || result.CheckedEntries != 2 {
		t.Fatalf("unexpected reconciliation: %+v, %v", result, err)
	}
	for _, stored := range []int64{7499, 7501} {
		state := f.store.data.wallets[wallet.ID]
		state.Balance = units(t, stored)
		f.store.data.wallets[wallet.ID] = state // Simula uma divergência persistida.
		before := f.store.data.clone()
		result, err := f.wallets.Reconcile(t.Context(), application.InternalActor(), wallet.ID)
		if err != nil || result.Consistent || result.Difference != units(t, stored-7500) {
			t.Fatalf("unexpected divergence: %+v, %v", result, err)
		}
		if !reflect.DeepEqual(before, f.store.data) {
			t.Fatal("reconciliation changed financial records")
		}
	}
	if !bytes.Contains(f.logs.Bytes(), []byte(`"walletId"`)) {
		t.Fatal("reconciliation did not log its divergence")
	}
}

func TestReconciliationSupportsGrossTotalsLargerThanInt64(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 0)
	for i := range 2 {
		f.process(t, wagerInput(t, wallet, domain.KindWin, math.MaxInt64, fmt.Sprintf("win-%d", i)))
		f.process(t, wagerInput(t, wallet, domain.KindBet, math.MaxInt64, fmt.Sprintf("bet-%d", i)))
	}
	result, err := f.wallets.Reconcile(t.Context(), application.InternalActor(), wallet.ID)
	if err != nil || !result.Consistent || result.CalculatedBalance != units(t, 0) || result.CheckedEntries != 4 {
		t.Fatalf("unexpected reconciliation of large totals: %+v, %v", result, err)
	}
}

func TestCancellationRollsBackEvenAfterWrites(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.store.cancelAt, f.store.cancel = "outbox.insert.2", cancel
	before := f.store.data.clone()
	result, err := f.wallets.Create(ctx, application.InternalActor(), application.CreateWalletInput{
		PlayerID: "player-1", InitialBalance: units(t, 10000),
	})
	if !errors.Is(err, context.Canceled) || result != (domain.WalletState{}) || !reflect.DeepEqual(before, f.store.data) {
		t.Fatalf("canceled creation was not rolled back: %+v, %v", result, err)
	}
	f.store.cancelAt = ""
	calls := f.store.calls
	_, err = f.wallets.Get(ctx, application.InternalActor(), "wallet-1")
	if !errors.Is(err, context.Canceled) || f.store.calls != calls {
		t.Fatal("already canceled context reached a transaction")
	}
}

func TestUseCasesRejectMissingDependencies(t *testing.T) {
	for _, dependencies := range []application.Dependencies{
		{}, {Transactions: newMemoryStore(), ReferenceTTL: -time.Second},
	} {
		if _, err := application.NewWalletService(dependencies); !errors.Is(err, application.ErrInvalidDependencies) {
			t.Errorf("wallet dependencies: %v", err)
		}
		if _, err := application.NewWageringService(dependencies); !errors.Is(err, application.ErrInvalidDependencies) {
			t.Errorf("wager dependencies: %v", err)
		}
	}
}
