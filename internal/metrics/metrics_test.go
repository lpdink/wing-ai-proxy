package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsRegistered(t *testing.T) {
	// Verify all metrics are registered and can be observed
	RequestsTotal.WithLabelValues("test-provider", "test-model", "200").Inc()
	RequestDuration.WithLabelValues("test-provider", "test-model").Observe(0.5)
	UpstreamFailures.WithLabelValues("test-provider", "test-model").Inc()
	TimeToFirstByte.WithLabelValues("test-provider", "test-model").Observe(0.1)
	AuditQueueLength.Set(42)

	// Gather all metrics and verify they exist
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}

	expected := map[string]bool{
		"proxy_requests_total":             false,
		"proxy_request_duration_seconds":   false,
		"proxy_upstream_failures_total":    false,
		"proxy_time_to_first_byte_seconds": false,
		"proxy_audit_queue_length":         false,
	}

	for _, mf := range mfs {
		if _, ok := expected[mf.GetName()]; ok {
			expected[mf.GetName()] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("metric %q not found in gatherer", name)
		}
	}
}
