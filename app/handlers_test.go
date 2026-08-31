package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// renderIndex serves one request against indexHandler with grafanaDashboardURL
// set to url, restoring it afterwards. The package var is set directly rather
// than through the environment because it's read once at package
// initialization, long before any test runs.
func renderIndex(t *testing.T, url string) (int, string) {
	t.Helper()

	original := grafanaDashboardURL
	grafanaDashboardURL = url
	t.Cleanup(func() { grafanaDashboardURL = original })

	rec := httptest.NewRecorder()
	indexHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Code, rec.Body.String()
}

func TestIndexRendersDashboardLinkWhenConfigured(t *testing.T) {
	const url = "https://grafana.example.com/public-dashboards/abc123"

	code, body := renderIndex(t, url)

	if code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
	if !strings.Contains(body, url) {
		t.Error("configured dashboard URL is missing from the page")
	}
	if !strings.Contains(body, "View live dashboard") {
		t.Error("dashboard link text is missing")
	}
	// The engagement tracker keys off this attribute; losing it would make
	// grafana-dashboard clicks silently stop being counted.
	if !strings.Contains(body, `data-engagement="grafana-dashboard"`) {
		t.Error("dashboard link lost its data-engagement attribute")
	}
}

// The case this change exists for: after a cluster rebuild the share token is
// gone, and a dead "View live dashboard" button is worse than no button.
func TestIndexHidesDashboardLinkWhenUnset(t *testing.T) {
	code, body := renderIndex(t, "")

	if code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
	if strings.Contains(body, "View live dashboard") {
		t.Error("dashboard link is rendered even though no URL is configured")
	}
	if strings.Contains(body, `data-engagement="grafana-dashboard"`) {
		t.Error("dashboard link markup is rendered even though no URL is configured")
	}
	// Absent link, present page - guards against the template silently
	// failing and returning an empty body that would pass the checks above.
	if !strings.Contains(body, "observability") {
		t.Error("observability section missing; the page did not render properly")
	}
}

// An empty href would still render a clickable element that navigates to the
// current page, which is the failure the {{if}} guard is meant to prevent.
func TestIndexNeverRendersAnEmptyDashboardHref(t *testing.T) {
	_, body := renderIndex(t, "")

	if strings.Contains(body, `href=""`) {
		t.Error("page contains an empty href")
	}
}

// setVisitors points the package counter at a known state and restores it, so
// display tests don't depend on whatever else ran first. history is the
// per-day series behind the sparkline; pass nil when the test only cares about
// the number.
func setVisitors(t *testing.T, count int64, loaded bool, history []dayCount) {
	t.Helper()
	visitors.mu.Lock()
	prevTotal, prevDelta, prevLoaded, prevHistory := visitors.total, visitors.delta, visitors.loaded, visitors.history
	visitors.total, visitors.delta, visitors.loaded, visitors.history = count, nil, loaded, history
	visitors.mu.Unlock()

	t.Cleanup(func() {
		visitors.mu.Lock()
		visitors.total, visitors.delta, visitors.loaded, visitors.history = prevTotal, prevDelta, prevLoaded, prevHistory
		visitors.mu.Unlock()
	})
}

// days builds a history from consecutive counts starting at an arbitrary fixed
// date - the dates themselves never reach the sparkline, only their order does.
func days(counts ...int64) []dayCount {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	out := make([]dayCount, len(counts))
	for i, n := range counts {
		out[i] = dayCount{Day: utcDay(start.AddDate(0, 0, i)), Count: n}
	}
	return out
}

// daysEndingToday is for the tooltip's "N today" line, which looks the current
// UTC day up by name - a fixed start date would never match it.
func daysEndingToday(counts ...int64) []dayCount {
	today := time.Now().UTC()
	out := make([]dayCount, len(counts))
	for i, n := range counts {
		day := today.AddDate(0, 0, -(len(counts) - 1 - i))
		out[i] = dayCount{Day: utcDay(day), Count: n}
	}
	return out
}

func TestHomepageShowsVisitorCountWhenLoaded(t *testing.T) {
	setVisitors(t, 4102, true, nil)

	code, body := renderIndex(t, "")
	if code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
	if !strings.Contains(body, "4102") {
		t.Error("visitor count is missing from the page")
	}
	// The label lost its window qualifier when the source stopped being a
	// rolling 15-day Prometheus query.
	if strings.Contains(body, "visitors (15d)") {
		t.Error("stale '(15d)' label - the count is no longer a rolling window")
	}
}

// The case the dash exists for: nothing has been read from blob storage yet,
// and a confident 0 would be indistinguishable from a genuinely empty count.
func TestHomepageShowsDashWhenCountNotYetLoaded(t *testing.T) {
	setVisitors(t, 0, false, nil)

	code, body := renderIndex(t, "")
	if code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
	if !strings.Contains(body, "—") {
		t.Error("expected a dash placeholder while the count is unknown")
	}
	if strings.Contains(body, `class="stat-value">0<`) {
		t.Error("rendered a literal 0 for an unread count - that is the failure this guards against")
	}
}

// A real zero must still render as 0 once it has actually been read, otherwise
// the dash would hide a true value.
func TestHomepageShowsZeroWhenGenuinelyZero(t *testing.T) {
	setVisitors(t, 0, true, nil)

	_, body := renderIndex(t, "")
	if !strings.Contains(body, `class="stat-value">0<`) {
		t.Error("a loaded zero should render as 0, not as a dash")
	}
}

// setArgoSync points the package cache at a known state and restores it.
// Counts are derived from the app list rather than set directly, so tests
// exercise the same derivation the page does - synthesising a healthy count
// that the list could not produce would test nothing real.
func setArgoSync(t *testing.T, total, healthy int, loaded bool) {
	t.Helper()
	apps := make([]argoApp, 0, total)
	for i := 0; i < total; i++ {
		a := argoApp{Name: fmt.Sprintf("app-%02d", i), Sync: "Synced", Health: "Healthy"}
		if i >= healthy {
			a.Sync, a.Health = "OutOfSync", "Degraded"
		}
		apps = append(apps, a)
	}
	setArgoApps(t, apps, loaded)
}

// setArgoApps is the same, for tests that care about the exact list the
// tooltip renders rather than just the counts.
func setArgoApps(t *testing.T, apps []argoApp, loaded bool) {
	t.Helper()
	argoSync.mu.Lock()
	prevApps, prevLoaded := argoSync.apps, argoSync.loaded
	argoSync.apps, argoSync.loaded = apps, loaded
	argoSync.mu.Unlock()

	t.Cleanup(func() {
		argoSync.mu.Lock()
		argoSync.apps, argoSync.loaded = prevApps, prevLoaded
		argoSync.mu.Unlock()
	})
}

func TestSyncPanelShowsAllHealthy(t *testing.T) {
	setArgoSync(t, 7, 7, true)

	_, body := renderIndex(t, "")
	if !strings.Contains(body, "7/7 Synced") {
		t.Error("expected the aggregate count on the page")
	}
	if strings.Contains(body, "stat-sync degraded") {
		t.Error("marked degraded while everything is healthy")
	}
	// The placeholder this replaced must not survive anywhere.
	if strings.Contains(body, `<span class="pulse-dot"></span>Synced &middot; Healthy`) {
		t.Error("hardcoded placeholder still present")
	}
}

// The case the panel exists for: something has drifted and the page says so
// rather than continuing to claim health.
func TestSyncPanelShowsDegraded(t *testing.T) {
	setArgoSync(t, 7, 6, true)

	_, body := renderIndex(t, "")
	if !strings.Contains(body, "6/7 Synced") {
		t.Error("expected the degraded count on the page")
	}
	if !strings.Contains(body, "stat-sync degraded") {
		t.Error("not marked degraded - the dot would stay green beside a 6/7")
	}
}

// Nothing read from Prometheus yet must not render as healthy. Same reasoning
// as the visitor count's dash: unknown and fine look identical otherwise.
func TestSyncPanelShowsDashWhenUnknown(t *testing.T) {
	setArgoSync(t, 0, 0, false)

	_, body := renderIndex(t, "")
	if strings.Contains(body, "Synced ·") {
		t.Error("claimed sync state before reading any")
	}
	if !strings.Contains(body, "stat-sync degraded") {
		t.Error("unknown state should not render as green")
	}
}

// A cluster with zero Applications is not "all healthy" - 0/0 must not be
// treated as success by the total>0 guard.
func TestSyncPanelZeroApplicationsIsNotHealthy(t *testing.T) {
	setArgoSync(t, 0, 0, true)

	_, body := renderIndex(t, "")
	if !strings.Contains(body, "stat-sync degraded") {
		t.Error("0/0 should not render as healthy")
	}
}

// --- visitors sparkline (#44) ----------------------------------------------

// The placeholder this replaced. It described no data at all, on a page whose
// premise is that its numbers are real.
const hardcodedVisitorPolyline = `points="0,20 15,18 30,15 45,16 60,9 75,7 90,3"`

func TestVisitorSparklineRendersRealHistory(t *testing.T) {
	setVisitors(t, 100, true, days(10, 20, 40, 30))

	_, body := renderIndex(t, "")
	if strings.Contains(body, hardcodedVisitorPolyline) {
		t.Error("hardcoded placeholder polyline is still being rendered")
	}
	// Four days across the 90x24 viewBox: x steps 0/30/60/90, and y is
	// 24-((v-min)/span)*24 with min=10 and span=30 - so the low day sits on
	// the floor at 24.0 and the peak (40) is pinned to the top at 0.0.
	if !strings.Contains(body, `points="0.0,24.0 30.0,16.0 60.0,0.0 90.0,8.0"`) {
		t.Errorf("visitor sparkline points are wrong or missing; body:\n%s", visitorStatBlock(body))
	}
}

// One day is genuinely the state right after this ships. sparklinePoints would
// render it as a flat centered line, which looks like a real flat trend rather
// than an absence of one.
func TestVisitorSparklineIsEmptyWithASingleDay(t *testing.T) {
	setVisitors(t, 5, true, days(5))

	_, body := renderIndex(t, "")
	if !strings.Contains(body, `<polyline points="" />`) {
		t.Errorf("expected an empty polyline with only one day of history; got:\n%s", visitorStatBlock(body))
	}
	if strings.Contains(body, "0.0,12.0 90.0,12.0") {
		t.Error("rendered a flat centered line for a single day - indistinguishable from real flat traffic")
	}
}

func TestVisitorSparklineIsEmptyBeforeAnythingIsLoaded(t *testing.T) {
	setVisitors(t, 0, false, nil)

	_, body := renderIndex(t, "")
	if strings.Contains(body, hardcodedVisitorPolyline) {
		t.Error("hardcoded placeholder rendered while nothing is loaded")
	}
	if !strings.Contains(body, `<polyline points="" />`) {
		t.Error("expected an empty polyline before the first successful read")
	}
}

// visitorStatBlock extracts the visitors stat panel for readable failures.
func visitorStatBlock(body string) string {
	start := strings.Index(body, `<p class="stat-label">visitors</p>`)
	if start < 0 {
		return "(visitors panel not found)"
	}
	end := start + 400
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}

// --- last deploy timestamp (#42) -------------------------------------------

// The script reformats this client-side, so the server's job is to emit a
// parseable machine value. If the attribute or its format changes, the page
// silently falls back to showing a raw timestamp - which is exactly what #42
// set out to remove, and would not fail anything else.
func TestLastDeployRendersAParseableDatetimeAttribute(t *testing.T) {
	original := buildTime
	buildTime = "2026-08-31T01:33:01Z"
	t.Cleanup(func() { buildTime = original })

	_, body := renderIndex(t, "")

	want := `<time datetime="2026-08-31T01:33:01Z">2026-08-31T01:33:01Z</time>`
	if !strings.Contains(body, want) {
		t.Errorf("expected %s in the page", want)
	}
	// Parseable by Date.parse, which is what the script relies on. RFC3339 is
	// the subset of ISO 8601 every browser handles.
	if _, err := time.Parse(time.RFC3339, buildTime); err != nil {
		t.Errorf("buildTime %q is not RFC3339, so Date.parse would return NaN: %v", buildTime, err)
	}
}

// Without JS the element must still show the real timestamp. Rendering an
// empty element would trade an ugly value for no value at all.
func TestLastDeployHasANoScriptFallback(t *testing.T) {
	original := buildTime
	buildTime = "2026-08-31T01:33:01Z"
	t.Cleanup(func() { buildTime = original })

	_, body := renderIndex(t, "")
	if strings.Contains(body, `<time datetime="2026-08-31T01:33:01Z"></time>`) {
		t.Error("time element is empty - a visitor without JS would see nothing")
	}
}

// buildTime is "dev" for a local `go run`. The page must still render, and the
// script is expected to leave the value alone rather than print "NaN ago".
func TestLastDeployHandlesTheDevBuildTime(t *testing.T) {
	original := buildTime
	buildTime = "dev"
	t.Cleanup(func() { buildTime = original })

	code, body := renderIndex(t, "")
	if code != http.StatusOK {
		t.Fatalf("got status %d, want 200", code)
	}
	if !strings.Contains(body, `<time datetime="dev">dev</time>`) {
		t.Error("expected the dev build time to render unchanged")
	}
}

// --- panel source tooltips (#48) -------------------------------------------

// The point of the tooltips is that they are true. A panel not backed by
// Prometheus must not display a query, because a plausible-looking query under
// a number that never ran one is worse than saying nothing.
func TestOnlyQueryBackedPanelsShowAQuery(t *testing.T) {
	s := stats()

	withQuery := map[string]StatSource{
		"sync":       s.SyncSource,
		"req/s":      s.RequestRateSource,
		"p95":        s.P95LatencySource,
		"error rate": s.ErrorRateSource,
	}
	for name, src := range withQuery {
		if src.Query == "" {
			t.Errorf("%s is Prometheus-backed but shows no query", name)
		}
	}

	withoutQuery := map[string]StatSource{
		"visitors":    s.VisitorSource,
		"uptime":      s.UptimeSource,
		"last deploy": s.LastDeploySource,
	}
	for name, src := range withoutQuery {
		if src.Query != "" {
			t.Errorf("%s is not Prometheus-backed but shows query %q", name, src.Query)
		}
		if src.Source == "" {
			t.Errorf("%s has no source description, so its tooltip would be blank", name)
		}
	}
}

// Tooltips quote the same constants the pollers run. Retyping them would let a
// tooltip drift into describing a query the app no longer makes - which is the
// exact failure #48 exists to prevent, and would never fail anything else.
func TestTooltipQueriesAreTheQueriesActuallyRun(t *testing.T) {
	s := stats()
	for _, tc := range []struct {
		name, got, want string
	}{
		{"req/s", s.RequestRateSource.Query, requestRateQuery},
		{"p95", s.P95LatencySource.Query, p95LatencyQuery},
		{"error rate", s.ErrorRateSource.Query, errorRateQuery},
		{"sync", s.SyncSource.Query, argoAppsQuery},
	} {
		if tc.got != tc.want {
			t.Errorf("%s tooltip shows %q, but the poller runs %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestSyncTooltipListsEveryApplication(t *testing.T) {
	setArgoApps(t, []argoApp{
		{Name: "argocd", Sync: "Synced", Health: "Healthy"},
		{Name: "ingress-nginx", Sync: "OutOfSync", Health: "Degraded"},
	}, true)

	_, body := renderIndex(t, "")
	for _, want := range []string{"argocd", "ingress-nginx", "OutOfSync", "Degraded"} {
		if !strings.Contains(body, want) {
			t.Errorf("sync tooltip is missing %q", want)
		}
	}
	// The unhealthy row must be marked, or a list of seven names in one colour
	// makes the reader find the odd one out by eye.
	if !strings.Contains(body, `stat-tip-app degraded`) {
		t.Error("the OutOfSync application is not marked degraded in the tooltip")
	}
}

// Before the first successful poll there is nothing to list, and the tooltip
// must not imply an empty cluster.
func TestSyncTooltipListsNothingBeforeTheFirstPoll(t *testing.T) {
	setArgoApps(t, nil, false)

	_, body := renderIndex(t, "")
	if strings.Contains(body, "stat-tip-apps") {
		t.Error("rendered an application list before anything was read")
	}
	// The query itself is still worth showing - it is true regardless.
	if !strings.Contains(body, argoAppsQuery) {
		t.Error("sync tooltip dropped its query when no data was loaded")
	}
}

func TestVisitorTooltipExplainsValueVersusLine(t *testing.T) {
	setVisitors(t, 117, true, daysEndingToday(10, 23))

	_, body := renderIndex(t, "")
	for _, want := range []string{
		"Azure Blob Storage",
		"exact all-time count",
		"visits per day",
		"117 all-time · 23 today (UTC)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("visitor tooltip is missing %q", want)
		}
	}
}

// Nothing read yet: the tooltip may describe the source, but must not state a
// count it does not have.
func TestVisitorTooltipOmitsCountsBeforeLoad(t *testing.T) {
	setVisitors(t, 0, false, nil)

	_, body := renderIndex(t, "")
	if strings.Contains(body, "all-time ·") {
		t.Error("tooltip stated a visitor count before anything was read")
	}
	if !strings.Contains(body, "Azure Blob Storage") {
		t.Error("tooltip dropped its source description when nothing was loaded")
	}
}

// Every panel must be reachable without a mouse.
func TestEveryStatPanelIsFocusable(t *testing.T) {
	_, body := renderIndex(t, "")
	panels := strings.Count(body, `<div class="stat"`)
	focusable := strings.Count(body, `<div class="stat" tabindex="0">`)
	if panels != focusable {
		t.Errorf("%d stat panels but %d are focusable - the rest are hover-only", panels, focusable)
	}
	if tips := strings.Count(body, `class="stat-tip"`); tips != focusable {
		t.Errorf("%d focusable panels but %d tooltips", focusable, tips)
	}
}
