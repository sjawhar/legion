package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/kvwatch"
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

	// unknown holds the JSON of every key a stored registration carries that SessionEntry does not
	// name: a field a newer build writes. encodeSessionEntry writes it back beside the known fields,
	// so a heartbeat through this build keeps it while pods of two builds take turns writing one
	// session during a roll. It is never modified once decoded, so copies of an entry may share it.
	unknown map[string]json.RawMessage
}

// sessionEntryKeys are SessionEntry's JSON keys; any other key of a stored registration is kept in
// SessionEntry.unknown.
var sessionEntryKeys = jsonKeys(reflect.TypeFor[SessionEntry]())

// jsonKeys are the object keys encoding/json writes for t's exported fields.
func jsonKeys(t reflect.Type) []string {
	keys := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = field.Name
		}
		keys = append(keys, name)
	}
	return keys
}

// decodeSessionEntry reads a stored registration: the fields SessionEntry names, and every other key
// into its unknown map.
func decodeSessionEntry(data []byte) (SessionEntry, error) {
	var entry SessionEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return SessionEntry{}, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return SessionEntry{}, err
	}
	for _, key := range sessionEntryKeys {
		delete(all, key)
	}
	if len(all) > 0 {
		entry.unknown = all
	}
	return entry, nil
}

// encodeSessionEntry writes entry for the bucket: its known fields, and the unknown keys it was
// decoded with.
func encodeSessionEntry(entry SessionEntry) ([]byte, error) {
	known, err := json.Marshal(entry)
	if err != nil || len(entry.unknown) == 0 {
		return known, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(known, &fields); err != nil {
		return nil, err
	}
	all := maps.Clone(entry.unknown)
	maps.Copy(all, fields)
	return json.Marshal(all)
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
	seen     bus.KeyValue
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
// (lastSeenKey), so every listener pod answers LastSeen for a lapsed session alike, a pod that
// started after the session lapsed included. Without it the registry keeps last-seen times in
// process (SessionRegistry.seenLocal): a listener whose NATS user lacks the grant for that bucket
// runs so, and a restart forgets them.
func WithLastSeenBucket(kv bus.KeyValue) SessionRegistryOption {
	return func(o *sessionRegistryOpts) { o.seen = kv }
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
	ttl time.Duration

	mu             sync.RWMutex
	cache          map[string]cachedSession
	cacheRevisions map[string]uint64
	// seenLocal is LastSeen's per-process fallback, used while seen has no handle: the UpdatedAt
	// of each session's latest registration this registry wrote or its watcher delivered, until
	// one session TTL after that registration's expiry. It is guarded by mu.
	seenLocal map[string]int64

	// seen is the listener state bucket LastSeen reads a lapsed session's `seen.` record from
	// (WithLastSeenBucket); its KeyValue is nil when the registry has none.
	seen bus.KeyValue

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
		seenLocal:      map[string]int64{},
		seen:           opts.seen,
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
		item, err := decodeSessionEntry(entry.Value())
		if err != nil {
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

// Put writes entry as sessionID's registration. Several live processes can hold the same session
// (shared on-disk state), and blind last-writer-wins routed deliveries to whichever heartbeated last,
// including processes not driving the session: mergeForClaim arbitrates between entry and the stored
// registration, and the merge is written by compare-and-set (write), so the registration it ran on
// is the one replaced.
func (r *SessionRegistry) Put(sessionID string, entry SessionEntry) error {
	return r.write(sessionID, func(cur SessionEntry, curErr error, now int64) (SessionEntry, error) {
		return mergeForClaim(cur, curErr, entry, now)
	})
}

// Amend applies change to sessionID's stored registration and writes the result as Put writes it, so
// a change made on a listener whose cache trails the bucket lands on what another listener wrote
// since, not on its own stale copy. base is the registration change applies to when the bucket holds
// none (the caller's earlier read, which an expiry or a delete has overtaken), so the write registers
// the session again.
func (r *SessionRegistry) Amend(sessionID string, base SessionEntry, change func(*SessionEntry)) error {
	return r.write(sessionID, func(cur SessionEntry, curErr error, now int64) (SessionEntry, error) {
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
func (r *SessionRegistry) write(sessionID string, merge func(cur SessionEntry, curErr error, now int64) (SessionEntry, error)) error {
	kv := r.watcher.KV()
	cur, revision, found := r.cachedEntry(sessionID)
	read := !found
	for range writeAttempts {
		if read {
			var err error
			if cur, revision, found, err = r.readStored(kv, sessionID); err != nil {
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
		var written uint64
		if revision == 0 {
			written, err = kv.Create(sessionID, buf)
		} else {
			written, err = kv.Update(sessionID, buf, revision)
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
		r.recordSeen(kv, sessionID, written)
		return nil
	}
	return fmt.Errorf("%w: session %s", ErrWriteConflict, sessionID)
}

// cachedEntry is sessionID's live cached registration and the revision the cache holds for it.
func (r *SessionRegistry) cachedEntry(sessionID string) (SessionEntry, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	cs, ok := r.cache[sessionID]
	return cs.entry, r.cacheRevisions[sessionID], ok
}

// readStored reads sessionID's registration from the bucket for a write, and applies it to the cache
// as the watcher would. revision is the one to write at, 0 when the bucket holds no value. A value
// that does not decode is found false, as the cache, which evicts it, sees no registration either,
// and the write replaces it at its revision.
func (r *SessionRegistry) readStored(kv bus.KeyValue, sessionID string) (entry SessionEntry, revision uint64, found bool, err error) {
	stored, err := kv.Get(sessionID)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return SessionEntry{}, 0, false, nil
	}
	if err != nil {
		return SessionEntry{}, 0, false, err
	}
	r.applyWatched(stored)
	entry, err = decodeSessionEntry(stored.Value())
	return entry, stored.Revision(), err == nil, nil
}

func (r *SessionRegistry) Get(sessionID string) (SessionEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	cs, ok := r.cache[sessionID]
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

// LastSeen is sessionID's last registration write, in Unix milliseconds, or 0 when there is none to
// report. While the session is live it is the live registration's UpdatedAt. Once the session lapsed
// (its registration expired or was deleted) it is the time its last registration write recorded
// (recordSeen), for one session TTL after that registration's expiry, so callers can report why an
// otherwise durable reference stopped resolving. With the listener state bucket
// (WithLastSeenBucket) that time is the session's `seen.` record, the server's timestamp of the
// write, which every listener pod reads alike, one that started after the session lapsed included.
// The bucket keeps a record for its MaxAge (10 minutes) after the record's last write, which covers
// the default 5 minute session TTL twice; a session bucket made with a longer TTL loses the record
// before that retention ends. Without the bucket the time is this process's seenLocal, which knows
// only the registrations this process saw and which a restart forgets. The retention is measured on
// this process's clock against the server's timestamp; a skew of seconds against a TTL of minutes
// moves its end by that skew.
func (r *SessionRegistry) LastSeen(sessionID string) int64 {
	now := time.Now()
	r.mu.Lock()
	r.pruneLocked(now)
	cs, live := r.cache[sessionID]
	at := r.seenLocal[sessionID]
	r.mu.Unlock()
	if live {
		return cs.entry.UpdatedAt
	}
	if r.ttl <= 0 {
		return 0
	}
	if r.seen.KeyValue != nil {
		var err error
		if at, err = r.readSeen(sessionID); err != nil {
			r.logger().Warn("session last-seen unavailable: its record in the listener state bucket could not be read",
				slog.String("session_id", sessionID), slog.String("key", lastSeenKey(sessionID)), slog.String("error", err.Error()))
			return 0
		}
	}
	if at <= 0 || !now.Before(r.seenUntil(at)) {
		return 0
	}
	return at
}

// seenUntil is when a lapsed session's last-seen time at (Unix milliseconds) stops being reported:
// one session TTL after the expiry of the registration written at at.
func (r *SessionRegistry) seenUntil(at int64) time.Time {
	return time.UnixMilli(at).Add(2 * r.ttl)
}

// seenRecord is the value of a session's `seen.` record in the listener state bucket: at is the
// server's timestamp, in Unix milliseconds, of the session's last registration write.
type seenRecord struct {
	At int64 `json:"at"`
}

// lastSeenKey is sessionID's last-seen key in the listener state bucket, `seen.<session token>`,
// with the session token every key of that bucket names a session by (dedupe.SessionToken).
func lastSeenKey(sessionID string) string {
	return "seen." + dedupe.SessionToken(sessionID)
}

// recordSeen records the registration write at revision written as sessionID's last-seen time in
// the listener state bucket, when the registry has it: it reads the write back from the session
// bucket for the server's timestamp, the clock the registration's expiry runs on, and puts it under
// lastSeenKey. A failure of either logs a WARN and leaves the registration, which is written, as it
// is: the record keeps the previous write's time. A revision the read no longer finds was replaced
// by a newer write, which records its own.
func (r *SessionRegistry) recordSeen(kv bus.KeyValue, sessionID string, written uint64) {
	if r.seen.KeyValue == nil {
		return
	}
	stored, err := kv.GetRevision(sessionID, written)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return
	}
	if err != nil {
		r.logger().Warn("session last-seen not recorded: the registration write could not be read back",
			slog.String("session_id", sessionID), slog.Uint64("revision", written), slog.String("error", err.Error()))
		return
	}
	value, err := json.Marshal(seenRecord{At: stored.Created().UnixMilli()})
	if err == nil {
		_, err = r.seen.Put(lastSeenKey(sessionID), value)
	}
	if err != nil {
		r.logger().Warn("session last-seen not recorded in the listener state bucket",
			slog.String("session_id", sessionID), slog.String("key", lastSeenKey(sessionID)), slog.String("error", err.Error()))
	}
}

// readSeen reads sessionID's last-seen time, in Unix milliseconds, from its record in the listener
// state bucket: 0 when there is none.
func (r *SessionRegistry) readSeen(sessionID string) (int64, error) {
	entry, err := r.seen.Get(lastSeenKey(sessionID))
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

func (r *SessionRegistry) Delete(sessionID string) error {
	revision := r.cachedRevision(sessionID)
	kv := r.watcher.KV()
	read, err := kv.DeleteAtRead(sessionID)
	if err != nil {
		return err
	}
	revision = max(revision, read)
	entries, err := kv.History(sessionID)
	if err == nil && len(entries) > 0 {
		latest := entries[len(entries)-1]
		r.mu.Lock()
		defer r.mu.Unlock()
		if latest.Operation() == nats.KeyValueDelete || latest.Operation() == nats.KeyValuePurge {
			r.evictCachedSessionLocked(sessionID, latest.Revision())
			return nil
		}
		item, err := decodeSessionEntry(latest.Value())
		if err != nil {
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

// List returns all live sessions from the in-memory cache, pruning entries past
// their local TTL. Cache-only: no JetStream round-trips, so it can't hang on a
// slow KV leader the way the old Keys()+per-key Get() loop did.
func (r *SessionRegistry) List() ([]ListEntry, error) {
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

// pruneLocked removes expired live entries and the per-process last-seen times past their retention.
// Callers must hold r.mu for writing.
func (r *SessionRegistry) pruneLocked(now time.Time) {
	for key, cs := range r.cache {
		if cs.expired(now) {
			delete(r.cache, key)
		}
	}
	for key, at := range r.seenLocal {
		if !now.Before(r.seenUntil(at)) {
			delete(r.seenLocal, key)
		}
	}
}

// cacheSessionLocked caches entry as sessionID's registration at revision unless the cache holds a
// later one. Without the listener state bucket it is also where a registration's UpdatedAt becomes
// the session's per-process last-seen time (seenLocal), whichever process wrote it.
func (r *SessionRegistry) cacheSessionLocked(sessionID string, entry SessionEntry, expiresAt time.Time, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	r.cache[sessionID] = cachedSession{entry: entry, expiresAt: expiresAt}
	r.cacheRevisions[sessionID] = revision
	if r.seen.KeyValue == nil && r.ttl > 0 && entry.UpdatedAt > r.seenLocal[sessionID] {
		if r.seenLocal == nil {
			r.seenLocal = map[string]int64{}
		}
		r.seenLocal[sessionID] = entry.UpdatedAt
	}
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
	delete(r.cache, sessionID)
	r.cacheRevisions[sessionID] = revision
}
