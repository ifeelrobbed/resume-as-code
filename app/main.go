package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Overridden at build time via -ldflags "-X main.buildTime=..." and
// "-X main.gitRevision=..."; left as "dev" for local `go run`.
var buildTime = "dev"
var gitRevision = "dev"

var startTime = time.Now()

// draining flips on SIGTERM and makes readyzHandler answer 503. Kubernetes
// removes a pod from Service endpoints and sends SIGTERM at roughly the same
// moment, so a server that stops accepting the instant it's signalled still
// drops requests that were routed to it microseconds earlier. Failing readiness
// first, then waiting for that removal to propagate, is what actually closes
// that window.
//
// Only readiness reads this. Liveness reads /status, which stays 200 while
// draining - a pod on its way out is not a wedged process to restart.
var draining atomic.Bool

// Timeouts, none of which net/http applies by default. Without
// ReadHeaderTimeout in particular, a client that opens a connection and dribbles
// headers forever holds a goroutine indefinitely - the Slowloris shape gosec
// flags as G114, and this server is on the public internet.
//
// WriteTimeout has to exceed the slowest handler: /metrics grows with
// cardinality and template rendering is the next slowest, both far below this.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
)

// drainDelay covers one readiness probe period (5s, see the Deployment) plus a
// little, so kubelet has actually observed the 503 and removed this pod from
// endpoints before connections stop being accepted.
//
// shutdownTimeout then bounds waiting for in-flight requests to finish. The sum
// must stay under the pod's terminationGracePeriodSeconds (30s by default) or
// kubelet SIGKILLs mid-drain and the whole exercise is wasted: 6 + 20 = 26s,
// leaving a little headroom. Raising either means raising that too.
const (
	drainDelay      = 6 * time.Second
	shutdownTimeout = 20 * time.Second
)

// noCacheHeaders forces browsers to revalidate every static asset request
// with the server instead of trusting a cached copy indefinitely - without
// any Cache-Control at all, browsers apply their own heuristic caching, and
// a stale-but-still-heuristically-fresh script can silently keep running
// old JS well past a deploy (hit in production 2026-08-17: a browser served
// an old cached engagement.js from before a bugfix). http.FileServer
// already sets Last-Modified and answers conditional GETs (If-Modified-
// Since) with 304, so this doesn't add a real round trip cost for an
// unchanged file - it just stops the browser from skipping that check.
func noCacheHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", instrument("index", indexHandler))
	mux.HandleFunc("GET /resume", instrument("resume", resumeHandler))
	mux.HandleFunc("GET /status", instrument("status", statusHandler))
	mux.HandleFunc("GET /readyz", instrument("readyz", readyzHandler))
	mux.HandleFunc("POST /engagement/click", instrument("engagement-click", engagementClickHandler))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCacheHeaders(http.FileServer(http.Dir("static")))))
	return mux
}

func main() {
	// Cancelled on SIGTERM (what kubelet sends) or SIGINT (Ctrl-C locally).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The pollers take the same context so they stop with the process rather
	// than logging failures into a shutdown that's already underway.
	go pollVisitorCount(ctx)
	go pollSparkline(ctx, "request rate", requestRateQuery, &requestRate)
	go pollSparkline(ctx, "p95 latency", p95LatencyQuery, &p95Latency)
	go pollSparkline(ctx, "error rate", errorRateQuery, &errorRate)

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           newMux(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	go func() {
		// ErrServerClosed is the expected result of Shutdown, not a failure.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen on %s: %v", srv.Addr, err)
		}
	}()
	log.Printf("listening on %s (build %s)", srv.Addr, buildTime)

	<-ctx.Done()
	// Restore default signal handling so a second SIGTERM kills immediately
	// rather than being swallowed by a drain that's evidently stuck.
	stop()

	log.Printf("shutdown signal received; failing readiness and draining for %s", drainDelay)
	draining.Store(true)
	time.Sleep(drainDelay)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown did not finish within %s, closing connections: %v", shutdownTimeout, err)
		_ = srv.Close()
	}
	log.Print("stopped")
}
