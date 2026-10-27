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

// The pools a Pool opens, by the name each one carries: the suffix of its connections'
// application_name in pg_stat_activity (configurePool), its entry in Stats, and what Use is asked
// about.
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
)

// poolNames is every pool name, in the order Stats reports them.
var poolNames = [...]string{
	RequestsPool, RoomsPool, HealthPool, ListenPool, LeasesPool, OutboxPool,
	BackgroundPool, LivePool, SettlementsPool,
}

// defaultApplicationName is the application_name prefix of every pool's connections when the
// connection string names none; a connection string's own application_name replaces it, so a
// deployment that names its process still finds each pool under that name.
const defaultApplicationName = "envoy-dispatch"

// The acquire bounds: how long one acquire on each pool waits for a connection before it gives up
// with ErrPoolBusy. Before them every wait was unbounded, so past capacity callers queued until
// their clients gave up, and the queue only grew. A bound refuses the excess instead, and the
// caller's own deadline wins whenever it is sooner (acquireTracer). Each bound is per acquire: a
// request makes a dozen acquires one after another, so the request's own deadline, not these,
// bounds their sum.
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
// outbox is its pool's only caller and holds at most its one connection, so its acquire never
// waits behind another caller.
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
var ErrPoolBusy = errors.New("no connection of the pool came free within its acquire bound")

// IsPoolBusy reports whether err is an acquire that ran out its pool's bound (ErrPoolBusy), as
// opposed to the caller's own deadline or cancellation.
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
// still runs on the caller's own context.
type acquireTracer struct {
	// base and name make the application_name, and inner is the tracer the configuration had
	// before this one was composed with it: kept so a pool opened from this pool's configuration
	// composes its own tracer with that one rather than with this.
	base, name string
	bound      time.Duration
	inner      pgx.QueryTracer

	waiting  atomic.Int64
	timeouts atomic.Uint64
	count    atomic.Uint64
	waited   atomic.Int64 // nanoseconds
	buckets  [AcquireBucketCount]atomic.Uint64
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

// TraceQueryStart and TraceQueryEnd do nothing: a connection configuration's Tracer must be a
// pgx.QueryTracer, and this one traces acquires only.
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

// configurePool names config's connections <base>/<name> in application_name, where base is the
// connection string's application_name or defaultApplicationName, and sets the pool's acquire
// tracer, bounded at bound (zero is none) and composed with any tracer config already carries. A
// configuration copied from another pool's is re-configured from that pool's base and original
// tracer, not stacked on its name and its acquire tracer.
func configurePool(config *pgxpool.Config, name string, bound time.Duration) *acquireTracer {
	base, inner := config.ConnConfig.RuntimeParams["application_name"], config.ConnConfig.Tracer
	if configured := acquireTracerOf(inner); configured != nil {
		base, inner = configured.base, configured.inner
	}
	if base == "" {
		base = defaultApplicationName
	}
	tracer := &acquireTracer{base: base, name: name, bound: bound, inner: inner}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["application_name"] = base + "/" + name
	if inner == nil {
		config.ConnConfig.Tracer = tracer
		return tracer
	}
	composed := multitracer.New(inner)
	composed.PoolAcquireTracers = append([]pgxpool.AcquireTracer{tracer}, composed.PoolAcquireTracers...)
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
