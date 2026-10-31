package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// newTestRegistry is a registry with the given TTL built from a struct literal, as the cache's unit
// tests build one: no NATS, no watcher, and no expiry heap or timer until its first write. With no
// watcher the copy counts as following its bucket (SessionRegistry.following), so rows expire by
// their deadlines alone.
func newTestRegistry(ttl time.Duration) *SessionRegistry {
	r := &SessionRegistry{
		now:      time.Now,
		ttl:      ttl,
		lastSeen: map[string]lastSeenSession{},
	}
	r.cache = newSessionCopy(r)
	return r
}

// applyWatched applies one record as the session bucket's watcher delivers it
// (sessionCache.Apply), which is how the cache's own tests write to the copy.
func (r *SessionRegistry) applyWatched(entry natsgo.KeyValueEntry) {
	sessionCache{r}.Apply(kvwatch.LegacyEntry(entry), kvwatch.Read{})
}

// sweep runs a read of the bucket that delivers nothing, so the copy removes every row and fence
// entry it found no evidence of (kvwatch.Copy.CompleteRead): the one place the merged design
// drops a fence, in place of the lastSeen deadline that dropped it before.
func (r *SessionRegistry) sweep() {
	sessionCache{r}.BeginRead(kvwatch.Read{})
	sessionCache{r}.CompleteRead(kvwatch.Read{})
}

// watchedEntry is a nats.KeyValueEntry the session bucket's watcher could have delivered.
type watchedEntry struct {
	key      string
	value    []byte
	revision uint64
	created  time.Time
	op       natsgo.KeyValueOp
}

func (e watchedEntry) Bucket() string               { return "envoy_sessions" }
func (e watchedEntry) Key() string                  { return e.key }
func (e watchedEntry) Value() []byte                { return e.value }
func (e watchedEntry) Revision() uint64             { return e.revision }
func (e watchedEntry) Created() time.Time           { return e.created }
func (e watchedEntry) Delta() uint64                { return 0 }
func (e watchedEntry) Operation() natsgo.KeyValueOp { return e.op }

// watchedPut is the watcher's delivery of a put of item under key at revision, written at created.
func watchedPut(t testing.TB, key string, revision uint64, created time.Time, item SessionEntry) watchedEntry {
	t.Helper()
	value, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal %s: %v", key, err)
	}
	return watchedEntry{key: key, value: value, revision: revision, created: created, op: natsgo.KeyValuePut}
}

// watchedDelete is the watcher's delivery of a delete marker for key at revision.
func watchedDelete(key string, revision uint64) watchedEntry {
	return watchedEntry{key: key, revision: revision, created: time.Now(), op: natsgo.KeyValueDelete}
}

// waitUntil polls condition until it holds, failing the test after five seconds.
func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the registry's expiry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForCacheSize waits, reading only CacheSize, until the cache holds want entries.
func waitForCacheSize(t *testing.T, r *SessionRegistry, want int) {
	t.Helper()
	waitUntil(t, func() bool { return r.CacheSize() == want })
}

// lastSeenHeld reports whether the registry still holds a lastSeen record for id, expired or not.
func lastSeenHeld(r *SessionRegistry, id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.lastSeen[id]
	return ok
}

// fence is the revision the copy's fence holds for id, and whether it holds one. A registry built
// from a struct literal has no watcher, so its read epoch is the zero one (kvwatch.Copy.FenceFor).
func fence(r *SessionRegistry, id string) (uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	revision := r.cache.FenceFor(0, id)
	return revision, revision != 0
}

// TestRegistryReadsRaceWatchedWritesAndExpiry runs Get, List, LastSeen and LiveCount from several
// goroutines while one applies watcher puts and deletes and a short TTL keeps the expiry callback
// removing entries, for the race detector. Every read answers an entry that was written, and once
// the writes stop every entry, lastSeen record and fence leaves at its deadline.
func TestRegistryReadsRaceWatchedWritesAndExpiry(t *testing.T) {
	const ttl = 20 * time.Millisecond
	r := newTestRegistry(ttl)
	ids := make([]string, 40)
	puts := make([]watchedEntry, len(ids))
	for i := range ids {
		ids[i] = fmt.Sprintf("ses_race_%02d", i)
		puts[i] = watchedPut(t, ids[i], 0, time.Time{}, SessionEntry{Port: i + 1, UpdatedAt: time.Now().UnixMilli()})
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		revision := uint64(0)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			revision++
			j := i % len(ids)
			if i%7 == 0 {
				r.applyWatched(watchedDelete(ids[j], revision))
				continue
			}
			put := puts[j]
			put.revision, put.created = revision, time.Now()
			r.applyWatched(put)
		}
	})
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i, id := range ids {
					if entry, err := r.Get(id); err == nil && entry.Port != i+1 {
						t.Errorf("Get(%s).Port = %d, want %d", id, entry.Port, i+1)
						return
					}
					_ = r.LastSeen(id)
				}
				entries, err := r.List()
				if err != nil || len(entries) > len(ids) {
					t.Errorf("List = %d entries, %v; want at most %d", len(entries), err, len(ids))
					return
				}
				if live := r.LiveCount(); live > len(ids) {
					t.Errorf("LiveCount = %d, want at most %d", live, len(ids))
					return
				}
			}
		})
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	waitUntil(t, func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.cache.Len() == 0 && len(r.lastSeen) == 0
	})
	// The fences outlive the rows: a completed read of the bucket is the one thing that drops
	// them (kvwatch.Copy.CompleteRead), and a read that delivers nothing drops them all.
	r.sweep()
	if revision, held := fence(r, ids[0]); held {
		t.Fatalf("a completed read left fence %d for %s", revision, ids[0])
	}
	if live := r.LiveCount(); live != 0 {
		t.Fatalf("LiveCount after every deadline = %d, want 0", live)
	}
}

// TestExpiryKeepsAnEntryRewrittenBetweenItsDeadlineAndItsCallback holds the registry's write lock
// across an entry's deadline, so the expiry callback waits, and writes the session's next heartbeat
// as the watcher would. When the callback runs it must find the newer entry and keep it.
func TestExpiryKeepsAnEntryRewrittenBetweenItsDeadlineAndItsCallback(t *testing.T) {
	const ttl = time.Minute
	r := newTestRegistry(ttl)
	const id = "ses_rewritten"
	deadline := time.Now().Add(100 * time.Millisecond)
	r.applyWatched(watchedPut(t, id, 1, deadline.Add(-ttl), SessionEntry{Port: 1, UpdatedAt: deadline.Add(-ttl).UnixMilli()}))

	r.mu.Lock()
	time.Sleep(time.Until(deadline) + 50*time.Millisecond)
	now := time.Now()
	fresh := SessionEntry{Port: 2, UpdatedAt: now.UnixMilli()}
	// What applyWatched does with the watcher's next put; it takes the lock this test holds.
	value, err := json.Marshal(fresh)
	if err != nil {
		t.Fatalf("marshal %s: %v", id, err)
	}
	if err := r.cache.Delivered(kvwatch.Entry{Key: id, Value: value, Revision: 2, Created: now}); err != nil {
		t.Fatalf("deliver %s: %v", id, err)
	}
	r.mu.Unlock()
	// The timer's callback runs once the lock is free; run it here too, so it has certainly run
	// after the write.
	r.expireDue()

	got, err := r.Get(id)
	if err != nil {
		t.Fatalf("Get(%s) after the old deadline's callback: %v", id, err)
	}
	if got.Port != 2 {
		t.Fatalf("Get(%s).Port = %d, want the rewritten entry's 2", id, got.Port)
	}
	if size := r.CacheSize(); size != 1 {
		t.Fatalf("cache size = %d, want 1", size)
	}
	if seen := r.LastSeen(id); seen != 0 || lastSeenHeld(r, id) {
		t.Fatalf("LastSeen(%s) = %d (record held %v), want no record for a live session", id, seen, lastSeenHeld(r, id))
	}
}

// TestRevisionFenceLeavesWithAReadAndAStaleReplayStaysAbsent removes a session, waits for its
// lastSeen record to leave one TTL later, and then replays the put the fence used to hold off, as
// a late watcher delivery would. The put is more than one TTL old, so the session must stay
// absent, with no LastSeen answer; the fence leaves at the next completed read of the bucket, not
// at the record's deadline, and a replay after that stays absent on its age alone (decodeLocked).
func TestRevisionFenceLeavesWithAReadAndAStaleReplayStaysAbsent(t *testing.T) {
	const ttl = 100 * time.Millisecond
	r := newTestRegistry(ttl)
	const id = "ses_fenced"
	created := time.Now()
	stale := watchedPut(t, id, 5, created, SessionEntry{Port: 1, UpdatedAt: created.UnixMilli()})
	r.applyWatched(stale)
	r.applyWatched(watchedDelete(id, 6))
	if revision, ok := fence(r, id); !ok || revision != 6 {
		t.Fatalf("fence after the delete = %d (held %v), want 6", revision, ok)
	}
	r.applyWatched(stale)
	if _, err := r.Get(id); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(%s) after a replay the fence holds off = %v, want ErrKeyNotFound", id, err)
	}

	waitUntil(t, func() bool { return !lastSeenHeld(r, id) })
	if r.LastSeen(id) != 0 {
		t.Fatalf("LastSeen %d after the record left; want 0", r.LastSeen(id))
	}
	// The fence leaves only with a completed read that found the bucket holds no such key.
	if revision, held := fence(r, id); !held || revision != 6 {
		t.Fatalf("fence after the record left = %d (held %v), want the delete's 6", revision, held)
	}
	r.sweep()

	// With the fence swept, the replay is just a record of the bucket again: it is past its
	// deadline, so it never reads as live, the expiry removes it at once, and the record its
	// removal writes leaves at its own deadline, a retention past the row's.
	r.applyWatched(stale)
	if _, err := r.Get(id); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(%s) after a stale replay = %v, want ErrKeyNotFound", id, err)
	}
	if live := r.LiveCount(); live != 0 {
		t.Fatalf("LiveCount %d after a stale replay; want 0", live)
	}
	waitForCacheSize(t, r, 0)
	waitUntil(t, func() bool { return r.LastSeen(id) == 0 })
}

// TestRevisionFencesStayBoundedAsSessionsComeAndGo registers waves of sessions, deletes half of
// each wave and lets the other half expire, and then waits: one TTL after the last of them left,
// the registry holds no entry, lastSeen record, fence or deadline for any of them (R3).
func TestRevisionFencesStayBoundedAsSessionsComeAndGo(t *testing.T) {
	const ttl = 50 * time.Millisecond
	r := newTestRegistry(ttl)
	revision := uint64(0)
	for wave := range 3 {
		for i := range 100 {
			id := fmt.Sprintf("ses_wave%d_%03d", wave, i)
			revision++
			now := time.Now()
			r.applyWatched(watchedPut(t, id, revision, now, SessionEntry{Port: i + 1, UpdatedAt: now.UnixMilli()}))
			if i%2 == 0 {
				revision++
				r.applyWatched(watchedDelete(id, revision))
			}
		}
	}
	waitUntil(t, func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.cache.Len() == 0 && len(r.lastSeen) == 0 &&
			len(r.expiries) == 0 && len(r.expiryIndex) == 0
	})
	// Each wave's fences leave with the next completed read of the bucket, so nothing it holds
	// outlives the bucket's own keys (kvwatch.Copy.CompleteRead).
	r.sweep()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if got := r.cache.FenceFor(0, "ses_wave0_000"); got != 0 {
		t.Fatalf("a completed read left fence %d", got)
	}
}

// TestNonPositiveTTLNeverRemovesOrSchedules writes, ages and deletes sessions in registries whose
// TTL is zero or negative: nothing expires, nothing is scheduled, and a delete keeps its fence.
func TestNonPositiveTTLNeverRemovesOrSchedules(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		t.Run(ttl.String(), func(t *testing.T) {
			r := newTestRegistry(ttl)
			old := time.Now().Add(-time.Hour)
			r.applyWatched(watchedPut(t, "ses_kept", 1, old, SessionEntry{Port: 1, UpdatedAt: old.UnixMilli()}))
			r.applyWatched(watchedPut(t, "ses_deleted", 2, old, SessionEntry{Port: 2, UpdatedAt: old.UnixMilli()}))
			r.applyWatched(watchedDelete("ses_deleted", 3))
			time.Sleep(20 * time.Millisecond)

			if _, err := r.Get("ses_kept"); err != nil {
				t.Fatalf("Get(ses_kept): %v", err)
			}
			if live, size := r.LiveCount(), r.CacheSize(); live != 1 || size != 1 {
				t.Fatalf("LiveCount %d, CacheSize %d; want 1 and 1", live, size)
			}
			if revision, ok := fence(r, "ses_deleted"); !ok || revision != 3 {
				t.Fatalf("fence for ses_deleted = %d (held %v), want 3", revision, ok)
			}
			if seen := r.LastSeen("ses_deleted"); seen != 0 {
				t.Fatalf("LastSeen(ses_deleted) = %d, want 0 with no TTL", seen)
			}
			r.mu.RLock()
			scheduled, timer := len(r.expiries), r.expiryTimer
			r.mu.RUnlock()
			if scheduled != 0 || timer != nil {
				t.Fatalf("scheduled %d deadlines (timer %v), want none", scheduled, timer)
			}
		})
	}
}

// TestLastSeenRetentionKeepsTheRecordAndFenceThatLong lets a session expire and removes another in
// a registry whose retention is longer than its TTL: LastSeen answers each one's final heartbeat,
// and the fence holds off a stale replay, until the retention has passed since the session left
// the cache, well past one TTL, and both leave together after it.
func TestLastSeenRetentionKeepsTheRecordAndFenceThatLong(t *testing.T) {
	const (
		ttl       = 50 * time.Millisecond
		retention = time.Second
	)
	r := newTestRegistry(ttl)
	r.lastSeenRetention = retention
	created := time.Now()
	heartbeat := created.UnixMilli()
	stale := watchedPut(t, "ses_deleted", 1, created, SessionEntry{Port: 1, UpdatedAt: heartbeat})
	r.applyWatched(watchedPut(t, "ses_expired", 2, created, SessionEntry{Port: 2, UpdatedAt: heartbeat}))
	r.applyWatched(stale)
	deleted := time.Now()
	r.applyWatched(watchedDelete("ses_deleted", 3))
	waitForCacheSize(t, r, 0)

	// Several TTLs after both left, well inside the retention.
	time.Sleep(4 * ttl)
	for _, id := range []string{"ses_expired", "ses_deleted"} {
		if got := r.LastSeen(id); got != heartbeat {
			t.Fatalf("LastSeen(%s) %s after it left = %d, want its heartbeat %d", id, time.Since(created), got, heartbeat)
		}
	}
	if revision, ok := fence(r, "ses_deleted"); !ok || revision != 3 {
		t.Fatalf("fence for ses_deleted = %d (held %v), want 3 within the retention", revision, ok)
	}
	r.applyWatched(stale)
	if _, err := r.Get("ses_deleted"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(ses_deleted) after a replay the fence holds off = %v, want ErrKeyNotFound", err)
	}
	if got := r.LastSeen("ses_deleted"); got != heartbeat {
		t.Fatalf("LastSeen(ses_deleted) after a replay the fence holds off = %d, want %d", got, heartbeat)
	}

	waitUntil(t, func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return len(r.lastSeen) == 0 && len(r.expiries) == 0
	})
	if gone := time.Since(deleted); gone < retention {
		t.Fatalf("records and fences left %s after ses_deleted was removed, before the retention %s", gone, retention)
	}
	for _, id := range []string{"ses_expired", "ses_deleted"} {
		if got := r.LastSeen(id); got != 0 {
			t.Fatalf("LastSeen(%s) after the retention = %d, want 0", id, got)
		}
	}
}

// TestLastSeenRetentionShorterThanTheTTLKeepsOneTTL asks for a retention shorter than the TTL: the
// record is still kept one TTL, and the fence stays until a completed read of the bucket drops it.
func TestLastSeenRetentionShorterThanTheTTLKeepsOneTTL(t *testing.T) {
	const ttl = 500 * time.Millisecond
	r := newTestRegistry(ttl)
	r.lastSeenRetention = time.Millisecond
	now := time.Now()
	r.applyWatched(watchedPut(t, "ses_deleted", 1, now, SessionEntry{Port: 1, UpdatedAt: now.UnixMilli()}))
	deleted := time.Now()
	r.applyWatched(watchedDelete("ses_deleted", 2))

	// Many retentions later, well inside one TTL.
	time.Sleep(50 * time.Millisecond)
	if got := r.LastSeen("ses_deleted"); got != now.UnixMilli() {
		t.Fatalf("LastSeen(ses_deleted) %s after it left = %d, want its heartbeat %d", time.Since(deleted), got, now.UnixMilli())
	}
	if revision, ok := fence(r, "ses_deleted"); !ok || revision != 2 {
		t.Fatalf("fence for ses_deleted = %d (held %v), want 2 within one TTL", revision, ok)
	}
	waitUntil(t, func() bool { return !lastSeenHeld(r, "ses_deleted") })
	if gone := time.Since(deleted); gone < ttl {
		t.Fatalf("the record left %s after the delete, before one TTL %s", gone, ttl)
	}
	r.sweep()
	if revision, ok := fence(r, "ses_deleted"); ok {
		t.Fatalf("a completed read left fence %d", revision)
	}
}
