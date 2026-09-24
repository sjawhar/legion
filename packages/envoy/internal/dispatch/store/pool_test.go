package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The pool refuses a second connection to a caller that already holds one of its transactions:
// that acquisition is what deadlocks the pool once every connection is held by a writer waiting
// on a lock. A caller that needs a connection of its own opens it elsewhere - the document
// rooms pool - and a caller that only needs to read does it through its transaction.
func TestPoolRefusesASecondConnectionInsideATransaction(t *testing.T) {
	database := openTestStore(t)
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
	if err := database.Pool.Ping(ctx); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("ping inside a transaction: %v, want ErrNestedAcquire", err)
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
