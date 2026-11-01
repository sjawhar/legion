package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/store"
)

// writeAttempts bounds the compare-and-set attempts of one registration write (SessionRegistry.write).
const writeAttempts = 5

// ErrWriteConflict is a registration write whose compare-and-set lost to another writer on each of
// its writeAttempts: every time it wrote, the stored revision had moved since it read. Nothing was
// written; the caller may retry, and the listener answers it as a 503 with Retry-After.
var ErrWriteConflict = errors.New("the session registration changed under every write attempt")

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
	// AcceptedHost is the host name (os.Hostname, the pod's name in Kubernetes) of the listener pod
	// that accepted the session's latest port registration: the subscribe handler stamps every
	// registration with a port, so a heartbeat accepted by another pod of the machine re-stamps it
	// with that pod's name, the pod that last heard from the session. Deliverer refuses a delivery
	// whose address is local to the delivering pod (a loopback bridge, or host.docker.internal)
	// when AcceptedHost names another pod, since that pod's loopback is not this one's. Empty for a
	// registration without a port, or one written by a build before this field, which is delivered
	// as before.
	AcceptedHost string `json:"accepted_host,omitempty"`

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
	// for a client that predates the field. It is the client's own id, not the listener pod's,
	// and every pod of the machine reads it from this one bucket, so a claim is weighed alike
	// whichever pod serves the registration or the claim.
	ClientInstanceID string `json:"client_instance_id,omitempty"`

	// unknown holds the JSON of every key a stored registration carries that SessionEntry does not
	// name: a field a newer build writes. encodeSessionEntry writes it back beside the known fields,
	// so a heartbeat through this build keeps it while pods of two builds take turns writing one
	// session during a roll. It is never modified once decoded, so copies of an entry may share it.
	unknown map[string]json.RawMessage
}

// sessionEntryKeys are SessionEntry's JSON keys; any other key of a stored registration is kept in
// SessionEntry.unknown.
var sessionEntryKeys = store.JSONKeys(reflect.TypeFor[SessionEntry]())

// decodeSessionEntry reads a stored registration: the fields SessionEntry names, and every other key
// into its unknown map.
func decodeSessionEntry(data []byte) (SessionEntry, error) {
	var entry SessionEntry
	unknown, err := store.DecodeKeeping(data, &entry, sessionEntryKeys)
	if err != nil {
		return SessionEntry{}, err
	}
	entry.unknown = unknown
	return entry, nil
}

// encodeSessionEntry writes entry for the bucket: its known fields, and the unknown keys it was
// decoded with.
func encodeSessionEntry(entry SessionEntry) ([]byte, error) {
	return store.EncodeKeeping(entry, entry.unknown)
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
	lastSeenRetention time.Duration
	log               *slog.Logger
	seen              bus.KeyValue
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

// WithLastSeenBucket sets the listener state bucket (bus.ListenerStateBucket), where every
// registration write also records the session's last-seen time under `seen.<session token>`
// (lastSeenKey), the one record every listener pod answers LastSeen for a lapsed session from, a
// pod that started after the session lapsed included. Without it the registry keeps last-seen
// times in process (SessionRegistry.seenLocal): a listener whose NATS user lacks the grant for
// that bucket runs so, and a restart forgets them.
func WithLastSeenBucket(kv bus.KeyValue) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.seen = kv }
}

// WithLastSeenRetention sets how long after a session leaves the cache LastSeen still answers its
// final heartbeat; the id's revision fence is kept as long. The default, and any value shorter
// than the bucket's TTL, is one TTL: the fence must outlive the id's leaving by at least one TTL
// for dropping it to stay safe (expireDue). The listener keeps it for as long as the interest
// reaper can still hold a dead session's interest, so its interests API can report the session's
// last heartbeat for every interest row it still lists. With the listener state bucket
// (WithLastSeenBucket) every pod answers out of the session's `seen.` record for this whole
// window, and the bucket keeps a record longer than the listener ever sets this retention
// (listenerStateMaxAge, cmd/listener/lease.go).
func WithLastSeenRetention(d time.Duration) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.lastSeenRetention = d }
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
	// seenLocal is what LastSeen answers a registry with no last-seen bucket from, and is written
	// only by such a registry (cacheSessionLocked): the UpdatedAt of each session's latest
	// registration this registry wrote or its watcher delivered, until the retention past that
	// registration's expiry. It is guarded by mu.
	seenLocal map[string]int64

	// seen is the listener state bucket LastSeen reads a lapsed session's `seen.` record from, the
	// one answer every pod shares (WithLastSeenBucket); its KeyValue is nil when the registry has
	// none.
	seen bus.KeyValue

	// expiries holds the deadline of each cached entry and of each last-seen record and revision
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

	// changeMu guards onChange, the functions OnChange registered, by registration number.
	changeMu   sync.Mutex
	onChange   map[uint64]func(sessionID string)
	nextChange uint64
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
// that timer would, so the timer's item for it finds the entry gone and does nothing, and a
// deadline the heap never held (an entry a test placed in the cache itself) is dropped here too.
// List answers the entries that remain, the sessions that are live.
func (r *SessionRegistry) PruneLapsed() {
	now := time.Now()
	var lapsed []string
	r.mu.Lock()
	for sessionID, cs := range r.cache {
		if !cs.expired(now) {
			continue
		}
		delete(r.cache, sessionID)
		r.scheduleLocked(sessionID, expireLastSeen, r.lastSeenUntilLocked(sessionID, cs.expiresAt))
		lapsed = append(lapsed, sessionID)
	}
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
		seenLocal:         map[string]int64{},
		seen:              opts.seen,
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
// an entry with its deadline, and a last-seen item still removes the per-process record, which
// resetCache keeps, and drops a fence only for an id the cache does not hold.
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

// applyWatched applies one entry the KV watcher delivered to the cache. A revision the cache
// already holds, such as the echo of the registry's own Put, is cached again but reported only when
// it adds or drops the session's entry.
func (r *SessionRegistry) applyWatched(entry nats.KeyValueEntry) {
	if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
		r.mu.Lock()
		cachedRevision := r.cacheRevisions[entry.Key()]
		_, wasCached := r.cache[entry.Key()]
		applied := r.evictCachedSessionLocked(entry.Key(), entry.Revision())
		_, isCached := r.cache[entry.Key()]
		r.mu.Unlock()
		if applied && (entry.Revision() > cachedRevision || isCached != wasCached) {
			r.notifyChanged(entry.Key())
		}
		return
	}
	_, _ = r.applyValue(entry)
}

// applyValue applies a stored value of entry to the cache, calls OnChange's functions when that
// changed it, and returns the value decoded: one that does not decode is evicted with a WARN, and
// its error returned. A revision the cache already holds, such as the echo of the registry's own
// Put, is cached again but reported only when it adds or drops the session's entry.
func (r *SessionRegistry) applyValue(entry nats.KeyValueEntry) (SessionEntry, error) {
	r.mu.Lock()
	cachedRevision := r.cacheRevisions[entry.Key()]
	_, wasCached := r.cache[entry.Key()]
	var applied bool
	item, err := decodeSessionEntry(entry.Value())
	if err != nil {
		applied = r.evictCachedSessionLocked(entry.Key(), entry.Revision())
		r.logger().Warn("session registry watcher evicted malformed value", slog.String("key", entry.Key()), slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
	} else {
		applied = r.cacheSessionLocked(entry.Key(), item, r.expiryFor(entry.Created(), item.UpdatedAt), entry.Revision())
	}
	_, isCached := r.cache[entry.Key()]
	r.mu.Unlock()
	if applied && (entry.Revision() > cachedRevision || isCached != wasCached) {
		r.notifyChanged(entry.Key())
	}
	if err != nil {
		return SessionEntry{}, err
	}
	return item, nil
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

// retention is how long after a session leaves the cache its last-seen record and revision fence
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

// PutWithin writes entry as sessionID's registration, each bucket call waiting no longer than ctx
// (bus.Bounded): an HTTP route passes its request's own context. Several live processes can hold the
// same session (shared on-disk state), and blind last-writer-wins routed deliveries to whichever
// heartbeated last, including processes not driving the session: mergeForClaim arbitrates between
// entry and the stored registration, and the merge is written by compare-and-set (write), so the
// registration it ran on is the one replaced.
func (r *SessionRegistry) PutWithin(ctx context.Context, sessionID string, entry SessionEntry) error {
	return r.write(ctx, sessionID, func(cur SessionEntry, curErr error, now int64) (SessionEntry, error) {
		return mergeForClaim(cur, curErr, entry, now)
	})
}

// Amend applies change to sessionID's stored registration and writes the result as PutWithin writes
// it, within ctx, so a change made on a listener whose cache trails the bucket lands on what another
// listener wrote since, not on its own stale copy. base is the registration change applies to when
// the bucket holds none (the caller's earlier read, which an expiry or a delete has overtaken), so the
// write registers the session again.
func (r *SessionRegistry) Amend(ctx context.Context, sessionID string, base SessionEntry, change func(*SessionEntry)) error {
	return r.write(ctx, sessionID, func(cur SessionEntry, curErr error, now int64) (SessionEntry, error) {
		next := base
		if curErr == nil {
			next = cur
		}
		change(&next)
		return mergeForClaim(cur, curErr, next, now)
	})
}

// write writes sessionID's registration by compare-and-set on its KV revision, so a write another
// listener made between this one's read and its write is never overwritten. merge computes the value
// to write from the stored registration (curErr nats.ErrKeyNotFound when there is none) and the time.
// The first attempt merges onto the cache. A cache miss, or a write the bucket refuses because the
// revision moved (another listener wrote, or MaxAge removed the key under a cached revision), reads
// the bucket entry and merges onto that: Update at its revision, or Create when there is none. After
// writeAttempts refused writes it returns ErrWriteConflict, having written nothing.
//
// Each bucket call waits no longer than ctx (bus.Bounded). A write ctx abandoned may still land after
// this answer. That is safe: it is a compare-and-set at the revision it read, so it replaces only that
// revision and never a write that landed since, and it writes the entry the caller's retry merges
// again.
func (r *SessionRegistry) write(ctx context.Context, sessionID string, merge func(cur SessionEntry, curErr error, now int64) (SessionEntry, error)) error {
	kv := r.watcher.KV()
	cur, revision, found := r.cachedEntry(sessionID)
	read := !found
	for range writeAttempts {
		if read {
			var err error
			if cur, revision, found, err = r.readStored(ctx, kv, sessionID); err != nil {
				return err
			}
		}
		read = true
		var curErr error
		if !found {
			curErr = nats.ErrKeyNotFound
		}
		entry, err := merge(cur, curErr, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		buf, err := encodeSessionEntry(entry)
		if err != nil {
			return err
		}
		at := revision
		written, answered, err := bus.Bounded(ctx, func() (uint64, error) {
			if at == 0 {
				return kv.Create(sessionID, buf)
			}
			return kv.Update(sessionID, buf, at)
		})
		if !answered {
			return fmt.Errorf("write session %s to the bucket: %w", sessionID, err)
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.cacheSessionLocked(sessionID, entry, r.expiryFor(time.Time{}, entry.UpdatedAt), written)
		r.mu.Unlock()
		r.recordSeen(ctx, kv, sessionID, written)
		return nil
	}
	return fmt.Errorf("%w: session %s", ErrWriteConflict, sessionID)
}

// cachedEntry is sessionID's live cached registration and the revision the cache holds for it. An
// entry past its deadline reads as absent, as every read reports it (Get), so the write reads the
// bucket and writes at the revision it finds there.
func (r *SessionRegistry) cachedEntry(sessionID string) (SessionEntry, uint64, bool) {
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	cs, ok := r.cache[sessionID]
	if !ok || cs.expired(now) {
		return SessionEntry{}, 0, false
	}
	return cs.entry, r.cacheRevisions[sessionID], true
}

// readStored reads sessionID's registration from the bucket for a write, within ctx (bus.Bounded), and
// applies it to the cache as the watcher would (applyValue: Get answers only a stored value, never a
// delete marker). revision is the one to write at, 0 when the bucket holds no value. A value that does
// not decode is found false, as the cache, which evicts it, sees no registration either, and the write
// replaces it at its revision.
func (r *SessionRegistry) readStored(ctx context.Context, kv bus.KeyValue, sessionID string) (entry SessionEntry, revision uint64, found bool, err error) {
	stored, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if !answered {
		return SessionEntry{}, 0, false, fmt.Errorf("read session %s from the bucket: %w", sessionID, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return SessionEntry{}, 0, false, nil
	}
	if err != nil {
		return SessionEntry{}, 0, false, err
	}
	entry, err = r.applyValue(stored)
	return entry, stored.Revision(), err == nil, nil
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

// LastSeen is sessionID's last registration write, in Unix milliseconds, or 0 when there is none to
// report. While the session is live it is the live registration's UpdatedAt. Once the session lapsed
// (its registration expired or was deleted) it is the time its last registration write recorded
// (recordSeen), for the registry's retention (one TTL unless WithLastSeenRetention set longer)
// after that registration's expiry, so callers can report why an otherwise durable reference
// stopped resolving.
//
// With the listener state bucket (WithLastSeenBucket) that time is the session's `seen.` record
// alone, the server's timestamp of the write, which every pod reads alike, a pod started after the
// session lapsed included: every pod of the fleet answers one time for one session through the
// whole window, and a record the bucket has not got (it was never written, its MaxAge removed it,
// or it could not be read) is no answer on any of them. The bucket keeps a record for
// listenerStateMaxAge after its last write, which is sized past this window, so the record
// outlives the answer rather than ending it. Without the bucket the time is this process's own
// record (seenLocal), the UpdatedAt of every registration this process wrote or its watcher saw,
// which a restart forgets. The window is measured on this process's clock against the server's
// timestamp; a skew of seconds against a TTL of minutes moves its end by that skew.
//
// Its read of the bucket waits no longer than ctx (bus.Bounded), so a caller answering a request
// keeps its own budget: a bucket that does not answer costs that budget once, and every further
// row of the same request reads an ended ctx and answers at once. A read ctx ended is a WARN and
// no time, as an unreadable record is.
//
// It takes only the shared lock and changes nothing: an expired entry the expiry timer has not
// removed yet answers the lapsed session's record, which is what its removal leaves in place, so
// the answer is the same before and after the removal.
func (r *SessionRegistry) LastSeen(ctx context.Context, sessionID string) int64 {
	now := time.Now()
	r.mu.RLock()
	cs, cached := r.cache[sessionID]
	at := r.seenLocal[sessionID]
	r.mu.RUnlock()
	if cached && !cs.expired(now) {
		return cs.entry.UpdatedAt
	}
	if r.ttl <= 0 {
		return 0
	}
	if r.seen.KeyValue != nil {
		stored, err := r.readSeen(ctx, sessionID)
		if err != nil {
			r.logger().Warn("session last-seen unavailable: its record in the listener state bucket could not be read",
				slog.String("session_id", sessionID), slog.String("key", lastSeenKey(sessionID)), slog.String("error", err.Error()))
		}
		at = stored
	}
	if at <= 0 || !now.Before(r.seenUntil(at)) {
		return 0
	}
	return at
}

// seenUntil is when a lapsed session's last-seen time at (Unix milliseconds) stops being reported:
// the retention after the expiry of the registration written at at.
func (r *SessionRegistry) seenUntil(at int64) time.Time {
	return time.UnixMilli(at).Add(r.ttl + r.retention())
}

// seenRecord is the value of a session's `seen.` record in the listener state bucket: at is the
// server's timestamp, in Unix milliseconds, of the session's last registration write.
type seenRecord struct {
	At int64 `json:"at"`
}

// lastSeenKey is sessionID's last-seen key in the listener state bucket, `seen.<session token>`
// (bus.LastSeenPrefix), with the session token every key of that bucket names a session by
// (dedupe.SessionToken).
func lastSeenKey(sessionID string) string {
	return bus.LastSeenPrefix + dedupe.SessionToken(sessionID)
}

// recordSeen records the registration write at revision written as sessionID's last-seen time in
// the listener state bucket, when the registry has it: it reads the write back from the session
// bucket for the server's timestamp, the clock the registration's expiry runs on, and puts it under
// lastSeenKey, each bucket call waiting no longer than ctx (bus.Bounded). A failure of either logs a
// WARN and leaves the registration, which is written, as it is: the record keeps the previous
// write's time. The read is by key, a direct get on the key's own subject, which the listener's
// grant allows; a get by revision is a direct get on the stream's bare subject, which it does not.
// A read that finds another revision, or no record, means a newer write replaced this one, and
// that write records its own.
func (r *SessionRegistry) recordSeen(ctx context.Context, kv bus.KeyValue, sessionID string, written uint64) {
	if r.seen.KeyValue == nil {
		return
	}
	stored, _, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(sessionID) })
	if errors.Is(err, nats.ErrKeyNotFound) || err == nil && stored.Revision() != written {
		return
	}
	if err != nil {
		r.logger().Warn("session last-seen not recorded: the registration write could not be read back",
			slog.String("session_id", sessionID), slog.Uint64("revision", written), slog.String("error", err.Error()))
		return
	}
	value, err := json.Marshal(seenRecord{At: stored.Created().UnixMilli()})
	if err == nil {
		_, _, err = bus.Bounded(ctx, func() (uint64, error) { return r.seen.Put(lastSeenKey(sessionID), value) })
	}
	if err != nil {
		r.logger().Warn("session last-seen not recorded in the listener state bucket",
			slog.String("session_id", sessionID), slog.String("key", lastSeenKey(sessionID)), slog.String("error", err.Error()))
	}
}

// readSeen reads sessionID's last-seen time, in Unix milliseconds, from its record in the listener
// state bucket: 0 when there is none. The read waits no longer than ctx (bus.Bounded), so a route
// that answers a caller is held no longer than its own budget by a bucket that does not answer;
// a read ctx ended is an error naming the key, which the caller reports as no time.
func (r *SessionRegistry) readSeen(ctx context.Context, sessionID string) (int64, error) {
	key := lastSeenKey(sessionID)
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return r.seen.Get(key) })
	if !answered {
		return 0, fmt.Errorf("read %s from the listener state bucket: %w", key, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var record seenRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return 0, fmt.Errorf("decode %s: %w", entry.Key(), err)
	}
	return record.At, nil
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
	r.reconcileDeleted(ctx, kv, sessionID, max(revision, read))
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
	kv := r.watcher.KV()
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
		stored, err := decodeSessionEntry(entry.Value())
		if err != nil {
			// A value this build cannot read names no instance, so it belongs to none: it is
			// deleted as an entry that carries no id is, and the deregistration is not blocked
			// by a record no reader can act on. decodeSessionEntry keeps nothing it decoded
			// before the error for that reason: on a type error json.Unmarshal still fills every
			// field it could read, wherever the bad field sits, so a value carrying a client
			// instance id would otherwise be compared against that id and kept — the opposite of
			// what this branch decides.
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
		r.reconcileDeleted(ctx, kv, sessionID, max(r.cachedRevision(sessionID), revision))
		return EntryDeleted, nil
	}
}

// reconcileDeleted brings the cache up to what the bucket holds for sessionID after a delete this
// registry made: the key's history has the last word, so a registration that landed since the
// delete is cached rather than dropped, and a value this build cannot read is evicted and logged.
// A history that does not answer leaves the fence revision+1 behind instead, above the revision
// the delete acted on, so the watcher's own in-flight update for an older revision cannot restore
// the entry. revision is the newest revision the caller knows of for the key. It calls OnChange's
// functions for sessionID once it has released r.mu, when it changed the cache.
func (r *SessionRegistry) reconcileDeleted(ctx context.Context, kv bus.KeyValue, sessionID string, revision uint64) {
	var applied bool
	// A history that does not answer falls back to the fence below: the delete itself answered.
	entries, _, err := bus.Bounded(ctx, func() ([]nats.KeyValueEntry, error) { return kv.History(sessionID) })
	if err == nil && len(entries) > 0 {
		latest := entries[len(entries)-1]
		r.mu.Lock()
		if latest.Operation() == nats.KeyValueDelete || latest.Operation() == nats.KeyValuePurge {
			applied = r.evictCachedSessionLocked(sessionID, latest.Revision())
		} else {
			item, decodeErr := decodeSessionEntry(latest.Value())
			if decodeErr != nil {
				applied = r.evictCachedSessionLocked(sessionID, latest.Revision())
				r.logger().Warn("session registry cache evicted malformed value",
					slog.String("key", latest.Key()),
					slog.Uint64("revision", latest.Revision()),
					slog.String("error", decodeErr.Error()),
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

// cacheSessionLocked caches entry as sessionID's registration at revision unless the revision fence
// holds a newer revision, and schedules the entry's removal at expiresAt. Without the listener
// state bucket it is also where a registration's UpdatedAt becomes the session's per-process
// last-seen time (seenLocal), whichever process wrote it; with the bucket there is no such record,
// since LastSeen answers a lapsed session from the bucket's shared one alone and a per-process
// record would answer one pod differently from the next. A put whose deadline has already passed
// for an id the cache does not hold is not inserted: it would read as absent anyway, and inserting
// it would bring back a last-seen record for a session that left long ago. That is the late
// delivery of a revision older than a fence expireDue already dropped. It reports whether it
// changed the cache, for the caller to call OnChange's functions once it has released r.mu: a
// revision the fence kept and a put not inserted both report false. Callers must hold r.mu for
// writing.
func (r *SessionRegistry) cacheSessionLocked(sessionID string, entry SessionEntry, expiresAt time.Time, revision uint64) bool {
	if revision < r.cacheRevisions[sessionID] {
		return false
	}
	cs := cachedSession{entry: entry, expiresAt: expiresAt}
	if _, cached := r.cache[sessionID]; !cached && cs.expired(time.Now()) {
		return false
	}
	r.cache[sessionID] = cs
	r.cacheRevisions[sessionID] = revision
	if r.seen.KeyValue == nil && r.ttl > 0 && entry.UpdatedAt > r.seenLocal[sessionID] {
		if r.seenLocal == nil {
			r.seenLocal = map[string]int64{}
		}
		r.seenLocal[sessionID] = entry.UpdatedAt
	}
	r.scheduleLocked(sessionID, expireLive, expiresAt)
	return true
}

func (r *SessionRegistry) cachedRevision(sessionID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheRevisions[sessionID]
}

// evictCachedSessionLocked removes sessionID from the cache for a delete, a purge or a malformed
// value at revision, unless the revision fence holds a newer revision. The fence moves to revision
// and leaves the retention from now, by when the session's last-seen record has ended too
// (expireDue). It reports whether it moved the fence, for the caller to call OnChange's functions
// once it has released r.mu; a revision the fence kept reports false. Callers must hold r.mu for
// writing.
func (r *SessionRegistry) evictCachedSessionLocked(sessionID string, revision uint64) bool {
	if revision < r.cacheRevisions[sessionID] {
		return false
	}
	now := time.Now()
	delete(r.cache, sessionID)
	r.cacheRevisions[sessionID] = revision
	r.scheduleLocked(sessionID, expireLastSeen, r.lastSeenUntilLocked(sessionID, now))
	return true
}

// lastSeenUntilLocked is when sessionID's per-process last-seen record and revision fence may be
// dropped after its entry left the cache at left: the retention past that, and never before the
// record's own end (seenUntil), so nothing LastSeen still reports is dropped. Callers must hold
// r.mu.
func (r *SessionRegistry) lastSeenUntilLocked(sessionID string, left time.Time) time.Time {
	until := left.Add(r.retention())
	if at, ok := r.seenLocal[sessionID]; ok && r.seenUntil(at).After(until) {
		return r.seenUntil(at)
	}
	return until
}
