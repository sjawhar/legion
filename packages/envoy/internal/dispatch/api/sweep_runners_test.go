package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// sweepRunnerSchedule is the pace a sweep's runners keep here: a short gap, so a pass that is
// owed runs at once and a test is about which runner judges what rather than about waiting.
var sweepRunnerSchedule = suggestionSchedule{minGap: 50 * time.Millisecond, page: suggestionOutcomePage}

// The statements a pass of the suggestion sweep is recognised by, each a substring of one of its
// queries: the cursor it holds for update (one per pass), the first pass's read of where to start
// (startSuggestionCursor), a later pass's page (readSuggestionPage) and the three rules, which
// run only over a page that holds something (judgeSuggestionPage).
const (
	cursorHeld    = "select event_id from suggestion_outcome_cursor for update"
	firstPassRead = "select coalesce((select max(id) from events where id <= $2::bigint), 0)"
	laterPageRead = "where id > $1 and id <= $4::bigint order by id limit $2"
	judgingRule   = "update write_suggestions ws"
)

// sweepLeases is the pool a leased loop takes its lease on, the one cmd/dispatch gives it.
func sweepLeases(t *testing.T, database *store.Store) *store.RawPool {
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

// countStatements counts the recorded statements holding needle.
func countStatements(recorded []string, needle string) int {
	count := 0
	for _, sql := range recorded {
		if strings.Contains(sql, needle) {
			count++
		}
	}
	return count
}

// recordedPool is a process's pool with every statement the code under test makes recorded, which
// is how a sweep's passes are counted now that they take no lease to log.
func recordedPool(t *testing.T, database *store.Store) (*store.Pool, *storetest.StatementRecorder) {
	t.Helper()
	recorder := &storetest.StatementRecorder{}
	return storetest.Traced(t, database.Pool, recorder, nil), recorder
}

// sweepHorizon is one runner's events.Horizon, one per process as a server keeps one
// (api.Background's EventHorizon), observing through that process's own pool.
func sweepHorizon(pool *store.Pool) *events.Horizon {
	return events.NewHorizon(pool)
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

// Two envoy-dispatch processes sweep one database, each through the sweep's woken loop and each
// on a pool of its own (storetest.SecondProcess, as a second process has). The loop takes no
// lease: what keeps the two from judging one page twice is the suggestion_outcome_cursor row each
// page holds `for update` (judgeSuggestionPage). So both boot passes reach that row - a lease
// would have left one of them waiting outside it - and this test holds the row with a lock of its
// own to see both arrive. Once it lets go, one pass judges the page and advances the cursor, and
// the other, which was waiting on the row, reads the cursor that pass advanced and finds its page
// empty: one judging pass across the two, and both make progress rather than one giving its pass
// up to the other's.
func TestTwoSuggestionSweepRunnersNeverJudgeOnePageTwiceAndBothPass(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	firstPool, firstStatements := recordedPool(t, database)
	secondPool, secondStatements := recordedPool(t, second)
	recorded := func() []string {
		return append(firstStatements.Statements(), secondStatements.Statements()...)
	}

	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction holding the cursor: %v", err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(ctx, `lock table suggestion_outcome_cursor in exclusive mode`); err != nil {
		t.Fatalf("hold the cursor: %v", err)
	}

	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, firstPool, sweepHorizon(firstPool), nil, sweepRunnerSchedule)
	})
	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, secondPool, sweepHorizon(secondPool), nil, sweepRunnerSchedule)
	})
	if waiting := storetest.WaitForLockWaiters(t, gate, 2, 30*time.Second); waiting < 2 {
		t.Fatalf("%d of the two runners' passes reached the cursor lock, want both: the cursor row, not a lease, is what serialises them", waiting)
	}
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatalf("release the cursor: %v", err)
	}

	waitUntil(t, 30*time.Second, "one runner to judge the page and the other to read the cursor it advanced", func() bool {
		return countStatements(recorded(), firstPassRead) == 1 && countStatements(recorded(), laterPageRead) >= 1
	})
	var cursor *int64
	if err := database.Pool.QueryRow(ctx, `select event_id from suggestion_outcome_cursor`).Scan(&cursor); err != nil {
		t.Fatalf("read the cursor: %v", err)
	}
	if cursor == nil {
		t.Fatal("the cursor is still unset after both passes")
	}
	// Each runner held the cursor row for a pass of its own, and only one of the two judged: the
	// rules ran once over the page, not once per process.
	for runner, statements := range map[string]*storetest.StatementRecorder{"the first": firstStatements, "the second": secondStatements} {
		if held := countStatements(statements.Statements(), cursorHeld); held == 0 {
			t.Errorf("%s runner never held the cursor row: it made no pass of its own", runner)
		}
	}
	if judged := countStatements(recorded(), judgingRule); judged != len(suggestionRules(everySuggestion)) {
		t.Fatalf("the rules ran %d times across the two runners, want the %d of one judging pass: the page was judged twice",
			judged, len(suggestionRules(everySuggestion)))
	}
}

// One runner, nothing else on the database: every wake must earn a pass, each one gap after the
// one before at the soonest, and no wake earns more than one. A pass is counted by the cursor row
// it holds, one read per pass, since the loop takes no lease and logs nothing of its own.
func TestALoneSuggestionSweepRunnerPassesOnEveryWake(t *testing.T) {
	database := storetest.Open(t)
	pool, statements := recordedPool(t, database)
	wake := make(chan struct{}, 1)
	runSweep(t, func(ctx context.Context) {
		runSuggestionOutcomeSweep(ctx, pool, sweepHorizon(pool), wake, sweepRunnerSchedule)
	})
	passes := func() int { return countStatements(statements.Statements(), cursorHeld) }

	const wakes = 5
	for woken := 0; woken <= wakes; woken++ {
		waitUntil(t, 10*time.Second, "the pass the last wake earned", func() bool { return passes() == woken+1 })
		if woken < wakes {
			wake <- struct{}{}
		}
	}
	time.Sleep(4 * sweepRunnerSchedule.minGap)
	if ran := passes(); ran != wakes+1 {
		t.Fatalf("a lone runner made %d passes for its start and %d wakes; want %d", ran, wakes, wakes+1)
	}
}
