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
		RunSuggestionOutcomeSweep(firstCtx, database.Pool, sweepLeaseTick, firstLeases)
	}()
	if waiting := storetest.WaitForLockWaiters(t, gate, 1, 30*time.Second); waiting < 1 {
		t.Fatal("the first runner's pass never reached the write_suggestions lock: it never took the lease")
	}

	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		RunSuggestionOutcomeSweep(secondCtx, second.Pool, sweepLeaseTick, secondLeases)
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
