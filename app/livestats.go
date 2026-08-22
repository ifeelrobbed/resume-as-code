package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
func pollVisitorCount() {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		if v, err := queryVisitorCount(client); err != nil {
			log.Printf("visitor count poll: %v", err)
		} else {
			visitorCount.set(v)
		}
		time.Sleep(pollInterval)
	}
}

type prometheusQueryResponse struct {
	Data struct {
		Result []struct {
			Value [2]interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func queryVisitorCount(client *http.Client) (int64, error) {
	u := prometheusURL + "/api/v1/query?query=" + url.QueryEscape(visitorCountQuery)
	resp, err := client.Get(u)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var pr prometheusQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, err
	}
	if len(pr.Data.Result) == 0 {
		return 0, nil
	}

	str, _ := pr.Data.Result[0].Value[1].(string)
	f, err := strconv.ParseFloat(str, 64)
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
func pollSparkline(name, query string, cache *sparklineCache) {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		if points, err := queryRange(client, query); err != nil {
			log.Printf("%s poll: %v", name, err)
		} else {
			cache.set(points)
		}
		time.Sleep(sparklinePollInterval)
	}
}

// prometheusRangeResponse is query_range's shape - a matrix of series, each
// with a list of [timestamp, value] samples, unlike query's single "value".
type prometheusRangeResponse struct {
	Data struct {
		Result []struct {
			Values [][2]interface{} `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func queryRange(client *http.Client, query string) ([]float64, error) {
	now := time.Now()
	start := now.Add(-sparklineWindow).Unix()
	end := now.Unix()

	u := prometheusURL + "/api/v1/query_range?query=" + url.QueryEscape(query) +
		"&start=" + strconv.FormatInt(start, 10) +
		"&end=" + strconv.FormatInt(end, 10) +
		"&step=" + sparklineStep

	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var pr prometheusRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	if len(pr.Data.Result) == 0 {
		return nil, nil
	}

	values := pr.Data.Result[0].Values
	points := make([]float64, 0, len(values))
	for _, v := range values {
		str, _ := v[1].(string)
		f, err := strconv.ParseFloat(str, 64)
		if err != nil {
			continue
		}
		points = append(points, f)
	}
	return points, nil
}
