package main

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

var httpRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "resume_http_requests_total",
		Help: "Total HTTP requests, labeled by handler and status code",
	},
	[]string{"handler", "code"},
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
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

// instrument wraps a handler to count requests by handler name and response
// status code, published under resume_http_requests_total.
func instrument(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, r)
		httpRequestsTotal.WithLabelValues(name, strconv.Itoa(sw.status)).Inc()
	}
}
