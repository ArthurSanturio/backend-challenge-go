package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type ledgerRepository struct{ tx pgx.Tx }

var _ application.LedgerRepository = (*ledgerRepository)(nil)

func (r *ledgerRepository) Insert(ctx context.Context, entry domain.WalletLedgerEntry) error {
	state, err := entry.Snapshot()
	if err != nil {
		return err
	}
	amount, err := state.Money.MinorUnits()
	if err != nil {
		return err
	}
	currency, err := state.Money.Currency()
	if err != nil {
		return err
	}
	before, err := state.BalanceBefore.MinorUnits()
	if err != nil {
		return err
	}
	after, err := state.BalanceAfter.MinorUnits()
	if err != nil {
		return err
	}
	_, err = r.tx.Exec(ctx, `INSERT INTO wallet_ledger_entries
		(id,wallet_id,transaction_id,direction,amount_minor,currency,balance_before_minor,balance_after_minor,wallet_version,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,(SELECT version FROM wallets WHERE id=$2),$9)`,
		state.ID, state.WalletID, state.TransactionID, string(state.Direction), amount, currency, before, after,
		state.CreatedAt.UTC().Truncate(time.Microsecond))
	return databaseError(err)
}

func (r *ledgerRepository) List(ctx context.Context, walletID string, position *application.LedgerPosition, limit int) ([]domain.WalletLedgerEntry, error) {
	if limit < 1 || limit > 201 {
		return nil, application.ErrInvalidInput
	}
	query := `SELECT id,wallet_id,transaction_id,direction,amount_minor,currency,balance_before_minor,balance_after_minor,created_at
		FROM wallet_ledger_entries WHERE wallet_id=$1`
	arguments := []any{walletID}
	if position != nil {
		query += ` AND (created_at,id) > ($2,$3)`
		arguments = append(arguments, position.CreatedAt.UTC(), position.ID)
	}
	query += ` ORDER BY created_at,id LIMIT $` + strconv.Itoa(len(arguments)+1)
	arguments = append(arguments, limit)
	rows, err := r.tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	result := make([]domain.WalletLedgerEntry, 0)
	for rows.Next() {
		var state domain.LedgerEntryState
		var direction, currency string
		var amount, before, after int64
		if err := rows.Scan(&state.ID, &state.WalletID, &state.TransactionID, &direction, &amount, &currency, &before, &after, &state.CreatedAt); err != nil {
			return nil, databaseError(err)
		}
		state.Direction = domain.LedgerDirection(direction)
		state.Money, err = domain.NewMoneyFromMinorUnits(amount, currency)
		if err != nil {
			return nil, err
		}
		state.BalanceBefore, err = domain.NewMoneyFromMinorUnits(before, currency)
		if err != nil {
			return nil, err
		}
		state.BalanceAfter, err = domain.NewMoneyFromMinorUnits(after, currency)
		if err != nil {
			return nil, err
		}
		entry, err := domain.RehydrateWalletLedgerEntry(state)
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, databaseError(rows.Err())
}

func (r *ledgerRepository) CalculateBalance(ctx context.Context, walletID, currency string) (domain.Money, int64, error) {
	var exact string
	var count int64
	err := r.tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN direction='CREDIT'
		THEN amount_minor::numeric ELSE -amount_minor::numeric END),0)::text, COUNT(*)
		FROM wallet_ledger_entries WHERE wallet_id=$1 AND currency=$2`, walletID, currency).Scan(&exact, &count)
	if err != nil {
		return domain.Money{}, 0, databaseError(err)
	}
	units, err := strconv.ParseInt(exact, 10, 64)
	if err != nil {
		return domain.Money{}, 0, domain.ErrMoneyOverflow
	}
	balance, err := domain.NewMoneyFromMinorUnits(units, currency)
	return balance, count, err
}
