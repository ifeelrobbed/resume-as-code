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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scoped to handler="index" deliberately: a live rate has to exclude the
// readiness/liveness probes kubelet fires at /readyz and /status every 5s/15s.
// Those aren't excluded from resume_http_requests_total the way the blackbox
// exporter's traffic is (see instrument() in metrics.go) - confirmed live that
// they alone produce a flat ~0.267 req/s baseline that would otherwise swamp
// real traffic in the sparkline.
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

// pollSparkline refreshes cache's 24h window from query every few minutes - the
// window barely moves minute to minute, so there's nothing to gain from polling
// harder. name is just for the log line on failure.
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

// Argo CD sync state for the homepage's sync panel (#43). Replaces a hardcoded
// "Synced - Healthy" that had been displayed since launch regardless of what
// the cluster was actually doing.
//
// One query returning every series, not two count()s. #43 counted server-side
// to avoid a parse path that inspects `metric` rather than `value`; the panel
// tooltip now lists each application by name (#48), so those labels are needed
// anyway - and reading them turns out to be strictly better:
//
//   - One round trip instead of two, so total and healthy come from the same
//     snapshot. The old pair could straddle a state change and needed a clamp
//     to stop the panel reading "8/7"; that whole class of skew is gone.
//   - No "or vector(0)" special case. An empty result now means Prometheus
//     returned nothing, which is genuinely unknown, rather than being
//     ambiguous with every application counting as zero.
const argoAppsQuery = `argocd_app_info`

const argoPollInterval = 30 * time.Second

// argoApp is one Argo CD Application as the panel tooltip lists it.
type argoApp struct {
	Name   string
	Sync   string
	Health string
}

// healthy reports whether this application is in the state the panel counts as
// good. Defined once here so the count behind "6/7" and the per-application
// list in the tooltip can never disagree about what healthy means.
func (a argoApp) healthy() bool {
	return a.Sync == "Synced" && a.Health == "Healthy"
}

// argoSyncCache holds the last successfully read set of applications. Like the
// sparkline caches, a failed poll leaves the previous values in place rather
// than blanking the panel on one bad scrape.
type argoSyncCache struct {
	mu      sync.RWMutex
	apps    []argoApp
	loaded  bool
	updated time.Time
}

var argoSync argoSyncCache

func (c *argoSyncCache) get() (apps []argoApp, total, healthy int, loaded bool, updated time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, a := range c.apps {
		if a.healthy() {
			healthy++
		}
	}
	return c.apps, len(c.apps), healthy, c.loaded, c.updated
}

func (c *argoSyncCache) set(apps []argoApp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apps, c.loaded, c.updated = apps, true, time.Now()
}

func pollArgoSync(ctx context.Context, cache *argoSyncCache) {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		refreshArgoSync(client, prometheusURL, cache)
		select {
		case <-ctx.Done():
			return
		case <-time.After(argoPollInterval):
		}
	}
}

func refreshArgoSync(client *http.Client, baseURL string, cache *argoSyncCache) {
	apps, err := queryArgoApps(client, baseURL)
	if err != nil {
		log.Printf("argocd sync poll: %v (keeping last value)", err)
		return
	}
	cache.set(apps)
}

// queryArgoApps reads every argocd_app_info series and reduces it to the
// name/sync/health the panel needs, sorted by name so the tooltip does not
// reshuffle between polls - Prometheus makes no ordering promise.
func queryArgoApps(client *http.Client, baseURL string) ([]argoApp, error) {
	u := baseURL + "/api/v1/query?query=" + url.QueryEscape(argoAppsQuery)

	var pr prometheusInstantResponse
	if err := getPrometheusJSON(client, u, &pr); err != nil {
		return nil, err
	}
	if len(pr.Data.Result) == 0 {
		return nil, errNoData
	}

	apps := make([]argoApp, 0, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		name := r.Metric["name"]
		if name == "" {
			// A series with no name label is not something the tooltip can
			// show; skipping beats rendering a blank row.
			continue
		}
		apps = append(apps, argoApp{
			Name:   name,
			Sync:   r.Metric["sync_status"],
			Health: r.Metric["health_status"],
		})
	}
	if len(apps) == 0 {
		return nil, errNoData
	}
	slices.SortFunc(apps, func(a, b argoApp) int { return strings.Compare(a.Name, b.Name) })
	return apps, nil
}

// prometheusInstantResponse is the shape of an instant query - a vector of
// series each with a single [timestamp, value] sample, unlike query_range's
// matrix of many.
type prometheusInstantResponse struct {
	prometheusEnvelope
	Data struct {
		Result []struct {
			// Metric carries the series labels. queryArgoApps reads
			// name/sync_status/health_status from here, which is the whole
			// reason an instant query is used rather than asking Prometheus
			// to count and return a bare number.
			Metric map[string]string `json:"metric"`
			Value  [2]interface{}    `json:"value"`
		} `json:"result"`
	} `json:"data"`
}
