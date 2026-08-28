package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// increase(), not a raw sum() of the counter, because
// resume_http_requests_total lives in the app's own process memory and
// resets to 0 on every pod restart/redeploy - increase() detects each
// reset and adds the pre-reset total back in. The [15d] window matches
// Prometheus's configured retention (kube-prometheus-stack/application.yaml)
// - it's a rolling 15-day count, not all-time, since anything Prometheus
// itself has already rolled off can't be recovered.
const visitorCountQuery = `sum(increase(resume_http_requests_total{handler="index"}[15d]))`

const pollInterval = 30 * time.Second

// Scoped to handler="index" deliberately - unlike visitorCountQuery's
// increase() (which only cares about total visits), a live rate needs to
// exclude the readiness/liveness probes kubelet fires at /status every
// 5s/15s. Those aren't excluded from resume_http_requests_total the way
// the blackbox exporter's traffic is (see instrument() in metrics.go) -
// confirmed live that they alone produce a flat ~0.267 req/s baseline
// that would otherwise swamp real traffic in the sparkline.
const requestRateQuery = `sum(rate(resume_http_requests_total{handler="index"}[5m]))`

// Same handler="index" scoping, and for the same reason: resume_http_
// request_duration_seconds is a HistogramVec keyed by handler, so filtering
// to index here already excludes /status's kubelet-probe-dominated latency
// without needing a separate exclusion.
const p95LatencyQuery = `histogram_quantile(0.95, sum(rate(resume_http_request_duration_seconds_bucket{handler="index"}[5m])) by (le))`

// References the recording rule by name rather than re-deriving it here,
// unlike requestRateQuery/p95LatencyQuery - promql/resume-site-rules.yaml's
// error_ratio5m is already correctly scoped to handler="index" (was fixed
// there specifically because the unscoped version was silently
// desensitizing the ResumeSiteElevatedErrorRate alert), so there's nothing
// to fix or duplicate on the app side this time.
const errorRateQuery = `resume_site:http_requests:error_ratio5m`

// Shared by both sparkline metrics above - a 24h window barely moves
// minute to minute, so there's nothing to gain from polling as often as
// the single-value visitor count, or from giving each metric its own
// window/step.
const sparklineWindow = 24 * time.Hour
const sparklineStep = "30m"
const sparklinePollInterval = 5 * time.Minute

var prometheusURL = envOrDefault("PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090")

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// errNoData means the round trip succeeded and the query was valid, but
// matched no series - a real state on a low-traffic site (a fresh deploy, or a
// window with no qualifying samples yet), and deliberately distinct from a
// failure. Callers handle both the same way, by leaving the cache alone; what
// keeping them separate buys is a log line that says which one happened.
var errNoData = errors.New("prometheus returned no data")

// prometheusEnvelope is the status wrapper every /api/v1 response carries.
// Checked in addition to the HTTP status code, not instead of it: Prometheus
// answers a malformed query with a 400/422 *and* a filled-in envelope, while
// an upstream failure (a restarting pod, a proxy in between) can produce a
// non-200 carrying no envelope at all. Neither check subsumes the other.
type prometheusEnvelope struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func (e prometheusEnvelope) err() error {
	if e.Status == "success" {
		return nil
	}
	if e.Error != "" {
		return fmt.Errorf("prometheus query failed (%s): %s", e.ErrorType, e.Error)
	}
	return fmt.Errorf("prometheus query returned status %q", e.Status)
}

// prometheusResponse is implemented by both response structs below, via the
// embedded envelope.
type prometheusResponse interface {
	err() error
}

// getPrometheusJSON issues the query and decodes it into v. Any non-200 is an
// error rather than an empty result: without this check an error body still
// decodes cleanly into a response struct carrying no series, which is
// indistinguishable from a successful empty query - that's how a Prometheus
// restart used to surface on the homepage as a confident "0 visitors".
func getPrometheusJSON(client *http.Client, u string, v prometheusResponse) error {
	resp, err := client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Bounded read: enough of the body to identify the failure from a log
		// line, without copying an arbitrarily large error page into memory.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if snippet := strings.TrimSpace(string(body)); snippet != "" {
			return fmt.Errorf("prometheus returned %s: %s", resp.Status, snippet)
		}
		return fmt.Errorf("prometheus returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return err
	}
	return v.err()
}

// sampleValue pulls the float out of a Prometheus [timestamp, value] pair. The
// value arrives as a string rather than a JSON number, deliberately on
// Prometheus's side, so that "NaN"/"+Inf"/"-Inf" survive the round trip -
// ParseFloat accepts all three, and callers upstream already expect NaN (see
// stats() and sparklinePoints() in handlers.go).
func sampleValue(pair [2]interface{}) (float64, error) {
	s, ok := pair[1].(string)
	if !ok {
		return 0, fmt.Errorf("sample value: expected string, got %T", pair[1])
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("sample value %q: %w", s, err)
	}
	return f, nil
}

// visitorCount is served from Prometheus rather than tracked in-process -
// the app's own counters reset on every pod restart/redeploy, but
// Prometheus persists resume_http_requests_total on a PVC, so reading it
// back is what makes the homepage number survive a deploy.
type visitorCountCache struct {
	mu        sync.RWMutex
	value     int64
	updatedAt time.Time
}

var visitorCount visitorCountCache

func (c *visitorCountCache) get() (int64, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.value, c.updatedAt
}

func (c *visitorCountCache) set(v int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = v
	c.updatedAt = time.Now()
}

// pollVisitorCount refreshes the cache from Prometheus on a fixed interval,
// polling immediately rather than waiting out the first tick. A failed
// query leaves the last-known value in place - staleness shows up on the
// page as an old "as of" time rather than a hidden fallback value.
func pollVisitorCount(ctx context.Context) {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		refreshVisitorCount(client, prometheusURL, &visitorCount)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

// refreshVisitorCount runs a single poll. Split out from pollVisitorCount's
// loop so that the promise in that comment - a failed query must never
// overwrite a good cached value - is directly testable rather than trapped
// inside an infinite loop.
func refreshVisitorCount(client *http.Client, baseURL string, cache *visitorCountCache) {
	v, err := queryVisitorCount(client, baseURL)
	switch {
	case errors.Is(err, errNoData):
		log.Printf("visitor count poll: no matching series yet, keeping last value")
	case err != nil:
		log.Printf("visitor count poll: %v (keeping last value)", err)
	default:
		cache.set(v)
	}
}

type prometheusQueryResponse struct {
	prometheusEnvelope
	Data struct {
		Result []struct {
			Value [2]interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func queryVisitorCount(client *http.Client, baseURL string) (int64, error) {
	u := baseURL + "/api/v1/query?query=" + url.QueryEscape(visitorCountQuery)

	var pr prometheusQueryResponse
	if err := getPrometheusJSON(client, u, &pr); err != nil {
		return 0, err
	}
	if len(pr.Data.Result) == 0 {
		return 0, errNoData
	}

	f, err := sampleValue(pr.Data.Result[0].Value)
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

// sparklineCache holds a 24h window of samples for a homepage sparkline -
// a slice, not a single value, since this backs a chart rather than a
// single stat. Shared by both requestRate and p95Latency below, since both
// are polled and rendered identically - only the query string differs.
type sparklineCache struct {
	mu        sync.RWMutex
	points    []float64
	updatedAt time.Time
}

var requestRate sparklineCache
var p95Latency sparklineCache
var errorRate sparklineCache

func (c *sparklineCache) get() ([]float64, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.points, c.updatedAt
}

func (c *sparklineCache) set(points []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.points = points
	c.updatedAt = time.Now()
}

// pollSparkline refreshes cache's 24h window from query every few minutes
// rather than every pollInterval - the window barely moves minute to
// minute, so there's nothing to gain from polling as often as the
// single-value visitor count. name is just for the log line on failure.
func pollSparkline(ctx context.Context, name, query string, cache *sparklineCache) {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		refreshSparkline(client, prometheusURL, name, query, cache)
		select {
		case <-ctx.Done():
			return
		case <-time.After(sparklinePollInterval):
		}
	}
}

// refreshSparkline runs a single poll, split out from pollSparkline's loop for
// the same reason as refreshVisitorCount above.
func refreshSparkline(client *http.Client, baseURL, name, query string, cache *sparklineCache) {
	points, err := queryRange(client, baseURL, query)
	switch {
	case errors.Is(err, errNoData):
		log.Printf("%s poll: no matching series yet, keeping last value", name)
	case err != nil:
		log.Printf("%s poll: %v (keeping last value)", name, err)
	default:
		cache.set(points)
	}
}

// prometheusRangeResponse is query_range's matrix result type - a set of
// series, each with its own [timestamp, value] samples, unlike the instant
// query's single "value" vector.
type prometheusRangeResponse struct {
	prometheusEnvelope
	Data struct {
		Result []struct {
			Values [][2]interface{} `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func queryRange(client *http.Client, baseURL, query string) ([]float64, error) {
	now := time.Now()
	start := now.Add(-sparklineWindow).Unix()
	end := now.Unix()

	u := baseURL + "/api/v1/query_range?query=" + url.QueryEscape(query) +
		"&start=" + strconv.FormatInt(start, 10) +
		"&end=" + strconv.FormatInt(end, 10) +
		"&step=" + sparklineStep

	var pr prometheusRangeResponse
	if err := getPrometheusJSON(client, u, &pr); err != nil {
		return nil, err
	}
	if len(pr.Data.Result) == 0 {
		return nil, errNoData
	}

	// An unparseable sample fails the whole refresh rather than being skipped
	// the way it used to be: "NaN"/"+Inf"/"-Inf" all parse fine, so anything
	// left over is a protocol surprise, and silently dropping it would render
	// a sparkline that's quietly missing points. Failing keeps the previous
	// window on screen instead, which is at least a window that was real.
	values := pr.Data.Result[0].Values
	points := make([]float64, 0, len(values))
	for _, v := range values {
		f, err := sampleValue(v)
		if err != nil {
			return nil, err
		}
		points = append(points, f)
	}
	return points, nil
}
