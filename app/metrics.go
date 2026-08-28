package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// blackboxExporterUserAgent identifies requests from the site's own
// uptime probe (manifests/platform/prometheus-blackbox-exporter) - a
// prefix match, not an exact one, so a future chart version bump (which
// changes the trailing version number) doesn't silently start counting
// probe traffic as real requests again.
const blackboxExporterUserAgent = "Blackbox-Exporter/"

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

// Saturation - "how much concurrent work is the process doing right now" -
// not covered by the RED trio above, and not something the default go_*/
// process_* collectors give you in a directly business-meaningful way
// (goroutine count is a proxy, not the real thing). Deliberately NOT
// excluded for the blackbox exporter's traffic the way httpRequestsTotal/
// httpRequestDuration are: this is a resource-utilization signal, not an
// attribution signal, so a synthetic request genuinely does occupy a
// connection/goroutine the same as any other while it's in flight.
var httpRequestsInFlight = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "resume_http_requests_in_flight",
		Help: "Number of HTTP requests currently being served",
	},
)

// Always 1 - the labels are the actual payload. Lets Grafana annotate
// dashboards at deploy boundaries (a spike right after a revision change
// is visually obvious) instead of having to cross-reference the site's own
// "last deploy" stat or kubectl by hand. gitRevision/buildTime are package
// vars set via -ldflags -X before any init() runs, so they're already
// correct by the time this file's init() reads them below.
var buildInfo = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "resume_build_info",
		Help: "Always 1; labels carry the running build's git revision and build time",
	},
	[]string{"revision", "build_time"},
)

func init() {
	prometheus.MustRegister(httpRequestsTotal, httpRequestDuration, engagementClicksTotal, httpRequestsInFlight, buildInfo)
	buildInfo.WithLabelValues(gitRevision, buildTime).Set(1)
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

// instrument wraps a handler to track in-flight concurrency (all traffic),
// count requests (by handler name and response status code), and time them
// (by handler name), published under resume_http_requests_in_flight,
// resume_http_requests_total, and resume_http_request_duration_seconds.
// Skips the count/duration recording entirely for the blackbox exporter's
// own probe traffic - it's a synthetic, once-a-minute, in-cluster-to-LB-
// and-back request, not a real visitor, and letting it into these metrics
// would both inflate the visitor count and skew the latency histogram
// optimistic. The exporter's own probe_* metrics already track its
// activity properly. In-flight is tracked regardless (see its own comment).
func instrument(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpRequestsInFlight.Inc()
		defer httpRequestsInFlight.Dec()

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, r)
		if strings.HasPrefix(r.UserAgent(), blackboxExporterUserAgent) {
			return
		}
		httpRequestsTotal.WithLabelValues(name, strconv.Itoa(sw.status)).Inc()
		httpRequestDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())

		// The durable visitor count (#75) increments here rather than inside
		// indexHandler so that "is this a real visit?" - including the blackbox
		// exclusion just above - is decided in exactly one place.
		if name == "index" {
			visitors.inc()
		}
	}
}
