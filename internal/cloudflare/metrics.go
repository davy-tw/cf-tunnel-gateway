package cloudflare

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	apiCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cf_tunnel_gateway_cloudflare_api_calls_total",
		Help: "Cloudflare API calls by operation and result (ok, error, rate_limited).",
	}, []string{"operation", "result"})
	apiLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cf_tunnel_gateway_cloudflare_api_call_duration_seconds",
		Help:    "Cloudflare API call latency by operation.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})
)

func init() { metrics.Registry.MustRegister(apiCalls, apiLatency) }

func observe(op string, start time.Time, err error) {
	apiLatency.WithLabelValues(op).Observe(time.Since(start).Seconds())
	result := "ok"
	var rl *RateLimitError
	switch {
	case errors.As(err, &rl):
		result = "rate_limited"
	case err != nil:
		result = "error"
	}
	apiCalls.WithLabelValues(op, result).Inc()
}
