package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// newTestRegistry is a registry with the given TTL built from a struct literal, as the cache's unit
// tests build one: no NATS, no watcher, and no expiry heap or timer until its first write.
func newTestRegistry(ttl time.Duration) *SessionRegistry {
	return &SessionRegistry{
		ttl:            ttl,
		cache:          map[string]cachedSession{},
		cacheRevisions: map[string]uint64{},
		lastSeen:       map[string]lastSeenSession{},
	}
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

// fence is the registry's revision fence for id and whether it holds one.
func fence(r *SessionRegistry, id string) (uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	revision, ok := r.cacheRevisions[id]
	return revision, ok
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
		return len(r.cache) == 0 && len(r.lastSeen) == 0 && len(r.cacheRevisions) == 0
	})
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
	r.cacheSessionLocked(id, fresh, r.expiryFor(now, fresh.UpdatedAt), 2)
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

// TestRevisionFenceLeavesWithLastSeenAndAStaleReplayStaysAbsent removes a session, waits for its
// lastSeen record and revision fence to leave one TTL later, and then replays the put the fence
// used to hold off, as a late watcher delivery would. The put is more than one TTL old, so the
// session must stay absent, with no LastSeen answer and no fence.
func TestRevisionFenceLeavesWithLastSeenAndAStaleReplayStaysAbsent(t *testing.T) {
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

	waitUntil(t, func() bool {
		_, ok := fence(r, id)
		return !ok
	})
	if lastSeenHeld(r, id) || r.LastSeen(id) != 0 {
		t.Fatalf("lastSeen record held %v, LastSeen %d, after the fence left; want neither", lastSeenHeld(r, id), r.LastSeen(id))
	}

	r.applyWatched(stale)
	if _, err := r.Get(id); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(%s) after a stale replay = %v, want ErrKeyNotFound", id, err)
	}
	if live, size := r.LiveCount(), r.CacheSize(); live != 0 || size != 0 {
		t.Fatalf("LiveCount %d, CacheSize %d after a stale replay; want 0 and 0", live, size)
	}
	if seen := r.LastSeen(id); seen != 0 {
		t.Fatalf("LastSeen(%s) after a stale replay = %d, want 0", id, seen)
	}
	if revision, ok := fence(r, id); ok {
		t.Fatalf("a stale replay left fence %d", revision)
	}
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
		return len(r.cache) == 0 && len(r.lastSeen) == 0 && len(r.cacheRevisions) == 0 &&
			len(r.expiries) == 0 && len(r.expiryIndex) == 0
	})
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
