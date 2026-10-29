package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Traced opens a second store on the database database is already on, whose every connection
// reports what it runs to tracer, and closes it after t ends. A test that counts a read's round
// trips drives a server on it: pgx reports one query start per statement sent on its own and one
// batch start per batch, whatever that batch holds, so a count of the two is the read's round
// trips. tracer may also implement pgx.BatchTracer, which pgx then uses for batches.
//
// Everything else is the configuration database's own pool was opened with, as SecondProcess
// copies it; the production pool is built in store.Open and is not a seam a test may set a
// tracer on.
func Traced(t testing.TB, database *store.Store, tracer pgx.QueryTracer) *store.Store {
	t.Helper()
	config := database.Pool.Config()
	config.ConnConfig.Tracer = tracer
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a traced pool on the same database: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}
