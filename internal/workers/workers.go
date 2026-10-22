// Package workers executa trabalhos duráveis; não depende de Fx, SQL ou AWS.
package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

var (
	ErrLeaseLost = errors.New("worker lease is no longer owned")
	// ErrPermanentDelivery classifica a entrega para DLQ, sem alterar a operação financeira.
	ErrPermanentDelivery = errors.New("permanent event delivery error")
)

type OutboxJob struct {
	EventID, AggregateID, Payload, Token string
	Attempts                             int
	ToDLQ                                bool
}
type ReferenceJob struct {
	TransactionID, Token string
	Attempts             int
}
type Store interface {
	ClaimOutbox(context.Context, int) (OutboxJob, bool, error)
	OutboxDelivered(context.Context, OutboxJob) error
	RetryOutbox(context.Context, OutboxJob, time.Duration, bool) error
	ClaimReference(context.Context) (ReferenceJob, bool, error)
	ReferenceCompleted(context.Context, ReferenceJob) error
	RetryReference(context.Context, ReferenceJob, time.Duration) error
}
type Sender interface {
	Send(context.Context, OutboxJob) error
}
type Observer interface {
	Event(string, string)
	Duration(string, time.Duration)
}
type Config struct {
	OutboxMaxAttempts, ReferenceMaxAttempts int
	Observer                                Observer
}
type Manager struct {
	store                            Store
	sender                           Sender
	wagers                           *application.WageringService
	logger                           *slog.Logger
	config                           Config
	polling                          context.Context
	cancel                           context.CancelFunc
	done                             chan struct{}
	stopping                         atomic.Bool
	outboxHealthy, referencesHealthy atomic.Bool
}

func New(store Store, sender Sender, wagers *application.WageringService, config Config, logger *slog.Logger) (*Manager, error) {
	if store == nil || wagers == nil || logger == nil || config.OutboxMaxAttempts < 1 || config.ReferenceMaxAttempts < 1 {
		return nil, application.ErrInvalidDependencies
	}
	return &Manager{store: store, sender: sender, wagers: wagers, config: config, logger: logger, done: make(chan struct{})}, nil
}
func (m *Manager) Start() {
	m.referencesHealthy.Store(true)
	m.outboxHealthy.Store(m.sender != nil)
	m.polling, m.cancel = context.WithCancel(context.Background())
	var group sync.WaitGroup
	if m.sender != nil {
		group.Add(1)
		go func() { defer group.Done(); m.loop("outbox", &m.outboxHealthy, m.publish) }()
	}
	group.Add(1)
	go func() { defer group.Done(); m.loop("references", &m.referencesHealthy, m.resume) }()
	go func() { group.Wait(); close(m.done) }()
}
func (m *Manager) StopPolling() {
	m.stopping.Store(true)
	if m.cancel != nil {
		m.cancel()
	}
}
func (m *Manager) Wait(ctx context.Context) error {
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *Manager) loop(component string, healthy *atomic.Bool, step func() (bool, error)) {
	defer healthy.Store(false)
	for m.polling.Err() == nil {
		worked, err := step()
		healthy.Store(err == nil)
		if err != nil && m.polling.Err() == nil {
			m.event(component, "error")
			m.logger.Warn("Worker step failed; durable work will be retried", "worker", component)
		}
		if !worked || err != nil {
			timer := time.NewTimer(time.Second)
			select {
			case <-m.polling.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
func (m *Manager) publish() (worked bool, stepErr error) {
	claim, cancelClaim := context.WithTimeout(m.polling, 5*time.Second)
	job, found, err := m.store.ClaimOutbox(claim, m.config.OutboxMaxAttempts)
	cancelClaim()
	if err != nil || !found {
		return false, err
	}
	started := time.Now()
	defer m.duration("outbox", started)
	logger := outboxLogger(m.logger, job)
	defer func() {
		if stepErr != nil {
			logger.Warn("Outbox step failed; durable work will be retried")
		}
	}()
	// O cancelamento do polling não corta o envio/ack de uma reserva em andamento.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	delivery, cancelDelivery := context.WithTimeout(ctx, 10*time.Second)
	err = m.sender.Send(delivery, job)
	cancelDelivery()
	if err != nil {
		permanent := errors.Is(err, ErrPermanentDelivery)
		if retryErr := m.store.RetryOutbox(ctx, job, Backoff(job.Attempts), permanent); retryErr != nil {
			return true, retryErr
		}
		m.event("outbox", "retry")
		logger.Warn("Outbox delivery retained for retry", "nextToDLQ", job.ToDLQ || permanent)
		return true, nil
	}
	if err := m.store.OutboxDelivered(ctx, job); err != nil {
		return true, err
	}
	if job.ToDLQ {
		m.event("outbox", "dlq")
	} else {
		m.event("outbox", "published")
	}
	logger.Info("Outbox delivery confirmed")
	return true, nil
}
func (m *Manager) resume() (worked bool, stepErr error) {
	claim, cancelClaim := context.WithTimeout(m.polling, 5*time.Second)
	job, found, err := m.store.ClaimReference(claim)
	cancelClaim()
	if err != nil || !found {
		return false, err
	}
	started := time.Now()
	defer m.duration("references", started)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	metadata := application.RequestMetadata{CorrelationID: job.TransactionID, CausationID: "reference-worker:" + job.TransactionID}
	// A retomada tem sua própria causa; transactionId liga esta etapa ao
	// recebimento original, mesmo quando aquele recebeu outro correlationId.
	logger := m.logger.With("worker", "references", "transactionId", job.TransactionID,
		"correlationId", metadata.CorrelationID, "causationId", metadata.CausationID, "attempt", job.Attempts)
	defer func() {
		if stepErr != nil {
			logger.Warn("Reference step failed; durable work will be retried")
		}
	}()
	var result application.ProcessWagerResult
	if job.Attempts >= m.config.ReferenceMaxAttempts {
		result, err = m.wagers.ResumeFinalReferenceAttempt(ctx, application.InternalActor(), job.TransactionID, metadata)
	} else {
		result, err = m.wagers.Resume(ctx, application.InternalActor(), job.TransactionID, metadata)
	}
	if err != nil {
		// Erros de infraestrutura preservam retry, inclusive após o limite de
		// tentativas de referência. Não autorizam REJECTED nem FAILED.
		retryErr := m.store.RetryReference(ctx, job, Backoff(job.Attempts))
		m.event("references", "infrastructure_retry")
		logger.Warn("Pending operation retained after infrastructure error")
		if retryErr != nil {
			return true, retryErr
		}
		return true, nil
	}
	logger = logger.With("walletId", result.Transaction.WalletID, "providerId", result.Transaction.ProviderID,
		"externalTransactionId", result.Transaction.ExternalTransactionID, "status", result.Transaction.Status)
	switch result.Transaction.Status {
	case domain.StatusProcessed, domain.StatusRejected, domain.StatusFailed:
		if err := m.store.ReferenceCompleted(ctx, job); err != nil {
			return true, err
		}
		m.event("references", string(result.Transaction.Status))
		logger.Info("Pending operation resumed")
	default:
		m.event("references", "retry")
		if err := m.store.RetryReference(ctx, job, Backoff(job.Attempts)); err != nil {
			return true, err
		}
		logger.Info("Pending operation still waiting for reference")
	}
	return true, nil
}

// Lê apenas identificadores do envelope durável. Não registra payload, valores,
// credenciais ou token de reserva, nem interfere na validação feita pelo sender.
func outboxLogger(logger *slog.Logger, job OutboxJob) *slog.Logger {
	logger = logger.With("worker", "outbox", "eventId", job.EventID, "aggregateId", job.AggregateID,
		"attempt", job.Attempts, "toDLQ", job.ToDLQ)
	var envelope struct {
		EventType     string `json:"eventType"`
		CorrelationID string `json:"correlationId"`
		CausationID   string `json:"causationId"`
		Data          struct {
			TransactionID         string `json:"transactionId"`
			WalletID              string `json:"walletId"`
			ProviderID            string `json:"providerId"`
			ExternalTransactionID string `json:"externalTransactionId"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(job.Payload), &envelope); err != nil {
		return logger
	}
	for _, field := range []struct{ name, value string }{
		{"eventType", envelope.EventType}, {"correlationId", envelope.CorrelationID},
		{"causationId", envelope.CausationID}, {"transactionId", envelope.Data.TransactionID},
		{"walletId", envelope.Data.WalletID}, {"providerId", envelope.Data.ProviderID},
		{"externalTransactionId", envelope.Data.ExternalTransactionID},
	} {
		if field.value != "" {
			logger = logger.With(field.name, field.value)
		}
	}
	return logger
}

func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		return 5 * time.Minute
	}
	delay := time.Duration(1<<attempt) * time.Second
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func (m *Manager) Check() error {
	if m.stopping.Load() || !m.referencesHealthy.Load() || (m.sender != nil && !m.outboxHealthy.Load()) {
		return errors.New("workers are unavailable or stopping")
	}
	return nil
}
func (m *Manager) event(component, outcome string) {
	if m.config.Observer != nil {
		m.config.Observer.Event(component, outcome)
	}
}
func (m *Manager) duration(component string, started time.Time) {
	if m.config.Observer != nil {
		m.config.Observer.Duration(component, time.Since(started))
	}
}
