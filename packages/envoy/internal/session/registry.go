package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

const SessionBucket = "envoy_sessions"

type SessionEntry struct {
	Port      int    `json:"port"`
	MachineID string `json:"machine_id"`
	Dir       string `json:"dir"`
	Title     string `json:"title"`
	UpdatedAt int64  `json:"updated_at"`
	// Driving reports whether the claiming process is the one actually driving
	// this session (its explicit -s target or a session it owns), as opposed to a
	// process that merely has the session loaded from shared on-disk state.
	Driving        bool `json:"driving,omitempty"`
	SelfSubscribed bool `json:"self_subscribed,omitempty"`
}

type SessionRegistryOption func(*sessionRegistryOpts)

type sessionRegistryOpts struct {
	replicas int
	ttl      time.Duration
}

func WithSessionReplicas(n int) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.replicas = n }
}

func WithSessionTTL(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.ttl = d }
}

// cachedSession is a SessionEntry plus its local expiry deadline. JetStream
// MaxAge TTL expiry is NOT delivered as a watcher delete event, so the registry
// enforces the TTL locally by pruning entries past expiresAt at read time.
type cachedSession struct {
	entry     SessionEntry
	expiresAt time.Time // zero means never expires (ttl <= 0)
}

func (c cachedSession) expired(now time.Time) bool {
	return !c.expiresAt.IsZero() && now.After(c.expiresAt)
}

// SessionRegistry is a cache-backed view of the envoy_sessions KV bucket. A
// long-lived WatchAll() goroutine keeps an in-memory cache in sync so List()
// and Get() answer from memory with no per-key JetStream round-trips. The old
// implementation did kv.Keys() then a sequential kv.Get() per key, which took
// 12s+ with ~83 sessions over a ~150ms Tailscale RTT and timed out
// GET /v1/sessions. This mirrors store.Registry, which already solved the same
// problem for the interest registry.
type SessionRegistry struct {
	kv   nats.KeyValue
	kvMu sync.RWMutex
	ttl  time.Duration

	mu             sync.RWMutex
	cache          map[string]cachedSession
	cacheRevisions map[string]uint64
	watchErr       error

	watcherMu         sync.Mutex
	watcher           nats.KeyWatcher
	watcherGeneration uint64

	// readyCh is closed when watch() finishes its initial scan of existing KV
	// entries (signalled by the nil sentinel WatchAll() emits after delivering
	// the current value of each existing key). After it closes, the cache is
	// consistent with the durable KV state.
	readyCh   chan struct{}
	readyOnce sync.Once
}

func OpenSessionRegistry(conn *nats.Conn, options ...SessionRegistryOption) (*SessionRegistry, error) {
	opts := sessionRegistryOpts{replicas: 1, ttl: 5 * time.Minute}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(SessionBucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:   SessionBucket,
			TTL:      opts.ttl,
			Replicas: opts.replicas,
			Storage:  nats.FileStorage,
		})
	}
	if err != nil {
		return nil, err
	}
	// Prune against the bucket's real MaxAge, not the hardcoded default. If the
	// envoy_sessions bucket was created with a different TTL (an older build, a
	// config change), pruning against opts.ttl would either hide live sessions
	// or surface expired ones. Status() is one round-trip; fall back to the
	// configured ttl if it fails or reports no TTL.
	ttl := opts.ttl
	if status, statusErr := kv.Status(); statusErr == nil {
		if bucketTTL := status.TTL(); bucketTTL > 0 {
			ttl = bucketTTL
		}
	}
	r := &SessionRegistry{
		kv:             kv,
		ttl:            ttl,
		cache:          map[string]cachedSession{},
		cacheRevisions: map[string]uint64{},
		readyCh:        make(chan struct{}),
	}
	// watch() populates the cache asynchronously via a long-lived KV watcher.
	// The synchronous Keys()+per-key Get() loop it replaces blocks for seconds
	// when the KV stream leader is on a remote node.
	go r.watch()
	return r, nil
}

// Ping verifies the session KV bucket is reachable AND the cache watcher is
// still alive. It uses kv.Status() (one round-trip) so /healthz and the
// self-health monitor stay fast — they must never iterate keys. Once the
// initial scan has completed, a dead watcher is reported so /healthz exposes
// the stale cache while NATS reconnects.
func (r *SessionRegistry) Ping() error {
	if r == nil {
		return ErrNoKV
	}
	kv := r.currentKV()
	if kv == nil {
		return ErrNoKV
	}
	if _, err := kv.Status(); err != nil {
		return err
	}
	return r.watchHealthError()
}

// watchHealthError reports a dead-watcher failure once the initial scan has
// completed. It returns nil during normal startup (before readyCh closes) so a
// listener still warming up is never marked unhealthy, and nil while the
// watcher is alive. Surfaced through Ping() by /healthz and the monitor.
func (r *SessionRegistry) watchHealthError() error {
	if r == nil || !r.CacheReady() {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.watchErr
}

// ErrNoKV is returned when methods are called on a nil SessionRegistry.
var ErrNoKV = fmt.Errorf("session registry: KV unavailable")

// errSessionWatcherStopped is the sentinel watchErr recorded when the KV
// watcher's Updates() channel closes after the initial scan — typically the
// underlying conn died and took the JetStream subscription with it. The cache
// is frozen at that point, so Ping() exposes the failure.

var errSessionWatcherStopped = fmt.Errorf("session registry watcher stopped")

func (r *SessionRegistry) watch() {
	if err := r.startWatch(r.currentKV()); err != nil {
		slog.Error("session registry watch failed", slog.String("error", err.Error()))
		r.setWatchErr(err)
		// Unblock callers of WaitForCacheReady even on watcher failure — they'd
		// rather see the empty-cache symptom (Put write-through still surfaces
		// local sessions) than hang. /healthz exposes the unavailable registry.
		r.signalReady()
	}
}

// Rewatch recreates the KV watcher against conn and clears a sticky terminal
// watcher error only after the replacement watcher starts successfully.
func (r *SessionRegistry) Rewatch(conn *nats.Conn) error {
	if r == nil || conn == nil {
		return ErrNoKV
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return fmt.Errorf("open session registry JetStream: %w", err)
	}
	kv, err := js.KeyValue(SessionBucket)
	if err != nil {
		return fmt.Errorf("open session registry KV bucket: %w", err)
	}
	if err := r.startWatch(kv); err != nil {
		return fmt.Errorf("watch session registry KV bucket: %w", err)
	}
	return nil
}

func (r *SessionRegistry) startWatch(kv nats.KeyValue) error {
	if kv == nil {
		return ErrNoKV
	}
	watcher, err := kv.WatchAll()
	if err != nil {
		return err
	}

	r.watcherMu.Lock()
	previous := r.watcher
	r.watcherGeneration++
	generation := r.watcherGeneration
	r.watcher = watcher
	r.setKV(kv)
	r.watcherMu.Unlock()

	r.setWatchErr(nil)
	if previous != nil {
		_ = previous.Stop()
	}
	go r.consumeWatch(watcher, generation)
	return nil
}

func (r *SessionRegistry) consumeWatch(watcher nats.KeyWatcher, generation uint64) {
	for entry := range watcher.Updates() {
		if entry == nil {
			// WatchAll() emits a nil sentinel after delivering the current value
			// of each existing key. Treat that as "initial scan complete".
			r.signalReady()
			continue
		}
		r.mu.Lock()
		if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
			r.evictCachedSessionLocked(entry.Key(), entry.Revision())
		} else {
			var item SessionEntry
			if err := json.Unmarshal(entry.Value(), &item); err != nil {
				r.evictCachedSessionLocked(entry.Key(), entry.Revision())
				slog.Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key()), slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
			} else {
				r.cacheSessionLocked(entry.Key(), item, r.expiryFor(entry.Created(), item.UpdatedAt), entry.Revision())
			}
		}
		r.mu.Unlock()
	}
	if !r.finishWatch(generation) {
		return
	}
	// Reaching here means watcher.Updates() closed: the watcher terminated (the
	// conn dropped and took the JetStream subscription with it, or the bucket was
	// removed). Without recording this, the cache would keep serving whatever it
	// last held forever and /healthz would still report ready. Flag it; Ping()
	// gates on CacheReady(), so a close before the initial scan completes still
	// unblocks waiters via signalReady() without wedging /healthz mid-startup.
	werr := watcherTerminalError(watcher)
	r.setWatchErr(werr)
	slog.Error("session registry watcher stopped", slog.String("error", werr.Error()))
	r.signalReady()
}

// watcherTerminalError returns the watcher's terminal error if one was emitted
// on its Error() channel (e.g. ErrKeyWatcherTimeout), falling back to a sentinel
// when the channel closed cleanly without a specific cause.
func watcherTerminalError(w nats.KeyWatcher) error {
	select {
	case err, ok := <-w.Error():
		if ok && err != nil {
			return err
		}
	default:
	}
	return errSessionWatcherStopped
}

// expiryFor computes the local TTL deadline for a cached entry. It prefers the
// KV revision's Created() timestamp (the JetStream MaxAge clock), falling back
// to the SessionEntry.UpdatedAt set by Put when no KV metadata is available
// (the write-through path). A non-positive ttl disables local expiry.
func (r *SessionRegistry) expiryFor(created time.Time, updatedAtMillis int64) time.Time {
	if r.ttl <= 0 {
		return time.Time{}
	}
	if !created.IsZero() {
		return created.Add(r.ttl)
	}
	if updatedAtMillis > 0 {
		return time.UnixMilli(updatedAtMillis).Add(r.ttl)
	}
	return time.Now().Add(r.ttl)
}

func (r *SessionRegistry) currentKV() nats.KeyValue {
	r.kvMu.RLock()
	defer r.kvMu.RUnlock()
	return r.kv
}

func (r *SessionRegistry) setKV(kv nats.KeyValue) {
	r.kvMu.Lock()
	r.kv = kv
	r.kvMu.Unlock()
}

func (r *SessionRegistry) finishWatch(generation uint64) bool {
	r.watcherMu.Lock()
	defer r.watcherMu.Unlock()
	if generation != r.watcherGeneration {
		return false
	}
	r.watcher = nil
	return true
}

func (r *SessionRegistry) setWatchErr(err error) {
	r.mu.Lock()
	r.watchErr = err
	r.mu.Unlock()
}

// signalReady closes readyCh exactly once, unblocking any callers of
// WaitForCacheReady. Safe to call from both the success and error paths.
func (r *SessionRegistry) signalReady() {
	r.readyOnce.Do(func() {
		if r.readyCh != nil {
			close(r.readyCh)
		}
	})
}

// WaitForCacheReady blocks until watch() has finished its initial scan of
// existing KV entries, or until the context is cancelled. Callers should set a
// bounded timeout: WatchAll() on a healthy cluster completes in milliseconds.
// On error the caller logs and proceeds (fail open); Put write-through keeps
// local sessions visible and /healthz exposes an unavailable registry.
func (r *SessionRegistry) WaitForCacheReady(ctx context.Context) error {
	if r == nil || r.readyCh == nil {
		return nil
	}
	select {
	case <-r.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TTL returns the liveness window configured by the backing session bucket.
func (r *SessionRegistry) TTL() time.Duration {
	if r == nil {
		return 0
	}
	return r.ttl
}

// CacheReady reports whether the initial KV scan has completed. Observability
// for /healthz — it does NOT iterate keys, so it never hangs.
func (r *SessionRegistry) CacheReady() bool {
	if r == nil || r.readyCh == nil {
		return false
	}
	select {
	case <-r.readyCh:
		return true
	default:
		return false
	}
}

// CacheSize returns the number of cached sessions. Observability only; reports
// the raw count without pruning expired-but-not-yet-read entries.
func (r *SessionRegistry) CacheSize() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// WatchError returns the active watcher's terminal error string, or "" while
// the watcher is running. Surfaced via /healthz for observability.
func (r *SessionRegistry) WatchError() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.watchErr == nil {
		return ""
	}
	return r.watchErr.Error()
}

// WatchFailed reports whether the current watcher has stopped. The listener
// uses it to distinguish a rebuildable dead cache from a transient KV timeout.
func (r *SessionRegistry) WatchFailed() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.watchErr != nil
}

func (r *SessionRegistry) Put(sessionID string, entry SessionEntry) error {
	if r == nil {
		return ErrNoKV
	}
	// Several live processes can hold the same session (shared on-disk state), and
	// blind last-writer-wins routed deliveries to whichever heartbeated last —
	// including processes not driving the session. mergeForClaim arbitrates.
	cur, curErr := r.Get(sessionID)
	entry, err := mergeForClaim(cur, curErr, entry, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	buf, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	kv := r.currentKV()
	if kv == nil {
		return ErrNoKV
	}
	revision, err := kv.Put(sessionID, buf)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cacheSessionLocked(sessionID, entry, r.expiryFor(time.Time{}, entry.UpdatedAt), revision)
	r.mu.Unlock()
	return nil
}

func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
	if r == nil {
		return SessionEntry{}, ErrNoKV
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	cs, ok := r.cache[sessionID]
	if !ok {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	return cs.entry, nil
}

func (r *SessionRegistry) Delete(sessionID string) error {
	if r == nil {
		return ErrNoKV
	}
	revision := r.cachedRevision(sessionID)
	kv := r.currentKV()
	if kv == nil {
		return ErrNoKV
	}
	entry, err := kv.Get(sessionID)
	opts := []nats.DeleteOpt{}
	if err == nil {
		if entry.Revision() > revision {
			revision = entry.Revision()
		}
		opts = append(opts, nats.LastRevision(entry.Revision()))
	} else if !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	if err := kv.Delete(sessionID, opts...); err != nil {
		return err
	}
	entries, err := kv.History(sessionID)
	if err == nil && len(entries) > 0 {
		latest := entries[len(entries)-1]
		r.mu.Lock()
		defer r.mu.Unlock()
		if latest.Operation() == nats.KeyValueDelete || latest.Operation() == nats.KeyValuePurge {
			r.evictCachedSessionLocked(sessionID, latest.Revision())
			return nil
		}
		var item SessionEntry
		if err := json.Unmarshal(latest.Value(), &item); err != nil {
			r.evictCachedSessionLocked(sessionID, latest.Revision())
			slog.Warn("session registry cache evicted malformed value",
				slog.String("key", latest.Key()),
				slog.Uint64("revision", latest.Revision()),
				slog.String("error", err.Error()),
			)
			return nil
		}
		r.cacheSessionLocked(sessionID, item, r.expiryFor(latest.Created(), item.UpdatedAt), latest.Revision())
		return nil
	}
	r.mu.Lock()
	r.evictCachedSessionLocked(sessionID, revision+1)
	r.mu.Unlock()
	return nil
}

// ListEntry is a single entry from List(), keyed by session ID.
type ListEntry struct {
	SessionID string `json:"session_id"`
	SessionEntry
}

// List returns all live sessions from the in-memory cache, pruning entries past
// their local TTL. Cache-only: no JetStream round-trips, so it can't hang on a
// slow KV leader the way the old Keys()+per-key Get() loop did.
func (r *SessionRegistry) List() ([]ListEntry, error) {
	if r == nil {
		return nil, ErrNoKV
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	entries := make([]ListEntry, 0, len(r.cache))
	for key, cs := range r.cache {
		entries = append(entries, ListEntry{SessionID: key, SessionEntry: cs.entry})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}

// pruneLocked deletes entries past their local TTL. Callers must hold r.mu for
// writing.
func (r *SessionRegistry) pruneLocked(now time.Time) {
	for key, cs := range r.cache {
		if cs.expired(now) {
			delete(r.cache, key)
		}
	}
}

func (r *SessionRegistry) cachedRevision(sessionID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheRevisions[sessionID]
}

func (r *SessionRegistry) cacheSessionLocked(sessionID string, entry SessionEntry, expiresAt time.Time, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	r.cache[sessionID] = cachedSession{entry: entry, expiresAt: expiresAt}
	r.cacheRevisions[sessionID] = revision
}

func (r *SessionRegistry) evictCachedSessionLocked(sessionID string, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	delete(r.cache, sessionID)
	r.cacheRevisions[sessionID] = revision
}
