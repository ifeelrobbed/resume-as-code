package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestStatusReadyByDefault(t *testing.T) {
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

// The behaviour the drain depends on: kubelet reads this endpoint for
// readiness, and a 503 is what removes the pod from Service endpoints before
// the server stops accepting connections.
func TestStatusFailsReadinessWhileDraining(t *testing.T) {
	setDraining(t, true)

	code, body := callStatus(t)
	if code != http.StatusServiceUnavailable {
		t.Errorf("got status %d while draining, want 503 - readiness would not fail and the pod would keep receiving traffic", code)
	}
	if body["draining"] != true {
		t.Errorf("got draining=%v, want true", body["draining"])
	}
	// Still valid JSON with the usual fields, so the endpoint stays useful for
	// a human checking why a pod is unready.
	if body["buildTime"] == nil || body["uptime"] == nil {
		t.Errorf("status lost a field while draining: %v", body)
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

// Pollers must stop when the context is cancelled rather than outliving the
// server and logging failures into a shutdown already in progress.
func TestPollersStopOnContextCancel(t *testing.T) {
	srv := prometheusStub(t, http.StatusServiceUnavailable, "down")

	original := prometheusURL
	prometheusURL = srv
	t.Cleanup(func() { prometheusURL = original })

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		pollVisitorCount(ctx)
		close(done)
	}()

	// One poll runs immediately, then the loop waits on ctx or the interval.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pollVisitorCount did not return after context cancellation")
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
