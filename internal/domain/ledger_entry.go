package domain

import "time"

type LedgerDirection string

const (
	DirectionDebit  LedgerDirection = "DEBIT"
	DirectionCredit LedgerDirection = "CREDIT"
)

type LedgerEntryState struct {
	ID            string
	WalletID      string
	TransactionID string
	Direction     LedgerDirection
	Money         Money
	BalanceBefore Money
	BalanceAfter  Money
	CreatedAt     time.Time
}

// WalletLedgerEntry é um lançamento imutável, sem métodos de alteração.
type WalletLedgerEntry struct {
	state LedgerEntryState
}

// NewWalletLedgerEntry usa uma transação processada e seu saldo final original.
// O saldo anterior é fornecido pelo caso de uso que aplicou a movimentação.
func NewWalletLedgerEntry(id string, transaction *WagerTransaction, balanceBefore Money, createdAt time.Time) (WalletLedgerEntry, error) {
	state, err := transaction.Snapshot()
	if err != nil {
		return WalletLedgerEntry{}, err
	}
	if state.Status != StatusProcessed {
		return WalletLedgerEntry{}, ErrInvalidLedgerEntry
	}
	if state.Kind == KindLoss {
		return WalletLedgerEntry{}, ErrNoLedgerMovement
	}
	if createdAt.IsZero() || createdAt.Before(state.UpdatedAt) {
		return WalletLedgerEntry{}, ErrInvalidLedgerTime
	}
	return RehydrateWalletLedgerEntry(LedgerEntryState{
		ID: id, WalletID: state.WalletID, TransactionID: state.ID,
		Direction: state.Direction, Money: state.Money,
		BalanceBefore: balanceBefore, BalanceAfter: state.ResultBalance, CreatedAt: createdAt,
	})
}

// RehydrateWalletLedgerEntry valida o registro sem movimentar a carteira.
func RehydrateWalletLedgerEntry(state LedgerEntryState) (WalletLedgerEntry, error) {
	entry := WalletLedgerEntry{state: state}
	if err := entry.Validate(); err != nil {
		return WalletLedgerEntry{}, err
	}
	entry.state.CreatedAt = state.CreatedAt.UTC()
	return entry, nil
}

func (e WalletLedgerEntry) Validate() error {
	s := e.state
	if !validIdentifier(s.ID) || !validIdentifier(s.WalletID) || !validIdentifier(s.TransactionID) {
		return ErrInvalidLedgerEntry
	}
	if s.CreatedAt.IsZero() {
		return ErrInvalidLedgerTime
	}
	units, err := s.Money.MinorUnits()
	if err != nil {
		return err
	}
	if units <= 0 {
		return ErrInvalidMovement
	}
	for _, balance := range []Money{s.BalanceBefore, s.BalanceAfter} {
		units, err := balance.MinorUnits()
		if err != nil {
			return err
		}
		if units < 0 {
			return ErrNegativeBalance
		}
	}
	var expected Money
	switch s.Direction {
	case DirectionCredit:
		expected, err = s.BalanceBefore.Add(s.Money)
	case DirectionDebit:
		expected, err = s.BalanceBefore.Subtract(s.Money)
	default:
		return ErrInvalidLedgerEntry
	}
	if err != nil {
		return err
	}
	comparison, err := expected.Compare(s.BalanceAfter)
	if err != nil {
		return err
	}
	if comparison != 0 {
		return ErrInvalidLedgerEntry
	}
	return nil
}

func (e WalletLedgerEntry) Snapshot() (LedgerEntryState, error) {
	if err := e.Validate(); err != nil {
		return LedgerEntryState{}, err
	}
	return e.state, nil
}
