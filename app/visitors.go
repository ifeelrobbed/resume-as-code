package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// The visitor count lives in Azure Blob Storage rather than being derived from
// Prometheus. The Prometheus version is a rolling 15-day window that resets to
// zero whenever the TSDB is lost, which a cluster rebuild does; a blob outside
// the cluster makes it an all-time number that survives one (#75).
//
// Authentication is workload identity - a projected service account token
// exchanged for an Azure token at runtime - so no credential exists in the
// image, a Secret, or this repo.

// errConflict means the stored value changed between load and save. The caller
// re-reads and retries rather than clobbering whatever landed in between.
var errConflict = errors.New("visitor store: version conflict")

// visitorHistoryDays bounds the per-day history behind the homepage sparkline.
// 30 keeps the blob under a kilobyte and is more days than the 90px-wide
// viewBox can distinguish anyway; the running total is never trimmed, only the
// history behind the line.
const visitorHistoryDays = 30

// dayCount is one UTC day's visits. The day is a "2006-01-02" string rather
// than a time.Time so the blob stays readable with `az storage blob download`
// and carries no timezone to misinterpret - the count is a whole-day figure,
// not an instant.
type dayCount struct {
	Day   string `json:"day"`
	Count int64  `json:"count"`
}

// visitorState is what the store round-trips: the all-time total plus the
// history behind the sparkline. They travel together because they have to be
// written in the same conditional request - persisting them separately would
// let the number and the line beneath it disagree, which is the exact failure
// the sparkline is being fixed to avoid (#44).
type visitorState struct {
	Total   int64
	History []dayCount
}

// visitorStore is the seam that keeps this testable without Azure. version is
// opaque to callers - it is an ETag, but nothing outside the blob
// implementation should care.
type visitorStore interface {
	load(ctx context.Context) (state visitorState, version string, err error)
	save(ctx context.Context, state visitorState, version string) (newVersion string, err error)
}

// visitorDoc is the blob's contents. JSON rather than a bare integer so a human
// reading it with `az storage blob download` gets something self-describing,
// and so fields can be added without a migration.
//
// History is omitempty and decodes to nil against the documents written before
// #44, so an existing blob keeps its count and simply starts accumulating days
// from the next flush - no migration, no reset.
type visitorDoc struct {
	Count   int64      `json:"count"`
	Updated time.Time  `json:"updated"`
	History []dayCount `json:"history,omitempty"`
}

// visitorCounter holds the last persisted total plus whatever has arrived since,
// so the displayed number is correct between flushes rather than lagging by up
// to a flush interval.
//
// loaded stays false until the first successful read, and the page shows a dash
// rather than a zero until then - a confident 0 on a site whose whole point is
// that its numbers are real is worse than an obvious blank.
type visitorCounter struct {
	mu      sync.Mutex
	total   int64            // last value known to be persisted
	delta   map[string]int64 // increments not yet persisted, by UTC day
	history []dayCount       // last persisted per-day history, oldest first
	version string           // opaque version of the persisted value
	loaded  bool
	updated time.Time

	// now exists so tests can drive visits across a day boundary without
	// waiting for midnight. nil means time.Now - tests set it per instance
	// rather than swapping a package var, for the same reason pollVisitors
	// takes the counter as a parameter.
	now func() time.Time
}

var visitors visitorCounter

func (c *visitorCounter) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// utcDay is the bucket key: whole UTC days, so the buckets do not shift with
// the server's zone or with daylight saving.
func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// inc records one visit. Called from instrument() for the index handler only,
// so the blackbox exporter's synthetic traffic is excluded by the same check
// that already keeps it out of the Prometheus counters.
//
// The visit is bucketed by day here, at the moment it happens, rather than at
// flush time. Flushing attributes a whole batch to one day, which would put
// every visit in the 60 seconds before midnight on the wrong side of it.
func (c *visitorCounter) inc() {
	c.mu.Lock()
	if c.delta == nil {
		c.delta = make(map[string]int64)
	}
	c.delta[utcDay(c.clock())]++
	c.mu.Unlock()
}

// get returns the count to display, whether it can be trusted yet, and when it
// was last confirmed against the store.
func (c *visitorCounter) get() (count int64, loaded bool, updated time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total + sumDays(c.delta), c.loaded, c.updated
}

// dailyHistory returns visits per UTC day, oldest first, for the sparkline.
// Unpersisted visits are merged in so today's point tracks live traffic instead
// of stepping only once a minute.
//
// nil until the first successful read, matching get()'s loaded flag: the line
// and the number must not disagree about whether anything is known yet.
func (c *visitorCounter) dailyHistory() []dayCount {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		return nil
	}
	return mergeDays(c.history, c.delta)
}

// sumDays totals a pending-visit map. A nil map sums to zero.
func sumDays(days map[string]int64) int64 {
	var n int64
	for _, v := range days {
		n += v
	}
	return n
}

// mergeDays adds pending per-day counts onto a persisted history and returns a
// new slice sorted oldest-first. Neither input is modified - the caller may
// still be holding the persisted history from before a failed save.
func mergeDays(base []dayCount, extra map[string]int64) []dayCount {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	totals := make(map[string]int64, len(base)+len(extra))
	for _, d := range base {
		totals[d.Day] += d.Count
	}
	for day, n := range extra {
		totals[day] += n
	}

	merged := make([]dayCount, 0, len(totals))
	for day, n := range totals {
		merged = append(merged, dayCount{Day: day, Count: n})
	}
	// Lexicographic sort is chronological for "2006-01-02" - that format is
	// why the key is a string rather than a parsed date.
	slices.SortFunc(merged, func(a, b dayCount) int { return strings.Compare(a.Day, b.Day) })
	return merged
}

// trimDays keeps the most recent n days. Trimming the front, not the back:
// the sparkline shows a recent trend, and an unbounded history would grow the
// blob forever.
func trimDays(days []dayCount, n int) []dayCount {
	if len(days) <= n {
		return days
	}
	return days[len(days)-n:]
}

// flush persists whatever has accumulated. It is safe to call when nothing has,
// and it never loses counts: the delta is only reduced after a write succeeds,
// and by exactly the amount written, so visits arriving mid-flush survive.
func (c *visitorCounter) flush(ctx context.Context, store visitorStore) error {
	c.mu.Lock()
	// Cloned, not aliased: the map keeps being written by inc() during the
	// round trip, and the amount to subtract afterwards has to be the amount
	// actually sent.
	pending := maps.Clone(c.delta)
	loaded := c.loaded
	c.mu.Unlock()

	pendingTotal := sumDays(pending)

	// Nothing to write and the total is already known - the common case on a
	// low-traffic site, and worth skipping so most ticks cost no requests.
	if pendingTotal == 0 && loaded {
		return nil
	}

	// One retry: a conflict means something else wrote between our load and
	// save, so re-reading picks up their value and adds ours on top. More than
	// one retry would be theatre at a single replica.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		stored, version, err := store.load(ctx)
		if err != nil {
			return fmt.Errorf("load: %w", err)
		}

		// Merged against what was just read, not against the local copy, for
		// the same reason the total is: a conflicting writer's days have to
		// survive too, not be overwritten by ours.
		next := visitorState{
			Total:   stored.Total + pendingTotal,
			History: trimDays(mergeDays(stored.History, pending), visitorHistoryDays),
		}

		newVersion, err := store.save(ctx, next, version)
		if err == nil {
			c.mu.Lock()
			// Subtract rather than zero, per day: visits may have arrived
			// during the round trip - possibly on a later day than the ones
			// written - and they are still unpersisted.
			for day, n := range pending {
				c.delta[day] -= n
				if c.delta[day] <= 0 {
					delete(c.delta, day)
				}
			}
			c.total = next.Total
			c.history = next.History
			c.version = newVersion
			c.loaded = true
			c.updated = c.clock()
			c.mu.Unlock()
			return nil
		}
		if !errors.Is(err, errConflict) {
			return fmt.Errorf("save: %w", err)
		}
		lastErr = err
	}
	return fmt.Errorf("save: %w", lastErr)
}

// pollVisitors flushes on an interval and once more on the way out. The final
// flush is why graceful shutdown had to land first: without it, everything
// since the last tick is lost on every deploy.
// Takes the counter rather than reaching for the package var, matching
// pollSparkline's shape and letting tests drive their own instance instead of
// swapping a global that holds a mutex.
func pollVisitors(ctx context.Context, c *visitorCounter, store visitorStore) {
	for {
		select {
		case <-ctx.Done():
			// Deliberately not ctx - it is already cancelled. A fresh, bounded
			// context gives the last write a chance to complete inside the
			// drain window.
			flushCtx, cancel := context.WithTimeout(context.Background(), visitorFlushTimeout)
			defer cancel()
			if err := c.flush(flushCtx, store); err != nil {
				log.Printf("visitor flush on shutdown: %v", err)
			}
			return
		case <-time.After(visitorFlushInterval):
			if err := c.flush(ctx, store); err != nil {
				log.Printf("visitor flush: %v", err)
			}
		}
	}
}

const (
	// Long enough that a quiet minute costs nothing, short enough that an
	// ungraceful kill loses little. The graceful path loses nothing.
	visitorFlushInterval = 60 * time.Second
	visitorFlushTimeout  = 10 * time.Second
)

// blobVisitorStore persists the count as a single small blob.
type blobVisitorStore struct {
	client    *azblob.Client
	container string
	name      string
}

// newBlobVisitorStore wires up workload identity explicitly rather than through
// DefaultAzureCredential. The default walks a chain of credential sources and
// would silently fall back to something else if the projected token were
// missing - here that should be a hard failure, not a quiet change of identity.
func newBlobVisitorStore(account, container, name string) (*blobVisitorStore, error) {
	cred, err := azidentity.NewWorkloadIdentityCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("workload identity credential: %w", err)
	}
	client, err := azblob.NewClient(fmt.Sprintf("https://%s.blob.core.windows.net/", account), cred, nil)
	if err != nil {
		return nil, fmt.Errorf("blob client: %w", err)
	}
	return &blobVisitorStore{client: client, container: container, name: name}, nil
}

// load reads the current value. A missing blob is the first-run case, not an
// error: count 0 with an empty version, which save turns into a create-only
// write.
func (s *blobVisitorStore) load(ctx context.Context) (visitorState, string, error) {
	resp, err := s.client.DownloadStream(ctx, s.container, s.name, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return visitorState{}, "", nil
		}
		return visitorState{}, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return visitorState{}, "", err
	}
	var doc visitorDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return visitorState{}, "", fmt.Errorf("decoding %s: %w", s.name, err)
	}
	var version string
	if resp.ETag != nil {
		version = string(*resp.ETag)
	}
	return visitorState{Total: doc.Count, History: doc.History}, version, nil
}

// save writes count back, but only if the stored value is still the one load
// returned. An empty version means "the blob did not exist", so the write is
// conditional on it still not existing - two replicas starting together cannot
// both create it and lose one of the counts.
func (s *blobVisitorStore) save(ctx context.Context, state visitorState, version string) (string, error) {
	body, err := json.Marshal(visitorDoc{
		Count:   state.Total,
		Updated: time.Now().UTC(),
		History: state.History,
	})
	if err != nil {
		return "", err
	}

	conds := &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{}}
	if version == "" {
		conds.ModifiedAccessConditions.IfNoneMatch = to(azcore.ETagAny)
	} else {
		conds.ModifiedAccessConditions.IfMatch = to(azcore.ETag(version))
	}

	resp, err := s.client.UploadBuffer(ctx, s.container, s.name, body, &azblob.UploadBufferOptions{
		AccessConditions: conds,
	})
	if err != nil {
		// Both codes mean the same thing to us: someone else got there first.
		if bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists) {
			return "", errConflict
		}
		return "", err
	}
	if resp.ETag == nil {
		// UploadBuffer does not always surface an ETag; an empty version makes
		// the next save re-read rather than write blind against a stale one.
		return "", nil
	}
	return string(*resp.ETag), nil
}

// to takes the address of a value, which the SDK's option structs want for
// every optional field.
func to[T any](v T) *T { return &v }

// visitorStoreFromEnv builds the store from the Deployment's environment.
// Returns nil when unconfigured, which is the local `go run` case - the app
// runs fine without a counter rather than failing to start.
func visitorStoreFromEnv() visitorStore {
	account := envOrDefault("VISITOR_STORAGE_ACCOUNT", "")
	container := envOrDefault("VISITOR_STORAGE_CONTAINER", "")
	name := envOrDefault("VISITOR_BLOB_NAME", "")
	if account == "" || container == "" || name == "" {
		log.Print("visitor store not configured; the durable count is disabled")
		return nil
	}
	store, err := newBlobVisitorStore(account, container, name)
	if err != nil {
		log.Printf("visitor store unavailable: %v", err)
		return nil
	}
	return store
}
