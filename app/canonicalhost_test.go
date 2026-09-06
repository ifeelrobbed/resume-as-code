package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// serveThroughRedirect runs one request through canonicalHostRedirect wrapping a
// handler that records whether it was reached, and returns the response plus
// that flag. The inner handler stands in for newMux(): what matters is only
// whether the middleware passed the request through or answered it itself.
func serveThroughRedirect(t *testing.T, host, target string) (*http.Response, bool) {
	t.Helper()

	var reached bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host

	rec := httptest.NewRecorder()
	canonicalHostRedirect(inner).ServeHTTP(rec, req)
	return rec.Result(), reached
}

// The whole point: a www visitor lands on the apex, on the page they asked for.
// Redirecting to the homepage instead is the specific failure that ruled out
// ingress-nginx's permanent-redirect annotation, so the path and query being
// carried over is the assertion that matters here.
func TestWWWRedirectsToApexPreservingPathAndQuery(t *testing.T) {
	for _, target := range []string{"/", "/resume", "/resume?filter=Go", "/static/style.css"} {
		resp, reached := serveThroughRedirect(t, "www."+canonicalHost, target)

		if reached {
			t.Errorf("%s: request reached the mux instead of being redirected", target)
		}
		// 308, not 301: a 301 turns a POST into a GET, and /engagement/click
		// is a POST.
		if resp.StatusCode != http.StatusPermanentRedirect {
			t.Errorf("%s: got status %d, want %d", target, resp.StatusCode, http.StatusPermanentRedirect)
		}
		if want := siteBaseURL + target; resp.Header.Get("Location") != want {
			t.Errorf("%s: Location is %q, want %q", target, resp.Header.Get("Location"), want)
		}
	}
}

// The apex itself must pass straight through. A middleware that redirected
// everything would loop forever, and it sits in front of every route.
func TestCanonicalHostIsNotRedirected(t *testing.T) {
	for _, host := range []string{canonicalHost, canonicalHost + ":8080"} {
		resp, reached := serveThroughRedirect(t, host, "/resume")

		if !reached {
			t.Errorf("%s: request was redirected instead of served", host)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: got status %d, want 200", host, resp.StatusCode)
		}
	}
}

// Hostnames are case-insensitive and a client may send any casing, so matching
// on the literal lowercase string would let WWW through to be served on the
// non-canonical host - the exact duplicate-content situation the redirect
// exists to prevent.
func TestWWWMatchIsCaseInsensitiveAndPortTolerant(t *testing.T) {
	for _, host := range []string{
		"WWW." + canonicalHost,
		"Www." + canonicalHost,
		"www." + canonicalHost + ":8080",
	} {
		resp, reached := serveThroughRedirect(t, host, "/")

		if reached {
			t.Errorf("%s: was served instead of redirected", host)
		}
		if resp.StatusCode != http.StatusPermanentRedirect {
			t.Errorf("%s: got status %d, want %d", host, resp.StatusCode, http.StatusPermanentRedirect)
		}
	}
}

// Anything else is served rather than redirected. In the cluster the probes
// reach the pod by IP and kubelet sends that IP as the Host, so a middleware
// that redirected unknown hosts would fail both probes and take the pod out of
// service. Localhost matters for `make run` and the container smoke test.
func TestOtherHostsAreServedNotRedirected(t *testing.T) {
	for _, host := range []string{"localhost:8080", "10.244.1.61:8080", "resume-site.resume-site.svc"} {
		_, reached := serveThroughRedirect(t, host, "/readyz")

		if !reached {
			t.Errorf("%s: was redirected; probes and local runs would break", host)
		}
	}
}

// canonicalHostRedirect builds its target from siteBaseURL, which the page
// metadata also uses. If the two ever named different hosts, visitors would be
// redirected to one host while og:url advertised another.
func TestRedirectTargetMatchesAdvertisedCanonicalURL(t *testing.T) {
	if want := "https://" + canonicalHost; siteBaseURL != want {
		t.Fatalf("siteBaseURL is %q, want %q", siteBaseURL, want)
	}

	meta := pageMeta("/resume", "t", "d")
	resp, _ := serveThroughRedirect(t, "www."+canonicalHost, "/resume")

	if got := resp.Header.Get("Location"); got != meta.URL {
		t.Errorf("redirect sends visitors to %q but og:url advertises %q", got, meta.URL)
	}
}
