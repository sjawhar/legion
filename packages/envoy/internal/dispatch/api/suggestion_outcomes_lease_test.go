package api

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// sweepLeaseTick is the interval both runners tick on: short, so each reaches its first pass at
// once, and the test is about which of the two runs it rather than about waiting.
const sweepLeaseTick = 20 * time.Millisecond

// sweepLeases is the pool a sweep takes its lease on, the one cmd/dispatch gives it.
func sweepLeases(t *testing.T, database *store.Store) *pgxpool.Pool {
	t.Helper()
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	return leases
}

// countLeaseLines counts the lease lines carrying message for lease.
func countLeaseLines(logs *storetest.LockedLog, message, lease string) int {
	count := 0
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, `msg="`+message+`"`) && strings.Contains(line, "lease="+lease) {
			count++
		}
	}
	return count
}

// Two envoy-dispatch processes sweep one database, each through RunSuggestionOutcomeSweep itself
// and each on a pool of its own (storetest.SecondProcess, as a second process has): one takes the
// suggestion-outcomes lease and runs the pass, the other finds it held and skips. The first pass
// is held inside its own first statement by a lock this test takes on write_suggestions, which is
// the moment the lease is about - both processes' tickers firing while one is still sweeping -
// and both runners are stopped before that lock is released, so the pass runs exactly once.
func TestTwoSweepRunnersRunThePassOnceAndSkipOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	firstLeases, secondLeases := sweepLeases(t, database), sweepLeases(t, second)
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding write_suggestions: %v", err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(ctx, `lock table write_suggestions in exclusive mode`); err != nil {
		t.Fatalf("hold write_suggestions: %v", err)
	}

	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		RunSuggestionOutcomeSweep(firstCtx, database.Pool, sweepLeaseTick, firstLeases, nil)
	}()
	if waiting := storetest.WaitForLockWaiters(t, gate, 1, 30*time.Second); waiting < 1 {
		t.Fatal("the first runner's pass never reached the write_suggestions lock: it never took the lease")
	}

	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		RunSuggestionOutcomeSweep(secondCtx, second.Pool, sweepLeaseTick, secondLeases, nil)
	}()
	for deadline := time.Now().Add(30 * time.Second); countLeaseLines(logs, "dispatch duty: leased pass skipped", suggestionOutcomeLease) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("the second runner never skipped the %s lease:\n%s", suggestionOutcomeLease, logs.Lines("lease="))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Both runners stop while the lock still holds the first one's pass: whatever each did, it
	// did before the gate let go, so the counts below are the two tickers' one round. The first
	// runner's pass is therefore one its own shutdown cut short, which since INFRA-66's third
	// round reports itself as that rather than as a pass that ran - it serves no period, since
	// it did not finish its work. What this row is about is unchanged and is counted below: one
	// of the two tickers got into the pass and the other found the lease held.
	stopSecond()
	<-secondDone
	stopFirst()
	<-firstDone
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release write_suggestions: %v", err)
	}

	entered := countLeaseLines(logs, "dispatch duty: leased pass ran", suggestionOutcomeLease) +
		countLeaseLines(logs, "dispatch duty: leased pass cut short by shutdown; its period is left unserved", suggestionOutcomeLease)
	if entered != 1 {
		t.Fatalf("the two runners got into the pass %d times; want once:\n%s", entered, logs.Lines("lease="))
	}
	if skipped := countLeaseLines(logs, "dispatch duty: leased pass skipped", suggestionOutcomeLease); skipped == 0 {
		t.Fatalf("neither runner skipped:\n%s", logs.Lines("lease="))
	}
}

// One runner, nothing else on the database: every tick must make a pass. The lease is free at
// each of them and no other process has served the period, so a skip here is an interval of this
// loop's work that no pass of any process did - the loop running slower than it is configured to,
// with one process, which is what this deployment runs today.
//
// It is the loop's own shape that is at stake. Every leased loop ticks on the interval it passes
// as its period (RunSuggestionOutcomeSweep, redeliver.Sweeper.Run, duty.RunJittered's callers),
// so the period and the gap between two ticks are the same span, and the record of a pass is
// stamped a moment after the tick that made it - the time it takes to take the lease. A tick
// whose own moment is shorter than the previous one's then falls inside the period that pass
// recorded and is dropped. Ten ticks are driven because one skip is what this is about and a
// shorter window could miss it.
func TestALoneRunnerMakesAPassOnEveryTick(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	database := storetest.Open(t)
	leases := sweepLeases(t, database)
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const tick = time.Second
	const ticks = 10
	done := make(chan struct{})
	go func() { defer close(done); RunSuggestionOutcomeSweep(ctx, database.Pool, tick, leases, nil) }()
	time.Sleep(tick*ticks + tick/2)
	stop()
	<-done

	ran := countLeaseLines(logs, "dispatch duty: leased pass ran", suggestionOutcomeLease)
	skipped := countLeaseLines(logs, "dispatch duty: leased pass skipped", suggestionOutcomeLease)
	t.Logf("one runner, %d ticks of %v: ran %d, skipped %d", ticks, tick, ran, skipped)
	if skipped != 0 {
		t.Errorf("a lone runner skipped %d of its %d ticks; no other process swept, so each of those intervals went unswept:\n%s",
			skipped, ran+skipped, logs.Lines("lease="))
	}
}
