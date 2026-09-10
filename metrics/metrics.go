package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	namespace = "bsc_mev_sentry"

	ApiLatencyHist = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "api",
		Name:      "latency",
		Buckets:   prometheus.ExponentialBuckets(0.01, 3, 15),
	}, []string{"method"})

	ApiErrorCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "api",
		Name:      "error",
	}, []string{"method", "code"})

	AccountError = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "account",
		Name:      "error",
	}, []string{"account", "message"})

	ChainError = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "chainRPC",
		Name:      "error",
	})

	// Registry synchronization (see the registry package).
	RegistrySyncTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "registry",
		Name:      "sync_total",
		Help:      "Registry sync attempts by result: applied, unchanged, empty, error.",
	}, []string{"result"})

	RegistrySyncedBlock = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "registry",
		Name:      "synced_block",
		Help:      "Block number of the registry snapshot currently applied to the allowlist.",
	})

	RegistryBuilderCount = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "registry",
		Name:      "builder_count",
		Help:      "Number of builder keys in the effective allowlist after local overrides.",
	})

	RegistryLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "registry",
		Name:      "last_success_timestamp_seconds",
		Help:      "Unix time of the last successful registry read.",
	})
)
