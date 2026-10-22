package application_test

// Estes adaptadores existem somente nos testes unitários dos casos de uso.
// Não implementam persistência real nem substituem os futuros testes PostgreSQL.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

var errStorage = errors.New("injected storage failure")

type memoryData struct {
	wallets map[string]domain.WalletState
	wagers  map[string]domain.WagerTransactionState
	ledger  []domain.LedgerEntryState
	outbox  []application.OutboxRecord
}

func (d memoryData) clone() memoryData {
	copy := memoryData{
		wallets: make(map[string]domain.WalletState), wagers: make(map[string]domain.WagerTransactionState),
		ledger: append([]domain.LedgerEntryState(nil), d.ledger...),
		outbox: append([]application.OutboxRecord(nil), d.outbox...),
	}
	for id, state := range d.wallets {
		copy.wallets[id] = state
	}
	for id, state := range d.wagers {
		copy.wagers[id] = state
	}
	return copy
}

type memoryStore struct {
	data     memoryData
	failAt   string
	calls    int
	cancelAt string
	cancel   context.CancelFunc
}

func newMemoryStore() *memoryStore {
	return &memoryStore{data: memoryData{
		wallets: make(map[string]domain.WalletState), wagers: make(map[string]domain.WagerTransactionState),
	}}
}

type memoryTx struct {
	store *memoryStore
	data  memoryData
	steps map[string]int
}

func (s *memoryStore) WithinTransaction(ctx context.Context, action func(application.Repositories) error) error {
	s.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := &memoryTx{store: s, data: s.data.clone(), steps: make(map[string]int)}
	if err := action(application.Repositories{
		Wallets: walletRepository{tx}, Wagers: wagerRepository{tx},
		Ledger: ledgerRepository{tx}, Outbox: outboxRepository{tx},
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failAt == "commit" {
		return errStorage
	}
	s.data = tx.data
	return nil
}

func (t *memoryTx) step(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.steps[name]++
	if t.store.failAt == name || t.store.failAt == fmt.Sprintf("%s.%d", name, t.steps[name]) {
		return errStorage
	}
	if (t.store.cancelAt == name || t.store.cancelAt == fmt.Sprintf("%s.%d", name, t.steps[name])) && t.store.cancel != nil {
		t.store.cancel()
	}
	return nil
}

type walletRepository struct{ tx *memoryTx }

func (r walletRepository) Get(ctx context.Context, id string) (*domain.Wallet, error) {
	if err := r.tx.step(ctx, "wallet.get"); err != nil {
		return nil, err
	}
	return r.get(id)
}

func (r walletRepository) get(id string) (*domain.Wallet, error) {
	state, exists := r.tx.data.wallets[id]
	if !exists {
		return nil, application.ErrNotFound
	}
	return domain.RehydrateWallet(state)
}

func (r walletRepository) Lock(ctx context.Context, id string) (*domain.Wallet, error) {
	if err := r.tx.step(ctx, "wallet.lock"); err != nil {
		return nil, err
	}
	return r.get(id)
}

func (r walletRepository) GetByPlayerCurrency(ctx context.Context, playerID, currency string) (*domain.Wallet, error) {
	if err := r.tx.step(ctx, "wallet.find"); err != nil {
		return nil, err
	}
	for _, state := range r.tx.data.wallets {
		if state.PlayerID == playerID && state.Currency == currency {
			return domain.RehydrateWallet(state)
		}
	}
	return nil, application.ErrNotFound
}

func (r walletRepository) Insert(ctx context.Context, wallet *domain.Wallet) error {
	if err := r.tx.step(ctx, "wallet.insert"); err != nil {
		return err
	}
	state, err := wallet.Snapshot()
	if err != nil {
		return err
	}
	for _, stored := range r.tx.data.wallets {
		if stored.ID == state.ID || (stored.PlayerID == state.PlayerID && stored.Currency == state.Currency) {
			return application.ErrConflict
		}
	}
	r.tx.data.wallets[state.ID] = state
	return nil
}

func (r walletRepository) Update(ctx context.Context, wallet *domain.Wallet) error {
	if err := r.tx.step(ctx, "wallet.update"); err != nil {
		return err
	}
	state, err := wallet.Snapshot()
	if err != nil {
		return err
	}
	previous, exists := r.tx.data.wallets[state.ID]
	if !exists {
		return application.ErrNotFound
	}
	if state.Version != previous.Version+1 {
		return application.ErrConflict
	}
	r.tx.data.wallets[state.ID] = state
	return nil
}

type wagerRepository struct{ tx *memoryTx }

func (r wagerRepository) Get(ctx context.Context, id string) (*domain.WagerTransaction, error) {
	if err := r.tx.step(ctx, "wager.get"); err != nil {
		return nil, err
	}
	state, exists := r.tx.data.wagers[id]
	if !exists {
		return nil, application.ErrNotFound
	}
	return domain.RehydrateWagerTransaction(state)
}

func (r wagerRepository) GetByExternalID(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	if err := r.tx.step(ctx, "wager.external"); err != nil {
		return nil, err
	}
	for _, state := range r.tx.data.wagers {
		if state.ProviderID == providerID && state.ExternalTransactionID == externalID {
			return domain.RehydrateWagerTransaction(state)
		}
	}
	return nil, application.ErrNotFound
}

func (r wagerRepository) GetByIdempotencyKey(ctx context.Context, providerID, key string) (*domain.WagerTransaction, error) {
	if err := r.tx.step(ctx, "wager.key"); err != nil {
		return nil, err
	}
	for _, state := range r.tx.data.wagers {
		if state.ProviderID == providerID && state.IdempotencyKey == key {
			return domain.RehydrateWagerTransaction(state)
		}
	}
	return nil, application.ErrNotFound
}

func (r wagerRepository) HasProcessedReversal(ctx context.Context, referenceID string) (bool, error) {
	if err := r.tx.step(ctx, "wager.reversals"); err != nil {
		return false, err
	}
	for _, state := range r.tx.data.wagers {
		if state.Status == domain.StatusProcessed && state.ReferenceTransactionID == referenceID &&
			(state.Kind == domain.KindRefund || state.Kind == domain.KindRollback) {
			return true, nil
		}
	}
	return false, nil
}

func (r wagerRepository) Insert(ctx context.Context, transaction *domain.WagerTransaction) error {
	if err := r.tx.step(ctx, "wager.insert"); err != nil {
		return err
	}
	state, err := transaction.Snapshot()
	if err != nil {
		return err
	}
	if _, exists := r.tx.data.wallets[state.WalletID]; !exists {
		return application.ErrNotFound
	}
	for _, previous := range r.tx.data.wagers {
		if previous.ID == state.ID {
			return application.ErrConflict
		}
		if state.Kind == domain.KindOpening {
			if previous.Kind == domain.KindOpening && previous.WalletID == state.WalletID {
				return application.ErrConflict
			}
		} else if previous.Kind != domain.KindOpening && previous.ProviderID == state.ProviderID &&
			(previous.ExternalTransactionID == state.ExternalTransactionID || previous.IdempotencyKey == state.IdempotencyKey) {
			return application.ErrConflict
		}
	}
	r.tx.data.wagers[state.ID] = state
	return nil
}

func (r wagerRepository) Update(ctx context.Context, transaction *domain.WagerTransaction) error {
	if err := r.tx.step(ctx, "wager.update"); err != nil {
		return err
	}
	state, err := transaction.Snapshot()
	if err != nil {
		return err
	}
	if _, exists := r.tx.data.wagers[state.ID]; !exists {
		return application.ErrNotFound
	}
	r.tx.data.wagers[state.ID] = state
	return nil
}

type ledgerRepository struct{ tx *memoryTx }

func (r ledgerRepository) Insert(ctx context.Context, entry domain.WalletLedgerEntry) error {
	if err := r.tx.step(ctx, "ledger.insert"); err != nil {
		return err
	}
	state, err := entry.Snapshot()
	if err != nil {
		return err
	}
	if _, exists := r.tx.data.wagers[state.TransactionID]; !exists {
		return application.ErrNotFound
	}
	for _, previous := range r.tx.data.ledger {
		if previous.ID == state.ID || (previous.WalletID == state.WalletID && previous.TransactionID == state.TransactionID) {
			return application.ErrConflict
		}
	}
	r.tx.data.ledger = append(r.tx.data.ledger, state)
	return nil
}

func (r ledgerRepository) List(ctx context.Context, walletID string, after *application.LedgerPosition, limit int) ([]domain.WalletLedgerEntry, error) {
	if err := r.tx.step(ctx, "ledger.list"); err != nil {
		return nil, err
	}
	var states []domain.LedgerEntryState
	for _, state := range r.tx.data.ledger {
		if state.WalletID == walletID && (after == nil || state.CreatedAt.After(after.CreatedAt) ||
			(state.CreatedAt.Equal(after.CreatedAt) && state.ID > after.ID)) {
			states = append(states, state)
		}
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].CreatedAt.Equal(states[j].CreatedAt) {
			return states[i].ID < states[j].ID
		}
		return states[i].CreatedAt.Before(states[j].CreatedAt)
	})
	var result []domain.WalletLedgerEntry
	for i, state := range states {
		if i == limit {
			break
		}
		entry, err := domain.RehydrateWalletLedgerEntry(state)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, nil
}

func (r ledgerRepository) CalculateBalance(ctx context.Context, walletID, currency string) (domain.Money, int64, error) {
	if err := r.tx.step(ctx, "ledger.calculate"); err != nil {
		return domain.Money{}, 0, err
	}
	total := new(big.Int)
	var count int64
	for _, state := range r.tx.data.ledger {
		if state.WalletID != walletID {
			continue
		}
		units, err := state.Money.MinorUnits()
		if err != nil {
			return domain.Money{}, 0, err
		}
		if state.Direction == domain.DirectionDebit {
			total.Sub(total, big.NewInt(units))
		} else {
			total.Add(total, big.NewInt(units))
		}
		count++
	}
	if !total.IsInt64() {
		return domain.Money{}, 0, domain.ErrMoneyOverflow
	}
	money, err := domain.NewMoneyFromMinorUnits(total.Int64(), currency)
	return money, count, err
}

type outboxRepository struct{ tx *memoryTx }

func (r outboxRepository) Insert(ctx context.Context, record application.OutboxRecord) error {
	if err := r.tx.step(ctx, "outbox.insert"); err != nil {
		return err
	}
	for _, previous := range r.tx.data.outbox {
		if previous.EventID == record.EventID {
			return application.ErrConflict
		}
	}
	r.tx.data.outbox = append(r.tx.data.outbox, record)
	return nil
}
