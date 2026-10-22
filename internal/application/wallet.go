// Package application coordena os casos de uso e as dependências de persistência.
package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type WalletService struct{ service }

func NewWalletService(dependencies Dependencies) (*WalletService, error) {
	s, err := newService(dependencies)
	if err != nil {
		return nil, err
	}
	return &WalletService{service: s}, nil
}

type CreateWalletInput struct {
	PlayerID       string
	InitialBalance domain.Money
	Metadata       RequestMetadata
}

func (s *WalletService) Create(ctx context.Context, actor Actor, input CreateWalletInput) (domain.WalletState, error) {
	if err := actor.authorizeInternal(); err != nil {
		return domain.WalletState{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.WalletState{}, err
	}
	if !validText(input.PlayerID) {
		return domain.WalletState{}, ErrInvalidInput
	}
	if err := validateMetadata(input.Metadata); err != nil {
		return domain.WalletState{}, err
	}
	id, err := s.newID()
	if err != nil {
		return domain.WalletState{}, err
	}
	now := s.now()
	wallet, err := domain.NewWallet(id, input.PlayerID, input.InitialBalance, now)
	if err != nil {
		return domain.WalletState{}, err
	}
	state, err := wallet.Snapshot()
	if err != nil {
		return domain.WalletState{}, err
	}
	err = s.within(ctx, func(repositories Repositories) error {
		_, err := repositories.Wallets.GetByPlayerCurrency(ctx, input.PlayerID, state.Currency)
		if err == nil {
			return ErrConflict
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := repositories.Wallets.Insert(ctx, wallet); err != nil {
			return err
		}
		units, err := input.InitialBalance.MinorUnits()
		if err != nil {
			return err
		}
		if units == 0 {
			return nil
		}
		opening, err := domain.NewOpeningTransaction("opening:"+id, id, input.PlayerID, input.InitialBalance, now)
		if err != nil {
			return err
		}
		if err := opening.Process(wallet, nil, now); err != nil {
			return err
		}
		if err := repositories.Wagers.Insert(ctx, opening); err != nil {
			return err
		}
		zero, err := domain.ZeroMoney(state.Currency)
		if err != nil {
			return err
		}
		entry, err := domain.NewWalletLedgerEntry("ledger:opening:"+id, opening, zero, now)
		if err != nil {
			return err
		}
		if err := repositories.Ledger.Insert(ctx, entry); err != nil {
			return err
		}
		return s.appendTransactionEvents(ctx, repositories, opening, wallet, zero, input.Metadata)
	})
	if err != nil {
		return domain.WalletState{}, err
	}
	return state, nil
}

func (s *WalletService) Get(ctx context.Context, actor Actor, walletID string) (domain.WalletState, error) {
	if err := actor.authorizeInternal(); err != nil {
		return domain.WalletState{}, err
	}
	if !validText(walletID) {
		return domain.WalletState{}, ErrInvalidInput
	}
	var result domain.WalletState
	err := s.within(ctx, func(repositories Repositories) error {
		wallet, err := repositories.Wallets.Get(ctx, walletID)
		if err != nil {
			return err
		}
		result, err = wallet.Snapshot()
		return err
	})
	if err != nil {
		return domain.WalletState{}, err
	}
	return result, nil
}

type LedgerPage struct {
	Entries    []domain.LedgerEntryState
	NextCursor string
}

type ledgerCursor struct {
	Version   int       `json:"version"`
	WalletID  string    `json:"walletId"`
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

func (s *WalletService) ListLedger(ctx context.Context, actor Actor, walletID, cursor string, limit int) (LedgerPage, error) {
	if err := actor.authorizeInternal(); err != nil {
		return LedgerPage{}, err
	}
	if !validText(walletID) || limit < 0 || limit > 200 {
		return LedgerPage{}, ErrInvalidInput
	}
	if limit == 0 {
		limit = 50
	}
	position, err := decodeCursor(cursor, walletID)
	if err != nil {
		return LedgerPage{}, err
	}
	result := LedgerPage{Entries: make([]domain.LedgerEntryState, 0, limit)}
	err = s.within(ctx, func(repositories Repositories) error {
		if _, err := repositories.Wallets.Get(ctx, walletID); err != nil {
			return err
		}
		entries, err := repositories.Ledger.List(ctx, walletID, position, limit+1)
		if err != nil {
			return err
		}
		for i, entry := range entries {
			if i == limit {
				last := result.Entries[len(result.Entries)-1]
				payload, err := json.Marshal(ledgerCursor{Version: 1, WalletID: walletID, CreatedAt: last.CreatedAt, ID: last.ID})
				if err != nil {
					return err
				}
				result.NextCursor = base64.RawURLEncoding.EncodeToString(payload)
				break
			}
			state, err := entry.Snapshot()
			if err != nil {
				return err
			}
			result.Entries = append(result.Entries, state)
		}
		return nil
	})
	if err != nil {
		return LedgerPage{}, err
	}
	return result, nil
}

func decodeCursor(cursor, walletID string) (*LedgerPosition, error) {
	if cursor == "" {
		return nil, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("ledger cursor: %w", ErrInvalidInput)
	}
	var decoded ledgerCursor
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Version != 1 ||
		decoded.WalletID != walletID || decoded.CreatedAt.IsZero() || !validText(decoded.ID) {
		return nil, fmt.Errorf("ledger cursor: %w", ErrInvalidInput)
	}
	return &LedgerPosition{CreatedAt: decoded.CreatedAt.UTC(), ID: decoded.ID}, nil
}

type ReconciliationResult struct {
	WalletID          string
	StoredBalance     domain.Money
	CalculatedBalance domain.Money
	Difference        domain.Money
	Consistent        bool
	CheckedEntries    int64
}

func (s *WalletService) Reconcile(ctx context.Context, actor Actor, walletID string) (ReconciliationResult, error) {
	if err := actor.authorizeInternal(); err != nil {
		return ReconciliationResult{}, err
	}
	if !validText(walletID) {
		return ReconciliationResult{}, ErrInvalidInput
	}
	var result ReconciliationResult
	err := s.within(ctx, func(repositories Repositories) error {
		wallet, err := repositories.Wallets.Lock(ctx, walletID)
		if err != nil {
			return err
		}
		state, err := wallet.Snapshot()
		if err != nil {
			return err
		}
		calculated, count, err := repositories.Ledger.CalculateBalance(ctx, walletID, state.Currency)
		if err != nil {
			return err
		}
		difference, err := state.Balance.Subtract(calculated)
		if err != nil {
			return err
		}
		units, err := difference.MinorUnits()
		if err != nil {
			return err
		}
		result = ReconciliationResult{
			WalletID: walletID, StoredBalance: state.Balance, CalculatedBalance: calculated,
			Difference: difference, Consistent: units == 0, CheckedEntries: count,
		}
		return nil
	})
	if err != nil {
		return ReconciliationResult{}, err
	}
	if s.dependencies.Observer != nil {
		s.dependencies.Observer.Reconciliation(result.Consistent)
	}
	if !result.Consistent {
		s.dependencies.Logger.WarnContext(ctx, "wallet reconciliation mismatch", "walletId", walletID, "checkedEntries", result.CheckedEntries)
	}
	return result, nil
}
