package main

import (
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Overridden at build time via -ldflags "-X main.buildTime=...";
// left as "dev" for local `go run`.
var buildTime = "dev"

var startTime = time.Now()

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

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", instrument("index", indexHandler))
	mux.HandleFunc("GET /resume", instrument("resume", resumeHandler))
	mux.HandleFunc("GET /status", instrument("status", statusHandler))
	mux.HandleFunc("POST /engagement/click", instrument("engagement-click", engagementClickHandler))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCacheHeaders(http.FileServer(http.Dir("static")))))

	go pollVisitorCount()

	addr := ":8080"
	log.Printf("listening on %s (build %s)", addr, buildTime)
	log.Fatal(http.ListenAndServe(addr, mux))
}
