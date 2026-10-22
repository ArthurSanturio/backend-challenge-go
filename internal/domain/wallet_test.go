package domain_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

func TestNewWallet(t *testing.T) {
	for _, currency := range []string{"BRL", "USD", "EUR"} {
		for _, units := range []int64{0, 10000, math.MaxInt64} {
			balance := walletMoney(t, units, currency)
			createdAt := walletTime().In(time.FixedZone("UTC-3", -3*60*60))
			wallet, err := domain.NewWallet("wallet-1", "player-1", balance, createdAt)
			if err != nil {
				t.Fatal(err)
			}
			state := walletSnapshot(t, wallet)
			want := domain.WalletState{
				ID: "wallet-1", PlayerID: "player-1", Currency: currency,
				Balance: balance, Version: 1, CreatedAt: walletTime(), UpdatedAt: walletTime(),
			}
			if state != want {
				t.Errorf("state = %+v, want %+v", state, want)
			}
		}
	}
}

func TestNewWalletRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		playerID  string
		balance   domain.Money
		createdAt time.Time
		wantErr   error
	}{
		{"empty wallet ID", "", "player-1", moneyFromUnits(t, 0), walletTime(), domain.ErrInvalidWallet},
		{"blank wallet ID", " \t", "player-1", moneyFromUnits(t, 0), walletTime(), domain.ErrInvalidWallet},
		{"padded wallet ID", " wallet-1", "player-1", moneyFromUnits(t, 0), walletTime(), domain.ErrInvalidWallet},
		{"empty player ID", "wallet-1", "", moneyFromUnits(t, 0), walletTime(), domain.ErrInvalidWallet},
		{"blank player ID", "wallet-1", "\t", moneyFromUnits(t, 0), walletTime(), domain.ErrInvalidWallet},
		{"padded player ID", "wallet-1", "player-1 ", moneyFromUnits(t, 0), walletTime(), domain.ErrInvalidWallet},
		{"invalid money", "wallet-1", "player-1", domain.Money{}, walletTime(), domain.ErrInvalidMoney},
		{"negative balance", "wallet-1", "player-1", moneyFromUnits(t, -1), walletTime(), domain.ErrNegativeBalance},
		{"minimum balance", "wallet-1", "player-1", moneyFromUnits(t, math.MinInt64), walletTime(), domain.ErrNegativeBalance},
		{"zero timestamp", "wallet-1", "player-1", moneyFromUnits(t, 0), time.Time{}, domain.ErrInvalidWalletTime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wallet, err := domain.NewWallet(tt.id, tt.playerID, tt.balance, tt.createdAt)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if wallet != nil {
				t.Fatal("invalid creation returned a wallet")
			}
		})
	}
}

func TestWalletCreditAndDebit(t *testing.T) {
	wallet := newWallet(t)
	if err := wallet.Credit(moneyFromUnits(t, 2500), walletTime().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	afterCredit := walletSnapshot(t, wallet)
	if afterCredit.Balance != moneyFromUnits(t, 12500) || afterCredit.Version != 2 ||
		afterCredit.UpdatedAt != walletTime().Add(time.Minute) {
		t.Fatalf("unexpected credit state: %+v", afterCredit)
	}

	debitTime := walletTime().Add(2 * time.Minute).In(time.FixedZone("UTC-3", -3*60*60))
	if err := wallet.Debit(moneyFromUnits(t, 8000), debitTime); err != nil {
		t.Fatal(err)
	}
	afterDebit := walletSnapshot(t, wallet)
	if afterDebit.Balance != moneyFromUnits(t, 4500) || afterDebit.Version != 3 ||
		afterDebit.UpdatedAt != debitTime.UTC() {
		t.Fatalf("unexpected debit state: %+v", afterDebit)
	}
	if afterDebit.ID != afterCredit.ID || afterDebit.PlayerID != afterCredit.PlayerID ||
		afterDebit.Currency != afterCredit.Currency || afterDebit.CreatedAt != walletTime() {
		t.Fatal("movement changed the wallet identity, currency or creation time")
	}
}

func TestWalletDebitCanReachZero(t *testing.T) {
	wallet := newWallet(t)
	if err := wallet.Debit(moneyFromUnits(t, 10000), walletTime()); err != nil {
		t.Fatal(err)
	}
	state := walletSnapshot(t, wallet)
	if state.Balance != moneyFromUnits(t, 0) || state.Version != 2 || state.UpdatedAt != walletTime() {
		t.Fatalf("unexpected zero balance state: %+v", state)
	}
}

func TestWalletInsufficientFundsPreservesState(t *testing.T) {
	wallet := newWallet(t)
	if err := wallet.Debit(moneyFromUnits(t, 8000), walletTime().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := walletSnapshot(t, wallet)
	err := wallet.Debit(moneyFromUnits(t, 8000), walletTime().Add(2*time.Minute))
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}
	if walletSnapshot(t, wallet) != before {
		t.Fatal("rejected debit changed wallet state")
	}
	if before.Balance != moneyFromUnits(t, 2000) || before.Version != 2 {
		t.Fatalf("unexpected final state: %+v", before)
	}
}

func TestWalletRejectsInvalidMovementsWithoutMutation(t *testing.T) {
	tests := []struct {
		name       string
		amount     domain.Money
		occurredAt time.Time
		wantErr    error
	}{
		{"zero", moneyFromUnits(t, 0), walletTime(), domain.ErrInvalidMovement},
		{"negative", moneyFromUnits(t, -1), walletTime(), domain.ErrInvalidMovement},
		{"minimum int64", moneyFromUnits(t, math.MinInt64), walletTime(), domain.ErrInvalidMovement},
		{"invalid money", domain.Money{}, walletTime(), domain.ErrInvalidMoney},
		{"different currency", walletMoney(t, 100, "USD"), walletTime(), domain.ErrCurrencyMismatch},
		{"missing timestamp", moneyFromUnits(t, 100), time.Time{}, domain.ErrInvalidWalletTime},
		{"older timestamp", moneyFromUnits(t, 100), walletTime().Add(-time.Nanosecond), domain.ErrInvalidWalletTime},
	}
	for _, tt := range tests {
		for _, operation := range []string{"credit", "debit"} {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				wallet := newWallet(t)
				before := walletSnapshot(t, wallet)
				var err error
				if operation == "credit" {
					err = wallet.Credit(tt.amount, tt.occurredAt)
				} else {
					err = wallet.Debit(tt.amount, tt.occurredAt)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				if walletSnapshot(t, wallet) != before {
					t.Fatal("invalid movement changed wallet state")
				}
			})
		}
	}
}

func TestWalletCannotMoveBeforeLastUpdate(t *testing.T) {
	wallet := newWallet(t)
	if err := wallet.Credit(moneyFromUnits(t, 1), walletTime().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := walletSnapshot(t, wallet)
	err := wallet.Debit(moneyFromUnits(t, 1), walletTime().Add(time.Minute))
	if !errors.Is(err, domain.ErrInvalidWalletTime) {
		t.Fatalf("error = %v, want ErrInvalidWalletTime", err)
	}
	if walletSnapshot(t, wallet) != before {
		t.Fatal("backdated movement changed wallet state")
	}
}

func TestWalletCreditOverflowPreservesState(t *testing.T) {
	wallet, err := domain.NewWallet("wallet-1", "player-1", moneyFromUnits(t, math.MaxInt64), walletTime())
	if err != nil {
		t.Fatal(err)
	}
	before := walletSnapshot(t, wallet)
	err = wallet.Credit(moneyFromUnits(t, 1), walletTime().Add(time.Minute))
	if !errors.Is(err, domain.ErrMoneyOverflow) {
		t.Fatalf("error = %v, want ErrMoneyOverflow", err)
	}
	if walletSnapshot(t, wallet) != before {
		t.Fatal("overflow changed wallet state")
	}
	if err := wallet.Debit(moneyFromUnits(t, math.MaxInt64), walletTime()); err != nil {
		t.Fatal(err)
	}
	if walletSnapshot(t, wallet).Balance != moneyFromUnits(t, 0) {
		t.Fatal("debit of maximum balance did not reach zero")
	}
}

func TestRehydrateWalletPreservesStoredState(t *testing.T) {
	state := walletSnapshot(t, newWallet(t))
	state.Version = 7
	state.Balance = moneyFromUnits(t, 1234)
	state.UpdatedAt = walletTime().Add(time.Hour)
	wallet, err := domain.RehydrateWallet(state)
	if err != nil {
		t.Fatal(err)
	}
	if got := walletSnapshot(t, wallet); got != state {
		t.Fatalf("rehydration changed stored state: got %+v, want %+v", got, state)
	}

	if err := wallet.Credit(moneyFromUnits(t, 1), state.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	got := walletSnapshot(t, wallet)
	if got.Version != 8 || got.Balance != moneyFromUnits(t, 1235) {
		t.Fatalf("unexpected state after rehydrated credit: %+v", got)
	}
}

func TestRehydrateWalletNormalizesUTC(t *testing.T) {
	state := walletSnapshot(t, newWallet(t))
	zone := time.FixedZone("UTC-3", -3*60*60)
	state.CreatedAt = state.CreatedAt.In(zone)
	state.UpdatedAt = state.UpdatedAt.Add(time.Hour).In(zone)
	wallet, err := domain.RehydrateWallet(state)
	if err != nil {
		t.Fatal(err)
	}
	got := walletSnapshot(t, wallet)
	if got.CreatedAt != walletTime() || got.UpdatedAt != walletTime().Add(time.Hour) {
		t.Fatalf("timestamps were not normalized to UTC: %+v", got)
	}
}

func TestRehydrateWalletRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*domain.WalletState)
		wantErr error
	}{
		{"empty ID", func(s *domain.WalletState) { s.ID = "" }, domain.ErrInvalidWallet},
		{"empty player", func(s *domain.WalletState) { s.PlayerID = "" }, domain.ErrInvalidWallet},
		{"invalid currency", func(s *domain.WalletState) { s.Currency = "ZZZ" }, domain.ErrInvalidCurrency},
		{"currency mismatch", func(s *domain.WalletState) { s.Currency = "EUR" }, domain.ErrCurrencyMismatch},
		{"invalid money", func(s *domain.WalletState) { s.Balance = domain.Money{} }, domain.ErrInvalidMoney},
		{"negative balance", func(s *domain.WalletState) { s.Balance = moneyFromUnits(t, -1) }, domain.ErrNegativeBalance},
		{"zero version", func(s *domain.WalletState) { s.Version = 0 }, domain.ErrInvalidWallet},
		{"negative version", func(s *domain.WalletState) { s.Version = -1 }, domain.ErrInvalidWallet},
		{"missing creation", func(s *domain.WalletState) { s.CreatedAt = time.Time{} }, domain.ErrInvalidWalletTime},
		{"missing update", func(s *domain.WalletState) { s.UpdatedAt = time.Time{} }, domain.ErrInvalidWalletTime},
		{"update before creation", func(s *domain.WalletState) { s.UpdatedAt = s.CreatedAt.Add(-time.Nanosecond) }, domain.ErrInvalidWalletTime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := walletSnapshot(t, newWallet(t))
			tt.change(&state)
			wallet, err := domain.RehydrateWallet(state)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if wallet != nil {
				t.Fatal("invalid rehydration returned a wallet")
			}
		})
	}
}

func TestWalletVersionOverflowPreservesState(t *testing.T) {
	for _, operation := range []string{"credit", "debit"} {
		t.Run(operation, func(t *testing.T) {
			state := walletSnapshot(t, newWallet(t))
			state.Version = math.MaxInt64
			wallet, err := domain.RehydrateWallet(state)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "credit" {
				err = wallet.Credit(moneyFromUnits(t, 1), walletTime())
			} else {
				err = wallet.Debit(moneyFromUnits(t, 1), walletTime())
			}
			if !errors.Is(err, domain.ErrWalletVersionOverflow) {
				t.Fatalf("error = %v, want ErrWalletVersionOverflow", err)
			}
			if walletSnapshot(t, wallet) != state {
				t.Fatal("version overflow changed wallet state")
			}
		})
	}
}

func TestWalletSnapshotAndRehydrationDoNotExposeMutableState(t *testing.T) {
	wallet := newWallet(t)
	before := walletSnapshot(t, wallet)
	copy := walletSnapshot(t, wallet)
	rehydrated, err := domain.RehydrateWallet(copy)
	if err != nil {
		t.Fatal(err)
	}
	copy.ID = "another-wallet"
	copy.PlayerID = "another-player"
	copy.Currency = "USD"
	copy.Balance = walletMoney(t, 999, "USD")
	copy.Version = 99
	copy.CreatedAt = walletTime().Add(time.Hour)
	copy.UpdatedAt = copy.CreatedAt
	if walletSnapshot(t, wallet) != before || walletSnapshot(t, rehydrated) != before {
		t.Fatal("changing a state copy changed a wallet")
	}
}

func TestWalletRejectsUninitializedValues(t *testing.T) {
	for _, wallet := range []*domain.Wallet{nil, {}} {
		_, snapshotErr := wallet.Snapshot()
		for _, err := range []error{
			wallet.Validate(), snapshotErr,
			wallet.Credit(moneyFromUnits(t, 1), walletTime()),
			wallet.Debit(moneyFromUnits(t, 1), walletTime()),
		} {
			if !errors.Is(err, domain.ErrInvalidWallet) {
				t.Errorf("error = %v, want ErrInvalidWallet", err)
			}
		}
	}
}

func newWallet(t *testing.T) *domain.Wallet {
	t.Helper()
	wallet, err := domain.NewWallet("wallet-1", "player-1", moneyFromUnits(t, 10000), walletTime())
	if err != nil {
		t.Fatal(err)
	}
	return wallet
}

func walletSnapshot(t *testing.T, wallet *domain.Wallet) domain.WalletState {
	t.Helper()
	state, err := wallet.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func walletMoney(t *testing.T, units int64, currency string) domain.Money {
	t.Helper()
	money, err := domain.NewMoneyFromMinorUnits(units, currency)
	if err != nil {
		t.Fatal(err)
	}
	return money
}

func walletTime() time.Time {
	return time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
}
