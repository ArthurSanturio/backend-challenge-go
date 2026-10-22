package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type walletRepository struct{ tx pgx.Tx }

var _ application.WalletRepository = (*walletRepository)(nil)

const walletColumns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

func (r *walletRepository) Get(ctx context.Context, id string) (*domain.Wallet, error) {
	return scanWallet(r.tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id))
}

func (r *walletRepository) Lock(ctx context.Context, id string) (*domain.Wallet, error) {
	return scanWallet(r.tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id))
}

func (r *walletRepository) GetByPlayerCurrency(ctx context.Context, playerID, currency string) (*domain.Wallet, error) {
	return scanWallet(r.tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE player_id = $1 AND currency = $2`, playerID, currency))
}

func scanWallet(row pgx.Row) (*domain.Wallet, error) {
	var state domain.WalletState
	var units int64
	if err := row.Scan(&state.ID, &state.PlayerID, &state.Currency, &units, &state.Version, &state.CreatedAt, &state.UpdatedAt); err != nil {
		return nil, databaseError(err)
	}
	balance, err := domain.NewMoneyFromMinorUnits(units, state.Currency)
	if err != nil {
		return nil, err
	}
	state.Balance = balance
	return domain.RehydrateWallet(state)
}

func (r *walletRepository) Insert(ctx context.Context, wallet *domain.Wallet) error {
	state, err := wallet.Snapshot()
	if err != nil {
		return err
	}
	units, err := state.Balance.MinorUnits()
	if err != nil {
		return err
	}
	_, err = r.tx.Exec(ctx, `INSERT INTO wallets (`+walletColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		state.ID, state.PlayerID, state.Currency, units, state.Version,
		state.CreatedAt.UTC().Truncate(time.Microsecond), state.UpdatedAt.UTC().Truncate(time.Microsecond))
	return databaseError(err)
}

func (r *walletRepository) Update(ctx context.Context, wallet *domain.Wallet) error {
	state, err := wallet.Snapshot()
	if err != nil {
		return err
	}
	units, err := state.Balance.MinorUnits()
	if err != nil {
		return err
	}
	result, err := r.tx.Exec(ctx, `UPDATE wallets SET balance_minor=$2, version=$3, updated_at=$4
		WHERE id=$1 AND version=$3-1 AND player_id=$5 AND currency=$6`,
		state.ID, units, state.Version, state.UpdatedAt.UTC().Truncate(time.Microsecond), state.PlayerID, state.Currency)
	if err != nil {
		return databaseError(err)
	}
	if result.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return nil
}
