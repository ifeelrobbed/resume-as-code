package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postClick(t *testing.T, target string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	engagementClickHandler(rec, httptest.NewRequest(http.MethodPost, "/engagement/click?target="+target, nil))
	return rec.Code
}

// The allowlist is the only thing standing between a public endpoint and
// unbounded label cardinality, so an unknown target must be refused rather
// than counted.
func TestEngagementRejectsTargetsOutsideTheAllowlist(t *testing.T) {
	for _, target := range []string{"not-a-target", "", "nav-linkedin-x", "../etc"} {
		if code := postClick(t, target); code != http.StatusBadRequest {
			t.Errorf("target %q returned %d, want 400 - it would become a new time series", target, code)
		}
	}
}

func TestEngagementAcceptsEveryTargetTheTemplatesUse(t *testing.T) {
	for _, target := range []string{"linkedin", "github", "nav-linkedin", "nav-github", "github-source", "grafana-dashboard"} {
		if code := postClick(t, target); code != http.StatusNoContent {
			t.Errorf("target %q returned %d, want 204", target, code)
		}
	}
}

// The nav pair exist to reach a visitor who never scrolls to the footer (#49).
// Sharing a label with the footer pair would make it impossible to tell
// whether the new placement gets used or merely relocates clicks.
func TestNavAndFooterTargetsAreDistinct(t *testing.T) {
	for _, pair := range [][2]string{{"linkedin", "nav-linkedin"}, {"github", "nav-github"}} {
		if pair[0] == pair[1] {
			t.Fatalf("footer and nav share the target %q", pair[0])
		}
		for _, target := range pair {
			if !engagementTargets[target] {
				t.Errorf("%q is missing from the allowlist, so its clicks would 400", target)
			}
		}
	}
}

// Every data-engagement value the templates render has to be in the allowlist,
// or those clicks are silently dropped with a 400 nobody sees. Both pages are
// checked because they render the same nav.
func TestEveryRenderedEngagementTargetIsAllowed(t *testing.T) {
	for _, page := range []string{"index.html", "resume.html"} {
		body := renderPage(t, page)
		for _, target := range engagementAttrs(body) {
			if !engagementTargets[target] {
				t.Errorf("%s renders data-engagement=%q, which the allowlist rejects", page, target)
			}
		}
	}
}

// The swap #49 asked for: no contact anchor left in either nav, and both
// external links present on both pages.
func TestNavLinksReplacedTheContactAnchor(t *testing.T) {
	for _, page := range []string{"index.html", "resume.html"} {
		body := renderPage(t, page)
		nav := body[strings.Index(body, `class="nav-links`):]
		nav = nav[:strings.Index(nav, "</div>")]

		if strings.Contains(nav, "#contact") {
			t.Errorf("%s nav still links to the contact anchor", page)
		}
		if strings.Contains(nav, ">contact<") {
			t.Errorf("%s nav still shows a contact item", page)
		}
		for _, want := range []string{"linkedin.com/in/robertjohncameron", "github.com/ifeelrobbed"} {
			if !strings.Contains(nav, want) {
				t.Errorf("%s nav is missing %s", page, want)
			}
		}

		// The nav must use its own targets, not the footer's. Checking the
		// allowlist is not enough: reusing "linkedin" here would still be
		// allowed and still record a click, while silently merging the counts
		// and making it impossible to tell whether the nav placement works -
		// which is the only reason this change exists. Found by mutation; the
		// distinctness test above passed happily with the footer's label here.
		for _, want := range []string{`data-engagement="nav-linkedin"`, `data-engagement="nav-github"`} {
			if !strings.Contains(nav, want) {
				t.Errorf("%s nav does not carry %s - its clicks would merge with the footer's", page, want)
			}
		}
	}
}

// engagementAttrs pulls every data-engagement value out of rendered HTML.
func engagementAttrs(body string) []string {
	const marker = `data-engagement="`
	var out []string
	for rest := body; ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			return out
		}
		rest = rest[i+len(marker):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end:]
	}
}

// renderPage renders a template with the same data the handlers pass it.
func renderPage(t *testing.T, name string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	switch name {
	case "index.html":
		indexHandler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	case "resume.html":
		resumeHandler(rec, httptest.NewRequest(http.MethodGet, "/resume", nil))
	default:
		t.Fatalf("unknown page %q", name)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s rendered %d, want 200", name, rec.Code)
	}
	return rec.Body.String()
}
