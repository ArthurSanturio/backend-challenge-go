// Package bootstrap compõe os adaptadores e casos de uso com Uber Fx.
// Apenas esta fronteira da aplicação conhece o container de dependências.
package bootstrap

import (
	"log/slog"
	"os"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/observability"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

// Module registra construtores. Fx resolve as dependências e cria uma instância
// de cada componente solicitado; Invoke conecta o servidor ao ciclo de vida.
var Module = fx.Module("api",
	fx.Module("configuration", fx.Provide(readConfig, newLogger)),
	fx.Module("postgres", fx.Provide(newDatabase, provideTransactor)),
	fx.Module("observability", fx.Provide(newMetrics)),
	fx.Module("use-cases", fx.Provide(
		newDependencies, application.NewWalletService, application.NewWageringService,
	)),
	fx.Module("sqs", fx.Provide(newSQSRuntime), fx.Invoke(registerSQS)),
	fx.Module("workers", fx.Provide(newWorkerRuntime), fx.Invoke(registerWorkers)),
	fx.Module("http", fx.Provide(newAuthenticator, newRouter, newHTTPServer), fx.Invoke(registerHTTP)),
	fx.Invoke(stopSQSPollingFirst, stopWorkersPollingFirst),
)

func New() *fx.App {
	return fx.New(Module,
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(30*time.Second),
		fx.WithLogger(func(logger *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: logger}
		}),
	)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}
func newDependencies(transactions application.Transactor, logger *slog.Logger, configuration config, metrics *observability.Metrics) application.Dependencies {
	return application.Dependencies{Transactions: transactions, Logger: logger, ReferenceTTL: configuration.referenceTTL, Observer: metrics}
}
