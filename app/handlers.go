package main

import (
	"fmt"
	"html/template"
	"net/http"
	"sync/atomic"
	"time"
)

var visitorCount int64

var templates = template.Must(template.ParseGlob("templates/*.html"))

// PageData is the data available to every page template. GrafanaURL is
// empty until Grafana ships (phase 2) - templates hide that stat when it's "".
type PageData struct {
	VisitorCount int64
	Uptime       string
	LastDeploy   string
	GrafanaURL   string
}

func pageData() PageData {
	return PageData{
		VisitorCount: atomic.LoadInt64(&visitorCount),
		Uptime:       time.Since(startTime).Round(time.Second).String(),
		LastDeploy:   buildTime,
		GrafanaURL:   "",
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&visitorCount, 1)
	if err := templates.ExecuteTemplate(w, "index.html", pageData()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func resumeHandler(w http.ResponseWriter, r *http.Request) {
	if err := templates.ExecuteTemplate(w, "resume.html", pageData()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"buildTime":%q,"uptime":%q}`, buildTime, time.Since(startTime).Round(time.Second).String())
}
