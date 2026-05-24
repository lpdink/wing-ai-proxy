package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts total proxy requests by provider, model, and status.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxy_requests_total",
		Help: "Total number of proxy requests",
	}, []string{"provider", "model", "status"})

	// RequestDuration tracks request latency in seconds.
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proxy_request_duration_seconds",
		Help:    "Request latency in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"provider", "model"})

	// UpstreamFailures counts upstream failures.
	UpstreamFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "proxy_upstream_failures_total",
		Help: "Total number of upstream failures",
	}, []string{"provider", "model"})

	// TimeToFirstByte tracks time to first byte from upstream.
	TimeToFirstByte = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "proxy_time_to_first_byte_seconds",
		Help:    "Time to first byte from upstream in seconds",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"provider", "model"})

	// AuditQueueLength tracks the current audit queue size.
	AuditQueueLength = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "proxy_audit_queue_length",
		Help: "Current number of records in the audit queue",
	})
)
