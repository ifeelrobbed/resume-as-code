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
// StatSource is what a panel's hover tooltip shows: where the number actually
// comes from (#48). Grafana makes every panel's query visible to anyone who
// opens it; this is the same idea for a page nobody can open in an editor.
//
// Query is empty for the panels no query backs - uptime is computed in this
// process, last deploy is injected at image build, and the visitor count comes
// from blob storage. That absence is the signal, and it is why each Source
// names its own mechanism in plain terms rather than by contrast with the
// others: a reader who has never heard of this project should still be able to
// tell a queried number from a measured one, and phrasing like "not a query"
// only means something to someone who already knows what the alternative was.
type StatSource struct {
	Source string   // where it comes from, in words
	Query  string   // PromQL, when there is one
	Notes  []string // anything else worth knowing, one line each

	// TimestampISO is rendered as a <time> the page script reformats into the
	// reader's own timezone. It lives in the tooltip rather than in a title
	// attribute on the value: a title is a second tooltip nested inside this
	// one, which nothing advertises and a touch device cannot reach at all.
	TimestampISO string
}

type Stats struct {
	VisitorCount            string
	VisitorCountUpdatedUnix int64
	VisitorSparkline        string
	Uptime                  string
	LastDeploy              string
	SyncStatus              string
	SyncAllHealthy          bool
	RequestRateCurrent      string
	RequestRateSparkline    string
	P95LatencyCurrent       string
	P95LatencySparkline     string
	ErrorRateCurrent        string
	ErrorRateSparkline      string

	ArgoApps []argoApp

	VisitorSource     StatSource
	UptimeSource      StatSource
	LastDeploySource  StatSource
	SyncSource        StatSource
	RequestRateSource StatSource
	P95LatencySource  StatSource
	ErrorRateSource   StatSource
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

	// Visits per day, from the same blob as the number above it, so the line
	// and the count can never tell different stories (#44). Replaces a
	// hardcoded polyline that described no data at all.
	//
	// Two points minimum. sparklinePoints() renders a single value as a flat
	// centered line, which here would be indistinguishable from a real week of
	// identical traffic - and this history starts accumulating from the first
	// flush after deploy, so one point is genuinely the early state rather
	// than a hypothetical. An empty string renders an empty polyline, which
	// draws nothing, which is the honest answer until there is a trend to show.
	history := visitors.dailyHistory()
	visitorSparkline := ""
	if len(history) >= 2 {
		daily := make([]float64, len(history))
		for i, d := range history {
			daily[i] = float64(d.Count)
		}
		visitorSparkline = sparklinePoints(daily)
	}

	// Argo CD sync state across every Application, not just this one - the
	// panel is about whether the platform is converged, and the app-of-apps is
	// the more interesting thing to show. Degrades to a dash rather than
	// claiming health it hasn't confirmed (#43).
	argoApps, argoTotal, argoHealthy, argoLoaded, _ := argoSync.get()
	syncStatus := "—"
	syncAllHealthy := false
	if argoLoaded {
		syncStatus = fmt.Sprintf("%d/%d Synced · Healthy", argoHealthy, argoTotal)
		syncAllHealthy = argoHealthy == argoTotal && argoTotal > 0
	}

	// One decimal, because the query now yields requests per hour rather than
	// per second - a whole number would round a quiet afternoon to 0.
	ratePoints, _ := requestRate.get()
	rateCurrent := "—"
	if n := len(ratePoints); n > 0 && !math.IsNaN(ratePoints[n-1]) {
		rateCurrent = fmt.Sprintf("%.1f", ratePoints[n-1])
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

	// Computed here from the cached start instant rather than read as a
	// pre-formatted number, so it advances every second between the minute-
	// apart polls behind it.
	//
	// A dash when Prometheus is unreachable, rather than falling back to this
	// process's own startTime. That fallback is tempting and currently
	// accurate - there is one replica - but it is exactly the per-pod number
	// #141 removed, and it would start disagreeing between pods the moment
	// #142 adds a second. /status keeps the local value for the pod-level
	// question; this panel does not answer that question.
	uptime := "—"
	if started, ok := podStart.get(); ok {
		uptime = time.Since(started).Round(time.Second).String()
	}

	// Panel provenance (#48). The queries are the same constants the pollers
	// run, referenced rather than retyped, so a tooltip cannot drift into
	// describing a query the app no longer makes.
	visitorNotes := []string{
		"the number: exact all-time count",
		"the line: visits per day, last 30 days",
	}
	if loaded {
		visitorNotes = append(visitorNotes, fmt.Sprintf("%s all-time · %d today (UTC)", count, visitsToday(history)))
	}

	return Stats{
		VisitorCount:            count,
		VisitorCountUpdatedUnix: updatedUnix,
		VisitorSparkline:        visitorSparkline,
		ArgoApps:                argoApps,

		VisitorSource: StatSource{
			Source: "Stored in Azure Blob Storage",
			Notes:  visitorNotes,
		},
		UptimeSource: StatSource{
			Source: "Prometheus, from the pods' own process metrics",
			Query:  podStartQuery,
			Notes: []string{
				"the youngest pod's process start time, counted forward",
				"a pod, not the site — it resets whenever a pod restarts, deploy or not",
				"below last deploy means something restarted the pods on its own",
				"reads — when Prometheus cannot be reached",
			},
		},
		LastDeploySource: StatSource{
			Source: "Stamped into the image when it was built",
			Notes: []string{
				`baked in with -ldflags "-X main.buildTime"`,
				"the panel counts forward from this moment",
			},
			TimestampISO: buildTime,
		},
		SyncSource: StatSource{
			Source: "Prometheus, scraped from the Argo CD controller",
			Query:  argoAppsQuery,
			Notes:  []string{"counted healthy only when both Synced and Healthy"},
		},
		RequestRateSource: StatSource{
			Source: "Prometheus",
			Query:  requestRateQuery,
			Notes: []string{
				"the number: the most recent 3 hours, as an hourly rate",
				"the line: the last 24 hours",
			},
		},
		P95LatencySource: StatSource{
			Source: "Prometheus",
			Query:  p95LatencyQuery,
			Notes: []string{
				"the number: the most recent 3 hours",
				"the line: the last 24 hours",
				"each point spans 3 hours so quiet stretches still hold a sample",
				"reads — when no requests at all landed in the window",
			},
		},
		ErrorRateSource: StatSource{
			Source: "Prometheus, from a pre-computed rule",
			Query:  errorRateQuery,
			Notes: []string{
				"the number: the most recent 5 minutes",
				"the line: the last 24 hours",
				"reports nothing at all while there are no errors, which is why this can read —",
			},
		},
		Uptime:                  uptime,
		LastDeploy:              buildTime,
		SyncStatus:              syncStatus,
		SyncAllHealthy:          syncAllHealthy,
		RequestRateCurrent:      rateCurrent,
		RequestRateSparkline:    sparklinePoints(ratePoints),
		P95LatencyCurrent:       latencyCurrent,
		P95LatencySparkline:     sparklinePoints(latencyPoints),
		ErrorRateCurrent:        errorCurrent,
		ErrorRateSparkline:      sparklinePoints(errorPoints),
	}
}

// visitsToday returns the current UTC day's count from the history behind the
// sparkline, or 0 before any visit has been recorded today. Bucketed in UTC
// like the rest of the history, which is why the tooltip says so - a visitor
// west of Greenwich reading this late in their evening is already on the next
// UTC day.
func visitsToday(history []dayCount) int64 {
	today := utcDay(time.Now())
	for _, d := range history {
		if d.Day == today {
			return d.Count
		}
	}
	return 0
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

// siteBaseURL is the canonical origin, and the only reason it exists is that
// Open Graph requires absolute URLs - a crawler resolves og:image against
// nothing, so a relative path yields no thumbnail at all.
//
// A constant rather than an env var, unlike grafanaDashboardURL just above.
// That one is configuration because it genuinely changes: the share token dies
// with Grafana's database, so a cluster rebuild has to be able to fix the link
// by editing a manifest. This does not change - it is the same hostname that
// manifests/apps/resume-site/deploy/ingress.yaml terminates TLS for and that
// the blackbox exporter probes. Deriving it from the request Host instead would
// make it vary by however the crawler arrived, which is the one thing a
// canonical URL must not do: a probe or a port-forward hitting the pod directly
// would publish og:url pointing at a pod IP.
//
// No trailing slash - pageMeta joins paths onto it.
//
// Built from canonicalHost so the hostname is written once. canonicalHostRedirect
// in main.go sends "www." + canonicalHost here, and the two disagreeing would
// mean redirecting visitors to a host the metadata doesn't advertise.
const canonicalHost = "robertjcameron.com"
const siteBaseURL = "https://" + canonicalHost

// ogImageWidth/ogImageHeight must match the PNG that `make og-image` produces.
// Declaring them lets a crawler reserve the right space before it has fetched
// the image; LinkedIn in particular renders a large card immediately rather
// than falling back to a small one while it measures.
const (
	ogImagePath   = "/static/og-image.png"
	ogImageWidth  = "1200"
	ogImageHeight = "630"

	// Read out by screen readers in place of the card, so it describes what the
	// card says rather than naming the file.
	ogImageAlt = "A dark card reading resume-as-code above robertjcameron.com, " +
		"over four linked boxes: Terraform, AKS, GitOps, and a highlighted live site."
)

// Meta is the per-page Open Graph and description metadata (see the "meta"
// template in templates/). Every page renders the same card image; only the
// title, description and URL differ, which is why this is built per handler
// rather than folded into Stats.
type Meta struct {
	Title       string
	Description string
	URL         string
	Image       string
	ImageWidth  string
	ImageHeight string
	ImageAlt    string
}

// pageMeta builds the metadata for one page. path is site-relative and must
// start with "/".
//
// The descriptions passed in are written for this purpose rather than reusing
// Resume.Bio: the bio's last clause is "the stats below are read from the
// cluster it's deployed on", which reads as a broken promise in a LinkedIn
// preview where there is nothing below it.
func pageMeta(path, title, description string) Meta {
	return Meta{
		Title:       title,
		Description: description,
		URL:         siteBaseURL + path,
		Image:       siteBaseURL + ogImagePath,
		ImageWidth:  ogImageWidth,
		ImageHeight: ogImageHeight,
		ImageAlt:    ogImageAlt,
	}
}

// IndexData is what templates/index.html renders. Recent is the homepage's
// condensed preview - just the two most recent Experience entries.
type IndexData struct {
	Stats               Stats
	Resume              Resume
	Recent              []Experience
	GrafanaDashboardURL string
	Meta                Meta
}

// ResumeData is what templates/resume.html renders. SpecYAML is the same
// Resume data marshaled to YAML and syntax-colored (see yaml.go) - the
// "spec" view is never hand-duplicated content, just a different
// projection of the same source.
type ResumeData struct {
	Stats    Stats
	Resume   Resume
	SpecYAML template.HTML
	Meta     Meta
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	data := IndexData{
		Stats:               stats(),
		Resume:              resume,
		Recent:              resume.Experience[:2],
		GrafanaDashboardURL: grafanaDashboardURL,
		Meta: pageMeta("/",
			resume.Name+" - "+resume.Title,
			"A resume that deploys itself: Terraform to AKS, GitOps with Argo CD, "+
				"and the cluster's own live metrics rendered on the page."),
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
		Meta: pageMeta("/resume",
			resume.Name+" - Resume",
			"Twenty years in IT as a rendered timeline, and the same data again "+
				"as a Kubernetes-style spec."),
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
