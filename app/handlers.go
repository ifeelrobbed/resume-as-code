package main

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"os"
	"strconv"
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
// was last confirmed against blob storage, as a bare unix timestamp so the
// template can render it in the visitor's own timezone), process uptime,
// build time (see main.go's buildTime, injected via -ldflags), and 24h
// request-rate/p95-latency/error-rate sparklines.
type Stats struct {
	VisitorCount            string
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
	// Read from the durable blob-backed counter rather than Prometheus (#75).
	// The Prometheus version was increase(...[15d]) - a rolling window that
	// reset to zero whenever the TSDB was lost, and an extrapolated estimate
	// rather than a count: measured 34 against an exact 37 over the same
	// period. This is the exact number and it survives a cluster rebuild.
	rawCount, loaded, updatedAt := visitors.get()

	// A dash until the first successful read, never a zero. An unread count and
	// a genuinely empty one look identical otherwise, and a confident 0 on a
	// site whose point is that its numbers are real is the worse failure.
	// Formatted as a string for the same reason as the three stats below.
	count := "—"
	if loaded {
		count = strconv.FormatInt(rawCount, 10)
	}

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

// statusHandler is informational, and backs the liveness probe: it answers 200
// for as long as the process is alive, including while draining. Liveness asks
// "is this process wedged, restart it?", and a draining pod is neither wedged
// nor a restart candidate - it is leaving on purpose. Conflating that with
// readiness is what makes a drain look like a failure.
//
// draining is still reported, so curling this endpoint answers "why is that pod
// unready?" without needing cluster access.
func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// visitorsDurable is here to be compared against the Prometheus-derived
	// number on the homepage before that number switches source (#75, step 6).
	// visitorsLoaded distinguishes "genuinely zero" from "not read yet", which
	// is the difference between a real count and a placeholder.
	count, loaded, _ := visitors.get()

	fmt.Fprintf(w, `{"buildTime":%q,"uptime":%q,"draining":%t,"visitorsDurable":%d,"visitorsLoaded":%t}`,
		buildTime, time.Since(startTime).Round(time.Second).String(), draining.Load(), count, loaded)
}

// readyzHandler backs the readiness probe, and nothing else. Once draining is
// set (see main.go) it answers 503, which is what makes kubelet pull this pod
// out of the Service's endpoints before connections stop being accepted -
// without that, requests routed in the instant before SIGTERM still land on a
// closing server.
//
// Deliberately separate from statusHandler rather than sharing one endpoint.
// Readiness and liveness answer different questions, and a single endpoint has
// to answer both wrongly during a drain: either it stays 200 and the pod keeps
// receiving traffic it is about to stop serving, or it returns 503 and reports
// a live process as dead.
//
// Plain text rather than JSON: kubelet reads only the status code, and the
// body exists for whoever curls it by hand.
func readyzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "draining")
		return
	}
	fmt.Fprintln(w, "ok")
}
