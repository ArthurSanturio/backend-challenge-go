package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type wagerRepository struct{ tx pgx.Tx }

var _ application.WagerRepository = (*wagerRepository)(nil)

const wagerColumns = `id, COALESCE(external_transaction_id,''), COALESCE(provider_id,''),
	COALESCE(idempotency_key,''), COALESCE(payload_hash,''), wallet_id, player_id,
	COALESCE(round_id,''), COALESCE(game_id,''), kind, amount_minor, currency,
	COALESCE(reference_external_transaction_id,''), COALESCE(reference_transaction_id,''),
	status, COALESCE(direction,''), result_balance_minor, COALESCE(failure_code,''), created_at, updated_at`

func (r *wagerRepository) Get(ctx context.Context, id string) (*domain.WagerTransaction, error) {
	return scanWager(r.tx.QueryRow(ctx, `SELECT `+wagerColumns+` FROM wager_transactions WHERE id=$1`, id))
}

func (r *wagerRepository) GetByExternalID(ctx context.Context, providerID, externalID string) (*domain.WagerTransaction, error) {
	return scanWager(r.tx.QueryRow(ctx, `SELECT `+wagerColumns+` FROM wager_transactions WHERE provider_id=$1 AND external_transaction_id=$2`, providerID, externalID))
}

func (r *wagerRepository) GetByIdempotencyKey(ctx context.Context, providerID, key string) (*domain.WagerTransaction, error) {
	return scanWager(r.tx.QueryRow(ctx, `SELECT `+wagerColumns+` FROM wager_transactions WHERE provider_id=$1 AND idempotency_key=$2`, providerID, key))
}

func (r *wagerRepository) HasProcessedReversal(ctx context.Context, referenceID string) (bool, error) {
	var found bool
	err := r.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM wager_transactions
		WHERE reference_transaction_id=$1 AND status='PROCESSED' AND kind IN ('REFUND','ROLLBACK'))`, referenceID).Scan(&found)
	return found, databaseError(err)
}

func scanWager(row pgx.Row) (*domain.WagerTransaction, error) {
	var state domain.WagerTransactionState
	var units int64
	var kind, status, direction, failure, currency string
	var result pgtype.Int8
	err := row.Scan(&state.ID, &state.ExternalTransactionID, &state.ProviderID, &state.IdempotencyKey,
		&state.PayloadHash, &state.WalletID, &state.PlayerID, &state.RoundID, &state.GameID,
		&kind, &units, &currency, &state.ReferenceExternalTransactionID, &state.ReferenceTransactionID,
		&status, &direction, &result, &failure, &state.CreatedAt, &state.UpdatedAt)
	if err != nil {
		return nil, databaseError(err)
	}
	state.Kind, state.Status = domain.TransactionKind(kind), domain.TransactionStatus(status)
	state.Direction, state.FailureCode = domain.LedgerDirection(direction), domain.FailureCode(failure)
	state.Money, err = domain.NewMoneyFromMinorUnits(units, currency)
	if err != nil {
		return nil, err
	}
	if result.Valid {
		state.ResultBalance, err = domain.NewMoneyFromMinorUnits(result.Int64, currency)
		if err != nil {
			return nil, err
		}
	}
	return domain.RehydrateWagerTransaction(state)
}

func wagerArguments(transaction *domain.WagerTransaction) ([]any, error) {
	state, err := transaction.Snapshot()
	if err != nil {
		return nil, err
	}
	units, err := state.Money.MinorUnits()
	if err != nil {
		return nil, err
	}
	currency, err := state.Money.Currency()
	if err != nil {
		return nil, err
	}
	var result any
	if state.Status == domain.StatusProcessed {
		result, err = state.ResultBalance.MinorUnits()
		if err != nil {
			return nil, err
		}
	}
	return []any{
		state.ID, nullableText(state.ExternalTransactionID), nullableText(state.ProviderID),
		nullableText(state.IdempotencyKey), nullableText(state.PayloadHash), state.WalletID, state.PlayerID,
		nullableText(state.RoundID), nullableText(state.GameID), string(state.Kind), units, currency,
		nullableText(state.ReferenceExternalTransactionID), nullableText(state.ReferenceTransactionID),
		string(state.Status), nullableText(string(state.Direction)), result, nullableText(string(state.FailureCode)),
		state.CreatedAt.UTC().Truncate(time.Microsecond), state.UpdatedAt.UTC().Truncate(time.Microsecond),
	}, nil
}

func (r *wagerRepository) Insert(ctx context.Context, transaction *domain.WagerTransaction) error {
	arguments, err := wagerArguments(transaction)
	if err != nil {
		return err
	}
	_, err = r.tx.Exec(ctx, `INSERT INTO wager_transactions
		(id, external_transaction_id, provider_id, idempotency_key, payload_hash, wallet_id, player_id,
		round_id, game_id, kind, amount_minor, currency, reference_external_transaction_id, reference_transaction_id,
		status, direction, result_balance_minor, failure_code, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`, arguments...)
	return databaseError(err)
}

func (r *wagerRepository) Update(ctx context.Context, transaction *domain.WagerTransaction) error {
	arguments, err := wagerArguments(transaction)
	if err != nil {
		return err
	}
	result, err := r.tx.Exec(ctx, `UPDATE wager_transactions SET
		external_transaction_id=$2, provider_id=$3, idempotency_key=$4, payload_hash=$5, wallet_id=$6,
		player_id=$7, round_id=$8, game_id=$9, kind=$10, amount_minor=$11, currency=$12,
		reference_external_transaction_id=$13, reference_transaction_id=$14, status=$15,
		direction=$16, result_balance_minor=$17, failure_code=$18, created_at=$19, updated_at=$20
		WHERE id=$1 AND status IN ('PENDING','PENDING_REFERENCE')`, arguments...)
	if err != nil {
		return databaseError(err)
	}
	if result.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return nil
}
