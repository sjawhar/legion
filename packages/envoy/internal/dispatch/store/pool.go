package store

import (
	"context"
	"errors"
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
// take it from this pool: a cold document room loads on the rooms pool docs owns
// (docs.NewPgVersioned), because that load outlives the request and must not roll back with it.
// Everything else reads through the transaction it is already inside.
//
// This error makes the rule enforce itself. Every transaction this pool opens marks the request
// (api.Register installs the marker on every route; the document service and the architecture
// importer install it on their own goroutines), and an acquisition that arrives under an open
// mark fails here, loudly, instead of wedging production.
var ErrNestedAcquire = errors.New(
	"a transaction is already open on this request: a second pooled connection would deadlock the pool",
)

type txMarker struct {
	open atomic.Int32
}

type txMarkerKey struct{}

// WithTransactionTracking marks ctx so the pool can refuse a second connection while one of its
// transactions is open. API requests are marked by middleware; every background entry point
// that opens a transaction - settlement, the architecture importer, the outbox publisher, the
// CLI backfills, startup seeding - marks its own context.
func WithTransactionTracking(ctx context.Context) context.Context {
	if ctx.Value(txMarkerKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, txMarkerKey{}, &txMarker{})
}

// HoldsConnection marks ctx as already holding a pooled connection until the returned release
// runs, so an acquisition under it is refused like one inside a transaction. The durable
// document appends hold their connection directly, outside any transaction of this pool's.
func HoldsConnection(ctx context.Context) (context.Context, func()) {
	marker := &txMarker{}
	marker.open.Add(1)
	return context.WithValue(ctx, txMarkerKey{}, marker), func() { marker.open.Store(0) }
}

func markerFrom(ctx context.Context) *txMarker {
	marker, _ := ctx.Value(txMarkerKey{}).(*txMarker)
	return marker
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

// Pool is the shared Dispatch connection pool. Its guarded methods refuse an acquisition made
// while the caller holds one of its transactions; see ErrNestedAcquire.
type Pool struct {
	*pgxpool.Pool
}

// NewPool wraps an open pgx pool.
func NewPool(pool *pgxpool.Pool) *Pool {
	return &Pool{Pool: pool}
}

func (p *Pool) guard(ctx context.Context) error {
	marker := markerFrom(ctx)
	if marker == nil || marker.open.Load() == 0 {
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
	return p.Pool.Query(ctx, sql, args...)
}

// QueryRow runs a single-row query on a pooled connection.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := p.guard(ctx); err != nil {
		return errRow{err: err}
	}
	return p.Pool.QueryRow(ctx, sql, args...)
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := p.guard(ctx); err != nil {
		return pgconn.CommandTag{}, err
	}
	return p.Pool.Exec(ctx, sql, args...)
}

// Acquire takes a pooled connection the caller holds until it releases it.
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.Pool.Acquire(ctx)
}

// AcquireFunc runs fn with a pooled connection.
func (p *Pool) AcquireFunc(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	if err := p.guard(ctx); err != nil {
		return err
	}
	return p.Pool.AcquireFunc(ctx, fn)
}

// AcquireAllIdle takes every idle connection the pool holds.
func (p *Pool) AcquireAllIdle(ctx context.Context) []*pgxpool.Conn {
	if err := p.guard(ctx); err != nil {
		return nil
	}
	return p.Pool.AcquireAllIdle(ctx)
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
	return p.Pool.CopyFrom(ctx, table, columns, source)
}

// SendBatch runs a batch on a pooled connection.
func (p *Pool) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if err := p.guard(ctx); err != nil {
		return errBatchResults{err: err}
	}
	return p.Pool.SendBatch(ctx, batch)
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
	tx, err := p.Pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	marker := markerFrom(ctx)
	if marker == nil {
		return tx, nil
	}
	marker.open.Add(1)
	return &trackedTx{Tx: tx, marker: marker}, nil
}

// trackedTx clears the request's open-transaction mark however the transaction ends. Callers
// commit and then run a deferred rollback, so the clear is idempotent.
type trackedTx struct {
	pgx.Tx
	marker *txMarker
	once   sync.Once
}

func (t *trackedTx) done() {
	t.once.Do(func() { t.marker.open.Add(-1) })
}

func (t *trackedTx) Commit(ctx context.Context) error {
	defer t.done()
	return t.Tx.Commit(ctx)
}

func (t *trackedTx) Rollback(ctx context.Context) error {
	defer t.done()
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
