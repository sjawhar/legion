package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Reopened opens a second store on the database database is already on, from the configuration
// database's own pool was opened with and whatever change configure makes to a copy of it, and
// closes it after t ends. The production pool is built in store.Open and is not a seam a test
// may configure, so a test that needs a pool of its own shape reopens one here.
func Reopened(t testing.TB, database *store.Store, configure func(*pgxpool.Config)) *store.Store {
	t.Helper()
	config := database.Pool.Config()
	configure(config)
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a second pool on the same database: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}

// Traced is a Reopened store whose every connection reports what it runs to tracer. A test that
// counts a read's round trips drives a server on it: pgx reports one query start per statement
// sent on its own and one batch start per batch, whatever that batch holds, so a count of the two
// is the read's round trips. tracer may also implement pgx.BatchTracer, which pgx then uses for
// batches.
func Traced(t testing.TB, database *store.Store, tracer pgx.QueryTracer) *store.Store {
	t.Helper()
	return Reopened(t, database, func(config *pgxpool.Config) { config.ConnConfig.Tracer = tracer })
}

// SingleConnection is a Reopened store of one connection, which every read on it takes, so a test
// reads one connection's own state - the statements it has prepared (pg_prepared_statements) - as
// the reads it drives leave it.
func SingleConnection(t testing.TB, database *store.Store) *store.Store {
	t.Helper()
	return Reopened(t, database, func(config *pgxpool.Config) {
		config.MaxConns = 1
		config.MinConns = 0
		config.MinIdleConns = 0
	})
}
