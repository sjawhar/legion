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
	"github.com/sjawhar/envoy/internal/kvwatch"
)

const SessionBucket = "envoy_sessions"

type SessionEntry struct {
	Port         int      `json:"port"`
	MachineID    string   `json:"machine_id"`
	Dir          string   `json:"dir"`
	Title        string   `json:"title"`
	Capabilities []string `json:"capabilities"`
	UpdatedAt    int64    `json:"updated_at"`
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

type lastSeenSession struct {
	lastSeen  int64
	expiresAt time.Time
}

func (s lastSeenSession) expired(now time.Time) bool {
	return !s.expiresAt.IsZero() && now.After(s.expiresAt)
}

// SessionRegistry is a cache-backed view of the envoy_sessions KV bucket. A
// long-lived WatchAll() goroutine keeps an in-memory cache in sync so List()
// and Get() answer from memory with no per-key JetStream round-trips. The old
// implementation did kv.Keys() then a sequential kv.Get() per key, which took
// 12s+ with ~83 sessions over a ~150ms Tailscale RTT and timed out
// GET /v1/sessions. This mirrors store.Registry, which already solved the same
// problem for the interest registry.
type SessionRegistry struct {
	ttl time.Duration

	mu             sync.RWMutex
	cache          map[string]cachedSession
	cacheRevisions map[string]uint64
	lastSeen       map[string]lastSeenSession

	// watcher feeds the cache from the session bucket and holds the handle the registry writes
	// through. Once it is ready the cache is consistent with the bucket.
	watcher *kvwatch.Watcher
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
		ttl:            ttl,
		cache:          map[string]cachedSession{},
		cacheRevisions: map[string]uint64{},
		lastSeen:       map[string]lastSeenSession{},
	}
	r.watcher = kvwatch.New("session registry", kv, r.applyWatched, r.resetCache)
	// The watcher populates the cache asynchronously. A synchronous Keys()+per-key Get() loop
	// blocks for seconds when the KV stream leader is on a remote node.
	r.watcher.Start()
	return r, nil
}

// Ping verifies the session KV bucket is reachable AND the cache watcher is
// still alive. It uses kv.Status() (one round-trip) so /healthz and the
// self-health monitor stay fast — they must never iterate keys. It also prunes
// expired diagnostic last-seen timestamps on every monitor cycle. A dead watcher
// is reported so /healthz exposes the stale cache while NATS reconnects.
func (r *SessionRegistry) Ping() error {
	if r == nil {
		return ErrNoKV
	}
	if err := r.watcher.Check(); err != nil {
		return err
	}
	r.mu.Lock()
	r.pruneLastSeenLocked(time.Now())
	r.mu.Unlock()
	return r.watcher.Err()
}

// ErrNoKV is returned when methods are called on a nil SessionRegistry.
var ErrNoKV = fmt.Errorf("session registry: KV unavailable")

// Rewatch moves the cache's watcher and the handle the registry writes through to conn
// (kvwatch.Watcher.Rewatch).
func (r *SessionRegistry) Rewatch(conn *nats.Conn) error {
	return r.watcher.Rewatch(conn)
}

// resetCache empties the cache and its revision fence, for a recreated session bucket.
func (r *SessionRegistry) resetCache() {
	r.mu.Lock()
	r.cache = map[string]cachedSession{}
	r.cacheRevisions = map[string]uint64{}
	r.mu.Unlock()
}

// applyWatched applies one entry the KV watcher delivered to the cache.
func (r *SessionRegistry) applyWatched(entry nats.KeyValueEntry) {
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

// StopWatch retires the cache's watcher for a shutdown (kvwatch.Watcher.Stop).
func (r *SessionRegistry) StopWatch() {
	r.watcher.Stop()
}

// WaitForCacheReady blocks until the watcher has delivered every existing session, or until ctx
// is done. Callers bound ctx; on error the caller logs and proceeds (fail open): Put write-through
// keeps local sessions visible and /healthz exposes an unavailable registry.
func (r *SessionRegistry) WaitForCacheReady(ctx context.Context) error {
	return r.watcher.WaitReady(ctx)
}

// TTL returns the liveness window configured by the backing session bucket.
func (r *SessionRegistry) TTL() time.Duration {
	if r == nil {
		return 0
	}
	return r.ttl
}

// CacheReady reports whether the cache is ready (kvwatch.Watcher.Ready). Observability for
// /healthz; it does not iterate keys, so it never hangs.
func (r *SessionRegistry) CacheReady() bool {
	return r.watcher.Ready()
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

// WatchErr is the cache watcher's terminal error, or nil while it runs, so the listener can tell a
// rebuildable dead cache from a transient KV timeout.
func (r *SessionRegistry) WatchErr() error {
	return r.watcher.Err()
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
	revision, err := r.watcher.KV().Put(sessionID, buf)
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

// LastSeen returns a recently expired or explicitly removed session's final
// heartbeat time. It is retained for one additional registry TTL so callers
// can report why an otherwise durable reference stopped resolving.
func (r *SessionRegistry) LastSeen(sessionID string) int64 {
	if r == nil {
		return 0
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	entry, ok := r.lastSeen[sessionID]
	if !ok {
		return 0
	}
	return entry.lastSeen
}
func (r *SessionRegistry) Delete(sessionID string) error {
	if r == nil {
		return ErrNoKV
	}
	revision := r.cachedRevision(sessionID)
	kv := r.watcher.KV()
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

// pruneLocked removes expired live entries and last-seen records. Callers must
// hold r.mu for writing.
func (r *SessionRegistry) pruneLocked(now time.Time) {
	for key, cs := range r.cache {
		if cs.expired(now) {
			r.retainLastSeenLocked(key, cs.entry.UpdatedAt, now)
			delete(r.cache, key)
		}
	}
	r.pruneLastSeenLocked(now)
}

func (r *SessionRegistry) pruneLastSeenLocked(now time.Time) {
	for key, entry := range r.lastSeen {
		if entry.expired(now) {
			delete(r.lastSeen, key)
		}
	}
}
func (r *SessionRegistry) cacheSessionLocked(sessionID string, entry SessionEntry, expiresAt time.Time, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	r.pruneLastSeenLocked(time.Now())
	r.cache[sessionID] = cachedSession{entry: entry, expiresAt: expiresAt}
	delete(r.lastSeen, sessionID)
	r.cacheRevisions[sessionID] = revision
}

func (r *SessionRegistry) cachedRevision(sessionID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheRevisions[sessionID]
}

func (r *SessionRegistry) evictCachedSessionLocked(sessionID string, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	now := time.Now()
	r.pruneLastSeenLocked(now)
	if entry, ok := r.cache[sessionID]; ok {
		r.retainLastSeenLocked(sessionID, entry.entry.UpdatedAt, now)
	}
	delete(r.cache, sessionID)
	r.cacheRevisions[sessionID] = revision
}

func (r *SessionRegistry) retainLastSeenLocked(sessionID string, lastSeen int64, now time.Time) {
	if r.ttl <= 0 || lastSeen <= 0 {
		return
	}
	if r.lastSeen == nil {
		r.lastSeen = map[string]lastSeenSession{}
	}
	r.lastSeen[sessionID] = lastSeenSession{
		lastSeen:  lastSeen,
		expiresAt: now.Add(r.ttl),
	}
}
