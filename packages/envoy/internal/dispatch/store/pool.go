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

// ErrNestedAcquire is a pool this package guards (Pool: the request pool and the pools it wraps)
// refusing a connection to a caller that already holds a connection of one of them.
//
// One caller, one connection, is the rule the pool cannot survive without. A transaction
// holds its connection until it commits; a caller that asks for a second one while it holds a
// row or advisory lock waits for a connection only the callers queued behind that lock can
// release, and they are waiting for the lock. Two anchored writes and two document
// settlements on one issue closed that cycle on a four-connection pool, and nothing but
// pg_terminate_backend recovered it. Rationing connections cannot fix it, and neither can
// SharedPoolSize's size: the queue behind one writer's issue lock is unbounded - a
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

// ConnectionHeld reports whether ctx's caller holds a pooled connection: its transaction, a
// cursor, a connection it acquired or marked (HoldsConnection), or its request's pinned connection
// (PinConnection), idle between statements or not. Whatever a connection holder must never wait
// for refuses the callers it reports - a document's writer gate, whose holder may be waiting for a
// connection itself. The pools refuse a second acquire to every one of them (ErrNestedAcquire);
// only the pool that pinned the connection serves a call under a pin, and it runs that call on
// the pinned connection instead of taking a second.
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
	// openFailures is the error each separate pool it names fails to open with, in place of
	// opening (SetOpenFailureForTest): a test sees what its caller does when a pool cannot open.
	openFailures map[string]error
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

// Config returns a copy of the pool's configuration.
func (p *Pool) Config() *pgxpool.Config { return p.pool.Config() }

// Stat reports the pool's current connection counts.
func (p *Pool) Stat() *pgxpool.Stat { return p.pool.Stat() }

// busy maps an acquire that ran out this pool's bound to ErrPoolBusy (poolBusy).
func (p *Pool) busy(ctx context.Context, err error) error { return poolBusy(ctx, p.name, err) }

// refuseOrPin is the refusal every way this pool hands out a connection makes first, and the
// pinned connection to run the call on where ctx pins one on this pool (nil where it pins none).
// A caller already holding a connection of these pools is refused with ErrNestedAcquire: its
// transaction, its open cursor or one it acquired (holding.connection), and its request's pinned
// connection on every pool but the one that pinned it. A pinned read holds its request-pool
// connection between its statements, so a background, live or settlements connection handed to it
// would be two of these pools' connections held by one caller, which is the cycle the guard
// exists to prevent; the pool that pinned it serves it on that very connection instead
// (pinnedConnection.take). Its callers log the refusal themselves (logRefusal), which keys it to
// the call site that asked for the connection by how deep the stack is where it is logged.
func (p *Pool) refuseOrPin(ctx context.Context) (*pinnedConnection, error) {
	state, _ := ctx.Value(holdingKey{}).(*holding)
	if state == nil {
		return nil, nil
	}
	if state.connection.Load() || (p.name != RequestsPool && state.pin.held()) {
		return nil, ErrNestedAcquire
	}
	return p.pinned(state), nil
}

// guard is the refusal alone (refuseOrPin), for a method that hands out no connection to run a
// call on.
func (p *Pool) guard(ctx context.Context) error {
	_, err := p.refuseOrPin(ctx)
	if err != nil {
		logRefusal()
	}
	return err
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
// a call on the pgx pool. It refuses a caller already holding a connection (refuseOrPin), and a
// call while another runs on the pinned connection, with ErrNestedAcquire. Every exported method
// that hands out a connection calls it directly, as guard is called, so a refusal records that
// method's caller.
func (p *Pool) target(ctx context.Context) (connection, func(), error) {
	pin, err := p.refuseOrPin(ctx)
	if err != nil {
		logRefusal()
		return nil, nil, err
	}
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
// connection (refuseOrPin) or a pinned connection a call runs on, the pinned connection where ctx
// pins an idle one on this pool, and nil otherwise.
func (p *Pool) handOver(ctx context.Context) (*pgxpool.Conn, error) {
	pin, err := p.refuseOrPin(ctx)
	if err != nil {
		logRefusal()
		return nil, err
	}
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
			// The COMMIT was never sent, so this transaction rolled back and its outcome is
			// known, whatever ended the step: the error is the step's own.
			_ = t.Tx.Rollback(ctx)
			return err
		}
	}
	return commitTimedOut(ctx, t.Tx.Commit(ctx))
}

func (t *trackedTx) Rollback(ctx context.Context) error {
	defer t.end()
	return t.Tx.Rollback(ctx)
}

// commitTimeout bounds a commit CommitContext has detached from its caller.
const commitTimeout = 10 * time.Second

// ErrCommitTimeout is a commit the bound of its own context cut off (CommitContext): the database
// did not answer the COMMIT within commitTimeout. Its outcome is unknown - the transaction may
// have committed, and nothing the server holds says whether it did - so it is neither a pool that
// refused before anything ran (ErrPoolBusy) nor a failure that rolled back: a request answers it
// 500, naming the unknown outcome, and a client must read what it wrote before sending it again
// (api.writeHandlerError, cmd/dispatch/README.md "Load errors"). The error wraps
// context.DeadlineExceeded too, so errors.Is finds both.
var ErrCommitTimeout = errors.New("the commit ran out its bound; its outcome is unknown")

// IsCommitTimeout reports whether err is a commit cut off at its bound (ErrCommitTimeout).
func IsCommitTimeout(err error) bool { return errors.Is(err, ErrCommitTimeout) }

// commitTimedOut names err ErrCommitTimeout when ctx, the commit's own context, is the bound that
// ended it: pgx answers a statement its context cut off with that context's error and not with
// its cause, so the cause is read here instead. A commit that failed for any other reason, and
// one that did not fail at all, is returned as it is.
func commitTimedOut(ctx context.Context, err error) error {
	if err == nil || !errors.Is(context.Cause(ctx), ErrCommitTimeout) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrCommitTimeout, err)
}

// commitBoundKey carries the bound a test puts on the commits made under a context
// (WithCommitBoundForTest).
type commitBoundKey struct{}

// WithCommitBoundForTest returns ctx with bound as the bound of every commit made under it, in
// place of commitTimeout. It is for tests outside this package alone, which make a commit run out
// its bound in milliseconds rather than in production's ten seconds; nothing in production calls
// it, and a context carrying none commits under commitTimeout.
func WithCommitBoundForTest(ctx context.Context, bound time.Duration) context.Context {
	return context.WithValue(ctx, commitBoundKey{}, bound)
}

// CommitContext is the context a transaction commits on: ctx's values without its cancellation
// or deadline, bounded at commitTimeout with ErrCommitTimeout as the cause. A commit cut short by
// its caller - a client that went away, a caller's deadline that passed - has an unknown outcome,
// since the server may have committed it, and the document service fails every room such a
// transaction wrote, which then reload the durable document (docs.Ledger.Commit); a request that
// ended mid-commit would take its rooms down with it. So a commit in flight runs to its end, and
// only its own bound, ten seconds, keeps one the database never answers from holding its
// connection for good. Every transaction this pool opens commits on it.
func CommitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bound, ok := ctx.Value(commitBoundKey{}).(time.Duration)
	if !ok {
		bound = commitTimeout
	}
	return context.WithTimeoutCause(context.WithoutCancel(ctx), bound, ErrCommitTimeout)
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
