// Package httpapi adapta HTTP aos casos de uso, sem regras financeiras ou SQL.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application"
)

type Config struct {
	Wallets        *application.WalletService
	Wagering       *application.WageringService
	Auth           *Authenticator
	CheckPostgres  func(context.Context) error
	CheckSQS       func(context.Context) error
	Logger         *slog.Logger
	MetricsHandler http.Handler
	ObserveHTTP    func(string, string, int, time.Duration)
	Checks         map[string]func(context.Context) error
	DisabledChecks map[string]string
}
type api struct{ config Config }

func NewRouter(config Config) (http.Handler, error) {
	if config.Wallets == nil || config.Wagering == nil || config.Auth == nil || config.Auth.verifier == nil || config.CheckPostgres == nil {
		return nil, errors.New("HTTP dependencies are required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	a := &api{config: config}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
	})
	mux.HandleFunc("GET /health/ready", a.ready)
	if config.MetricsHandler != nil {
		mux.Handle("GET /metrics", config.MetricsHandler)
	}
	mux.HandleFunc("POST /wallets", config.Auth.protect(true, a.createWallet))
	mux.HandleFunc("GET /wallets/{walletId}", config.Auth.protect(true, a.getWallet))
	mux.HandleFunc("GET /wallets/{walletId}/ledger", config.Auth.protect(true, a.listLedger))
	mux.HandleFunc("POST /wallets/{walletId}/reconciliation", config.Auth.protect(true, a.reconcile))
	mux.HandleFunc("POST /wagering/transactions", config.Auth.protect(false, a.processWager))
	mux.HandleFunc("GET /wagering/transactions/{transactionId}", config.Auth.protect(false, a.getWager))
	mux.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", config.Auth.protect(false, a.getWagerByExternalID))
	return a.middleware(mux), nil
}

func (a *api) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{"sqs": "not_configured"}
	scope := "http-postgresql"
	callbacks := map[string]func(context.Context) error{"postgresql": a.config.CheckPostgres}
	if a.config.CheckSQS != nil {
		callbacks["sqs"] = a.config.CheckSQS
		scope = "http-postgresql-sqs"
	}
	for name, value := range a.config.DisabledChecks {
		checks[name] = value
	}
	for name, check := range a.config.Checks {
		callbacks[name] = check
	}
	type outcome struct {
		name string
		err  error
	}
	responses := make(chan outcome, len(callbacks))
	for name, check := range callbacks {
		checks[name] = "unavailable"
		go func(name string, check func(context.Context) error) { responses <- outcome{name, check(ctx)} }(name, check)
	}
	for remaining := len(callbacks); remaining > 0; remaining-- {
		select {
		case result := <-responses:
			if result.err == nil {
				checks[result.name] = "ready"
			}
		case <-ctx.Done():
			remaining = 1
		}
	}
	status := http.StatusOK
	for name := range callbacks {
		if checks[name] != "ready" {
			status = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, status, map[string]any{"ready": status == http.StatusOK, "checks": checks, "scope": scope})
}
