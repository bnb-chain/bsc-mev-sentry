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
)
