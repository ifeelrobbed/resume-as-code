package main

import "net/http"

// engagementTargets is an allowlist, not a passthrough - target comes from
// a query param on a public endpoint, so anyone (not just our own JS) can
// hit it with an arbitrary value. Feeding that straight into a Prometheus
// label would let a single curl loop create unbounded time series.
// The nav pair are separate targets from the footer pair rather than sharing a
// label. The point of putting them in the nav (#49) is to reach a visitor who
// never scrolls to the footer, and merging the counts would make exactly that
// question unanswerable - whether the new placement gets used, or just moves
// clicks that would have happened anyway. Four series instead of two is a
// rounding error against the cardinality work in #73.
var engagementTargets = map[string]bool{
	"linkedin":          true,
	"github":            true,
	"nav-linkedin":      true,
	"nav-github":        true,
	"github-source":     true,
	"grafana-dashboard": true,
}

func engagementClickHandler(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if !engagementTargets[target] {
		http.Error(w, "unknown target", http.StatusBadRequest)
		return
	}
	engagementClicksTotal.WithLabelValues(target).Inc()
	w.WriteHeader(http.StatusNoContent)
}
