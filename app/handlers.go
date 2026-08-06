package main

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

var visitorCount int64

// yearOnly and monthYear reformat an Experience/SubRole "YYYY-MM" date for
// display; "present" passes through unchanged. Two formats because the
// homepage's condensed preview only needs the year, but the full resume
// page shows month precision - same underlying data either way.
var templateFuncs = template.FuncMap{
	"yearOnly": func(s string) string {
		if s == "present" {
			return s
		}
		year, _, ok := strings.Cut(s, "-")
		if !ok {
			return s
		}
		return year
	},
	"monthYear": func(s string) string {
		if s == "present" {
			return s
		}
		year, month, ok := strings.Cut(s, "-")
		if !ok {
			return s
		}
		return month + "/" + year
	},
	"join": strings.Join,
}

var templates = template.Must(template.New("").Funcs(templateFuncs).ParseGlob("templates/*.html"))

// Stats is the live data every page can show - visitor count, process
// uptime, and build time (see main.go's buildTime, injected via -ldflags).
type Stats struct {
	VisitorCount int64
	Uptime       string
	LastDeploy   string
}

func stats() Stats {
	return Stats{
		VisitorCount: atomic.LoadInt64(&visitorCount),
		Uptime:       time.Since(startTime).Round(time.Second).String(),
		LastDeploy:   buildTime,
	}
}

// IndexData is what templates/index.html renders. Recent is the homepage's
// condensed preview - just the two most recent Experience entries.
type IndexData struct {
	Stats  Stats
	Resume Resume
	Recent []Experience
}

// ResumeData is what templates/resume.html renders. SpecYAML is the same
// Resume data marshaled to YAML and syntax-colored (see yaml.go) - the
// "spec" view is never hand-duplicated content, just a different
// projection of the same source.
type ResumeData struct {
	Stats    Stats
	Resume   Resume
	SpecYAML template.HTML
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&visitorCount, 1)
	data := IndexData{
		Stats:  stats(),
		Resume: resume,
		Recent: resume.Experience[:2],
	}
	if err := templates.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func resumeHandler(w http.ResponseWriter, r *http.Request) {
	specYAML, err := specHTML(buildSpecDoc(resume))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := ResumeData{
		Stats:    stats(),
		Resume:   resume,
		SpecYAML: specYAML,
	}
	if err := templates.ExecuteTemplate(w, "resume.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"buildTime":%q,"uptime":%q}`, buildTime, time.Since(startTime).Round(time.Second).String())
}
