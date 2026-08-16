package main

import "net/http"

// engagementTargets is an allowlist, not a passthrough - target comes from
// a query param on a public endpoint, so anyone (not just our own JS) can
// hit it with an arbitrary value. Feeding that straight into a Prometheus
// label would let a single curl loop create unbounded time series.
var engagementTargets = map[string]bool{
	"linkedin":      true,
	"github":        true,
	"github-source": true,
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
