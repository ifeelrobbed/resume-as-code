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

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", instrument("index", indexHandler))
	mux.HandleFunc("GET /resume", instrument("resume", resumeHandler))
	mux.HandleFunc("GET /status", instrument("status", statusHandler))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	go pollVisitorCount()

	addr := ":8080"
	log.Printf("listening on %s (build %s)", addr, buildTime)
	log.Fatal(http.ListenAndServe(addr, mux))
}
