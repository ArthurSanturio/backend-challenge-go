package bootstrap

import (
	"context"
	"errors"
	"log/slog"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/observability"
	"github.com/junglegaming/backend-challenge-go/internal/postgres"
	"github.com/junglegaming/backend-challenge-go/internal/sqsconsumer"
	"github.com/junglegaming/backend-challenge-go/internal/workers"
	"go.uber.org/fx"
)

type workerRuntime struct {
	configuration config
	metrics       *observability.Metrics
	database      *database
	wagers        *application.WageringService
	logger        *slog.Logger
	manager       *workers.Manager
	publisher     *sqsconsumer.EventPublisher
}

func newWorkerRuntime(configuration config, database *database, wagers *application.WageringService, logger *slog.Logger, metrics *observability.Metrics) *workerRuntime {
	return &workerRuntime{metrics: metrics, configuration: configuration, database: database, wagers: wagers, logger: logger}
}
func registerWorkers(lifecycle fx.Lifecycle, runtime *workerRuntime) {
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if !runtime.configuration.workersEnabled {
				return nil
			}
			var migrated bool
			if err := runtime.database.pool.QueryRow(ctx, `SELECT to_regclass('reference_jobs') IS NOT NULL AND EXISTS (
    SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='outbox_events' AND column_name='lease_token'
   )`).Scan(&migrated); err != nil || !migrated {
				return errors.New("workers require migration 000003")
			}
			var sender workers.Sender
			if runtime.configuration.sqsEnabled {
				client, err := sqsconsumer.NewClient(ctx, runtime.configuration.awsRegion, runtime.configuration.sqsEndpoint)
				if err != nil {
					return err
				}
				publisher, err := sqsconsumer.NewEventPublisher(ctx, client)
				if err != nil {
					if closer, ok := client.Options().HTTPClient.(interface{ CloseIdleConnections() }); ok {
						closer.CloseIdleConnections()
					}
					return err
				}
				publisher.SetObserver(runtime.metrics)
				runtime.publisher = publisher
				sender = publisher
			}
			manager, err := workers.New(postgres.NewWorkerStore(runtime.database.pool), sender, runtime.wagers,
				workers.Config{OutboxMaxAttempts: runtime.configuration.outboxMaxAttempts, ReferenceMaxAttempts: runtime.configuration.referenceMaxAttempts, Observer: runtime.metrics}, runtime.logger)
			if err != nil {
				if runtime.publisher != nil {
					runtime.publisher.Close()
				}
				return err
			}
			runtime.manager = manager
			manager.Start()
			runtime.logger.InfoContext(ctx, "Durable workers started", "publisherEnabled", sender != nil)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if runtime.manager == nil {
				return nil
			}
			runtime.manager.StopPolling()
			err := runtime.manager.Wait(ctx)
			if runtime.publisher != nil {
				runtime.publisher.Close()
			}
			return err
		},
	})
}
func stopWorkersPollingFirst(lifecycle fx.Lifecycle, runtime *workerRuntime) {
	lifecycle.Append(fx.Hook{OnStop: func(context.Context) error {
		if runtime.manager != nil {
			runtime.manager.StopPolling()
		}
		return nil
	}})
}
func (runtime *workerRuntime) check(ctx context.Context) error {
	if runtime.configuration.workersEnabled {
		if runtime.manager == nil {
			return errors.New("workers are not started")
		}
		if err := runtime.manager.Check(); err != nil {
			return err
		}
	}
	if runtime.configuration.workersEnabled && runtime.configuration.sqsEnabled {
		if runtime.publisher == nil {
			return errors.New("event publisher is not started")
		}
		return runtime.publisher.Check(ctx)
	}
	return nil
}
