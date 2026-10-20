package duty

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// leasesOf is the pool Exclusive takes its lease connections from, the one the server gives it.
func leasesOf(t *testing.T, database *store.Store) *pgxpool.Pool {
	t.Helper()
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	return leases
}

// Two processes on one database are two callers here, and the second must skip rather than queue:
// it is refused while the first is still inside its pass, which is the only moment that matters.
func TestExclusiveRefusesASecondCallerWhileTheFirstIsInsideItsPass(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()

	inside, finish := make(chan struct{}), make(chan struct{})
	first := make(chan bool, 1)
	go func() {
		first <- Exclusive(ctx, leases, "compaction", func(context.Context) {
			close(inside)
			<-finish
		})
	}()
	<-inside

	secondRan := false
	if Exclusive(ctx, leases, "compaction", func(context.Context) { secondRan = true }) {
		t.Error("the second caller took a lease the first holds")
	}
	if secondRan {
		t.Error("the second caller ran its pass while the first was inside its own")
	}
	close(finish)
	if !<-first {
		t.Error("the first caller did not take the lease")
	}
}

// The lease is the pass's, not the process's: the next call takes it once the pass returns.
func TestExclusiveFreesTheLeaseWhenThePassReturns(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()

	for pass := 1; pass <= 2; pass++ {
		ran := false
		if !Exclusive(ctx, leases, "compaction", func(context.Context) { ran = true }) || !ran {
			t.Fatalf("pass %d did not run under its own lease", pass)
		}
	}
}

// Each loop has a lease of its own, so one loop's long pass never keeps another loop's pass from
// running: the second key is the name's hashtext.
func TestExclusiveLeasesOfDifferentNamesDoNotExcludeEachOther(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()

	otherRan := false
	held := Exclusive(ctx, leases, "compaction", func(context.Context) {
		otherRan = Exclusive(ctx, leases, "redelivery sweep", func(context.Context) {})
	})
	if !held {
		t.Fatal("the first lease was not taken")
	}
	if !otherRan {
		t.Error("a second lease name was refused while the first was held")
	}
}

// The holder's death releases the lease, which is why a session-level lock is what the loops need:
// a process killed mid-pass - a deploy replacing its task - leaves nothing behind for the survivor
// to wait out. A connection closed with the lease still locked is that death.
func TestExclusiveTakesALeaseWhoseHolderDiedWithoutUnlocking(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()
	const name = "architecture import"

	holder, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the dying holder's connection: %v", err)
	}
	var locked bool
	if err := holder.QueryRow(ctx, takeLease, leaseNamespace, name).Scan(&locked); err != nil || !locked {
		holder.Release()
		t.Fatalf("hold the lease: %v (%t)", err, locked)
	}
	// Hijack takes the connection out of the pool, so closing it ends that session for good
	// rather than handing it back; nothing unlocks the lease first.
	dead := holder.Hijack()
	if err := dead.Close(ctx); err != nil {
		t.Fatalf("close the holder's connection: %v", err)
	}
	waitForRelease(t, database, name)

	ran := false
	if !Exclusive(ctx, leases, name, func(context.Context) { ran = true }) || !ran {
		t.Fatal("the dead holder's lease was not free for the next pass")
	}
}

// waitForRelease waits for a dead session's lease to be gone. Closing a connection sends Terminate
// and returns without waiting for the server, and Postgres releases a session's advisory locks when
// its backend exits, so the next statement can arrive first. Waiting on the lease itself - a
// blocking pg_advisory_lock, unlocked again at once - is exactly that wait, and needs no polling.
func waitForRelease(t *testing.T, database *store.Store, name string) {
	t.Helper()
	ctx := context.Background()
	waiter, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection to wait on the lease: %v", err)
	}
	defer waiter.Release()
	if _, err := waiter.Exec(ctx, `select pg_advisory_lock($1, hashtext($2))`, leaseNamespace, name); err != nil {
		t.Fatalf("wait for the dead holder's lease: %v", err)
	}
	var freed bool
	if err := waiter.QueryRow(ctx, freeLease, leaseNamespace, name).Scan(&freed); err != nil || !freed {
		t.Fatalf("give the waited-for lease back: %v (%t)", err, freed)
	}
}
