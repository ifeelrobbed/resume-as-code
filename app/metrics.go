package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var httpRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "resume_http_requests_total",
		Help: "Total HTTP requests, labeled by handler and status code",
	},
	[]string{"handler", "code"},
)

// Not labeled by code (unlike httpRequestsTotal) - crossing handler with
// status code would multiply the number of bucket sets for little benefit,
// since latency is normally sliced by handler alone (p50/p95/p99 via
// histogram_quantile()).
var httpRequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "resume_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, labeled by handler",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"handler"},
)

// Labeled by target (see engagement.go's allowlist) - never fed directly
// from request input, since an unbounded/attacker-controlled label value
// would create unbounded time series.
var engagementClicksTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "resume_engagement_clicks_total",
		Help: "Outbound engagement link clicks, labeled by target",
	},
	[]string{"target"},
)

func init() {
	prometheus.MustRegister(httpRequestsTotal, httpRequestDuration, engagementClicksTotal)
}

// statusWriter captures the status code a handler writes so it can be
// reported after the handler returns (WriteHeader happens deep inside
// template execution / http.Error, not at the call site).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// instrument wraps a handler to count requests (by handler name and
// response status code) and time them (by handler name), published under
// resume_http_requests_total and resume_http_request_duration_seconds.
func instrument(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, r)
		httpRequestsTotal.WithLabelValues(name, strconv.Itoa(sw.status)).Inc()
		httpRequestDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
	}
}
