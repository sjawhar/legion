package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/routing"
)

// writeAttempts bounds the compare-and-set attempts of one interest write (Registry.writeInterest).
const writeAttempts = 5

// ErrWriteConflict is an interest write whose compare-and-set lost to another writer on each of its
// writeAttempts: every time it wrote, the stored revision had moved since it read. Nothing was
// written; the caller may retry, and the listener answers it as a 503 with Retry-After.
var ErrWriteConflict = errors.New("the interest changed under every write attempt")

// RoleClaim is the durable ownership record for a role lane. Role claims are
// independent of interest records: a listener restart can temporarily age an
// interest out before its still-running holder re-registers.
type RoleClaim struct {
	HolderSessionID   string `json:"holder_session_id"`
	ClaimedAt         int64  `json:"claimed_at"`
	PreviousSessionID string `json:"previous_session_id"`

	// unknown holds the JSON of every key a stored claim carries that RoleClaim does not name: a
	// field a newer build writes, which a role claim through this build writes back. It is never
	// modified once decoded, so copies of a claim may share it.
	unknown map[string]json.RawMessage
}

// interestKeys and roleClaimKeys are Interest's and RoleClaim's JSON keys; any other key of a stored
// value is kept in its unknown map, and written back by every rewrite of the value, so pods of two
// builds taking turns at one record during a roll do not drop each other's fields.
var (
	interestKeys  = jsonKeys(reflect.TypeFor[Interest]())
	roleClaimKeys = jsonKeys(reflect.TypeFor[RoleClaim]())
)

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

// decodeKeeping reads data into value, the fields value's type names, and returns the JSON of every
// other key of the object, or nil when there is none.
func decodeKeeping(data []byte, value any, known []string) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, value); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for _, key := range known {
		delete(all, key)
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// encodeKeeping writes value's fields and the unknown keys it was decoded with as one object.
func encodeKeeping(value any, unknown map[string]json.RawMessage) ([]byte, error) {
	known, err := json.Marshal(value)
	if err != nil || len(unknown) == 0 {
		return known, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(known, &fields); err != nil {
		return nil, err
	}
	all := maps.Clone(unknown)
	maps.Copy(all, fields)
	return json.Marshal(all)
}

func decodeInterest(data []byte) (Interest, error) {
	var item Interest
	unknown, err := decodeKeeping(data, &item, interestKeys)
	if err != nil {
		return Interest{}, err
	}
	item.unknown = unknown
	return item, nil
}

func encodeInterest(item Interest) ([]byte, error) {
	return encodeKeeping(item, item.unknown)
}

// ExpiredRoleClaimRelease reports the conditional-delete outcome for an
// expired role claim.
type ExpiredRoleClaimRelease uint8

const (
	ExpiredRoleClaimReleased ExpiredRoleClaimRelease = iota
	ExpiredRoleClaimMissing
	ExpiredRoleClaimSuperseded
	ExpiredRoleClaimRetained
)

type Registry struct {
	// kvMu guards roleKV, which Rewatch moves to a replacement connection. The interest bucket's
	// handle is the watcher's (kvwatch.Watcher.KV).
	kvMu   sync.RWMutex
	roleKV bus.KeyValue
	log    *slog.Logger
	now    func() time.Time
	// started is the registry clock's reading when Open returned: this process's own start, the
	// machine's service start while serviceSince has none to give (serviceStart).
	started time.Time
	// serviceSince reads the machine's service start (WithServiceSince); nil when the caller gave
	// none.
	serviceSince func() time.Time

	mu    sync.RWMutex
	cache map[string]Interest
	// cacheRevisions holds the latest KV revision applied for each cache key,
	// including delete tombstones. It prevents a delayed local write-through
	// from replacing a newer watcher update.
	cacheRevisions map[string]uint64
	// watcher feeds the cache from the interest bucket. Once it is ready the cache is consistent
	// with the bucket, and Match answers every "is this session subscribed?" question without
	// falling through.
	watcher *kvwatch.Watcher
}

// OpenOption configures the registry.
type OpenOption func(*openOpts)

type openOpts struct {
	replicas     int
	now          func() time.Time
	log          *slog.Logger
	serviceSince func() time.Time
}

// WithLogger sets the logger the registry and its cache watcher write through. The listener passes
// its own, so the registry's lines and the watcher's (internal/kvwatch) are the JSON records with
// machine_id that the rest of its output is; the default is slog.Default(), which is what every
// other caller and the tests use. The listener must not set that default instead: it also routes
// the stdlib log package's output into the same handler, and the deployed CloudWatch metric filters
// for publish failures, webhook refusals and dropped stream subjects are space-delimited text
// patterns anchored on that package's date and time prefix
// (defined in the deployment repository's listener infrastructure), so JSON there silently stops three
// alarms. No deployed pattern matches a line from this package or from internal/kvwatch.
func WithLogger(log *slog.Logger) OpenOption {
	return func(o *openOpts) { o.log = log }
}

// WithReplicas overrides the KV bucket replica count. Use 1 for single-node test NATS.
func WithReplicas(n int) OpenOption {
	return func(o *openOpts) { o.replicas = n }
}

// WithClock sets the registry's clock: the time every interest and role claim records, the reaper's
// staleness window, the time a role claim's grace window is measured at, and the process's own
// start (read when Open returns), which serves as the machine's service start when WithServiceSince
// gives none. Tests move it to end a window without waiting for it.
func WithClock(now func() time.Time) OpenOption {
	return func(o *openOpts) { o.now = now }
}

// WithServiceSince sets where the registry reads the machine's service start for a role claim's
// grace (ReleaseExpiredRoleClaim): since a time from which this machine's listener pods have served
// without a gap. The listener passes its delivery lease's service_since, which every pod of the
// machine reads from one record, so every pod gives a claim the same grace. since answers the zero
// time while there is no record to read it from (a standby that has not read the record yet), and
// without this option it is never set: the registry then takes its own start as the service start.
func WithServiceSince(since func() time.Time) OpenOption {
	return func(o *openOpts) { o.serviceSince = since }
}

func Open(conn *nats.Conn, options ...OpenOption) (*Registry, error) {
	opts := openOpts{replicas: 1, now: time.Now, log: slog.Default()}
	for _, o := range options {
		o(&opts)
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: opts.replicas, Storage: nats.FileStorage})
	if err != nil {
		return nil, err
	}
	roleKV, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: opts.replicas, Storage: nats.FileStorage})
	if err != nil {
		return nil, err
	}
	r := &Registry{
		roleKV:         roleKV,
		log:            opts.log,
		cache:          map[string]Interest{},
		cacheRevisions: map[string]uint64{},
		now:            opts.now,
		started:        opts.now(),
		serviceSince:   opts.serviceSince,
	}
	r.watcher = kvwatch.New("interest registry", kv, r.applyWatched, r.resetCache, kvwatch.WithLogger(opts.log))
	// No eager load: the watcher populates the cache asynchronously. A synchronous load of N
	// individual kv.Get() calls blocks indefinitely when the KV stream leader is on a remote node.
	r.watcher.Start()
	return r, nil
}

// Ping verifies both KV buckets are reachable through the current NATS
// connection and reports the cache watcher's terminal error, including an
// interest bucket recreated under it (kvwatch.Watcher.Check). /healthz and the
// listener monitor use failures to report an unavailable dependency while NATS
// reconnects.
func (r *Registry) Ping() error {
	if err := r.watcher.Check(); err != nil {
		return err
	}
	if _, err := r.roles().Status(); err != nil {
		return err
	}
	return r.watcher.Err()
}

// WatchErr is the cache watcher's terminal error, or nil while it runs, so the listener can tell a
// rebuildable dead cache from a transient KV timeout.
func (r *Registry) WatchErr() error {
	return r.watcher.Err()
}

// StopWatch retires the cache's watcher for a shutdown (kvwatch.Watcher.Stop).
func (r *Registry) StopWatch() {
	r.watcher.Stop()
}

// EndWatch ends the cache's watchers at shutdown, within ctx, while the connection still carries
// requests (kvwatch.Watcher.End).
func (r *Registry) EndWatch(ctx context.Context) {
	r.watcher.End(ctx)
}

func (r *Registry) interests() bus.KeyValue {
	return r.watcher.KV()
}

// logger is the logger Open was given, or the default one. A Registry the package's own unit tests
// build as a literal has no logger, and its zero value logging where slog.Default() points is the
// same thing every caller but the listener gets.
func (r *Registry) logger() *slog.Logger {
	if r.log == nil {
		return slog.Default()
	}
	return r.log
}

func (r *Registry) roles() bus.KeyValue {
	r.kvMu.RLock()
	defer r.kvMu.RUnlock()
	return r.roleKV
}

// serviceStart is the machine's service start a role claim's grace counts from: the time
// WithServiceSince reads (the delivery lease's service_since, a server timestamp), or this
// process's own start while that is zero or was never set, which is the service start of a pod
// that serves alone.
func (r *Registry) serviceStart() time.Time {
	if r.serviceSince != nil {
		if since := r.serviceSince(); !since.IsZero() {
			return since
		}
	}
	return r.started
}

func (r *Registry) cachedRevision(sessionID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cacheRevisions[sessionID]
}

func (r *Registry) cacheInterestLocked(sessionID string, item Interest, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	if r.cache == nil {
		r.cache = map[string]Interest{}
	}
	if r.cacheRevisions == nil {
		r.cacheRevisions = map[string]uint64{}
	}
	r.cache[sessionID] = item
	r.cacheRevisions[sessionID] = revision
}

func (r *Registry) evictCachedInterestLocked(sessionID string, revision uint64) {
	if revision < r.cacheRevisions[sessionID] {
		return
	}
	delete(r.cache, sessionID)
	if r.cacheRevisions == nil {
		r.cacheRevisions = map[string]uint64{}
	}
	r.cacheRevisions[sessionID] = revision
}

// deleteInterest deletes sessionID's interest at the revision it reads (bus.KeyValue.DeleteAtRead).
func (r *Registry) deleteInterest(sessionID string) error {
	cached := r.cachedRevision(sessionID)
	read, err := r.interests().DeleteAtRead(sessionID)
	if err != nil {
		return err
	}
	r.applyDelete(sessionID, max(cached, read))
	return nil
}

// deleteInterestAt deletes sessionID's interest only while revision is its latest; a write since
// fails the delete with nats.ErrKeyExists.
func (r *Registry) deleteInterestAt(sessionID string, revision uint64) error {
	cached := r.cachedRevision(sessionID)
	if err := r.interests().Delete(sessionID, nats.LastRevision(revision)); err != nil {
		return err
	}
	r.applyDelete(sessionID, max(cached, revision))
	return nil
}

// applyDelete brings the cache up to a delete of sessionID's interest, the delete of a value no
// newer than revision, which the cache held or the delete read before it was sent.
func (r *Registry) applyDelete(sessionID string, revision uint64) {
	entries, err := r.interests().History(sessionID)
	if err == nil && len(entries) > 0 {
		latest := entries[len(entries)-1]
		if latest.Operation() == nats.KeyValueDelete || latest.Operation() == nats.KeyValuePurge {
			r.mu.Lock()
			r.evictCachedInterestLocked(sessionID, latest.Revision())
			r.mu.Unlock()
			return
		}

		item, err := decodeInterest(latest.Value())
		if err != nil {
			r.mu.Lock()
			r.evictCachedInterestLocked(sessionID, latest.Revision())
			r.mu.Unlock()
			r.logger().Warn("registry cache evicted malformed value",
				slog.String("key", latest.Key()),
				slog.Uint64("revision", latest.Revision()),
				slog.String("error", err.Error()),
			)
			return
		}
		r.mu.Lock()
		r.cacheInterestLocked(sessionID, item, latest.Revision())
		r.mu.Unlock()
		return
	}

	// KeyValue.Delete does not return its revision. A successful delete must
	// still fence off any watcher update from the preceding revision while the
	// delete marker remains pending.
	r.mu.Lock()
	r.evictCachedInterestLocked(sessionID, revision+1)
	r.mu.Unlock()
}

// Rewatch reopens both buckets on conn and replaces the cache's watcher with one there
// (kvwatch.Watcher.Rewatch). A NATS server restart loses the watcher's ordered consumer, and
// nats.go replaces it only once it notices the missed heartbeats, up to twenty seconds later;
// until then the cache misses every write another listener makes, and a drain deletes a consumer
// the server no longer has. The listener's reconnect hook calls this, so the cache follows the
// bucket from the reconnect on.
func (r *Registry) Rewatch(conn *nats.Conn) error {
	// The role bucket opens first, so a failure leaves both handles on the previous connection
	// rather than the interests on conn and the roles behind.
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return fmt.Errorf("open role registry JetStream: %w", err)
	}
	roleKV, err := bus.OpenKeyValue(js, r.roles().Bucket())
	if err != nil {
		return fmt.Errorf("open role KV bucket: %w", err)
	}
	if err := r.watcher.Rewatch(conn); err != nil {
		return err
	}
	r.kvMu.Lock()
	r.roleKV = roleKV
	r.kvMu.Unlock()
	return nil
}

// resetCache empties the cache and its revision fence, for a recreated interest bucket.
func (r *Registry) resetCache() {
	r.mu.Lock()
	r.cache = map[string]Interest{}
	r.cacheRevisions = map[string]uint64{}
	r.mu.Unlock()
}

// applyWatched applies one entry the KV watcher delivered to the cache. The revision fence in
// cacheInterestLocked and evictCachedInterestLocked keeps a delayed local write-through and a
// watcher update from undoing each other.
func (r *Registry) applyWatched(entry nats.KeyValueEntry) {
	if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
		r.mu.Lock()
		r.evictCachedInterestLocked(entry.Key(), entry.Revision())
		r.mu.Unlock()
		return
	}

	item, err := decodeInterest(entry.Value())
	if err != nil {
		r.mu.Lock()
		r.evictCachedInterestLocked(entry.Key(), entry.Revision())
		r.mu.Unlock()
		r.logger().Warn("registry watcher evicted malformed value",
			slog.String("key", entry.Key()),
			slog.Uint64("revision", entry.Revision()),
			slog.String("error", err.Error()),
		)
		return
	}
	r.mu.Lock()
	r.cacheInterestLocked(entry.Key(), item, entry.Revision())
	r.mu.Unlock()
}

// WaitForCacheReady blocks until the watcher has delivered every existing interest, or until ctx
// is done. After it returns nil, Match sees every existing subscription in the bucket. Callers
// bound ctx: a watcher that fails to start releases it too, and the caller then serves the
// write-through path while /healthz exposes the unavailable registry.
func (r *Registry) WaitForCacheReady(ctx context.Context) error {
	return r.watcher.WaitReady(ctx)
}

// Upsert adds topics to item.SessionID's interest, merged onto the stored one (mergeForUpsert) and
// written by compare-and-set (writeInterest).
func (r *Registry) Upsert(item Interest, topics []string) (Interest, error) {
	return r.writeInterest(item.SessionID, func(cur Interest, curErr error) (Interest, bool, error) {
		merged, err := mergeForUpsert(cur, curErr, item, topics, r.now().UnixMilli())
		return merged, false, err
	})
}

// writeInterest writes sessionID's interest by compare-and-set on its KV revision, so a write another
// listener made between this one's read and its write is never overwritten. change computes the
// interest to write from the stored one and the error reading it (nats.ErrKeyNotFound when there is
// none), or remove to delete it. The first attempt changes the cached interest. A cache miss, or a
// write the bucket refuses because the revision moved, reads the bucket entry and changes that: an
// Update or a delete at its revision, or a Create when there is none. After writeAttempts refused
// writes it returns ErrWriteConflict, having written nothing.
func (r *Registry) writeInterest(sessionID string, change func(cur Interest, curErr error) (next Interest, remove bool, err error)) (Interest, error) {
	r.mu.RLock()
	cur, found := r.cache[sessionID]
	revision := r.cacheRevisions[sessionID]
	r.mu.RUnlock()
	var curErr error
	read := !found
	for range writeAttempts {
		if read {
			cur, revision, curErr = r.readInterest(sessionID)
		}
		read = true
		next, remove, err := change(cur, curErr)
		if err != nil {
			return Interest{}, err
		}
		switch {
		case remove && revision == 0:
			return next, nil
		case remove:
			err = r.deleteInterestAt(sessionID, revision)
		default:
			err = r.putInterestAt(sessionID, next, revision)
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		if err != nil {
			return Interest{}, err
		}
		return next, nil
	}
	return Interest{}, fmt.Errorf("%w: %s", ErrWriteConflict, sessionID)
}

// putInterestAt writes item as sessionID's interest while revision is its latest (0: while it has
// none), and applies the write to the cache.
func (r *Registry) putInterestAt(sessionID string, item Interest, revision uint64) error {
	buf, err := encodeInterest(item)
	if err != nil {
		return err
	}
	var written uint64
	if revision == 0 {
		written, err = r.interests().Create(sessionID, buf)
	} else {
		written, err = r.interests().Update(sessionID, buf, revision)
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cacheInterestLocked(sessionID, item, written)
	r.mu.Unlock()
	return nil
}

// readInterest reads sessionID's interest from the bucket, applies it to the cache, and returns it
// with its revision. A key with no value is nats.ErrKeyNotFound at revision 0; a value that does not
// decode is its decode error at its revision.
func (r *Registry) readInterest(sessionID string) (Interest, uint64, error) {
	entry, err := r.interests().Get(sessionID)
	if err != nil {
		return Interest{}, 0, err
	}
	item, err := decodeInterest(entry.Value())
	if err != nil {
		return Interest{}, entry.Revision(), err
	}
	r.mu.Lock()
	r.cacheInterestLocked(sessionID, item, entry.Revision())
	r.mu.Unlock()
	return item, entry.Revision(), nil
}

// mergeForUpsert computes the Interest to persist by reconciling the requested
// item/topics with the current cached/durable state returned by r.Get.
//
// Critical invariant: a transient KV error (anything other than ErrKeyNotFound)
// MUST be returned so the caller refuses to overwrite durable state with the
// partial heartbeat payload. Silently treating transient errors as "no prior
// state" is what caused Atlas's pr.11416.> subscription to disappear: when r.Get
// failed transiently during a 2-minute heartbeat, the original implementation
// merged only the agent topic and Put a truncated state to KV, which then
// propagated to the in-memory cache via watch() and made all subsequent github
// events fall through registry.Match with "no matching interests".
//
// ErrKeyNotFound is the genuinely-new-session case — proceed with the passed-in
// item and topics.
func mergeForUpsert(cur Interest, getErr error, item Interest, topics []string, now int64) (Interest, error) {
	if getErr != nil && !errors.Is(getErr, nats.ErrKeyNotFound) {
		return Interest{}, getErr
	}
	if getErr == nil {
		item = cur
	}
	item.MachineID = first(item.MachineID, curValue(cur.MachineID))
	item.Dir = first(item.Dir, curValue(cur.Dir))
	item.UpdatedAt = now
	item = Merge(item, topics)
	sort.Strings(item.Topics)
	return item, nil
}

// decodeRoleClaim reads a stored claim: the fields RoleClaim names, every other key into its unknown
// map, or a bare session id an earlier build stored.
func decodeRoleClaim(value []byte) (RoleClaim, error) {
	var claim RoleClaim
	unknown, err := decodeKeeping(value, &claim, roleClaimKeys)
	if err == nil {
		if strings.TrimSpace(claim.HolderSessionID) == "" {
			return RoleClaim{}, fmt.Errorf("role claim has no holder")
		}
		claim.unknown = unknown
		return claim, nil
	}
	if json.Valid(value) {
		return RoleClaim{}, fmt.Errorf("decode role claim: %w", err)
	}
	// Accept bare session-ID records so persisted claims remain routable;
	// a later claim normalizes the row to the structured format.
	holder := strings.TrimSpace(string(value))
	if holder == "" {
		return RoleClaim{}, fmt.Errorf("decode legacy role claim: empty holder")
	}
	return RoleClaim{HolderSessionID: holder}, nil
}

func (r *Registry) roleClaim(role string) (RoleClaim, nats.KeyValueEntry, error) {
	entry, err := r.roles().Get(role)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return RoleClaim{}, nil, nil
	}
	if err != nil {
		return RoleClaim{}, nil, err
	}
	claim, err := decodeRoleClaim(entry.Value())
	if err != nil {
		return RoleClaim{}, nil, err
	}
	return claim, entry, nil
}

func (r *Registry) releaseRoleClaims(sessionID string, topics []string) error {
	for _, topic := range topics {
		if !strings.HasPrefix(topic, contracts.RoleTopicPrefix) {
			continue
		}
		role := strings.TrimPrefix(topic, contracts.RoleTopicPrefix)
		if role == "" || strings.ContainsAny(role, "*>") {
			continue
		}
		if err := r.releaseRoleClaim(sessionID, role); err != nil {
			return err
		}
	}
	return nil
}

// releaseRoleClaim deletes role's claim when sessionID holds it. A claim stored under a key NATS
// refuses (bus.ErrRefused: too long, or outside its key alphabet) has a holder nothing can tell, so
// it is left to the role reaper, which deletes it, or skips one no KV call can delete.
func (r *Registry) releaseRoleClaim(sessionID, role string) error {
	claim, entry, err := r.roleClaim(role)
	if errors.Is(err, bus.ErrRefused) {
		r.logger().Warn("role registry left a stored claim it cannot read to the role reaper", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
		return nil
	}
	if err != nil {
		return err
	}
	if entry == nil || claim.HolderSessionID != sessionID {
		return nil
	}
	err = r.roles().Delete(role, nats.LastRevision(entry.Revision()))
	if err != nil && !errors.Is(err, nats.ErrKeyExists) && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	return nil
}

// ReleaseExpiredRoleClaim drops role only when sessionID remains its holder and
// the claim has no grace left. A claim the bucket held from before the machine's
// service start (serviceStart) keeps its grace for one session TTL after that
// start, so a listener restart, or a gap with no pod serving, gives an existing
// holder one TTL to register again, while an already-expired holder cannot block
// a new claimant forever. A claim written since the service start gets none.
// Every pod of a machine reads one service start (WithServiceSince), so every
// pod answers one claim alike, whenever it started.
//
// The claim's age is its KV entry's timestamp, the server's clock, and the
// window is measured on this process's clock: a skew of seconds between the
// two, against a session TTL of minutes, moves the window's end by that skew.
//
// Its result distinguishes a confirmed deletion from a concurrent removal or
// replacement, so callers never report that a lost conditional delete released
// a claim.
func (r *Registry) ReleaseExpiredRoleClaim(role, sessionID string, sessionTTL time.Duration) (ExpiredRoleClaimRelease, error) {
	claim, entry, err := r.roleClaim(role)
	if err != nil {
		return ExpiredRoleClaimRetained, err
	}
	if entry == nil {
		return ExpiredRoleClaimMissing, nil
	}
	if claim.HolderSessionID != sessionID {
		return ExpiredRoleClaimSuperseded, nil
	}
	if since := r.serviceStart(); sessionTTL > 0 && entry.Created().Before(since) && r.now().Sub(since) < sessionTTL {
		return ExpiredRoleClaimRetained, nil
	}
	err = r.roles().Delete(role, nats.LastRevision(entry.Revision()))
	switch {
	case err == nil:
		return ExpiredRoleClaimReleased, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return ExpiredRoleClaimMissing, nil
	case errors.Is(err, nats.ErrKeyExists):
		return ExpiredRoleClaimSuperseded, nil
	default:
		return ExpiredRoleClaimRetained, err
	}
}

func (r *Registry) releaseAllRoleClaims(sessionID string) error {
	roles, err := r.roles().Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, role := range roles {
		if err := r.releaseRoleClaim(sessionID, role); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) Remove(sessionID string, topics []string) error {
	if len(topics) == 0 {
		item, err := r.Get(sessionID)
		if err == nil {
			if err := r.releaseRoleClaims(sessionID, item.Topics); err != nil {
				return err
			}
		} else if !errors.Is(err, nats.ErrKeyNotFound) {
			return err
		}
		if err := r.releaseAllRoleClaims(sessionID); err != nil {
			return err
		}
	} else if err := r.releaseRoleClaims(sessionID, topics); err != nil {
		return err
	}
	return r.removeInterestTopics(sessionID, topics)
}

// removeInterestTopics removes topics from sessionID's stored interest by compare-and-set
// (writeInterest), deleting the interest when none is left; no topics removes it whole.
func (r *Registry) removeInterestTopics(sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry).
	if len(topics) == 0 {
		return r.deleteInterest(sessionID)
	}
	_, err := r.writeInterest(sessionID, func(cur Interest, curErr error) (Interest, bool, error) {
		if curErr != nil {
			return Interest{}, false, curErr
		}
		item := Remove(cur, topics)
		if len(item.Topics) == 0 {
			return item, true, nil
		}
		item.UpdatedAt = r.now().UnixMilli()
		return item, false, nil
	})
	return err
}

// ErrRoleHeld reports a soft claim that found the role held by another session.
// The holder is carried on the error so callers can report who has it.
type ErrRoleHeld struct {
	Role   string
	Holder string
}

func (e *ErrRoleHeld) Error() string {
	return fmt.Sprintf("role %s is held by %s", e.Role, e.Holder)
}

// SetRole makes sessionID the holder of role. A hard claim (soft=false) is
// last-claim-wins: it takes the role from whoever holds it. A soft claim
// succeeds only if the role is unheld, already held by sessionID, or held by
// a session listed in supersedable; any other holder returns *ErrRoleHeld
// and leaves the claim untouched. Callers decide what "supersedable" means —
// the listener passes the ids it has established are no longer live, so a
// resumed session can recover a role its dead predecessor held without ever
// taking one from a live peer.
func (r *Registry) SetRole(sessionID, machineID, role string, soft bool, supersedable ...string) (Interest, error) {
	return r.SetRoleWithPrevious(sessionID, machineID, role, "", soft, supersedable...)
}

// SetRoleWithPrevious records the session ID that a successful soft claim
// continues. The predecessor is provenance only after the listener has
// evaluated the existing soft-claim rules.
func (r *Registry) SetRoleWithPrevious(sessionID, machineID, role, previousSessionID string, soft bool, supersedable ...string) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + role
	oldClaim, entry, err := r.roleClaim(role)
	if err != nil {
		return Interest{}, err
	}

	oldSessionID := oldClaim.HolderSessionID
	if soft && oldSessionID != "" && oldSessionID != sessionID && !slices.Contains(supersedable, oldSessionID) {
		return Interest{}, &ErrRoleHeld{Role: role, Holder: oldSessionID}
	}

	item, err := r.Upsert(Interest{SessionID: sessionID, MachineID: machineID}, []string{roleTopic})
	if err != nil {
		return Interest{}, err
	}

	// The claim replaces the stored one at its revision, keeping the fields a newer build stored in
	// it that this build does not know.
	claim := RoleClaim{
		HolderSessionID:   sessionID,
		ClaimedAt:         r.now().UnixMilli(),
		PreviousSessionID: "",
		unknown:           oldClaim.unknown,
	}
	if soft {
		claim.PreviousSessionID = previousSessionID
	}
	data, err := encodeKeeping(claim, claim.unknown)
	if err != nil {
		return Interest{}, err
	}
	if entry == nil {
		_, err = r.roles().Create(role, data)
	} else {
		_, err = r.roles().Update(role, data, entry.Revision())
	}
	if err != nil {
		holder, holderErr := r.RoleHolder(role)
		if holderErr == nil && holder == sessionID {
			return item, nil
		}
		if rollbackErr := r.removeInterestTopics(sessionID, []string{roleTopic}); rollbackErr != nil {
			return Interest{}, errors.Join(err, holderErr, rollbackErr)
		}
		if holderErr != nil {
			return Interest{}, errors.Join(err, holderErr)
		}
		// A soft claim that lost the CAS race lost to a concurrent claimant;
		// report that as held rather than as a storage failure.
		if soft && holder != "" {
			return Interest{}, &ErrRoleHeld{Role: role, Holder: holder}
		}
		return Interest{}, err
	}

	if oldSessionID != "" && oldSessionID != sessionID {
		err := r.removeInterestTopics(oldSessionID, []string{roleTopic})
		if errors.Is(err, bus.ErrRefused) {
			// The old holder's id is a key NATS refuses (too long, or outside its key alphabet): an
			// earlier build registered it, or a stored claim names it. The claim is written and the
			// caller named no such key, so it succeeds; the interest reaper removes the old holder's
			// interest once its session is gone, or skips one no KV call can delete.
			r.logger().Warn("registry role claim left an old holder's interest it cannot write to the reaper",
				slog.String("role", role),
				slog.Int("key_bytes", len(oldSessionID)),
				slog.String("error", err.Error()),
			)
		} else if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
			r.logger().Warn("registry role claim old holder cleanup failed",
				slog.String("role", role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", sessionID),
				slog.String("error", err.Error()),
			)
			return Interest{}, err
		}
	}
	return item, nil
}

// RoleClaim returns the durable claim for role. An unclaimed role returns a
// zero-value claim without an error.
func (r *Registry) RoleClaim(role string) (RoleClaim, error) {
	claim, _, err := r.roleClaim(role)
	return claim, err
}

// RoleHolder returns the authoritative session ID for role. An unclaimed role
// returns an empty holder without an error.
func (r *Registry) RoleHolder(role string) (string, error) {
	claim, err := r.RoleClaim(role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
}

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache.
func (r *Registry) Get(sessionID string) (Interest, error) {
	r.mu.RLock()
	item, ok := r.cache[sessionID]
	r.mu.RUnlock()
	if ok {
		return item, nil
	}
	item, _, err := r.readInterest(sessionID)
	return item, err
}

func (r *Registry) Match(machineID string, topic string) []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Interest{}
	for _, item := range r.cache {
		if item.MachineID != machineID {
			continue
		}
		for _, pattern := range item.Topics {
			if routing.Match(pattern, topic) {
				out = append(out, item)
				break
			}
		}
	}
	return out
}

// List returns all cached interests sorted by SessionID for deterministic output.
func (r *Registry) List() []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Interest, 0, len(r.cache))
	for _, item := range r.cache {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

func first(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func curValue(value string) string {
	return value
}

// Reap cross-references the interest cache with a session liveness check and
// deletes interests whose sessions are dead AND whose UpdatedAt exceeds the
// grace window. A role claim is intentionally not an interest: it remains
// available for its holder to re-register after a listener restart, and the
// role delivery path drops it if that holder never becomes live again.
func (r *Registry) Reap(isAlive func(string) bool, graceWindow time.Duration) (int, error) {
	now := r.now().UnixMilli()
	graceMs := graceWindow.Milliseconds()

	r.mu.RLock()
	var stale []string
	for sid, item := range r.cache {
		if isAlive(sid) {
			continue // session still alive
		}
		if now-item.UpdatedAt <= graceMs {
			continue // within grace window
		}
		stale = append(stale, sid)
	}
	r.mu.RUnlock()

	reaped := 0
	for _, sid := range stale {
		err := r.deleteInterest(sid)
		if errors.Is(err, bus.ErrRefused) {
			r.logger().Warn("reaper skipped an interest it cannot delete", slog.Int("key_bytes", len(sid)), slog.String("error", err.Error()))
			continue
		}
		if err != nil {
			return 0, err
		}
		reaped++
	}
	return reaped, nil
}

// ReapRoleClaims removes claims whose holders did not re-register before the
// session TTL elapsed. It is intentionally separate from Reap: the interest
// reaper must not tear down a role during a listener restart grace window. A
// claim this build cannot read (bus.ErrRefused: one stored under a key NATS
// refuses, too long or outside its key alphabet) no build can resolve, so it is
// deleted too, at the revision the sweep's scan saw, which keeps a claim an
// earlier build without the key check writes there in the meantime; one it
// cannot delete either (a key outside the alphabet) is skipped, and the sweep
// goes on.
func (r *Registry) ReapRoleClaims(isAlive func(string) bool, sessionTTL time.Duration) (int, error) {
	type stored struct {
		role     string
		revision uint64
	}
	var claims []stored
	if _, err := kvwatch.ScanExistingKeys(r.roles(), "role claim reaper", r.logger(), func(entry nats.KeyValueEntry) {
		claims = append(claims, stored{role: entry.Key(), revision: entry.Revision()})
	}); err != nil {
		return 0, err
	}
	reaped := 0
	for _, stored := range claims {
		role := stored.role
		claim, err := r.RoleClaim(role)
		if errors.Is(err, bus.ErrRefused) {
			switch err := r.roles().Delete(role, nats.LastRevision(stored.revision)); {
			case err == nil:
				r.logger().Warn("role reaper deleted a claim it cannot read", slog.Int("key_bytes", len(role)))
				reaped++
			case errors.Is(err, nats.ErrKeyExists):
				r.logger().Warn("role reaper kept a claim it cannot read, rewritten since its scan", slog.Int("key_bytes", len(role)))
			case errors.Is(err, bus.ErrRefused):
				r.logger().Warn("role reaper skipped a claim it cannot delete", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
			default:
				return 0, err
			}
			continue
		}
		if err != nil {
			return 0, err
		}
		if claim.HolderSessionID == "" || isAlive(claim.HolderSessionID) {
			continue
		}
		release, err := r.ReleaseExpiredRoleClaim(role, claim.HolderSessionID, sessionTTL)
		if err != nil {
			return 0, err
		}
		if release == ExpiredRoleClaimReleased {
			reaped++
		}
	}
	return reaped, nil
}

// StartReaper runs Reap in a background goroutine at the given interval until ctx ends, and returns
// a channel closed once it has returned: a pass in flight when ctx ends finishes first. A pass
// lists the bucket through watchers whose consumers nats.go creates and deletes, so the listener's
// shutdown waits for it before it drains NATS (kvwatch.Watcher.End says what a drain does to such
// a delete).
func (r *Registry) StartReaper(ctx context.Context, isAlive func(string) bool, interval, graceWindow time.Duration) <-chan struct{} {
	return everyInterval(ctx, interval, func() {
		count, err := r.Reap(isAlive, graceWindow)
		if err != nil {
			r.logger().Error("reaper cycle failed", slog.String("error", err.Error()))
			return
		}
		r.logger().Info("reaper cycle", slog.Int("reaped", count))
	})
}

// StartRoleClaimReaper expires role holders that fail to re-register after a
// listener restart. It is independent of interest reaping because a role is a
// point-to-point route rather than a general subscription. It runs until ctx ends, and the
// channel it returns is closed once it has returned, as StartReaper's is.
func (r *Registry) StartRoleClaimReaper(ctx context.Context, isAlive func(string) bool, interval, sessionTTL time.Duration) <-chan struct{} {
	return everyInterval(ctx, interval, func() {
		count, err := r.ReapRoleClaims(isAlive, sessionTTL)
		if err != nil {
			r.logger().Error("role claim reaper cycle failed", slog.String("error", err.Error()))
			return
		}
		r.logger().Info("role claim reaper cycle", slog.Int("reaped", count))
	})
}

// everyInterval runs pass in a background goroutine at each tick of interval until ctx ends, and
// returns a channel closed once the goroutine has returned.
func everyInterval(ctx context.Context, interval time.Duration, pass func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			// A tick and the end can both be ready, and select picks either.
			if ctx.Err() != nil {
				return
			}
			pass()
		}
	}()
	return done
}
