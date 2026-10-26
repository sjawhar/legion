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
	// os.hostname()/ENVOY_MACHINE_ID) names as where the session process actually runs. It is
	// never the routing key: MachineID is the listener that owns this entry's delivery (the
	// registering listener's own ENVOY_MACHINE_ID, store.Registry.Match, hostFor), and stays
	// that way whether every session registers through one shared listener or each machine runs
	// its own. Host is empty for a session whose plugin predates this field (subscribeBody had
	// no "host"); HostOrMachine falls back to MachineID so every caller sees a populated value.
	Host string `json:"host,omitempty"`
}

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
	now      func() time.Time
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

// WithSessionClock sets the registry's clock, time.Now by default: the time a heartbeat records,
// and the time a row's expiry and a last-seen record's retention are measured against. Tests move
// it past a session's TTL without waiting it out.
func WithSessionClock(now func() time.Time) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.now = now }
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
//
// Every watcher the registry's watcher installs reads the whole bucket again (kvwatch.Cache): the
// cache keeps every row through the read and removes the rows the read found no evidence of only
// once it completes (sessionCache), so a list served during the read is the copy it had.
type SessionRegistry struct {
	now func() time.Time

	mu sync.RWMutex
	// ttl is the bucket's MaxAge, the lifetime of a row's local expiry and of a last-seen record. A
	// read of a bucket with another MaxAge moves it (sessionCache.BeginRead).
	ttl            time.Duration
	cache          map[string]cachedSession
	cacheRevisions map[string]uint64
	lastSeen       map[string]lastSeenSession
	// marks are the sessions the current read has evidence of (kvwatch.Marks), and readEpoch the
	// epoch that read began in: the bucket incarnation cacheRevisions belongs to.
	marks     kvwatch.Marks
	readEpoch uint64

	// watcher feeds the cache from the session bucket and holds the handle the registry writes
	// through. Once a read has completed the cache is consistent with the bucket.
	watcher *kvwatch.Watcher

	log *slog.Logger
}

func OpenSessionRegistry(conn *nats.Conn, options ...SessionRegistryOption) (*SessionRegistry, error) {
	opts := sessionRegistryOpts{replicas: 1, ttl: 5 * time.Minute, now: time.Now}
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
	// configured ttl if it fails or reports no TTL. Every read of the bucket adopts its MaxAge
	// again (sessionCache.BeginRead), so a bucket recreated with another TTL is pruned by its own.
	ttl := opts.ttl
	if status, statusErr := kv.Status(); statusErr == nil {
		if bucketTTL := status.TTL(); bucketTTL > 0 {
			ttl = bucketTTL
		}
	}
	r := &SessionRegistry{
		now:            opts.now,
		ttl:            ttl,
		cache:          map[string]cachedSession{},
		cacheRevisions: map[string]uint64{},
		lastSeen:       map[string]lastSeenSession{},
		log:            opts.log,
	}
	r.watcher = kvwatch.New("session registry", kv, sessionCache{r}, kvwatch.WithLogger(opts.log))
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
	r.pruneLastSeenLocked(r.now())
	r.mu.Unlock()
	return r.watcher.Err()
}

// Rewatch moves the cache's watcher and the handle the registry writes through to conn
// (kvwatch.Watcher.Rewatch).
func (r *SessionRegistry) Rewatch(conn *nats.Conn) error {
	return r.watcher.Rewatch(conn)
}

// Servable reports whether the copy List answers from can be served (kvwatch.Watcher.Servable):
// nil once a read of the bucket has completed and the copy follows it, or stopped following it no
// longer ago than the stale bound, and kvwatch.ErrRefilling otherwise. It reads the watcher's
// state only, never the cache.
func (r *SessionRegistry) Servable() error {
	return r.watcher.Servable()
}

// NeedsRead reports whether the copy does not follow its bucket with no read under way to make it,
// which only a Rewatch repairs (kvwatch.Watcher.NeedsRead).
func (r *SessionRegistry) NeedsRead() bool {
	return r.watcher.NeedsRead()
}

// Disconnected says the connection the cache's watcher reads through was lost
// (kvwatch.Watcher.Disconnected).
func (r *SessionRegistry) Disconnected() {
	r.watcher.Disconnected()
}

// logger is the logger WithSessionLogger set, or the default one.
func (r *SessionRegistry) logger() *slog.Logger {
	if r.log == nil {
		return slog.Default()
	}
	return r.log
}

// sessionCache is the registry as the cache its watcher feeds (kvwatch.Cache). The hooks are its
// own type's, not the registry's methods, so nothing but the watcher calls them.
type sessionCache struct{ r *SessionRegistry }

// BeginRead starts a read of the session bucket: a fresh set of marks, the bucket's MaxAge as the
// ttl when it has one, so a bucket recreated with another TTL expires its rows by its own, and, for
// a recreated bucket whose revisions number from 1 again, no revision fence. Every row stays.
func (c sessionCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marks.Begin()
	r.readEpoch = read.Epoch
	if read.Recreated {
		clear(r.cacheRevisions)
	}
	if read.MaxAge > 0 {
		r.ttl = read.MaxAge
	}
}

// Apply takes one record the read delivered. Its session is marked before the revision fence is
// compared, so a delivered value older than a local heartbeat still keeps the session through the
// sweep.
func (c sessionCache) Apply(entry nats.KeyValueEntry, _ kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marks.Mark(entry.Key())
	if err := r.applyEntryLocked(entry); err != nil {
		r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key()), slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
	}
}

// CompleteRead removes every row and fence entry the read found no evidence of: the bucket no
// longer holds that session. A removed row's last heartbeat is kept for LastSeen, as an expired
// row's is. The expired last-seen records go first, once for the whole read: the records the read
// applied leave them in place (cacheSessionLocked says why).
func (c sessionCache) CompleteRead(kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.pruneLastSeenLocked(now)
	kvwatch.Sweep(&r.marks, r.cache, func(sessionID string, removed cachedSession) {
		r.retainLastSeenLocked(sessionID, removed.entry.UpdatedAt, now)
	})
	kvwatch.Sweep(&r.marks, r.cacheRevisions, nil)
	r.marks.End()
}

// applyEntryLocked applies one record of the bucket, delivered or read, to the cache under r.mu.
// A value it cannot decode evicts the session, and is returned for the caller to log.
func (r *SessionRegistry) applyEntryLocked(entry nats.KeyValueEntry) error {
	if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
		r.evictCachedSessionLocked(entry.Key(), entry.Revision())
		return nil
	}
	var item SessionEntry
	if err := json.Unmarshal(entry.Value(), &item); err != nil {
		r.evictCachedSessionLocked(entry.Key(), entry.Revision())
		return err
	}
	r.cacheSessionLocked(entry.Key(), item, r.expiryFor(entry.Created(), item.UpdatedAt), entry.Revision())
	return nil
}

// expiryFor computes the local TTL deadline for a cached entry. It prefers the
// KV revision's Created() timestamp (the JetStream MaxAge clock), falling back
// to the SessionEntry.UpdatedAt set by Put when no KV metadata is available
// (the write-through path). A non-positive ttl disables local expiry. Callers hold r.mu.
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
	return r.now().Add(r.ttl)
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
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	entry, err := mergeForClaim(cur, curErr, entry, r.now().UnixMilli())
	if err != nil {
		return err
	}
	buf, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	kv, epoch := r.watcher.Handle()
	revision, err := kv.Put(sessionID, buf)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.marks.MarkCurrent(r.watcher, epoch, sessionID) {
		r.cacheSessionLocked(sessionID, entry, r.expiryFor(time.Time{}, entry.UpdatedAt), revision)
	}
	r.mu.Unlock()
	return nil
}

// Get answers sessionID's row from the cache. A row past its TTL is not found, even while the
// copy retains it for List because it does not follow its bucket (pruneLocked): to one session's
// caller an expired row is a session gone.
func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	cs, ok := r.cache[sessionID]
	if !ok || cs.expired(now) {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	return cs.entry, nil
}

// Refresh reads sessionID's entry from the session bucket itself, applies it to the cache as a
// delivered record is applied, and then answers as Get does. The cache follows the bucket
// through a watcher and so trails it: a session another listener registered a moment ago can be in
// the bucket and not yet here. Get is the read for a caller that refuses or retries when a session
// is missing; a caller about to take something away from a session because it looks gone asks here
// first. A key no read may name is nats.ErrKeyNotFound, since no session can register under it:
// one bus.KeyValue refuses (bus.ErrRefused), `ses:bad` among them, which is outside nats.go's key
// alphabet and which an earlier build or a direct bucket write could leave as a role holder. Any
// other error is a read that did not answer, ctx's end among them: nats.go's KV read takes no
// context and waits out the JetStream MaxWait (10 s), so Refresh stops waiting when ctx ends and
// lets that read finish on its own. A read of a bucket since recreated is not applied, and Get
// answers from what the new bucket's read delivered.
func (r *SessionRegistry) Refresh(ctx context.Context, sessionID string) (SessionEntry, error) {
	type read struct {
		entry nats.KeyValueEntry
		err   error
	}
	answered := make(chan read, 1)
	kv, epoch := r.watcher.Handle()
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
	r.mu.Lock()
	if r.marks.MarkCurrent(r.watcher, epoch, sessionID) {
		if err := r.applyEntryLocked(got.entry); err != nil {
			r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", got.entry.Key()), slog.Uint64("revision", got.entry.Revision()), slog.String("error", err.Error()))
		}
	}
	r.mu.Unlock()
	return r.Get(sessionID)
}

// LastSeen returns a recently expired or explicitly removed session's final
// heartbeat time. It is retained for one additional registry TTL so callers
// can report why an otherwise durable reference stopped resolving. A row past its TTL that the
// copy retains while it does not follow its bucket answers the same, as Get answers it gone.
func (r *SessionRegistry) LastSeen(sessionID string) int64 {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	if entry, ok := r.lastSeen[sessionID]; ok {
		return entry.lastSeen
	}
	if cs, ok := r.cache[sessionID]; ok && cs.expired(now) {
		return cs.entry.UpdatedAt
	}
	return 0
}

func (r *SessionRegistry) Delete(sessionID string) error {
	// The revision fence read before the delete belongs to the bucket this handle reaches only when
	// the read under way began in its epoch: between a watch moving the epoch and that read
	// beginning, the fence still holds the recreated bucket's revisions.
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	var revision uint64
	if r.readEpoch == epoch {
		revision = r.cacheRevisions[sessionID]
	}
	r.mu.RUnlock()
	read, err := kv.DeleteAtRead(sessionID)
	if err != nil {
		return err
	}
	revision = max(revision, read)
	entries, err := kv.History(sessionID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.marks.MarkCurrent(r.watcher, epoch, sessionID) {
		return nil
	}
	r.pruneLastSeenLocked(r.now())
	if err == nil && len(entries) > 0 {
		latest := entries[len(entries)-1]
		if err := r.applyEntryLocked(latest); err != nil {
			r.logger().Warn("session registry cache evicted malformed value",
				slog.String("key", latest.Key()),
				slog.Uint64("revision", latest.Revision()),
				slog.String("error", err.Error()),
			)
		}
		return nil
	}
	r.evictCachedSessionLocked(sessionID, revision+1)
	return nil
}

// ListEntry is a single entry from List(), keyed by session ID.
type ListEntry struct {
	SessionID string `json:"session_id"`
	SessionEntry
}

// List returns every session the in-memory cache holds. Cache-only: no JetStream round-trips, so
// it can't hang on a slow KV leader the way the old Keys()+per-key Get() loop did. While the copy
// follows its bucket, rows past their local TTL are pruned first; while it does not, every row is
// listed, expired ones included (pruneLocked says why). It never refuses: whether the copy can be
// served at all is Servable's to say.
func (r *SessionRegistry) List() ([]ListEntry, error) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	entries := make([]ListEntry, 0, len(r.cache))
	for key, cs := range r.cache {
		entries = append(entries, ListEntry{SessionID: key, SessionEntry: cs.entry})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}

// pruneLocked removes expired last-seen records and, while the copy follows its bucket
// (kvwatch.Watcher.Following), the rows past their local TTL, keeping each one's last heartbeat
// for LastSeen. A row's TTL is the only word that its session ended, since JetStream's MaxAge
// expiry is never delivered; but while the copy does not follow its bucket it cannot see the
// heartbeats that renewed a row either, and the read that restores following delivers them. So a
// row is pruned for its age only once that read has completed, and until then it stays listed.
// Callers must hold r.mu for writing.
func (r *SessionRegistry) pruneLocked(now time.Time) {
	if r.watcher.Following() {
		for key, cs := range r.cache {
			if cs.expired(now) {
				r.retainLastSeenLocked(key, cs.entry.UpdatedAt, now)
				delete(r.cache, key)
			}
		}
	}
	r.pruneLastSeenLocked(now)
}

// pruneLastSeenLocked removes the last-seen records past their retention. It walks every record,
// so it runs once per call or read, never per applied record. Callers must hold r.mu for writing.
func (r *SessionRegistry) pruneLastSeenLocked(now time.Time) {
	for key, entry := range r.lastSeen {
		if entry.expired(now) {
			delete(r.lastSeen, key)
		}
	}
}

// cacheSessionLocked and evictCachedSessionLocked apply one record and leave expired last-seen
// records in place: a read applies every record of the bucket through them, and pruning the whole
// lastSeen map per record made a read cost its records times the sessions a sweep had just moved
// there. Every path that reads lastSeen (LastSeen, through pruneLocked) prunes it first, and Ping,
// Get, List, Delete and CompleteRead prune it once each, so an expired record is never answered
// and does not outlive the next of those.
func (r *SessionRegistry) cacheSessionLocked(sessionID string, entry SessionEntry, expiresAt time.Time, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	r.cache[sessionID] = cachedSession{entry: entry, expiresAt: expiresAt}
	delete(r.lastSeen, sessionID)
	r.cacheRevisions[sessionID] = revision
}

func (r *SessionRegistry) evictCachedSessionLocked(sessionID string, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	if entry, ok := r.cache[sessionID]; ok {
		r.retainLastSeenLocked(sessionID, entry.entry.UpdatedAt, r.now())
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
