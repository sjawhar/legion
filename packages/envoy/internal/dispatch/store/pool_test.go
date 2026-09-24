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
