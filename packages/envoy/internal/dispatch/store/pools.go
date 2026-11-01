package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file holds the pools of its own a Pool opens beside the one it wraps - their sizes and the
// budget those sizes partition (ConnectionsPerProcess), the accessor each caller reaches its pool
// through, the opening itself and what the metrics read off every pool that is open. The guard,
// the pin and the statement wrappers are in pool.go.

// openedPool is one pgx pool a Pool has open, under its name, with its acquire tracer and the raw
// pool its callers reach it through where it is handed out whole (RawPool).
type openedPool struct {
	name    string
	pool    *pgxpool.Pool
	acquire *acquireTracer
	raw     *RawPool
}

// RawPool is a separate pool handed out whole, as the pgx pool it is: the rooms pool a document
// load opens its own transaction on, the leases pool a maintenance pass holds its lease on for a
// whole pass, the listen pool whose connection is hijacked, the horizon pool the event log's safe
// horizon batches its observations on, the registry's and the health probe's.
// Each needs what only a pgx pool gives, which is why these are not wrapped (Pool.wrapped) and why
// their callers stand outside the nested-acquire guard: a separate pool cannot close the cycle the
// guard prevents.
//
// Every way it hands out a connection answers an acquire that ran out the pool's bound with
// ErrPoolBusy, as a wrapped pool does (Pool.busy), so its callers tell a pool with nothing free
// from a deadline of their own by IsPoolBusy. Its other methods - Close, Config, Stat - are the
// pgx pool's own and take no connection.
type RawPool struct {
	*pgxpool.Pool
	name string
}

// busy maps an acquire that ran out this pool's bound to ErrPoolBusy (poolBusy).
func (r *RawPool) busy(ctx context.Context, err error) error { return poolBusy(ctx, r.name, err) }

// Acquire takes a connection the caller holds until it releases it: the lease a maintenance pass
// holds for its whole pass, and the connection Listener hijacks.
func (r *RawPool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := r.Pool.Acquire(ctx)
	return conn, r.busy(ctx, err)
}

// AcquireFunc runs fn with a connection of the pool. Only the acquire's error is mapped: fn's own
// is returned as fn returned it.
func (r *RawPool) AcquireFunc(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	conn, err := r.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return fn(conn)
}

// Query runs a query on a connection of the pool, which the rows hold until they close.
func (r *RawPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := r.Pool.Query(ctx, sql, args...)
	return rows, r.busy(ctx, err)
}

// QueryRow runs a single-row query on a connection of the pool. pgxpool defers an acquire's error
// to the row's Scan, so the row maps it there.
func (r *RawPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return busyRow{Row: r.Pool.QueryRow(ctx, sql, args...), ctx: ctx, pool: r.name}
}

// Exec runs a statement on a connection of the pool.
func (r *RawPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := r.Pool.Exec(ctx, sql, args...)
	return tag, r.busy(ctx, err)
}

// SendBatch runs a batch on a connection of the pool. pgxpool defers an acquire's error to the
// results, so the results map it there.
func (r *RawPool) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	return busyBatchResults{BatchResults: r.Pool.SendBatch(ctx, batch), ctx: ctx, pool: r.name}
}

// CopyFrom streams rows into a table on a connection of the pool.
func (r *RawPool) CopyFrom(
	ctx context.Context,
	table pgx.Identifier,
	columns []string,
	source pgx.CopyFromSource,
) (int64, error) {
	copied, err := r.Pool.CopyFrom(ctx, table, columns, source)
	return copied, r.busy(ctx, err)
}

// Begin opens a transaction of its own on a connection of the pool. Unlike a wrapped pool's it is
// the pgx transaction itself: it commits on its caller's context, not apart from it
// (CommitContext), since these pools' transactions are the loads and registrations that belong to
// no request.
func (r *RawPool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := r.Pool.Begin(ctx)
	return tx, r.busy(ctx, err)
}

// BeginTx opens a transaction with options, as Begin opens one.
func (r *RawPool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := r.Pool.BeginTx(ctx, options)
	return tx, r.busy(ctx, err)
}

// Ping takes a connection and asks the database to answer on it.
func (r *RawPool) Ping(ctx context.Context) error {
	return r.busy(ctx, r.Pool.Ping(ctx))
}

// SharedPoolSize bounds the shared pool - the request pool - deliberately and in code. pgx's
// default, max(4, NumCPU), is four on production's one Fargate task at cpu="512", and a
// connection per blocked writer is all it takes to starve everything else: with four writers
// parked on one issue's row lock, a read of an unrelated issue waited the whole seventeen seconds
// the lock was held, where sixteen connections served 3,853 such reads at a four-millisecond
// median. The ceiling is picked here rather than derived from the task's CPU allotment or read
// out of the DSN another repository's URL builder writes.
//
// It is 48 of one connection budget of 90 (ConnectionsPerProcess) that every pool of a process
// shares, partitioned rather than grown: requests 48, background 6 (Background), live 4 (Live),
// settlements 4 (Settlements), rooms 4, health 1, the four permanent listeners (the listener
// pool's one dial is one of them redialling), leases leasePoolSize (15), outbox 1, the event
// horizon 2 (Horizon) and the server process registry's own 1 (registryPoolSize). The partition
// itself added nothing: it is the 87 the process opened before, the request pool having given up
// 16, which background, live, settlements and the leases' two spare connections took; the event
// horizon's 2 and the background loops' shared wake listener are the only connections added
// since, and the horizon's came with the pool itself. A write
// takes a connection per statement one after another, and a read pins one for its whole length
// (PinConnection), so under load the API holds every connection of this pool; the background
// loops, the live-update readers and the settlements wait on pools of their own, so neither side
// can starve the other, and an acquire here waits at most requestAcquireBound before it is
// refused with ErrPoolBusy rather than queueing without end. Admission derives the units it may
// hand out at once from this number, the connections left to the pool's uncounted users taken off
// (admission.MaxAdmittedUnits), so a change here moves that cap with it.
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
const SharedPoolSize = 48

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
// the request pool's, and a load that waits that bound out is refused with ErrPoolBusy (RawPool).
func (p *Pool) Rooms() (*RawPool, error) {
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

// listenerPoolSize bounds the pool Listener takes its connections from. Four permanent
// listeners dial through it - the delivery hub, the event stream relay, the stale-room evictor
// and the shared hub cmd/dispatch's woken background loops wait on (pglisten.Hub, one
// connection for all of their channels) - and each connection is hijacked as soon as it is
// opened, so it leaves with its caller and the pool holds none between dials: what one bounds is
// dials in flight, not connections held. A listener redialling while another dials waits that
// dial out, which is what a loop that dials and then waits for notifications for the life of the
// process can afford.
const listenerPoolSize = 1

// Listener returns one connection of its own for LISTEN, from a one-connection pool opened from the
// shared pool's configuration and hijacked, so it holds no shared connection. The caller closes it.
// The delivery hub (api/delivery_live_hub.go), the event stream relay (api/event_stream_relay.go),
// the document rooms' stale-room evictor (docs/stale_room.go) and the background loops' shared
// wake hub (cmd/dispatch) each hold one for as long as they wait for notifications, which is the
// life of the process: a connection of the shared pool held that long would be one fewer for
// every request, and a LISTEN on a pooled connection would end when the pool recycled it. A dial
// that does not get the pool's connection within listenAcquireBound is refused with ErrPoolBusy.
func (p *Pool) Listener(ctx context.Context) (*pgx.Conn, error) {
	listen, err := p.separate(ListenPool, listenerPoolSize)
	if err != nil {
		return nil, err
	}
	conn, err := listen.Acquire(ctx)
	if err != nil {
		return nil, err
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
// connections (ErrNestedAcquire). Its acquires are bounded at leaseAcquireBound, and a pass that
// waits that bound out is refused with ErrPoolBusy (RawPool), which makes it skip its tick.
func (p *Pool) Leases() (*RawPool, error) {
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
// of the process (Listener): the delivery hub, the event stream relay, the stale-room evictor and
// the shared wake hub cmd/dispatch's woken background loops wait on, which main.go dials through
// database.Pool.Listener.
const permanentListeners = 4

// ConnectionsPerProcess is the most connections one Dispatch server process opens to Postgres, the
// budget of 90 SharedPoolSize describes: every pool's size, the registry's own pool
// (processes.Register) and the event horizon's (Horizon) among them, and one connection per
// permanent listener, which the listener pool dials and hands off rather than holds (its one dial
// in flight is a permanent listener redialling, so it is not counted twice).
const ConnectionsPerProcess = SharedPoolSize + backgroundPoolSize + livePoolSize + settlementsPoolSize +
	roomsPoolSize + healthPoolSize + leasePoolSize + outboxPoolSize + horizonPoolSize + registryPoolSize +
	permanentListeners

// Outbox is the pool the events outbox claims, reads and records events through, and reads the
// followers and authors a publish routes to. Under load the API's requests queue for the shared
// pool's connections, and an outbox statement queued behind them holds back every event behind
// it; on a pool of its own it waits for none of them, and holds no connection a request could use.
func (p *Pool) Outbox() (*Pool, error) {
	return p.wrapped(OutboxPool, outboxPoolSize)
}

// horizonPoolSize bounds the pool the event log's safe horizon (events.Horizon) works through. The
// server keeps one Horizon, whose concurrent callers share the one observation in flight, so one
// connection serves every observation. The other statements the Horizon runs share both
// connections with it: the stall diagnosis and the write of the proven horizon (event_horizon),
// which run after an observation has released its waiters, and the reads every GET /auth/whoami
// and the relay's first start make (a mark, the oldest transaction that can write the event log,
// the persisted horizon), each one statement.
const horizonPoolSize = 2

// Horizon is the pool the event log's safe horizon (events.Horizon) takes its observations, its
// marks and the reads beside them, its stall diagnosis and its persist write through. Every
// reader that pages the event log by a global id cursor - the event stream relay, the delivery
// hub, a references read since a cursor - reads only up to what an observation has proven safe.
// Under load the API's requests queue for the shared pool's connections, and an observation
// queued behind them holds back every live reader of the event log; on a pool of its own it waits
// for none of them, and holds no connection a request could use. Like the rooms pool it is handed
// out raw and its callers stand outside the nested-acquire guard, because a separate pool cannot
// close the cycle the guard prevents: none of those statements (each one batch or one row) asks
// for a second connection while it holds one, and a page's whoami takes one while its request
// holds nothing else. Its acquires are bounded at requestAcquireBound, since a page's load waits
// on one, and an acquire that waits that bound out is refused with ErrPoolBusy (RawPool).
func (p *Pool) Horizon() (*RawPool, error) {
	return p.separate(HorizonPool, horizonPoolSize)
}

// backgroundPoolSize bounds the pool for work no request waits on: background loops such as the
// embedding queue and its coverage pass, the suggestion sweep, the delivery reconcile and intakes,
// the architecture importer's ticker, the documents' resumption query, task progress,
// reference-excerpt and block-index reconciliation, the edit-notice sweep, the repair of closed and
// reopened issues' asks, the server process registry's prune and the fleet build announcer. Six is
// the background's share of the budget SharedPoolSize describes; nobody waits on these loops, so
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
// Four is their share of the budget SharedPoolSize describes.
const livePoolSize = 4

// Live is the pool for the live-update readers, so an event reaches the open streams while the
// requests hold every connection of theirs; its acquires are bounded at requestAcquireBound, since
// a stream is waiting on each one.
func (p *Pool) Live() (*Pool, error) {
	return p.wrapped(LivePool, livePoolSize)
}

// settlementsPoolSize bounds the pool for document settlements. Four is the settlements' share of
// the budget SharedPoolSize describes.
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

// SetOpenFailureForTest makes the separate pool named name fail to open with err, in place of
// opening it. It is for tests outside this package alone, which see what a caller does when the
// pool it works through cannot be opened; nothing in production calls it, since a pool opens from
// this pool's own configuration. A pool already open fails the same way, so what the test sees
// does not depend on what opened the pool first.
func (p *Pool) SetOpenFailureForTest(name string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.openFailures == nil {
		p.openFailures = make(map[string]error)
	}
	p.openFailures[name] = err
}

// separate is the raw pool of the separate pool named name (open): its pgx pool with the acquires
// its callers make mapped to ErrPoolBusy.
func (p *Pool) separate(name string, size int32) (*RawPool, error) {
	opened, err := p.open(name, size)
	if err != nil {
		return nil, err
	}
	return opened.raw, nil
}

// open opens the pool named name on demand, sized at size (newSeparate), and closes it with this
// pool. Each caller holds mu for the whole check-and-open so two of them cannot open two pools,
// and one opened after Close would be a pool nobody closes.
func (p *Pool) open(name string, size int32) (openedPool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return openedPool{}, ErrPoolClosed
	}
	if err, failing := p.openFailures[name]; failing {
		return openedPool{}, err
	}
	if opened, ok := p.separates[name]; ok {
		return opened, nil
	}
	opened, err := p.newSeparate(context.Background(), name, size)
	if err != nil {
		return openedPool{}, err
	}
	p.recordLocked(opened)
	return opened, nil
}

// newSeparate opens one separate pool: size connections copied from this pool's configuration and
// configured as its own (configurePool: its application_name and its acquire bound, acquireBounds'
// where a test set one, else defaultAcquireBound's). Its callers hold mu, which is what keeps two
// of them from opening two pools under one name and an open from racing this pool's close; pgx
// opens no connection here, so nothing waits on the database under the lock.
func (p *Pool) newSeparate(ctx context.Context, name string, size int32) (openedPool, error) {
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
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return openedPool{}, fmt.Errorf("open %s pool: %w", name, err)
	}
	return openedPool{name: name, pool: pool, acquire: acquire, raw: &RawPool{Pool: pool, name: name}}, nil
}

// recordLocked keeps opened among this pool's separate pools, in place of one already under its
// name, so Stats reports it and Close closes it. Its callers hold mu.
func (p *Pool) recordLocked(opened openedPool) {
	if p.separates == nil {
		p.separates = make(map[string]openedPool, len(poolNames))
	}
	p.separates[opened.name] = opened
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

// OpenRegistry opens the server process registry's own pool, which processes.Register uses and its
// Deregister closes: this pool's configuration sized at registryPoolSize and named as the
// registry's (configurePool, RegistryPool), so its connection is dispatch-registry in
// pg_stat_activity and its acquires are counted apart from the requests'. The pool is recorded
// with the others, replacing one an earlier registration opened, so Stats and the metrics report
// the heartbeat's connection and its waits as they report every other pool's, and Close closes it
// with them; closing it twice is harmless, since a pgx pool closes once.
func (p *Pool) OpenRegistry(ctx context.Context) (*RawPool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	opened, err := p.newSeparate(ctx, RegistryPool, registryPoolSize)
	if err != nil {
		return nil, err
	}
	p.recordLocked(opened)
	return opened.raw, nil
}

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
