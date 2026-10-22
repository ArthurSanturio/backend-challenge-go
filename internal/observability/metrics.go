// Package observability expõe métricas sem identificar jogadores ou operações.
package observability

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Snapshot struct{ OutboxPending, OutboxLag, OutboxDLQ, ReferencesPending float64 }
type Metrics struct {
	registry       *prometheus.Registry
	results        *prometheus.CounterVec
	operations     *prometheus.CounterVec
	latency        *prometheus.HistogramVec
	httpRequests   *prometheus.CounterVec
	httpLatency    *prometheus.HistogramVec
	reconciliation *prometheus.CounterVec
	conflicts      prometheus.Counter
	dlqDepth       *prometheus.GaugeVec
}

func New(snapshot func(context.Context) (Snapshot, error)) *Metrics {
	r := prometheus.NewRegistry()
	m := &Metrics{registry: r,
		results:        prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wallet_wager_results_total", Help: "Resultados observados após commit; replays são identificados."}, []string{"status", "replay"}),
		operations:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wallet_background_operations_total", Help: "Resultados, retries e falhas dos componentes em segundo plano."}, []string{"component", "outcome"}),
		latency:        prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "wallet_processing_duration_seconds", Help: "Duração do processamento, sem valores monetários."}, []string{"component"}),
		httpRequests:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wallet_http_requests_total", Help: "Respostas HTTP por padrão de rota."}, []string{"method", "route", "status"}),
		httpLatency:    prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "wallet_http_duration_seconds", Help: "Duração HTTP por padrão de rota."}, []string{"route"}),
		reconciliation: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wallet_reconciliations_total", Help: "Reconciliações e divergências observadas."}, []string{"consistent"}),
		conflicts:      prometheus.NewCounter(prometheus.CounterOpts{Name: "wallet_conflicts_total", Help: "Conflitos de idempotência ou concorrência SQL."}),
		dlqDepth:       prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "wallet_sqs_dlq_depth", Help: "Profundidade aproximada da DLQ na última verificação."}, []string{"queue"}),
	}
	r.MustRegister(m.results, m.operations, m.latency, m.httpRequests, m.httpLatency, m.reconciliation, m.conflicts, m.dlqDepth, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if snapshot != nil {
		r.MustRegister(&storageCollector{snapshot: snapshot})
	}
	return m
}
func (m *Metrics) WagerResult(status domain.TransactionStatus, replay bool) {
	m.results.WithLabelValues(string(status), strconv.FormatBool(replay)).Inc()
}
func (m *Metrics) Reconciliation(consistent bool) {
	m.reconciliation.WithLabelValues(strconv.FormatBool(consistent)).Inc()
}
func (m *Metrics) Conflict() { m.conflicts.Inc() }
func (m *Metrics) Event(component, outcome string) {
	m.operations.WithLabelValues(component, outcome).Inc()
}
func (m *Metrics) Duration(component string, duration time.Duration) {
	m.latency.WithLabelValues(component).Observe(duration.Seconds())
}
func (m *Metrics) HTTP(method, route string, status int, duration time.Duration) {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		method = "OTHER"
	}
	if route == "" {
		route = "unmatched"
	}
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpLatency.WithLabelValues(route).Observe(duration.Seconds())
}
func (m *Metrics) QueueDepth(queue string, depth float64) {
	m.dlqDepth.WithLabelValues(queue).Set(depth)
}
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Timeout: 4 * time.Second})
}

type storageCollector struct {
	snapshot func(context.Context) (Snapshot, error)
}

var storageDescriptors = []*prometheus.Desc{
	prometheus.NewDesc("wallet_storage_metrics_up", "Consulta das métricas duráveis disponível.", nil, nil),
	prometheus.NewDesc("wallet_outbox_pending", "Eventos ainda não confirmados nem enviados para DLQ.", nil, nil),
	prometheus.NewDesc("wallet_outbox_oldest_age_seconds", "Idade do evento pendente mais antigo.", nil, nil),
	prometheus.NewDesc("wallet_outbox_dead_lettered", "Eventos com entrega à DLQ confirmada.", nil, nil),
	prometheus.NewDesc("wallet_reference_jobs_pending", "Operações com agenda ainda não concluída.", nil, nil),
}

func (c *storageCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range storageDescriptors {
		ch <- d
	}
}
func (c *storageCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := c.snapshot(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(storageDescriptors[0], prometheus.GaugeValue, 0)
		return
	}
	values := []float64{1, s.OutboxPending, s.OutboxLag, s.OutboxDLQ, s.ReferencesPending}
	for i, v := range values {
		ch <- prometheus.MustNewConstMetric(storageDescriptors[i], prometheus.GaugeValue, v)
	}
}
