package dedupe

import (
	"sync"
	"testing"
	"time"
)

// testClock provides a thread-safe injectable clock for tests.
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

func TestCache_FirstDeliveryNotSeen(t *testing.T) {
	clock := newTestClock()
	c := NewWithClock(10*time.Minute, clock.Now)
	defer c.Stop()

	if c.Seen("key-1", "ses-A") {
		t.Fatal("first check should return false (not yet delivered)")
	}
}

func TestCache_AfterRecordIsSeen(t *testing.T) {
	clock := newTestClock()
	c := NewWithClock(10*time.Minute, clock.Now)
	defer c.Stop()

	c.Record("key-1", "ses-A")
	if !c.Seen("key-1", "ses-A") {
		t.Fatal("should be seen after Record")
	}
}

func TestCache_ClearRemovesRecordedDelivery(t *testing.T) {
	clock := newTestClock()
	c := NewWithClock(10*time.Minute, clock.Now)
	defer c.Stop()

	c.Record("key-1", "ses-A")
	c.Clear("key-1", "ses-A")

	if c.Seen("key-1", "ses-A") {
		t.Fatal("cleared delivery should not be seen")
	}
}

func TestCache_DifferentSessionsIndependent(t *testing.T) {
	clock := newTestClock()
	c := NewWithClock(10*time.Minute, clock.Now)
	defer c.Stop()

	c.Record("key-1", "ses-A")
	if c.Seen("key-1", "ses-B") {
		t.Fatal("different session should not be seen")
	}
}

func TestCache_DifferentKeysIndependent(t *testing.T) {
	clock := newTestClock()
	c := NewWithClock(10*time.Minute, clock.Now)
	defer c.Stop()

	c.Record("key-1", "ses-A")
	if c.Seen("key-2", "ses-A") {
		t.Fatal("different dedupe key should not be seen")
	}
}

func TestCache_ExpiryAfterTTL(t *testing.T) {
	clock := newTestClock()
	c := NewWithClock(10*time.Minute, clock.Now)
	defer c.Stop()

	c.Record("key-1", "ses-A")
	clock.Advance(11 * time.Minute) // past 10min TTL

	if c.Seen("key-1", "ses-A") {
		t.Fatal("entry should have expired after TTL")
	}
}

func TestCache_NotSeenAtExactTTLBoundary(t *testing.T) {
	clock := newTestClock()
	ttl := 10 * time.Minute
	c := NewWithClock(ttl, clock.Now)
	defer c.Stop()

	c.Record("key-1", "ses-A")

	// At exactly TTL: Sub(t) == ttl, which satisfies <= ttl
	clock.Advance(ttl)
	if !c.Seen("key-1", "ses-A") {
		t.Fatal("at exact TTL boundary, entry should still be seen (<=)")
	}

	// One nanosecond past: now expired
	clock.Advance(1)
	if c.Seen("key-1", "ses-A") {
		t.Fatal("past TTL boundary, entry should not be seen")
	}
}

func TestCache_RecordOnlyAfterSuccess(t *testing.T) {
	// Verify that Seen alone never records — only Record does.
	clock := newTestClock()
	c := NewWithClock(10*time.Minute, clock.Now)
	defer c.Stop()

	// Multiple Seen calls without Record
	c.Seen("key-1", "ses-A")
	c.Seen("key-1", "ses-A")

	if c.Len() != 0 {
		t.Fatalf("Seen should not record entries, got len=%d", c.Len())
	}
}

func TestCache_ConcurrentAccess(t *testing.T) {
	c := New(10 * time.Minute)
	defer c.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		key := "key"
		session := "ses"
		go func() {
			defer wg.Done()
			c.Record(key, session)
		}()
		go func() {
			defer wg.Done()
			c.Seen(key, session)
		}()
		go func() {
			defer wg.Done()
			c.Len()
		}()
	}
	wg.Wait()
}

func TestCache_BackgroundCleanup(t *testing.T) {
	clock := newTestClock()
	// Short TTL so the real ticker fires quickly.
	c := NewWithClock(50*time.Millisecond, clock.Now)
	defer c.Stop()

	c.Record("key-1", "ses-A")
	if c.Len() != 1 {
		t.Fatalf("expected 1 entry, got %d", c.Len())
	}

	// Advance fake clock past TTL
	clock.Advance(100 * time.Millisecond)

	// Wait for the real ticker to fire and run cleanup
	time.Sleep(150 * time.Millisecond)

	if c.Len() != 0 {
		t.Fatalf("expected 0 entries after background cleanup, got %d", c.Len())
	}
}

func TestCache_StopHaltsCleanup(t *testing.T) {
	c := New(10 * time.Millisecond)
	c.Stop()

	// Should not panic on stop — goroutine exits cleanly.
	// Record after stop should still work (no cleanup, but functionally correct).
	c.Record("key-1", "ses-A")
	if !c.Seen("key-1", "ses-A") {
		t.Fatal("Record/Seen should still work after Stop")
	}
}

func TestCache_Len(t *testing.T) {
	clock := newTestClock()
	c := NewWithClock(10*time.Minute, clock.Now)
	defer c.Stop()

	if c.Len() != 0 {
		t.Fatalf("expected 0, got %d", c.Len())
	}

	c.Record("key-1", "ses-A")
	c.Record("key-2", "ses-A")
	c.Record("key-1", "ses-B")

	if c.Len() != 3 {
		t.Fatalf("expected 3, got %d", c.Len())
	}
}

// TestCache_PreDeliveryAttemptDedupe validates the pattern used by the listener:
// record BEFORE delivery so redeliveries of the same (dedupe_key, session_id)
// are skipped even when the first attempt timed out (slow-204 case).
func TestCache_PreDeliveryAttemptDedupe(t *testing.T) {
	clock := newTestClock()
	attempts := NewWithClock(5*time.Minute, clock.Now)
	defer attempts.Stop()

	dedupeKey := "publish.abc123"
	sessionID := "ses-target"

	// First delivery attempt: not yet seen
	if attempts.Seen(dedupeKey, sessionID) {
		t.Fatal("first attempt should not be seen")
	}

	// Record attempt BEFORE calling prompt_async
	attempts.Record(dedupeKey, sessionID)

	// Simulate JetStream redelivery (same dedupe_key) — should be skipped
	if !attempts.Seen(dedupeKey, sessionID) {
		t.Fatal("redelivery should be seen after attempt was recorded")
	}

	// Different dedupe_key to same session is still allowed
	if attempts.Seen("publish.different", sessionID) {
		t.Fatal("different dedupe_key should not be blocked")
	}

	// Same dedupe_key to different session is still allowed
	if attempts.Seen(dedupeKey, "ses-other") {
		t.Fatal("same dedupe_key to different session should not be blocked")
	}

	// After 5-min TTL, redelivery is allowed again
	clock.Advance(5*time.Minute + 1)
	if attempts.Seen(dedupeKey, sessionID) {
		t.Fatal("attempt should expire after TTL")
	}
}
