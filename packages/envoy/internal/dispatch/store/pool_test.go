package store

import (
	"context"
	"errors"
	"testing"
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
	if _, err := database.Pool.Begin(ctx); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("second transaction: %v, want ErrNestedAcquire", err)
	}
	if _, err := database.Pool.Acquire(ctx); !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("connection acquired inside a transaction: %v, want ErrNestedAcquire", err)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatalf("pool read after the transaction ended: %v", err)
	}
}
