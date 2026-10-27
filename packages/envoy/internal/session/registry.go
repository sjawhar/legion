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
	// Delivery is DeliveryConsumer when the session registered for consumer delivery and the
	// listener it registered through holds its session consumer (bus.SessionConsumers): the
	// session's stored messages reach it from that consumer, relayed by the listener that owns it,
	// so the durable path acknowledges them with no push and no exception
	// (Deliverer.DeliverWithResultForEntry). Empty for a session on the core path: a plugin that did
	// not ask, an older listener, or a hub that refused the consumer.
	Delivery string `json:"delivery,omitempty"`
	// Epoch is the session's delivery epoch while Delivery is DeliveryConsumer: a random string per
	// plugin NATS connection, so a new one tells the relay the session reconnected and its consumer
	// restarts at its ack floor.
	Epoch string `json:"epoch,omitempty"`
}

// DeliveryConsumer is SessionEntry.Delivery for a session whose stored messages its session consumer
// delivers.
const DeliveryConsumer = "consumer"

// HostOrMachine is the session's real machine for display: Host when the registering plugin
// reported one, MachineID (the routing listener's own id) otherwise. MachineID keeps its
// routing meaning either way; this method only decides what a human sees.
func (e SessionEntry) HostOrMachine() string {
	return cmp.Or(e.Host, e.MachineID)
}

type SessionRegistryOption func(*sessionRegistryOpts)

type sessionRegistryOpts struct {
	replicas int
	ttl      time.Duration
	log      *slog.Logger
}

func WithSessionReplicas(n int) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.replicas = n }
}

func WithSessionTTL(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.ttl = d }
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

	log *slog.Logger

	// changeMu guards onChange, the functions OnChange registered, by registration number.
	changeMu   sync.Mutex
	onChange   map[uint64]func(sessionID string)
	nextChange uint64
}

// OnChange registers changed to be called with a session's id after each change to the session's
// cached entry: a write (Put, Refresh, or the watcher delivering another listener's write), a delete
// or purge, an eviction of a malformed value, the cache's reset for a recreated bucket (each id it
// held), and a lapse. A lapse has no watch event (JetStream MaxAge expiry is not delivered, see
// cachedSession), so the registry finds it when it prunes: at each Get, List and LastSeen, and at
// PruneLapsed, which a caller that must see a lapse promptly calls on its own ticker.
//
// changed runs outside the registry's lock, on the goroutine that made the change: the watcher's,
// or a caller's of Put, Delete, Get or the like. Calls can therefore run concurrently and out of
// revision order, so changed reads the session's current state (Get) rather than trusting the order
// of its calls, and it must not block. stop unregisters it.
func (r *SessionRegistry) OnChange(changed func(sessionID string)) (stop func()) {
	r.changeMu.Lock()
	defer r.changeMu.Unlock()
	if r.onChange == nil {
		r.onChange = map[uint64]func(string){}
	}
	r.nextChange++
	id := r.nextChange
	r.onChange[id] = changed
	return func() {
		r.changeMu.Lock()
		delete(r.onChange, id)
		r.changeMu.Unlock()
	}
}

// PruneLapsed drops every cached entry past its TTL, calling OnChange's functions for each. List
// answers the entries that remain, the sessions that are live.
func (r *SessionRegistry) PruneLapsed() {
	r.mu.Lock()
	lapsed := r.pruneLocked(time.Now())
	r.mu.Unlock()
	r.notifyChanged(lapsed...)
}

// notifyChanged calls OnChange's functions for each of sessionIDs. Callers must not hold r.mu.
func (r *SessionRegistry) notifyChanged(sessionIDs ...string) {
	if len(sessionIDs) == 0 {
		return
	}
	r.changeMu.Lock()
	if len(r.onChange) == 0 {
		r.changeMu.Unlock()
		return
	}
	callbacks := make([]func(string), 0, len(r.onChange))
	for _, changed := range r.onChange {
		callbacks = append(callbacks, changed)
	}
	r.changeMu.Unlock()
	for _, sessionID := range sessionIDs {
		for _, changed := range callbacks {
			changed(sessionID)
		}
	}
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
		log:            opts.log,
	}
	r.watcher = kvwatch.New("session registry", kv, r.applyWatched, r.resetCache, kvwatch.WithLogger(opts.log))
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
	if err := r.watcher.Check(); err != nil {
		return err
	}
	r.mu.Lock()
	r.pruneLastSeenLocked(time.Now())
	r.mu.Unlock()
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

// resetCache empties the cache and its revision fence, for a recreated session bucket.
func (r *SessionRegistry) resetCache() {
	r.mu.Lock()
	dropped := make([]string, 0, len(r.cache))
	for sessionID := range r.cache {
		dropped = append(dropped, sessionID)
	}
	r.cache = map[string]cachedSession{}
	r.cacheRevisions = map[string]uint64{}
	r.mu.Unlock()
	r.notifyChanged(dropped...)
}

// applyWatched applies one entry the KV watcher delivered to the cache.
func (r *SessionRegistry) applyWatched(entry nats.KeyValueEntry) {
	var applied bool
	r.mu.Lock()
	if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
		applied = r.evictCachedSessionLocked(entry.Key(), entry.Revision())
	} else {
		var item SessionEntry
		if err := json.Unmarshal(entry.Value(), &item); err != nil {
			applied = r.evictCachedSessionLocked(entry.Key(), entry.Revision())
			r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key()), slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
		} else {
			applied = r.cacheSessionLocked(entry.Key(), item, r.expiryFor(entry.Created(), item.UpdatedAt), entry.Revision())
		}
	}
	r.mu.Unlock()
	if applied {
		r.notifyChanged(entry.Key())
	}
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

// CacheReady reports whether the cache is ready (kvwatch.Watcher.Ready). Observability for
// /healthz; it does not iterate keys, so it never hangs.
func (r *SessionRegistry) CacheReady() bool {
	return r.watcher.Ready()
}

// CacheSize returns the number of cached sessions. Observability only; reports
// the raw count without pruning expired-but-not-yet-read entries.
func (r *SessionRegistry) CacheSize() int {
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
	applied := r.cacheSessionLocked(sessionID, entry, r.expiryFor(time.Time{}, entry.UpdatedAt), revision)
	r.mu.Unlock()
	if applied {
		r.notifyChanged(sessionID)
	}
	return nil
}

func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
	r.mu.Lock()
	lapsed := r.pruneLocked(time.Now())
	cs, ok := r.cache[sessionID]
	r.mu.Unlock()
	r.notifyChanged(lapsed...)
	if !ok {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	return cs.entry, nil
}

// Refresh reads sessionID's entry from the session bucket itself, applies it to the cache through
// the watcher's own path (applyWatched), and then answers as Get does. The cache follows the bucket
// through a watcher and so trails it: a session another listener registered a moment ago can be in
// the bucket and not yet here. Get is the read for a caller that refuses or retries when a session
// is missing; a caller about to take something away from a session because it looks gone asks here
// first. A key no read may name is nats.ErrKeyNotFound, since no session can register under it:
// one bus.KeyValue refuses (bus.ErrRefused), `ses:bad` among them, which is outside nats.go's key
// alphabet and which an earlier build or a direct bucket write could leave as a role holder. Any
// other error is a read that did not answer, ctx's end among them: nats.go's KV read takes no
// context and waits out the JetStream MaxWait (10 s), so Refresh stops waiting when ctx ends and
// lets that read finish on its own.
func (r *SessionRegistry) Refresh(ctx context.Context, sessionID string) (SessionEntry, error) {
	type read struct {
		entry nats.KeyValueEntry
		err   error
	}
	answered := make(chan read, 1)
	kv := r.watcher.KV()
	go func() {
		entry, err := kv.Get(sessionID)
		answered <- read{entry: entry, err: err}
	}()
	var got read
	select {
	case got = <-answered:
	case <-ctx.Done():
		return SessionEntry{}, fmt.Errorf("read session %s from the bucket: %w", sessionID, ctx.Err())
	}
	if errors.Is(got.err, bus.ErrRefused) {
		return SessionEntry{}, fmt.Errorf("%w: %w", nats.ErrKeyNotFound, got.err)
	}
	if got.err != nil {
		return SessionEntry{}, got.err
	}
	r.applyWatched(got.entry)
	return r.Get(sessionID)
}

// LastSeen returns a recently expired or explicitly removed session's final
// heartbeat time. It is retained for one additional registry TTL so callers
// can report why an otherwise durable reference stopped resolving.
func (r *SessionRegistry) LastSeen(sessionID string) int64 {
	now := time.Now()
	r.mu.Lock()
	lapsed := r.pruneLocked(now)
	entry, ok := r.lastSeen[sessionID]
	r.mu.Unlock()
	r.notifyChanged(lapsed...)
	if !ok {
		return 0
	}
	return entry.lastSeen
}
func (r *SessionRegistry) Delete(sessionID string) error {
	revision := r.cachedRevision(sessionID)
	kv := r.watcher.KV()
	read, err := kv.DeleteAtRead(sessionID)
	if err != nil {
		return err
	}
	revision = max(revision, read)
	var applied bool
	entries, err := kv.History(sessionID)
	if err == nil && len(entries) > 0 {
		latest := entries[len(entries)-1]
		r.mu.Lock()
		if latest.Operation() == nats.KeyValueDelete || latest.Operation() == nats.KeyValuePurge {
			applied = r.evictCachedSessionLocked(sessionID, latest.Revision())
		} else {
			var item SessionEntry
			if err := json.Unmarshal(latest.Value(), &item); err != nil {
				applied = r.evictCachedSessionLocked(sessionID, latest.Revision())
				r.logger().Warn("session registry cache evicted malformed value",
					slog.String("key", latest.Key()),
					slog.Uint64("revision", latest.Revision()),
					slog.String("error", err.Error()),
				)
			} else {
				applied = r.cacheSessionLocked(sessionID, item, r.expiryFor(latest.Created(), item.UpdatedAt), latest.Revision())
			}
		}
		r.mu.Unlock()
	} else {
		r.mu.Lock()
		applied = r.evictCachedSessionLocked(sessionID, revision+1)
		r.mu.Unlock()
	}
	if applied {
		r.notifyChanged(sessionID)
	}
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
	r.mu.Lock()
	lapsed := r.pruneLocked(time.Now())
	entries := make([]ListEntry, 0, len(r.cache))
	for key, cs := range r.cache {
		entries = append(entries, ListEntry{SessionID: key, SessionEntry: cs.entry})
	}
	r.mu.Unlock()
	r.notifyChanged(lapsed...)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}

// pruneLocked removes expired live entries and last-seen records, and returns the ids of the
// entries it removed, for notifyChanged once r.mu is released. Callers must hold r.mu for writing.
func (r *SessionRegistry) pruneLocked(now time.Time) []string {
	var lapsed []string
	for key, cs := range r.cache {
		if cs.expired(now) {
			r.retainLastSeenLocked(key, cs.entry.UpdatedAt, now)
			delete(r.cache, key)
			lapsed = append(lapsed, key)
		}
	}
	r.pruneLastSeenLocked(now)
	return lapsed
}

func (r *SessionRegistry) pruneLastSeenLocked(now time.Time) {
	for key, entry := range r.lastSeen {
		if entry.expired(now) {
			delete(r.lastSeen, key)
		}
	}
}

// cacheSessionLocked caches entry as sessionID's at revision, and reports false when the cache
// already holds a later revision, which it keeps.
func (r *SessionRegistry) cacheSessionLocked(sessionID string, entry SessionEntry, expiresAt time.Time, revision uint64) bool {
	if revision < r.cacheRevisions[sessionID] {
		return false
	}
	r.pruneLastSeenLocked(time.Now())
	r.cache[sessionID] = cachedSession{entry: entry, expiresAt: expiresAt}
	delete(r.lastSeen, sessionID)
	r.cacheRevisions[sessionID] = revision
	return true
}

func (r *SessionRegistry) cachedRevision(sessionID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheRevisions[sessionID]
}

// evictCachedSessionLocked drops sessionID's entry as of revision, and reports false when the
// cache already holds a later revision, which it keeps.
func (r *SessionRegistry) evictCachedSessionLocked(sessionID string, revision uint64) bool {
	if revision < r.cacheRevisions[sessionID] {
		return false
	}
	now := time.Now()
	r.pruneLastSeenLocked(now)
	if entry, ok := r.cache[sessionID]; ok {
		r.retainLastSeenLocked(sessionID, entry.entry.UpdatedAt, now)
	}
	delete(r.cache, sessionID)
	r.cacheRevisions[sessionID] = revision
	return true
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
