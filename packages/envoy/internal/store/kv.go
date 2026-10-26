package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

type Registry struct {
	// kvMu guards roleKV, which Rewatch moves to a replacement connection. The interest bucket's
	// handle is the watcher's (kvwatch.Watcher.KV).
	kvMu                  sync.RWMutex
	roleKV                bus.KeyValue
	log                   *slog.Logger
	now                   func() time.Time
	openedAt              time.Time
	restoredRoleRevisions map[string]uint64
	mu                    sync.RWMutex
	// cache is the copy of the interest bucket: its interests, revision fence, marks and read epoch
	// (kvwatch.Copy, newInterestCopy), which every write site reaches through.
	cache kvwatch.Copy[Interest]
	// watcher feeds the cache from the interest bucket. Every watcher it installs reads the whole
	// bucket again (interestCache): the cache keeps every interest through the read and removes
	// the ones the read found no evidence of only once it completes. Once a read has completed the
	// cache is consistent with the bucket, and Match answers every "is this session subscribed?"
	// question without falling through.
	watcher *kvwatch.Watcher
}

// OpenOption configures the registry.
type OpenOption func(*openOpts)

type openOpts struct {
	replicas int
	now      func() time.Time
	log      *slog.Logger
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
// staleness window, and a restored role claim's grace window, which starts when Open returns. Tests
// move it to end a window without waiting for it.
func WithClock(now func() time.Time) OpenOption {
	return func(o *openOpts) { o.now = now }
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
	restoredRoleRevisions, err := roleRevisions(roleKV, kvwatch.ScanIdleTimeout, opts.log)
	if err != nil {
		return nil, err
	}
	r := &Registry{
		roleKV:                roleKV,
		log:                   opts.log,
		cache:                 newInterestCopy(),
		now:                   opts.now,
		openedAt:              opts.now(),
		restoredRoleRevisions: restoredRoleRevisions,
	}
	r.watcher = kvwatch.New("interest registry", kv, interestCache{r}, kvwatch.WithLogger(opts.log))
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

// Servable reports whether the copy List answers from can be served (kvwatch.Watcher.Servable):
// nil once a read of the interest bucket has completed and the copy follows it, or stopped
// following it no longer ago than the stale bound, and kvwatch.ErrRefilling otherwise. It reads the
// watcher's state only, never the cache.
func (r *Registry) Servable() error {
	return r.watcher.Servable()
}

// NeedsRead reports whether the copy does not follow its bucket with no read under way to make it,
// which only a Rewatch repairs (kvwatch.Watcher.NeedsRead).
func (r *Registry) NeedsRead() bool {
	return r.watcher.NeedsRead()
}

// Disconnected says the connection the cache's watcher reads through was lost
// (kvwatch.Watcher.Disconnected).
func (r *Registry) Disconnected() {
	r.watcher.Disconnected()
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
	return nil
}
