package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

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
	// Host is the machine the client-reported `machineID()` (envoy-client's
	// os.hostname()/ENVOY_MACHINE_ID) names as where the session process actually runs, and the
	// address its owning listener reaches it at (Deliverer.hostFor), unless the session runs on
	// that listener's machine (it registered through it, or Host is that listener's machine id)
	// and its configured HostBridge wins. Only the owning listener delivers
	// to the session; another leaves it to the owner (ErrWrongMachine), whatever Host says. The
	// owner is the listener the session's stored subscription names, the one store.Registry.Match
	// fans its topics out on, else, with no subscription stored, the one MachineID names (any
	// listener when that is empty
	// too). MachineID is the registering listener's own ENVOY_MACHINE_ID, whether every session
	// registers through one shared listener or each machine runs its own; a re-registration
	// through another listener moves it, while the stored subscription keeps its first listener's.
	// Host is empty for a session whose plugin predates this field (subscribeBody had no "host");
	// its owning listener then falls back to host.docker.internal when the session registered
	// through it, else to MachineID, and HostOrMachine falls back to MachineID so every display
	// caller sees a populated value.
	Host string `json:"host,omitempty"`
	// Plugin is the npm package the session registered through and PluginVersion that plugin's
	// build; both empty for a session whose plugin predates the fields.
	Plugin        string `json:"plugin,omitempty"`
	PluginVersion string `json:"plugin_version,omitempty"`
	// ClientInstanceID identifies the client instance that registered this session: one Oh My
	// Pi Envoy extension instance, one Claude Code channel-server process, each minting the id
	// at its own start and sending it on every registration. A soft claim that names this
	// session as its predecessor takes the role only when the claimant carries the same id, or
	// when either side carries none: the same conversation continues inside the instance that
	// held it, while another instance's live session keeps its role however it is named. Empty
	// for a client that predates the field.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
}

// HostOrMachine is the session's real machine for display: Host when the registering plugin
// reported one, MachineID (the routing listener's own id) otherwise. MachineID keeps its
// routing meaning either way; this method only decides what a human sees.
func (e SessionEntry) HostOrMachine() string {
	return cmp.Or(e.Host, e.MachineID)
}

type SessionRegistryOption func(*sessionRegistryOpts)

type sessionRegistryOpts struct {
	replicas          int
	ttl               time.Duration
	lastSeenRetention time.Duration
	log               *slog.Logger
}

func WithSessionReplicas(n int) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.replicas = n }
}

func WithSessionTTL(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.ttl = d }
}

// WithLastSeenRetention sets how long after a session leaves the cache LastSeen still answers its
// final heartbeat; the id's revision fence is kept as long. The default, and any value shorter
// than the bucket's TTL, is one TTL: the fence must outlive the id's leaving by at least one TTL
// for dropping it to stay safe (expireDue). The listener keeps it for as long as the interest
// reaper can still hold a dead session's interest, so its interests API can report the session's
// last heartbeat for every interest row it still lists.
func WithLastSeenRetention(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.lastSeenRetention = d }
}

// WithSessionLogger sets the logger the registry and its cache watcher write through. The listener
// passes its own, so their lines are JSON records with machine_id, as internal/store's are
// (store.WithLogger says why the listener never sets the default instead); without it they log
// where slog.Default() points.
func WithSessionLogger(log *slog.Logger) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.log = log }
}

// cachedSession is a SessionEntry plus its local expiry deadline. JetStream
// MaxAge TTL expiry is NOT delivered as a watcher delete event, so the registry
// enforces the TTL locally: a read reports an entry past expiresAt as absent, and
// the expiry timer removes it from the cache at that deadline (expireDue).
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
	// lastSeenRetention is WithLastSeenRetention's value; zero, as in a registry built from a
	// struct literal, means one TTL (retention).
	lastSeenRetention time.Duration

	mu             sync.RWMutex
	cache          map[string]cachedSession
	cacheRevisions map[string]uint64
	lastSeen       map[string]lastSeenSession

	// expiries holds the deadline of each cached entry and of each lastSeen record and revision
	// fence, earliest first; expiryIndex finds an id's item so a write moves it in place.
	// expiryTimer is armed for expiryArmedFor, the earliest deadline when it was last armed, and runs
	// expireDue. A registry built from a struct literal starts with none of them (scheduleLocked).
	expiries       expiryHeap
	expiryIndex    map[expiryKey]*expiryItem
	expiryTimer    *time.Timer
	expiryArmedFor time.Time

	// watcher feeds the cache from the session bucket and holds the handle the registry writes
	// through. Once it is ready the cache is consistent with the bucket.
	watcher *kvwatch.Watcher

	log *slog.Logger
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
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{
		Bucket:   bus.SessionBucket,
		TTL:      opts.ttl,
		Replicas: opts.replicas,
		Storage:  nats.FileStorage,
	})
	if err != nil {
		return nil, err
	}
	// Expire against the bucket's real MaxAge, not the hardcoded default. If the
	// envoy_sessions bucket was created with a different TTL (an older build, a
	// config change), expiring against opts.ttl would either hide live sessions
	// or surface expired ones. Status() is one round-trip; fall back to the
	// configured ttl if it fails or reports no TTL.
	ttl := opts.ttl
	if status, statusErr := kv.Status(); statusErr == nil {
		if bucketTTL := status.TTL(); bucketTTL > 0 {
			ttl = bucketTTL
		}
	}
	r := &SessionRegistry{
		ttl:               ttl,
		lastSeenRetention: opts.lastSeenRetention,
		cache:             map[string]cachedSession{},
		cacheRevisions:    map[string]uint64{},
		lastSeen:          map[string]lastSeenSession{},
		log:               opts.log,
	}
	r.watcher = kvwatch.New("session registry", kv, r.applyWatched, r.resetCache, kvwatch.WithLogger(opts.log))
	// The watcher populates the cache asynchronously. A synchronous Keys()+per-key Get() loop
	// blocks for seconds when the KV stream leader is on a remote node.
	r.watcher.Start()
	return r, nil
}

// Ping verifies the session KV bucket is reachable AND the cache watcher is
// still alive. It uses kv.Status() (one round-trip) so /healthz and the
// self-health monitor stay fast — they must never iterate keys. A dead watcher
// is reported so /healthz exposes the stale cache while NATS reconnects.
func (r *SessionRegistry) Ping() error {
	if err := r.watcher.Check(); err != nil {
		return err
	}
	return r.watcher.Err()
}

// Rewatch moves the cache's watcher and the handle the registry writes through to conn
// (kvwatch.Watcher.Rewatch).
func (r *SessionRegistry) Rewatch(conn *nats.Conn) error {
	return r.watcher.Rewatch(conn)
}

// logger is the logger WithSessionLogger set, or the default one.
func (r *SessionRegistry) logger() *slog.Logger {
	if r.log == nil {
		return slog.Default()
	}
	return r.log
}

// resetCache empties the cache and its revision fence, for a recreated session bucket. The expiry
// heap keeps its items, and they stay correct: a live item removes nothing unless the cache holds
// an entry with its deadline, and a lastSeen item still removes the lastSeen record, which
// resetCache keeps, and drops a fence only for an id the cache does not hold.
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
			r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key()), slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
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

// EndWatch ends the cache's watchers at shutdown, within ctx, while the connection still carries
// requests (kvwatch.Watcher.End).
func (r *SessionRegistry) EndWatch(ctx context.Context) {
	r.watcher.End(ctx)
}

// WaitForCacheReady blocks until the watcher has delivered every existing session, or until ctx
// is done. Callers bound ctx; on error the caller logs and proceeds (fail open): Put write-through
// keeps local sessions visible and /healthz exposes an unavailable registry.
func (r *SessionRegistry) WaitForCacheReady(ctx context.Context) error {
	return r.watcher.WaitReady(ctx)
}

// TTL returns the liveness window configured by the backing session bucket.
func (r *SessionRegistry) TTL() time.Duration {
	return r.ttl
}

// retention is how long after a session leaves the cache its lastSeen record and revision fence
// are kept: WithLastSeenRetention's value, and never less than one TTL, so a registry built from a
// struct literal keeps them one TTL. It means nothing when the TTL is not positive: then nothing
// expires and no record is kept.
func (r *SessionRegistry) retention() time.Duration {
	return max(r.lastSeenRetention, r.ttl)
}

// CacheReady reports whether the cache is ready (kvwatch.Watcher.Ready). Observability for
// /healthz; it does not iterate keys, so it never hangs.
func (r *SessionRegistry) CacheReady() bool {
	return r.watcher.Ready()
}

// CacheSize returns the number of cached sessions. Observability only; reports
// the raw count, including expired entries the expiry timer has not removed yet.
func (r *SessionRegistry) CacheSize() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cache)
}

// LiveCount returns the number of cached sessions that have not expired: the length List would
// answer, counted under the shared lock with no allocation.
func (r *SessionRegistry) LiveCount() int {
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	live := 0
	for _, cs := range r.cache {
		if !cs.expired(now) {
			live++
		}
	}
	return live
}

// WatchErr is the cache watcher's terminal error, or nil while it runs, so the listener can tell a
// rebuildable dead cache from a transient KV timeout.
func (r *SessionRegistry) WatchErr() error {
	return r.watcher.Err()
}

// Put is PutWithin with no deadline of its own, for a caller that answers no request.
func (r *SessionRegistry) Put(sessionID string, entry SessionEntry) error {
	return r.PutWithin(context.Background(), sessionID, entry)
}

// PutWithin writes sessionID's entry, its bucket write waiting no longer than ctx (bus.Bounded): an
// HTTP route passes its request's own context. A write ctx abandoned may still land after this
// answer. That is safe: it writes the entry the caller's retry writes again, it rides the
// connection ahead of any write this listener sends on it later, so it cannot undo one, and
// against another listener's write it is last-writer-wins, as every Put already is.
func (r *SessionRegistry) PutWithin(ctx context.Context, sessionID string, entry SessionEntry) error {
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
	kv := r.watcher.KV()
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.Put(sessionID, buf) })
	if !answered {
		return fmt.Errorf("write session %s to the bucket: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cacheSessionLocked(sessionID, entry, r.expiryFor(time.Time{}, entry.UpdatedAt), revision)
	r.mu.Unlock()
	return nil
}

// Get returns sessionID's live entry from the cache, or nats.ErrKeyNotFound when the cache holds
// none or holds one past its deadline. It takes only the shared lock and changes nothing: the
// expiry timer, not a read, removes an expired entry.
func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
	r.mu.RLock()
	cs, ok := r.cache[sessionID]
	r.mu.RUnlock()
	if !ok || cs.expired(time.Now()) {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	return cs.entry, nil
}

// Refresh reads sessionID's entry from the session bucket itself, applies it to the cache through
// the watcher's own path (applyWatched), and then answers as Get does. The cache follows the bucket
// through a watcher and so trails it: a session another listener registered a moment ago, or one
// this listener has not re-read since its connection to the bus came back, can be in the bucket and
// not yet here. A caller that acts on a session being missing asks here — one about to take
// something away from a session because it looks gone, and one about to tell a sender that its
// destination is gone, which makes that sender route around the pane, report it, or stop. Get is
// the read for a caller that only enriches what it already holds, or that asks again anyway, since
// a cache miss cannot tell a session that ended from one this cache has not caught up with. A key
// no read may name is nats.ErrKeyNotFound, since no session can register under it: one
// bus.KeyValue refuses (bus.ErrRefused), `ses:bad` among them, which is outside nats.go's key
// alphabet and which an earlier build or a direct bucket write could leave as a role holder. Any
// other error is a read that did not answer, ctx's end among them: nats.go's KV read takes no
// context and waits out the JetStream MaxWait (10 s), so Refresh stops waiting when ctx ends and
// lets that read finish on its own (bus.Bounded).
func (r *SessionRegistry) Refresh(ctx context.Context, sessionID string) (SessionEntry, error) {
	kv := r.watcher.KV()
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if !answered {
		return SessionEntry{}, fmt.Errorf("read session %s from the bucket: %w", sessionID, err)
	}
	if errors.Is(err, bus.ErrRefused) {
		return SessionEntry{}, fmt.Errorf("%w: %w", nats.ErrKeyNotFound, err)
	}
	if err != nil {
		return SessionEntry{}, err
	}
	r.applyWatched(entry)
	return r.Get(sessionID)
}

// LastSeen returns a recently expired or explicitly removed session's final
// heartbeat time. It is retained for the registry's retention (one TTL unless
// WithLastSeenRetention set longer) after the session left the cache, so callers
// can report why an otherwise durable reference stopped resolving. A live
// session answers 0. It takes only the shared lock and changes nothing: an
// expired entry the expiry timer has not removed yet answers what its removal
// will record (its UpdatedAt until the retention past its deadline), so the
// answer is the same before and after the removal.
func (r *SessionRegistry) LastSeen(sessionID string) int64 {
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if cs, ok := r.cache[sessionID]; ok {
		if !cs.expired(now) {
			return 0
		}
		if seen, ok := r.lastSeenRecord(cs.entry.UpdatedAt, cs.expiresAt); ok && !seen.expired(now) {
			return seen.lastSeen
		}
	}
	seen, ok := r.lastSeen[sessionID]
	if !ok || seen.expired(now) {
		return 0
	}
	return seen.lastSeen
}

// Delete deletes sessionID's entry at the revision it reads (bus.KeyValue.DeleteAtRead), each
// bucket call waiting no longer than ctx (bus.Bounded): DELETE /v1/sessions/<session_id> passes its
// request's own context. A delete ctx abandoned may still land after this answer, and that is safe:
// it deletes only the revision it read, so a registration that landed since is kept, and a key it
// found absent it deletes at none, as DeleteAtRead always has.
func (r *SessionRegistry) Delete(ctx context.Context, sessionID string) error {
	revision := r.cachedRevision(sessionID)
	kv := r.watcher.KV()
	read, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.DeleteAtRead(sessionID) })
	if !answered {
		return fmt.Errorf("delete session %s from the bucket: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	revision = max(revision, read)
	// A history that does not answer falls back to the fence below: the delete itself answered.
	entries, _, err := bus.Bounded(ctx, func() ([]nats.KeyValueEntry, error) { return kv.History(sessionID) })
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
			r.logger().Warn("session registry cache evicted malformed value",
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

// List returns all live sessions from the in-memory cache, leaving out entries past
// their local TTL as it copies. It takes only the shared lock and changes nothing.
// Cache-only: no JetStream round-trips, so it can't hang on a slow KV leader the
// way the old Keys()+per-key Get() loop did.
func (r *SessionRegistry) List() ([]ListEntry, error) {
	now := time.Now()
	r.mu.RLock()
	entries := make([]ListEntry, 0, len(r.cache))
	for key, cs := range r.cache {
		if cs.expired(now) {
			continue
		}
		entries = append(entries, ListEntry{SessionID: key, SessionEntry: cs.entry})
	}
	r.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}

// cacheSessionLocked caches entry for sessionID at revision unless the revision fence holds a newer
// revision, and schedules the entry's removal at expiresAt. A put whose deadline has already passed
// for an id the cache does not hold is not inserted: it would read as absent anyway, and inserting
// it would bring back a lastSeen record for a session that left long ago. That is the late delivery
// of a revision older than a fence expireDue already dropped. Callers must hold r.mu for writing.
func (r *SessionRegistry) cacheSessionLocked(sessionID string, entry SessionEntry, expiresAt time.Time, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	cs := cachedSession{entry: entry, expiresAt: expiresAt}
	if _, cached := r.cache[sessionID]; !cached && cs.expired(time.Now()) {
		return
	}
	r.cache[sessionID] = cs
	delete(r.lastSeen, sessionID)
	r.cacheRevisions[sessionID] = revision
	r.scheduleLocked(sessionID, expireLive, expiresAt)
}

func (r *SessionRegistry) cachedRevision(sessionID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheRevisions[sessionID]
}

// evictCachedSessionLocked removes sessionID from the cache for a delete, a purge or a malformed
// value at revision, unless the revision fence holds a newer revision, and moves the removed entry's
// final heartbeat to lastSeen: for the retention from now, or for an entry already past its
// deadline, for the retention past that deadline, which is what its expiry would have recorded. The
// fence moves to revision and leaves the retention from now (expireDue), whether or not a lastSeen
// record was written. Callers must hold r.mu for writing.
func (r *SessionRegistry) evictCachedSessionLocked(sessionID string, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	now := time.Now()
	if cs, ok := r.cache[sessionID]; ok {
		left := now
		if cs.expired(now) {
			left = cs.expiresAt
		}
		r.retainLastSeenLocked(sessionID, cs.entry.UpdatedAt, left)
	}
	delete(r.cache, sessionID)
	r.cacheRevisions[sessionID] = revision
	r.scheduleLocked(sessionID, expireLastSeen, now.Add(r.retention()))
}

// retainLastSeenLocked records lastSeen as sessionID's final heartbeat until the retention after
// left, the time the session left the cache. Callers must hold r.mu for writing.
func (r *SessionRegistry) retainLastSeenLocked(sessionID string, lastSeen int64, left time.Time) {
	seen, ok := r.lastSeenRecord(lastSeen, left)
	if !ok {
		return
	}
	if r.lastSeen == nil {
		r.lastSeen = map[string]lastSeenSession{}
	}
	r.lastSeen[sessionID] = seen
}

// lastSeenRecord is the record a session whose final heartbeat was lastSeen keeps for the
// retention after it left the cache at left, or false when none is kept (no TTL, or no heartbeat
// time). LastSeen answers an expired entry still in the cache from the same record, so it answers
// the same before and after the entry's removal.
func (r *SessionRegistry) lastSeenRecord(lastSeen int64, left time.Time) (lastSeenSession, bool) {
	if r.ttl <= 0 || lastSeen <= 0 {
		return lastSeenSession{}, false
	}
	return lastSeenSession{lastSeen: lastSeen, expiresAt: left.Add(r.retention())}, true
}
