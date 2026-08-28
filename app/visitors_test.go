package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeStore stands in for Azure. Versions are a simple counter rather than
// ETags - the counter only ever compares them for equality.
type fakeStore struct {
	mu sync.Mutex

	count   int64
	version int
	exists  bool

	loadErr  error
	saveErr  error
	loads    int
	saves    int
	conflict int // return errConflict from save this many times, then succeed
}

func (f *fakeStore) load(ctx context.Context) (int64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.loadErr != nil {
		return 0, "", f.loadErr
	}
	if !f.exists {
		return 0, "", nil
	}
	return f.count, versionString(f.version), nil
}

func (f *fakeStore) save(ctx context.Context, count int64, version string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	if f.saveErr != nil {
		return "", f.saveErr
	}
	if f.conflict > 0 {
		f.conflict--
		// Simulate someone else winning the race: the stored value moves on.
		f.count += 100
		f.version++
		f.exists = true
		return "", errConflict
	}
	expected := ""
	if f.exists {
		expected = versionString(f.version)
	}
	if version != expected {
		return "", errConflict
	}
	f.count = count
	f.version++
	f.exists = true
	return versionString(f.version), nil
}

func versionString(v int) string { return "v" + string(rune('0'+v)) }

func newCounter() *visitorCounter { return &visitorCounter{} }

func TestFlushCreatesBlobOnFirstRun(t *testing.T) {
	store := &fakeStore{}
	c := newCounter()
	c.inc()
	c.inc()

	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if store.count != 2 {
		t.Errorf("stored %d, want 2", store.count)
	}
	got, loaded, _ := c.get()
	if got != 2 || !loaded {
		t.Errorf("got count=%d loaded=%v, want 2/true", got, loaded)
	}
}

func TestFlushAddsToExistingValue(t *testing.T) {
	store := &fakeStore{count: 4000, version: 1, exists: true}
	c := newCounter()
	c.inc()

	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if store.count != 4001 {
		t.Errorf("stored %d, want 4001 - the existing value must be added to, not replaced", store.count)
	}
}

// The common case on a low-traffic site: most ticks have nothing to write and
// should cost no requests at all.
func TestFlushSkipsWhenNothingPendingAndAlreadyLoaded(t *testing.T) {
	store := &fakeStore{count: 10, version: 1, exists: true}
	c := newCounter()

	// First flush loads and establishes the total.
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	loadsAfterFirst, savesAfterFirst := store.loads, store.saves

	// Second flush with no new visits should do nothing.
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if store.loads != loadsAfterFirst || store.saves != savesAfterFirst {
		t.Errorf("idle flush made requests: loads %d->%d, saves %d->%d",
			loadsAfterFirst, store.loads, savesAfterFirst, store.saves)
	}
}

// The behaviour that makes the counter safe to run: a failed write must not
// discard the visits it was trying to persist.
func TestFlushKeepsDeltaWhenSaveFails(t *testing.T) {
	store := &fakeStore{saveErr: errors.New("storage unreachable")}
	c := newCounter()
	c.inc()
	c.inc()
	c.inc()

	if err := c.flush(context.Background(), store); err == nil {
		t.Fatal("want an error when the store is unreachable")
	}

	got, loaded, _ := c.get()
	if got != 3 {
		t.Errorf("got %d after a failed flush, want the 3 pending visits retained", got)
	}
	if loaded {
		t.Error("loaded should stay false - nothing was ever read successfully")
	}

	// Recovery: once the store works, the retained visits are written.
	store.saveErr = nil
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("recovery flush: %v", err)
	}
	if store.count != 3 {
		t.Errorf("stored %d after recovery, want 3", store.count)
	}
}

func TestFlushKeepsDeltaWhenLoadFails(t *testing.T) {
	store := &fakeStore{loadErr: errors.New("storage unreachable")}
	c := newCounter()
	c.inc()

	if err := c.flush(context.Background(), store); err == nil {
		t.Fatal("want an error when load fails")
	}
	if got, _, _ := c.get(); got != 1 {
		t.Errorf("got %d, want the pending visit retained", got)
	}
}

// A conflict means another writer got there first. Re-reading and adding on top
// is what stops the two updates clobbering each other.
func TestFlushRetriesOnConflict(t *testing.T) {
	store := &fakeStore{count: 50, version: 1, exists: true, conflict: 1}
	c := newCounter()
	c.inc()

	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("flush should recover from one conflict: %v", err)
	}
	// The fake bumps the stored value by 100 on conflict, so the retry must add
	// our 1 to 150 rather than to the stale 50.
	if store.count != 151 {
		t.Errorf("stored %d, want 151 - the retry must build on the value that won the race", store.count)
	}
}

func TestFlushGivesUpAfterRepeatedConflicts(t *testing.T) {
	store := &fakeStore{count: 1, version: 1, exists: true, conflict: 5}
	c := newCounter()
	c.inc()

	err := c.flush(context.Background(), store)
	if err == nil {
		t.Fatal("want an error when conflicts persist")
	}
	if !errors.Is(err, errConflict) {
		t.Errorf("got %v, want it to wrap errConflict", err)
	}
	if got, _, _ := c.get(); got != 1 {
		t.Errorf("got %d, want the pending visit retained after giving up", got)
	}
}

// Visits arriving during a flush belong to the next one, not to the void.
func TestFlushDoesNotDropVisitsArrivingMidFlight(t *testing.T) {
	c := newCounter()
	c.inc() // will be flushed

	// Both channels created up front: the test receives on entered before the
	// goroutine has necessarily reached save, so lazily creating it would race.
	store := &blockingStore{entered: make(chan struct{}), released: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- c.flush(context.Background(), store) }()

	<-store.entered
	c.inc() // arrives while the save is in flight
	close(store.released)

	if err := <-done; err != nil {
		t.Fatalf("flush: %v", err)
	}
	got, _, _ := c.get()
	if got != 2 {
		t.Errorf("got %d, want 2 - one persisted plus one that arrived mid-flush", got)
	}
}

type blockingStore struct {
	entered  chan struct{}
	released chan struct{}
	count    int64
}

func (b *blockingStore) load(context.Context) (int64, string, error) {
	return b.count, "", nil
}

func (b *blockingStore) save(_ context.Context, count int64, _ string) (string, error) {
	close(b.entered)
	<-b.released
	b.count = count
	return "v1", nil
}

func TestCounterIsConcurrencySafe(t *testing.T) {
	c := newCounter()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.inc()
			c.get()
		}()
	}
	wg.Wait()
	if got, _, _ := c.get(); got != 50 {
		t.Errorf("got %d, want 50", got)
	}
}

// pollVisitors must flush on the way out, or every deploy loses whatever
// arrived since the last tick. This is what graceful shutdown was landed for.
func TestPollVisitorsFlushesOnShutdown(t *testing.T) {
	store := &fakeStore{}
	c := newCounter()
	c.inc()
	c.inc()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pollVisitors(ctx, c, store); close(done) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pollVisitors did not return after cancellation")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.count != 2 {
		t.Errorf("stored %d on shutdown, want 2 - the final flush did not happen", store.count)
	}
}
