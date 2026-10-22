package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

func TestNewWagerTransaction(t *testing.T) {
	for _, kind := range []domain.TransactionKind{domain.KindBet, domain.KindWin, domain.KindLoss, domain.KindRefund, domain.KindRollback} {
		t.Run(string(kind), func(t *testing.T) {
			units := int64(2500)
			if kind == domain.KindLoss {
				units = 0
			}
			input := transactionInput(t, kind, units)
			transaction, err := domain.NewWagerTransaction(input, walletTime())
			if err != nil {
				t.Fatal(err)
			}
			state := transactionSnapshot(t, transaction)
			if state.WagerTransactionInput != input || state.Status != domain.StatusPending ||
				state.CreatedAt != walletTime() || state.UpdatedAt != walletTime() ||
				state.ResultBalance != (domain.Money{}) || state.Direction != "" || state.FailureCode != "" {
				t.Fatalf("unexpected initial state: %+v", state)
			}
		})
	}
}

func TestTransactionAmountPolicy(t *testing.T) {
	for _, kind := range []domain.TransactionKind{
		domain.KindOpening, domain.KindBet, domain.KindWin, domain.KindLoss, domain.KindRefund, domain.KindRollback,
	} {
		for _, units := range []int64{-1, 0, 1} {
			input := transactionInput(t, kind, units)
			var err error
			if kind == domain.KindOpening {
				_, err = domain.NewOpeningTransaction(input.ID, input.WalletID, input.PlayerID, input.Money, walletTime())
			} else {
				_, err = domain.NewWagerTransaction(input, walletTime())
			}
			valid := (kind == domain.KindLoss && units == 0) || (kind != domain.KindLoss && units > 0)
			if valid && err != nil {
				t.Errorf("kind %s, units %d: %v", kind, units, err)
			}
			if !valid && !errors.Is(err, domain.ErrInvalidTransactionAmount) {
				t.Errorf("kind %s, units %d: error = %v, want ErrInvalidTransactionAmount", kind, units, err)
			}
		}
	}
}

func TestOpeningTransaction(t *testing.T) {
	input := transactionInput(t, domain.KindOpening, 10000)
	if _, err := domain.NewWagerTransaction(input, walletTime()); !errors.Is(err, domain.ErrInvalidTransaction) {
		t.Fatalf("external OPENING: error = %v, want ErrInvalidTransaction", err)
	}
	transaction, wallet, _ := processTransaction(t, input, nil, 10000)
	state := transactionSnapshot(t, transaction)
	if state.Status != domain.StatusProcessed || state.Direction != domain.DirectionCredit ||
		state.ResultBalance != input.Money || state.ProviderID != "" || state.ExternalTransactionID != "" ||
		state.IdempotencyKey != "" || state.PayloadHash != "" || state.RoundID != "" || state.GameID != "" {
		t.Fatalf("unexpected opening state: %+v", state)
	}
	if walletSnapshot(t, wallet).Version != 1 {
		t.Fatal("opening increased wallet version")
	}
	state.ProviderID = "provider-1"
	if _, err := domain.RehydrateWagerTransaction(state); !errors.Is(err, domain.ErrInvalidTransaction) {
		t.Fatalf("internal opening with external metadata: %v", err)
	}

	pending, err := domain.NewOpeningTransaction("opening-1", "wallet-1", "player-1", moneyFromUnits(t, 500), walletTime())
	if err != nil {
		t.Fatal(err)
	}
	before := transactionSnapshot(t, pending)
	if err := pending.Process(wallet, nil, walletTime().Add(3*time.Minute)); !errors.Is(err, domain.ErrInvalidTransaction) {
		t.Fatalf("opening with wrong initial balance: %v", err)
	}
	if transactionSnapshot(t, pending) != before {
		t.Fatal("invalid opening processing changed transaction")
	}
}

func TestTransactionRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		change  func(*domain.WagerTransactionInput)
		wantErr error
	}{
		{"ID", func(i *domain.WagerTransactionInput) { i.ID = "" }, domain.ErrInvalidTransaction},
		{"player", func(i *domain.WagerTransactionInput) { i.PlayerID = "" }, domain.ErrInvalidTransaction},
		{"wallet", func(i *domain.WagerTransactionInput) { i.WalletID = "" }, domain.ErrInvalidTransaction},
		{"provider", func(i *domain.WagerTransactionInput) { i.ProviderID = "" }, domain.ErrInvalidTransaction},
		{"external ID", func(i *domain.WagerTransactionInput) { i.ExternalTransactionID = "" }, domain.ErrInvalidTransaction},
		{"key", func(i *domain.WagerTransactionInput) { i.IdempotencyKey = "" }, domain.ErrInvalidTransaction},
		{"hash", func(i *domain.WagerTransactionInput) { i.PayloadHash = "" }, domain.ErrInvalidTransaction},
		{"round", func(i *domain.WagerTransactionInput) { i.RoundID = "" }, domain.ErrInvalidTransaction},
		{"game", func(i *domain.WagerTransactionInput) { i.GameID = "" }, domain.ErrInvalidTransaction},
		{"whitespace", func(i *domain.WagerTransactionInput) { i.ProviderID = " provider-1" }, domain.ErrInvalidTransaction},
		{"kind", func(i *domain.WagerTransactionInput) { i.Kind = "UNKNOWN" }, domain.ErrInvalidTransaction},
		{"money", func(i *domain.WagerTransactionInput) { i.Money = domain.Money{} }, domain.ErrInvalidMoney},
		{"unexpected reference", func(i *domain.WagerTransactionInput) { i.ReferenceExternalTransactionID = "original" }, domain.ErrInvalidReference},
		{"missing reference", func(i *domain.WagerTransactionInput) { i.Kind = domain.KindRefund }, domain.ErrInvalidReference},
		{"self reference", func(i *domain.WagerTransactionInput) {
			i.Kind = domain.KindWin
			i.ReferenceExternalTransactionID = i.ExternalTransactionID
		}, domain.ErrInvalidReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := transactionInput(t, domain.KindBet, 100)
			tt.change(&input)
			transaction, err := domain.NewWagerTransaction(input, walletTime())
			if !errors.Is(err, tt.wantErr) || transaction != nil {
				t.Fatalf("transaction = %v, error = %v; want nil, %v", transaction, err, tt.wantErr)
			}
		})
	}
	if _, err := domain.NewWagerTransaction(transactionInput(t, domain.KindBet, 100), time.Time{}); !errors.Is(err, domain.ErrInvalidTransactionTime) {
		t.Fatalf("zero creation timestamp: %v", err)
	}
}

func TestTransactionProcessPreservesOriginalBalance(t *testing.T) {
	transaction, wallet, _ := processTransaction(t, transactionInput(t, domain.KindBet, 2500), nil, 10000)
	before := transactionSnapshot(t, transaction)
	if before.Status != domain.StatusProcessed || before.Direction != domain.DirectionDebit || before.ResultBalance != moneyFromUnits(t, 7500) {
		t.Fatalf("unexpected processed state: %+v", before)
	}
	if err := wallet.Credit(moneyFromUnits(t, 1000), walletTime().Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if transactionSnapshot(t, transaction) != before {
		t.Fatal("later wallet movement changed original transaction result")
	}
}

func TestLossDoesNotChangeWalletAndValidatesCurrency(t *testing.T) {
	wallet := newWallet(t)
	transaction := newTransaction(t, transactionInput(t, domain.KindLoss, 0))
	before := walletSnapshot(t, wallet)
	if err := transaction.Process(wallet, nil, walletTime()); err != nil {
		t.Fatal(err)
	}
	if walletSnapshot(t, wallet) != before || transactionSnapshot(t, transaction).Direction != "" {
		t.Fatal("LOSS changed the wallet or has a financial direction")
	}
	input := transactionInput(t, domain.KindLoss, 0)
	input.Money = walletMoney(t, 0, "USD")
	other := newTransaction(t, input)
	if err := other.Process(wallet, nil, walletTime()); !errors.Is(err, domain.ErrCurrencyMismatch) {
		t.Fatalf("LOSS in different currency: %v", err)
	}
	if transactionSnapshot(t, other).Status != domain.StatusPending {
		t.Fatal("invalid LOSS was marked as processed")
	}
}

func TestTransactionReferenceRules(t *testing.T) {
	betInput := originalInput(t, domain.KindBet, 2500)
	bet, _, _ := processTransaction(t, betInput, nil, 10000)
	winInput := originalInput(t, domain.KindWin, 2500)
	win, _, _ := processTransaction(t, winInput, nil, 10000)
	refundInput := originalInput(t, domain.KindRefund, 2500)
	refund, _, _ := processTransaction(t, refundInput, bet, 10000)
	loss, _, _ := processTransaction(t, originalInput(t, domain.KindLoss, 0), nil, 10000)

	tests := []struct {
		name      string
		kind      domain.TransactionKind
		reference *domain.WagerTransaction
		units     int64
		direction domain.LedgerDirection
		wantErr   error
	}{
		{"WIN references BET", domain.KindWin, bet, 5000, domain.DirectionCredit, nil},
		{"REFUND references BET", domain.KindRefund, bet, 2500, domain.DirectionCredit, nil},
		{"ROLLBACK of BET", domain.KindRollback, bet, 2500, domain.DirectionCredit, nil},
		{"ROLLBACK of WIN", domain.KindRollback, win, 2500, domain.DirectionDebit, nil},
		{"ROLLBACK of REFUND", domain.KindRollback, refund, 2500, domain.DirectionDebit, nil},
		{"partial REFUND", domain.KindRefund, bet, 1000, "", domain.ErrInvalidReference},
		{"partial ROLLBACK", domain.KindRollback, win, 1000, "", domain.ErrInvalidReference},
		{"REFUND of WIN", domain.KindRefund, win, 2500, "", domain.ErrInvalidReference},
		{"WIN references WIN", domain.KindWin, win, 2500, "", domain.ErrInvalidReference},
		{"ROLLBACK of LOSS", domain.KindRollback, loss, 2500, "", domain.ErrInvalidReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := transactionInput(t, tt.kind, tt.units)
			input.ReferenceExternalTransactionID = transactionSnapshot(t, tt.reference).ExternalTransactionID
			transaction := newTransaction(t, input)
			before := transactionSnapshot(t, transaction)
			direction, err := transaction.MovementDirection(tt.reference)
			if err != nil || tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
			} else if direction != tt.direction {
				t.Fatalf("direction = %s, want %s", direction, tt.direction)
			}
			if transactionSnapshot(t, transaction) != before {
				t.Fatal("checking a reference changed transaction state")
			}
		})
	}
}

func TestTransactionRejectsReferenceMismatches(t *testing.T) {
	original, _, _ := processTransaction(t, originalInput(t, domain.KindBet, 2500), nil, 10000)
	tests := []struct {
		name    string
		change  func(*domain.WagerTransactionState)
		wantErr error
	}{
		{"provider", func(s *domain.WagerTransactionState) { s.ProviderID = "other-provider" }, domain.ErrReferenceMismatch},
		{"player", func(s *domain.WagerTransactionState) { s.PlayerID = "other-player" }, domain.ErrReferenceMismatch},
		{"wallet", func(s *domain.WagerTransactionState) { s.WalletID = "other-wallet" }, domain.ErrReferenceMismatch},
		{"round", func(s *domain.WagerTransactionState) { s.RoundID = "other-round" }, domain.ErrReferenceMismatch},
		{"external ID", func(s *domain.WagerTransactionState) { s.ExternalTransactionID = "other-external" }, domain.ErrReferenceMismatch},
		{"internal ID", func(s *domain.WagerTransactionState) { s.ID = "transaction-1" }, domain.ErrReferenceMismatch},
		{"currency", func(s *domain.WagerTransactionState) {
			s.Money = walletMoney(t, 2500, "USD")
			s.ResultBalance = walletMoney(t, 7500, "USD")
		}, domain.ErrCurrencyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := transactionSnapshot(t, original)
			tt.change(&state)
			reference, err := domain.RehydrateWagerTransaction(state)
			if err != nil {
				t.Fatal(err)
			}
			transaction := newTransaction(t, transactionInput(t, domain.KindRefund, 2500))
			_, err = transaction.MovementDirection(reference)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestPendingReferenceCanBeResolved(t *testing.T) {
	transaction := newTransaction(t, transactionInput(t, domain.KindRefund, 2500))
	if err := transaction.WaitForReference(walletTime().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := transactionSnapshot(t, transaction)
	if before.Status != domain.StatusPendingReference {
		t.Fatal("transaction is not waiting for reference")
	}
	if err := transaction.WaitForReference(walletTime().Add(time.Minute)); !errors.Is(err, domain.ErrInvalidTransactionTransition) {
		t.Fatalf("repeated pending transition: %v", err)
	}
	pendingReference := newTransaction(t, originalInput(t, domain.KindBet, 2500))
	if _, err := transaction.MovementDirection(pendingReference); !errors.Is(err, domain.ErrReferenceNotProcessed) {
		t.Fatalf("pending original: %v", err)
	}
	if _, err := transaction.MovementDirection(nil); !errors.Is(err, domain.ErrInvalidReference) {
		t.Fatalf("missing original: %v", err)
	}
	if transactionSnapshot(t, transaction) != before {
		t.Fatal("unavailable reference changed pending transaction")
	}
	original, _, _ := processTransaction(t, originalInput(t, domain.KindBet, 2500), nil, 10000)
	wallet := newWallet(t)
	if err := wallet.Credit(moneyFromUnits(t, 2500), walletTime().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Process(wallet, original, walletTime().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	state := transactionSnapshot(t, transaction)
	if state.Status != domain.StatusProcessed || state.ReferenceTransactionID != transactionSnapshot(t, original).ID {
		t.Fatalf("unexpected resolution: %+v", state)
	}
}

func TestTransactionTerminalStatesCannotTransition(t *testing.T) {
	for _, status := range []domain.TransactionStatus{domain.StatusProcessed, domain.StatusRejected, domain.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			transaction := newTransaction(t, transactionInput(t, domain.KindBet, 100))
			var err error
			switch status {
			case domain.StatusProcessed:
				transaction, _, _ = processTransaction(t, transactionInput(t, domain.KindBet, 100), nil, 10000)
			case domain.StatusRejected:
				err = transaction.Reject(domain.FailureInsufficientFunds, walletTime())
			case domain.StatusFailed:
				err = transaction.Fail(walletTime())
			}
			if err != nil {
				t.Fatal(err)
			}
			before := transactionSnapshot(t, transaction)
			_, directionErr := transaction.MovementDirection(nil)
			for _, err := range []error{
				directionErr, transaction.Process(newWallet(t), nil, walletTime().Add(3*time.Minute)),
				transaction.Reject(domain.FailureInsufficientFunds, walletTime().Add(3*time.Minute)),
				transaction.Fail(walletTime().Add(3 * time.Minute)), transaction.WaitForReference(walletTime().Add(3 * time.Minute)),
			} {
				if !errors.Is(err, domain.ErrInvalidTransactionTransition) {
					t.Errorf("error = %v, want ErrInvalidTransactionTransition", err)
				}
			}
			if transactionSnapshot(t, transaction) != before {
				t.Fatal("terminal state changed")
			}
		})
	}
}

func TestTransactionRejectionCodes(t *testing.T) {
	tests := []struct {
		kind  domain.TransactionKind
		code  domain.FailureCode
		valid bool
	}{
		{domain.KindBet, domain.FailureInsufficientFunds, true},
		{domain.KindRollback, domain.FailureReversalInsufficientFunds, true},
		{domain.KindRefund, domain.FailureReferenceNotFound, true},
		{domain.KindRefund, domain.FailureReferenceNotProcessed, true},
		{domain.KindRefund, domain.FailureReferenceMismatch, true},
		{domain.KindRefund, domain.FailureAlreadyReversed, true},
		{domain.KindBet, domain.FailureInvalidOperation, true},
		{domain.KindBet, domain.FailureReversalInsufficientFunds, false},
		{domain.KindRollback, domain.FailureInsufficientFunds, false},
		{domain.KindBet, domain.FailureReferenceNotFound, false},
		{domain.KindBet, domain.FailurePermanentInfrastructure, false},
		{domain.KindBet, "", false},
		{domain.KindBet, "UNKNOWN", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind)+"/"+string(tt.code), func(t *testing.T) {
			transaction := newTransaction(t, transactionInput(t, tt.kind, 100))
			before := transactionSnapshot(t, transaction)
			err := transaction.Reject(tt.code, walletTime())
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
				state := transactionSnapshot(t, transaction)
				if state.Status != domain.StatusRejected || state.FailureCode != tt.code {
					t.Fatalf("unexpected rejection: %+v", state)
				}
			} else if !errors.Is(err, domain.ErrInvalidFailureCode) || transactionSnapshot(t, transaction) != before {
				t.Fatalf("invalid rejection: error = %v, state = %+v", err, transactionSnapshot(t, transaction))
			}
		})
	}
}

func TestTransactionTimeAndWalletValidationPreservesState(t *testing.T) {
	transaction := newTransaction(t, transactionInput(t, domain.KindBet, 100))
	before := transactionSnapshot(t, transaction)
	for _, occurredAt := range []time.Time{{}, walletTime().Add(-time.Second)} {
		for _, err := range []error{
			transaction.Process(newWallet(t), nil, occurredAt),
			transaction.Reject(domain.FailureInsufficientFunds, occurredAt), transaction.Fail(occurredAt),
		} {
			if !errors.Is(err, domain.ErrInvalidTransactionTime) {
				t.Errorf("error = %v, want ErrInvalidTransactionTime", err)
			}
		}
	}
	otherWallet, err := domain.NewWallet("other-wallet", "player-1", moneyFromUnits(t, 10000), walletTime())
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Process(otherWallet, nil, walletTime()); !errors.Is(err, domain.ErrInvalidTransaction) {
		t.Fatalf("wrong wallet: %v", err)
	}
	wallet := newWallet(t)
	if err := wallet.Debit(moneyFromUnits(t, 100), walletTime().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Process(wallet, nil, walletTime()); !errors.Is(err, domain.ErrInvalidTransactionTime) {
		t.Fatalf("processing before wallet movement: %v", err)
	}
	if transactionSnapshot(t, transaction) != before {
		t.Fatal("invalid processing changed the transaction")
	}
}

func TestTransactionRehydrationAndSnapshot(t *testing.T) {
	transaction, _, _ := processTransaction(t, transactionInput(t, domain.KindBet, 2500), nil, 10000)
	state := transactionSnapshot(t, transaction)
	copy, err := domain.RehydrateWagerTransaction(state)
	if err != nil {
		t.Fatal(err)
	}
	if transactionSnapshot(t, copy) != state {
		t.Fatal("rehydration changed stored state")
	}
	state.ID, state.Status = "other-ID", domain.StatusFailed
	state.Money, state.ResultBalance = moneyFromUnits(t, 100), moneyFromUnits(t, 999)
	if transactionSnapshot(t, copy) != transactionSnapshot(t, transaction) {
		t.Fatal("changing snapshot changed a transaction")
	}
}

func TestTransactionRejectsInconsistentStoredState(t *testing.T) {
	processed, _, _ := processTransaction(t, transactionInput(t, domain.KindBet, 100), nil, 10000)
	tests := []struct {
		name   string
		change func(*domain.WagerTransactionState)
	}{
		{"unknown status", func(s *domain.WagerTransactionState) { s.Status = "UNKNOWN" }},
		{"pending with result", func(s *domain.WagerTransactionState) { s.Status = domain.StatusPending }},
		{"wrong direction", func(s *domain.WagerTransactionState) { s.Direction = domain.DirectionCredit }},
		{"missing result", func(s *domain.WagerTransactionState) { s.ResultBalance = domain.Money{} }},
		{"negative result", func(s *domain.WagerTransactionState) { s.ResultBalance = moneyFromUnits(t, -1) }},
		{"wrong result currency", func(s *domain.WagerTransactionState) { s.ResultBalance = walletMoney(t, 100, "USD") }},
		{"unexpected reference", func(s *domain.WagerTransactionState) { s.ReferenceTransactionID = "other-ID" }},
		{"failure on success", func(s *domain.WagerTransactionState) { s.FailureCode = domain.FailureInsufficientFunds }},
		{"missing creation", func(s *domain.WagerTransactionState) { s.CreatedAt = time.Time{} }},
		{"backdated update", func(s *domain.WagerTransactionState) { s.UpdatedAt = s.CreatedAt.Add(-time.Second) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := transactionSnapshot(t, processed)
			tt.change(&state)
			if got, err := domain.RehydrateWagerTransaction(state); err == nil || got != nil {
				t.Fatalf("invalid stored state accepted: %v, %v", got, err)
			}
		})
	}
}

func TestTransactionRejectsUninitializedValues(t *testing.T) {
	for _, transaction := range []*domain.WagerTransaction{nil, {}} {
		_, snapshotErr := transaction.Snapshot()
		_, directionErr := transaction.MovementDirection(nil)
		for _, err := range []error{
			transaction.Validate(), snapshotErr, directionErr,
			transaction.Process(newWallet(t), nil, walletTime()),
			transaction.Reject(domain.FailureInsufficientFunds, walletTime()),
			transaction.WaitForReference(walletTime()), transaction.Fail(walletTime()),
		} {
			if !errors.Is(err, domain.ErrInvalidTransaction) {
				t.Errorf("error = %v, want ErrInvalidTransaction", err)
			}
		}
	}
}

func transactionInput(t *testing.T, kind domain.TransactionKind, units int64) domain.WagerTransactionInput {
	t.Helper()
	input := domain.WagerTransactionInput{
		ID: "transaction-1", ExternalTransactionID: "external-1", ProviderID: "provider-1",
		IdempotencyKey: "provider-1:external-1", PayloadHash: strings.Repeat("a", 64),
		WalletID: "wallet-1", PlayerID: "player-1", RoundID: "round-1", GameID: "game-1",
		Kind: kind, Money: moneyFromUnits(t, units),
	}
	if kind == domain.KindRefund || kind == domain.KindRollback {
		input.ReferenceExternalTransactionID = "external-original"
	}
	return input
}

func originalInput(t *testing.T, kind domain.TransactionKind, units int64) domain.WagerTransactionInput {
	t.Helper()
	input := transactionInput(t, kind, units)
	input.ID = "original-" + string(kind)
	input.ExternalTransactionID = "external-original"
	if kind == domain.KindRefund {
		input.ExternalTransactionID = "external-refund"
	}
	return input
}

func newTransaction(t *testing.T, input domain.WagerTransactionInput) *domain.WagerTransaction {
	t.Helper()
	transaction, err := domain.NewWagerTransaction(input, walletTime())
	if err != nil {
		t.Fatal(err)
	}
	return transaction
}

func transactionSnapshot(t *testing.T, transaction *domain.WagerTransaction) domain.WagerTransactionState {
	t.Helper()
	state, err := transaction.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func processTransaction(t *testing.T, input domain.WagerTransactionInput, reference *domain.WagerTransaction, initialUnits int64) (*domain.WagerTransaction, *domain.Wallet, domain.Money) {
	t.Helper()
	var transaction *domain.WagerTransaction
	var err error
	if input.Kind == domain.KindOpening {
		transaction, err = domain.NewOpeningTransaction(input.ID, input.WalletID, input.PlayerID, input.Money, walletTime())
	} else {
		transaction, err = domain.NewWagerTransaction(input, walletTime())
	}
	if err != nil {
		t.Fatal(err)
	}
	currency, err := input.Money.Currency()
	if err != nil {
		t.Fatal(err)
	}
	initial := walletMoney(t, initialUnits, currency)
	wallet, err := domain.NewWallet(input.WalletID, input.PlayerID, initial, walletTime())
	if err != nil {
		t.Fatal(err)
	}
	direction, err := transaction.MovementDirection(reference)
	if err != nil {
		t.Fatal(err)
	}
	processedAt := walletTime().Add(2 * time.Minute)
	if input.Kind != domain.KindOpening {
		switch direction {
		case domain.DirectionCredit:
			err = wallet.Credit(input.Money, processedAt)
		case domain.DirectionDebit:
			err = wallet.Debit(input.Money, processedAt)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := transaction.Process(wallet, reference, processedAt); err != nil {
		t.Fatal(err)
	}
	return transaction, wallet, initial
}
