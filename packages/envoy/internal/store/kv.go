package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// writeAttempts bounds the compare-and-set attempts of one interest write (Registry.writeInterest).
const writeAttempts = 5

// ErrWriteConflict is an interest write whose compare-and-set lost to another writer on each of its
// writeAttempts: every time it wrote, the stored revision had moved since it read. Nothing was
// written; the caller may retry, and the listener answers it as a 503 with Retry-After.
var ErrWriteConflict = errors.New("the interest changed under every write attempt")

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
	mu           sync.RWMutex
	// cache is the copy of the interest bucket: its interests, revision fence, marks and read epoch
	// (kvwatch.Copy, newInterestCopy), which every write site reaches through.
	cache kvwatch.Copy[Interest]
	// index indexes the cached interests' topic patterns by subject token (subjectIndex), which is
	// what Match and MatchAll walk rather than every cached interest. The copy's own hooks keep it
	// in step (indexInterestLocked, unindexInterestLocked), so a recreated bucket's read takes its
	// departed sessions out of it at the sweep that removes their rows.
	index *subjectIndex
	// watcher feeds the cache from the interest bucket. Every watcher it installs reads the whole
	// bucket again (interestCache): the cache keeps every interest through the read and removes
	// the ones the read found no evidence of only once it completes. Once a read has completed the
	// cache is consistent with the bucket, and Match answers every "is this session subscribed?"
	// question without falling through.
	watcher *kvwatch.Watcher
	// roleWatcher feeds roleHolders from the role bucket, so a session's roles are read from the
	// claims with no bucket call (RolesHeldBy), which answers only while its copy can be served
	// (kvwatch.Watcher.Servable), and a role's holder is too (CachedRoleHolder). holdersMu guards
	// roleHolders, the copy of the role bucket: the session each role's claim names, its revision
	// fence, which keeps a delayed watcher record from undoing a claim this registry wrote or read,
	// its marks and its read epoch (kvwatch.Copy, newRoleHolderCopy), so a claim write this registry
	// made through a handle on a bucket since recreated cannot land in the new bucket's view and
	// fence out its lower revisions.
	roleWatcher *kvwatch.Watcher
	holdersMu   sync.RWMutex
	roleHolders kvwatch.Copy[string]
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
	kv, err := bus.EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: opts.replicas, Storage: nats.FileStorage})
	if err != nil {
		return nil, err
	}
	roleKV, err := bus.EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: opts.replicas, Storage: nats.FileStorage})
	if err != nil {
		return nil, err
	}
	r := &Registry{
		roleKV:       roleKV,
		log:          opts.log,
		index:        newSubjectIndex(),
		roleHolders:  newRoleHolderCopy(),
		now:          opts.now,
		started:      opts.now(),
		serviceSince: opts.serviceSince,
	}
	r.cache = newInterestCopy(r)
	r.watcher = kvwatch.New("interest registry", kv, interestCache{r}, kvwatch.WithLogger(opts.log))
	r.roleWatcher = r.newRoleWatcher(roleKV)
	// No eager load: the watchers populate the caches asynchronously. A synchronous load of N
	// individual kv.Get() calls blocks indefinitely when the KV stream leader is on a remote node.
	r.watcher.Start()
	r.roleWatcher.Start()
	return r, nil
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

// newRoleWatcher returns the watcher that feeds the holder view from kv. A read whose scan timed
// out is read again by the watcher itself, so a view RolesHeldBy refuses becomes servable without
// waiting for a Rewatch.
func (r *Registry) newRoleWatcher(kv bus.KeyValue) *kvwatch.Watcher {
	return kvwatch.New("role claims", kv, roleCache{r}, kvwatch.WithLogger(r.log))
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

// Servable reports whether the copy List answers from can be served (kvwatch.Watcher.Servable):
// nil once a read of the interest bucket has completed and the copy follows it, or stopped
// following it no longer ago than the stale bound, and kvwatch.ErrRefilling otherwise. It reads the
// watcher's state only, never the cache. The role claim view has its own answer (RolesHeldBy).
func (r *Registry) Servable() error {
	return r.watcher.Servable()
}

// NeedsRead reports whether either copy does not follow its bucket with no read under way to make
// it, which only a Rewatch repairs (kvwatch.Watcher.NeedsRead).
func (r *Registry) NeedsRead() bool {
	return r.watcher.NeedsRead() || r.roleWatcher.NeedsRead()
}

// Disconnected says the connection both caches' watchers read through was lost
// (kvwatch.Watcher.Disconnected).
func (r *Registry) Disconnected() {
	r.watcher.Disconnected()
	r.roleWatcher.Disconnected()
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

// Rewatch reopens both buckets on conn and replaces the cache's watcher with one there
// (kvwatch.Watcher.Rewatch). A NATS server restart loses the watcher's ordered consumer, and
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
	roleKV, err := bus.OpenKeyValue(conn, js, r.roles().Bucket())
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

// CachedInterest returns sessionID's interest from the cache alone, and whether the cache holds
// one; it never reads the bucket. It is Cached, under the name the listener's durable delivery
// reads the cache by (cmd/listener/durable.go).
func (r *Registry) CachedInterest(sessionID string) (Interest, bool) {
	return r.Cached(sessionID)
}

// CachedRoleHolder returns role's holder from the role claim watcher's copy of the bucket, and on
// a miss reads the bucket as RoleHolder does, which fills the copy (setOwnRoleHolder). The copy
// holds every claim this registry wrote, read or watched, so it answers without a round trip; a
// claim another listener wrote reaches it through the watcher, a moment after the bucket. The
// listener's role lane resolves each role message's holder through it, and reads the bucket
// itself (RoleHolder) when the holder it found was superseded. An unclaimed role is a miss, which
// returns an empty holder without an error.
func (r *Registry) CachedRoleHolder(role string) (string, error) {
	r.holdersMu.RLock()
	holder, ok := r.roleHolders.Get(role)
	r.holdersMu.RUnlock()
	if ok {
		return holder, nil
	}
	// The epoch is read before the bucket, so a claim read through a handle on a bucket since
	// recreated is dropped rather than fencing out the new bucket's lower revisions.
	epoch := r.roleViewEpoch()
	claim, err := r.RoleClaim(context.Background(), role)
	if err != nil {
		return "", err
	}
	// A zero revision is a role no record claims (RoleClaim): the bucket's revisions start at one,
	// and the zero claim a miss answers carries none.
	if claim.Revision == 0 {
		return "", nil
	}
	r.setOwnRoleHolder(role, claim.HolderSessionID, claim.Revision, epoch)
	return claim.HolderSessionID, nil
}
