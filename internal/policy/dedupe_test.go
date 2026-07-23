package policy

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestNewDeduperRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	clock := func() time.Time { return time.Time{} }
	for _, test := range []struct {
		ttl      time.Duration
		capacity int
		clock    Clock
	}{
		{ttl: 0, capacity: 1, clock: clock},
		{ttl: time.Second, capacity: 0, clock: clock},
		{ttl: time.Second, capacity: 1, clock: nil},
	} {
		deduper, err := NewDeduper(test.ttl, test.capacity, test.clock)
		if deduper != nil || !errors.Is(err, ErrInvalidDeduperConfig) {
			t.Fatalf("NewDeduper(%v, %d, clock) = %v, %v, want ErrInvalidDeduperConfig", test.ttl, test.capacity, deduper, err)
		}
	}
}

func TestDeduperRecognizesNewDuplicateAndExpiredKeys(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC))
	deduper := mustDeduper(t, time.Minute, 2, clock.Now)
	if !deduper.AddIfNew("message-1") {
		t.Fatal("first key should be new")
	}
	if deduper.AddIfNew("message-1") {
		t.Fatal("duplicate key should not be new")
	}
	clock.Advance(time.Minute)
	if !deduper.AddIfNew("message-1") {
		t.Fatal("key at exact expiry boundary should be new")
	}
	if deduper.AddIfNew("") || deduper.AddIfNew(" \t\n") {
		t.Fatal("empty message ids must not be accepted")
	}
	if !deduper.AddIfNew(" key ") || !deduper.AddIfNew("key") {
		t.Fatal("non-empty message ids must retain their exact protocol identity")
	}
}

func TestDeduperEvictsOldestLiveKeyAndDoesNotRefreshDuplicates(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC))
	deduper := mustDeduper(t, time.Minute, 2, clock.Now)
	if !deduper.AddIfNew("first") || !deduper.AddIfNew("second") {
		t.Fatal("initial keys should be new")
	}
	clock.Advance(30 * time.Second)
	if deduper.AddIfNew("first") {
		t.Fatal("duplicate must remain duplicate and must not refresh ttl")
	}
	if !deduper.AddIfNew("third") {
		t.Fatal("third key should be new")
	}
	if !deduper.AddIfNew("first") {
		t.Fatal("oldest live key should be evicted when capacity is full")
	}
	clock.Advance(31 * time.Second)
	if !deduper.AddIfNew("second") {
		t.Fatal("duplicate must not refresh ttl")
	}
}

func TestDeduperDropsExpiredEntriesInBulkAndRemainsBoundedAfterReinsert(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC))
	deduper := mustDeduper(t, time.Second, 2, clock.Now)
	if !deduper.AddIfNew("a") || !deduper.AddIfNew("b") {
		t.Fatal("initial keys should be new")
	}
	clock.Advance(time.Second)
	if !deduper.AddIfNew("a") || !deduper.AddIfNew("c") {
		t.Fatal("expired keys should be removed before reinsertion")
	}
	if deduper.entryCount() > 2 || deduper.orderCount() > 2 {
		t.Fatalf("deduper grew beyond capacity: entries=%d order=%d", deduper.entryCount(), deduper.orderCount())
	}
}

func TestDeduperHandlesClockRollbackConservatively(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC))
	deduper := mustDeduper(t, time.Minute, 1, clock.Now)
	if !deduper.AddIfNew("message") {
		t.Fatal("first key should be new")
	}
	clock.Advance(30 * time.Second)
	clock.Set(clock.Now().Add(-time.Hour))
	if deduper.AddIfNew("message") {
		t.Fatal("clock rollback must conservatively preserve an existing key")
	}
	if !deduper.AddIfNew("other") {
		t.Fatal("different key should be admitted by evicting the oldest live key")
	}
}

func TestDeduperUsesMonotonicLogicalTimeDuringClockRollback(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	clock := newTestClock(start)
	deduper := mustDeduper(t, time.Minute, 4, clock.Now)
	clock.Advance(30 * time.Second)
	if !deduper.AddIfNew("before-rollback") {
		t.Fatal("seed key should be new")
	}
	clock.Set(start.Add(-time.Hour))
	if !deduper.AddIfNew("during-rollback") {
		t.Fatal("new key during rollback should use the logical clock")
	}
	if deduper.AddIfNew("during-rollback") {
		t.Fatal("rollback should retain key")
	}
	clock.Set(start.Add(31 * time.Second))
	if deduper.AddIfNew("during-rollback") {
		t.Fatal("wall clock recovery before logical ttl must retain key")
	}
	clock.Set(start.Add(90 * time.Second))
	if !deduper.AddIfNew("during-rollback") {
		t.Fatal("key should be accepted at logical ttl boundary")
	}
}

func TestDeduperMetadataStaysBoundedUnderLongCapacityAndDuplicateLoads(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC))
	const capacity = 7
	deduper := mustDeduper(t, time.Hour, capacity, clock.Now)
	for index := range 20000 {
		key := string(rune('a'+index%26)) + "-" + string(rune(index%10+'0'))
		deduper.AddIfNew(key)
		deduper.AddIfNew(key)
		if deduper.entryCount() > capacity || deduper.orderCount() > capacity {
			t.Fatalf("metadata exceeded capacity: entries=%d order=%d", deduper.entryCount(), deduper.orderCount())
		}
	}
	clock.Advance(time.Hour)
	for index := range capacity {
		if !deduper.AddIfNew("reinsert-" + string(rune(index+'0'))) {
			t.Fatal("expired key should be reinserted")
		}
	}
	if deduper.entryCount() != capacity || deduper.orderCount() != capacity {
		t.Fatalf("metadata = entries=%d order=%d, want %d", deduper.entryCount(), deduper.orderCount(), capacity)
	}
}

func TestNilDeduperRejectsEveryKey(t *testing.T) {
	t.Parallel()

	var deduper *Deduper
	if deduper.AddIfNew("message") || deduper.AddIfNew("") {
		t.Fatal("nil Deduper must reject every key")
	}
}

func TestDeduperAllowsOnlyOneConcurrentFirstInsert(t *testing.T) {
	t.Parallel()

	clock := newTestClock(time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC))
	deduper := mustDeduper(t, time.Hour, 10, clock.Now)
	const workers = 128
	var start sync.WaitGroup
	start.Add(1)
	results := make(chan bool, workers)
	var workersGroup sync.WaitGroup
	for range workers {
		workersGroup.Add(1)
		go func() {
			defer workersGroup.Done()
			start.Wait()
			results <- deduper.AddIfNew("same-message")
		}()
	}
	start.Done()
	workersGroup.Wait()
	close(results)

	newCount := 0
	for result := range results {
		if result {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("concurrent first inserts = %d, want 1", newCount)
	}
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock {
	return &testClock{now: now}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func (c *testClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func mustDeduper(t *testing.T, ttl time.Duration, capacity int, clock Clock) *Deduper {
	t.Helper()
	deduper, err := NewDeduper(ttl, capacity, clock)
	if err != nil {
		t.Fatalf("NewDeduper() error = %v", err)
	}
	return deduper
}
