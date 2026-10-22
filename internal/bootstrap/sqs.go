package bootstrap

import (
	"context"
	"errors"
	"log/slog"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/observability"
	"github.com/junglegaming/backend-challenge-go/internal/sqsconsumer"
	"go.uber.org/fx"
)

type sqsRuntime struct {
	configuration config
	metrics       *observability.Metrics
	database      *database
	wagers        *application.WageringService
	logger        *slog.Logger
	consumer      *sqsconsumer.Consumer
}

func newSQSRuntime(configuration config, database *database, wagers *application.WageringService, logger *slog.Logger, metrics *observability.Metrics) *sqsRuntime {
	return &sqsRuntime{metrics: metrics, configuration: configuration, database: database, wagers: wagers, logger: logger}
}
func registerSQS(lifecycle fx.Lifecycle, runtime *sqsRuntime) {
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if !runtime.configuration.sqsEnabled {
				return nil
			}
			var migrated bool
			if err := runtime.database.pool.QueryRow(ctx, `SELECT to_regclass('inbox_messages') IS NOT NULL`).Scan(&migrated); err != nil || !migrated {
				return errors.New("SQS requires the inbox migration before starting")
			}
			client, err := sqsconsumer.NewClient(ctx, runtime.configuration.awsRegion, runtime.configuration.sqsEndpoint)
			if err != nil {
				return err
			}
			runtime.consumer = sqsconsumer.New(client, runtime.wagers, runtime.logger)
			runtime.consumer.SetObserver(runtime.metrics)
			return runtime.consumer.Start(ctx)
		},
		OnStop: func(ctx context.Context) error {
			if runtime.consumer == nil {
				return nil
			}
			runtime.consumer.StopPolling()
			return runtime.consumer.Wait(ctx)
		},
	})
}

// Registrado por último: é executado primeiro na parada, antes de aguardar HTTP.
func stopSQSPollingFirst(lifecycle fx.Lifecycle, runtime *sqsRuntime) {
	lifecycle.Append(fx.Hook{OnStop: func(context.Context) error {
		if runtime.consumer != nil {
			runtime.consumer.StopPolling()
		}
		return nil
	}})
}
func (runtime *sqsRuntime) check(ctx context.Context) error {
	if runtime.consumer == nil {
		return errors.New("SQS is not started")
	}
	return runtime.consumer.Check(ctx)
}
