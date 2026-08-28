package main

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

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

// Stats is the live data every page can show - visitor count (and when it
// was last confirmed via Prometheus, as a bare unix timestamp so the
// template can render it in the visitor's own timezone), process uptime,
// build time (see main.go's buildTime, injected via -ldflags), and 24h
// request-rate/p95-latency/error-rate sparklines.
type Stats struct {
	VisitorCount            int64
	VisitorCountUpdatedUnix int64
	Uptime                  string
	LastDeploy              string
	RequestRateCurrent      string
	RequestRateSparkline    string
	P95LatencyCurrent       string
	P95LatencySparkline     string
	ErrorRateCurrent        string
	ErrorRateSparkline      string
}

func stats() Stats {
	count, updatedAt := visitorCount.get()
	var updatedUnix int64
	if !updatedAt.IsZero() {
		updatedUnix = updatedAt.Unix()
	}

	ratePoints, _ := requestRate.get()
	rateCurrent := "—"
	if len(ratePoints) > 0 {
		rateCurrent = fmt.Sprintf("%.2f", ratePoints[len(ratePoints)-1])
	}

	// histogram_quantile()'s buckets are seconds - ms reads better here
	// than e.g. "0.012s" for a page this fast.
	latencyPoints, _ := p95Latency.get()
	latencyCurrent := "—"
	if n := len(latencyPoints); n > 0 && !math.IsNaN(latencyPoints[n-1]) {
		latencyCurrent = fmt.Sprintf("%.1fms", latencyPoints[n-1]*1000)
	}

	// error_ratio5m is a fraction (0-1), not a percentage - *100 for display.
	errorPoints, _ := errorRate.get()
	errorCurrent := "—"
	if n := len(errorPoints); n > 0 && !math.IsNaN(errorPoints[n-1]) {
		errorCurrent = fmt.Sprintf("%.2f%%", errorPoints[n-1]*100)
	}

	return Stats{
		VisitorCount:            count,
		VisitorCountUpdatedUnix: updatedUnix,
		Uptime:                  time.Since(startTime).Round(time.Second).String(),
		LastDeploy:              buildTime,
		RequestRateCurrent:      rateCurrent,
		RequestRateSparkline:    sparklinePoints(ratePoints),
		P95LatencyCurrent:       latencyCurrent,
		P95LatencySparkline:     sparklinePoints(latencyPoints),
		ErrorRateCurrent:        errorCurrent,
		ErrorRateSparkline:      sparklinePoints(errorPoints),
	}
}

// sparklinePoints normalizes a series of values into an SVG polyline
// points string fit to the site's existing 90x24 sparkline viewBox.
// Drops NaN values first - histogram_quantile() returns NaN for a step
// with no samples yet (e.g. early in a fresh 24h window), and a single
// NaN would otherwise poison the min/max scan below (comparisons against
// NaN are always false, so min/max could get stuck at NaN for the rest
// of the series). Falls back to a flat centered line when there's no
// variance (or only one point) rather than dividing by zero - a real
// possibility here, since requestRateQuery's traffic can be this flat for
// real stretches.
func sparklinePoints(values []float64) string {
	const width, height = 90.0, 24.0

	clean := make([]float64, 0, len(values))
	for _, v := range values {
		if !math.IsNaN(v) {
			clean = append(clean, v)
		}
	}
	values = clean

	if len(values) == 0 {
		return ""
	}
	if len(values) == 1 {
		return fmt.Sprintf("0.0,%.1f %.1f,%.1f", height/2, width, height/2)
	}

	min, max := values[0], values[0]
	for _, v := range values {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min

	var b strings.Builder
	step := width / float64(len(values)-1)
	for i, v := range values {
		if i > 0 {
			b.WriteByte(' ')
		}
		x := float64(i) * step
		y := height / 2
		if span > 0 {
			y = height - ((v-min)/span)*height
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return b.String()
}

// grafanaDashboardURL is the Grafana Public Dashboard share link for the
// resume-site dashboard (manifests/platform/kube-prometheus-stack/
// resume-site-dashboard.yaml) - enabled once, manually, via Grafana's API
// (not provisionable as code, see BOOTSTRAP.md). The access token is tied to
// that dashboard's fixed uid ("resume-site"), so it survives re-provisioning
// the dashboard JSON but not a share being disabled/re-created - and not
// losing Grafana's database, which a cluster rebuild does.
//
// Read from the environment rather than compiled in for exactly that reason:
// as a constant, regenerating the share meant editing Go, rebuilding an image
// and redeploying. As an env var set in the Deployment it's a one-line
// manifest change, which is what makes a rebuild a runbook step rather than a
// development task (#65).
//
// No fallback value. An unset variable hides the link (see index.html) rather
// than rendering one that 404s - a dead "View live dashboard" button is worse
// than no button, and this is the same reasoning as the "as of" timestamp on
// the visitor count: don't show something that looks live when it isn't.
var grafanaDashboardURL = os.Getenv("GRAFANA_DASHBOARD_URL")

// IndexData is what templates/index.html renders. Recent is the homepage's
// condensed preview - just the two most recent Experience entries.
type IndexData struct {
	Stats               Stats
	Resume              Resume
	Recent              []Experience
	GrafanaDashboardURL string
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
	data := IndexData{
		Stats:               stats(),
		Resume:              resume,
		Recent:              resume.Experience[:2],
		GrafanaDashboardURL: grafanaDashboardURL,
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

// statusHandler backs both the readiness and liveness probes. Once draining is
// set (see main.go), it answers 503 so kubelet takes this pod out of the
// Service's endpoints before connections stop being accepted - without that,
// requests routed in the instant before SIGTERM still land on a closing server.
//
// Liveness reads the same endpoint, but a failing liveness probe during
// termination is harmless: kubelet stops acting on probes once a pod is
// shutting down, and the process is exiting regardless.
func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	fmt.Fprintf(w, `{"buildTime":%q,"uptime":%q,"draining":%t}`,
		buildTime, time.Since(startTime).Round(time.Second).String(), draining.Load())
}
