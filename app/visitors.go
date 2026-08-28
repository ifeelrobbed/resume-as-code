package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

// visitorStore is the seam that keeps this testable without Azure. version is
// opaque to callers - it is an ETag, but nothing outside the blob
// implementation should care.
type visitorStore interface {
	load(ctx context.Context) (count int64, version string, err error)
	save(ctx context.Context, count int64, version string) (newVersion string, err error)
}

// visitorDoc is the blob's contents. JSON rather than a bare integer so a human
// reading it with `az storage blob download` gets something self-describing,
// and so fields can be added without a migration.
type visitorDoc struct {
	Count   int64     `json:"count"`
	Updated time.Time `json:"updated"`
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
	total   int64  // last value known to be persisted
	delta   int64  // increments not yet persisted
	version string // opaque version of the persisted value
	loaded  bool
	updated time.Time
}

var visitors visitorCounter

// inc records one visit. Called from instrument() for the index handler only,
// so the blackbox exporter's synthetic traffic is excluded by the same check
// that already keeps it out of the Prometheus counters.
func (c *visitorCounter) inc() {
	c.mu.Lock()
	c.delta++
	c.mu.Unlock()
}

// get returns the count to display, whether it can be trusted yet, and when it
// was last confirmed against the store.
func (c *visitorCounter) get() (count int64, loaded bool, updated time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total + c.delta, c.loaded, c.updated
}

// flush persists whatever has accumulated. It is safe to call when nothing has,
// and it never loses counts: the delta is only reduced after a write succeeds,
// and by exactly the amount written, so visits arriving mid-flush survive.
func (c *visitorCounter) flush(ctx context.Context, store visitorStore) error {
	c.mu.Lock()
	pending := c.delta
	loaded := c.loaded
	c.mu.Unlock()

	// Nothing to write and the total is already known - the common case on a
	// low-traffic site, and worth skipping so most ticks cost no requests.
	if pending == 0 && loaded {
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

		newVersion, err := store.save(ctx, stored+pending, version)
		if err == nil {
			c.mu.Lock()
			// Subtract rather than zero: visits may have arrived during the
			// round trip, and they are still unpersisted.
			c.delta -= pending
			c.total = stored + pending
			c.version = newVersion
			c.loaded = true
			c.updated = time.Now()
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
func (s *blobVisitorStore) load(ctx context.Context) (int64, string, error) {
	resp, err := s.client.DownloadStream(ctx, s.container, s.name, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return 0, "", nil
		}
		return 0, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	var doc visitorDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, "", fmt.Errorf("decoding %s: %w", s.name, err)
	}
	var version string
	if resp.ETag != nil {
		version = string(*resp.ETag)
	}
	return doc.Count, version, nil
}

// save writes count back, but only if the stored value is still the one load
// returned. An empty version means "the blob did not exist", so the write is
// conditional on it still not existing - two replicas starting together cannot
// both create it and lose one of the counts.
func (s *blobVisitorStore) save(ctx context.Context, count int64, version string) (string, error) {
	body, err := json.Marshal(visitorDoc{Count: count, Updated: time.Now().UTC()})
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
