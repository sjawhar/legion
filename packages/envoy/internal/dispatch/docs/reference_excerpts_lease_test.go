package docs

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// excerptLeases is the pool a reconciliation tick takes its lease on, the one cmd/dispatch gives
// the loop.
func excerptLeases(t *testing.T, database *store.Store) *pgxpool.Pool {
	t.Helper()
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	return leases
}

// countExcerptLeaseLines counts the lease lines carrying message for lease.
func countExcerptLeaseLines(logs *storetest.LockedLog, message, lease string) int {
	count := 0
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, `msg="`+message+`"`) && strings.Contains(line, "lease="+lease) {
			count++
		}
	}
	return count
}

// Two envoy-dispatch processes reconcile one database's reference excerpts, each through
// RunReferenceExcerptReconciliation itself and each on a pool of its own (storetest.SecondProcess,
// as a second process has): one takes the reference-excerpts lease and runs the tick, the other
// finds it held and skips rather than cutting the same excerpts from the same documents. The
// first tick is held inside its own first statement by a lock this test takes on refs, which is
// the moment the lease is about - both processes' loops ticking at once - and both runners are
// stopped before that lock is released, so the tick runs exactly once.
func TestTwoReconciliationRunnersRunTheTickOnceAndSkipOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	firstLeases, secondLeases := excerptLeases(t, database), excerptLeases(t, second)
	document := createProjectDocument(t, database, "# Document\n\nSee dispatch://CORE-1 here.\n")
	if _, err := database.Pool.Exec(ctx,
		`insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', 'CORE-1')`, document); err != nil {
		t.Fatalf("index the document's mention: %v", err)
	}
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	newRunner := func(database *store.Store) *Service {
		service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		return service
	}
	first, other := newRunner(database), newRunner(second)

	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding refs: %v", err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(ctx, `lock table refs in exclusive mode`); err != nil {
		t.Fatalf("hold refs: %v", err)
	}

	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		first.RunReferenceExcerptReconciliation(firstCtx, firstLeases)
	}()
	if waiting := storetest.WaitForLockWaiters(t, gate, 1, 30*time.Second); waiting < 1 {
		t.Fatal("the first runner's tick never reached the refs lock: it never took the lease")
	}

	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		other.RunReferenceExcerptReconciliation(secondCtx, secondLeases)
	}()
	for deadline := time.Now().Add(30 * time.Second); countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", referenceExcerptLease) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("the second runner never skipped the %s lease:\n%s", referenceExcerptLease, logs.Lines("lease="))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Both runners stop while the lock still holds the first one's tick: whatever each did, it
	// did before the gate let go, so the counts below are the two loops' one round. The first
	// runner's tick is therefore one its own shutdown cut short, which since INFRA-66's third
	// round reports itself as that rather than as a pass that ran - it serves no period, since
	// it did not finish its work. What this row is about is unchanged and is counted below: one
	// of the two loops got into the tick and the other found the lease held.
	stopSecond()
	<-secondDone
	stopFirst()
	<-firstDone
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release refs: %v", err)
	}

	entered := countExcerptLeaseLines(logs, "dispatch duty: leased pass ran", referenceExcerptLease) +
		countExcerptLeaseLines(logs, "dispatch duty: leased pass cut short by shutdown; its period is left unserved", referenceExcerptLease)
	if entered != 1 {
		t.Fatalf("the two runners got into the tick %d times; want once:\n%s", entered, logs.Lines("lease="))
	}
	if skipped := countExcerptLeaseLines(logs, "dispatch duty: leased pass skipped", referenceExcerptLease); skipped == 0 {
		t.Fatalf("neither runner skipped:\n%s", logs.Lines("lease="))
	}
}
