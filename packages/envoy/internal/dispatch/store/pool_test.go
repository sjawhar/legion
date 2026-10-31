package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The pool refuses a second connection to a caller that already holds one of its transactions:
// that acquisition is what deadlocks the pool once every connection is held by a writer waiting
// on a lock. A caller that needs a connection of its own opens it elsewhere - the document
// rooms pool - and a caller that only needs to read does it through its transaction.
func TestPoolRefusesASecondConnectionInsideATransaction(t *testing.T) {
	database := openTestStore(t)
	// The health probe reads the applied schema version, so the database must have one.
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := WithTransactionTracking(context.Background())

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback(ctx)

	var one int
	if err := tx.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("read through the transaction: %v", err)
	}
	if _, err := database.Pool.Query(ctx, "select 1"); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("pool query inside a transaction: %v, want ErrNestedAcquire", err)
	}
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("pool row read inside a transaction: %v, want ErrNestedAcquire", err)
	}
	if _, err := database.Pool.Exec(ctx, "select 1"); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("pool statement inside a transaction: %v, want ErrNestedAcquire", err)
	}
	if _, err := database.Pool.Begin(ctx); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("second transaction: %v, want ErrNestedAcquire", err)
	}
	if _, err := database.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("second transaction with options: %v, want ErrNestedAcquire", err)
	}
	// Healthy is the deliberate exception: it probes its own pool, so it cannot close the
	// cycle the guard prevents and must answer even for a caller holding a connection.
	if _, err := database.Pool.Healthy(ctx); err != nil {
		t.Fatalf("health probe inside a transaction: %v, want success", err)
	}
	if _, err := database.Pool.Acquire(ctx); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("connection acquired inside a transaction: %v, want ErrNestedAcquire", err)
	}
	if err := database.Pool.AcquireFunc(ctx, func(*pgxpool.Conn) error {
		return nil
	}); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("connection acquired for a function inside a transaction: %v, want ErrNestedAcquire", err)
	}
	if conns := database.Pool.AcquireAllIdle(ctx); conns != nil {
		for _, conn := range conns {
			conn.Release()
		}
		t.Fatal("idle connections taken inside a transaction, want none")
	}
	if _, err := database.Pool.CopyFrom(ctx, pgx.Identifier{"schema_migrations"}, []string{"version"},
		pgx.CopyFromRows(nil)); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("copy inside a transaction: %v, want ErrNestedAcquire", err)
	}
	batch := &pgx.Batch{}
	batch.Queue("select 1")
	if err := database.Pool.SendBatch(ctx, batch).Close(); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("batch inside a transaction: %v, want ErrNestedAcquire", err)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("pool read after the transaction ended: %v", err)
	}
}

// An open cursor is a held connection too: pgx hands it back when the rows close, so work
// inside the loop competes with the caller's own cursor for the pool.
func TestPoolRefusesASecondConnectionInsideAnOpenCursor(t *testing.T) {
	database := openTestStore(t)
	ctx := WithTransactionTracking(context.Background())

	rows, err := database.Pool.Query(ctx, "select generate_series(1, 3)")
	if err != nil {
		t.Fatalf("open the cursor: %v", err)
	}
	// A cursor a failed assertion left open holds its connection, and t.Cleanup's pool close
	// waits for it: without this the test would hang the package instead of naming itself.
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("read the first row: %v", rows.Err())
	}
	var one int
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("pool read inside an open cursor: %v, want ErrNestedAcquire", err)
	}
	if _, err := database.Pool.Exec(ctx, "select 1"); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("pool statement inside an open cursor: %v, want ErrNestedAcquire", err)
	}
	if _, err := database.Pool.Begin(ctx); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("transaction inside an open cursor: %v, want ErrNestedAcquire", err)
	}

	rows.Close()
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("pool read after the cursor closed: %v", err)
	}

	drained, err := database.Pool.Query(ctx, "select generate_series(1, 2)")
	if err != nil {
		t.Fatalf("open the second cursor: %v", err)
	}
	defer drained.Close()
	for drained.Next() {
	}
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("pool read after the cursor ran out: %v", err)
	}

	// pgx treats a scan failure as fatal: it closes the rows and hands the connection back,
	// so the mark has to go back with it. A null array element is a value the destination
	// cannot take.
	failing, err := database.Pool.Query(ctx, "select array['a', null]::text[]")
	if err != nil {
		t.Fatalf("open the third cursor: %v", err)
	}
	defer failing.Close()
	if !failing.Next() {
		t.Fatalf("read the first row of the third cursor: %v", failing.Err())
	}
	var elements []string
	if err := failing.Scan(&elements); err == nil {
		t.Fatal("scanned a null array element into []string, want a scan failure")
	}
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("pool read after a scan error closed the cursor: %v", err)
	}
}

// A cursor releases its connection once. A drained cursor closed later must not clear the mark
// of whatever holds a connection by then - here a transaction opened after the drain, which
// would otherwise be free to take a second connection and wedge the pool.
func TestClosingADrainedCursorLeavesALaterHolderMarked(t *testing.T) {
	database := openTestStore(t)
	ctx := WithTransactionTracking(context.Background())

	rows, err := database.Pool.Query(ctx, "select generate_series(1, 2)")
	if err != nil {
		t.Fatalf("open the cursor: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a transaction after the drain: %v", err)
	}
	defer tx.Rollback(ctx)

	var one int
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("pool read inside the transaction: %v, want ErrNestedAcquire", err)
	}
	rows.Close()
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("pool read after the drained cursor closed: %v, want ErrNestedAcquire", err)
	}
}

// A durable document append holds its connection directly, outside any transaction of this
// pool's, and takes the room's advisory lock on it. A second connection under that is the same
// deadlock, so the same refusal covers it.
func TestPoolRefusesASecondConnectionWhileOneIsHeld(t *testing.T) {
	database := openTestStore(t)
	ctx, release := HoldsConnection(context.Background())

	var one int
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("pool read while a connection is held: %v, want ErrNestedAcquire", err)
	}

	release()
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("pool read after the connection was released: %v", err)
	}
}

// Reads a caller runs side by side each take one connection: each read's context carries a hold
// mark of its own, so one read's open cursor does not refuse the other, while each is still
// refused a second connection of its own, and a caller already holding one is refused them all.
func TestConcurrentReadsEachHoldOneConnection(t *testing.T) {
	database := openTestStore(t)
	ctx := WithTransactionTracking(context.Background())

	first, err := ForConcurrentRead(ctx)
	if err != nil {
		t.Fatalf("first read's context: %v", err)
	}
	second, err := ForConcurrentRead(ctx)
	if err != nil {
		t.Fatalf("second read's context: %v", err)
	}
	rows, err := database.Pool.Query(first, "select generate_series(1, 2)")
	if err != nil {
		t.Fatalf("first read's cursor: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("first read's first row: %v", rows.Err())
	}
	var one int
	if err := database.Pool.QueryRow(second, "select 1").Scan(&one); err != nil {
		t.Fatalf("second read beside the first's open cursor: %v, want it served", err)
	}
	if err := database.Pool.QueryRow(first, "select 1").Scan(&one); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("a second connection under the first read's cursor: %v, want ErrNestedAcquire", err)
	}
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("the caller's own read beside its reads: %v, want it served", err)
	}
	rows.Close()

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := ForConcurrentRead(ctx); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("concurrent reads under an open transaction: %v, want ErrNestedAcquire", err)
	}
}

// Listener hands out a connection of its own, outside the shared pool: one taken while every shared
// connection is held is still served, it receives notifications, and once it is closed the next
// call is served too. The shared pool here holds four connections rather than sharedPoolSize's:
// the property is the same at any size, and holding forty-eight server connections while the
// other packages' tests share the server would push a default Postgres past its hundred.
func TestListenerIsServedWhileTheSharedPoolIsFullyAcquired(t *testing.T) {
	ctx := context.Background()
	config := openTestStore(t).Pool.Config().Copy()
	config.MaxConns = 4
	shared, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a four-connection shared pool: %v", err)
	}
	database := &Store{Pool: NewPool(shared)}
	t.Cleanup(database.Pool.Close)
	held := make([]*pgxpool.Conn, 0, database.Pool.Config().MaxConns)
	for range database.Pool.Config().MaxConns {
		conn, err := database.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a shared connection: %v", err)
		}
		held = append(held, conn)
	}
	defer func() {
		for _, conn := range held {
			conn.Release()
		}
	}()
	if stat := database.Pool.Stat(); stat.AcquiredConns() != stat.MaxConns() {
		t.Fatalf("shared pool holds %d of %d connections, want all", stat.AcquiredConns(), stat.MaxConns())
	}

	for round := range 2 {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		listener, err := database.Pool.Listener(waitCtx)
		if err != nil {
			cancel()
			t.Fatalf("round %d: Listener with the shared pool fully acquired: %v", round, err)
		}
		if _, err := listener.Exec(waitCtx, "listen pool_listener_test"); err != nil {
			t.Fatalf("round %d: listen: %v", round, err)
		}
		if _, err := held[0].Exec(waitCtx, "select pg_notify('pool_listener_test', 'ping')"); err != nil {
			t.Fatalf("round %d: notify: %v", round, err)
		}
		notification, err := listener.WaitForNotification(waitCtx)
		if err != nil || notification.Payload != "ping" {
			t.Fatalf("round %d: notification = %+v, %v; want payload ping", round, notification, err)
		}
		if err := listener.Close(waitCtx); err != nil {
			t.Fatalf("round %d: close: %v", round, err)
		}
		cancel()
	}
}

// The event log's safe horizon observes on a pool of its own: with every shared connection held,
// as under request load, an observation - the shape of the batch events.Horizon sends, one short
// transaction that reads its snapshot's xmin and takes a transaction id - and the stall
// diagnosis's single-row read are still served. Queued behind the shared pool, the observation
// holds back every live reader of the event log for as long as the requests ahead of it take.
func TestHorizonIsServedWhileTheSharedPoolIsFullyAcquired(t *testing.T) {
	ctx := context.Background()
	config := openTestStore(t).Pool.Config().Copy()
	config.MaxConns = 4
	shared, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a four-connection shared pool: %v", err)
	}
	database := &Store{Pool: NewPool(shared)}
	t.Cleanup(database.Pool.Close)
	held := make([]*pgxpool.Conn, 0, database.Pool.Config().MaxConns)
	for range database.Pool.Config().MaxConns {
		conn, err := database.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a shared connection: %v", err)
		}
		held = append(held, conn)
	}
	defer func() {
		for _, conn := range held {
			conn.Release()
		}
	}()
	if stat := database.Pool.Stat(); stat.AcquiredConns() != stat.MaxConns() {
		t.Fatalf("shared pool holds %d of %d connections, want all", stat.AcquiredConns(), stat.MaxConns())
	}

	horizon, err := database.Pool.Horizon()
	if err != nil {
		t.Fatalf("open the horizon pool: %v", err)
	}
	for round := range 2 {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		batch := &pgx.Batch{}
		batch.Queue(`begin`)
		batch.Queue(`select pg_snapshot_xmin(pg_current_snapshot())::text, pg_current_xact_id()::text`)
		batch.Queue(`commit`)
		results := horizon.SendBatch(waitCtx, batch)
		var xmin, xid string
		_, err := results.Exec()
		if err == nil {
			err = results.QueryRow().Scan(&xmin, &xid)
		}
		if err == nil {
			_, err = results.Exec()
		}
		if closeErr := results.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			cancel()
			t.Fatalf("round %d: observation with the shared pool fully acquired: %v", round, err)
		}
		var backends int
		if err := horizon.QueryRow(waitCtx, "select count(*) from pg_stat_activity").Scan(&backends); err != nil {
			cancel()
			t.Fatalf("round %d: diagnosis read with the shared pool fully acquired: %v", round, err)
		}
		cancel()
	}
}

// A refusal is keyed and logged by the call site that asked for the connection: one caller
// tripping the guard in a loop logs its stack once, and another caller still logs its own.
// Leaving Begin's guard to BeginTx, instead of to the unexported begin both entry points share,
// records Begin's own frame instead, which is no call site at all - every caller the compiler
// gives no inlined copy of Begin, a transaction opened through an interface (refs.Pool) among
// them, then shares that single key and only the first of them ever logs a stack.
func TestEachBeginRefusalIsKeyedToItsCaller(t *testing.T) {
	database := openTestStore(t)
	// The guard logs each call site once per process, so a repeat run has nothing left to log.
	loggedSites.Clear()
	ctx, release := HoldsConnection(context.Background())
	defer release()
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	if _, err := beginFromOneSite(ctx, database.Pool); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("transaction from the first call site: %v, want ErrNestedAcquire", err)
	}
	if _, err := beginFromAnotherSite(ctx, database.Pool); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("transaction from the second call site: %v, want ErrNestedAcquire", err)
	}

	refusals := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "second pooled connection requested while one is held") {
			refusals++
		}
	}
	if refusals != 2 {
		t.Fatalf("refusals logged = %d, want one per call site:\n%s", refusals, logs.String())
	}
	sites := refusalSites()
	for _, want := range []string{"beginFromOneSite", "beginFromAnotherSite"} {
		if !slices.ContainsFunc(sites, func(site string) bool { return strings.HasSuffix(site, want) }) {
			t.Errorf("refusals keyed to %v, want one keyed to %s", sites, want)
		}
	}
}

// refusalSites names the function of every call site the guard has logged a refusal for.
func refusalSites() []string {
	var sites []string
	loggedSites.Range(func(key, _ any) bool {
		frame, _ := runtime.CallersFrames([]uintptr{key.(uintptr)}).Next()
		sites = append(sites, frame.Function)
		return true
	})
	return sites
}

func beginFromOneSite(ctx context.Context, pool *Pool) (pgx.Tx, error) {
	return pool.Begin(ctx)
}

func beginFromAnotherSite(ctx context.Context, pool *Pool) (pgx.Tx, error) {
	return pool.Begin(ctx)
}

// testAcquireBound is the acquire bound the busy rows give a pool, so a full pool refuses in a
// fraction of a second instead of the seconds production waits.
const testAcquireBound = 300 * time.Millisecond

// busySlack is how long past its bound an acquire may take to report ErrPoolBusy.
const busySlack = 200 * time.Millisecond

// smallRequestPool opens a request pool of size connections on the test database, its acquires
// bounded at bound. The properties below hold at any size, and holding sharedPoolSize server
// connections while the other packages' tests share the server would push a default Postgres past
// its hundred.
func smallRequestPool(t *testing.T, size int32, bound time.Duration) *Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	config.MaxConns = size
	configurePool(config, RequestsPool, bound)
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a %d-connection request pool: %v", size, err)
	}
	pool := NewPool(opened)
	t.Cleanup(pool.Close)
	return pool
}

// holdEvery takes count connections with acquire and holds them until the returned release runs,
// or the test ends.
func holdEvery(t *testing.T, count int32, acquire func(context.Context) (*pgxpool.Conn, error)) func() {
	t.Helper()
	held := make([]*pgxpool.Conn, 0, count)
	release := func() {
		for _, conn := range held {
			conn.Release()
		}
		held = nil
	}
	t.Cleanup(release)
	for range count {
		conn, err := acquire(context.Background())
		if err != nil {
			t.Fatalf("hold a connection: %v", err)
		}
		held = append(held, conn)
	}
	return release
}

// The pools are a partition, not a queue: with every connection of the request pool held, a query
// on each of the background, live, settlements and lease pools is still answered, and with every
// connection of the background pool held a request is still answered. The waits here are bounded
// by the pools' own acquire bounds, so a pool that queued behind another fails with ErrPoolBusy
// rather than hanging.
func TestEachPoolIsServedWhileAnotherIsFullyAcquired(t *testing.T) {
	ctx := context.Background()
	pool := smallRequestPool(t, 4, requestAcquireBound)
	releaseRequests := holdEvery(t, pool.Stat().MaxConns(), pool.Acquire)
	if stat := pool.Stat(); stat.AcquiredConns() != stat.MaxConns() {
		t.Fatalf("request pool holds %d of %d connections, want all", stat.AcquiredConns(), stat.MaxConns())
	}

	for _, separate := range []struct {
		name string
		open func() (*Pool, error)
	}{
		{BackgroundPool, pool.Background},
		{LivePool, pool.Live},
		{SettlementsPool, pool.Settlements},
	} {
		opened, err := separate.open()
		if err != nil {
			t.Fatalf("open the %s pool: %v", separate.name, err)
		}
		var one int
		if err := opened.QueryRow(ctx, "select 1").Scan(&one); err != nil {
			t.Errorf("a %s query with the request pool fully acquired: %v, want it answered", separate.name, err)
		}
	}
	leases, err := pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	var one int
	if err := leases.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Errorf("a leases query with the request pool fully acquired: %v, want it answered", err)
	}
	releaseRequests()

	background, err := pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	holdEvery(t, background.Stat().MaxConns(), background.Acquire)
	if stat := background.Stat(); stat.AcquiredConns() != backgroundPoolSize {
		t.Fatalf("background pool holds %d connections, want all %d", stat.AcquiredConns(), backgroundPoolSize)
	}
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Errorf("a request with the background pool fully acquired: %v, want it answered", err)
	}
}

// poolCall is one way a Pool hands out a connection, run to its end: rows closed, transaction
// ended, batch closed, connection released.
type poolCall struct {
	name string
	call func(context.Context, *Pool) error
}

// poolCalls is every way a Pool hands out a connection, each running sql.
func poolCalls(sql string) []poolCall {
	return []poolCall{
		{"Query", func(ctx context.Context, pool *Pool) error {
			rows, err := pool.Query(ctx, sql)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
			}
			return rows.Err()
		}},
		{"QueryRow", func(ctx context.Context, pool *Pool) error {
			var value int
			return pool.QueryRow(ctx, sql).Scan(&value)
		}},
		{"Exec", func(ctx context.Context, pool *Pool) error {
			_, err := pool.Exec(ctx, sql)
			return err
		}},
		{"Begin+Commit", func(ctx context.Context, pool *Pool) error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			_, execErr := tx.Exec(ctx, sql)
			return errors.Join(execErr, tx.Commit(ctx))
		}},
		{"BeginTx+Rollback", func(ctx context.Context, pool *Pool) error {
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			if err != nil {
				return err
			}
			_, execErr := tx.Exec(ctx, sql)
			return errors.Join(execErr, tx.Rollback(ctx))
		}},
		{"SendBatch", func(ctx context.Context, pool *Pool) error {
			batch := &pgx.Batch{}
			batch.Queue(sql)
			results := pool.SendBatch(ctx, batch)
			_, execErr := results.Exec()
			return errors.Join(execErr, results.Close())
		}},
		{"Acquire", func(ctx context.Context, pool *Pool) error {
			conn, err := pool.Acquire(ctx)
			if err != nil {
				return err
			}
			defer conn.Release()
			_, err = conn.Exec(ctx, sql)
			return err
		}},
		{"AcquireFunc", func(ctx context.Context, pool *Pool) error {
			return pool.AcquireFunc(ctx, func(conn *pgxpool.Conn) error {
				_, err := conn.Exec(ctx, sql)
				return err
			})
		}},
	}
}

// An acquire on a full pool waits its pool's bound and no longer, and is answered with
// ErrPoolBusy - through every way the pool hands out a connection, the ones that defer their
// errors to a row or a batch result included - and the pool's counters see each wait and each
// refusal. A caller whose own deadline is sooner gets its own error, not ErrPoolBusy.
func TestAFullPoolAnswersErrPoolBusyWithinItsBound(t *testing.T) {
	pool := smallRequestPool(t, 1, testAcquireBound)
	releaseHeld := holdEvery(t, 1, pool.Acquire)

	calls := poolCalls("select 1")
	calls = append(calls, poolCall{"CopyFrom", func(ctx context.Context, pool *Pool) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"pool_busy_copy"}, []string{"value"}, pgx.CopyFromRows(nil))
		return err
	}})
	for _, call := range calls {
		started := time.Now()
		err := call.call(context.Background(), pool)
		waited := time.Since(started)
		if !IsPoolBusy(err) || !errors.Is(err, ErrPoolBusy) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s on a full pool: %v, want ErrPoolBusy wrapping context.DeadlineExceeded", call.name, err)
		}
		if waited > testAcquireBound+busySlack {
			t.Errorf("%s on a full pool answered after %v, want within its bound %v + %v", call.name, waited, testAcquireBound, busySlack)
		}
	}

	caller, cancel := context.WithTimeout(context.Background(), testAcquireBound/3)
	defer cancel()
	started := time.Now()
	_, err := pool.Exec(caller, "select 1")
	if waited := time.Since(started); waited >= testAcquireBound {
		t.Errorf("an acquire under a sooner caller deadline answered after %v, want before the pool's bound %v", waited, testAcquireBound)
	}
	if !errors.Is(err, context.DeadlineExceeded) || IsPoolBusy(err) {
		t.Errorf("an acquire under a sooner caller deadline: %v, want the caller's context.DeadlineExceeded and not ErrPoolBusy", err)
	}

	waiting := make(chan error, 1)
	go func() { waiting <- poolCalls("select 1")[1].call(context.Background(), pool) }()
	deadline := time.Now().Add(testAcquireBound)
	for pool.Stats()[0].Acquire.Waiting != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("the waiting gauge never counted the acquire in progress: %+v", pool.Stats()[0].Acquire)
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-waiting; !IsPoolBusy(err) {
		t.Fatalf("the waiting acquire: %v, want ErrPoolBusy", err)
	}

	releaseHeld()
	if stat := pool.Stat(); stat.AcquiredConns() != 0 {
		t.Errorf("%d connections still taken once the busy calls returned and the held one was released, want 0", stat.AcquiredConns())
	}
	stats := pool.Stats()
	if len(stats) != 1 || stats[0].Name != RequestsPool {
		t.Fatalf("stats = %+v, want the request pool alone", stats)
	}
	acquire := stats[0].Acquire
	refused := uint64(len(calls) + 1)
	if acquire.Waiting != 0 || acquire.Timeouts != refused {
		t.Errorf("waiting %d and timeouts %d, want 0 waiting and %d timeouts (the caller's own deadline is not one)", acquire.Waiting, acquire.Timeouts, refused)
	}
	// The held connection's acquire, every refusal and the caller-deadline acquire.
	if acquire.Count != refused+2 || acquire.Buckets[AcquireBucketCount-1] != acquire.Count ||
		acquire.Sum < (time.Duration(refused)*testAcquireBound).Seconds() {
		t.Errorf("acquire histogram %+v, want %d acquires, all within the last bucket, summing at least %d bounds", acquire, refused+2, refused)
	}
}

// Every way the request pool hands out a connection gives it back however the call ends: on
// success, on a statement that fails, and on an acquire that ran out the pool's bound.
func TestEveryPoolMethodGivesItsConnectionBack(t *testing.T) {
	ctx := context.Background()
	pool := smallRequestPool(t, 1, testAcquireBound)
	for _, outcome := range []struct {
		name, sql string
		fails     bool
	}{
		{"success", "select 1", false},
		{"a failing statement", "select 1/0", true},
	} {
		for _, call := range poolCalls(outcome.sql) {
			if err := call.call(ctx, pool); (err != nil) != outcome.fails {
				t.Errorf("%s on %s: %v, want failure %t", call.name, outcome.name, err, outcome.fails)
			}
			if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
				t.Fatalf("%s on %s left %d connections taken, want 0", call.name, outcome.name, acquired)
			}
		}
	}
	for _, call := range poolCalls("select 1") {
		release := holdEvery(t, 1, pool.Acquire)
		if err := call.call(ctx, pool); !IsPoolBusy(err) {
			t.Errorf("%s on a full pool: %v, want ErrPoolBusy", call.name, err)
		}
		release()
		if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
			t.Fatalf("%s refused busy left %d connections taken, want 0", call.name, acquired)
		}
	}
}

// A connection counts as finished (ConnectionUse.Finished) only when its caller hands it back open
// and idle, on a pool whose configuration carried no tracer and on one that carried a tracer of its
// own. One whose statement its context cut off is taken back and discarded, and finished nothing:
// the shutdown's wait for the pools to come back counts a database that has stopped answering by
// probes that fail with no connection finishing, and a discard must not read as work done.
func TestOnlyAConnectionHandedBackOpenCountsAsFinished(t *testing.T) {
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	config.MaxConns = 1
	config.ConnConfig.Tracer = &countingTracer{}
	configurePool(config, RequestsPool, testAcquireBound)
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	traced := NewPool(opened)
	t.Cleanup(traced.Close)

	for name, pool := range map[string]*Pool{"plain": smallRequestPool(t, 1, testAcquireBound), "traced": traced} {
		before := pool.Use()
		var one int
		if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
			t.Fatalf("%s pool: query: %v", name, err)
		}
		if after := pool.Use(); after.Returned != before.Returned+1 || after.Finished != before.Finished+1 {
			t.Errorf("%s pool: a query that answered moved Returned %d->%d and Finished %d->%d, want each up by one",
				name, before.Returned, after.Returned, before.Finished, after.Finished)
		}

		before = pool.Use()
		cut, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		_, err := pool.Exec(cut, "select pg_sleep(10)")
		cancel()
		if err == nil {
			t.Fatalf("%s pool: a ten-second statement answered within its 100 ms context", name)
		}
		// pgxpool discards a closed connection on a goroutine of its own, so the pool takes it back
		// a moment after the statement returns.
		after := pool.Use()
		for deadline := time.Now().Add(5 * time.Second); after.InUse != 0 && time.Now().Before(deadline); after = pool.Use() {
			time.Sleep(10 * time.Millisecond)
		}
		if after.Returned != before.Returned+1 || after.Finished != before.Finished || after.InUse != 0 {
			t.Errorf("%s pool: a statement its context cut off moved Returned %d->%d, Finished %d->%d and left %d in use, want Returned up by one, Finished unchanged and none in use",
				name, before.Returned, after.Returned, before.Finished, after.Finished, after.InUse)
		}
	}
}

// The separate pools refuse a full pool's acquire with ErrPoolBusy too: a wrapped one through its
// methods, the listener's through Listener.
func TestSeparatePoolsAnswerErrPoolBusyWhenFull(t *testing.T) {
	pool := smallRequestPool(t, 1, testAcquireBound)
	pool.acquireBounds = map[string]time.Duration{BackgroundPool: testAcquireBound, ListenPool: testAcquireBound}

	background, err := pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	holdEvery(t, background.Stat().MaxConns(), background.Acquire)
	started := time.Now()
	var one int
	err = background.QueryRow(context.Background(), "select 1").Scan(&one)
	if waited := time.Since(started); !IsPoolBusy(err) || waited > testAcquireBound+busySlack {
		t.Errorf("a query on the full background pool: %v after %v, want ErrPoolBusy within %v", err, waited, testAcquireBound+busySlack)
	}

	listen, err := pool.separate(ListenPool, listenerPoolSize)
	if err != nil {
		t.Fatalf("open the listener pool: %v", err)
	}
	holdEvery(t, listenerPoolSize, listen.Acquire)
	started = time.Now()
	conn, err := pool.Listener(context.Background())
	if conn != nil {
		conn.Close(context.Background())
	}
	if waited := time.Since(started); !IsPoolBusy(err) || waited > testAcquireBound+busySlack {
		t.Errorf("a listener dialled through the full listener pool: %v after %v, want ErrPoolBusy within %v", err, waited, testAcquireBound+busySlack)
	}
}

// The raw pools - the ones handed out whole, rooms and leases among them - answer a full pool
// with ErrPoolBusy too, through every way their callers take a connection: a document load's
// transaction and read on the rooms pool, a maintenance pass's Acquire on the leases pool. A
// caller that cannot tell a pool with nothing free from a deadline of its own has to guess, and
// the guess reads a commit cut off at its bound as a pool that refused before anything ran.
func TestTheRawPoolsAnswerErrPoolBusyWhenFull(t *testing.T) {
	pool := smallRequestPool(t, 1, testAcquireBound)
	pool.acquireBounds = map[string]time.Duration{RoomsPool: testAcquireBound, LeasesPool: testAcquireBound}

	rooms, err := pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}
	holdEvery(t, rooms.Stat().MaxConns(), rooms.Acquire)
	for _, call := range []struct {
		name string
		call func(context.Context) error
	}{
		{"BeginTx", func(ctx context.Context) error {
			tx, err := rooms.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err == nil {
				_ = tx.Rollback(ctx)
			}
			return err
		}},
		{"QueryRow", func(ctx context.Context) error {
			var one int
			return rooms.QueryRow(ctx, "select 1").Scan(&one)
		}},
		{"Query", func(ctx context.Context) error {
			rows, err := rooms.Query(ctx, "select 1")
			if err == nil {
				rows.Close()
			}
			return err
		}},
		{"Exec", func(ctx context.Context) error {
			_, err := rooms.Exec(ctx, "select 1")
			return err
		}},
	} {
		started := time.Now()
		err := call.call(context.Background())
		if waited := time.Since(started); !IsPoolBusy(err) || waited > testAcquireBound+busySlack {
			t.Errorf("%s on the full rooms pool: %v after %v, want ErrPoolBusy within %v",
				call.name, err, waited, testAcquireBound+busySlack)
		}
	}

	leases, err := pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	holdEvery(t, leases.Stat().MaxConns(), leases.Acquire)
	started := time.Now()
	conn, err := leases.Acquire(context.Background())
	if conn != nil {
		conn.Release()
	}
	if waited := time.Since(started); !IsPoolBusy(err) || waited > testAcquireBound+busySlack {
		t.Errorf("an acquire on the full leases pool: %v after %v, want ErrPoolBusy within %v",
			err, waited, testAcquireBound+busySlack)
	}

	// A caller whose own deadline came first keeps its own error on a raw pool too.
	caller, cancel := context.WithTimeout(context.Background(), testAcquireBound/3)
	defer cancel()
	if _, err := rooms.Exec(caller, "select 1"); !errors.Is(err, context.DeadlineExceeded) || IsPoolBusy(err) {
		t.Errorf("a rooms acquire under a sooner caller deadline: %v, want the caller's context.DeadlineExceeded and not ErrPoolBusy", err)
	}
}

// commitHoldKey is the advisory lock the commit-holding trigger waits on.
const commitHoldKey = 592006

// A commit the database does not answer within the commit's own bound is ErrCommitTimeout and
// not a busy pool: nothing says whether the transaction committed, so no caller may take it for
// a refusal that ran nothing. A deferred constraint trigger runs when its transaction commits,
// so a trigger waiting on a lock another session holds makes the COMMIT itself wait, and the
// bound is the test's (WithCommitBoundForTest) rather than production's ten seconds.
func TestACommitCutOffAtItsBoundIsACommitTimeout(t *testing.T) {
	database := openEmptyTestStore(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		create table commit_timeout_test (value int primary key);
		create function hold_commit_timeout_test() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock_shared(592006);
			return null;
		end $$;
		create constraint trigger hold_commit_timeout_test after insert on commit_timeout_test
			deferrable initially deferred for each row execute function hold_commit_timeout_test();
	`); err != nil {
		t.Fatalf("create the commit-holding trigger: %v", err)
	}

	holder, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("hold a connection for the commit-holding lock: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `select pg_advisory_lock($1)`, commitHoldKey); err != nil {
		t.Fatalf("take the commit-holding lock: %v", err)
	}
	defer func() { _, _ = holder.Exec(context.Background(), `select pg_advisory_unlock($1)`, commitHoldKey) }()

	writing := WithCommitBoundForTest(WithTransactionTracking(ctx), 300*time.Millisecond)
	tx, err := database.Pool.Begin(writing)
	if err != nil {
		t.Fatalf("open the write's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(writing, `insert into commit_timeout_test (value) values (1)`); err != nil {
		t.Fatalf("write the row whose commit is held: %v", err)
	}
	started := time.Now()
	err = tx.Commit(writing)
	if waited := time.Since(started); waited > time.Second {
		t.Errorf("the held commit answered after %v, want within its 300 ms bound and a little", waited)
	}
	if !IsCommitTimeout(err) {
		t.Fatalf("a commit held past its bound: %v, want ErrCommitTimeout", err)
	}
	if IsPoolBusy(err) {
		t.Fatalf("a commit cut off at its bound reads as a busy pool: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a commit cut off at its bound: %v, want it to wrap context.DeadlineExceeded too", err)
	}
}

// A tracer the configuration already carried keeps every call it took, and the acquire bound
// still applies beside it.
func TestThePoolsAcquireTracerKeepsATracerAlreadyConfigured(t *testing.T) {
	config, err := pgxpool.ParseConfig(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	config.MaxConns = 1
	counting := &countingTracer{}
	config.ConnConfig.Tracer = counting
	configurePool(config, RequestsPool, testAcquireBound)
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	pool := NewPool(opened)
	t.Cleanup(pool.Close)

	var one int
	if err := pool.QueryRow(context.Background(), "select 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	if counting.queries.Load() == 0 || counting.acquires.Load() == 0 {
		t.Errorf("the configured tracer saw %d queries and %d acquires, want both", counting.queries.Load(), counting.acquires.Load())
	}
	holdEvery(t, 1, pool.Acquire)
	if err := pool.QueryRow(context.Background(), "select 1").Scan(&one); !IsPoolBusy(err) {
		t.Errorf("a query on the full pool with a tracer of its own: %v, want ErrPoolBusy", err)
	}
}

type countingTracer struct {
	queries, acquires atomic.Int32
}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.queries.Add(1)
	return ctx
}

func (*countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *countingTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	c.acquires.Add(1)
	return ctx
}

func (*countingTracer) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {}

// Each pool's connections carry dispatch-<pool> in application_name, so pg_stat_activity tells
// the pools apart, and a connection string's own application_name does not change it.
func TestEachPoolNamesItsConnectionsInPgStatActivity(t *testing.T) {
	ctx := context.Background()
	pool := openEmptyTestStore(t).Pool

	requests := holdOne(t, pool.Acquire)
	for _, open := range []func() (*Pool, error){pool.Outbox, pool.Background, pool.Live, pool.Settlements} {
		opened, err := open()
		if err != nil {
			t.Fatalf("open a separate pool: %v", err)
		}
		holdOne(t, opened.Acquire)
	}
	for _, open := range []func() (*RawPool, error){
		pool.Rooms, pool.Leases, pool.Horizon,
		func() (*RawPool, error) { return pool.separate(HealthPool, healthPoolSize) },
	} {
		opened, err := open()
		if err != nil {
			t.Fatalf("open a separate pool: %v", err)
		}
		holdOne(t, opened.Acquire)
	}
	registry, err := pool.OpenRegistry(ctx)
	if err != nil {
		t.Fatalf("open the registry's pool: %v", err)
	}
	t.Cleanup(registry.Close)
	holdOne(t, registry.Acquire)
	listener, err := pool.Listener(ctx)
	if err != nil {
		t.Fatalf("dial a listener: %v", err)
	}
	t.Cleanup(func() { listener.Close(context.Background()) })

	// The database is this test's own, so every connection to it is one of this store's.
	var names []string
	if err := requests.QueryRow(ctx, `select array_agg(distinct application_name order by application_name)
		from pg_stat_activity where datname = current_database()`).Scan(&names); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	var want []string
	for _, name := range poolNames {
		want = append(want, "dispatch-"+name)
	}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("pg_stat_activity names %v, want %v", names, want)
	}

	named, err := url.Parse(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	query := named.Query()
	query.Set("application_name", "pooltest_"+randomDatabaseSuffix(t))
	named.RawQuery = query.Encode()
	database, err := Open(ctx, named.String())
	if err != nil {
		t.Fatalf("open a store whose connection string names an application: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	conn := holdOne(t, database.Pool.Acquire)
	var name string
	if err := conn.QueryRow(ctx, `select application_name from pg_stat_activity where pid = pg_backend_pid()`).Scan(&name); err != nil {
		t.Fatalf("read the connection's application_name: %v", err)
	}
	if name != "dispatch-requests" {
		t.Errorf("a request connection from a connection string naming an application is %q, want dispatch-requests", name)
	}
}

// holdOne takes one connection with acquire and holds it until the test ends.
func holdOne(t *testing.T, acquire func(context.Context) (*pgxpool.Conn, error)) *pgxpool.Conn {
	t.Helper()
	conn, err := acquire(context.Background())
	if err != nil {
		t.Fatalf("hold a connection: %v", err)
	}
	t.Cleanup(conn.Release)
	return conn
}

// The lease pool has a connection for every lease name duty.Exclusive accepts, and two spare.
func TestTheLeasePoolCountsEveryLeaseName(t *testing.T) {
	names := len(duty.LeaseNames())
	if leaseNameCount != names || leasePoolSize != names+2 {
		t.Errorf("leaseNameCount %d and leasePoolSize %d, want %d lease names and %d connections",
			leaseNameCount, leasePoolSize, names, names+2)
	}
}
