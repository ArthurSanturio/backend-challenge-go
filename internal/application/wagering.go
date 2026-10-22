package application

import (
	"context"
	"errors"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type WageringService struct{ service }

func NewWageringService(dependencies Dependencies) (*WageringService, error) {
	s, err := newService(dependencies)
	if err != nil {
		return nil, err
	}
	return &WageringService{service: s}, nil
}

type ProcessWagerInput struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	WalletID                       string
	PlayerID                       string
	RoundID                        string
	GameID                         string
	Kind                           domain.TransactionKind
	Money                          domain.Money
	ReferenceExternalTransactionID string
	Metadata                       RequestMetadata
}

type ProcessWagerResult struct {
	Transaction      domain.WagerTransactionState
	IdempotentReplay bool
}

func (s *WageringService) Process(ctx context.Context, actor Actor, input ProcessWagerInput) (ProcessWagerResult, error) {
	if err := actor.authorizeProvider(input.ProviderID, false); err != nil {
		return ProcessWagerResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProcessWagerResult{}, err
	}
	hash, err := hashWager(input)
	if err != nil {
		return ProcessWagerResult{}, err
	}
	var result ProcessWagerResult
	err = s.within(ctx, func(repositories Repositories) error {
		var err error
		result, err = s.process(ctx, repositories, input, hash)
		return err
	})
	if err != nil {
		return ProcessWagerResult{}, err
	}
	s.observeResult(result)
	return result, nil
}

// process usa os repositórios da transação do chamador, inclusive pela inbox.
func (s *WageringService) process(ctx context.Context, repositories Repositories, input ProcessWagerInput, hash string) (ProcessWagerResult, error) {
	replay, found, err := findReplay(ctx, repositories.Wagers, input, hash)
	if err != nil {
		return ProcessWagerResult{}, err
	}
	if found {
		return replay, nil
	}
	wallet, err := repositories.Wallets.Lock(ctx, input.WalletID)
	if err != nil {
		return ProcessWagerResult{}, err
	}
	// Outra transação pode ter sido confirmada enquanto aguardávamos a carteira.
	replay, found, err = findReplay(ctx, repositories.Wagers, input, hash)
	if err != nil {
		return ProcessWagerResult{}, err
	}
	if found {
		return replay, nil
	}
	id, err := s.newID()
	if err != nil {
		return ProcessWagerResult{}, err
	}
	transaction, err := domain.NewWagerTransaction(domain.WagerTransactionInput{
		ID: id, ProviderID: input.ProviderID, ExternalTransactionID: input.ExternalTransactionID,
		IdempotencyKey: input.IdempotencyKey, PayloadHash: hash,
		WalletID: input.WalletID, PlayerID: input.PlayerID, RoundID: input.RoundID, GameID: input.GameID,
		Kind: input.Kind, Money: input.Money, ReferenceExternalTransactionID: input.ReferenceExternalTransactionID,
	}, s.now())
	if err != nil {
		return ProcessWagerResult{}, err
	}
	if err := s.execute(ctx, repositories, transaction, wallet, true, input.Metadata, false); err != nil {
		return ProcessWagerResult{}, err
	}
	state, err := transaction.Snapshot()
	return ProcessWagerResult{Transaction: state}, err
}

func findReplay(ctx context.Context, repository WagerRepository, input ProcessWagerInput, hash string) (ProcessWagerResult, bool, error) {
	transaction, err := repository.GetByIdempotencyKey(ctx, input.ProviderID, input.IdempotencyKey)
	if errors.Is(err, ErrNotFound) {
		transaction, err = repository.GetByExternalID(ctx, input.ProviderID, input.ExternalTransactionID)
	}
	if errors.Is(err, ErrNotFound) {
		return ProcessWagerResult{}, false, nil
	}
	if err != nil {
		return ProcessWagerResult{}, false, err
	}
	state, err := transaction.Snapshot()
	if err != nil {
		return ProcessWagerResult{}, false, err
	}
	if state.ProviderID != input.ProviderID || state.IdempotencyKey != input.IdempotencyKey || state.PayloadHash != hash {
		return ProcessWagerResult{}, false, ErrConflict
	}
	return ProcessWagerResult{Transaction: state, IdempotentReplay: true}, true, nil
}

// Resume retoma uma operação pendente, inclusive quando chamado pelos workers.
// Apenas o serviço interno pode chamá-lo; replays externos somente consultam o resultado.
func (s *WageringService) Resume(ctx context.Context, actor Actor, transactionID string, metadata RequestMetadata) (ProcessWagerResult, error) {
	return s.resume(ctx, actor, transactionID, metadata, false)
}

// ResumeFinalReferenceAttempt ainda permite processar uma referência disponível.
// Se continuar indisponível, conclui como rejeição de referência não encontrada.
func (s *WageringService) ResumeFinalReferenceAttempt(ctx context.Context, actor Actor, transactionID string, metadata RequestMetadata) (ProcessWagerResult, error) {
	return s.resume(ctx, actor, transactionID, metadata, true)
}
func (s *WageringService) resume(ctx context.Context, actor Actor, transactionID string, metadata RequestMetadata, finalReferenceAttempt bool) (ProcessWagerResult, error) {
	if err := actor.authorizeInternal(); err != nil {
		return ProcessWagerResult{}, err
	}
	if !validText(transactionID) {
		return ProcessWagerResult{}, ErrInvalidInput
	}
	if err := validateMetadata(metadata); err != nil {
		return ProcessWagerResult{}, err
	}
	var result ProcessWagerResult
	err := s.within(ctx, func(repositories Repositories) error {
		transaction, err := repositories.Wagers.Get(ctx, transactionID)
		if err != nil {
			return err
		}
		state, err := transaction.Snapshot()
		if err != nil {
			return err
		}
		if isTerminal(state.Status) {
			result = ProcessWagerResult{Transaction: state, IdempotentReplay: true}
			return nil
		}
		if state.Kind == domain.KindOpening {
			return ErrInvalidInput
		}
		wallet, err := repositories.Wallets.Lock(ctx, state.WalletID)
		if err != nil {
			return err
		}
		transaction, err = repositories.Wagers.Get(ctx, transactionID)
		if err != nil {
			return err
		}
		state, err = transaction.Snapshot()
		if err != nil {
			return err
		}
		if isTerminal(state.Status) {
			result = ProcessWagerResult{Transaction: state, IdempotentReplay: true}
			return nil
		}
		if err := s.execute(ctx, repositories, transaction, wallet, false, metadata, finalReferenceAttempt); err != nil {
			return err
		}
		result.Transaction, err = transaction.Snapshot()
		return err
	})
	if err != nil {
		return ProcessWagerResult{}, err
	}
	s.observeResult(result)
	return result, nil
}

func (s *WageringService) execute(ctx context.Context, repositories Repositories, transaction *domain.WagerTransaction, wallet *domain.Wallet, isNew bool, metadata RequestMetadata, finalReferenceAttempt bool) error {
	now := s.now()
	state, err := transaction.Snapshot()
	if err != nil {
		return err
	}
	reject := func(code domain.FailureCode) error {
		if err := transaction.Reject(code, now); err != nil {
			return err
		}
		if err := saveWager(ctx, repositories.Wagers, transaction, isNew); err != nil {
			return err
		}
		return s.appendTransactionEvents(ctx, repositories, transaction, wallet, domain.Money{}, metadata)
	}
	if err := transaction.ValidateWallet(wallet); err != nil {
		return reject(domain.FailureInvalidOperation)
	}
	var reference *domain.WagerTransaction
	if state.ReferenceExternalTransactionID != "" {
		if state.Status == domain.StatusPendingReference && !now.Before(state.CreatedAt.Add(s.dependencies.ReferenceTTL)) {
			return reject(domain.FailureReferenceNotFound)
		}
		reference, err = repositories.Wagers.GetByExternalID(ctx, state.ProviderID, state.ReferenceExternalTransactionID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		waiting := errors.Is(err, ErrNotFound)
		if !waiting {
			referenceState, err := reference.Snapshot()
			if err != nil {
				return err
			}
			waiting = referenceState.Status == domain.StatusPending || referenceState.Status == domain.StatusPendingReference
			if !waiting && referenceState.Status != domain.StatusProcessed {
				return reject(domain.FailureReferenceNotProcessed)
			}
		}
		if waiting {
			if finalReferenceAttempt {
				return reject(domain.FailureReferenceNotFound)
			}
			if state.Status == domain.StatusPendingReference {
				return nil
			}
			if err := transaction.WaitForReference(now); err != nil {
				return err
			}
			if err := saveWager(ctx, repositories.Wagers, transaction, isNew); err != nil {
				return err
			}
			return s.appendTransactionEvents(ctx, repositories, transaction, wallet, domain.Money{}, metadata)
		}
	}
	direction, err := transaction.MovementDirection(reference)
	if err != nil {
		return reject(domain.FailureReferenceMismatch)
	}
	if state.Kind == domain.KindRefund || state.Kind == domain.KindRollback {
		referenceState, err := reference.Snapshot()
		if err != nil {
			return err
		}
		reversed, err := repositories.Wagers.HasProcessedReversal(ctx, referenceState.ID)
		if err != nil {
			return err
		}
		if reversed {
			return reject(domain.FailureAlreadyReversed)
		}
	}
	before, err := wallet.Snapshot()
	if err != nil {
		return err
	}
	switch direction {
	case domain.DirectionCredit:
		err = wallet.Credit(state.Money, now)
	case domain.DirectionDebit:
		err = wallet.Debit(state.Money, now)
	}
	if errors.Is(err, domain.ErrInsufficientFunds) {
		if state.Kind == domain.KindRollback {
			return reject(domain.FailureReversalInsufficientFunds)
		}
		return reject(domain.FailureInsufficientFunds)
	}
	if errors.Is(err, domain.ErrMoneyOverflow) || errors.Is(err, domain.ErrWalletVersionOverflow) {
		return reject(domain.FailureInvalidOperation)
	}
	if err != nil {
		return err
	}
	if err := transaction.Process(wallet, reference, now); err != nil {
		return err
	}
	if direction != "" {
		if err := repositories.Wallets.Update(ctx, wallet); err != nil {
			return err
		}
	}
	if err := saveWager(ctx, repositories.Wagers, transaction, isNew); err != nil {
		return err
	}
	if direction != "" {
		id, err := s.newID()
		if err != nil {
			return err
		}
		entry, err := domain.NewWalletLedgerEntry(id, transaction, before.Balance, now)
		if err != nil {
			return err
		}
		if err := repositories.Ledger.Insert(ctx, entry); err != nil {
			return err
		}
	}
	return s.appendTransactionEvents(ctx, repositories, transaction, wallet, before.Balance, metadata)
}

func saveWager(ctx context.Context, repository WagerRepository, transaction *domain.WagerTransaction, isNew bool) error {
	if isNew {
		return repository.Insert(ctx, transaction)
	}
	return repository.Update(ctx, transaction)
}

func isTerminal(status domain.TransactionStatus) bool {
	return status == domain.StatusProcessed || status == domain.StatusRejected || status == domain.StatusFailed
}

func (s *WageringService) Get(ctx context.Context, actor Actor, transactionID string) (domain.WagerTransactionState, error) {
	if err := actor.authorizeProvider(actor.providerID, true); err != nil {
		return domain.WagerTransactionState{}, err
	}
	if !validText(transactionID) {
		return domain.WagerTransactionState{}, ErrInvalidInput
	}
	var result domain.WagerTransactionState
	err := s.within(ctx, func(repositories Repositories) error {
		transaction, err := repositories.Wagers.Get(ctx, transactionID)
		if err != nil {
			return err
		}
		state, err := transaction.Snapshot()
		if err != nil {
			return err
		}
		if err := actor.authorizeProvider(state.ProviderID, true); err != nil {
			return err
		}
		result = state
		return nil
	})
	if err != nil {
		return domain.WagerTransactionState{}, err
	}
	return result, nil
}

func (s *WageringService) GetByExternalID(ctx context.Context, actor Actor, providerID, externalID string) (domain.WagerTransactionState, error) {
	if err := actor.authorizeProvider(providerID, true); err != nil {
		return domain.WagerTransactionState{}, err
	}
	if !validText(providerID) || !validText(externalID) {
		return domain.WagerTransactionState{}, ErrInvalidInput
	}
	var result domain.WagerTransactionState
	err := s.within(ctx, func(repositories Repositories) error {
		transaction, err := repositories.Wagers.GetByExternalID(ctx, providerID, externalID)
		if err != nil {
			return err
		}
		state, err := transaction.Snapshot()
		if err != nil {
			return err
		}
		if state.ProviderID != providerID || state.ExternalTransactionID != externalID {
			return ErrNotFound
		}
		result = state
		return nil
	})
	if err != nil {
		return domain.WagerTransactionState{}, err
	}
	return result, nil
}
