package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNestedAcquire is a pool this package guards (Pool: the request pool and the pools it wraps)
// refusing a connection to a caller that already holds a connection of one of them.
//
// One caller, one connection, is the rule the pool cannot survive without. A transaction
// holds its connection until it commits; a caller that asks for a second one while it holds a
// row or advisory lock waits for a connection only the callers queued behind that lock can
// release, and they are waiting for the lock. Two anchored writes and two document
// settlements on one issue closed that cycle on a four-connection pool, and nothing but
// pg_terminate_backend recovered it. Rationing connections cannot fix it, and neither can
// sharedPoolSize's size: the queue behind one writer's issue lock is unbounded - a
// settlement per document, every issue-owned event append, the architecture importer - so a
// bigger pool only moves the number of callers it takes.
//
// So work that genuinely needs a connection of its own while a transaction is open takes it
// from a pool of its own - the rooms pool (Pool.Rooms) for a document load, the health pool
// (Pool.Healthy) for the load balancer's probe - not from here. Everything else reads through
// the transaction it is already inside.
//
// This error makes the rule enforce itself. Every entry point that opens one of this pool's
// transactions marks its context (api.Register marks every route; settlement, the architecture
// importer, the outbox publisher, the CLI backfills, the startup seeds and the migrations mark
// their own goroutines), and an acquisition that arrives under a held connection fails here,
// loudly, instead of wedging production.
var ErrNestedAcquire = errors.New(
	"this caller already holds a connection of the pool: a second one would deadlock it",
)

// holding records whether the context's caller holds a connection of this pool: its
// transaction, its open cursor, or one it acquired directly. One writer sets it and one reader
// tests it, so a flag is the whole state - a transaction never opens a second transaction of its
// own. A pinned request (PinConnection) also carries its pinned connection here, so a context
// made with a mark of its own (ForConcurrentRead) pins nothing.
type holding struct {
	connection atomic.Bool
	pin        *pinnedConnection
}

type holdingKey struct{}

// WithTransactionTracking marks ctx so the pool can refuse a second connection while one of its
// transactions is open. API requests are marked by middleware; every background entry point
// that opens a transaction - settlement, the architecture importer, the outbox publisher, the
// CLI backfills, startup seeding and the migrations - marks its own context.
func WithTransactionTracking(ctx context.Context) context.Context {
	if ctx.Value(holdingKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, holdingKey{}, &holding{})
}

// HoldsConnection marks ctx as already holding a pooled connection until the returned release
// runs, so an acquisition under it is refused like one inside a transaction. The durable
// document appends hold their connection directly, outside any transaction of this pool's.
func HoldsConnection(ctx context.Context) (context.Context, func()) {
	ctx = WithTransactionTracking(ctx)
	return ctx, hold(ctx)
}

// hold records that ctx's caller now holds a connection of this pool and returns the release
// that clears it, or nil for a context the pool cannot see, which has no mark to clear. The
// release runs at most once: a holder that releases twice - a transaction that commits and
// then runs its deferred rollback, a cursor drained and then closed - must not clear the mark
// of whatever holds a connection by then.
func hold(ctx context.Context) func() {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	if !ok {
		return nil
	}
	state.connection.Store(true)
	return sync.OnceFunc(func() { state.connection.Store(false) })
}

func holdsConnection(ctx context.Context) bool {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	return ok && state.connection.Load()
}

// ConnectionHeld reports whether ctx's caller holds a pooled connection: its transaction, a
// cursor, a connection it acquired or marked (HoldsConnection), or its request's pinned connection
// (PinConnection), idle between statements or not. Whatever a connection holder must never wait
// for refuses the callers it reports - a document's writer gate, whose holder may be waiting for a
// connection itself. The pools refuse a second acquire to the first three (ErrNestedAcquire); a
// request-pool call under a pin runs on the pinned connection instead.
func ConnectionHeld(ctx context.Context) bool {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	return ok && (state.connection.Load() || state.pin.held())
}

// ForConcurrentRead returns a context for one of a caller's reads that run side by side: ctx's
// deadline, cancellation and values, with a hold mark of its own and no pinned connection, so that
// read takes one connection and the pool still refuses it a second. Reads sharing one mark would
// refuse each other, since one read's open cursor marks the context they share, and so would reads
// sharing one pinned connection, which runs one call at a time. A caller already holding a
// connection (its transaction, a cursor, an acquired connection, its pinned connection) is refused
// with ErrNestedAcquire, because each of its reads would be a second connection taken while it
// holds one.
func ForConcurrentRead(ctx context.Context) (context.Context, error) {
	if ConnectionHeld(ctx) {
		logRefusal()
		return nil, ErrNestedAcquire
	}
	return context.WithValue(ctx, holdingKey{}, &holding{}), nil
}

// loggedSites remembers the call sites that have already logged a refusal, so a caller that
// trips the guard in a loop reports its stack once instead of flooding the log. The error
// itself is returned to every caller, every time.
var loggedSites sync.Map

func logRefusal() {
	var caller [1]uintptr
	// Skip runtime.Callers, logRefusal, guard, and the pool method that called it.
	if runtime.Callers(4, caller[:]) == 0 {
		return
	}
	if _, seen := loggedSites.LoadOrStore(caller[0], struct{}{}); seen {
		return
	}
	slog.Error("dispatch: second pooled connection requested while one is held",
		"error", ErrNestedAcquire, "stack", string(debug.Stack()))
}

// Pool is a Dispatch connection pool: the shared request pool store.Open opens, or one of the
// pools of its own that wraps (Outbox, Background, Live, Settlements). Every way it hands out a
// connection refuses one asked for while the caller already holds one; see ErrNestedAcquire. The
// pgx pool is private so no caller can reach past the refusal.
type Pool struct {
	pool *pgxpool.Pool
	// name is the pool's name (RequestsPool for the shared pool), and acquire the tracer that
	// bounds and counts its acquires, nil for a pool opened without one (configurePool).
	name    string
	acquire *acquireTracer
	// mu guards each separate pool's creation against this pool's close: one lock, so a
	// caller that loses the race is told the pool is closed rather than opening one nothing
	// will ever close.
	mu        sync.Mutex
	separates map[string]openedPool
	closed    bool
	// acquireBounds replaces defaultAcquireBound for the separate pools it names, read as each
	// one opens: a test shortens a bound with it, so a full pool refuses in milliseconds.
	acquireBounds map[string]time.Duration
}

// openedPool is one pgx pool a Pool has open, under its name, with its acquire tracer.
type openedPool struct {
	name    string
	pool    *pgxpool.Pool
	acquire *acquireTracer
}

// NewPool wraps an open pgx pool. Its name and acquire tracer are the ones configurePool set on
// the pool's configuration; a pool opened without them is the request pool, unbounded.
func NewPool(pool *pgxpool.Pool) *Pool {
	acquire := acquireTracerOf(pool.Config().ConnConfig.Tracer)
	name := RequestsPool
	if acquire != nil {
		name = acquire.name
	}
	return &Pool{pool: pool, name: name, acquire: acquire}
}

// sharedPoolSize bounds the shared pool - the request pool - deliberately and in code. pgx's
// default, max(4, NumCPU), is four on production's one Fargate task at cpu="512", and a
// connection per blocked writer is all it takes to starve everything else: with four writers
// parked on one issue's row lock, a read of an unrelated issue waited the whole seventeen seconds
// the lock was held, where sixteen connections served 3,853 such reads at a four-millisecond
// median. The ceiling is picked here rather than derived from the task's CPU allotment or read
// out of the DSN another repository's URL builder writes.
//
// It is 48 of one connection budget of 87 (ConnectionsPerProcess) that every pool of a process
// shares, partitioned rather than grown: requests 48, background 6 (Background), live 4 (Live),
// settlements 4 (Settlements), rooms 4, health 1, the three permanent listeners (the listener
// pool's one dial is one of them redialling), leases leasePoolSize (15), outbox 1 and the server
// process registry's own 1 (registryPoolSize). The partition itself added nothing: it is the 87
// the process opened before, the request pool having given up 16, which background, live,
// settlements and the leases' two spare connections took. A write takes a connection per statement
// one after another, and a read pins one for its whole length (PinConnection), so under load the
// API holds every connection of this pool; the background loops, the live-update readers and the
// settlements wait on pools of their own, so neither side can starve the other, and an acquire
// here waits at most requestAcquireBound before it is refused with ErrPoolBusy rather than
// queueing without end.
//
// The database bounds the budget from above. Dispatch's database is the gcp:Dispatch component's
// Cloud SQL Postgres instance, whose default max_connections follows its tier's memory
// (https://cloud.google.com/sql/docs/postgres/flags, max_connections), and every server pod opens
// up to ConnectionsPerProcess, the budget. During a roll, every pod that holds connections (the
// replicas, the surge pod and pods still terminating, for up to their grace period) times that
// sum must fit in max_connections less PostgreSQL's 3 superuser-reserved slots, so raising a pool
// or the pod count is checked against it: read SHOW max_connections on the instance before
// raising either. A new pool, or a new permanent Listener caller, goes into
// ConnectionsPerProcess with it.
const sharedPoolSize = 48

// roomsPoolSize bounds the connections document loads use. Loading takes no lock and always
// finishes, so one is enough for the pool to be deadlock-free; a few let cold rooms load
// concurrently. Measured on the real server, eight cold rooms loading at once are no faster at
// eight connections than at one, so this is small on purpose.
const roomsPoolSize = 4

// healthPoolSize bounds the connections the health probe uses. The probe answers one question -
// is Postgres reachable, and at which schema - and asks it on a connection nothing else can take,
// so one is not a ration but the whole design.
const healthPoolSize = 1

// healthProbeTimeout bounds the health probe's own wait, dial and query together, and owns the
// argument for why the probe is bounded at all. Every prober treats silence as a dead process:
// the ALB fails a poll at five seconds and three consecutive failures replace the task,
// cancelling every in-flight request of every other client, and the compose healthcheck and
// the deploy script are tighter still at three (deploy/compose/dispatch.compose.yml,
// deploy/scripts/autodeploy.sh). Two seconds fits inside the tightest of them, so a database
// that has stopped answering comes back as a 503 they can read.
//
// Nothing else bounds it usefully. A cold dial is floored at two minutes, not unbounded -
// pgxpool sets ConnectTimeout to two minutes whenever the config leaves it at zero, under
// pgx's own "ensure that a connect won't hang forever", and pgconn applies that to the whole
// connection process - but two minutes is forty times the tightest deadline above. Once the
// connection is up nothing bounds the query at all, because an http.Server request context
// carries no deadline, and warm is production's normal state: the probe runs every few seconds
// and an idle connection lives for half an hour. A link that drops packets - a security-group
// change, an availability-zone partition, an endpoint that accepts and then stalls - is the
// unbounded case whenever the connection was already open.
const healthProbeTimeout = 2 * time.Second

// ErrPoolClosed is a connection asked for after the pool was released.
var ErrPoolClosed = errors.New("connection pool is closed")

// Rooms is where a document load takes its connection. A load runs while the request that
// triggered it holds an open transaction on this pool - it happens when an anchored write
// stamps its mark in a room nobody has opened yet - and it is deliberately not part of that
// transaction, because the loaded room outlives the request and must not roll back with it. A
// second connection from the shared pool is exactly what deadlocks it (ErrNestedAcquire), so
// the loads have their own, opened on demand and closed with this pool: one per store, not one
// per caller who wants to read a document. Its acquires are bounded at requestAcquireBound like
// the request pool's; a caller of this raw pgx pool sees the bound as context.DeadlineExceeded
// with its own context still live.
func (p *Pool) Rooms() (*pgxpool.Pool, error) {
	return p.separate(RoomsPool, roomsPoolSize)
}

// Healthy reports the highest schema migration Postgres has applied, read within
// healthProbeTimeout on a connection of a pool nothing else uses; the read is the reachability
// probe too, so an error means the database did not answer. /healthz asks it, and it must never
// queue behind a writer: proving the shared pool has a free connection is the wrong question,
// because during a busy period it legitimately has none. Like the rooms pool this one is outside
// the nested-acquire guard, because a separate pool cannot close the cycle the guard prevents.
//
// The version is read from schema_migrations on every call rather than remembered from this
// process's own Migrate, so it reports what the database holds: a deploy check compares it with
// the highest migration in the commit the image was built from.
func (p *Pool) Healthy(ctx context.Context) (int, error) {
	health, err := p.separate(HealthPool, healthPoolSize)
	if err != nil {
		return 0, err
	}
	// Derived from the caller's context, so a request the client already gave up on ends with
	// it instead of holding the probe's one connection for the full bound.
	ctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	var version int
	if err := health.QueryRow(ctx, "select coalesce(max(version), 0) from schema_migrations").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// listenerPoolSize bounds the pool Listener takes its connections from. Three permanent
// listeners dial through it - the delivery hub, the event stream relay and the stale-room
// evictor - and each connection is hijacked as soon as it is opened, so it leaves with its
// caller and the pool holds none between dials: what one bounds is dials in flight, not
// connections held. A listener redialling while another dials waits that dial out, which is
// what a loop that dials and then waits for notifications for the life of the process can
// afford.
const listenerPoolSize = 1

// Listener returns one connection of its own for LISTEN, from a one-connection pool opened from the
// shared pool's configuration and hijacked, so it holds no shared connection. The caller closes it.
// The delivery hub (api/delivery_live_hub.go), the event stream relay (api/event_stream_relay.go)
// and the document rooms' stale-room evictor (docs/stale_room.go) each hold one for as long as
// they wait for notifications, which is the life of the process: a connection of the shared pool
// held that long would be one fewer for every request, and a LISTEN on a pooled connection would
// end when the pool recycled it. A dial that does not get the pool's connection within
// listenAcquireBound is refused with ErrPoolBusy.
func (p *Pool) Listener(ctx context.Context) (*pgx.Conn, error) {
	listen, err := p.separate(ListenPool, listenerPoolSize)
	if err != nil {
		return nil, err
	}
	conn, err := listen.Acquire(ctx)
	if err != nil {
		return nil, poolBusy(ctx, ListenPool, err)
	}
	return conn.Hijack(), nil
}

// leaseNameCount is how many lease names duty.Exclusive accepts: the names of duty's lease list,
// counted here because store cannot import duty, and held to that list by a test.
const leaseNameCount = 13

// leasePoolSize bounds the pool a periodic maintenance loop takes its once-overall lease on
// (duty.Exclusive): a connection for every lease name on duty's list, and two spare. A caller and
// not a lease needs the connection - Exclusive takes it before it learns whether the lease is
// held, so a pass that skips because another process holds the lease holds one for that round
// trip too - and every listed name has one caller, since each delivery intake takes its own
// durable's consumer lease from a bind poll of its own, so every name can be inside Exclusive at
// once. Exclusive refuses a name that is not on the list, so a loop added to the list adds its
// connection here rather than taking one another loop counted on.
//
// The two spare connections are for what holds one without being a listed lease's pass: pgxpool's
// own health check and lifetime expiry take an idle connection while they ping or replace it, and
// a pass whose unlock failed holds its connection until the close that ends its session
// (duty.freeLeaseOn, teardownTimeout). Without them the next pass waits on those (review BG-5);
// with them a wait at all is a fault, and an acquire that waits leaseAcquireBound makes Exclusive
// skip the pass rather than queue. They cannot come from the request pool: a lease is held for the
// whole pass, which is minutes for the compaction, and the request pool's connections are what
// every request reads through.
const leasePoolSize = leaseNameCount + 2

// Leases is where a periodic maintenance loop takes the connection it holds its lease on, so that
// only one process runs each pass (duty.Exclusive). The lease is a session-level advisory lock, so
// it lives on one connection for the length of the pass and dies with it; a connection of the
// shared pool held that long would be one fewer for every request, and a pass that then read
// through the shared pool - which is what a pass does - would be a caller holding two of its
// connections (ErrNestedAcquire). Its acquires are bounded at leaseAcquireBound, which a caller of
// this raw pgx pool sees as context.DeadlineExceeded with its own context still live.
func (p *Pool) Leases() (*pgxpool.Pool, error) {
	return p.separate(LeasesPool, leasePoolSize)
}

// outboxPoolSize bounds the pool the events outbox reads and records events through. The outbox
// is one goroutine that runs one statement at a time and closes each cursor before its next
// statement, so one connection is every connection it can use.
const outboxPoolSize = 1

// registryPoolSize bounds the pool the registry of live server processes stamps this process's row
// through: processes.Register opens it with OpenRegistry and its Deregister closes it, so each
// registration opens a pool of its own, which Stats reports while it is open.
const registryPoolSize = 1

// permanentListeners is how many connections the listener pool dials and hands off for the life
// of the process: the delivery hub, the event stream relay and the stale-room evictor (Listener).
const permanentListeners = 3

// ConnectionsPerProcess is the most connections one Dispatch server process opens to Postgres, the
// budget of 87 sharedPoolSize describes: every pool's size, the registry's own pool
// (processes.Register) among them, and one connection per permanent listener, which the listener
// pool dials and hands off rather than holds (its one dial in flight is a permanent listener
// redialling, so it is not counted twice).
const ConnectionsPerProcess = sharedPoolSize + backgroundPoolSize + livePoolSize + settlementsPoolSize +
	roomsPoolSize + healthPoolSize + leasePoolSize + outboxPoolSize + registryPoolSize + permanentListeners

// Outbox is the pool the events outbox claims, reads and records events through, and reads the
// followers and authors a publish routes to. Under load the API's requests queue for the shared
// pool's connections, and an outbox statement queued behind them holds back every event behind
// it; on a pool of its own it waits for none of them, and holds no connection a request could use.
func (p *Pool) Outbox() (*Pool, error) {
	return p.wrapped(OutboxPool, outboxPoolSize)
}

// backgroundPoolSize bounds the pool for work no request waits on: background loops such as the
// embedding queue and its coverage pass, the suggestion sweep, the delivery reconcile and intakes,
// the architecture importer's ticker, the documents' resumption query, task progress,
// reference-excerpt and block-index reconciliation, the edit-notice sweep, the repair of closed and
// reopened issues' asks, the server process registry's prune and the fleet build announcer. Six is
// the background's share of the budget sharedPoolSize describes; nobody waits on these loops, so
// they wait for each other here (up to backgroundAcquireBound) instead of for the requests'
// connections.
const backgroundPoolSize = 6

// Background is the pool for background loops, so that under load a request never queues behind a
// loop's statement for a connection, nor a loop behind the requests.
func (p *Pool) Background() (*Pool, error) {
	return p.wrapped(BackgroundPool, backgroundPoolSize)
}

// livePoolSize bounds the pool for the live-update readers: the event stream relay and the
// delivery hub, each reading what a notification announced and handing it to every open stream.
// Four is their share of the budget sharedPoolSize describes.
const livePoolSize = 4

// Live is the pool for the live-update readers, so an event reaches the open streams while the
// requests hold every connection of theirs; its acquires are bounded at requestAcquireBound, since
// a stream is waiting on each one.
func (p *Pool) Live() (*Pool, error) {
	return p.wrapped(LivePool, livePoolSize)
}

// settlementsPoolSize bounds the pool for document settlements. Four is the settlements' share of
// the budget sharedPoolSize describes.
const settlementsPoolSize = 4

// Settlements is the pool for document settlements, so a burst of settlements - every room a
// shutdown or a timer settles - neither waits for the requests' connections nor takes them.
func (p *Pool) Settlements() (*Pool, error) {
	return p.wrapped(SettlementsPool, settlementsPoolSize)
}

// wrapped is the separate pool name, wrapped so the nested-acquire guard and the ErrPoolBusy
// mapping apply to it as they do to the request pool.
func (p *Pool) wrapped(name string, size int32) (*Pool, error) {
	opened, err := p.open(name, size)
	if err != nil {
		return nil, err
	}
	return &Pool{pool: opened.pool, name: opened.name, acquire: opened.acquire}, nil
}

// SetAcquireBoundForTest sets the acquire bound the separate pool named name opens with, in place
// of its own (defaultAcquireBound). It is for tests outside this package alone, which shorten a
// bound so a full pool refuses in milliseconds rather than production's seconds; nothing in
// production calls it, since every pool's bound is set in code. A pool already open keeps its bound.
func (p *Pool) SetAcquireBoundForTest(name string, bound time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.acquireBounds == nil {
		p.acquireBounds = make(map[string]time.Duration)
	}
	p.acquireBounds[name] = bound
}

// separate is the pgx pool of the separate pool named name (open).
func (p *Pool) separate(name string, size int32) (*pgxpool.Pool, error) {
	opened, err := p.open(name, size)
	if err != nil {
		return nil, err
	}
	return opened.pool, nil
}

// open opens the pool named name on demand, sized at size, copied from this pool's configuration
// and configured as its own (configurePool: its application_name and its acquire bound,
// acquireBounds' or defaultAcquireBound's), and closes it with this pool. Each caller holds mu for
// the whole check-and-open so two of them cannot open two pools, and one opened after Close would
// be a pool nobody closes.
func (p *Pool) open(name string, size int32) (openedPool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return openedPool{}, ErrPoolClosed
	}
	if opened, ok := p.separates[name]; ok {
		return opened, nil
	}
	config := p.pool.Config()
	config.MaxConns = size
	// Both minimums, not just MinConns: a floor inherited from the shared pool's DSN would be
	// a target these pools cannot reach - pool_min_idle_conns=2 on a one-connection health
	// pool - which pgxpool re-attempts, and puddle refuses as full, every health-check period.
	config.MinConns = 0
	config.MinIdleConns = 0
	bound, ok := p.acquireBounds[name]
	if !ok {
		bound = defaultAcquireBound(name)
	}
	acquire := configurePool(config, name, bound)
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return openedPool{}, fmt.Errorf("open %s pool: %w", name, err)
	}
	if p.separates == nil {
		p.separates = make(map[string]openedPool, len(poolNames))
	}
	opened := openedPool{name: name, pool: pool, acquire: acquire}
	p.separates[name] = opened
	return opened, nil
}

// Close releases every connection the pool holds, every separate pool's included.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	separates := p.separates
	p.separates = nil
	p.mu.Unlock()
	for _, separate := range separates {
		separate.pool.Close()
	}
	p.pool.Close()
}

// Config returns a copy of the pool's configuration.
func (p *Pool) Config() *pgxpool.Config { return p.pool.Config() }

// OpenRegistry opens the server process registry's own pool, which processes.Register uses and its
// Deregister closes: this pool's configuration sized at registryPoolSize and named as the
// registry's (configurePool, RegistryPool), so its connection is dispatch-registry in
// pg_stat_activity and its acquires are counted apart from the requests'. The pool is recorded
// with the others, replacing one an earlier registration opened, so Stats and the metrics report
// the heartbeat's connection and its waits as they report every other pool's, and Close closes it
// with them; closing it twice is harmless, since a pgx pool closes once.
func (p *Pool) OpenRegistry(ctx context.Context) (*pgxpool.Pool, error) {
	config := p.pool.Config()
	config.MaxConns = registryPoolSize
	config.MinConns = 0
	config.MinIdleConns = 0
	p.mu.Lock()
	bound, ok := p.acquireBounds[RegistryPool]
	p.mu.Unlock()
	if !ok {
		bound = defaultAcquireBound(RegistryPool)
	}
	acquire := configurePool(config, RegistryPool, bound)
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open %s pool: %w", RegistryPool, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		opened.Close()
		return nil, ErrPoolClosed
	}
	if p.separates == nil {
		p.separates = make(map[string]openedPool, len(poolNames))
	}
	p.separates[RegistryPool] = openedPool{name: RegistryPool, pool: opened, acquire: acquire}
	return opened, nil
}

// Stat reports the pool's current connection counts.
func (p *Pool) Stat() *pgxpool.Stat { return p.pool.Stat() }

// opened lists this pool and every separate pool it has opened, in poolNames order.
func (p *Pool) opened() []openedPool {
	p.mu.Lock()
	defer p.mu.Unlock()
	opened := make([]openedPool, 1, 1+len(p.separates))
	opened[0] = openedPool{name: p.name, pool: p.pool, acquire: p.acquire}
	for _, name := range poolNames {
		if separate, ok := p.separates[name]; ok {
			opened = append(opened, separate)
		}
	}
	return opened
}

// Stats reports, for this pool and every separate pool it has opened, the pool's connection
// counts and what its acquire tracer has counted: what the metrics a scrape reads are made of.
func (p *Pool) Stats() []PoolStats {
	opened := p.opened()
	stats := make([]PoolStats, len(opened))
	for i, pool := range opened {
		stats[i] = PoolStats{Name: pool.name, Stat: pool.pool.Stat()}
		if pool.acquire != nil {
			stats[i].Acquire = pool.acquire.stats()
		}
	}
	return stats
}

// ConnectionUse is how many connections of a set of pools are in use, how many those pools have
// taken back since they opened, whether returned or discarded, and how many of those came back
// finished: handed back open and idle, so the work on them ended (acquireTracer.TraceRelease). A
// connection discarded because the database stopped answering - its statement cut off by its
// context, or its ping on acquire unanswered - counts in Returned and not in Finished.
type ConnectionUse struct {
	InUse    int32
	Returned int64
	Finished int64
}

// Use reports the ConnectionUse of the pools named, of those this pool has opened, or of every
// pool it has opened when no name is given. A caller asking whether work in flight has finished
// names the pools that work runs on and leaves the health probe's out: the probe is how a caller
// asks whether the database answers, and its connection taken back says nothing about the work. A
// pool opened without the acquire tracer counts no connection finished.
func (p *Pool) Use(names ...string) ConnectionUse {
	var use ConnectionUse
	for _, pool := range p.opened() {
		if len(names) > 0 && !slices.Contains(names, pool.name) {
			continue
		}
		stat := pool.pool.Stat()
		use.InUse += stat.AcquiredConns()
		use.Returned += stat.AcquireCount() - int64(stat.AcquiredConns())
		if pool.acquire != nil {
			use.Finished += int64(pool.acquire.finished.Load())
		}
	}
	return use
}

// busy maps an acquire that ran out this pool's bound to ErrPoolBusy (poolBusy).
func (p *Pool) busy(ctx context.Context, err error) error { return poolBusy(ctx, p.name, err) }

func (p *Pool) guard(ctx context.Context) error {
	if !holdsConnection(ctx) {
		return nil
	}
	logRefusal()
	return ErrNestedAcquire
}

// connection is what one call runs its statement on: the pgx pool, which takes a connection for
// the call, or the request's pinned connection (PinConnection). pgxpool's Pool and Conn share
// these methods.
type connection interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error)
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// target is what one call runs on - ctx's pinned connection where it pins one on this pool, else
// the pgx pool - and done, which gives the pinned connection back once the call is over, nil for
// a call on the pgx pool. It refuses a caller already holding a connection, and a call while
// another runs on the pinned connection, with ErrNestedAcquire. Every exported method that hands
// out a connection calls it directly, as guard is called, so a refusal records that method's
// caller.
func (p *Pool) target(ctx context.Context) (connection, func(), error) {
	state, _ := ctx.Value(holdingKey{}).(*holding)
	if state != nil && state.connection.Load() {
		logRefusal()
		return nil, nil, ErrNestedAcquire
	}
	pin := p.pinned(state)
	if pin == nil {
		return p.pool, nil, nil
	}
	conn, done, err := pin.take(ctx, p)
	if err != nil {
		if errors.Is(err, ErrNestedAcquire) {
			logRefusal()
		}
		return nil, nil, err
	}
	if conn == nil {
		return p.pool, nil, nil
	}
	return conn, done, nil
}

// finished gives a call's pinned connection back (target); a call on the pgx pool has nothing to.
func finished(done func()) {
	if done != nil {
		done()
	}
}

// joined is release, then done where the call ran on a pinned connection.
func joined(release, done func()) func() {
	if done == nil {
		return release
	}
	return func() {
		release()
		done()
	}
}

// Query runs a query on a pooled connection. The rows hold that connection until they close,
// so the caller counts as holding one for as long as they are open. A cursor is one of this
// pool's three connection-holders - a transaction, an open cursor, and a connection taken
// with Acquire (the durable append's withRoomLock, marked with HoldsConnection) - and work
// done inside the loop - a publish, a follower lookup, a write per row - is exactly the shape
// that wedges the pool. On a pinned connection an open cursor is a call still running on it, so a
// statement inside the loop is refused there too.
//
// Every method below that takes a connection returns ErrPoolBusy for an acquire that ran out the
// pool's bound (acquireTracer), however it reports its errors, and runs on the request's pinned
// connection where its context pins one (PinConnection).
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := on.Query(ctx, sql, args...)
	if err != nil {
		finished(done)
		return nil, p.busy(ctx, err)
	}
	// A context without a mark pins nothing, so done is nil whenever release is.
	release := hold(ctx)
	if release == nil {
		return rows, nil
	}
	return &trackedRows{Rows: rows, release: joined(release, done)}, nil
}

// trackedRows clears the caller's mark when pgx hands the cursor's connection back: an
// explicit Close, a Next that runs out of rows, or a Scan or Values error, which pgx treats as
// fatal and closes the rows on.
type trackedRows struct {
	pgx.Rows
	release func()
}

func (r *trackedRows) Close() {
	defer r.release()
	r.Rows.Close()
}

func (r *trackedRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.release()
	return false
}

func (r *trackedRows) Scan(dest ...any) error {
	err := r.Rows.Scan(dest...)
	if err != nil {
		r.release()
	}
	return err
}

func (r *trackedRows) Values() ([]any, error) {
	values, err := r.Rows.Values()
	if err != nil {
		r.release()
	}
	return values, err
}

// QueryRow runs a single-row query on a pooled connection. pgxpool defers an acquire's error to
// the row's Scan, so the row maps it there.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	on, done, err := p.target(ctx)
	if err != nil {
		return errRow{err: err}
	}
	return busyRow{Row: on.QueryRow(ctx, sql, args...), ctx: ctx, pool: p.name, done: done}
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := on.Exec(ctx, sql, args...)
	finished(done)
	return tag, p.busy(ctx, err)
}

// Acquire takes a pooled connection the caller holds until it releases it. Under a pin it hands
// over the request's pinned connection, if it holds one, rather than taking a second; the request's
// next call takes another.
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := p.handOver(ctx)
	if conn != nil || err != nil {
		return conn, err
	}
	conn, err = p.pool.Acquire(ctx)
	return conn, p.busy(ctx, err)
}

// handOver is Acquire's refusal and its pinned connection: ErrNestedAcquire for a caller holding a
// connection or a pinned connection a call runs on, the pinned connection where ctx pins an idle
// one on this pool, and nil otherwise.
func (p *Pool) handOver(ctx context.Context) (*pgxpool.Conn, error) {
	state, _ := ctx.Value(holdingKey{}).(*holding)
	if state != nil && state.connection.Load() {
		logRefusal()
		return nil, ErrNestedAcquire
	}
	pin := p.pinned(state)
	if pin == nil {
		return nil, nil
	}
	conn, err := pin.handOver(p)
	if err != nil {
		logRefusal()
	}
	return conn, err
}

// AcquireFunc runs fn with a pooled connection - the pinned one under a pin. Only the acquire's
// error is mapped: fn's own is returned as fn returned it.
func (p *Pool) AcquireFunc(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	on, done, err := p.target(ctx)
	if err != nil {
		return err
	}
	if pinned, ok := on.(*pgxpool.Conn); ok {
		defer done()
		return fn(pinned)
	}
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return p.busy(ctx, err)
	}
	defer conn.Release()
	return fn(conn)
}

// AcquireAllIdle takes every idle connection the pool holds.
func (p *Pool) AcquireAllIdle(ctx context.Context) []*pgxpool.Conn {
	if err := p.guard(ctx); err != nil {
		return nil
	}
	return p.pool.AcquireAllIdle(ctx)
}

// CopyFrom streams rows into a table on a pooled connection.
func (p *Pool) CopyFrom(
	ctx context.Context,
	table pgx.Identifier,
	columns []string,
	source pgx.CopyFromSource,
) (int64, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return 0, err
	}
	copied, err := on.CopyFrom(ctx, table, columns, source)
	finished(done)
	return copied, p.busy(ctx, err)
}

// SendBatch runs a batch on a pooled connection. pgxpool defers an acquire's error to the
// results, so the results map it there.
func (p *Pool) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	on, done, err := p.target(ctx)
	if err != nil {
		return errBatchResults{err: err}
	}
	return busyBatchResults{BatchResults: on.SendBatch(ctx, batch), ctx: ctx, pool: p.name, done: done}
}

// Begin opens a transaction and marks the request as holding one.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return nil, err
	}
	return p.begin(ctx, on, done, pgx.TxOptions{})
}

// BeginTx opens a transaction with options and marks the request as holding one.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return nil, err
	}
	return p.begin(ctx, on, done, options)
}

// begin is the body both transaction entry points share, on what target gave them: target runs in
// the exported method, as it does in every exported method that hands out one of this pool's
// connections, so a refusal records the caller that asked for the transaction rather than one of
// this pool's own frames. A transaction on a pinned connection gives it back when it ends.
func (p *Pool) begin(ctx context.Context, on connection, done func(), options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := on.BeginTx(ctx, options)
	if err != nil {
		finished(done)
		return nil, p.busy(ctx, err)
	}
	release := hold(ctx)
	if release == nil {
		release = func() {}
	}
	beforeCommit, _ := ctx.Value(beforeCommitKey{}).(BeforeCommit)
	return &trackedTx{Tx: tx, release: joined(release, done), beforeCommit: beforeCommit}, nil
}

// BeforeCommit is a step every transaction a request opens takes just before its COMMIT, inside
// that transaction, so what it writes commits with the request's own write or not at all. An
// error from it rolls the transaction back, and Commit returns that error.
type BeforeCommit func(ctx context.Context, tx pgx.Tx) error

type beforeCommitKey struct{}

// WithBeforeCommit returns ctx carrying step, which every transaction this pool opens under ctx,
// or a context derived from it, takes before it commits (BeforeCommit). A keyed write marks its
// Idempotency-Key committed this way, in the transaction of the write itself (api/request_keys.go).
// A transaction already failed when its caller commits it skips the step: pgx answers that commit
// with its own rollback error, as it would without one.
func WithBeforeCommit(ctx context.Context, step BeforeCommit) context.Context {
	return context.WithValue(ctx, beforeCommitKey{}, step)
}

// trackedTx commits apart from its caller's cancellation (CommitContext), clears the caller's
// mark however the transaction ends, and takes the caller's BeforeCommit step before it commits.
// Callers commit and then run a deferred rollback; the release absorbs that second call. Either
// part may be absent: a context the pool does not track has no mark, and one with no step commits
// as it is.
type trackedTx struct {
	pgx.Tx
	release      func()
	beforeCommit BeforeCommit
}

func (t *trackedTx) Commit(ctx context.Context) error {
	defer t.end()
	ctx, cancel := CommitContext(ctx)
	defer cancel()
	if t.beforeCommit != nil && t.Tx.Conn().PgConn().TxStatus() == 'T' {
		if err := t.beforeCommit(ctx, t.Tx); err != nil {
			_ = t.Tx.Rollback(ctx)
			return err
		}
	}
	return t.Tx.Commit(ctx)
}

func (t *trackedTx) Rollback(ctx context.Context) error {
	defer t.end()
	return t.Tx.Rollback(ctx)
}

// commitTimeout bounds a commit CommitContext has detached from its caller.
const commitTimeout = 10 * time.Second

// CommitContext is the context a transaction commits on: ctx's values without its cancellation
// or deadline, bounded at commitTimeout. A commit cut short by its caller - a client that went
// away, a caller's deadline that passed - has an unknown outcome, since the server may have
// committed it, and the document service fails every room such a transaction wrote, which then
// reload the durable document (docs.Ledger.Commit); a request that ended mid-commit would take its
// rooms down with it. So a commit in flight runs to its end, and only its own bound, ten seconds,
// keeps one the database never answers from holding its connection for good. Every transaction
// this pool opens commits on it.
func CommitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
}

// end clears the caller's mark, when the pool tracks one.
func (t *trackedTx) end() {
	if t.release != nil {
		t.release()
	}
}

type errRow struct {
	err error
}

func (r errRow) Scan(...any) error { return r.err }

// errBatchResults carries a refusal through the batch surface, which has no error return of
// its own: every result the caller reads is the refusal.
type errBatchResults struct {
	err error
}

func (b errBatchResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, b.err }
func (b errBatchResults) Query() (pgx.Rows, error)         { return nil, b.err }
func (b errBatchResults) QueryRow() pgx.Row                { return errRow{err: b.err} }
func (b errBatchResults) Close() error                     { return b.err }

// busyRow maps a deferred acquire error to ErrPoolBusy when the row is scanned, and gives back the
// pinned connection the row was read on, if any, once it is.
type busyRow struct {
	pgx.Row
	ctx  context.Context
	pool string
	done func()
}

func (r busyRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	finished(r.done)
	return poolBusy(r.ctx, r.pool, err)
}

// busyBatchResults maps a deferred acquire error to ErrPoolBusy in every result the caller reads,
// and gives back the pinned connection the batch ran on, if any, once it closes.
type busyBatchResults struct {
	pgx.BatchResults
	ctx  context.Context
	pool string
	done func()
}

func (b busyBatchResults) Exec() (pgconn.CommandTag, error) {
	tag, err := b.BatchResults.Exec()
	return tag, poolBusy(b.ctx, b.pool, err)
}

func (b busyBatchResults) Query() (pgx.Rows, error) {
	rows, err := b.BatchResults.Query()
	return rows, poolBusy(b.ctx, b.pool, err)
}

func (b busyBatchResults) QueryRow() pgx.Row {
	return busyRow{Row: b.BatchResults.QueryRow(), ctx: b.ctx, pool: b.pool}
}

func (b busyBatchResults) Close() error {
	err := b.BatchResults.Close()
	finished(b.done)
	return poolBusy(b.ctx, b.pool, err)
}
