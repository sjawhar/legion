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
	kv  nats.KeyValue
	ttl time.Duration

	mu       sync.RWMutex
	cache    map[string]cachedSession
	watchErr error

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
		kv:      kv,
		ttl:     ttl,
		cache:   map[string]cachedSession{},
		readyCh: make(chan struct{}),
	}
	// watch() populates the cache asynchronously via a long-lived KV watcher.
	// The synchronous Keys()+per-key Get() loop it replaces blocks for seconds
	// when the KV stream leader is on a remote node.
	go r.watch()
	return r, nil
}

// Ping verifies the session KV bucket is reachable AND the cache watcher is
// still alive. Returns nil on success. It uses kv.Status() (one round-trip) so
// /healthz and the self-health watchdog stay fast — they must never iterate
// keys. Once the initial scan has completed, a dead watcher is also reported as
// an error: the watchdog can then SIGTERM the listener so the restart policy
// rebuilds the watcher on a fresh conn. A cache frozen by a dead watcher would
// otherwise serve dead sessions forever, which is worse than a restart.
func (r *SessionRegistry) Ping() error {
	if r == nil {
		return ErrNoKV
	}
	if _, err := r.kv.Status(); err != nil {
		return err
	}
	return r.watchHealthError()
}

// watchHealthError reports a dead-watcher failure once the initial scan has
// completed. It returns nil during normal startup (before readyCh closes) so a
// listener still warming up is never marked unhealthy, and nil while the watcher
// is alive. Surfaced via Ping() for /healthz and the self-health watchdog.
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
// is frozen at that point, so Ping() surfaces this to trigger a restart.
var errSessionWatcherStopped = fmt.Errorf("session registry watcher stopped")

func (r *SessionRegistry) watch() {
	w, err := r.kv.WatchAll()
	if err != nil {
		slog.Error("session registry watch failed", slog.String("error", err.Error()))
		r.setWatchErr(err)
		// Unblock callers of WaitForCacheReady even on watcher failure — they'd
		// rather see the empty-cache symptom (Put write-through still surfaces
		// local sessions) than hang. The self-health watchdog catches a
		// persistently broken registry via its KV pings.
		r.signalReady()
		return
	}
	for entry := range w.Updates() {
		if entry == nil {
			// WatchAll() emits a nil sentinel after delivering the current value
			// of each existing key. Treat that as "initial scan complete".
			r.signalReady()
			continue
		}
		r.mu.Lock()
		if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
			delete(r.cache, entry.Key())
		} else {
			var item SessionEntry
			if err := json.Unmarshal(entry.Value(), &item); err == nil {
				r.cache[entry.Key()] = cachedSession{
					entry:     item,
					expiresAt: r.expiryFor(entry.Created(), item.UpdatedAt),
				}
			}
		}
		r.mu.Unlock()
	}
	// Reaching here means w.Updates() closed: the watcher terminated (the conn
	// dropped and took the JetStream subscription with it, or the bucket was
	// removed). Without recording this, the cache would keep serving whatever it
	// last held forever and /healthz would still report ready. Flag it; Ping()
	// gates on CacheReady(), so a close before the initial scan completes still
	// unblocks waiters via signalReady() without wedging /healthz mid-startup.
	werr := watcherTerminalError(w)
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
// On error the caller should log + proceed (fail open) — Put write-through
// keeps local sessions visible and the self-health watchdog arranges a restart
// for a persistently broken link.
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

// WatchError returns the watcher startup error string, or "" when the watcher
// started cleanly. Surfaced via /healthz for observability.
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

func (r *SessionRegistry) Put(sessionID string, entry SessionEntry) error {
	if r == nil {
		return ErrNoKV
	}
	entry.UpdatedAt = time.Now().UnixMilli()
	buf, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := r.kv.Put(sessionID, buf); err != nil {
		return err
	}
	// Write through to the local cache so this session is visible immediately,
	// even if the watcher lags or the initial scan hasn't finished yet.
	r.mu.Lock()
	r.cache[sessionID] = cachedSession{
		entry:     entry,
		expiresAt: r.expiryFor(time.Time{}, entry.UpdatedAt),
	}
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
	if err := r.kv.Delete(sessionID); err != nil {
		return err
	}
	// Write through so the deletion is visible immediately.
	r.mu.Lock()
	delete(r.cache, sessionID)
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
