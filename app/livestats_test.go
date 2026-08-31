package main

import (
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"os"
	"testing"
	"time"
)

// The code under test logs on every failure path, which is the point - but it
// makes `go test -v` unreadable. Discard it; the assertions cover behavior.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// prometheusStub serves one canned status/body for any request, standing in
// for a Prometheus that is healthy, restarting, or answering a bad query.
func prometheusStub(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func testClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func TestQueryRange(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		want        []float64
		wantNoData  bool
		wantSomeErr bool
	}{
		{
			name:   "valid series",
			status: http.StatusOK,
			body:   `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1756000000,"0.5"],[1756001800,"1.5"],[1756003600,"2.5"]]}]}}`,
			want:   []float64{0.5, 1.5, 2.5},
		},
		{
			// histogram_quantile() emits NaN for a step with no samples;
			// sparklinePoints() strips those downstream, so parsing must keep
			// them rather than reject the series.
			name:   "NaN samples survive parsing",
			status: http.StatusOK,
			body:   `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1756000000,"NaN"],[1756001800,"1.5"]]}]}}`,
			want:   []float64{math.NaN(), 1.5},
		},
		{
			name:       "empty result set",
			status:     http.StatusOK,
			body:       `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
			wantNoData: true,
		},
		{
			name:        "service unavailable",
			status:      http.StatusServiceUnavailable,
			body:        `503 Service Temporarily Unavailable`,
			wantSomeErr: true,
		},
		{
			name:        "unparseable sample fails the whole series",
			status:      http.StatusOK,
			body:        `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1756000000,"0.5"],[1756001800,"banana"]]}]}}`,
			wantSomeErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := queryRange(testClient(), prometheusStub(t, tt.status, tt.body), requestRateQuery)

			switch {
			case tt.wantNoData:
				if !errors.Is(err, errNoData) {
					t.Fatalf("got err %v, want errNoData", err)
				}
			case tt.wantSomeErr:
				if err == nil {
					t.Fatalf("got %v and nil error, want an error", got)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(got) != len(tt.want) {
					t.Fatalf("got %d points, want %d", len(got), len(tt.want))
				}
				for i := range tt.want {
					if math.IsNaN(tt.want[i]) {
						if !math.IsNaN(got[i]) {
							t.Errorf("point %d: got %v, want NaN", i, got[i])
						}
						continue
					}
					if got[i] != tt.want[i] {
						t.Errorf("point %d: got %v, want %v", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestRefreshSparklinePreservesCacheOnFailure(t *testing.T) {
	var cache sparklineCache

	healthy := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1756000000,"1"],[1756001800,"2"]]}]}}`
	refreshSparkline(testClient(), prometheusStub(t, http.StatusOK, healthy), "test", requestRateQuery, &cache)

	got, updatedAt := cache.get()
	if len(got) != 2 {
		t.Fatalf("setup: got %d points, want 2", len(got))
	}

	refreshSparkline(testClient(), prometheusStub(t, http.StatusServiceUnavailable, "down"), "test", requestRateQuery, &cache)

	gotAfter, updatedAfter := cache.get()
	if len(gotAfter) != 2 {
		t.Errorf("got %d points after a failed poll, want the cached 2 held", len(gotAfter))
	}
	if !updatedAfter.Equal(updatedAt) {
		t.Errorf("updatedAt moved on a failed poll (%v -> %v)", updatedAt, updatedAfter)
	}
}

func TestQueryArgoApps(t *testing.T) {
	const twoApps = `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"name":"root","sync_status":"Synced","health_status":"Healthy"},"value":[1756000000,"1"]},` +
		`{"metric":{"name":"argocd","sync_status":"OutOfSync","health_status":"Degraded"},"value":[1756000000,"1"]}]}}`

	tests := []struct {
		name        string
		status      int
		body        string
		want        []argoApp
		wantNoData  bool
		wantSomeErr bool
	}{
		{
			// Sorted by name, not returned in Prometheus's order - the input
			// here is deliberately reversed. Without this the tooltip would
			// reshuffle its rows between polls.
			name:   "series are reduced and sorted by name",
			status: http.StatusOK,
			body:   twoApps,
			want: []argoApp{
				{Name: "argocd", Sync: "OutOfSync", Health: "Degraded"},
				{Name: "root", Sync: "Synced", Health: "Healthy"},
			},
		},
		{
			// Prometheus reachable but the controller is not being scraped.
			// Unknown, not "zero applications" - the panel shows a dash.
			name:       "empty result",
			status:     http.StatusOK,
			body:       `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			wantNoData: true,
		},
		{
			// A series with no name label cannot be listed. Skipping it beats
			// rendering a blank row; if that leaves nothing, it is no data.
			name:       "series without a name label are skipped",
			status:     http.StatusOK,
			body:       `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]}]}}`,
			wantNoData: true,
		},
		{
			name:        "prometheus unavailable",
			status:      http.StatusServiceUnavailable,
			body:        `503 Service Temporarily Unavailable`,
			wantSomeErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := queryArgoApps(testClient(), prometheusStub(t, tt.status, tt.body))
			switch {
			case tt.wantNoData:
				if !errors.Is(err, errNoData) {
					t.Fatalf("got err %v, want errNoData", err)
				}
			case tt.wantSomeErr:
				if err == nil {
					t.Fatalf("got %v and nil error, want an error", got)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// The count behind "6/7" and the tooltip's per-application list come from the
// same slice, so they cannot disagree - this pins that they are derived, not
// stored separately.
func TestArgoCacheDerivesCountsFromTheAppList(t *testing.T) {
	var cache argoSyncCache
	cache.set([]argoApp{
		{Name: "a", Sync: "Synced", Health: "Healthy"},
		{Name: "b", Sync: "Synced", Health: "Healthy"},
		{Name: "c", Sync: "OutOfSync", Health: "Healthy"},
		{Name: "d", Sync: "Synced", Health: "Degraded"},
	})

	apps, total, healthy, loaded, _ := cache.get()
	if !loaded {
		t.Fatal("loaded should be true after a successful set")
	}
	if total != 4 || healthy != 2 {
		t.Errorf("got %d/%d, want 2/4 - only Synced+Healthy counts", healthy, total)
	}
	if len(apps) != 4 {
		t.Errorf("tooltip list has %d apps, want all 4 including the unhealthy ones", len(apps))
	}
}

// A failed poll must leave the panel showing the last known state rather than
// blanking it, matching how the sparkline caches behave.
func TestRefreshArgoSyncPreservesCacheOnFailure(t *testing.T) {
	var cache argoSyncCache
	ok := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"name":"root","sync_status":"Synced","health_status":"Healthy"},"value":[1756000000,"1"]}]}}`
	refreshArgoSync(testClient(), prometheusStub(t, http.StatusOK, ok), &cache)

	_, total, healthy, loaded, updatedAt := cache.get()
	if total != 1 || healthy != 1 || !loaded {
		t.Fatalf("setup: got %d/%d loaded=%v, want 1/1 true", healthy, total, loaded)
	}

	refreshArgoSync(testClient(), prometheusStub(t, http.StatusServiceUnavailable, "down"), &cache)

	apps2, total2, healthy2, _, updated2 := cache.get()
	if total2 != 1 || healthy2 != 1 {
		t.Errorf("got %d/%d after a failed poll, want the cached 1/1 held", healthy2, total2)
	}
	if len(apps2) != 1 || apps2[0].Name != "root" {
		t.Errorf("the tooltip's app list was lost on a failed poll: %v", apps2)
	}
	if !updated2.Equal(updatedAt) {
		t.Error("updatedAt moved on a failed poll")
	}
}
