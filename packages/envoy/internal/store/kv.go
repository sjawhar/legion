package store

import (
	"bytes"
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
	interestKeys  = JSONKeys(reflect.TypeFor[Interest]())
	roleClaimKeys = JSONKeys(reflect.TypeFor[RoleClaim]())
)

// JSONKeys are the object keys encoding/json writes for t's exported fields. A record type that
// keeps the keys of a newer build passes them to DecodeKeeping as the keys it knows.
func JSONKeys(t reflect.Type) []string {
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

// DecodeKeeping reads data into value, the fields value's type names, and returns the JSON of every
// key of the object not in known, or nil when there is none: the fields a newer build writes, which
// EncodeKeeping writes back.
func DecodeKeeping(data []byte, value any, known []string) (map[string]json.RawMessage, error) {
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

// EncodeKeeping writes value's fields and the unknown keys it was decoded with (DecodeKeeping) as one
// object, value's fields winning over an unknown key of the same name.
func EncodeKeeping(value any, unknown map[string]json.RawMessage) ([]byte, error) {
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
	unknown, err := DecodeKeeping(data, &item, interestKeys)
	if err != nil {
		return Interest{}, err
	}
	item.unknown = unknown
	return item, nil
}

func encodeInterest(item Interest) ([]byte, error) {
	return EncodeKeeping(item, item.unknown)
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
	// roleWatcher feeds holders from the role bucket, so a session's roles are read from the claims
	// with no bucket call (RolesHeldBy), which answers only while its first scan is complete
	// (kvwatch.Watcher.Complete). holdersMu guards holders, the session each role's claim names,
	// holderRevisions, the latest revision applied for each role, delete markers included, which keeps
	// a delayed watcher update from undoing a claim this registry wrote, and holderEpoch, which a
	// recreated bucket's reset advances, so a claim write this registry made on the old bucket cannot
	// land in the new bucket's view and fence out its lower revisions.
	roleWatcher     *kvwatch.Watcher
	holdersMu       sync.RWMutex
	holders         map[string]string
	holderRevisions map[string]uint64
	holderEpoch     uint64
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

// WithReplicas sets the deployment's replica count (bus.ReplicasEnvVar, default 1) for the interest
// and role buckets: the count a new bucket is created at, and the count an existing bucket with
// fewer copies is raised to at open (bus.EnsureKeyValue) and by the listener's keeper after it
// (bus.WithKeptBuckets). It stays there while every program of the deployment carries one count.
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
	r.roleWatcher = r.newRoleWatcher(roleKV)
	// No eager load: the watchers populate the caches asynchronously. A synchronous load of N
	// individual kv.Get() calls blocks indefinitely when the KV stream leader is on a remote node.
	r.watcher.Start()
	r.roleWatcher.Start()
	return r, nil
}

// newRoleWatcher returns the watcher that feeds the holder view from kv. It rescans the bucket
// after a first scan nats.go's idle timer ended (kvwatch.WithRescan), so a view RolesHeldBy
// refuses as incomplete becomes complete without waiting for a Rewatch.
func (r *Registry) newRoleWatcher(kv bus.KeyValue) *kvwatch.Watcher {
	return kvwatch.New("role claims", kv, r.applyRoleClaim, r.resetRoleHolders, kvwatch.WithLogger(r.log), kvwatch.WithRescan())
}

// Ping verifies both KV buckets are reachable through the current NATS connection and reports
// either watcher's terminal error, including a bucket recreated under it (kvwatch.Watcher.Check).
// /healthz and the listener monitor use failures to report an unavailable dependency while NATS
// reconnects.
func (r *Registry) Ping() error {
	if err := r.watcher.Check(); err != nil {
		return err
	}
	if err := r.roleWatcher.Check(); err != nil {
		return err
	}
	return r.WatchErr()
}

// WatchErr is the interest watcher's terminal error, else the role claim watcher's, or nil while
// both run, so the listener can tell a rebuildable dead cache from a transient KV timeout.
func (r *Registry) WatchErr() error {
	if err := r.watcher.Err(); err != nil {
		return err
	}
	return r.roleWatcher.Err()
}

// StopWatch retires both watchers for a shutdown (kvwatch.Watcher.Stop).
func (r *Registry) StopWatch() {
	r.watcher.Stop()
	r.roleWatcher.Stop()
}

// EndWatch ends both watchers at shutdown, together and within ctx, while the connection still
// carries requests (kvwatch.Watcher.End).
func (r *Registry) EndWatch(ctx context.Context) {
	var ended sync.WaitGroup
	ended.Go(func() { r.watcher.End(ctx) })
	ended.Go(func() { r.roleWatcher.End(ctx) })
	ended.Wait()
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

// Logger is the logger the registry logs through: the listener's own, which it passes to Open, so
// a route that reads the registry logs what it makes of a read there too.
func (r *Registry) Logger() *slog.Logger {
	return r.logger()
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

// deleteInterest deletes sessionID's interest at the revision it reads (bus.KeyValue.DeleteAtRead),
// each bucket call waiting no longer than ctx (bus.Bounded). A delete ctx abandoned may still land
// after this answer, and that is safe: it deletes only the revision it read, so a write that landed
// since is kept, and a key it found absent it deletes at none, as DeleteAtRead always has.
func (r *Registry) deleteInterest(ctx context.Context, sessionID string) error {
	cached := r.cachedRevision(sessionID)
	interests := r.interests()
	read, answered, err := bus.Bounded(ctx, func() (uint64, error) { return interests.DeleteAtRead(sessionID) })
	if !answered {
		return fmt.Errorf("delete %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	r.applyDelete(ctx, sessionID, max(cached, read))
	return nil
}

// deleteInterestAt deletes sessionID's interest only while revision is its latest, waiting no
// longer than ctx (bus.Bounded); a write since fails the delete with nats.ErrKeyExists. Revision 0
// names no revision, which a conditional delete cannot express, so that interest is deleted at the
// revision it reads (deleteInterest). A delete ctx abandoned may still land after this answer, and
// that is safe for the same reason: it deletes only that revision.
func (r *Registry) deleteInterestAt(ctx context.Context, sessionID string, revision uint64) error {
	if revision == 0 {
		return r.deleteInterest(ctx, sessionID)
	}
	cached := r.cachedRevision(sessionID)
	interests := r.interests()
	_, answered, err := bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, interests.Delete(sessionID, nats.LastRevision(revision))
	})
	if !answered {
		return fmt.Errorf("delete %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return err
	}
	r.applyDelete(ctx, sessionID, max(cached, revision))
	return nil
}

// applyDelete brings the cache up to a delete of sessionID's interest, the delete of a value no
// newer than revision, which the cache held or the delete read before it was sent. A history that
// does not answer within ctx falls back to the fence below: the delete itself answered.
func (r *Registry) applyDelete(ctx context.Context, sessionID string, revision uint64) {
	interests := r.interests()
	entries, _, err := bus.Bounded(ctx, func() ([]nats.KeyValueEntry, error) { return interests.History(sessionID) })
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

// Rewatch reopens both buckets on conn and replaces both caches' watchers with ones there
// (kvwatch.Watcher.Rewatch). A NATS server restart loses a watcher's ordered consumer, and
// nats.go replaces it only once it notices the missed heartbeats, up to twenty seconds later;
// until then a cache misses every write another listener makes, and a drain deletes a consumer
// the server no longer has. The listener's reconnect hook calls this, so the caches follow their
// buckets from the reconnect on. A failure to open the role bucket or to rewatch the interest
// watcher moves nothing: both watchers and both handles stay where they were. The role claim
// watcher is rewatched last, once the interest watcher and the role handle have moved, and its
// failure is returned with the role claim view left on its previous watcher.
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
	return r.roleWatcher.Rewatch(conn)
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

// resetRoleHolders empties the role holder view and its revision fence, for a recreated role
// bucket.
func (r *Registry) resetRoleHolders() {
	r.holdersMu.Lock()
	r.holders = map[string]string{}
	r.holderRevisions = map[string]uint64{}
	r.holderEpoch++
	r.holdersMu.Unlock()
}

// roleHolderEpoch is the holder view's epoch: a claim write this registry makes reads it before
// the write and applies its result only if no reset has come between (setOwnRoleHolder).
func (r *Registry) roleHolderEpoch() uint64 {
	r.holdersMu.RLock()
	defer r.holdersMu.RUnlock()
	return r.holderEpoch
}

// applyRoleClaim applies one entry the role claim watcher delivered to the holder view. A claim
// this build cannot decode (ErrMalformedRoleClaim) names no holder: it gives its role to nobody,
// and the registry logs it at WARN once, as the interest watcher logs a value it cannot decode.
func (r *Registry) applyRoleClaim(entry nats.KeyValueEntry) {
	holder := ""
	if entry.Operation() != nats.KeyValueDelete && entry.Operation() != nats.KeyValuePurge {
		claim, err := decodeRoleClaim(entry.Value())
		if err != nil {
			r.logger().Warn("role claim watcher found a claim it cannot read; it names no holder",
				slog.String("role", entry.Key()),
				slog.Uint64("revision", entry.Revision()),
				slog.String("error", err.Error()),
			)
		}
		holder = claim.HolderSessionID
	}
	r.setRoleHolder(entry.Key(), holder, entry.Revision())
}

// setRoleHolder records holder, or no holder when it is empty, as role's at revision, unless a
// later revision of role is already applied.
func (r *Registry) setRoleHolder(role, holder string, revision uint64) {
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.setRoleHolderLocked(role, holder, revision)
}

// setOwnRoleHolder is setRoleHolder for a claim write or delete this registry made, which read
// epoch (roleHolderEpoch) before the write: a reset since then means the write's revision may be
// the old bucket's, so the result is left to the watcher of the new bucket.
func (r *Registry) setOwnRoleHolder(role, holder string, revision, epoch uint64) {
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	if epoch != r.holderEpoch {
		return
	}
	r.setRoleHolderLocked(role, holder, revision)
}

func (r *Registry) setRoleHolderLocked(role, holder string, revision uint64) {
	if revision < r.holderRevisions[role] {
		return
	}
	if r.holders == nil {
		r.holders = map[string]string{}
	}
	if r.holderRevisions == nil {
		r.holderRevisions = map[string]uint64{}
	}
	if holder == "" {
		delete(r.holders, role)
	} else {
		r.holders[role] = holder
	}
	r.holderRevisions[role] = revision
}

// ErrRoleViewNotReady is RolesHeldBy's answer when its view of the role claims is not complete, so
// it cannot say which roles a session holds: its watcher's first scan has not delivered every claim
// (the scan is still running, nats.go's idle timer ended it, or a recreated bucket's reset emptied
// the view and the refill is still running), the watcher has ended and not been replaced, or no
// watcher follows the claims. It is a read that did not answer, never the answer that a session
// holds no roles.
var ErrRoleViewNotReady = errors.New("the role claim view is not complete")

// RolesHeldBy returns the roles whose claims name sessionID, sorted, from the role claim
// watcher's view of the bucket, so it makes no bucket call. A session holds a role exactly when
// the role's claim names it; its interest record's topics say nothing about that. It answers only
// while the view is complete (kvwatch.Watcher.Complete): the current watcher's first scan
// delivered every claim, after any reset, and the watcher still runs. Otherwise it returns
// ErrRoleViewNotReady, wrapped, with the reason roleViewGeneration gives, the one test of the
// view's state, which RoleViewErr makes too. A first scan the idle timer ended is scanned again
// (newRoleWatcher), so the view becomes complete without a Rewatch.
//
// The view is read between two readings of the watcher's generation, and answers only when the
// first said complete and both name the same generation: a reset empties the view only under a new
// generation, so the read cannot have met a reset or the partial refill after it. Within one
// generation the view changes only by revision-fenced updates (setRoleHolder), each of which keeps
// every claim present at a revision no older than the scan's, so the answer is no older than
// that complete scan.
func (r *Registry) RolesHeldBy(sessionID string) ([]string, error) {
	generation, err := r.roleViewGeneration()
	if err != nil {
		return nil, err
	}
	roles := []string{}
	r.holdersMu.RLock()
	for role, holder := range r.holders {
		if holder == sessionID {
			roles = append(roles, role)
		}
	}
	r.holdersMu.RUnlock()
	if again, _ := r.roleWatcher.Complete(); again != generation {
		return nil, fmt.Errorf("%w: its watcher was replaced while it was read", ErrRoleViewNotReady)
	}
	sort.Strings(roles)
	return roles, nil
}

// roleViewGeneration is the one test of whether the role claim view can answer a read: nil and the
// generation of the watcher the answer would be read under, or the wrapped ErrRoleViewNotReady
// naming why no read is answered. RolesHeldBy and RoleViewErr both come here, so what a route
// reads and what /healthz reports of the view are the same state, read the same way, and cannot
// disagree. It makes no bucket call.
func (r *Registry) roleViewGeneration() (uint64, error) {
	if r.roleWatcher == nil {
		return 0, fmt.Errorf("%w: no watcher follows the role claims", ErrRoleViewNotReady)
	}
	if err := r.roleWatcher.Err(); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrRoleViewNotReady, err)
	}
	generation, complete := r.roleWatcher.Complete()
	if !complete {
		return 0, fmt.Errorf("%w: its watcher has not delivered every claim", ErrRoleViewNotReady)
	}
	return generation, nil
}

// RoleViewErr is nil while the role claim view would answer a read, and otherwise the wrapped
// ErrRoleViewNotReady that RolesHeldBy refuses that read with, naming the same reason: both read
// the view's state through roleViewGeneration, so a report that the view answers and a read it
// refuses can never disagree. It makes no bucket call, so a caller reports the view without
// waiting on the bus.
//
// /healthz reports it as a degraded dependency, and it is deliberately not part of Ping or
// WatchErr: those feed the listener's self-health monitor, which restarts the listener over a
// terminal failure that survives its recovery attempts, and no rebuild completes a scan the
// bucket keeps timing out on. What does complete it is the role claim watcher's own rescan
// (newRoleWatcher), which needs nothing from the listener.
func (r *Registry) RoleViewErr() error {
	_, err := r.roleViewGeneration()
	return err
}

// WaitForCacheReady blocks until both views answer, or until ctx is done. The interest watcher is
// waited for as ready (kvwatch.Watcher.WaitReady), having ended its first scan or failed to
// start, and the role claim watcher as complete (kvwatch.Watcher.WaitComplete): the interest view
// answers from whatever its cache holds, so a partial scan still serves, while the role claim
// view refuses every read until its scan has delivered every claim (ErrRoleViewNotReady), so
// readiness alone would let a caller serve a view that answers nothing. After it returns nil,
// Match sees every existing subscription in the bucket and RolesHeldBy answers. A role claim scan
// nats.go's idle timer ended releases readiness without completing, and the role claim watcher
// scans the bucket again after it (newRoleWatcher's kvwatch.WithRescan), so this wait ends on
// that rescan rather than hanging. Callers bound ctx: a watcher that fails to start releases it
// too, and so does one that can complete no scan at all, through its terminal error or its stop;
// the caller then serves the write-through path while /healthz exposes the unavailable registry.
// Its error names the cache that did not answer.
func (r *Registry) WaitForCacheReady(ctx context.Context) error {
	if err := r.watcher.WaitReady(ctx); err != nil {
		return fmt.Errorf("interest cache: %w", err)
	}
	if err := r.roleWatcher.WaitComplete(ctx); err != nil {
		return fmt.Errorf("role claim cache: %w", err)
	}
	return nil
}

// Upsert is UpsertWithin with no deadline of its own, for a caller that answers no request.
func (r *Registry) Upsert(item Interest, topics []string) (Interest, error) {
	return r.UpsertWithin(context.Background(), item, topics)
}

// UpsertWithin adds topics to item.SessionID's interest, merged onto the stored one
// (mergeForUpsert), stamped with the registry's clock and written by compare-and-set
// (writeInterest), each bucket call waiting no longer than ctx (bus.Bounded): an HTTP route passes
// its request's own context. It returns the record as it now stands. A record this build cannot
// decode (ErrMalformedInterest) is replaced by item and topics alone, and the registry logs that at
// WARN naming the session. A write ctx abandoned may still land after this answer. That is safe: it
// is a compare-and-set at the revision it read, so it replaces only that revision and never a write
// that landed since, and it writes the record the caller's retry merges again.
//
// A heartbeat re-sends the same topics every 90 seconds, so UpsertWithin returns the current row
// without writing when the encoding of merged equals that of the row it holds, apart from
// UpdatedAt, and the row is younger than InterestRefreshAge. An unchanged row is thus rewritten
// once per InterestRefreshAge, its UpdatedAt lags the session's last heartbeat by less than that,
// and Reap adds InterestRefreshAge to its grace window (InterestReapAllowance), so that lag never
// shortens a paused session's grace. A changed row is written, and so is a record it cannot
// decode; any other failed read refuses the write (mergeForUpsert). The session row
// (session.SessionRegistry.Put) is written on every heartbeat: its bucket's MaxAge is the liveness
// clock, and NATS connect notices ($SYS) are not available to replace it.
func (r *Registry) UpsertWithin(ctx context.Context, item Interest, topics []string) (Interest, error) {
	var malformed bool
	merged, revision, err := r.writeInterest(ctx, item.SessionID, func(cur Interest, curErr error) (Interest, writeAction, error) {
		malformed = errors.Is(curErr, ErrMalformedInterest)
		now := r.now().UnixMilli()
		merged, err := mergeForUpsert(cur, curErr, item, topics, now)
		if err != nil {
			return Interest{}, writeStore, err
		}
		if curErr == nil && now-cur.UpdatedAt < InterestRefreshAge.Milliseconds() {
			same, err := storesTheSameInterest(cur, merged)
			if err != nil {
				return Interest{}, writeStore, err
			}
			if same {
				return cur, writeSkip, nil
			}
		}
		return merged, writeStore, nil
	})
	if err != nil {
		return Interest{}, err
	}
	if malformed {
		r.logger().Warn("registry replaced an interest record it cannot decode",
			slog.String("session_id", merged.SessionID),
			slog.Uint64("revision", revision),
			slog.Any("topics", merged.Topics),
		)
	}
	return merged, nil
}

// storesTheSameInterest reports whether writing next would store the row cur already holds. What
// the skip asks is whether the write would change the bucket, so it compares the encoding of next
// with that of cur rather than the fields one by one: a field added to Interest, a key only a newer
// build knows (Interest.unknown), and the difference between a null and an empty topic list are
// then covered by the encoding itself. cur is stamped with next's UpdatedAt, the one field a write
// of an unchanged row would move.
func storesTheSameInterest(cur, next Interest) (bool, error) {
	cur.UpdatedAt = next.UpdatedAt
	stored, err := encodeInterest(cur)
	if err != nil {
		return false, err
	}
	buf, err := encodeInterest(next)
	if err != nil {
		return false, err
	}
	return bytes.Equal(buf, stored), nil
}

// writeAction is what writeInterest does with what its change returned: store it, delete the
// stored interest, or leave the bucket as it is.
type writeAction uint8

const (
	writeStore writeAction = iota
	writeRemove
	writeSkip
)

// writeInterest writes sessionID's interest by compare-and-set on its KV revision, so a write another
// listener made between this one's read and its write is never overwritten. change computes the
// interest to write from the stored one and the error reading it (nats.ErrKeyNotFound when there is
// none, ErrMalformedInterest for a record this build cannot decode), or asks for the stored one to be
// deleted (writeRemove) or left alone (writeSkip, for a write that would store the row the bucket
// already holds). The first attempt changes the cached interest. A cache miss, or a write the bucket
// refuses because the revision moved, reads the bucket entry and changes that: an Update or a delete
// at its revision, or a Create when there is none. After writeAttempts refused writes it returns
// ErrWriteConflict, having written nothing. Each bucket call waits no longer than ctx (bus.Bounded).
// It returns the interest written and the revision written, 0 for a delete or for nothing to delete,
// and for a skip the revision the interest stands at.
func (r *Registry) writeInterest(ctx context.Context, sessionID string, change func(cur Interest, curErr error) (next Interest, action writeAction, err error)) (Interest, uint64, error) {
	r.mu.RLock()
	cur, found := r.cache[sessionID]
	revision := r.cacheRevisions[sessionID]
	r.mu.RUnlock()
	var curErr error
	read := !found
	for range writeAttempts {
		if read {
			cur, revision, curErr = r.readInterest(ctx, sessionID)
		}
		read = true
		next, action, err := change(cur, curErr)
		if err != nil {
			return Interest{}, 0, err
		}
		var written uint64
		switch {
		case action == writeSkip:
			return next, revision, nil
		case action == writeRemove && revision == 0:
			return next, 0, nil
		case action == writeRemove:
			err = r.deleteInterestAt(ctx, sessionID, revision)
		default:
			written, err = r.putInterestAt(ctx, sessionID, next, revision)
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		if err != nil {
			return Interest{}, 0, err
		}
		return next, written, nil
	}
	return Interest{}, 0, fmt.Errorf("%w: %s", ErrWriteConflict, sessionID)
}

// putInterestAt writes item as sessionID's interest while revision is its latest (0: while it has
// none), waiting no longer than ctx (bus.Bounded), applies the write to the cache, and returns the
// revision written.
func (r *Registry) putInterestAt(ctx context.Context, sessionID string, item Interest, revision uint64) (uint64, error) {
	buf, err := encodeInterest(item)
	if err != nil {
		return 0, err
	}
	interests := r.interests()
	written, answered, err := bus.Bounded(ctx, func() (uint64, error) {
		if revision == 0 {
			return interests.Create(sessionID, buf)
		}
		return interests.Update(sessionID, buf, revision)
	})
	if !answered {
		return 0, fmt.Errorf("write %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.cacheInterestLocked(sessionID, item, written)
	r.mu.Unlock()
	return written, nil
}

// readInterest reads sessionID's interest from the bucket, waiting no longer than ctx (bus.Bounded),
// applies it to the cache, and returns it with its revision. A key with no value is
// nats.ErrKeyNotFound at revision 0. A value that does not decode is ErrMalformedInterest at its
// revision, logged at WARN with the session it belongs to.
func (r *Registry) readInterest(ctx context.Context, sessionID string) (Interest, uint64, error) {
	interests := r.interests()
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return interests.Get(sessionID) })
	if !answered {
		return Interest{}, 0, fmt.Errorf("read %s's interest record: %w", sessionID, err)
	}
	if err != nil {
		return Interest{}, 0, err
	}
	item, err := decodeInterest(entry.Value())
	if err != nil {
		r.logger().Warn("registry read an interest record it cannot decode",
			slog.String("session_id", sessionID),
			slog.Uint64("revision", entry.Revision()),
			slog.String("error", err.Error()),
		)
		return Interest{}, entry.Revision(), fmt.Errorf("%w: %w", ErrMalformedInterest, err)
	}
	r.mu.Lock()
	r.cacheInterestLocked(sessionID, item, entry.Revision())
	r.mu.Unlock()
	return item, entry.Revision(), nil
}

// mergeForUpsert computes the Interest to persist by reconciling the requested
// item/topics with the current cached/durable state returned by r.Get.
//
// Critical invariant: a transient KV error (anything other than ErrKeyNotFound or ErrMalformedInterest)
// MUST be returned so the caller refuses to overwrite durable state with the
// partial heartbeat payload. Silently treating transient errors as "no prior
// state" is what caused Atlas's pr.11416.> subscription to disappear: when r.Get
// failed transiently during a 2-minute heartbeat, the original implementation
// merged only the agent topic and Put a truncated state to KV, which then
// propagated to the in-memory cache via watch() and made all subsequent github
// events fall through registry.Match with "no matching interests".
//
// ErrKeyNotFound is the genuinely-new-session case — proceed with the passed-in
// item and topics. ErrMalformedInterest is merged the same way: the record answered, so this is
// not the transient case, its topics are ones this build never routed (the cache holds no record
// it cannot decode), and the session's own subscribe is the authority on its record.
//
// The merged row keeps no topic reaching a private stream subject other than the
// session's own agent subject (dropPrivateStreamTopics): a row stored before the
// listener refused such a topic is cleaned at the session's next registration.
func mergeForUpsert(cur Interest, getErr error, item Interest, topics []string, now int64) (Interest, error) {
	if getErr != nil && !errors.Is(getErr, nats.ErrKeyNotFound) && !errors.Is(getErr, ErrMalformedInterest) {
		return Interest{}, getErr
	}
	sessionID := item.SessionID
	if getErr == nil {
		item = cur
	}
	item.MachineID = first(item.MachineID, curValue(cur.MachineID))
	item.Dir = first(item.Dir, curValue(cur.Dir))
	item.UpdatedAt = now
	item = Merge(item, topics)
	item.Topics = dropPrivateStreamTopics(sessionID, item.Topics)
	sort.Strings(item.Topics)
	return item, nil
}

// ErrMalformedRoleClaim is a role claim record the bucket holds and this build cannot read: one
// that names no holder, or JSON that is not a claim. The bus answered it, so it is not a read that
// failed, and reading it again returns the same record; each error wrapping it names which.
var ErrMalformedRoleClaim = errors.New("malformed role claim")

// decodeRoleClaim reads a stored claim: the fields RoleClaim names, every other key into its unknown
// map, or a bare session id an earlier build stored.
func decodeRoleClaim(value []byte) (RoleClaim, error) {
	var claim RoleClaim
	unknown, err := DecodeKeeping(value, &claim, roleClaimKeys)
	if err == nil {
		if strings.TrimSpace(claim.HolderSessionID) == "" {
			return RoleClaim{}, fmt.Errorf("%w: it names no holder", ErrMalformedRoleClaim)
		}
		claim.unknown = unknown
		return claim, nil
	}
	if json.Valid(value) {
		return RoleClaim{}, fmt.Errorf("%w: %w", ErrMalformedRoleClaim, err)
	}
	// Accept bare session-ID records so persisted claims remain routable;
	// a later claim normalizes the row to the structured format.
	holder := strings.TrimSpace(string(value))
	if holder == "" {
		return RoleClaim{}, fmt.Errorf("%w: a legacy record with an empty holder", ErrMalformedRoleClaim)
	}
	return RoleClaim{HolderSessionID: holder}, nil
}

// roleClaim reads role's claim from the bucket, waiting no longer than ctx (bus.Bounded).
func (r *Registry) roleClaim(ctx context.Context, role string) (RoleClaim, nats.KeyValueEntry, error) {
	roles := r.roles()
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return roles.Get(role) })
	if !answered {
		return RoleClaim{}, nil, fmt.Errorf("read role %s's claim: %w", role, err)
	}
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

func (r *Registry) releaseRoleClaims(ctx context.Context, sessionID string, topics []string) error {
	for _, topic := range topics {
		if !strings.HasPrefix(topic, contracts.RoleTopicPrefix) {
			continue
		}
		role := strings.TrimPrefix(topic, contracts.RoleTopicPrefix)
		if role == "" || strings.ContainsAny(role, "*>") {
			continue
		}
		if err := r.releaseRoleClaim(ctx, sessionID, role); err != nil {
			return err
		}
	}
	return nil
}

// releaseRoleClaim deletes role's claim when sessionID holds it, its read and its conditional delete
// each waiting no longer than ctx (bus.Bounded). A delete ctx abandoned may still land after this
// answer, and that is safe: it deletes only the revision it read, sessionID's claim, which the
// caller asked to release, and a claim written since is kept. A claim stored under a key NATS
// refuses (bus.ErrRefused: too long, or outside its key alphabet) has a holder nothing can tell, so
// it is left to the role reaper, which deletes it, or skips one no KV call can delete.
func (r *Registry) releaseRoleClaim(ctx context.Context, sessionID, role string) error {
	claim, entry, err := r.roleClaim(ctx, role)
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
	epoch := r.roleHolderEpoch()
	roles := r.roles()
	_, answered, err := bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, roles.Delete(role, nats.LastRevision(entry.Revision()))
	})
	if !answered {
		return fmt.Errorf("release role %s's claim held by %s: %w", role, sessionID, err)
	}
	if err != nil && !errors.Is(err, nats.ErrKeyExists) && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	if err == nil {
		r.forgetRoleHolder(role, entry.Revision(), epoch)
	}
	return nil
}

// forgetRoleHolder records that the claim of role at revision was deleted. A delete does not
// return its marker's revision, so the view is fenced one past the revision deleted, as
// deleteInterest fences the interest cache: a claim written since is later still, and applies.
func (r *Registry) forgetRoleHolder(role string, revision, epoch uint64) {
	r.setOwnRoleHolder(role, "", revision+1, epoch)
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
// a claim. A failure is reported retained, its error naming the release.
//
// The claim's read and its conditional delete each wait no longer than ctx (bus.Bounded): an HTTP
// route passes its request's own context, role delivery and the reaper context.Background(). A
// delete ctx abandoned may still land after this answer. That is safe to leave running: it deletes
// the claim only if the bucket still holds the very revision it read, naming sessionID, whom the
// caller has just found gone from the session bucket, so at worst a lapsed holder's claim is
// released a moment after the caller was told the bus did not answer, and a fresh claim that
// landed meanwhile is never touched.
func (r *Registry) ReleaseExpiredRoleClaim(ctx context.Context, role, sessionID string, sessionTTL time.Duration) (ExpiredRoleClaimRelease, error) {
	claim, entry, err := r.roleClaim(ctx, role)
	if err != nil {
		return ExpiredRoleClaimRetained, fmt.Errorf("release role %s's expired claim held by %s: %w", role, sessionID, err)
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
	epoch := r.roleHolderEpoch()
	roles := r.roles()
	_, _, err = bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, roles.Delete(role, nats.LastRevision(entry.Revision()))
	})
	switch {
	case err == nil:
		r.forgetRoleHolder(role, entry.Revision(), epoch)
		return ExpiredRoleClaimReleased, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return ExpiredRoleClaimMissing, nil
	case errors.Is(err, nats.ErrKeyExists):
		return ExpiredRoleClaimSuperseded, nil
	default:
		return ExpiredRoleClaimRetained, fmt.Errorf("release role %s's expired claim held by %s: %w", role, sessionID, err)
	}
}

func (r *Registry) releaseAllRoleClaims(ctx context.Context, sessionID string) error {
	rolesKV := r.roles()
	roles, answered, err := bus.Bounded(ctx, func() ([]string, error) { return rolesKV.Keys() })
	if !answered {
		return fmt.Errorf("list the role claims to release %s's: %w", sessionID, err)
	}
	if errors.Is(err, nats.ErrNoKeysFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, role := range roles {
		if err := r.releaseRoleClaim(ctx, sessionID, role); err != nil {
			return err
		}
	}
	return nil
}

// Remove releases sessionID's role claims and removes topics from its interest record, or deletes
// the record when topics is empty, each bucket call waiting no longer than ctx (bus.Bounded): an
// HTTP route passes its request's own context. A write or delete ctx abandoned may still land after
// this answer; releaseRoleClaim, deleteInterest and removeInterestTopics each say why that is safe.
// A record this build cannot decode (ErrMalformedInterest) is deleted all the same; removing some
// topics leaves it as it is (removeInterestTopics).
func (r *Registry) Remove(ctx context.Context, sessionID string, topics []string) error {
	if len(topics) == 0 {
		item, err := r.Get(ctx, sessionID)
		if err == nil {
			if err := r.releaseRoleClaims(ctx, sessionID, item.Topics); err != nil {
				return err
			}
		} else if !errors.Is(err, nats.ErrKeyNotFound) && !errors.Is(err, ErrMalformedInterest) {
			return err
		}
		if err := r.releaseAllRoleClaims(ctx, sessionID); err != nil {
			return err
		}
	} else if err := r.releaseRoleClaims(ctx, sessionID, topics); err != nil {
		return err
	}
	return r.removeInterestTopics(ctx, sessionID, topics)
}

// removeInterestTopics removes topics from sessionID's stored interest by compare-and-set
// (writeInterest), deleting the interest when none is left; no topics removes it whole. Each bucket
// call waits no longer than ctx; a write ctx abandoned may still land after this answer, and is safe
// for the reasons UpsertWithin's is. A record this build cannot decode routes none of its topics
// here, so there is nothing to remove from it: it is left for the session's next subscribe to
// replace or a delete to remove, and the registry logs that at WARN naming the session.
func (r *Registry) removeInterestTopics(ctx context.Context, sessionID string, topics []string) error {
	// Empty topics = unsubscribe from everything (delete the entry).
	if len(topics) == 0 {
		return r.deleteInterest(ctx, sessionID)
	}
	_, _, err := r.writeInterest(ctx, sessionID, func(cur Interest, curErr error) (Interest, writeAction, error) {
		if curErr != nil {
			return Interest{}, writeStore, curErr
		}
		item := Remove(cur, topics)
		item.Topics = dropPrivateStreamTopics(sessionID, item.Topics)
		if len(item.Topics) == 0 {
			return item, writeRemove, nil
		}
		item.UpdatedAt = r.now().UnixMilli()
		return item, writeStore, nil
	})
	if errors.Is(err, ErrMalformedInterest) {
		r.logger().Warn("registry left an interest record it cannot decode unchanged",
			slog.String("session_id", sessionID),
			slog.Any("topics", topics),
		)
		return nil
	}
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
	return r.SetRoleWithPrevious(context.Background(), sessionID, machineID, role, "", soft, supersedable...)
}

// SetRoleWithPrevious records the session ID that a successful soft claim
// continues. The predecessor is provenance only after the listener has
// evaluated the existing soft-claim rules.
//
// Each bucket call waits no longer than ctx (bus.Bounded): POST /v1/roles/set passes its request's
// own context. A call ctx abandoned may still land after this answer. The claim's create or update
// is conditional on the revision the claim was read at, so it lands only if no other claim did
// since, and makes sessionID the holder it asked to be; the interest write is UpsertWithin's.
//
// The claim alone says who holds the role (RolesHeldBy, and role delivery), so the two cleanups
// that follow are best-effort tidying of interest topics, within ctx like the rest: the rollback of
// sessionID's topic when its claim did not land, and the removal of the role's topic from the old
// holder's record once it did. A topic either leaves behind gives nobody the role. A claim that
// landed is a success whatever its old holder's cleanup did: one that failed is logged at WARN, and
// one ctx ended before it finished at INFO, since nothing failed there.
func (r *Registry) SetRoleWithPrevious(ctx context.Context, sessionID, machineID, role, previousSessionID string, soft bool, supersedable ...string) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + role
	oldClaim, entry, err := r.roleClaim(ctx, role)
	if err != nil {
		return Interest{}, err
	}

	oldSessionID := oldClaim.HolderSessionID
	if soft && oldSessionID != "" && oldSessionID != sessionID && !slices.Contains(supersedable, oldSessionID) {
		return Interest{}, &ErrRoleHeld{Role: role, Holder: oldSessionID}
	}

	item, err := r.UpsertWithin(ctx, Interest{SessionID: sessionID, MachineID: machineID}, []string{roleTopic})
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
	data, err := EncodeKeeping(claim, claim.unknown)
	if err != nil {
		return Interest{}, err
	}
	epoch := r.roleHolderEpoch()
	roles := r.roles()
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) {
		if entry == nil {
			return roles.Create(role, data)
		}
		return roles.Update(role, data, entry.Revision())
	})
	if !answered {
		err = fmt.Errorf("write role %s's claim: %w", role, err)
	}
	if err != nil {
		current, holderErr := r.RoleClaim(ctx, role)
		holder := current.HolderSessionID
		if holderErr == nil && holder == sessionID {
			return item, nil
		}
		if rollbackErr := r.removeInterestTopics(ctx, sessionID, []string{roleTopic}); rollbackErr != nil {
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
	r.setOwnRoleHolder(role, sessionID, revision, epoch)

	if oldSessionID != "" && oldSessionID != sessionID {
		err := r.removeInterestTopics(ctx, oldSessionID, []string{roleTopic})
		switch {
		case err == nil, errors.Is(err, nats.ErrKeyNotFound):
		case errors.Is(err, bus.ErrRefused):
			// The old holder's id is a key NATS refuses (too long, or outside its key alphabet): an
			// earlier build registered it, or a stored claim names it. The interest reaper removes
			// the old holder's interest once its session is gone, or skips one no KV call can delete.
			r.logger().Warn("registry role claim left an old holder's interest it cannot write to the reaper",
				slog.String("role", role),
				slog.Int("key_bytes", len(oldSessionID)),
				slog.String("error", err.Error()),
			)
		case ctx.Err() != nil:
			r.logger().Info("registry role claim left its old holder's topic: the request ended first",
				slog.String("role", role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", sessionID),
			)
		default:
			r.logger().Warn("registry role claim old holder cleanup failed",
				slog.String("role", role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", sessionID),
				slog.String("error", err.Error()),
			)
		}
	}
	return item, nil
}

// RoleClaim returns the durable claim for role, read from the bucket within ctx (bus.Bounded). An
// unclaimed role returns a zero-value claim without an error; a record that is not a claim,
// ErrMalformedRoleClaim.
func (r *Registry) RoleClaim(ctx context.Context, role string) (RoleClaim, error) {
	claim, _, err := r.roleClaim(ctx, role)
	return claim, err
}

// RoleHolder returns the authoritative session ID for role, read with no deadline of its own. Only
// tests call it, to read what a claim left behind; every route and the role lane read RoleClaim
// within a deadline (resolveRoleHolder, cmd/listener/role_holder.go). An unclaimed role returns an
// empty holder without an error.
func (r *Registry) RoleHolder(role string) (string, error) {
	claim, err := r.RoleClaim(context.Background(), role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
}

// ErrMalformedInterest is an interest record the bucket holds and this build cannot decode. The bus
// answered it, so it is not a read that failed, and reading it again returns the same record; the
// error wrapping it carries the decode failure.
var ErrMalformedInterest = errors.New("malformed interest record")

// Get returns the Interest for a session. Cache first, direct KV read on miss.
// The KV fallback also repopulates the cache, bridging the warm-up window after
// Open() before watch() has fully populated the cache. A cache hit answers in the
// caller's goroutine; only the bucket read on a miss waits on ctx (bus.Bounded). The cache holds only
// records it decoded (applyWatched evicts the rest), so only the bucket read can meet a record this
// build cannot decode: that is ErrMalformedInterest, logged at WARN with the session it belongs to.
func (r *Registry) Get(ctx context.Context, sessionID string) (Interest, error) {
	r.mu.RLock()
	item, ok := r.cache[sessionID]
	r.mu.RUnlock()
	if ok {
		return item, nil
	}
	item, _, err := r.readInterest(ctx, sessionID)
	return item, err
}

// Match returns the cached interests registered on machineID whose topics match topic: a
// listener delivers live only to the sessions of its own machine.
func (r *Registry) Match(machineID string, topic string) []Interest {
	return r.match(topic, func(item Interest) bool { return item.MachineID == machineID })
}

// MatchAll returns every cached interest whose topics match topic, on any machine, sorted by
// SessionID for deterministic output: a sender that must confirm delivery to each subscribed
// session needs them all, wherever each session registered.
func (r *Registry) MatchAll(topic string) []Interest {
	out := r.match(topic, func(Interest) bool { return true })
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// match returns the cached interests keep accepts that have a topic pattern matching topic.
func (r *Registry) match(topic string, keep func(Interest) bool) []Interest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Interest{}
	for _, item := range r.cache {
		if !keep(item) {
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

const (
	// InterestReapGrace is the grace window the listener passes to StartReaper: a dead
	// session's interest stays at least this long after its row's last write, plus
	// InterestRefreshAge.
	InterestReapGrace = 10 * time.Minute
	// InterestRefreshAge is the most an interest row's UpdatedAt may lag its session's last
	// heartbeat: a writer may skip rewriting an unchanged row younger than this, and must
	// rewrite it once it reaches this age. Reap adds it to the grace window, so a skipped
	// rewrite never shortens the grace a paused session gets after its last heartbeat.
	InterestRefreshAge = 5 * time.Minute
)

// InterestReapAllowance is the age past which Reap deletes a dead session's interest: grace, the
// window the row's session is given after its last write, plus InterestRefreshAge, the most that
// write may lag the session's last heartbeat. A row exactly that old is kept. The listener gives
// the same allowance to the last-seen record its session cache keeps
// (session.WithLastSeenRetention), so every row the reaper can still hold has one.
func InterestReapAllowance(grace time.Duration) time.Duration {
	return grace + InterestRefreshAge
}

// reapCandidate is one cached interest past the allowance as Reap's snapshot saw it.
type reapCandidate struct {
	sessionID string
	revision  uint64
}

// Reap cross-references the interest cache with a session liveness check and
// deletes an interest only when its session is dead AND more than the grace
// window plus InterestRefreshAge has passed since the row's last write
// (UpdatedAt, InterestReapAllowance); a row exactly that old is kept. A role
// claim is intentionally not an interest: it remains available for its holder
// to re-register after a listener restart, and the role delivery path drops it
// if that holder never becomes live again.
//
// Under the read lock it copies the session id and cache revision of every cached
// interest past that allowance, and then releases it, so isAlive runs with no interest
// lock held and a heartbeat's Upsert never waits on a pass. Before it deletes a
// candidate it reads it again and skips it when its revision changed or it is now
// within the allowance, and it deletes at exactly the revision it checked, so a write
// that lands after that check is kept: the delete fails with JetStream's wrong last
// sequence (10071, nats.ErrKeyExists), and the candidate counts as skipped.
func (r *Registry) Reap(isAlive func(string) bool, graceWindow time.Duration) (int, error) {
	now := r.now().UnixMilli()
	allowanceMs := InterestReapAllowance(graceWindow).Milliseconds()

	r.mu.RLock()
	var candidates []reapCandidate
	for sid, item := range r.cache {
		if now-item.UpdatedAt <= allowanceMs {
			continue // within grace window plus refresh age
		}
		candidates = append(candidates, reapCandidate{sessionID: sid, revision: r.cacheRevisions[sid]})
	}
	r.mu.RUnlock()

	reaped := 0
	for _, candidate := range candidates {
		sid := candidate.sessionID
		if isAlive(sid) {
			continue // session still alive
		}
		r.mu.RLock()
		item, cached := r.cache[sid]
		revision := r.cacheRevisions[sid]
		r.mu.RUnlock()
		if !cached || revision != candidate.revision || now-item.UpdatedAt <= allowanceMs {
			continue // written or removed during the pass
		}
		err := r.deleteInterestAt(context.Background(), sid, revision)
		if errors.Is(err, nats.ErrKeyExists) {
			continue // written since the check; the write keeps the interest
		}
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
		claim, err := r.RoleClaim(context.Background(), role)
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
		release, err := r.ReleaseExpiredRoleClaim(context.Background(), role, claim.HolderSessionID, sessionTTL)
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
// a delete). Each pass logs how many interests it reaped and how long it took (elapsed_ms).
func (r *Registry) StartReaper(ctx context.Context, isAlive func(string) bool, interval, graceWindow time.Duration) <-chan struct{} {
	return everyInterval(ctx, interval, func() {
		started := time.Now()
		count, err := r.Reap(isAlive, graceWindow)
		if err != nil {
			r.logger().Error("reaper cycle failed", slog.String("error", err.Error()))
			return
		}
		r.logger().Info("reaper cycle", slog.Int("reaped", count), slog.Int64("elapsed_ms", time.Since(started).Milliseconds()))
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
