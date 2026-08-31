package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
)

// setDraining flips the flag and restores it, so tests can't leak state into
// each other through a package-level var.
func setDraining(t *testing.T, v bool) {
	t.Helper()
	original := draining.Load()
	draining.Store(v)
	t.Cleanup(func() { draining.Store(original) })
}

func callStatus(t *testing.T) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	statusHandler(rec, httptest.NewRequest(http.MethodGet, "/status", nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status body is not valid JSON (%q): %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func callReadyz(t *testing.T) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	readyzHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestStatusOKByDefault(t *testing.T) {
	setDraining(t, false)

	code, body := callStatus(t)
	if code != http.StatusOK {
		t.Errorf("got status %d, want 200", code)
	}
	if body["draining"] != false {
		t.Errorf("got draining=%v, want false", body["draining"])
	}
	if body["buildTime"] == nil || body["uptime"] == nil {
		t.Errorf("status lost a field: %v", body)
	}
}

// The reason /readyz exists. Liveness reads /status, so it must keep answering
// 200 through the drain - a pod leaving deliberately is not a wedged process,
// and a 503 here would report a healthy server as dead.
func TestStatusStaysHealthyWhileDraining(t *testing.T) {
	setDraining(t, true)

	code, body := callStatus(t)
	if code != http.StatusOK {
		t.Errorf("got status %d while draining, want 200 - liveness reads this endpoint", code)
	}
	// Still reports the drain, so curling it explains why a pod is unready.
	if body["draining"] != true {
		t.Errorf("got draining=%v, want true", body["draining"])
	}
}

func TestReadyzOKByDefault(t *testing.T) {
	setDraining(t, false)

	code, body := callReadyz(t)
	if code != http.StatusOK {
		t.Errorf("got status %d, want 200", code)
	}
	if body != "ok" {
		t.Errorf("got body %q, want %q", body, "ok")
	}
}

// The behaviour the whole drain depends on: kubelet reads this for readiness,
// and the 503 is what removes the pod from Service endpoints before the server
// stops accepting connections.
func TestReadyzFailsWhileDraining(t *testing.T) {
	setDraining(t, true)

	code, body := callReadyz(t)
	if code != http.StatusServiceUnavailable {
		t.Errorf("got status %d while draining, want 503 - readiness would not fail and the pod would keep receiving traffic", code)
	}
	if body != "draining" {
		t.Errorf("got body %q, want %q", body, "draining")
	}
}

// Readiness and liveness must disagree during a drain. If they ever return the
// same status, one of the two probes is wired to the wrong endpoint.
func TestProbesDivergeWhileDraining(t *testing.T) {
	setDraining(t, true)

	statusCode, _ := callStatus(t)
	readyzCode, _ := callReadyz(t)

	if statusCode == readyzCode {
		t.Errorf("both probes returned %d while draining; readiness must fail (503) while liveness stays healthy (200)", statusCode)
	}
}

// Guards the arithmetic in main.go's comment: the drain plus the shutdown wait
// has to finish inside the pod's terminationGracePeriodSeconds, or kubelet
// SIGKILLs mid-drain and none of this achieves anything.
func TestDrainFitsInsideTerminationGracePeriod(t *testing.T) {
	const terminationGracePeriod = 30 * time.Second // Kubernetes default

	if total := drainDelay + shutdownTimeout; total >= terminationGracePeriod {
		t.Errorf("drainDelay + shutdownTimeout = %s, which does not fit inside terminationGracePeriodSeconds=%s",
			total, terminationGracePeriod)
	}
	// The point of the delay is to outlast a readiness probe period, which the
	// Deployment sets to 5s.
	if drainDelay <= 5*time.Second {
		t.Errorf("drainDelay = %s, want longer than the 5s readiness probe period", drainDelay)
	}
}

func TestServerTimeoutsAreSet(t *testing.T) {
	// ReadHeaderTimeout is the one that matters for Slowloris (gosec G114);
	// the rest are here so a future edit can't quietly zero them.
	if readHeaderTimeout <= 0 || readTimeout <= 0 || writeTimeout <= 0 || idleTimeout <= 0 {
		t.Fatal("a server timeout is unset; net/http applies no default")
	}
	if readHeaderTimeout > readTimeout {
		t.Errorf("readHeaderTimeout (%s) exceeds readTimeout (%s)", readHeaderTimeout, readTimeout)
	}
	// Handlers must be able to finish inside the write timeout; template
	// rendering and /metrics are the slowest and are far below this.
	if writeTimeout < 5*time.Second {
		t.Errorf("writeTimeout (%s) is tight enough to cut off a slow handler", writeTimeout)
	}
}

func TestSparklinePollerStopsOnContextCancel(t *testing.T) {
	srv := prometheusStub(t, http.StatusServiceUnavailable, "down")

	original := prometheusURL
	prometheusURL = srv
	t.Cleanup(func() { prometheusURL = original })

	ctx, cancel := context.WithCancel(context.Background())

	var cache sparklineCache
	done := make(chan struct{})
	go func() {
		pollSparkline(ctx, "test", requestRateQuery, &cache)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pollSparkline did not return after context cancellation")
	}
}

// --- listener split (#76) ---------------------------------------------------

// The property this change exists for: /metrics must not be reachable on the
// listener the Ingress routes to. This is the test that fails if someone
// re-adds it to newMux for convenience.
func TestMetricsIsNotOnThePublicMux(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /metrics on the public mux returned %d, want 404 - it is reachable from the internet", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Error("public mux served Go runtime metrics")
	}
}

func TestMetricsIsServedOnTheAdminMux(t *testing.T) {
	rec := httptest.NewRecorder()
	newMetricsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics on the admin mux returned %d, want 200", rec.Code)
	}
	// A 200 with an empty body would pass a status-only check while breaking
	// scraping entirely.
	if !strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Error("admin mux responded 200 but served no Go runtime metrics")
	}
}

// The admin listener exists to keep things off the internet, so it must not
// quietly grow a second route.
func TestAdminMuxServesNothingElse(t *testing.T) {
	for _, path := range []string{"/", "/resume", "/status", "/readyz", "/static/style.css"} {
		rec := httptest.NewRecorder()
		newMetricsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("admin mux served %s with %d, want 404", path, rec.Code)
		}
	}
}

// Everything a visitor needs must stay on the public listener - the risk when
// splitting muxes is moving one route too many.
func TestPublicMuxStillServesTheSite(t *testing.T) {
	for _, path := range []string{"/", "/resume", "/status", "/readyz"} {
		rec := httptest.NewRecorder()
		newMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("public mux served %s with %d, want 200", path, rec.Code)
		}
	}
}

// The two listeners must not collide, and the admin port must be the one the
// Service/NetworkPolicy name.
func TestListenerAddressesAreDistinct(t *testing.T) {
	if publicAddr == metricsAddr {
		t.Fatal("both servers would bind the same address")
	}
	if metricsAddr != ":9090" {
		t.Errorf("metricsAddr = %s - manifests/apps/resume-site/deploy (Service, NetworkPolicy, containerPort) all name 9090", metricsAddr)
	}
	if publicAddr != ":8080" {
		t.Errorf("publicAddr = %s - the Service's http port targets 8080", publicAddr)
	}
}

// Scraping the admin listener must not inflate the counters it is reporting.
func TestAdminMuxIsNotInstrumented(t *testing.T) {
	before := promtestutil.CollectAndCount(httpRequestsTotal)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		newMetricsMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	}
	if after := promtestutil.CollectAndCount(httpRequestsTotal); after != before {
		t.Errorf("scraping /metrics added %d series to resume_http_requests_total", after-before)
	}
}
