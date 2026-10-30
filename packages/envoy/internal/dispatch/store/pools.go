package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file holds the pools of its own a Pool opens beside the one it wraps - their sizes and the
// budget those sizes partition (ConnectionsPerProcess), the accessor each caller reaches its pool
// through, the opening itself and what the metrics read off every pool that is open. The guard,
// the pin and the statement wrappers are in pool.go.

// openedPool is one pgx pool a Pool has open, under its name, with its acquire tracer.
type openedPool struct {
	name    string
	pool    *pgxpool.Pool
	acquire *acquireTracer
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

// separate is the pgx pool of the separate pool named name (open).
func (p *Pool) separate(name string, size int32) (*pgxpool.Pool, error) {
	opened, err := p.open(name, size)
	if err != nil {
		return nil, err
	}
	return opened.pool, nil
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
	return openedPool{name: name, pool: pool, acquire: acquire}, nil
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
func (p *Pool) OpenRegistry(ctx context.Context) (*pgxpool.Pool, error) {
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
	return opened.pool, nil
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
