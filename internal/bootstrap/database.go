package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/postgres"
	"go.uber.org/fx"
)

// database adia a aquisição de recursos até OnStart. Se outro construtor falhar,
// ainda não há pool para vazar. A implementação transacional continua em postgres.
type database struct {
	pool         *pgxpool.Pool
	transactions *postgres.Transactor
}

func newDatabase(lifecycle fx.Lifecycle, configuration config, logger *slog.Logger) *database {
	database := &database{}
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			startup, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			pool, err := postgres.Open(startup, configuration.databaseURL)
			if err != nil {
				// Fx registra erros de hooks. Não propagar URLs ou erros brutos do driver.
				return errors.New("PostgreSQL initialization failed; check configuration and connectivity")
			}
			transactions, err := postgres.NewTransactor(pool)
			if err != nil {
				pool.Close()
				return errors.New("PostgreSQL transactor initialization failed")
			}
			database.pool, database.transactions = pool, transactions
			logger.InfoContext(ctx, "PostgreSQL connection ready")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			// O hook HTTP é registrado depois deste; Fx o encerra primeiro.
			database.pool.Close()
			logger.InfoContext(ctx, "PostgreSQL connection closed")
			return nil
		},
	})
	return database
}

func provideTransactor(database *database) application.Transactor { return database }

func (database *database) WithinTransaction(ctx context.Context, action func(application.Repositories) error) error {
	if database.transactions == nil {
		return errors.New("PostgreSQL is not started")
	}
	return database.transactions.WithinTransaction(ctx, action)
}
func (database *database) ping(ctx context.Context) error {
	if database.pool == nil {
		return errors.New("PostgreSQL is not started")
	}
	return database.pool.Ping(ctx)
}

func (database *database) checkSchema(ctx context.Context, configuration config) error {
	var ready bool
	err := database.pool.QueryRow(ctx, `SELECT to_regclass('wallets') IS NOT NULL AND to_regclass('wallet_ledger_entries') IS NOT NULL
 AND to_regclass('wager_transactions') IS NOT NULL AND to_regclass('outbox_events') IS NOT NULL
 AND (NOT $1 OR to_regclass('inbox_messages') IS NOT NULL)
 AND (NOT $2 OR (to_regclass('reference_jobs') IS NOT NULL AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='outbox_events' AND column_name='lease_token')))`, configuration.sqsEnabled, configuration.workersEnabled).Scan(&ready)
	if err != nil || !ready {
		return errors.New("required database schema unavailable")
	}
	return nil
}
