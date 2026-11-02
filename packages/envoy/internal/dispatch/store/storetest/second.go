package storetest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SecondProcess opens a second store on the database database is already on, closed after t ends.
// Production runs two envoy-dispatch processes against one Postgres, and each holds a pool of its
// own: a test of what the two arbitrate between them through the database - the outbox's
// publisher hold (duty.Hold), a lease one of them runs a duty under (duty.Exclusive), a row both
// write - has to give each of them a pool of its own too. Two callers sharing a store share one
// process's pool and its connection limit, which is not the deployment such a test is about.
func SecondProcess(t testing.TB, database *store.Store) *store.Store {
	t.Helper()
	return openStore(t, database.Pool.Config(), "open a second process's pool on the same database")
}

// TracedStore opens a store of its own on the database database is already on, closed after t
// ends, whose every connection, and every connection of every pool it opens later, reports to
// tracer: the store opens each later pool from a copy of the configuration tracer is installed on
// (Traced). pgx hands tracer each pipeline too when it is also a pgx.BatchTracer.
func TracedStore(t testing.TB, database *store.Store, tracer pgx.QueryTracer) *store.Store {
	t.Helper()
	return &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
}

// openStore opens a store on a pool of config, closed after t ends, failing t with what on error.
func openStore(t testing.TB, config *pgxpool.Config, what string) *store.Store {
	t.Helper()
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}
