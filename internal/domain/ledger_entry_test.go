package domain_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

func TestNewWalletLedgerEntry(t *testing.T) {
	bet, _, _ := processTransaction(t, originalInput(t, domain.KindBet, 2500), nil, 10000)
	win, _, _ := processTransaction(t, originalInput(t, domain.KindWin, 2500), nil, 10000)
	refund, _, _ := processTransaction(t, originalInput(t, domain.KindRefund, 2500), bet, 10000)
	tests := []struct {
		name      string
		kind      domain.TransactionKind
		reference *domain.WagerTransaction
		direction domain.LedgerDirection
	}{
		{"opening", domain.KindOpening, nil, domain.DirectionCredit},
		{"bet", domain.KindBet, nil, domain.DirectionDebit},
		{"win", domain.KindWin, nil, domain.DirectionCredit},
		{"refund", domain.KindRefund, bet, domain.DirectionCredit},
		{"rollback of bet", domain.KindRollback, bet, domain.DirectionCredit},
		{"rollback of win", domain.KindRollback, win, domain.DirectionDebit},
		{"rollback of refund", domain.KindRollback, refund, domain.DirectionDebit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := transactionInput(t, tt.kind, 2500)
			if tt.reference != nil {
				input.ReferenceExternalTransactionID = transactionSnapshot(t, tt.reference).ExternalTransactionID
			}
			initial := int64(10000)
			if tt.kind == domain.KindOpening {
				initial = 2500
			}
			transaction, _, before := processTransaction(t, input, tt.reference, initial)
			if tt.kind == domain.KindOpening {
				before = moneyFromUnits(t, 0)
			}
			createdAt := walletTime().Add(3 * time.Minute).In(time.FixedZone("UTC-3", -3*60*60))
			entry, err := domain.NewWalletLedgerEntry("ledger-1", transaction, before, createdAt)
			if err != nil {
				t.Fatal(err)
			}
			state := ledgerSnapshot(t, entry)
			transactionState := transactionSnapshot(t, transaction)
			want := domain.LedgerEntryState{
				ID: "ledger-1", WalletID: input.WalletID, TransactionID: input.ID,
				Direction: tt.direction, Money: input.Money, BalanceBefore: before,
				BalanceAfter: transactionState.ResultBalance, CreatedAt: createdAt.UTC(),
			}
			if state != want {
				t.Fatalf("state = %+v, want %+v", state, want)
			}
		})
	}
}

func TestLedgerRejectsUnprocessedTransactionsAndLoss(t *testing.T) {
	for _, status := range []domain.TransactionStatus{domain.StatusPending, domain.StatusPendingReference, domain.StatusRejected, domain.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			transaction := newTransaction(t, transactionInput(t, domain.KindRefund, 2500))
			var err error
			switch status {
			case domain.StatusPendingReference:
				err = transaction.WaitForReference(walletTime())
			case domain.StatusRejected:
				err = transaction.Reject(domain.FailureReferenceNotFound, walletTime())
			case domain.StatusFailed:
				err = transaction.Fail(walletTime())
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = domain.NewWalletLedgerEntry("ledger-1", transaction, moneyFromUnits(t, 10000), walletTime())
			if !errors.Is(err, domain.ErrInvalidLedgerEntry) {
				t.Fatalf("error = %v, want ErrInvalidLedgerEntry", err)
			}
		})
	}
	loss, _, _ := processTransaction(t, transactionInput(t, domain.KindLoss, 0), nil, 10000)
	_, err := domain.NewWalletLedgerEntry("ledger-1", loss, moneyFromUnits(t, 10000), walletTime().Add(3*time.Minute))
	if !errors.Is(err, domain.ErrNoLedgerMovement) {
		t.Fatalf("LOSS: error = %v, want ErrNoLedgerMovement", err)
	}
	_, err = domain.NewWalletLedgerEntry("ledger-1", nil, moneyFromUnits(t, 10000), walletTime())
	if !errors.Is(err, domain.ErrInvalidTransaction) {
		t.Fatalf("nil transaction: %v", err)
	}
}

func TestNewLedgerValidatesPreviousBalanceAndTimestamp(t *testing.T) {
	transaction, _, _ := processTransaction(t, transactionInput(t, domain.KindBet, 2500), nil, 10000)
	tests := []struct {
		name      string
		id        string
		before    domain.Money
		createdAt time.Time
		wantErr   error
	}{
		{"missing ID", "", moneyFromUnits(t, 10000), walletTime().Add(3 * time.Minute), domain.ErrInvalidLedgerEntry},
		{"wrong balance", "ledger-1", moneyFromUnits(t, 10001), walletTime().Add(3 * time.Minute), domain.ErrInvalidLedgerEntry},
		{"wrong currency", "ledger-1", walletMoney(t, 10000, "USD"), walletTime().Add(3 * time.Minute), domain.ErrCurrencyMismatch},
		{"negative balance", "ledger-1", moneyFromUnits(t, -1), walletTime().Add(3 * time.Minute), domain.ErrNegativeBalance},
		{"invalid balance", "ledger-1", domain.Money{}, walletTime().Add(3 * time.Minute), domain.ErrInvalidMoney},
		{"missing timestamp", "ledger-1", moneyFromUnits(t, 10000), time.Time{}, domain.ErrInvalidLedgerTime},
		{"before processing", "ledger-1", moneyFromUnits(t, 10000), walletTime().Add(time.Minute), domain.ErrInvalidLedgerTime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := domain.NewWalletLedgerEntry(tt.id, transaction, tt.before, tt.createdAt)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if !errors.Is(entry.Validate(), domain.ErrInvalidLedgerEntry) {
				t.Fatal("invalid construction returned a usable ledger entry")
			}
		})
	}
}

func TestLedgerUsesOriginalTransactionBalance(t *testing.T) {
	transaction, wallet, before := processTransaction(t, transactionInput(t, domain.KindBet, 2500), nil, 10000)
	if err := wallet.Credit(moneyFromUnits(t, 5000), walletTime().Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	entry, err := domain.NewWalletLedgerEntry("ledger-1", transaction, before, walletTime().Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ledgerSnapshot(t, entry).BalanceAfter != moneyFromUnits(t, 7500) {
		t.Fatal("ledger used a later wallet balance")
	}
}

func TestRehydrateLedgerRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*domain.LedgerEntryState)
		wantErr error
	}{
		{"ID", func(s *domain.LedgerEntryState) { s.ID = "" }, domain.ErrInvalidLedgerEntry},
		{"wallet", func(s *domain.LedgerEntryState) { s.WalletID = "" }, domain.ErrInvalidLedgerEntry},
		{"transaction", func(s *domain.LedgerEntryState) { s.TransactionID = "" }, domain.ErrInvalidLedgerEntry},
		{"padded ID", func(s *domain.LedgerEntryState) { s.ID = " ledger-1" }, domain.ErrInvalidLedgerEntry},
		{"direction", func(s *domain.LedgerEntryState) { s.Direction = "UNKNOWN" }, domain.ErrInvalidLedgerEntry},
		{"money", func(s *domain.LedgerEntryState) { s.Money = domain.Money{} }, domain.ErrInvalidMoney},
		{"zero money", func(s *domain.LedgerEntryState) { s.Money = moneyFromUnits(t, 0) }, domain.ErrInvalidMovement},
		{"negative money", func(s *domain.LedgerEntryState) { s.Money = moneyFromUnits(t, -1) }, domain.ErrInvalidMovement},
		{"negative before", func(s *domain.LedgerEntryState) { s.BalanceBefore = moneyFromUnits(t, -1) }, domain.ErrNegativeBalance},
		{"negative after", func(s *domain.LedgerEntryState) { s.BalanceAfter = moneyFromUnits(t, -1) }, domain.ErrNegativeBalance},
		{"missing before", func(s *domain.LedgerEntryState) { s.BalanceBefore = domain.Money{} }, domain.ErrInvalidMoney},
		{"missing after", func(s *domain.LedgerEntryState) { s.BalanceAfter = domain.Money{} }, domain.ErrInvalidMoney},
		{"money currency", func(s *domain.LedgerEntryState) { s.Money = walletMoney(t, 2500, "USD") }, domain.ErrCurrencyMismatch},
		{"result currency", func(s *domain.LedgerEntryState) { s.BalanceAfter = walletMoney(t, 12500, "USD") }, domain.ErrCurrencyMismatch},
		{"wrong equation", func(s *domain.LedgerEntryState) { s.BalanceAfter = moneyFromUnits(t, 12501) }, domain.ErrInvalidLedgerEntry},
		{"timestamp", func(s *domain.LedgerEntryState) { s.CreatedAt = time.Time{} }, domain.ErrInvalidLedgerTime},
		{"credit overflow", func(s *domain.LedgerEntryState) { s.BalanceBefore = moneyFromUnits(t, math.MaxInt64) }, domain.ErrMoneyOverflow},
		{"debit beyond balance", func(s *domain.LedgerEntryState) {
			s.Direction = domain.DirectionDebit
			s.BalanceBefore = moneyFromUnits(t, 0)
			s.BalanceAfter = moneyFromUnits(t, 0)
		}, domain.ErrInvalidLedgerEntry},
	}
	transaction, _, before := processTransaction(t, transactionInput(t, domain.KindWin, 2500), nil, 10000)
	entry, err := domain.NewWalletLedgerEntry("ledger-1", transaction, before, walletTime().Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := ledgerSnapshot(t, entry)
			tt.change(&state)
			_, err := domain.RehydrateWalletLedgerEntry(state)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestLedgerRehydrationAndCopies(t *testing.T) {
	transaction, _, before := processTransaction(t, transactionInput(t, domain.KindWin, 2500), nil, 10000)
	entry, err := domain.NewWalletLedgerEntry("ledger-1", transaction, before, walletTime().Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	state := ledgerSnapshot(t, entry)
	copyState := state
	copyState.CreatedAt = copyState.CreatedAt.In(time.FixedZone("UTC-3", -3*60*60))
	copyEntry, err := domain.RehydrateWalletLedgerEntry(copyState)
	if err != nil {
		t.Fatal(err)
	}
	if ledgerSnapshot(t, copyEntry) != state {
		t.Fatal("rehydration changed ledger data or did not normalize UTC")
	}
	copyState.ID = "another-entry"
	copyState.Money = moneyFromUnits(t, 1)
	copyState.BalanceAfter = moneyFromUnits(t, 10001)
	if ledgerSnapshot(t, entry) != state || ledgerSnapshot(t, copyEntry) != state {
		t.Fatal("changing a snapshot changed an immutable ledger entry")
	}
}

func TestUninitializedLedger(t *testing.T) {
	var entry domain.WalletLedgerEntry
	_, snapshotErr := entry.Snapshot()
	for _, err := range []error{entry.Validate(), snapshotErr} {
		if !errors.Is(err, domain.ErrInvalidLedgerEntry) {
			t.Errorf("error = %v, want ErrInvalidLedgerEntry", err)
		}
	}
}

func ledgerSnapshot(t *testing.T, entry domain.WalletLedgerEntry) domain.LedgerEntryState {
	t.Helper()
	state, err := entry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return state
}
