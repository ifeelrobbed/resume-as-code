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

func TestQueryVisitorCount(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		want        int64
		wantNoData  bool
		wantSomeErr bool
	}{
		{
			name:   "valid result",
			status: http.StatusOK,
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1756000000,"1234"]}]}}`,
			want:   1234,
		},
		{
			name:   "float result truncates toward zero",
			status: http.StatusOK,
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1756000000,"1234.987"]}]}}`,
			want:   1234,
		},
		{
			// The regression this whole change exists for: an empty result must
			// not read as a successful zero.
			name:       "empty result set",
			status:     http.StatusOK,
			body:       `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			wantNoData: true,
		},
		{
			// Prometheus answers a bad query with 422 and a filled-in envelope.
			name:        "query error with envelope",
			status:      http.StatusUnprocessableEntity,
			body:        `{"status":"error","errorType":"bad_data","error":"invalid parameter \"query\""}`,
			wantSomeErr: true,
		},
		{
			// A restarting pod or an upstream proxy: non-200, no envelope. This
			// is the case that used to decode cleanly into a zero.
			name:        "service unavailable, non-JSON body",
			status:      http.StatusServiceUnavailable,
			body:        `503 Service Temporarily Unavailable`,
			wantSomeErr: true,
		},
		{
			// A 200 carrying an error envelope - rarer, but the envelope check
			// is what catches it, not the status code check.
			name:        "error envelope under a 200",
			status:      http.StatusOK,
			body:        `{"status":"error","errorType":"execution","error":"query timed out"}`,
			wantSomeErr: true,
		},
		{
			name:        "sample value is a number, not a string",
			status:      http.StatusOK,
			body:        `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1756000000,1234]}]}}`,
			wantSomeErr: true,
		},
		{
			name:        "malformed body",
			status:      http.StatusOK,
			body:        `{"status":"success","data":{`,
			wantSomeErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := queryVisitorCount(testClient(), prometheusStub(t, tt.status, tt.body))

			switch {
			case tt.wantNoData:
				if !errors.Is(err, errNoData) {
					t.Fatalf("got err %v, want errNoData", err)
				}
			case tt.wantSomeErr:
				if err == nil {
					t.Fatalf("got value %d and nil error, want an error", got)
				}
				if errors.Is(err, errNoData) {
					t.Fatalf("got errNoData, want a real failure - a broken query must not read as an empty one")
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tt.want {
					t.Fatalf("got %d, want %d", got, tt.want)
				}
			}
		})
	}
}

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

// The behavior issue #57 is actually about: whatever Prometheus does, a good
// cached value must survive it. Without this, a single 503 replaced a real
// visitor count with a confident 0 on the homepage.
func TestRefreshVisitorCountPreservesCacheOnFailure(t *testing.T) {
	failures := []struct {
		name   string
		status int
		body   string
	}{
		{"service unavailable", http.StatusServiceUnavailable, `503 Service Temporarily Unavailable`},
		{"query error", http.StatusUnprocessableEntity, `{"status":"error","errorType":"bad_data","error":"boom"}`},
		{"empty result set", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`},
		{"malformed body", http.StatusOK, `not json at all`},
	}

	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			var cache visitorCountCache

			healthy := `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1756000000,"4096"]}]}}`
			refreshVisitorCount(testClient(), prometheusStub(t, http.StatusOK, healthy), &cache)

			got, updatedAt := cache.get()
			if got != 4096 {
				t.Fatalf("setup: got %d, want 4096", got)
			}
			if updatedAt.IsZero() {
				t.Fatal("setup: updatedAt should be set after a successful poll")
			}

			refreshVisitorCount(testClient(), prometheusStub(t, f.status, f.body), &cache)

			gotAfter, updatedAfter := cache.get()
			if gotAfter != 4096 {
				t.Errorf("got %d after a failed poll, want the cached 4096 held", gotAfter)
			}
			if !updatedAfter.Equal(updatedAt) {
				t.Errorf("updatedAt moved on a failed poll (%v -> %v); the page would show a fresh timestamp on stale data",
					updatedAt, updatedAfter)
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

// A connection refused / DNS failure never reaches the decoder, so it takes a
// different path through getPrometheusJSON than any HTTP status does.
func TestQueryVisitorCountUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	if _, err := queryVisitorCount(testClient(), url); err == nil {
		t.Fatal("want an error when Prometheus is unreachable")
	} else if errors.Is(err, errNoData) {
		t.Fatalf("got errNoData for an unreachable server, want a real failure")
	}
}
