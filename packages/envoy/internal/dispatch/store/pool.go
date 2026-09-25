package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNestedAcquire is the shared pool refusing a second connection to a caller that already
// holds one of its connections.
//
// One caller, one connection, is the rule the pool cannot survive without. A transaction
// holds its connection until it commits; a caller that asks for a second one while it holds a
// row or advisory lock waits for a connection only the callers queued behind that lock can
// release, and they are waiting for the lock. Two anchored writes and two document
// settlements on one issue closed that cycle on a four-connection pool, and nothing but
// pg_terminate_backend recovered it. Rationing connections cannot fix it, and neither can
// sharedPoolSize's sixteen: the queue behind one writer's issue lock is unbounded - a
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
// transaction, or one it acquired directly. One writer sets it and one reader tests it, so a
// flag is the whole state - a transaction never opens a second transaction of its own.
type holding struct {
	connection atomic.Bool
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

// Pool is the shared Dispatch connection pool. Every way it hands out a connection refuses one
// asked for while the caller already holds one; see ErrNestedAcquire. The pgx pool is private
// so no caller can reach past the refusal.
type Pool struct {
	pool *pgxpool.Pool
	// mu guards each separate pool's creation against this pool's close: one lock, so a
	// caller that loses the race is told the pool is closed rather than opening one nothing
	// will ever close.
	mu     sync.Mutex
	rooms  *pgxpool.Pool
	health *pgxpool.Pool
	closed bool
}

// NewPool wraps an open pgx pool.
func NewPool(pool *pgxpool.Pool) *Pool {
	return &Pool{pool: pool}
}

// sharedPoolSize bounds the shared pool, deliberately and in code. pgx's default,
// max(4, NumCPU), is four on production's one Fargate task at cpu="512", and a connection per
// blocked writer is all it takes to starve everything else: with four writers parked on one
// issue's row lock, a read of an unrelated issue waited the whole seventeen seconds the lock
// was held, where sixteen connections served 3,853 such reads at a four-millisecond median.
// The shared Aurora cluster has thousands of connections spare, so the ceiling is picked here
// rather than derived from the task's CPU allotment or read out of the DSN another
// repository's URL builder writes.
const sharedPoolSize = 16

// roomsPoolSize bounds the connections document loads use. Loading takes no lock and always
// finishes, so one is enough for the pool to be deadlock-free; a few let cold rooms load
// concurrently. Measured on the real server, eight cold rooms loading at once are no faster at
// eight connections than at one, so this is small on purpose.
const roomsPoolSize = 4

// healthPoolSize bounds the connections the health probe uses. The probe answers one question -
// is Postgres reachable - and asks it on a connection nothing else can take, so one is not a
// ration but the whole design.
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
// per caller who wants to read a document.
func (p *Pool) Rooms() (*pgxpool.Pool, error) {
	return p.separate(&p.rooms, roomsPoolSize, "document rooms")
}

// Healthy reports whether Postgres is reachable, within healthProbeTimeout, on a connection of
// a pool nothing else uses. /healthz asks it, and it must never queue behind a writer: proving
// the shared pool has a free connection is the wrong question, because during a busy period it
// legitimately has none. Like the rooms pool this one is outside the nested-acquire guard,
// because a separate pool cannot close the cycle the guard prevents.
func (p *Pool) Healthy(ctx context.Context) error {
	health, err := p.separate(&p.health, healthPoolSize, "health probe")
	if err != nil {
		return err
	}
	// Derived from the caller's context, so a request the client already gave up on ends with
	// it instead of holding the probe's one connection for the full bound.
	ctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	return health.Ping(ctx)
}

// separate opens the pool held at field on demand, sized at size and copied from the shared
// pool's configuration, and closes it with the shared pool. Each caller holds mu for the whole
// check-and-open so two of them cannot open two pools, and one opened after Close would be a
// pool nobody closes.
func (p *Pool) separate(field **pgxpool.Pool, size int32, name string) (*pgxpool.Pool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	if *field != nil {
		return *field, nil
	}
	config := p.pool.Config().Copy()
	config.MaxConns = size
	// Both minimums, not just MinConns: a floor inherited from the shared pool's DSN would be
	// a target these pools cannot reach - pool_min_idle_conns=2 on a one-connection health
	// pool - which pgxpool re-attempts, and puddle refuses as full, every health-check period.
	config.MinConns = 0
	config.MinIdleConns = 0
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("open %s pool: %w", name, err)
	}
	*field = opened
	return opened, nil
}

// Close releases every connection the pool holds, the document rooms and the health probe
// included.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	rooms, health := p.rooms, p.health
	p.rooms, p.health = nil, nil
	p.mu.Unlock()
	if rooms != nil {
		rooms.Close()
	}
	if health != nil {
		health.Close()
	}
	p.pool.Close()
}

// Config returns a copy of the pool's configuration.
func (p *Pool) Config() *pgxpool.Config { return p.pool.Config() }

// Stat reports the pool's current connection counts.
func (p *Pool) Stat() *pgxpool.Stat { return p.pool.Stat() }

func (p *Pool) guard(ctx context.Context) error {
	if !holdsConnection(ctx) {
		return nil
	}
	logRefusal()
	return ErrNestedAcquire
}

// Query runs a query on a pooled connection. The rows hold that connection until they close,
// so the caller counts as holding one for as long as they are open. A cursor is one of this
// pool's three connection-holders - a transaction, an open cursor, and a connection taken
// with Acquire (the durable append's withRoomLock, marked with HoldsConnection) - and work
// done inside the loop - a publish, a follower lookup, a write per row - is exactly the shape
// that wedges the pool.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	release := hold(ctx)
	if release == nil {
		return rows, nil
	}
	return &trackedRows{Rows: rows, release: release}, nil
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

// QueryRow runs a single-row query on a pooled connection.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := p.guard(ctx); err != nil {
		return errRow{err: err}
	}
	return p.pool.QueryRow(ctx, sql, args...)
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := p.guard(ctx); err != nil {
		return pgconn.CommandTag{}, err
	}
	return p.pool.Exec(ctx, sql, args...)
}

// Acquire takes a pooled connection the caller holds until it releases it.
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.pool.Acquire(ctx)
}

// AcquireFunc runs fn with a pooled connection.
func (p *Pool) AcquireFunc(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	if err := p.guard(ctx); err != nil {
		return err
	}
	return p.pool.AcquireFunc(ctx, fn)
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
	if err := p.guard(ctx); err != nil {
		return 0, err
	}
	return p.pool.CopyFrom(ctx, table, columns, source)
}

// SendBatch runs a batch on a pooled connection.
func (p *Pool) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if err := p.guard(ctx); err != nil {
		return errBatchResults{err: err}
	}
	return p.pool.SendBatch(ctx, batch)
}

// Begin opens a transaction and marks the request as holding one.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.begin(ctx, pgx.TxOptions{})
}

// BeginTx opens a transaction with options and marks the request as holding one.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.begin(ctx, options)
}

// begin is the guarded body both transaction entry points share: guard runs in the exported
// method, as it does in every exported method that hands out one of this pool's connections,
// so a refusal records the caller that asked for the transaction rather than one of this
// pool's own frames.
func (p *Pool) begin(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	release := hold(ctx)
	if release == nil {
		return tx, nil
	}
	return &trackedTx{Tx: tx, release: release}, nil
}

// trackedTx clears the caller's mark however the transaction ends. Callers commit and then run
// a deferred rollback; the release absorbs that second call.
type trackedTx struct {
	pgx.Tx
	release func()
}

func (t *trackedTx) Commit(ctx context.Context) error {
	defer t.release()
	return t.Tx.Commit(ctx)
}

func (t *trackedTx) Rollback(ctx context.Context) error {
	defer t.release()
	return t.Tx.Rollback(ctx)
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
