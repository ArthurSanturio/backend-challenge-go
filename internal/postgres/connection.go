// Package postgres implementa a persistência com SQL explícito e pgx.
package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open valida a configuração e a conectividade. O chamador deve fechar o pool.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, errors.New("postgres database URL is required")
	}
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Não propagar a URL, que pode conter credenciais.
		return nil, errors.New("invalid postgres configuration")
	}
	configuration.ConnConfig.RuntimeParams["timezone"] = "UTC"
	configuration.ConnConfig.RuntimeParams["application_name"] = "backend-challenge-go"
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
