package main

import (
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
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

func TestQueryScalar(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		want        float64
		wantNoData  bool
		wantSomeErr bool
	}{
		{
			name:   "single value",
			status: http.StatusOK,
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1756000000,"7"]}]}}`,
			want:   7,
		},
		{
			// What `or vector(0)` produces when the filtered count matches
			// nothing: a real zero rather than an empty result.
			name:   "explicit zero from or vector(0)",
			status: http.StatusOK,
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1756000000,"0"]}]}}`,
			want:   0,
		},
		{
			// Without `or vector(0)` this is what an all-unhealthy cluster
			// would return - indistinguishable from Prometheus being down,
			// which is why the healthy query carries the fallback.
			name:       "empty result",
			status:     http.StatusOK,
			body:       `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			wantNoData: true,
		},
		{
			// An unaggregated query would return one series per Application.
			// Failing is better than silently reporting the first one.
			name:        "multiple series means the query is wrong",
			status:      http.StatusOK,
			body:        `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"name":"a"},"value":[1,"1"]},{"metric":{"name":"b"},"value":[1,"1"]}]}}`,
			wantSomeErr: true,
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
			got, err := queryScalar(testClient(), prometheusStub(t, tt.status, tt.body), argoAppsTotalQuery)
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
				if got != tt.want {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// A failed poll must leave the panel showing the last known state rather than
// blanking it, matching how the sparkline caches behave.
func TestRefreshArgoSyncPreservesCacheOnFailure(t *testing.T) {
	var cache argoSyncCache
	ok := `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1756000000,"7"]}]}}`
	refreshArgoSync(testClient(), prometheusStub(t, http.StatusOK, ok), &cache)

	total, healthy, loaded, updatedAt := cache.get()
	if total != 7 || healthy != 7 || !loaded {
		t.Fatalf("setup: got %d/%d loaded=%v, want 7/7 true", healthy, total, loaded)
	}

	refreshArgoSync(testClient(), prometheusStub(t, http.StatusServiceUnavailable, "down"), &cache)

	total2, healthy2, _, updated2 := cache.get()
	if total2 != 7 || healthy2 != 7 {
		t.Errorf("got %d/%d after a failed poll, want the cached 7/7 held", healthy2, total2)
	}
	if !updated2.Equal(updatedAt) {
		t.Error("updatedAt moved on a failed poll")
	}
}
