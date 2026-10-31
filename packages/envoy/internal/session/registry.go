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
	liveWindow        time.Duration
	goneHorizon       time.Duration
	lastSeenRetention time.Duration
	log               *slog.Logger
	now               func() time.Time
}

func WithSessionReplicas(n int) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.replicas = n }
}

func WithSessionTTL(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.ttl = d }
}

// WithLastSeenRetention sets how long after a session leaves the cache LastSeen still answers its
// final heartbeat. The default is LastSeenMemory (2 hours), longer than the gone horizon so that
// a caller told a session is gone can still say when it was last seen; any value shorter than the
// bucket's TTL is one TTL.
func WithLastSeenRetention(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.lastSeenRetention = d }
}

// WithSessionLiveWindow sets how long a session's last registration keeps it live (LiveWindow by
// default). A test makes it short to reach the unresponsive state without waiting out five
// minutes.
func WithSessionLiveWindow(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.liveWindow = d }
}

// WithSessionGoneHorizon sets how long past the live window an unresponsive session's record
// still stands before the session is gone (GoneHorizon by default).
func WithSessionGoneHorizon(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.goneHorizon = d }
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
//
// Every watcher the registry's watcher installs reads the whole bucket again (kvwatch.Cache): the
// cache keeps every row through the read and removes the rows the read found no evidence of only
// once it completes (sessionCache), so a list served during the read is the copy it had.
type SessionRegistry struct {
	now func() time.Time
	// liveWindow and goneHorizon are the windows this registry judges a record's age against
	// (StateOf); zero, as in a registry built from a struct literal, means the package's own
	// (windows). Both are fixed when the registry is opened, so no lock covers them.
	liveWindow  time.Duration
	goneHorizon time.Duration
	// lastSeenRetention is WithLastSeenRetention's value; zero, as in a registry built from a
	// struct literal, means one TTL (retention).
	lastSeenRetention time.Duration

	mu sync.RWMutex
	// ttl is the bucket's MaxAge, the lifetime of a row's local expiry and of a last-seen record,
	// and nothing to do with a session's state, which its own registration time decides (StateOf).
	// A read of a bucket with another MaxAge moves it (sessionCache.BeginRead), and so does the
	// listener when a pass of its reaper reads one (SetTTL).
	ttl time.Duration
	// cache is the copy of the bucket: its rows, revision fence, marks and read epoch
	// (kvwatch.Copy), which every write site reaches through. A row it removes keeps its last
	// heartbeat in lastSeen (removedLocked), and one it stores drops it (storedLocked). Its rows
	// are pointers so that a changed TTL can move every deadline in place with no row copied and
	// no record decoded again (SetTTL).
	cache    kvwatch.Copy[*cachedSession]
	lastSeen map[string]lastSeenSession
	// followingSince is when the read that last restored following swept the copy
	// (sessionCache.CompleteRead), and the zero time until the first one does, as in a registry
	// built from a struct literal, which has no watcher and so misses no record. No row left the
	// live view before it: a deadline that passed while the copy did not follow its bucket is no
	// evidence that the session ended, since the copy could not see the heartbeats that renewed
	// it either, so the retention of such a row's last heartbeat runs from here (departedLocked).
	followingSince time.Time

	// expiries holds the deadline of each cached row and of each lastSeen record, earliest first;
	// expiryIndex finds an id's item so a write moves it in place. expiryTimer is armed for
	// expiryArmedFor, the earliest deadline when it was last armed, and runs expireDue. A registry
	// built from a struct literal starts with none of them (scheduleLocked).
	expiries       expiryHeap
	expiryIndex    map[expiryKey]*expiryItem
	expiryTimer    *time.Timer
	expiryArmedFor time.Time

	// watcher feeds the cache from the session bucket and holds the handle the registry writes
	// through. Once a read has completed the cache is consistent with the bucket.
	watcher *kvwatch.Watcher

	log *slog.Logger
}

func OpenSessionRegistry(conn *nats.Conn, options ...SessionRegistryOption) (*SessionRegistry, error) {
	opts := sessionRegistryOpts{replicas: 1, ttl: 5 * time.Minute, liveWindow: LiveWindow, goneHorizon: GoneHorizon, lastSeenRetention: LastSeenMemory, now: time.Now}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := bus.EnsureKeyValue(conn, js, &nats.KeyValueConfig{
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
	// configured ttl if it fails or reports no TTL. Every read of the bucket adopts its MaxAge
	// again (sessionCache.BeginRead), so a bucket recreated with another TTL expires by its own.
	ttl := opts.ttl
	if status, statusErr := kv.Status(); statusErr == nil {
		if bucketTTL := status.TTL(); bucketTTL > 0 {
			ttl = bucketTTL
		}
	}
	r := &SessionRegistry{
		now:               opts.now,
		liveWindow:        opts.liveWindow,
		goneHorizon:       opts.goneHorizon,
		lastSeenRetention: opts.lastSeenRetention,
		ttl:               ttl,
		lastSeen:          map[string]lastSeenSession{},
		log:               opts.log,
	}
	r.cache = newSessionCopy(r)
	r.watcher = kvwatch.New("session registry", kv, sessionCache{r}, kvwatch.WithLogger(opts.log))
	// The watcher populates the cache asynchronously. A synchronous Keys()+per-key Get() loop
	// blocks for seconds when the KV stream leader is on a remote node.
	r.watcher.Start()
	return r, nil
}

// newSessionCopy is the copy of the session bucket r keeps: the one place its rows are decoded and
// the one place what it records beside them is kept in step (storedLocked, removedLocked).
func newSessionCopy(r *SessionRegistry) kvwatch.Copy[*cachedSession] {
	return kvwatch.NewCopy(kvwatch.Rows[*cachedSession]{Decode: r.decodeLocked, Stored: r.storedLocked, Removed: r.removedLocked})
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

// following reports whether the copy follows its bucket (kvwatch.Watcher.Following). A registry
// built from a struct literal, which only a test does, has no watcher and so misses no record: its
// rows expire by their deadlines alone.
func (r *SessionRegistry) following() bool {
	return r.watcher == nil || r.watcher.Following()
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

// BeginRead starts a read of the session bucket (kvwatch.Copy.BeginRead) with the bucket's MaxAge
// as the ttl when it has one, so a bucket recreated with another TTL expires its rows by its own.
// Every row stays.
func (c sessionCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache.BeginRead(read)
	if read.MaxAge > 0 {
		r.ttl = read.MaxAge
	}
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a value it could not
// decode, which evicted the session.
func (c sessionCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.cache.Delivered(entry); err != nil {
		r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key), slog.Uint64("revision", entry.Revision), slog.String("error", err.Error()))
	}
}

// CompleteRead removes every row and fence entry the read found no evidence of: the bucket no
// longer holds that session. A removed row's last heartbeat is kept for LastSeen, and its record's
// own deadline is scheduled, as an expired row's are (removedLocked). The copy follows its bucket
// from here (kvwatch.Watcher.completeRead calls this and only then records that it does), so this
// is where a row the copy retained past its deadline while it did not follow leaves the live view:
// the sweep takes away the rows the bucket no longer holds, and the rows it keeps are judged
// against their age again from now (departedLocked).
func (c sessionCache) CompleteRead(kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	r.followingSince = r.now()
	r.cache.CompleteRead()
}

// decodeLocked is the row a put record of the bucket, delivered or read, leaves in the cache, with
// its local expiry measured from the record's Created (expiryFor).
//
// A record older than the row the copy removed at its deadline needs no test of its own here: the
// copy keeps that key's revision in its fence when it removes a row for the cache's own reasons
// (kvwatch.Copy.Remove), and drops the fence only at a completed read that found the bucket no
// longer holds the key, so such a replay is fenced out before it is decoded. A record the copy
// does keep is answered absent once it is past its deadline (Get) and removed at it (expireDue).
// Callers hold r.mu.
func (r *SessionRegistry) decodeLocked(entry kvwatch.Entry) (*cachedSession, bool, error) {
	var item SessionEntry
	if err := json.Unmarshal(entry.Value, &item); err != nil {
		return nil, false, err
	}
	return &cachedSession{entry: item, expiresAt: r.expiryFor(entry.Created, item.UpdatedAt)}, true, nil
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

// TTL returns the maximum age of the backing session bucket, the backstop the registry expires a
// cached row by. It is not the window a session's state is measured in (LiveWindow).
func (r *SessionRegistry) TTL() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ttl
}

// SetTTL moves the bucket's maximum age the registry expires its rows by, for a caller that has
// just read one from the bucket itself. Every cached row's deadline moves with it, by the same
// amount, so each still stands one TTL from the moment its record was written: the registry keeps
// no other record of that moment, and decoding every row again to find it would cost a read of
// the whole bucket. A row whose new deadline has already passed is not removed here; it is
// scheduled like any other, and the expiry timer takes it at once through the one path that
// removes an expired row (expireDue), which keeps its last heartbeat for the retention and its
// revision in the fence. While the copy does not follow its bucket that timer expires nothing, so
// such a row waits for the read that restores following, as every other expiry does.
//
// A non-positive TTL turns local expiry off: the rows lose their deadlines and stay until a
// record or a completed read removes them.
func (r *SessionRegistry) SetTTL(ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.ttl
	if ttl == was {
		return
	}
	r.ttl = ttl
	for id, cs := range r.cache.All() {
		switch {
		case ttl <= 0:
			cs.expiresAt = time.Time{}
		case cs.expiresAt.IsZero():
			cs.expiresAt = r.expiryFor(time.Time{}, cs.entry.UpdatedAt)
		default:
			cs.expiresAt = cs.expiresAt.Add(ttl - was)
		}
		if !cs.expiresAt.IsZero() {
			r.setExpiryLocked(id, expireLive, cs.expiresAt)
		}
	}
	r.armExpiryLocked()
}

// windows is the live window and the gone horizon this registry judges a record's age against:
// the options' values, and the package's own for a registry built from a struct literal, which
// only a test does. Both are fixed when the registry is opened, so no lock covers them.
func (r *SessionRegistry) windows() (live, horizon time.Duration) {
	return cmp.Or(r.liveWindow, LiveWindow), cmp.Or(r.goneHorizon, GoneHorizon)
}

// stateOf is entry's state on this registry's windows (StateOf).
func (r *SessionRegistry) stateOf(entry SessionEntry, now time.Time) State {
	live, horizon := r.windows()
	return StateOf(entry, now, live, horizon)
}

// retention is how long after a session leaves the cache its lastSeen record is kept:
// WithLastSeenRetention's value, and never less than one TTL, so a registry built from a struct
// literal keeps it one TTL. It means nothing when the TTL is not positive: then nothing expires
// and no record is kept. Callers hold r.mu.
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
	return r.cache.Len()
}

// LiveCount returns the number of live sessions List would answer, counted under the shared lock
// with no allocation: the rows whose last registration is within the live window. It counts no
// unresponsive row, whatever the bucket's maximum age is — with a maximum age far longer than the
// live window, the rows that have not expired are every row the bucket holds, and counting those
// would report a box full of stalled sessions as a box full of working ones.
func (r *SessionRegistry) LiveCount() int {
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	live := 0
	for _, cs := range r.cache.All() {
		if r.stateOf(cs.entry, now) == Live {
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
// against another listener's write it is last-writer-wins, as every Put already is. The write
// reaches the cache through the handle's epoch (kvwatch.Copy.Put), so a write made through a
// handle on a bucket since recreated is dropped rather than fencing out the new bucket's rows.
func (r *SessionRegistry) PutWithin(ctx context.Context, sessionID string, entry SessionEntry) error {
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
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.Put(sessionID, buf) })
	if !answered {
		return fmt.Errorf("write session %s to the bucket: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cache.Put(r.watcher, epoch, sessionID, &cachedSession{entry: entry, expiresAt: r.expiryFor(time.Time{}, entry.UpdatedAt)}, revision)
	r.mu.Unlock()
	return nil
}

// Get returns sessionID's live entry from the cache, or nats.ErrKeyNotFound when the cache holds
// none or holds one past its deadline, even while the copy retains that row for List because it
// does not follow its bucket (expireDue): to one session's caller an expired row is a session
// gone. With the bucket's maximum age past the gone horizon, which is what the reaper deletes a
// record at, that is a gone session and nothing else; a caller that must tell an unresponsive
// session from a gone one asks Lookup, which judges the record rather than its deadline. It takes
// only the shared lock and changes nothing: the expiry timer, not a read, removes an expired
// entry.
func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
	now := r.now()
	r.mu.RLock()
	cs, ok := r.cache.Get(sessionID)
	r.mu.RUnlock()
	if !ok || cs.expired(now) {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	return cs.entry, nil
}

// Lookup answers sessionID's record and the state that record puts its session in (StateOf):
// live within the registry's live window of the last registration, unresponsive past it and
// within the gone horizon, and gone past that or with no record here at all. An unresponsive
// session is not a dead one — its claims stand, its messages are stored and no replacement is
// started for it — so every caller that would take something away from a session, or tell a
// sender its destination has ended, asks here rather than reading a not-found from Get as death.
//
// The state is the record's age alone, never its membership of this cache: a row the copy kept
// through a re-read is judged again, and a row past the bucket's backstop deadline, which Get
// reports absent, still reads unresponsive here while its registration is within the horizon. The
// entry is the record the cache holds, so a caller can report the last registration (UpdatedAt) of
// a gone session whose row is still here; a session with no row at all answers the zero entry, and
// LastSeen is what still knows when it was last seen. It takes only the shared lock and changes
// nothing.
func (r *SessionRegistry) Lookup(sessionID string) (SessionEntry, State) {
	now := r.now()
	r.mu.RLock()
	cs, ok := r.cache.Get(sessionID)
	r.mu.RUnlock()
	if !ok {
		return SessionEntry{}, Gone
	}
	return cs.entry, r.stateOf(cs.entry, now)
}

// Refresh reads sessionID's entry from the session bucket itself, applies it to the cache as a
// delivered record is applied, and then answers as Get does. The cache follows the bucket
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
//
// A read the cache does not take is answered as read, uncached, as store.Registry.Get answers its
// own: one made before a watch moved the epoch (a probe or a Rewatch installed a recreated stream),
// or one the revision fence drops because the cache holds a higher revision of the key, which may
// belong to a bucket incarnation the watcher has not yet found replaced. The bucket answered for
// the session a moment ago, and answering from a cache that did not take that answer would tell
// the caller a live session is gone. It answers as Get would for the same row: a delete or purge
// marker, a value that does not decode and a row past its TTL are not found.
func (r *SessionRegistry) Refresh(ctx context.Context, sessionID string) (SessionEntry, error) {
	kv, epoch := r.watcher.Handle()
	got, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if !answered {
		return SessionEntry{}, fmt.Errorf("read session %s from the bucket: %w", sessionID, err)
	}
	if errors.Is(err, bus.ErrRefused) {
		return SessionEntry{}, fmt.Errorf("%w: %w", nats.ErrKeyNotFound, err)
	}
	if err != nil {
		return SessionEntry{}, err
	}
	entry := kvwatch.LegacyEntry(got)
	now := r.now()
	r.mu.Lock()
	outcome, malformed := r.cache.Apply(r.watcher, epoch, entry)
	if outcome != kvwatch.Applied {
		answer, err := r.uncachedLocked(entry, now)
		r.mu.Unlock()
		return answer, err
	}
	if malformed != nil {
		r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key), slog.Uint64("revision", entry.Revision), slog.String("error", malformed.Error()))
	}
	r.mu.Unlock()
	return r.Get(sessionID)
}

// uncachedLocked answers entry, a read of the bucket the cache did not take (Refresh), as Get
// would answer the row applying it leaves: a delete or purge marker, and a value that does not
// decode, which applying evicts, are not found, and so is a row past its TTL, measured as the
// cache measures one (decodeLocked). Callers hold r.mu.
func (r *SessionRegistry) uncachedLocked(entry kvwatch.Entry, now time.Time) (SessionEntry, error) {
	if entry.Op != kvwatch.OpPut {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	cs, _, err := r.decodeLocked(entry)
	if err != nil || cs.expired(now) {
		return SessionEntry{}, nats.ErrKeyNotFound
	}
	return cs.entry, nil
}

// LastSeen returns a recently expired or explicitly removed session's final
// heartbeat time. It is retained for the registry's retention (one TTL unless
// WithLastSeenRetention set longer) after the session left the cache, so callers
// can report why an otherwise durable reference stopped resolving. A live
// session answers 0. It takes only the shared lock and changes nothing: an
// expired entry the expiry timer has not removed yet, which is also a row the copy retains while
// it does not follow its bucket, answers what its removal will record (its UpdatedAt until the
// retention past the moment it left the live view, departedLocked), so the answer is the same
// before and after the removal.
func (r *SessionRegistry) LastSeen(sessionID string) int64 {
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if cs, ok := r.cache.Get(sessionID); ok {
		if !cs.expired(now) {
			return 0
		}
		// A row past its deadline that the copy retains because it does not follow its bucket
		// answers its last heartbeat for as long as it is retained: no removal has recorded one,
		// and the read that restores following is what decides whether the session is gone.
		if !r.following() {
			return cs.entry.UpdatedAt
		}
		if seen, ok := r.lastSeenRecord(cs.entry.UpdatedAt, r.departedLocked(cs, now)); ok && !seen.expired(now) {
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
	// The fence read before the delete is the bucket's this handle reaches only when the read under
	// way began in its epoch (kvwatch.Copy.FenceFor).
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	revision := r.cache.FenceFor(epoch, sessionID)
	r.mu.RUnlock()
	read, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.DeleteAtRead(sessionID) })
	if !answered {
		return fmt.Errorf("delete session %s from the bucket: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	revision = max(revision, read)
	// A history that does not answer falls back to the fence below: the delete itself answered.
	// ReadHistory ends with ctx; bus.RequestTimeout bounds it for a caller that answers no request
	// and so passes a context that never ends.
	historyCtx, cancel := context.WithTimeout(ctx, bus.RequestTimeout)
	entries, err := kv.ReadHistory(historyCtx, sessionID)
	cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil || len(entries) == 0 {
		r.cache.Evict(r.watcher, epoch, sessionID, revision+1)
		return nil
	}
	latest := kvwatch.JetStreamEntry(entries[len(entries)-1])
	if _, malformed := r.cache.Apply(r.watcher, epoch, latest); malformed != nil {
		r.logger().Warn("session registry cache evicted malformed value",
			slog.String("key", latest.Key),
			slog.Uint64("revision", latest.Revision),
			slog.String("error", malformed.Error()),
		)
	}
	return nil
}

// ListEntry is a single entry from List(), keyed by session ID, with the state its record puts
// its session in. The row's last registration is its SessionEntry.UpdatedAt, which the listener's
// routes answer as last_seen.
type ListEntry struct {
	SessionID string `json:"session_id"`
	SessionEntry
	State State `json:"state"`
}

// ListOption selects which states List answers for. It is an option rather than an argument so
// that List() stays the live-only listing every caller already writes, and only the caller that
// wants more names what it wants.
type ListOption func(*listOpts)

type listOpts struct{ unresponsive bool }

// IncludeUnresponsive adds the unresponsive rows to the live ones List answers with, for a caller
// that must show, or route to, a session that has stopped answering but is not gone.
func IncludeUnresponsive() ListOption {
	return func(o *listOpts) { o.unresponsive = true }
}

// List returns the sessions the in-memory cache holds that are live, each with its state and its
// last registration, and with IncludeUnresponsive the unresponsive ones as well. A row is judged
// by its own last registration (StateOf), never by its presence here: a row the copy kept through
// a re-read is judged again, so a listener refilling its cache no longer leaves a working session
// out, and a stale copy no longer holds a session live. A gone row is never listed; the expiry
// timer and the session reaper are what remove it.
//
// It takes only the shared lock and changes nothing. Cache-only: no JetStream round-trips, so it
// can't hang on a slow KV leader the way the old Keys()+per-key Get() loop did. It never refuses:
// whether the copy can be served at all is Servable's to say.
func (r *SessionRegistry) List(include ...ListOption) ([]ListEntry, error) {
	var opts listOpts
	for _, o := range include {
		o(&opts)
	}
	now := r.now()
	r.mu.RLock()
	entries := make([]ListEntry, 0, r.cache.Len())
	for key, cs := range r.cache.All() {
		state := r.stateOf(cs.entry, now)
		if state == Gone || (state == Unresponsive && !opts.unresponsive) {
			continue
		}
		entries = append(entries, ListEntry{SessionID: key, SessionEntry: cs.entry, State: state})
	}
	r.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}

// storedLocked and removedLocked are what the cache records beside its copy when the copy stores
// or removes a row (kvwatch.Rows), and where each row's expiry is scheduled. A stored row's
// session is live, so its last-seen record goes and its own deadline is scheduled. A removed row's
// last heartbeat is kept for LastSeen, for the retention from the moment it left the live view
// (departedLocked), which is what its expiry would have recorded, so LastSeen answers the same
// before and after the removal. Neither walks the whole lastSeen map: a read applies every record
// of the bucket through them, and each record's own deadline is in the expiry heap, which removes
// the record for good (expireDue). Callers hold r.mu for writing.
func (r *SessionRegistry) storedLocked(sessionID string, stored *cachedSession) {
	delete(r.lastSeen, sessionID)
	r.scheduleLocked(sessionID, expireLive, stored.expiresAt)
}

func (r *SessionRegistry) removedLocked(sessionID string, removed *cachedSession) {
	left := r.departedLocked(removed, r.now())
	r.retainLastSeenLocked(sessionID, removed.entry.UpdatedAt, left)
	r.scheduleLocked(sessionID, expireLastSeen, left.Add(r.retention()))
}

// departedLocked is the moment row left, or leaves, the live view, from which its last heartbeat
// is kept for the retention: its own deadline when it is past it, and now when it is not, for a
// row the bucket no longer holds. A deadline that passed while the copy did not follow its bucket
// is no such moment: the copy could not see the heartbeats that renewed the row either, so it
// expired nothing (expireDue), and the row leaves the live view no earlier than the read that
// restored following (followingSince). Callers hold r.mu.
func (r *SessionRegistry) departedLocked(row *cachedSession, now time.Time) time.Time {
	if !row.expired(now) {
		return now
	}
	if row.expiresAt.Before(r.followingSince) {
		return r.followingSince
	}
	return row.expiresAt
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
