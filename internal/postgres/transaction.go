package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/junglegaming/backend-challenge-go/internal/application"
)

type Transactor struct{ pool *pgxpool.Pool }

var _ application.Transactor = (*Transactor)(nil)

func NewTransactor(pool *pgxpool.Pool) (*Transactor, error) {
	if pool == nil {
		return nil, application.ErrInvalidDependencies
	}
	return &Transactor{pool: pool}, nil
}

func (t *Transactor) WithinTransaction(ctx context.Context, action func(application.Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if action == nil {
		return application.ErrInvalidInput
	}
	// READ COMMITTED permite observar commits anteriores após aguardar a carteira.
	tx, err := t.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return databaseError(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if err := action(application.Repositories{
		Inbox:   &inboxRepository{tx: tx},
		Wallets: &walletRepository{tx: tx}, Wagers: &wagerRepository{tx: tx},
		Ledger: &ledgerRepository{tx: tx}, Outbox: &outboxRepository{tx: tx},
	}); err != nil {
		return databaseError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return databaseError(tx.Commit(ctx))
}

func databaseError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ErrNotFound
	}
	var databaseErr *pgconn.PgError
	if errors.As(err, &databaseErr) {
		switch databaseErr.Code {
		case "23505", "40001", "40P01": // Unicidade, serialização ou deadlock.
			return fmt.Errorf("%w: %w", application.ErrConflict, err)
		}
	}
	return err
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
