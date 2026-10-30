package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// acquireCount is how many acquires pool's own tracer has counted.
func acquireCount(pool *Pool) uint64 {
	return pool.Stats()[0].Acquire.Count
}

// Every way the request pool hands out a connection runs on a pinned context's one connection:
// the first call takes it, and every later one - cursors, rows, statements, transactions, batches,
// copies, AcquireFunc - runs on it without another acquire. The request holds it between calls
// and gives it back at its end, after which a call takes a connection for itself again.
func TestAPinnedContextRunsEveryCallOnOneConnection(t *testing.T) {
	pool := smallRequestPool(t, 2, testAcquireBound)
	ctx, end := PinConnection(WithTransactionTracking(context.Background()))
	if ConnectionHeld(ctx) {
		t.Fatal("a pinned context holds a connection before its first call")
	}

	before := acquireCount(pool)
	calls := poolCalls("select 1")
	// The temporary table lives in the connection's session, so the copy finds it only on the
	// connection the transaction created it on.
	calls = append(calls, poolCall{"Begin+create temporary table", func(ctx context.Context, pool *Pool) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "create temporary table pinned_copy (value int)"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}}, poolCall{"CopyFrom", func(ctx context.Context, pool *Pool) error {
		_, err := pool.CopyFrom(ctx, pgx.Identifier{"pinned_copy"}, []string{"value"}, pgx.CopyFromRows([][]any{{1}}))
		return err
	}})
	for _, call := range calls {
		if call.name == "Acquire" {
			continue
		}
		if err := call.call(ctx, pool); err != nil {
			t.Fatalf("%s on a pinned context: %v", call.name, err)
		}
		if acquired := pool.Stat().AcquiredConns(); acquired != 1 {
			t.Fatalf("after %s the pool has %d connections taken, want the pinned one", call.name, acquired)
		}
	}
	if acquires := acquireCount(pool) - before; acquires != 1 {
		t.Fatalf("every call on a pinned context took %d acquires, want 1", acquires)
	}
	if !ConnectionHeld(ctx) {
		t.Fatal("a pinned context between calls reports no connection held, want its pinned one")
	}

	var one int

	end()
	if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("the pin's end left %d connections taken, want 0", acquired)
	}
	if ConnectionHeld(ctx) {
		t.Fatal("an ended pin still reports its connection held")
	}
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("a read after the pin ended: %v", err)
	}
	if acquired, acquires := pool.Stat().AcquiredConns(), acquireCount(pool)-before; acquired != 0 || acquires != 2 {
		t.Fatalf("a read after the pin ended left %d connections taken after %d acquires, want 0 after 2", acquired, acquires)
	}
}

// A pinned request is refused a connection of the other guarded pools. It holds its request-pool
// connection between its statements, so a background, live or settlements connection handed to it
// would be a second connection of these pools held at once, which is the cycle ErrNestedAcquire
// exists to prevent; only the request pool serves a pin (PinConnection). Once the pin has ended
// those pools serve the context as they serve any other caller.
func TestAPinnedContextIsRefusedTheOtherPools(t *testing.T) {
	pool := smallRequestPool(t, 2, testAcquireBound)
	ctx, end := PinConnection(WithTransactionTracking(context.Background()))
	defer end()
	var one int
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	if !ConnectionHeld(ctx) {
		t.Fatal("the pin holds no connection, so nothing here is tested")
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
		for _, call := range poolCalls("select 1") {
			if err := call.call(ctx, opened); !errors.Is(err, ErrNestedAcquire) {
				t.Errorf("%s on the %s pool under a held pin: %v, want ErrNestedAcquire", call.name, separate.name, err)
			}
		}
		if conns := opened.Stat().AcquiredConns(); conns != 0 {
			t.Errorf("the %s pool has %d connections taken under a held pin, want 0", separate.name, conns)
		}
	}

	end()
	background, err := pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	if err := background.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("a background-pool read after the pin ended: %v, want it served", err)
	}
}

// Acquire under a pin hands the pinned connection over instead of taking a second one; its caller
// releases it to the pool, and the request's next call takes another.
func TestAcquireHandsOverThePinnedConnection(t *testing.T) {
	pool := smallRequestPool(t, 1, testAcquireBound)
	ctx, end := PinConnection(WithTransactionTracking(context.Background()))
	defer end()

	var one int
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	before := acquireCount(pool)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire under a held pin on a one-connection pool: %v, want the pinned connection", err)
	}
	if acquires := acquireCount(pool) - before; acquires != 0 {
		t.Fatalf("acquire under a held pin took %d acquires, want the pinned connection handed over", acquires)
	}
	conn.Release()
	if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("the handed-over connection's release left %d taken, want 0", acquired)
	}
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("a read after the hand-over: %v", err)
	}
	if acquires := acquireCount(pool) - before; acquires != 1 {
		t.Fatalf("the read after the hand-over took %d acquires, want 1", acquires)
	}
}

// A pinned connection runs one call at a time. A call that arrives while another still runs on it
// - under the request's own open cursor, transaction or unscanned row, or from a goroutine sharing
// the request's context while a statement of its runs - is refused at once with ErrNestedAcquire,
// as a second connection would be, never queued on the connection or interleaved with the call on
// it. Once the call ends the next is served, on a fresh connection where the call's cancellation
// closed the pinned one.
func TestAPinnedConnectionRefusesACallWhileAnotherRunsOnIt(t *testing.T) {
	pool := smallRequestPool(t, 2, testAcquireBound)
	ctx, end := PinConnection(WithTransactionTracking(context.Background()))
	defer end()
	var one int
	refused := func(holder string) {
		t.Helper()
		for _, call := range poolCalls("select 1") {
			if err := call.call(ctx, pool); !errors.Is(err, ErrNestedAcquire) {
				t.Errorf("%s under %s: %v, want ErrNestedAcquire", call.name, holder, err)
			}
		}
		if _, err := pool.CopyFrom(ctx, pgx.Identifier{"pinned_copy"}, []string{"value"}, pgx.CopyFromRows(nil)); !errors.Is(err, ErrNestedAcquire) {
			t.Errorf("CopyFrom under %s: %v, want ErrNestedAcquire", holder, err)
		}
		if _, err := ForConcurrentRead(ctx); !errors.Is(err, ErrNestedAcquire) {
			t.Errorf("concurrent reads under %s: %v, want ErrNestedAcquire", holder, err)
		}
	}

	rows, err := pool.Query(ctx, "select generate_series(1, 3)")
	if err != nil {
		t.Fatalf("open a cursor on the pinned connection: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("read the cursor's first row: %v", rows.Err())
	}
	refused("an open cursor")
	rows.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin on the pinned connection: %v", err)
	}
	defer tx.Rollback(ctx)
	refused("an open transaction")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	row := pool.QueryRow(ctx, "select 1")
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); !errors.Is(err, ErrNestedAcquire) {
		t.Errorf("a read under an unscanned row: %v, want ErrNestedAcquire", err)
	}
	if err := row.Scan(&one); err != nil {
		t.Fatalf("scan the first row: %v", err)
	}

	sleeping, cancel := context.WithCancel(ctx)
	defer cancel()
	slept := make(chan error, 1)
	go func() {
		_, err := pool.Exec(sleeping, "select pg_sleep(30) /* pinned-sleep */")
		slept <- err
	}()
	awaitActiveQuery(t, pool, "%pinned-sleep%", slept)
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); !errors.Is(err, ErrNestedAcquire) {
		t.Errorf("a read beside another goroutine's statement on the pinned connection: %v, want ErrNestedAcquire", err)
	}
	cancel()
	if err := <-slept; err == nil {
		t.Fatal("the cancelled sleep answered no error")
	}
	// The cancellation may have closed the pinned connection; the request then takes one
	// replacement, which every later call runs on.
	before := acquireCount(pool)
	for range 3 {
		if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
			t.Fatalf("a read after the cancelled statement: %v, want it served", err)
		}
	}
	if acquires := acquireCount(pool) - before; acquires > 1 {
		t.Fatalf("three reads after the cancelled statement took %d acquires, want at most the one replacement", acquires)
	}
}

// awaitActiveQuery returns once a query matching pattern runs on the database, read on a
// connection of pool's own, and fails the test if ran answers first.
func awaitActiveQuery(t *testing.T, pool *Pool, pattern string, ran <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var running bool
		if err := pool.QueryRow(context.Background(),
			`select exists (select 1 from pg_stat_activity where state = 'active' and query like $1 and pid <> pg_backend_pid())`,
			pattern).Scan(&running); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if running {
			return
		}
		select {
		case err := <-ran:
			t.Fatalf("the statement answered %v before it was seen running", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the statement was never seen running")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A pin's end while a call still runs on its connection leaves the connection to that call, and
// the connection goes back to the pool when the call ends rather than being released under it.
func TestAPinEndedDuringACallGivesItsConnectionBackWhenTheCallEnds(t *testing.T) {
	pool := smallRequestPool(t, 1, testAcquireBound)
	ctx, end := PinConnection(WithTransactionTracking(context.Background()))
	rows, err := pool.Query(ctx, "select generate_series(1, 3)")
	if err != nil {
		t.Fatalf("open a cursor on the pinned connection: %v", err)
	}
	defer rows.Close()
	end()
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil || count != 3 {
		t.Fatalf("read %d rows, error %v, after the pin ended, want all 3", count, err)
	}
	if acquired := pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("%d connections taken once the cursor ran out after the pin ended, want 0", acquired)
	}
	// The connection is back, so the one-connection pool serves an unpinned caller.
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire after the pinned cursor ended: %v", err)
	}
	conn.Release()
}
