package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// What AfterCommit registers runs once, when the transaction ends, with whether it committed: a
// commit runs it with true, a rollback with false, and the deferred rollback every caller runs
// after its commit runs nothing again. A transaction opened with no hold mark on its context is
// tracked as well, and one the pool did not open - begun on an acquired connection - is refused,
// as is a transaction that has already ended.
func TestAfterCommitRunsOnceWhenTheTransactionEnds(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		end  func(pgx.Tx) error
		want []bool
	}{
		{name: "commit", end: func(tx pgx.Tx) error { return tx.Commit(ctx) }, want: []bool{true}},
		{name: "rollback", end: func(tx pgx.Tx) error { return tx.Rollback(ctx) }, want: []bool{false}},
		{name: "commit then deferred rollback", end: func(tx pgx.Tx) error {
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			if err := tx.Rollback(ctx); !errors.Is(err, pgx.ErrTxClosed) {
				return fmt.Errorf("rollback after commit = %v, want pgx.ErrTxClosed", err)
			}
			return nil
		}, want: []bool{true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx)
			var ran []bool
			if !AfterCommit(tx, func(committed bool) { ran = append(ran, committed) }) {
				t.Fatal("AfterCommit refused a transaction of the pool")
			}
			if len(ran) != 0 {
				t.Fatalf("AfterCommit ran %v before the transaction ended", ran)
			}
			if err := tc.end(tx); err != nil {
				t.Fatalf("end the transaction: %v", err)
			}
			if !slices.Equal(ran, tc.want) {
				t.Fatalf("AfterCommit ran with %v, want %v", ran, tc.want)
			}
			if AfterCommit(tx, func(bool) { t.Error("a function registered after the end ran") }) {
				t.Error("AfterCommit accepted a transaction that has ended")
			}
		})
	}

	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection: %v", err)
	}
	defer conn.Release()
	own, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin on the acquired connection: %v", err)
	}
	defer own.Rollback(ctx)
	if AfterCommit(own, func(bool) { t.Error("a function registered on a transaction the pool did not open ran") }) {
		t.Error("AfterCommit accepted a transaction the pool did not open")
	}
	if err := own.Commit(ctx); err != nil {
		t.Fatalf("commit on the acquired connection: %v", err)
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
// the property is the same at any size, and holding sixty-four server connections while the
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

// RoomAppends hands out connections of a pool of its own: a connection taken there is not one of
// the shared pool's, names itself dispatch-room-appends to Postgres, and is counted by Use, which
// the process drain waits on (cmd/dispatch's waitUntilIdle) before it closes the pool. A rooms
// connection names itself dispatch-rooms, so pg_stat_activity tells a load from a room's append
// and both from a request.
func TestRoomAppendsIsAPoolOfItsOwnThatUseCounts(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	appends, err := database.Pool.RoomAppends()
	if err != nil {
		t.Fatalf("open the room-appends pool: %v", err)
	}
	if again, err := database.Pool.RoomAppends(); err != nil || again != appends {
		t.Fatalf("a second RoomAppends = %p, %v; want the pool the first opened, %p", again, err, appends)
	}
	if got := appends.Config().MaxConns; got != roomAppendsPoolSize {
		t.Fatalf("room-appends pool holds at most %d connections, want %d", got, roomAppendsPoolSize)
	}

	sharedAcquires := database.Pool.Stat().AcquireCount()
	before := database.Pool.Use()
	conn, err := appends.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a room-appends connection: %v", err)
	}
	released := false
	defer func() {
		if !released {
			conn.Release()
		}
	}()
	var name string
	if err := conn.QueryRow(ctx, "select current_setting('application_name')").Scan(&name); err != nil {
		t.Fatalf("read the room-appends connection's application name: %v", err)
	}
	if name != roomAppendsApplicationName {
		t.Fatalf("room-appends connection's application_name = %q, want %q", name, roomAppendsApplicationName)
	}
	if got := database.Pool.Stat().AcquireCount(); got != sharedAcquires {
		t.Fatalf("a room-appends connection took %d connections of the shared pool, want none", got-sharedAcquires)
	}
	if use := database.Pool.Use(); use.InUse != before.InUse+1 {
		t.Fatalf("Use counts %d connections in use with one room-appends connection held, want %d", use.InUse, before.InUse+1)
	}
	conn.Release()
	released = true
	if use := database.Pool.Use(); use.InUse != before.InUse || use.Returned != before.Returned+1 {
		t.Fatalf("after the room-appends connection's release Use = %+v, want %d in use and %d returned",
			use, before.InUse, before.Returned+1)
	}

	rooms, err := database.Pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}
	if err := rooms.QueryRow(ctx, "select current_setting('application_name')").Scan(&name); err != nil {
		t.Fatalf("read the rooms connection's application name: %v", err)
	}
	if name != roomsApplicationName {
		t.Fatalf("rooms connection's application_name = %q, want %q", name, roomsApplicationName)
	}
}

// Close waits for every connection in use to come back, and the health probe answers for as long
// as it waits: a caller waiting on Close (cmd/dispatch's closePool) asks it whether the connection
// it waits on will ever come back. Use keeps counting that connection, and once Close has returned
// the health probe's pool has closed too.
func TestHealthyAnswersWhileCloseWaitsForAConnectionInUse(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()
	// The health probe reads the applied schema version, so the database must have one.
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	held, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a shared connection: %v", err)
	}
	released := false
	defer func() {
		if !released {
			held.Release()
		}
	}()

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		database.Pool.Close()
	}()
	// Close refuses the other separate pools from the moment it begins.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := database.Pool.Rooms(); errors.Is(err, ErrPoolClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close never began")
		}
	}
	if _, err := database.Pool.Healthy(ctx); err != nil {
		t.Fatalf("health probe while Close waits for a connection in use: %v, want an answer", err)
	}
	if use := database.Pool.Use(); use.InUse != 1 {
		t.Fatalf("Use counts %d connections in use while Close waits for the one held, want 1", use.InUse)
	}
	select {
	case <-closed:
		t.Fatal("Close returned with a connection still in use")
	default:
	}

	held.Release()
	released = true
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return once the connection in use came back")
	}
	if _, err := database.Pool.Healthy(ctx); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("health probe after Close: %v, want ErrPoolClosed", err)
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
