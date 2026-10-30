package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The pools a Pool opens, by the name each one carries: its connections' application_name in
// pg_stat_activity is dispatch-<name> (configurePool), and the name is its entry in Stats and what
// Use is asked about.
const (
	// RequestsPool is the shared pool store.Open opens: what every API request reads and writes
	// through (sharedPoolSize).
	RequestsPool = "requests"
	// RoomsPool is where a document load takes its connection (Pool.Rooms).
	RoomsPool = "rooms"
	// HealthPool is the health probe's one connection (Pool.Healthy).
	HealthPool = "health"
	// ListenPool is what the LISTEN connections are dialled through (Pool.Listener).
	ListenPool = "listen"
	// LeasesPool is where a leased maintenance pass holds its lease (Pool.Leases).
	LeasesPool = "leases"
	// OutboxPool is the events outbox's one connection (Pool.Outbox).
	OutboxPool = "outbox"
	// BackgroundPool is the background loops' pool (Pool.Background).
	BackgroundPool = "background"
	// LivePool is the live-update readers' pool (Pool.Live).
	LivePool = "live"
	// SettlementsPool is the document settlements' pool (Pool.Settlements).
	SettlementsPool = "settlements"
	// RegistryPool is the server process registry's one connection (Pool.OpenRegistry).
	RegistryPool = "registry"
)

// poolNames is every pool name, in the order Stats reports them. The registry's pool is in Stats
// once processes.Register has opened it (Pool.OpenRegistry).
var poolNames = [...]string{
	RequestsPool, RoomsPool, HealthPool, ListenPool, LeasesPool, OutboxPool,
	BackgroundPool, LivePool, SettlementsPool, RegistryPool,
}

// applicationNamePrefix begins every pool's application_name: dispatch-<pool>, the one convention
// every pool of the process shares, so pg_stat_activity groups a process's connections by pool and
// the metrics' pool label is the part after the prefix. The connection string's own
// application_name, if it names one, is replaced: a pool's name is what an operator reading
// pg_stat_activity during an incident needs, and it is the same in every deployment.
const applicationNamePrefix = "dispatch-"

// The acquire bounds: how long one acquire on each pool waits for a connection before it gives up
// with ErrPoolBusy. Before them every wait was unbounded, so past capacity callers queued until
// their clients gave up, and the queue only grew. A bound refuses the excess instead, and the
// caller's own deadline wins whenever it is sooner (acquireTracer). Each bound is per acquire: a
// write makes an acquire per statement and transaction, one after another (a read pins one
// connection and makes one, PinConnection), so the request's own deadline, not these, bounds
// their sum.
const (
	// requestAcquireBound is the request pool's, and the room loads' and live readers' too: a
	// request a client is waiting on that has waited three seconds for a connection is better
	// answered busy, so the client retries, than held for a connection that may never come free.
	requestAcquireBound = 3 * time.Second
	// backgroundAcquireBound is the background loops' and the settlements': nobody is waiting on
	// them, so they may wait far longer than a request, and a refused pass only runs at its next
	// tick - but not without end, so a pool that never frees a connection shows as an error.
	backgroundAcquireBound = 30 * time.Second
	// leaseAcquireBound is the lease pool's. The pool has a connection for every lease name and
	// spare (leasePoolSize), so a wait at all means a connection is held by something that is not
	// a listed lease; a pass refused here skips (duty.Exclusive), and the next tick tries again.
	leaseAcquireBound = 2 * time.Second
	// listenAcquireBound is the listener pool's. Its one connection is hijacked as soon as it is
	// dialled (listenerPoolSize), so an acquire is a wait for the dials ahead of it - three
	// listeners at most - and then a dial of its own: milliseconds on a database that answers,
	// and far longer than ten seconds only on one that does not, where a listener that gives up
	// redials on its own backoff (pglisten.Listener) instead of waiting out the two-minute
	// connect timeout pgxpool floors a dial at.
	listenAcquireBound = 10 * time.Second
)

// defaultAcquireBound is pool name's acquire bound; zero is none. The health probe bounds its own
// wait, dial and query together (healthProbeTimeout), so its acquire needs nothing more; the
// outbox and the registry are each their pool's only caller, one statement or transaction at a
// time, and hold at most its one connection, so their acquires never wait behind another caller.
func defaultAcquireBound(name string) time.Duration {
	switch name {
	case RequestsPool, RoomsPool, LivePool:
		return requestAcquireBound
	case BackgroundPool, SettlementsPool:
		return backgroundAcquireBound
	case LeasesPool:
		return leaseAcquireBound
	case ListenPool:
		return listenAcquireBound
	}
	return 0
}

// ErrPoolBusy is an acquire that ran out its pool's acquire bound while the caller was still
// waiting: every connection of that pool stayed taken for the whole bound. Nothing ran, so the
// caller may retry; a request answers it as busy rather than as a failure. The error wraps
// context.DeadlineExceeded too, so errors.Is finds both, and IsPoolBusy tells it from a deadline
// of the caller's own.
//
// Every pool answers a bound it ran out with it, the wrapped pools (Pool) and the raw ones
// (RawPool: rooms, leases, listen, registry, health) alike, so a caller tells a pool with nothing
// free from its own deadline by IsPoolBusy rather than by guessing from a bare
// context.DeadlineExceeded - a guess that read a commit cut off at its bound (ErrCommitTimeout),
// whose outcome is unknown, as a pool that refused before anything ran.
var ErrPoolBusy = errors.New("no connection of the pool came free within its acquire bound")

// IsPoolBusy reports whether err is an acquire that ran out its pool's bound (ErrPoolBusy), as
// opposed to the caller's own deadline or cancellation. Nothing of what the caller asked for ran,
// so it may be retried whatever it was.
func IsPoolBusy(err error) bool { return errors.Is(err, ErrPoolBusy) }

// poolBusy wraps err as ErrPoolBusy when it is an acquire bound: a deadline exceeded while the
// caller's own context is still live can only be the bound acquireTracer put on the acquire,
// since the statement itself runs on the caller's context. A caller whose own deadline came first
// gets its own error back.
func poolBusy(ctx context.Context, pool string, err error) error {
	if err == nil || ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPoolBusy) {
		return err
	}
	return fmt.Errorf("%s pool: %w: %w", pool, ErrPoolBusy, err)
}

// acquireBuckets are the upper bounds of the acquire-wait histogram's buckets, from a
// millisecond, an idle pool's hand-over, to the longest acquire bound.
var acquireBuckets = [...]time.Duration{
	time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond,
	100 * time.Millisecond, 500 * time.Millisecond, time.Second, 2500 * time.Millisecond,
	5 * time.Second, 10 * time.Second, 30 * time.Second,
}

// AcquireBucketCount is how many buckets the acquire-wait histogram has.
const AcquireBucketCount = len(acquireBuckets)

// AcquireBucketBounds are the upper bounds, in seconds, of the acquire-wait histogram's buckets:
// AcquireStats.Buckets[i] counts the acquires that waited at most AcquireBucketBounds()[i].
func AcquireBucketBounds() [AcquireBucketCount]float64 {
	var bounds [AcquireBucketCount]float64
	for i, bound := range acquireBuckets {
		bounds[i] = bound.Seconds()
	}
	return bounds
}

// AcquireStats is what one pool's acquire tracer has counted since the pool opened.
type AcquireStats struct {
	// Waiting is the acquires in progress now: callers waiting for a connection of the pool.
	Waiting int64
	// Timeouts is the acquires that ran out the pool's acquire bound (ErrPoolBusy).
	Timeouts uint64
	// Count is the acquires that finished, with a connection or without one, and Sum their
	// waits added together, in seconds.
	Count uint64
	Sum   float64
	// Buckets[i] is how many of those acquires waited at most AcquireBucketBounds()[i]:
	// cumulative, as a Prometheus histogram's buckets are, so Count is the bucket past the last.
	Buckets [AcquireBucketCount]uint64
}

// PoolStats is one open pool's connection counts and acquire waits.
type PoolStats struct {
	Name    string
	Stat    *pgxpool.Stat
	Acquire AcquireStats
}

// acquireTracer bounds and measures every acquire of one pool. pgxpool's Acquire calls
// TraceAcquireStart first and waits for the connection, the ping of an idle one included, on the
// context it returns, and every way the pool hands out a connection - Query, QueryRow, Exec,
// BeginTx, SendBatch, CopyFrom, AcquireFunc - goes through Acquire. So the bound covers every
// caller of the pool, the store.Pool wrappers and the raw pgxpool users alike, while the statement
// still runs on the caller's own context. It counts the connections handed back open too
// (TraceRelease), which pgxpool reports to a configuration's Tracer the same way.
type acquireTracer struct {
	// name makes the application_name, and inner is the tracer the configuration had before this
	// one was composed with it: kept so a pool opened from this pool's configuration composes its
	// own tracer with that one rather than with this.
	name  string
	bound time.Duration
	inner pgx.QueryTracer

	waiting  atomic.Int64
	timeouts atomic.Uint64
	count    atomic.Uint64
	waited   atomic.Int64 // nanoseconds
	buckets  [AcquireBucketCount]atomic.Uint64
	// finished counts the connections handed back open and idle (TraceRelease).
	finished atomic.Uint64
}

// errAcquireBound is the cause of an acquire context the bound ended, which is how
// TraceAcquireEnd tells a bound run out from the caller's own deadline or cancellation.
var errAcquireBound = errors.New("acquire bound reached")

type acquireKey struct{}

// acquireSpan is one acquire's state from start to end.
type acquireSpan struct {
	started time.Time
	cancel  context.CancelFunc
}

func (t *acquireTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	t.waiting.Add(1)
	span := &acquireSpan{started: time.Now()}
	if t.bound > 0 {
		// A caller whose own deadline comes first keeps it, so its acquire ends with its own
		// error and never with ErrPoolBusy.
		if deadline, ok := ctx.Deadline(); !ok || deadline.Sub(span.started) > t.bound {
			ctx, span.cancel = context.WithTimeoutCause(ctx, t.bound, errAcquireBound)
		}
	}
	return context.WithValue(ctx, acquireKey{}, span)
}

func (t *acquireTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	t.waiting.Add(-1)
	span, ok := ctx.Value(acquireKey{}).(*acquireSpan)
	if !ok {
		return
	}
	waited := time.Since(span.started)
	if span.cancel != nil {
		if data.Err != nil && errors.Is(context.Cause(ctx), errAcquireBound) {
			t.timeouts.Add(1)
		}
		span.cancel()
	}
	t.count.Add(1)
	t.waited.Add(int64(waited))
	for i, bound := range acquireBuckets {
		if waited <= bound {
			t.buckets[i].Add(1)
			break
		}
	}
}

// TraceRelease counts a connection its caller hands back open and idle, which pgxpool keeps for its
// next caller: the work on it has finished. One pgxpool discards instead - closed by a statement
// whose context ended, or handed back mid-statement or inside a transaction - finished nothing, and
// neither did one pgxpool discards inside Acquire because its ping went unanswered, which no caller
// ever held, so it is never released.
func (t *acquireTracer) TraceRelease(_ *pgxpool.Pool, data pgxpool.TraceReleaseData) {
	if data.Conn.IsClosed() || data.Conn.PgConn().IsBusy() || data.Conn.PgConn().TxStatus() != 'I' {
		return
	}
	t.finished.Add(1)
}

// TraceQueryStart and TraceQueryEnd do nothing: a connection configuration's Tracer must be a
// pgx.QueryTracer, and this one traces acquires and releases only.
func (*acquireTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*acquireTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// stats reads what the tracer has counted. The counters are read one at a time, so a scrape that
// races an acquire may see it in one counter and not yet in another.
func (t *acquireTracer) stats() AcquireStats {
	stats := AcquireStats{
		Waiting:  t.waiting.Load(),
		Timeouts: t.timeouts.Load(),
		Count:    t.count.Load(),
		Sum:      time.Duration(t.waited.Load()).Seconds(),
	}
	var cumulative uint64
	for i := range t.buckets {
		cumulative += t.buckets[i].Load()
		stats.Buckets[i] = cumulative
	}
	return stats
}

// composedTracer is an acquireTracer composed with a tracer the configuration already had: every
// call that tracer takes still reaches it, and the acquire tracer runs first, so the other one
// sees the bounded context.
type composedTracer struct {
	*multitracer.Tracer
	acquire *acquireTracer
}

// acquireTracerOf is the acquire tracer configurePool set on a configuration, or nil.
func acquireTracerOf(tracer pgx.QueryTracer) *acquireTracer {
	switch tracer := tracer.(type) {
	case *acquireTracer:
		return tracer
	case *composedTracer:
		return tracer.acquire
	}
	return nil
}

// configurePool names config's connections dispatch-<name> in application_name
// (applicationNamePrefix) and sets the pool's acquire tracer, bounded at bound (zero is none) and
// composed with any tracer config already carries. A configuration copied from another pool's is
// re-configured from that pool's original tracer, not stacked on its acquire tracer.
func configurePool(config *pgxpool.Config, name string, bound time.Duration) *acquireTracer {
	inner := config.ConnConfig.Tracer
	if configured := acquireTracerOf(inner); configured != nil {
		inner = configured.inner
	}
	tracer := &acquireTracer{name: name, bound: bound, inner: inner}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["application_name"] = applicationNamePrefix + name
	if inner == nil {
		config.ConnConfig.Tracer = tracer
		return tracer
	}
	composed := multitracer.New(inner)
	composed.PoolAcquireTracers = append([]pgxpool.AcquireTracer{tracer}, composed.PoolAcquireTracers...)
	composed.PoolReleaseTracers = append([]pgxpool.ReleaseTracer{tracer}, composed.PoolReleaseTracers...)
	config.ConnConfig.Tracer = &composedTracer{Tracer: composed, acquire: tracer}
	return tracer
}

// BoundAcquires re-configures config, a copy of an open pool's configuration (Pool.Config), so a
// pool opened from it bounds its acquires at bound, under the same pool name. Tests outside this
// package shorten a full pool's wait with it, so the pool answers ErrPoolBusy in milliseconds
// rather than in production's seconds.
func BoundAcquires(config *pgxpool.Config, bound time.Duration) {
	name := RequestsPool
	if configured := acquireTracerOf(config.ConnConfig.Tracer); configured != nil {
		name = configured.name
	}
	configurePool(config, name, bound)
}
