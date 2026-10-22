package duty

import (
	"context"
	"os"
	"testing"
	"time"

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
// No period (0) in this row and the two below it: each is about the lease alone, and a period
// would be a second reason to skip that would hide which one refused the caller.
func TestExclusiveRefusesASecondCallerWhileTheFirstIsInsideItsPass(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()

	inside, finish := make(chan struct{}), make(chan struct{})
	first := make(chan bool, 1)
	go func() {
		first <- Exclusive(ctx, leases, "compaction", 0, func(context.Context) {
			close(inside)
			<-finish
		})
	}()
	<-inside

	secondRan := false
	if Exclusive(ctx, leases, "compaction", 0, func(context.Context) { secondRan = true }) {
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
		if !Exclusive(ctx, leases, "compaction", 0, func(context.Context) { ran = true }) || !ran {
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
	held := Exclusive(ctx, leases, "compaction", 0, func(context.Context) {
		otherRan = Exclusive(ctx, leases, "redelivery sweep", 0, func(context.Context) {})
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
	if !Exclusive(ctx, leases, name, 0, func(context.Context) { ran = true }) || !ran {
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

// A rolling update starts the second process minutes after the first, so the two processes' ticks
// of one loop never meet inside a pass: the first has finished and given the lease back long
// before the second boots, and mutual exclusion alone would let the second run the whole pass
// again. The pass the first recorded is what refuses it, for as long as the loop's own period,
// and no longer: once that period has run out the next tick runs, whichever process makes it.
func TestExclusiveSkipsASecondProcessInsideThePeriod(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	leases, otherLeases := leasesOf(t, database), leasesOf(t, second)
	ctx := context.Background()
	const name = "webhook-redelivery"
	const period = time.Hour

	ran := false
	if !Exclusive(ctx, leases, name, period, func(context.Context) { ran = true }) || !ran {
		t.Fatal("the first process's pass did not run")
	}

	otherRan := false
	if Exclusive(ctx, otherLeases, name, period, func(context.Context) { otherRan = true }) {
		t.Error("the second process took the pass inside a period the first one had served")
	}
	if otherRan {
		t.Error("the second process ran the whole pass again inside the period the first one served")
	}

	// The recorded pass moved back is the period having run out, which no test waits an hour
	// for: the repository's other claim rows age their row the same way rather than sleeping.
	movePassBack(t, database, name, 2)
	if !Exclusive(ctx, otherLeases, name, period, func(context.Context) { otherRan = true }) || !otherRan {
		t.Error("no pass ran once the recorded one was older than the period")
	}
}

// A pass shutdown cut short is a part of a pass, not one, so it serves no period: the process
// that is up at the next tick makes the pass. Recording it would cost a maintenance loop a whole
// period of work for a task replaced mid-pass - for the webhook sweep, a window of failed
// deliveries nobody lists - with nothing to show for it afterwards, and a rolling update's
// SIGTERM is the ordinary way into it.
func TestExclusiveLeavesThePeriodUnservedWhenShutdownCutsThePassShort(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	const name = "architecture-import"
	const period = time.Hour

	stopping, stop := context.WithCancel(context.Background())
	cut := false
	if Exclusive(stopping, leases, name, period, func(context.Context) {
		cut = true
		// The shutdown lands while the pass is still working, as a SIGTERM does.
		stop()
	}) {
		t.Error("a pass its shutdown cut short was reported as a pass that ran")
	}
	if !cut {
		t.Fatal("the pass never ran, so this row measures nothing")
	}

	ran := false
	if !Exclusive(context.Background(), leases, name, period, func(context.Context) { ran = true }) || !ran {
		t.Error("the next tick skipped a period the cut-short pass never served")
	}
}

// A period of zero or less is no period at all - mutual exclusion and nothing more - which is what
// the delivery consumers' reconciliation takes: it runs on every bind poll rather than on a
// ticker, and the throttle that spaces it is its own. Every call runs while the lease is free, and
// nothing is recorded for a later one to be judged by.
func TestExclusiveWithoutAPeriodRunsWheneverTheLeaseIsFree(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()
	const name = "delivery-consumers"

	for pass := 1; pass <= 3; pass++ {
		ran := false
		if !Exclusive(ctx, leases, name, 0, func(context.Context) { ran = true }) || !ran {
			t.Fatalf("pass %d did not run with no period and the lease free", pass)
		}
	}
	if recorded := recordedPasses(t, database, name); recorded != 0 {
		t.Errorf("a loop with no period recorded %d passes; it has no period for one to serve", recorded)
	}
}

// A period is its own loop's, as the lease is: the record is kept per lease, so the compaction
// that has just run holds back no sweep.
func TestExclusiveAPeriodHoldsBackOnlyItsOwnLoop(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()
	const period = time.Hour

	if !Exclusive(ctx, leases, "document-compaction", period, func(context.Context) {}) {
		t.Fatal("the first loop's pass did not run")
	}
	ran := false
	if !Exclusive(ctx, leases, "webhook-redelivery", period, func(context.Context) { ran = true }) || !ran {
		t.Error("a loop's pass was refused by another loop's period")
	}
}

// A loop whose period changes between ticks must keep running. The reference-excerpt
// reconciliation is one: it asks for referenceExcerptStep (2 s) while a full batch says drift
// remains and for ReferenceExcerptReconcileInterval (5 min) once none does, and it passes that
// wait as its period (docs.RunReferenceExcerptReconciliation). So a pass recorded under the
// short period is read back by a tick under the long one, and a record that cannot be compared
// across the two stops the loop for good: every mention a version write could not cut, and
// everything rebuild-refs inserts, keeps the document's name instead of an excerpt, with no line
// saying why.
func TestExclusiveDoesNotLockOutALoopWhosePeriodGrows(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	ctx := context.Background()
	const name = "reference-excerpts"
	const step, interval = 2 * time.Second, 5 * time.Minute

	stepped := false
	if !Exclusive(ctx, leases, name, step, func(context.Context) { stepped = true }) || !stepped {
		t.Fatal("the drift step's own pass did not run, so this row measures nothing")
	}
	ran := false
	if !Exclusive(ctx, leases, name, interval, func(context.Context) { ran = true }) || !ran {
		t.Error("the loop's full-interval tick was refused by the pass its own shorter-period tick recorded; no later tick of that loop can ever run")
	}
}

// movePassBack moves the pass recorded for name back by age, which is how a period that has run
// out is reached without waiting one out.
func movePassBack(t *testing.T, database *store.Store, name string, periods int64) {
	t.Helper()
	moved, err := database.Pool.Exec(context.Background(),
		`update duty_passes set bucket = bucket - $2 where lease = $1`, name, periods)
	if err != nil {
		t.Fatalf("move the recorded %s pass back: %v", name, err)
	}
	if moved.RowsAffected() != 1 {
		t.Fatalf("moved %d recorded %s passes back; the pass that ran recorded none", moved.RowsAffected(), name)
	}
}

// recordedPasses is how many passes duty_passes holds for name.
func recordedPasses(t *testing.T, database *store.Store, name string) int {
	t.Helper()
	var recorded int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from duty_passes where lease = $1`, name).Scan(&recorded); err != nil {
		t.Fatalf("count the recorded %s passes: %v", name, err)
	}
	return recorded
}
