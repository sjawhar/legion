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

// sweepLeaseSchedule is the pace both runners keep: a short gap, so a held lease is retried at
// once and the test is about which runner passes when rather than about waiting.
var sweepLeaseSchedule = suggestionSchedule{minGap: 50 * time.Millisecond, page: suggestionOutcomePage}

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

// captureSweepLogs sends the default logger's lines to the returned log until the test ends.
func captureSweepLogs(t *testing.T) *storetest.LockedLog {
	t.Helper()
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// runSweep runs run on a goroutine until the test ends, and fails the test if it does not return
// promptly once its context ends.
func runSweep(t *testing.T, run func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the suggestion outcome sweep did not return within 10s of its context ending")
		}
	})
}

// waitUntil polls condition until it holds, failing the test after within.
func waitUntil(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two envoy-dispatch processes sweep one database, each through the sweep's woken loop and each on
// a pool of its own (storetest.SecondProcess, as a second process has): one takes the
// suggestion-outcomes lease and runs its pass, and the other finds it held and does not run the
// same pass beside it. The first pass is held inside its own first statement by a lock this test
// takes on the cursor it reads for update, which is the moment the lease is about - both
// processes' loops woken at once. The second keeps trying, and once the first pass is done it runs
// a pass of its own: the first may have read before whatever woke the second, so a woken loop
// never gives its pass up to another process's.
func TestTwoSuggestionSweepRunnersNeverPassAtOnceAndBothPass(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	firstLeases, secondLeases := sweepLeases(t, database), sweepLeases(t, second)
	logs := captureSweepLogs(t)

	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding the cursor: %v", err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(ctx, `lock table suggestion_outcome_cursor in exclusive mode`); err != nil {
		t.Fatalf("hold the cursor: %v", err)
	}

	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, database.Pool, firstLeases, nil, sweepLeaseSchedule)
	})
	if waiting := storetest.WaitForLockWaiters(t, gate, 1, 30*time.Second); waiting < 1 {
		t.Fatal("the first runner's pass never reached the cursor lock: it never took the lease")
	}
	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, second.Pool, secondLeases, nil, sweepLeaseSchedule)
	})
	// The second runner's boot attempt and at least one retry find the lease held.
	waitUntil(t, 30*time.Second, "the second runner to find the lease held twice", func() bool {
		return countLeaseLines(logs, "dispatch duty: leased pass skipped", suggestionOutcomeLease) >= 2
	})
	if ran := countLeaseLines(logs, "dispatch duty: leased pass ran", suggestionOutcomeLease); ran != 0 {
		t.Fatalf("%d passes ran while the first one was held inside its pass; want none:\n%s", ran, logs.Lines("lease="))
	}

	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the cursor: %v", err)
	}
	waitUntil(t, 30*time.Second, "both runners to run a pass of their own", func() bool {
		return countLeaseLines(logs, "dispatch duty: leased pass ran", suggestionOutcomeLease) == 2
	})
	var cursor *int64
	if err := database.Pool.QueryRow(ctx, `select event_id from suggestion_outcome_cursor`).Scan(&cursor); err != nil {
		t.Fatalf("read the cursor: %v", err)
	}
	if cursor == nil {
		t.Fatal("the cursor is still unset after both passes")
	}
}

// One runner, nothing else on the database: every wake must earn a pass, each one gap after the
// one before at the soonest. The lease is free at each of them, so a skip here would be a change
// no pass of any process read - the loop doing less than it is woken for, with one process,
// which is what this deployment runs today.
func TestALoneSuggestionSweepRunnerPassesOnEveryWake(t *testing.T) {
	database := storetest.Open(t)
	leases := sweepLeases(t, database)
	logs := captureSweepLogs(t)
	wake := make(chan struct{}, 1)
	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, database.Pool, leases, wake, sweepLeaseSchedule)
	})

	const wakes = 5
	for woken := 0; woken <= wakes; woken++ {
		waitUntil(t, 10*time.Second, "the pass the last wake earned", func() bool {
			return countLeaseLines(logs, "dispatch duty: leased pass ran", suggestionOutcomeLease) == woken+1
		})
		if woken < wakes {
			wake <- struct{}{}
		}
	}
	if skipped := countLeaseLines(logs, "dispatch duty: leased pass skipped", suggestionOutcomeLease); skipped != 0 {
		t.Fatalf("a lone runner skipped %d passes:\n%s", skipped, logs.Lines("lease="))
	}
	time.Sleep(4 * sweepLeaseSchedule.minGap)
	if ran := countLeaseLines(logs, "dispatch duty: leased pass ran", suggestionOutcomeLease); ran != wakes+1 {
		t.Fatalf("a lone runner ran %d passes for its start and %d wakes; want %d:\n%s", ran, wakes, wakes+1, logs.Lines("lease="))
	}
}
