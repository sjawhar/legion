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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNestedAcquire is the shared pool refusing a second connection to a caller that already
// holds an open transaction.
//
// One transaction, one connection, is the rule the pool cannot survive without. A transaction
// holds its connection until it commits; a caller that asks for a second one while it holds a
// row or advisory lock waits for a connection only the callers queued behind that lock can
// release, and they are waiting for the lock. At four connections - production's pool, one
// Fargate task at cpu="512" - two anchored writes and two document settlements on one issue are
// enough, and nothing but pg_terminate_backend recovers it. Rationing connections cannot fix
// that, because the queue behind one writer's issue lock is unbounded: a settlement per
// document, every issue-owned event append, the architecture importer.
//
// So work that genuinely needs a connection of its own while a transaction is open does not
// take it from the shared pool: a cold document room loads on the rooms pool (Pool.Rooms),
// because that load outlives the request and must not roll back with it. Everything else reads
// through the transaction it is already inside.
//
// This error makes the rule enforce itself. Every entry point that opens one of this pool's
// transactions marks its context (api.Register marks every route; settlement, the architecture
// importer, the outbox publisher, the CLI backfills, the startup seeds and the migrations mark
// their own goroutines), and an acquisition that arrives under a held connection fails here,
// loudly, instead of wedging production.
var ErrNestedAcquire = errors.New(
	"a transaction is already open on this request: a second pooled connection would deadlock the pool",
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
	ctx, held := mark(WithTransactionTracking(ctx))
	return ctx, held
}

// mark records that ctx's caller now holds a connection, and returns the release that clears
// it. A context with no marker takes the flag anyway, so the caller needs no branch.
func mark(ctx context.Context) (context.Context, func()) {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	if !ok {
		state = &holding{}
		ctx = context.WithValue(ctx, holdingKey{}, state)
	}
	state.connection.Store(true)
	return ctx, func() { state.connection.Store(false) }
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
	slog.Error("dispatch: second pooled connection requested inside a transaction",
		"error", ErrNestedAcquire, "stack", string(debug.Stack()))
}

// Pool is the shared Dispatch connection pool. Every way it hands out a connection refuses one
// asked for while the caller already holds one; see ErrNestedAcquire. The pgx pool is private
// so no caller can reach past the refusal.
type Pool struct {
	pool *pgxpool.Pool
	// mu guards the rooms pool's creation against its close: one lock, so a Rooms that wins
	// the race returns the error rather than opening a pool nothing will ever close.
	mu     sync.Mutex
	rooms  *pgxpool.Pool
	closed bool
}

// NewPool wraps an open pgx pool.
func NewPool(pool *pgxpool.Pool) *Pool {
	return &Pool{pool: pool}
}

// roomsPoolSize bounds the connections document loads use. Loading takes no lock and always
// finishes, so one is enough for the pool to be deadlock-free; a few let cold rooms load
// concurrently. Measured on the real server, eight cold rooms loading at once are no faster at
// eight connections than at one, so this is small on purpose.
const roomsPoolSize = 4

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
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	if p.rooms != nil {
		return p.rooms, nil
	}
	config := p.pool.Config().Copy()
	config.MaxConns = roomsPoolSize
	config.MinConns = 0
	rooms, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("open document rooms pool: %w", err)
	}
	p.rooms = rooms
	return p.rooms, nil
}

// Close releases every connection the pool holds, the document rooms included.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	rooms := p.rooms
	p.rooms = nil
	p.mu.Unlock()
	if rooms != nil {
		rooms.Close()
	}
	p.pool.Close()
}

// Config returns a copy of the pool's configuration.
func (p *Pool) Config() *pgxpool.Config { return p.pool.Config() }

// Stat reports the pool's current connection counts.
func (p *Pool) Stat() *pgxpool.Stat { return p.pool.Stat() }

// Ping checks the database is reachable on a pooled connection.
func (p *Pool) Ping(ctx context.Context) error {
	if err := p.guard(ctx); err != nil {
		return err
	}
	return p.pool.Ping(ctx)
}

func (p *Pool) guard(ctx context.Context) error {
	if !holdsConnection(ctx) {
		return nil
	}
	logRefusal()
	return ErrNestedAcquire
}

// Query runs a query on a pooled connection.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.pool.Query(ctx, sql, args...)
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
	return p.BeginTx(ctx, pgx.TxOptions{})
}

// BeginTx opens a transaction with options and marks the request as holding one.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	_, release := mark(ctx)
	return &trackedTx{Tx: tx, release: release}, nil
}

// trackedTx clears the caller's mark however the transaction ends. Callers commit and then run
// a deferred rollback, so clearing twice has to be harmless - it is: the flag is set to false.
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
