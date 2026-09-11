package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeStore stands in for Azure. Versions are a simple counter rather than
// ETags - the counter only ever compares them for equality.
type fakeStore struct {
	mu sync.Mutex

	count   int64
	history []dayCount
	version int
	exists  bool

	loadErr  error
	saveErr  error
	loads    int
	saves    int
	conflict int // return errConflict from save this many times, then succeed
}

func (f *fakeStore) load(ctx context.Context) (visitorState, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.loadErr != nil {
		return visitorState{}, "", f.loadErr
	}
	if !f.exists {
		return visitorState{}, "", nil
	}
	return visitorState{Total: f.count, History: f.history}, versionString(f.version), nil
}

func (f *fakeStore) save(ctx context.Context, state visitorState, version string) (string, error) {
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
	f.count = state.Total
	f.history = state.History
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

// An idle tick must not write, because this pod has nothing of its own to add
// and a write would only be a chance to lose a race for no gain.
//
// It must still read. Before #142 this returned without touching the store at
// all, which was right while one replica was the only writer of a value it had
// itself cached. With a second replica a pod serving no traffic would sit on a
// total the other pod had long since moved past - not stale by a flush
// interval, but frozen, since nothing would ever make it look again.
func TestIdleFlushReadsButDoesNotWrite(t *testing.T) {
	store := &fakeStore{count: 10, version: 1, exists: true}
	c := newCounter()

	// First flush loads and establishes the total.
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	loadsAfterFirst, savesAfterFirst := store.loads, store.saves

	// Stand in for the other replica having taken some visits and persisted
	// them. Nothing about this pod changed.
	store.count = 17
	store.version++

	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("idle flush: %v", err)
	}

	if store.saves != savesAfterFirst {
		t.Errorf("idle flush wrote: saves %d->%d, want no write with nothing pending",
			savesAfterFirst, store.saves)
	}
	if store.loads != loadsAfterFirst+1 {
		t.Errorf("idle flush made %d loads, want exactly one - without it this pod never sees the other replica's writes",
			store.loads-loadsAfterFirst)
	}

	got, loaded, _ := c.get()
	if !loaded || got != 17 {
		t.Errorf("got %d (loaded=%v) after the other replica wrote 17, want 17 - the displayed count froze", got, loaded)
	}
}

// The read on an idle tick is the only thing standing between a quiet pod and
// a frozen number, so a failed read must not be mistaken for success - but it
// must also not discard what is already known.
func TestIdleFlushKeepsLastKnownTotalWhenReadFails(t *testing.T) {
	store := &fakeStore{count: 10, version: 1, exists: true}
	c := newCounter()
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("first flush: %v", err)
	}

	store.loadErr = errors.New("storage unreachable")
	if err := c.flush(context.Background(), store); err == nil {
		t.Error("a failed refresh reported success, so the poller would log nothing")
	}

	got, loaded, _ := c.get()
	if !loaded || got != 10 {
		t.Errorf("got %d (loaded=%v), want the last known 10 held rather than blanked", got, loaded)
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

// Pins the attempt budget rather than leaving it implied. Two conflicts is
// what two other writers can inflict in the worst case, and the old budget of
// two attempts survived exactly one - enough for the single replica it was
// written for, and quietly not enough once #142 added a second.
//
// TestFlushGivesUpAfterRepeatedConflicts below would pass at either budget, so
// without this nothing would notice the constant being lowered again.
func TestFlushSurvivesTwoConflicts(t *testing.T) {
	store := &fakeStore{count: 50, version: 1, exists: true, conflict: 2}
	c := newCounter()
	c.inc()

	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("flush should survive two conflicts with %d attempts: %v", visitorFlushAttempts, err)
	}
	// The fake adds 100 to the stored value on each conflict, so the visit must
	// land on 250 rather than on either value that lost.
	if store.count != 251 {
		t.Errorf("stored %d, want 251 - the last attempt must build on the value that won", store.count)
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

func (b *blockingStore) load(context.Context) (visitorState, string, error) {
	return visitorState{Total: b.count}, "", nil
}

func (b *blockingStore) save(_ context.Context, state visitorState, _ string) (string, error) {
	close(b.entered)
	<-b.released
	b.count = state.Total
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

// --- per-day history (#44) -------------------------------------------------

// atClock drives the counter's notion of "now" so day-boundary behaviour is
// testable without waiting for midnight.
func atClock(c *visitorCounter, t *time.Time) { c.now = func() time.Time { return *t } }

func TestFlushRecordsVisitsUnderTheirDay(t *testing.T) {
	store := &fakeStore{}
	c := newCounter()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	c.inc()
	c.inc()
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("flush: %v", err)
	}

	got := c.dailyHistory()
	want := []dayCount{{Day: "2026-08-30", Count: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	if store.count != 2 {
		t.Errorf("stored total = %d, want 2", store.count)
	}
}

// The reason inc() buckets by day instead of flush() doing it: with a 60s
// flush interval, attributing a whole batch to one day would misfile every
// visit in the last minute before midnight.
func TestVisitsStraddlingMidnightLandOnSeparateDays(t *testing.T) {
	store := &fakeStore{}
	c := newCounter()
	now := time.Date(2026, 8, 30, 23, 59, 30, 0, time.UTC)
	atClock(c, &now)

	c.inc() // 08-30
	now = now.Add(45 * time.Second)
	c.inc() // 08-31
	c.inc()

	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("flush: %v", err)
	}

	want := []dayCount{{Day: "2026-08-30", Count: 1}, {Day: "2026-08-31", Count: 2}}
	if got := c.dailyHistory(); !reflect.DeepEqual(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	// The total must be unaffected by how the days were split.
	if count, _, _ := c.get(); count != 3 {
		t.Errorf("total = %d, want 3", count)
	}
}

func TestHistoryAccumulatesAcrossFlushes(t *testing.T) {
	store := &fakeStore{}
	c := newCounter()
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	atClock(c, &now)

	c.inc()
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	now = now.Add(24 * time.Hour)
	c.inc()
	c.inc()
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	want := []dayCount{{Day: "2026-08-30", Count: 1}, {Day: "2026-08-31", Count: 2}}
	if got := c.dailyHistory(); !reflect.DeepEqual(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
}

// The sparkline should track live traffic rather than stepping once a minute,
// so unpersisted visits are merged into what dailyHistory reports.
func TestDailyHistoryIncludesUnflushedVisits(t *testing.T) {
	store := &fakeStore{}
	c := newCounter()
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	atClock(c, &now)

	c.inc()
	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("flush: %v", err)
	}
	c.inc() // not yet persisted

	want := []dayCount{{Day: "2026-08-30", Count: 2}}
	if got := c.dailyHistory(); !reflect.DeepEqual(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
	if store.count != 1 {
		t.Errorf("stored total = %d - the unflushed visit should not be persisted yet", store.count)
	}
}

// Same reasoning as the count's dash: unknown must not look like empty.
func TestDailyHistoryIsNilUntilLoaded(t *testing.T) {
	c := newCounter()
	c.inc()
	if got := c.dailyHistory(); got != nil {
		t.Errorf("history = %v, want nil before the first successful read", got)
	}
}

// An unbounded history would grow the blob forever. The trim keeps the recent
// end, which is what the sparkline shows.
func TestHistoryIsTrimmedToTheMostRecentDays(t *testing.T) {
	store := &fakeStore{}
	c := newCounter()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)

	for i := 0; i < visitorHistoryDays+10; i++ {
		c.inc()
		if err := c.flush(context.Background(), store); err != nil {
			t.Fatalf("flush on day %d: %v", i, err)
		}
		now = now.Add(24 * time.Hour)
	}

	got := c.dailyHistory()
	if len(got) != visitorHistoryDays {
		t.Fatalf("history length = %d, want %d", len(got), visitorHistoryDays)
	}
	// Oldest kept day is day 10 (0-indexed), i.e. 2026-01-11.
	if got[0].Day != "2026-01-11" {
		t.Errorf("oldest kept day = %s, want 2026-01-11 - the trim dropped the wrong end", got[0].Day)
	}
	if got[len(got)-1].Day != "2026-02-09" {
		t.Errorf("newest day = %s, want 2026-02-09", got[len(got)-1].Day)
	}
	// Trimming the line must never touch the total.
	if count, _, _ := c.get(); count != int64(visitorHistoryDays+10) {
		t.Errorf("total = %d, want %d - trimming history changed the count", count, visitorHistoryDays+10)
	}
}

// A conflicting writer's days have to survive the retry, exactly like their
// count does - merging against what was just re-read, not against the local
// copy from before the conflict.
func TestFlushMergesHistoryFromAConflictingWriter(t *testing.T) {
	store := &fakeStore{
		exists:   true,
		count:    5,
		history:  []dayCount{{Day: "2026-08-29", Count: 5}},
		conflict: 1,
	}
	c := newCounter()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	atClock(c, &now)
	c.inc()

	if err := c.flush(context.Background(), store); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// The other writer's day must still be there alongside ours.
	want := []dayCount{{Day: "2026-08-29", Count: 5}, {Day: "2026-08-30", Count: 1}}
	if got := c.dailyHistory(); !reflect.DeepEqual(got, want) {
		t.Errorf("history = %v, want %v - a conflicting writer's days were clobbered", got, want)
	}
}

// The delta is reduced by what was written, per day, so a visit arriving on a
// new day mid-flush is not silently dropped along with the ones that were.
func TestVisitsArrivingDuringFlushSurviveOnTheirOwnDay(t *testing.T) {
	released := make(chan struct{})
	store := &blockingStore{entered: make(chan struct{}), released: released}
	c := newCounter()
	now := time.Date(2026, 8, 30, 23, 59, 50, 0, time.UTC)
	atClock(c, &now)
	c.inc()

	done := make(chan error, 1)
	go func() { done <- c.flush(context.Background(), store) }()

	<-store.entered
	now = now.Add(20 * time.Second) // rolls over to 08-31
	c.inc()
	close(released)

	if err := <-done; err != nil {
		t.Fatalf("flush: %v", err)
	}
	if count, _, _ := c.get(); count != 2 {
		t.Errorf("total = %d, want 2 - the mid-flush visit was lost", count)
	}
	c.mu.Lock()
	remaining := c.delta["2026-08-31"]
	c.mu.Unlock()
	if remaining != 1 {
		t.Errorf("unpersisted 08-31 delta = %d, want 1", remaining)
	}
}

// An existing blob predates the history field entirely. It must keep its count
// and start accumulating days, not reset.
func TestDocWithoutHistoryDecodesAndKeepsItsCount(t *testing.T) {
	var doc visitorDoc
	if err := json.Unmarshal([]byte(`{"count":90,"updated":"2026-08-30T19:00:00Z"}`), &doc); err != nil {
		t.Fatalf("decoding a pre-#44 document: %v", err)
	}
	if doc.Count != 90 {
		t.Errorf("count = %d, want 90", doc.Count)
	}
	if doc.History != nil {
		t.Errorf("history = %v, want nil", doc.History)
	}
}

func TestDocRoundTripsHistory(t *testing.T) {
	in := visitorDoc{Count: 7, History: []dayCount{{Day: "2026-08-30", Count: 7}}}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out visitorDoc
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in.History, out.History) {
		t.Errorf("history round trip: got %v, want %v", out.History, in.History)
	}
}
