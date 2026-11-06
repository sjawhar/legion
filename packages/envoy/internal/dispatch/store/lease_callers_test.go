package store

import (
	"strconv"
	"strings"
	"testing"
)

// reserveLeaseCallers reserves count callers of the lease pool, each under a name of its own, and
// returns the names it reserved.
func reserveLeaseCallers(t *testing.T, callers *LeaseCallers, count int) []string {
	t.Helper()
	names := make([]string, 0, count)
	for i := range count {
		name := "caller " + strconv.Itoa(i)
		if _, err := callers.For(name); err != nil {
			t.Fatalf("reserve %s: %v", name, err)
		}
		names = append(names, name)
	}
	return names
}

// The lease pool has one connection per caller that can be inside a lease or a hold statement at
// the same moment, and the reservations are what holds the two together: exactly leasePoolSize
// callers pass, one more (a leased loop added without a connection) and one fewer (a connection
// added without a caller) fail, so neither can be merged in silently.
func TestTheLeasePoolIsCompleteOnlyAtOneCallerPerConnection(t *testing.T) {
	database := openTestStore(t)

	short, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	reserveLeaseCallers(t, short, leasePoolSize-1)
	if err := short.Complete(); err == nil {
		t.Errorf("%d callers of a lease pool of %d passed the check: a connection nothing reserved is one the process opens and nothing uses", leasePoolSize-1, leasePoolSize)
	}

	exact := leaseCallersOfANewPool(t)
	reserveLeaseCallers(t, exact, leasePoolSize)
	if err := exact.Complete(); err != nil {
		t.Errorf("%d callers of a lease pool of %d failed the check: %v", leasePoolSize, leasePoolSize, err)
	}

	over := leaseCallersOfANewPool(t)
	names := reserveLeaseCallers(t, over, leasePoolSize+1)
	err = over.Complete()
	if err == nil {
		t.Fatalf("%d callers of a lease pool of %d passed the check: the one without a connection waits for one for as long as the longest pass", leasePoolSize+1, leasePoolSize)
	}
	// The error says how many callers reserved a connection, how many the pool has, and which
	// callers they are, so whoever reads it can tell the caller that was added from the ones
	// the count was taken for.
	for _, want := range []string{strconv.Itoa(leasePoolSize + 1), strconv.Itoa(leasePoolSize)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %s, the callers reserved and the pool's connections", err, want)
		}
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %q does not name the reserved caller %q", err, name)
		}
	}
}

// Two callers cannot share one connection, so a name already reserved is refused: a second
// reservation under one name would count two callers as one.
func TestTheLeasePoolRefusesACallerNameTwice(t *testing.T) {
	callers := leaseCallersOfANewPool(t)
	if _, err := callers.For("document compaction"); err != nil {
		t.Fatalf("reserve the first caller: %v", err)
	}
	if _, err := callers.For("document compaction"); err == nil {
		t.Error("the lease pool reserved one name twice")
	} else if !strings.Contains(err.Error(), "document compaction") {
		t.Errorf("the refusal %q does not name the caller reserved twice", err)
	}
}

// Every call returns the one registry the pool keeps, so a caller that asks for the lease pool
// where it is wired is counted beside every other caller's reservation.
func TestTheLeasePoolKeepsOneRegistryPerPool(t *testing.T) {
	database := openTestStore(t)
	first, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	if _, err := first.For("the first caller"); err != nil {
		t.Fatalf("reserve a caller: %v", err)
	}
	second, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool again: %v", err)
	}
	if got := second.Reserved(); len(got) != 1 || got[0] != "the first caller" {
		t.Errorf("the second registry holds %q, want the reservation the first took", got)
	}
}

// leaseCallersOfANewPool opens a database of its own and returns its lease pool's registry, so a
// test can reserve a set of callers that no other set is counted with.
func leaseCallersOfANewPool(t *testing.T) *LeaseCallers {
	t.Helper()
	callers, err := openTestStore(t).Pool.Leases()
	if err != nil {
		t.Fatalf("open the lease pool: %v", err)
	}
	return callers
}
