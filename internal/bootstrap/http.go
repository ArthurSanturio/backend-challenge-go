package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/httpapi"
	"github.com/junglegaming/backend-challenge-go/internal/observability"
	"go.uber.org/fx"
)

func newAuthenticator(lifecycle fx.Lifecycle, configuration config) (*httpapi.Authenticator, error) {
	// Não usar o contexto de OnStart: ele expira após a inicialização. O cache
	// JWKS precisa continuar buscando chaves enquanto o servidor estiver ativo.
	lifetime, cancel := context.WithCancel(context.Background())
	auth, err := httpapi.NewAuthenticator(lifetime, configuration.oidc)
	if err != nil {
		cancel()
		return nil, err
	}
	lifecycle.Append(fx.Hook{OnStop: func(context.Context) error { cancel(); return nil }})
	return auth, nil
}

func newRouter(wallets *application.WalletService, wagering *application.WageringService,
	auth *httpapi.Authenticator, database *database, sqs *sqsRuntime, worker *workerRuntime, logger *slog.Logger, metrics *observability.Metrics) (http.Handler, error) {
	configuration := httpapi.Config{
		MetricsHandler: metrics.Handler(), ObserveHTTP: metrics.HTTP,
		Checks:         map[string]func(context.Context) error{"idp": auth.Check, "schema": func(ctx context.Context) error { return database.checkSchema(ctx, sqs.configuration) }},
		DisabledChecks: map[string]string{},
		Wallets:        wallets, Wagering: wagering, Auth: auth, CheckPostgres: database.ping, Logger: logger,
	}
	if sqs.configuration.sqsEnabled {
		configuration.CheckSQS = sqs.check
	}
	if worker.configuration.workersEnabled {
		configuration.Checks["workers"] = worker.check
	} else {
		configuration.DisabledChecks["workers"] = "disabled"
	}
	return httpapi.NewRouter(configuration)
}

func newHTTPServer(configuration config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: configuration.httpAddress, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
}

func registerHTTP(lifecycle fx.Lifecycle, server *http.Server, shutdown fx.Shutdowner, logger *slog.Logger) {
	stopped := make(chan error, 1)
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Abrir a porta no hook faz uma porta ocupada falhar durante App.Start,
			// permitindo que Fx reverta os componentes já iniciados.
			listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", server.Addr)
			if err != nil {
				return errors.New("HTTP listener could not bind the configured address")
			}
			go func() {
				err := server.Serve(listener)
				stopped <- err
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("HTTP server stopped unexpectedly")
					if err := shutdown.Shutdown(fx.ExitCode(1)); err != nil {
						logger.Error("Could not request application shutdown")
					}
				}
			}()
			logger.InfoContext(ctx, "HTTP API started", "address", listener.Addr().String())
			return nil
		},
		OnStop: func(ctx context.Context) error {
			logger.InfoContext(ctx, "HTTP API shutting down")
			grace, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			stopErr := server.Shutdown(grace)
			if stopErr != nil {
				_ = server.Close()
			}
			select {
			case serveErr := <-stopped:
				if stopErr != nil {
					return stopErr
				}
				if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
					return errors.New("HTTP server failed")
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
}
