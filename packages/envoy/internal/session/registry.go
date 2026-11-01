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
	// ConsumerStart is the stream sequence the session's consumer starts at, as the registration
	// that wrote this entry read it (bus.SessionConsumers.Ensure). Every stored message at or
	// after it is the consumer's to deliver; one below it is in no consumer, so the durable path
	// still delivers it (ConsumerHoldsStored). A restart of the consumer only ever moves its start
	// forward, to its ack floor, past messages it has acked, so a start recorded before that
	// restart still names a sequence the consumer covers from. Zero for a session on the core
	// path, and for one whose entry an earlier release wrote, which the durable path reads as
	// today: the consumer holds every stored message of it.
	ConsumerStart uint64 `json:"consumer_start,omitempty"`
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

// DeliveryConsumer is SessionEntry.Delivery for a session whose stored messages its session consumer
// delivers.
const DeliveryConsumer = "consumer"

// ConsumerHoldsStored reports whether the session's own consumer holds the stored message of
// stream sequence storedSeq, so no delivery path owes it a push: a session on consumer delivery is
// held every message at or after the sequence its consumer starts at (ConsumerStart). A durable
// message below that start is in no consumer — a push NAKed, or a message deferred, before the
// consumer was created, and redelivered after it — and the durable path delivers it as it would
// for a session with no consumer at all. storedSeq 0 names a message that is not the durable's
// copy of a stored one (a core or live frame), whose stored copy the consumer holds whatever its
// sequence.
func (e SessionEntry) ConsumerHoldsStored(storedSeq uint64) bool {
	return e.Delivery == DeliveryConsumer && (storedSeq == 0 || storedSeq >= e.ConsumerStart)
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
	maxAge            time.Duration
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

// WithSessionMaxAge sets the maximum age the registry writes on the session bucket, at open and on
// every pass of its reaper (bus.SetKeyValueMaxAge, ParseSessionMaxAge). The default, and any
// non-positive value, writes none and leaves the bucket's own age exactly as it is. It is not the
// TTL the registry expires a cached row by: that is always the age the bucket really carries,
// read back after this one is written (SetTTL).
func WithSessionMaxAge(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.maxAge = d }
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
	// js is the JetStream context the bucket was opened through, which the maximum age is written
	// through (applyMaxAge), and maxAge is WithSessionMaxAge's value, zero when the deployment
	// sets none and the bucket's own age stands. Both are fixed when the registry is opened, and
	// a registry built from a struct literal, which only a test does, has neither and writes no
	// age.
	js     nats.JetStreamContext
	maxAge time.Duration

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

	// changeMu guards onChange, the functions OnChange registered, by registration number.
	changeMu   sync.Mutex
	onChange   map[uint64]func(sessionID string)
	nextChange uint64

	// changed is every session the work under r.mu changed, which unlockAndNotify reports once
	// that lock is released. Guarded by r.mu.
	changed []string
}

// OnChange registers changed to be called with a session's id after each change to the session's
// cached entry: a write (Put, Refresh, or the watcher delivering another listener's write), a delete
// or purge, an eviction of a malformed value, the cache's reset for a recreated bucket (each id it
// held), and a lapse. A lapse has no watch event (JetStream MaxAge expiry is not delivered, see
// cachedSession), so the registry finds it from the entry's own deadline: the expiry timer removes
// it then (expireDue), and PruneLapsed runs that sweep at once for a caller that must not wait for
// the timer.
//
// changed runs outside the registry's lock, on the goroutine that made the change: the watcher's,
// the expiry timer's, or a caller's of Put, Delete, Refresh or PruneLapsed. Calls can therefore run
// concurrently and out of revision order, so changed reads the session's current state (Get) rather
// than trusting the order of its calls, and it must not block. stop unregisters it.
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

// PruneLapsed drops every cached entry whose deadline has passed, calling OnChange's functions for
// each, instead of leaving each one to the expiry timer that holds its deadline (expireDue): a
// caller that must see a lapse promptly calls it on its own ticker. It removes an entry exactly as
// that timer would, through the copy's own removal (kvwatch.Copy.Remove), so the timer's item for
// it finds the entry gone and does nothing, and a deadline the heap never held (an entry a test
// placed in the cache itself) is dropped here too. While the copy does not follow its bucket it
// drops nothing, as that timer pops nothing: the copy could not see the heartbeats that renewed
// those rows either, so a deadline that passed there is no lapse. List answers the entries that
// remain, the sessions that are live.
func (r *SessionRegistry) PruneLapsed() {
	r.mu.Lock()
	defer r.unlockAndNotify()
	if !r.following() {
		return
	}
	now := r.now()
	for sessionID, cs := range r.cache.All() {
		if !cs.expired(now) {
			continue
		}
		r.scheduleLocked(sessionID, expireLastSeen, r.retireLocked(sessionID, cs, now))
		r.cache.Remove(sessionID)
		r.changedLocked(sessionID)
	}
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

// changedLocked records sessionID as changed by the work under way, for unlockAndNotify to report
// once the lock is released. Callers hold r.mu for writing.
func (r *SessionRegistry) changedLocked(sessionID string) {
	r.changed = append(r.changed, sessionID)
}

// unlockAndNotify releases r.mu and calls OnChange's functions for every session the work under
// that lock changed: each row the copy stored or removed for the bucket's sake (storedLocked,
// removedLocked) and each the cache removed itself (expireDue, PruneLapsed). Every write site
// defers it in place of r.mu.Unlock, so no path changes the copy without reporting it. Callers
// hold r.mu for writing.
func (r *SessionRegistry) unlockAndNotify() {
	changed := r.changed
	r.changed = nil
	r.mu.Unlock()
	r.notifyChanged(changed...)
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
	// The deployment's maximum age is written before the bucket's own is read, so the TTL below is
	// the age the bucket carries from here on. A bucket created by an older build at the 5-minute
	// default is raised here, and a record then stands past the gone horizon instead of being
	// dropped while its session is only unresponsive.
	if _, err := bus.SetKeyValueMaxAge(js, bus.SessionBucket, opts.maxAge); err != nil {
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
		js:                js,
		maxAge:            opts.maxAge,
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
	defer r.unlockAndNotify()
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
	defer r.unlockAndNotify()
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
// against their age again from now (departedLocked). A recreated bucket's first read is where the
// rows of the bucket that went are reported gone, since the copy keeps them until the sweep.
func (c sessionCache) CompleteRead(kvwatch.Read) {
	r := c.r
	r.mu.Lock()
	defer r.unlockAndNotify()
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

// Windows is the live window and the gone horizon this registry judges a record's age against,
// for a caller that must measure a session's age by the same windows: the listener's role-claim
// reaper gives a restored claim's holder one live window to register again, since that is how
// long a registration keeps a session live here.
func (r *SessionRegistry) Windows() (live, horizon time.Duration) {
	return r.windows()
}

// EntryState is entry's state on this registry's windows, as of now, for a caller holding a
// record this registry's cache did not answer for: a read of the bucket the copy did not take
// (Refresh). A caller naming a session asks Lookup, which reads the record and judges it
// together.
func (r *SessionRegistry) EntryState(entry SessionEntry) State {
	return r.stateOf(entry, r.now())
}

// applyMaxAge writes the deployment's maximum age on the bucket when it has one (WithSessionMaxAge)
// and then adopts the age the bucket really carries, so a raise or a rollback another process made
// reaches this registry within one call and with no restart (SetTTL). The write is skipped on a
// registry built from a struct literal, which has no JetStream context, and on one whose
// deployment sets no age, which leaves the bucket's own standing.
//
// A write answers with the age the bucket carries once it has run (bus.SetKeyValueMaxAge), so
// only the registry that writes none reads the bucket's status for it: one stream read a pass,
// not two.
//
// It adopts only a positive age, as the open-time read does: a status read that reports none is
// as likely a bucket this build did not create as one that truly keeps its records for good, and
// turning local expiry off on that word would keep every row of a recreated bucket for the life
// of the process.
func (r *SessionRegistry) applyMaxAge() error {
	if r.js != nil && r.maxAge > 0 {
		written, err := bus.SetKeyValueMaxAge(r.js, bus.SessionBucket, r.maxAge)
		if err != nil {
			return err
		}
		r.adoptMaxAge(written)
		return nil
	}
	kv, _ := r.watcher.Handle()
	status, err := kv.Status()
	if err != nil {
		return fmt.Errorf("read the %s bucket's maximum age: %w", bus.SessionBucket, err)
	}
	r.adoptMaxAge(status.TTL())
	return nil
}

// adoptMaxAge expires cached rows by age, the bucket's own backstop, when that age is positive
// and is not the one this registry already runs with.
func (r *SessionRegistry) adoptMaxAge(age time.Duration) {
	if age > 0 && age != r.TTL() {
		r.SetTTL(age)
	}
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
	r.unlockAndNotify()
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
		r.unlockAndNotify()
		return answer, err
	}
	if malformed != nil {
		r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key), slog.Uint64("revision", entry.Revision), slog.String("error", malformed.Error()))
	}
	r.unlockAndNotify()
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
	r.reconcileDeleted(ctx, kv, epoch, sessionID, max(revision, read))
	return nil
}

// DeleteOutcome is what DeleteIfInstance did with the entry it was asked to remove, and what
// DELETE /v1/sessions/<session_id> words for the client that named its instance. The two outcomes
// that keep the entry are told apart because they are separate facts: one says another client
// instance's registration is stored under that session id, the other that the stored value kept
// changing under the conditional delete, which says nothing about whose registration is stored
// now. One "kept" answer for both would make the route's reason false for whichever case it did
// not describe.
type DeleteOutcome uint8

const (
	// EntryUnanswered goes with an error: a bucket call did not answer, so nothing is known
	// about the entry. It is the zero value, so a caller that reads the outcome without the
	// error never reads a delete that did not happen as one that did.
	EntryUnanswered DeleteOutcome = iota
	// EntryDeleted: the entry is gone, which a key that was already absent counts as, since
	// Delete is idempotent.
	EntryDeleted
	// EntryKeptForAnotherInstance: the stored entry carries another client instance's id, so
	// it is another process's registration and this caller does not remove it.
	EntryKeptForAnotherInstance
	// EntryKeptAfterLostRace: the entry matched, and both conditional deletes lost to a write
	// that landed between the read and the delete — most often the deleting instance's own
	// heartbeat. The entry stayed, and whose registration it holds now is exactly what this
	// call did not learn.
	EntryKeptAfterLostRace
)

// String names the outcome, for the log line or test failure that reports one.
func (o DeleteOutcome) String() string {
	switch o {
	case EntryDeleted:
		return "deleted"
	case EntryKeptForAnotherInstance:
		return "kept for another client instance"
	case EntryKeptAfterLostRace:
		return "kept after a lost compare-and-swap"
	default:
		return "unanswered"
	}
}

// DeleteIfInstance deletes sessionID's entry only while that entry carries instanceID or carries
// no client instance id at all (InstanceMatches), and reports what it did, each bucket call
// waiting no longer than ctx (bus.Bounded). It is the deregistration of a caller that names its
// own client instance: a Claude Code /clear handoff deletes the session it is handing off from
// before it claims that session's role, and must not delete the live entry another process wrote
// after it resumed the same session id. A caller that names no instance id compares nothing and
// calls Delete instead.
//
// The comparison holds at the revision it deletes: the entry is read for its value and its
// revision together, and the delete names that revision (nats.LastRevision), so a registration
// that landed in between is kept rather than deleted on the strength of a value that is no longer
// stored. JetStream refuses that delete with wrong last sequence (10071, which nats.go names
// nats.ErrKeyExists), and the whole sequence runs once more: the common cause is the deleting
// instance's own heartbeat landing between the read and the delete, and answering "kept" for it
// would leave the predecessor's entry alive and turn the role claim that follows into a 409 for
// no reason. A second lost compare-and-swap answers EntryKeptAfterLostRace and no error, the
// outcome that says the entry stayed without claiming another instance owns it.
//
// EntryDeleted and no error means the entry is gone; either kept outcome and no error means the
// entry stayed, and the caller keeps whatever it names; an error means the bucket did not answer,
// ctx's end among them, and the outcome is EntryUnanswered. The cache follows the bucket
// afterwards exactly as it does after Delete. A delete ctx abandoned may still land after this
// answer, and that is safe: it deletes only the revision it compared.
func (r *SessionRegistry) DeleteIfInstance(ctx context.Context, sessionID, instanceID string) (DeleteOutcome, error) {
	// The handle and the fence are read together before the bucket calls, as Delete reads them:
	// the fence is the bucket's this handle reaches only when the read under way began in its
	// epoch (kvwatch.Copy.FenceFor).
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	fence := r.cache.FenceFor(epoch, sessionID)
	r.mu.RUnlock()
	for attempt := 0; ; attempt++ {
		entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
		if !answered {
			return EntryUnanswered, fmt.Errorf("read session %s from the bucket: %w", sessionID, err)
		}
		if errors.Is(err, nats.ErrKeyNotFound) {
			return EntryDeleted, nil
		}
		if err != nil {
			return EntryUnanswered, err
		}
		revision := entry.Revision()
		var stored SessionEntry
		if err := json.Unmarshal(entry.Value(), &stored); err != nil {
			// A value this build cannot read names no instance, so it belongs to none: it is
			// deleted as an entry that carries no id is, and the deregistration is not blocked
			// by a record no reader can act on. What json.Unmarshal decoded before the error
			// is dropped for that reason: on a type error it still fills every field it could
			// read, wherever the bad field sits, so a value carrying a client instance id would
			// otherwise be compared against that id and kept — the opposite of what this branch
			// decides.
			stored = SessionEntry{}
			r.logger().Warn("session registry deleting a malformed value for a named client instance",
				slog.String("key", entry.Key()),
				slog.Uint64("revision", revision),
				slog.String("error", err.Error()),
			)
		}
		if !InstanceMatches(stored.ClientInstanceID, instanceID) {
			return EntryKeptForAnotherInstance, nil
		}
		_, answered, err = bus.Bounded(ctx, func() (struct{}, error) {
			return struct{}{}, kv.Delete(sessionID, nats.LastRevision(revision))
		})
		if !answered {
			return EntryUnanswered, fmt.Errorf("delete session %s from the bucket at revision %d: %w", sessionID, revision, err)
		}
		if errors.Is(err, nats.ErrKeyExists) {
			if attempt == 0 {
				continue
			}
			return EntryKeptAfterLostRace, nil
		}
		if err != nil {
			return EntryUnanswered, err
		}
		r.reconcileDeleted(ctx, kv, epoch, sessionID, max(fence, revision))
		return EntryDeleted, nil
	}
}

// reconcileDeleted brings the cache up to what the bucket holds for sessionID after a delete this
// registry made: the key's history has the last word, so a registration that landed since the
// delete is cached rather than dropped, and a value this build cannot read is evicted and logged.
// A history that does not answer leaves the fence revision+1 behind instead, above the revision
// the delete acted on, so the watcher's own in-flight update for an older revision cannot restore
// the entry. revision is the newest revision the caller knows of for the key, and epoch the one
// the kv handle was read in: a result through a handle of an earlier epoch reaches the copy no
// more here than anywhere else (kvwatch.Copy). It calls OnChange's functions for sessionID once it
// has released r.mu, when it changed the cache.
func (r *SessionRegistry) reconcileDeleted(ctx context.Context, kv bus.KeyValue, epoch uint64, sessionID string, revision uint64) {
	// A history that does not answer falls back to the fence below: the delete itself answered.
	// ReadHistory ends with ctx; bus.RequestTimeout bounds it for a caller that answers no request
	// and so passes a context that never ends.
	historyCtx, cancel := context.WithTimeout(ctx, bus.RequestTimeout)
	entries, err := kv.ReadHistory(historyCtx, sessionID)
	cancel()
	r.mu.Lock()
	defer r.unlockAndNotify()
	if err != nil || len(entries) == 0 {
		r.cache.Evict(r.watcher, epoch, sessionID, revision+1)
		return
	}
	latest := kvwatch.JetStreamEntry(entries[len(entries)-1])
	if _, malformed := r.cache.Apply(r.watcher, epoch, latest); malformed != nil {
		r.logger().Warn("session registry cache evicted malformed value",
			slog.String("key", latest.Key),
			slog.Uint64("revision", latest.Revision),
			slog.String("error", malformed.Error()),
		)
	}
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
// the record for good (expireDue). Each records its session as changed, for OnChange's functions
// once the caller has released r.mu (unlockAndNotify). Callers hold r.mu for writing.
func (r *SessionRegistry) storedLocked(sessionID string, stored *cachedSession) {
	delete(r.lastSeen, sessionID)
	r.scheduleLocked(sessionID, expireLive, stored.expiresAt)
	r.changedLocked(sessionID)
}

func (r *SessionRegistry) removedLocked(sessionID string, removed *cachedSession) {
	r.scheduleLocked(sessionID, expireLastSeen, r.retireLocked(sessionID, removed, r.now()))
	r.changedLocked(sessionID)
}

// retireLocked keeps row's final heartbeat for the retention after the moment that row left the
// live view (departedLocked), and returns the deadline that record expires at, which its caller
// arms the way its own path arms deadlines: a removal schedules it (scheduleLocked), while the
// expiry sweep sets it and re-arms once at the end (setExpiryLocked). Callers hold r.mu for
// writing.
func (r *SessionRegistry) retireLocked(sessionID string, row *cachedSession, now time.Time) time.Time {
	left := r.departedLocked(row, now)
	r.retainLastSeenLocked(sessionID, row.entry.UpdatedAt, left)
	return left.Add(r.retention())
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
