package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// --- Unit tests (no NATS — operate on a pre-populated cache) ---

func TestExpiryFor_PrefersCreated(t *testing.T) {
	r := &SessionRegistry{ttl: 5 * time.Minute}
	created := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	got := r.expiryFor(created, time.Now().UnixMilli())
	want := created.Add(5 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("expected expiry from Created() %v, got %v", want, got)
	}
}

func TestExpiryFor_FallsBackToUpdatedAt(t *testing.T) {
	r := &SessionRegistry{ttl: 5 * time.Minute}
	updated := time.Date(2021, 6, 1, 12, 0, 0, 0, time.UTC)
	got := r.expiryFor(time.Time{}, updated.UnixMilli())
	want := updated.Add(5 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("expected fallback expiry from UpdatedAt %v, got %v", want, got)
	}
}

func TestExpiryFor_ZeroTTLNeverExpires(t *testing.T) {
	r := &SessionRegistry{ttl: 0}
	got := r.expiryFor(time.Now(), time.Now().UnixMilli())
	if !got.IsZero() {
		t.Fatalf("expected zero (never-expires) for ttl<=0, got %v", got)
	}
}

// followingRegistry opens a registry on the test's own session bucket and waits for its first read
// to complete, so the copy follows the bucket and prunes rows past their TTL (pruneLocked). The
// bucket is empty, so a test puts rows in the cache itself.
func followingRegistry(t *testing.T) *SessionRegistry {
	t.Helper()
	client := setupNATS(t)
	r, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(5*time.Minute))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	waitFor(t, 5*time.Second, r.watcher.Following)
	return r
}

// hold puts rows in r's cache as a read delivers them (sessionCache.Apply), each at revision 1 and
// created one ttl before its expiresAt, so the cache computes that expiry for it (expiryFor).
func hold(t *testing.T, r *SessionRegistry, rows map[string]cachedSession) {
	t.Helper()
	r.mu.RLock()
	ttl := r.ttl
	r.mu.RUnlock()
	for sessionID, row := range rows {
		value, err := json.Marshal(row.entry)
		if err != nil {
			t.Fatalf("encode %s: %v", sessionID, err)
		}
		sessionCache{r}.Apply(kvwatch.Entry{Key: sessionID, Value: value, Revision: 1, Created: row.expiresAt.Add(-ttl)}, kvwatch.Read{})
	}
}

func TestList_PrunesExpiredEntries(t *testing.T) {
	now := time.Now()
	r := followingRegistry(t)
	hold(t, r, map[string]cachedSession{
		"ses_live":    {entry: SessionEntry{Port: 1}, expiresAt: now.Add(time.Minute)},
		"ses_expired": {entry: SessionEntry{Port: 2}, expiresAt: now.Add(-time.Minute)},
	})
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(entries) != 1 || entries[0].SessionID != "ses_live" {
		t.Fatalf("expected only ses_live, got %v", entries)
	}
	// The expired entry must be pruned from the cache, not just filtered.
	if r.CacheSize() != 1 {
		t.Fatalf("expected cache size 1 after prune, got %d", r.CacheSize())
	}
}

func TestGet_ExpiredReturnsKeyNotFound(t *testing.T) {
	now := time.Now()
	r := followingRegistry(t)
	hold(t, r, map[string]cachedSession{
		"ses_expired": {entry: SessionEntry{Port: 2}, expiresAt: now.Add(-time.Second)},
	})
	if _, err := r.Get("ses_expired"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound for expired entry, got %v", err)
	}
	if r.CacheSize() != 0 {
		t.Fatalf("expected expired entry pruned, cache size %d", r.CacheSize())
	}
}

func TestGet_ValidReturnsEntry(t *testing.T) {
	now := time.Now()
	r := followingRegistry(t)
	hold(t, r, map[string]cachedSession{
		"ses_ok": {entry: SessionEntry{Port: 99, MachineID: "m1"}, expiresAt: now.Add(time.Minute)},
	})
	got, err := r.Get("ses_ok")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Port != 99 || got.MachineID != "m1" {
		t.Fatalf("unexpected entry: %+v", got)
	}
}

func TestPingPrunesExpiredLastSeen(t *testing.T) {
	client := setupNATS(t)
	registry, err := OpenSessionRegistry(
		client.Conn,
		WithSessionReplicas(1),
		WithSessionTTL(time.Minute),
	)
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	registry.mu.Lock()
	registry.lastSeen = map[string]lastSeenSession{
		"ses_lapsed": {
			lastSeen:  time.Now().Add(-2 * time.Minute).UnixMilli(),
			expiresAt: time.Now().Add(-time.Minute),
		},
	}
	registry.mu.Unlock()

	if err := registry.Ping(); err != nil {
		t.Fatalf("ping session registry: %v", err)
	}
	registry.mu.RLock()
	_, retained := registry.lastSeen["ses_lapsed"]
	registry.mu.RUnlock()
	if retained {
		t.Fatal("Ping retained an expired last-seen timestamp")
	}
}
func TestLastSeenRetentionDoesNotRestoreLiveSession(t *testing.T) {
	now := time.Now()
	r := followingRegistry(t)
	r.mu.Lock()
	r.lastSeen["ses_lapsed"] = lastSeenSession{
		lastSeen:  now.Add(-time.Minute).UnixMilli(),
		expiresAt: now.Add(time.Minute),
	}
	r.mu.Unlock()

	if _, err := r.Get("ses_lapsed"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(lapsed) error = %v, want key not found", err)
	}
	entries, err := r.List()
	if err != nil {
		t.Fatalf("List sessions: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("live sessions = %+v, want none", entries)
	}
	if got := r.LastSeen("ses_lapsed"); got == 0 {
		t.Fatal("last seen timestamp was lost before its retention TTL")
	}

	hold(t, r, map[string]cachedSession{
		"ses_lapsed": {entry: SessionEntry{Port: 1, UpdatedAt: now.UnixMilli()}, expiresAt: now.Add(time.Minute)},
	})
	if _, err := r.Get("ses_lapsed"); err != nil {
		t.Fatalf("Get(reregistered): %v", err)
	}
	if got := r.LastSeen("ses_lapsed"); got != 0 {
		t.Fatalf("last seen = %d after re-registration, want removed diagnostic", got)
	}
}

// --- Integration tests (a real NATS server, shared via setupNATS) ---

// pollSessionList retries List() until len matches want or timeout fires.
func pollSessionList(t *testing.T, r *SessionRegistry, want int, timeout time.Duration) []ListEntry {
	t.Helper()
	deadline := time.After(timeout)
	for {
		got, err := r.List()
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		if len(got) == want {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for List to return %d entries (got %d)", want, len(got))
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestSessionWatch_BootstrapPopulatesExistingKeys(t *testing.T) {
	client := setupNATS(t)
	// Pre-populate via a first registry, then open a second to simulate a
	// listener restart against existing KV state.
	reg1, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := reg1.Put("ses_boot", SessionEntry{Port: 7, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}

	reg2, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg2.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	// Immediately after readiness, the pre-populated entry must be visible.
	entries, err := reg2.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].SessionID != "ses_boot" {
		t.Fatalf("expected ses_boot after bootstrap, got %v", entries)
	}
}

func TestSessionWatch_PropagatesExternalPut(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Write directly via KV (not reg.Put), so only the watcher can surface it.
	buf, err := json.Marshal(SessionEntry{Port: 5, MachineID: "m1", UpdatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := reg.watcher.KV().Put("ses_ext", buf); err != nil {
		t.Fatalf("kv put: %v", err)
	}
	entries := pollSessionList(t, reg, 1, 5*time.Second)
	if entries[0].SessionID != "ses_ext" || entries[0].Port != 5 {
		t.Fatalf("unexpected entry: %+v", entries[0])
	}
}

func TestSessionWatch_PropagatesDelete(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_d", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	pollSessionList(t, reg, 1, 5*time.Second)

	// Delete directly via KV (bypassing write-through) so we exercise the
	// watcher's delete handling specifically.
	if err := reg.watcher.KV().Delete("ses_d"); err != nil {
		t.Fatalf("kv delete: %v", err)
	}
	pollSessionList(t, reg, 0, 5*time.Second)
}

func TestSessionWatch_PropagatesPurge(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_p", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	pollSessionList(t, reg, 1, 5*time.Second)

	if err := reg.watcher.KV().Purge("ses_p"); err != nil {
		t.Fatalf("kv purge: %v", err)
	}
	pollSessionList(t, reg, 0, 5*time.Second)
}

func TestSessionPut_WriteThroughVisibleImmediately(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_wt", SessionEntry{Port: 8, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// No sleep, no poll — write-through must make it visible synchronously,
	// even before the watcher delivers the event.
	got, err := reg.Get("ses_wt")
	if err != nil {
		t.Fatalf("Get immediately after Put: %v", err)
	}
	if got.Port != 8 {
		t.Fatalf("expected port 8, got %d", got.Port)
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry from write-through, got %d", len(entries))
	}
}

func TestSessionDelete_WriteThroughRemovesImmediately(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_wd", SessionEntry{Port: 8, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := reg.Delete("ses_wd"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Write-through delete must remove from cache synchronously.
	if _, err := reg.Get("ses_wd"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound after delete, got %v", err)
	}
}

func TestSessionList_CacheOnlyAfterNATSShutdown(t *testing.T) {
	// After WaitForCacheReady, List must serve from cache even when NATS is gone
	// — no per-key Get() round-trips that would hang on a dead/slow link. This
	// is the core regression: the old List() did Keys()+per-key Get() and timed
	// out on a remote KV leader.
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := reg.Put("ses_cache", SessionEntry{Port: 3, MachineID: "m1"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	client.Close()

	done := make(chan struct{})
	go func() {
		entries, err := reg.List()
		if err != nil {
			t.Errorf("List after shutdown: %v", err)
		}
		if len(entries) != 1 || entries[0].SessionID != "ses_cache" {
			t.Errorf("expected ses_cache from cache after shutdown, got %v", entries)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("List() hung after NATS shutdown — cache-only invariant broken")
	}
}

func TestOpenSessionRegistry_PrunesUsingBucketTTL(t *testing.T) {
	client := setupNATS(t)
	// Create the bucket with a 2-minute TTL — deliberately different from the
	// 5-minute hardcoded default in OpenSessionRegistry.
	if _, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(2*time.Minute)); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	// Open a second registry WITHOUT a TTL option, so opts.ttl is the 5-minute
	// default. It must adopt the bucket's real 2-minute TTL via Status(), not
	// the default — otherwise pruning is computed against the wrong window.
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if ttl := reg.TTL(); ttl != 2*time.Minute {
		t.Fatalf("expected registry to adopt bucket TTL 2m, got %v", ttl)
	}
	// Rows are pruned for their age only once the copy follows the bucket (pruneLocked).
	waitFor(t, 5*time.Second, reg.watcher.Following)
	// An entry created 3 minutes ago is past the real 2-minute TTL but still
	// within the 5-minute default. Pruning against the bucket TTL drops it;
	// pruning against the default would wrongly keep it live.
	created := time.Now().Add(-3 * time.Minute)
	reg.mu.RLock()
	expiresAt := reg.expiryFor(created, 0)
	reg.mu.RUnlock()
	hold(t, reg, map[string]cachedSession{
		"ses_old": {entry: SessionEntry{Port: 1}, expiresAt: expiresAt},
	})
	entries, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected entry older than real TTL pruned, got %v", entries)
	}
}

func TestSessionWatch_DeadWatcherFailsPing(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	// Healthy watcher: no error yet.
	if err := reg.WatchErr(); err != nil {
		t.Fatalf("expected no watch error while healthy, got %v", err)
	}

	// Closing the connection closes the KV watcher's Updates() channel. Because
	// this happens AFTER the initial scan, the watcher must record a watch error so
	// the frozen cache is no longer reported as healthy.
	client.Close()

	deadline := time.After(5 * time.Second)
	for reg.WatchErr() == nil {
		select {
		case <-deadline:
			t.Fatal("watch error was never set after Updates() closed post-scan")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := reg.Ping(); err == nil {
		t.Fatal("expected Ping to fail once the watcher is dead")
	}
}
