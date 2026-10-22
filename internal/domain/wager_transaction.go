package domain

import (
	"fmt"
	"strings"
	"time"
)

type TransactionKind string

const (
	KindOpening  TransactionKind = "OPENING"
	KindBet      TransactionKind = "BET"
	KindWin      TransactionKind = "WIN"
	KindLoss     TransactionKind = "LOSS"
	KindRefund   TransactionKind = "REFUND"
	KindRollback TransactionKind = "ROLLBACK"
)

type TransactionStatus string

const (
	StatusPending          TransactionStatus = "PENDING"
	StatusPendingReference TransactionStatus = "PENDING_REFERENCE"
	StatusProcessed        TransactionStatus = "PROCESSED"
	StatusRejected         TransactionStatus = "REJECTED"
	StatusFailed           TransactionStatus = "FAILED"
)

type FailureCode string

const (
	FailureInsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	FailureReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"
	FailureReferenceNotProcessed     FailureCode = "REFERENCE_NOT_PROCESSED"
	FailureReferenceMismatch         FailureCode = "REFERENCE_MISMATCH"
	FailureAlreadyReversed           FailureCode = "ALREADY_REVERSED"
	FailureInvalidOperation          FailureCode = "INVALID_OPERATION"
	FailurePermanentInfrastructure   FailureCode = "PERMANENT_INFRASTRUCTURE_FAILURE"
)

// WagerTransactionInput contém os dados imutáveis de uma operação.
// OPENING usa o construtor interno, sem os metadados externos.
type WagerTransactionInput struct {
	ID                             string
	ExternalTransactionID          string
	ProviderID                     string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           TransactionKind
	Money                          Money
	ReferenceExternalTransactionID string
}

// WagerTransactionState é uma cópia dos dados usados na leitura e reidratação.
// ResultBalance preserva o saldo original para o futuro replay idempotente.
type WagerTransactionState struct {
	WagerTransactionInput
	Status                 TransactionStatus
	ReferenceTransactionID string
	Direction              LedgerDirection
	ResultBalance          Money
	FailureCode            FailureCode
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type WagerTransaction struct {
	state WagerTransactionState
}

// NewWagerTransaction aceita apenas os cinco tipos externos, sempre em PENDING.
func NewWagerTransaction(input WagerTransactionInput, createdAt time.Time) (*WagerTransaction, error) {
	if input.Kind == KindOpening {
		return nil, fmt.Errorf("OPENING requires the internal constructor: %w", ErrInvalidTransaction)
	}
	return RehydrateWagerTransaction(WagerTransactionState{
		WagerTransactionInput: input, Status: StatusPending,
		CreatedAt: createdAt.UTC(), UpdatedAt: createdAt.UTC(),
	})
}

// NewOpeningTransaction cria a identidade interna da abertura na versão pendente.
// O caso de uso concluirá a operação junto com carteira e ledger.
func NewOpeningTransaction(id, walletID, playerID string, amount Money, createdAt time.Time) (*WagerTransaction, error) {
	return RehydrateWagerTransaction(WagerTransactionState{
		WagerTransactionInput: WagerTransactionInput{
			ID: id, WalletID: walletID, PlayerID: playerID, Kind: KindOpening, Money: amount,
		},
		Status: StatusPending, CreatedAt: createdAt.UTC(), UpdatedAt: createdAt.UTC(),
	})
}

// RehydrateWagerTransaction valida o estado sem reaplicar transições ou dinheiro.
func RehydrateWagerTransaction(state WagerTransactionState) (*WagerTransaction, error) {
	transaction := &WagerTransaction{state: state}
	if err := transaction.Validate(); err != nil {
		return nil, err
	}
	transaction.state.CreatedAt = state.CreatedAt.UTC()
	transaction.state.UpdatedAt = state.UpdatedAt.UTC()
	return transaction, nil
}

func (t *WagerTransaction) Validate() error {
	if t == nil {
		return ErrInvalidTransaction
	}
	s := t.state
	if !validIdentifier(s.ID) || !validIdentifier(s.WalletID) || !validIdentifier(s.PlayerID) {
		return ErrInvalidTransaction
	}
	units, err := s.Money.MinorUnits()
	if err != nil {
		return err
	}
	switch s.Kind {
	case KindLoss:
		if units != 0 {
			return ErrInvalidTransactionAmount
		}
	case KindOpening, KindBet, KindWin, KindRefund, KindRollback:
		if units <= 0 {
			return ErrInvalidTransactionAmount
		}
	default:
		return ErrInvalidTransaction
	}
	if s.Kind == KindOpening {
		if s.ProviderID != "" || s.ExternalTransactionID != "" || s.IdempotencyKey != "" ||
			s.PayloadHash != "" || s.RoundID != "" || s.GameID != "" || s.ReferenceExternalTransactionID != "" {
			return ErrInvalidTransaction
		}
	} else {
		for _, value := range []string{s.ProviderID, s.ExternalTransactionID, s.IdempotencyKey, s.PayloadHash, s.RoundID, s.GameID} {
			if !validIdentifier(value) {
				return ErrInvalidTransaction
			}
		}
	}
	if s.ReferenceExternalTransactionID != "" {
		if !validIdentifier(s.ReferenceExternalTransactionID) || s.ReferenceExternalTransactionID == s.ExternalTransactionID ||
			(s.Kind != KindWin && s.Kind != KindRefund && s.Kind != KindRollback) {
			return ErrInvalidReference
		}
	} else if s.Kind == KindRefund || s.Kind == KindRollback {
		return ErrInvalidReference
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		return ErrInvalidTransactionTime
	}
	if s.Status == StatusProcessed {
		return t.validateProcessedState()
	}
	if s.ReferenceTransactionID != "" || s.Direction != "" || s.ResultBalance != (Money{}) {
		return ErrInvalidTransaction
	}
	switch s.Status {
	case StatusPending:
		if s.FailureCode != "" {
			return ErrInvalidFailureCode
		}
	case StatusPendingReference:
		if s.ReferenceExternalTransactionID == "" {
			return ErrInvalidReference
		}
		if s.FailureCode != "" {
			return ErrInvalidFailureCode
		}
	case StatusRejected:
		if !validRejectionCode(s.Kind, s.ReferenceExternalTransactionID != "", s.FailureCode) {
			return ErrInvalidFailureCode
		}
	case StatusFailed:
		if s.FailureCode != FailurePermanentInfrastructure {
			return ErrInvalidFailureCode
		}
	default:
		return ErrInvalidTransaction
	}
	return nil
}

func (t *WagerTransaction) validateProcessedState() error {
	s := t.state
	if s.FailureCode != "" {
		return ErrInvalidFailureCode
	}
	units, err := s.ResultBalance.MinorUnits()
	if err != nil {
		return err
	}
	if units < 0 {
		return ErrNegativeBalance
	}
	if _, err := s.ResultBalance.Compare(s.Money); err != nil {
		return err
	}
	if s.ReferenceExternalTransactionID != "" {
		if !validIdentifier(s.ReferenceTransactionID) || s.ReferenceTransactionID == s.ID {
			return ErrInvalidReference
		}
	} else if s.ReferenceTransactionID != "" {
		return ErrInvalidReference
	}
	switch s.Kind {
	case KindOpening:
		if s.Direction != DirectionCredit || s.ResultBalance != s.Money {
			return ErrInvalidTransaction
		}
	case KindBet:
		if s.Direction != DirectionDebit {
			return ErrInvalidTransaction
		}
	case KindWin, KindRefund:
		if s.Direction != DirectionCredit {
			return ErrInvalidTransaction
		}
	case KindLoss:
		if s.Direction != "" {
			return ErrInvalidTransaction
		}
	case KindRollback:
		if s.Direction != DirectionCredit && s.Direction != DirectionDebit {
			return ErrInvalidTransaction
		}
	}
	return nil
}

func (t *WagerTransaction) Snapshot() (WagerTransactionState, error) {
	if err := t.Validate(); err != nil {
		return WagerTransactionState{}, err
	}
	return t.state, nil
}

// ValidateWallet inclui a validação da moeda mesmo em LOSS, sem alterar a carteira.
func (t *WagerTransaction) ValidateWallet(wallet *Wallet) error {
	if err := t.Validate(); err != nil {
		return err
	}
	state, err := wallet.Snapshot()
	if err != nil {
		return err
	}
	if state.ID != t.state.WalletID || state.PlayerID != t.state.PlayerID {
		return ErrInvalidTransaction
	}
	if _, err := state.Balance.Compare(t.state.Money); err != nil {
		return err
	}
	return nil
}

// MovementDirection indica a movimentação a aplicar antes da conclusão.
// LOSS retorna direção vazia; referências precisam ser carregadas pelo caso de uso.
func (t *WagerTransaction) MovementDirection(reference *WagerTransaction) (LedgerDirection, error) {
	if err := t.ensurePending(); err != nil {
		return "", err
	}
	if err := t.validateReference(reference); err != nil {
		return "", err
	}
	switch t.state.Kind {
	case KindOpening, KindWin, KindRefund:
		return DirectionCredit, nil
	case KindBet:
		return DirectionDebit, nil
	case KindLoss:
		return "", nil
	case KindRollback:
		if reference.state.Kind == KindBet {
			return DirectionCredit, nil
		}
		return DirectionDebit, nil
	default:
		return "", ErrInvalidTransaction
	}
}

func (t *WagerTransaction) validateReference(reference *WagerTransaction) error {
	if t.state.ReferenceExternalTransactionID == "" {
		if reference != nil {
			return ErrInvalidReference
		}
		return nil
	}
	if reference == nil {
		return ErrInvalidReference
	}
	if err := reference.Validate(); err != nil {
		return err
	}
	r, s := reference.state, t.state
	if r.Status != StatusProcessed {
		return ErrReferenceNotProcessed
	}
	if r.ID == s.ID || r.ExternalTransactionID != s.ReferenceExternalTransactionID ||
		r.ProviderID != s.ProviderID || r.PlayerID != s.PlayerID || r.WalletID != s.WalletID || r.RoundID != s.RoundID {
		return ErrReferenceMismatch
	}
	comparison, err := s.Money.Compare(r.Money)
	if err != nil {
		return err
	}
	switch s.Kind {
	case KindWin:
		if r.Kind != KindBet {
			return ErrInvalidReference
		}
	case KindRefund:
		if r.Kind != KindBet || comparison != 0 {
			return ErrInvalidReference
		}
	case KindRollback:
		if (r.Kind != KindBet && r.Kind != KindWin && r.Kind != KindRefund) || comparison != 0 {
			return ErrInvalidReference
		}
	}
	return nil
}

// Process registra o saldo da carteira após a aplicação da operação pelo caso de uso.
// Não debita nem credita a carteira e não substitui a futura transação SQL.
func (t *WagerTransaction) Process(wallet *Wallet, reference *WagerTransaction, processedAt time.Time) error {
	direction, err := t.MovementDirection(reference)
	if err != nil {
		return err
	}
	if err := t.ValidateWallet(wallet); err != nil {
		return err
	}
	state, err := wallet.Snapshot()
	if err != nil {
		return err
	}
	if processedAt.Before(state.UpdatedAt) || (reference != nil && processedAt.Before(reference.state.UpdatedAt)) {
		return ErrInvalidTransactionTime
	}
	next := t.state
	next.Status, next.Direction, next.ResultBalance = StatusProcessed, direction, state.Balance
	if reference != nil {
		next.ReferenceTransactionID = reference.state.ID
	}
	return t.applyState(next, processedAt)
}

func (t *WagerTransaction) WaitForReference(occurredAt time.Time) error {
	if err := t.ensurePending(); err != nil {
		return err
	}
	if t.state.Status != StatusPending || t.state.ReferenceExternalTransactionID == "" {
		return ErrInvalidTransactionTransition
	}
	next := t.state
	next.Status = StatusPendingReference
	return t.applyState(next, occurredAt)
}

func (t *WagerTransaction) Reject(code FailureCode, occurredAt time.Time) error {
	if err := t.ensurePending(); err != nil {
		return err
	}
	next := t.state
	next.Status, next.FailureCode = StatusRejected, code
	return t.applyState(next, occurredAt)
}

// Fail registra apenas falha permanente comprovada de uma operação ainda pendente.
// Os adaptadores atuais não identificam tal causa e não chamam este método.
// Indisponibilidade, resultado de commit incerto e falha de publicação permitem
// recuperação; limites de tentativas e DLQ também não autorizam esta transição.
func (t *WagerTransaction) Fail(occurredAt time.Time) error {
	if err := t.ensurePending(); err != nil {
		return err
	}
	next := t.state
	next.Status, next.FailureCode = StatusFailed, FailurePermanentInfrastructure
	return t.applyState(next, occurredAt)
}

func (t *WagerTransaction) ensurePending() error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.state.Status != StatusPending && t.state.Status != StatusPendingReference {
		return ErrInvalidTransactionTransition
	}
	return nil
}

func (t *WagerTransaction) applyState(next WagerTransactionState, occurredAt time.Time) error {
	if occurredAt.IsZero() || occurredAt.Before(t.state.UpdatedAt) {
		return ErrInvalidTransactionTime
	}
	next.UpdatedAt = occurredAt.UTC()
	candidate := &WagerTransaction{state: next}
	if err := candidate.Validate(); err != nil {
		return err
	}
	t.state = next
	return nil
}

func validIdentifier(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func validRejectionCode(kind TransactionKind, hasReference bool, code FailureCode) bool {
	switch code {
	case FailureInsufficientFunds:
		return kind == KindBet
	case FailureReversalInsufficientFunds:
		return kind == KindRollback
	case FailureReferenceNotFound, FailureReferenceNotProcessed, FailureReferenceMismatch:
		return hasReference
	case FailureAlreadyReversed:
		return kind == KindRefund || kind == KindRollback
	case FailureInvalidOperation:
		return kind != KindOpening
	default:
		return false
	}
}
