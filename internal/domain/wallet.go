package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Wallet controla o saldo e a versão de uma carteira de jogador.
// Sua criação deve passar por NewWallet ou RehydrateWallet.
type Wallet struct {
	state WalletState
}

// WalletState é uma cópia dos dados da carteira, usada na leitura e reidratação.
// Alterar essa cópia não altera a entidade que a forneceu.
type WalletState struct {
	ID        string
	PlayerID  string
	Currency  string
	Balance   Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewWallet cria a carteira na versão 1, incluindo quando o saldo é positivo.
// O caso de uso fornecerá a identidade e o instante de criação.
func NewWallet(id, playerID string, initialBalance Money, createdAt time.Time) (*Wallet, error) {
	currency, err := initialBalance.Currency()
	if err != nil {
		return nil, err
	}
	wallet := &Wallet{state: WalletState{
		ID: id, PlayerID: playerID, Currency: currency, Balance: initialBalance,
		Version: 1, CreatedAt: createdAt.UTC(), UpdatedAt: createdAt.UTC(),
	}}
	if err := wallet.Validate(); err != nil {
		return nil, err
	}
	return wallet, nil
}

// RehydrateWallet restaura dados persistidos sem reaplicar crédito ou débito.
func RehydrateWallet(state WalletState) (*Wallet, error) {
	wallet := &Wallet{state: state}
	if err := wallet.Validate(); err != nil {
		return nil, err
	}
	wallet.state.CreatedAt = state.CreatedAt.UTC()
	wallet.state.UpdatedAt = state.UpdatedAt.UTC()
	return wallet, nil
}

// Validate verifica as invariantes, incluindo para uma entidade não inicializada.
func (w *Wallet) Validate() error {
	if w == nil {
		return ErrInvalidWallet
	}
	if w.state.ID == "" || strings.TrimSpace(w.state.ID) != w.state.ID {
		return fmt.Errorf("wallet ID must be non-empty and have no surrounding whitespace: %w", ErrInvalidWallet)
	}
	if w.state.PlayerID == "" || strings.TrimSpace(w.state.PlayerID) != w.state.PlayerID {
		return fmt.Errorf("player ID must be non-empty and have no surrounding whitespace: %w", ErrInvalidWallet)
	}
	if err := validateCurrency(w.state.Currency); err != nil {
		return err
	}
	currency, err := w.state.Balance.Currency()
	if err != nil {
		return err
	}
	if currency != w.state.Currency {
		return ErrCurrencyMismatch
	}
	units, err := w.state.Balance.MinorUnits()
	if err != nil {
		return err
	}
	if units < 0 {
		return ErrNegativeBalance
	}
	if w.state.Version < 1 {
		return fmt.Errorf("wallet version must be at least 1: %w", ErrInvalidWallet)
	}
	if w.state.CreatedAt.IsZero() || w.state.UpdatedAt.IsZero() || w.state.UpdatedAt.Before(w.state.CreatedAt) {
		return ErrInvalidWalletTime
	}
	return nil
}

// Snapshot devolve uma cópia do estado para leitura ou persistência.
func (w *Wallet) Snapshot() (WalletState, error) {
	if err := w.Validate(); err != nil {
		return WalletState{}, err
	}
	return w.state, nil
}

// Credit aumenta o saldo e a versão apenas depois de validar toda a operação.
func (w *Wallet) Credit(amount Money, occurredAt time.Time) error {
	if err := w.validateMovement(amount, occurredAt); err != nil {
		return err
	}
	balance, err := w.state.Balance.Add(amount)
	if err != nil {
		return err
	}
	w.applyBalance(balance, occurredAt)
	return nil
}

// Debit exige saldo suficiente e não permite que a carteira fique negativa.
func (w *Wallet) Debit(amount Money, occurredAt time.Time) error {
	if err := w.validateMovement(amount, occurredAt); err != nil {
		return err
	}
	comparison, err := w.state.Balance.Compare(amount)
	if err != nil {
		return err
	}
	if comparison < 0 {
		return ErrInsufficientFunds
	}
	balance, err := w.state.Balance.Subtract(amount)
	if err != nil {
		return err
	}
	w.applyBalance(balance, occurredAt)
	return nil
}

func (w *Wallet) validateMovement(amount Money, occurredAt time.Time) error {
	if err := w.Validate(); err != nil {
		return err
	}
	units, err := amount.MinorUnits()
	if err != nil {
		return err
	}
	if units <= 0 {
		return ErrInvalidMovement
	}
	currency, err := amount.Currency()
	if err != nil {
		return err
	}
	if currency != w.state.Currency {
		return ErrCurrencyMismatch
	}
	if occurredAt.IsZero() || occurredAt.Before(w.state.UpdatedAt) {
		return ErrInvalidWalletTime
	}
	if w.state.Version == math.MaxInt64 {
		return ErrWalletVersionOverflow
	}
	return nil
}

func (w *Wallet) applyBalance(balance Money, occurredAt time.Time) {
	w.state.Balance = balance
	w.state.Version++
	w.state.UpdatedAt = occurredAt.UTC()
}
