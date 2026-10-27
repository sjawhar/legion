package dedupe

import (
	"sync"
	"testing"
	"time"
)

// testClock is a thread-safe injectable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Now()}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestMemory(clock *testClock) *Memory {
	m := NewMemory()
	m.now = clock.Now
	return m
}

func mustAttempt(t *testing.T, ledger Ledger, dedupeKey, sessionID string, want Verdict) Attempt {
	t.Helper()
	attempt, err := ledger.Attempt(dedupeKey, sessionID)
	if err != nil {
		t.Fatalf("Attempt(%q, %q): %v", dedupeKey, sessionID, err)
	}
	if attempt.Verdict != want {
		t.Fatalf("Attempt(%q, %q) = %s, want %s", dedupeKey, sessionID, attempt.Verdict, want)
	}
	return attempt
}

func TestMemory_FirstAttemptProceeds(t *testing.T) {
	m := newTestMemory(newTestClock())
	mustAttempt(t, m, "key-1", "ses-A", Proceed)
}

func TestMemory_AnUnresolvedAttemptIsInFlightUntilAttemptedAge(t *testing.T) {
	clock := newTestClock()
	m := newTestMemory(clock)
	mustAttempt(t, m, "publish.abc123", "ses-target", Proceed)

	// A redelivery of the same pair while the first prompt's answer never came (slow 204, #389).
	mustAttempt(t, m, "publish.abc123", "ses-target", InFlight)
	// Another dedupe key to the same session, and the same key to another session, are their own.
	mustAttempt(t, m, "publish.different", "ses-target", Proceed)
	mustAttempt(t, m, "publish.abc123", "ses-other", Proceed)

	clock.Advance(AttemptedAge)
	mustAttempt(t, m, "publish.abc123", "ses-target", InFlight)
	clock.Advance(1)
	mustAttempt(t, m, "publish.abc123", "ses-target", Proceed)
}

func TestMemory_ARecordedPairIsDeliveredUntilDeliveredAge(t *testing.T) {
	clock := newTestClock()
	m := newTestMemory(clock)
	if err := m.Record(mustAttempt(t, m, "key-1", "ses-A", Proceed)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	mustAttempt(t, m, "key-1", "ses-A", Delivered)
	mustAttempt(t, m, "key-1", "ses-B", Proceed)
	mustAttempt(t, m, "key-2", "ses-A", Proceed)

	clock.Advance(DeliveredAge)
	mustAttempt(t, m, "key-1", "ses-A", Delivered)
	clock.Advance(1)
	mustAttempt(t, m, "key-1", "ses-A", Proceed)
}

func TestMemory_ClearLetsTheNextAttemptProceedAndKeepsADelivery(t *testing.T) {
	m := newTestMemory(newTestClock())
	attempt := mustAttempt(t, m, "key-1", "ses-A", Proceed)
	if err := m.Clear(attempt); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	retry := mustAttempt(t, m, "key-1", "ses-A", Proceed)

	if err := m.Record(retry); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := m.Clear(retry); err != nil {
		t.Fatalf("Clear after Record: %v", err)
	}
	mustAttempt(t, m, "key-1", "ses-A", Delivered)
}

func TestMemory_SettlingAnAttemptThatHoldsNothingChangesNothing(t *testing.T) {
	m := newTestMemory(newTestClock())
	if err := m.Record(Attempt{}); err != nil {
		t.Fatalf("Record of the zero attempt: %v", err)
	}
	if len(m.records) != 0 {
		t.Fatalf("Record of the zero attempt stored %d records", len(m.records))
	}
	mustAttempt(t, m, "key-1", "ses-A", Proceed)
	skipped := mustAttempt(t, m, "key-1", "ses-A", InFlight)
	if err := m.Clear(skipped); err != nil {
		t.Fatalf("Clear of an InFlight answer: %v", err)
	}
	mustAttempt(t, m, "key-1", "ses-A", InFlight)
}

func TestMemory_AWriteSweepsRecordsPastTheirAge(t *testing.T) {
	clock := newTestClock()
	m := newTestMemory(clock)
	mustAttempt(t, m, "key-1", "ses-A", Proceed)
	if err := m.Record(mustAttempt(t, m, "key-2", "ses-A", Proceed)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	clock.Advance(AttemptedAge + memorySweepEvery)
	mustAttempt(t, m, "key-3", "ses-A", Proceed)
	if _, ok := m.records[Key{DedupeKey: "key-1", SessionID: "ses-A"}]; ok {
		t.Fatal("the attempt past AttemptedAge was not swept")
	}
	if _, ok := m.records[Key{DedupeKey: "key-2", SessionID: "ses-A"}]; !ok {
		t.Fatal("the delivery inside DeliveredAge was swept")
	}
}

func TestMemory_ConcurrentAccess(t *testing.T) {
	m := NewMemory()
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			attempt, err := m.Attempt("key", "ses")
			if err == nil {
				_ = m.Record(attempt)
			}
		}()
		go func() {
			defer wg.Done()
			attempt, err := m.Attempt("key", "ses")
			if err == nil {
				_ = m.Clear(attempt)
			}
		}()
	}
	wg.Wait()
}
