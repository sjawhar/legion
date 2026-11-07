package storetest

import (
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// testLeaseCallers numbers the reservations Leases takes, so that two in one test are two
// callers: the lease pool refuses one name twice.
var testLeaseCallers atomic.Uint64

// Leases reserves a connection of pool's lease pool and returns the pool it is taken from. The
// server reserves one per caller that can be inside a lease or a hold statement at the same
// moment (store.Pool.Leases), and a test that takes a lease or a hold is such a caller; the
// count the server checks is the server's own wiring, so a test reserves whatever it needs.
func Leases(t testing.TB, pool *store.Pool) *pgxpool.Pool {
	t.Helper()
	callers, err := pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	leases, err := callers.For(t.Name() + " " + strconv.FormatUint(testLeaseCallers.Add(1), 10))
	if err != nil {
		t.Fatalf("reserve a connection of the lease pool: %v", err)
	}
	return leases
}
