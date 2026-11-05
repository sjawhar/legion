package storetest

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SecondProcess opens a second store on the database database is already on, closed after t ends.
// Production runs two envoy-dispatch processes against one Postgres, and each holds a pool of its
// own: a test of what the two arbitrate between them through the database - the outbox's
// publisher hold (duty.Hold), a lease one of them runs a duty under (duty.Exclusive), a row both
// write - has to give each of them a pool of its own too. Two callers sharing a store share one
// process's pool and its connection limit, which is not the deployment such a test is about.
// It is Reopened with the configuration unchanged, the second process being the same deployment
// twice.
func SecondProcess(t testing.TB, database *store.Store) *store.Store {
	t.Helper()
	return Reopened(t, database, func(*pgxpool.Config) {})
}
