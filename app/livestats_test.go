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
