package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// checkWakeRows is issue's check_wake rows, oldest first, with the wake each carries.
func checkWakeRows(t *testing.T, pool *pgxpool.Pool, issue string) []outboxRow {
	t.Helper()
	return issueOutboxOf(t, pool, issue, "check_wake")
}

// decodeWake is the wake a check_wake row carries.
func decodeWake(t *testing.T, row outboxRow) record.CheckWakeDue {
	t.Helper()
	var due record.CheckWakeDue
	if err := json.Unmarshal(row.payload, &due); err != nil {
		t.Fatalf("decode check_wake row %d: %v", row.id, err)
	}
	return due
}

// onlyWake is the one check_wake row issue has, failing the test where it has another count.
func onlyWake(t *testing.T, pool *pgxpool.Pool, issue string) outboxRow {
	t.Helper()
	rows := checkWakeRows(t, pool, issue)
	if len(rows) != 1 {
		t.Fatalf("check_wake rows of %s = %d, want one", issue, len(rows))
	}
	return rows[0]
}

// The checking agent names its own next look: a pending report carries the time, the check
// records it, and one check_wake row is due at that moment and at no earlier one — the only other
// row that could start a look is the check's own bound, two days out. The look ends as every
// pending does: the tester suspended with its session kept and the issue's pod released
// (issue_suspend). When the row's fact arrives the next look starts, woken by what the report
// said. A wake three hours out and one an hour out each start their look at their own time.
func TestAPendingsNamedWakeStartsTheNextLookAtThatTimeAndNoEarlier(t *testing.T) {
	for _, ahead := range []time.Duration{3 * time.Hour, time.Hour} {
		t.Run(ahead.String(), func(t *testing.T) {
			rig := childCheckRig(t)
			wake := checkOpened.Add(ahead)
			rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending",
				Summary: "the deploy has not carried the merge", WakeAt: &wake})

			check := productionCheck(t, rig.pool, "LEGION-209")
			if check == nil || !check.Open() || check.LookOpen || check.Looks != 1 || check.WakeAt == nil || !check.WakeAt.Equal(wake) {
				t.Fatalf("the check after the pending = %+v, want it open at look 1, the look closed, woken at %s", check, wake)
			}
			row := onlyWake(t, rig.pool, "LEGION-209")
			if !row.nextAt.Equal(wake) {
				t.Fatalf("the check_wake row is due %s, want the %s the report named", row.nextAt, wake)
			}
			due := decodeWake(t, row)
			if due.Look != 1 || !due.OpenedAt.Equal(checkOpened) || due.Reason != "the deploy has not carried the merge" {
				t.Fatalf("the check_wake row = %+v, want look 1 of the check opened at the merge, with the report's summary as its reason", due)
			}
			bounds := issueOutboxOf(t, rig.pool, "LEGION-209", "check_bound")
			if len(bounds) != 1 || bounds[0].nextAt.Before(wake) {
				t.Fatalf("check_bound rows = %+v, want the check's own bound, due after the named wake: nothing starts a look before it", bounds)
			}
			suspends := superviseRowsOf(t, rig.pool, "LEGION-209", "suspend")
			if len(suspends) != 1 || suspends[0].Role != claim.RoleTester {
				t.Fatalf("suspend rows = %+v, want the tester's alone, its session kept", suspends)
			}
			if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 1 {
				t.Fatalf("issue suspensions = %+v, want the one that releases the issue's pod", got)
			}

			rig.clock.now = wake
			rig.apply("wake", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Reason: "the deploy has not carried the merge"})
			check = productionCheck(t, rig.pool, "LEGION-209")
			if check.Looks != 2 || !check.LookOpen || check.Last || check.WakeAt != nil ||
				check.LastWakeKind != wakeNamed || check.LastWakeDetail != "the deploy has not carried the merge" || !check.LastWakeAt.Equal(wake) {
				t.Fatalf("the check at its named wake = %+v, want look 2 open at %s, woken by the report's own wake, with no wake standing", check, wake)
			}
			starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
			if len(starts) != 2 || starts[1].Role != claim.RoleTester || starts[1].CheckLook != 2 || !starts[1].CheckOpenedAt.Equal(checkOpened) {
				t.Fatalf("start rows = %+v, want look 2 handed to the tester the check records", starts)
			}
			if !strings.Contains(starts[1].Task, "the deploy has not carried the merge") {
				t.Fatalf("the look's task = %q, want the reason the wake carried", starts[1].Task)
			}
		})
	}
}

// A wake at or past the check's own bound names no row at all and leaves the record with none:
// the bound row already stands and starts the check's last look, and a wake row beside it would
// start an ordinary look after the last one.
func TestAWakeAtOrPastTheBoundNamesNoRowAndTheBoundStartsTheLastLook(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{name: "at the bound", at: checkOpened.Add(checkBound)},
		{name: "past the bound", at: checkOpened.Add(checkBound + time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			wake := tc.at
			rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "the deploy is days out", WakeAt: &wake})

			check := productionCheck(t, rig.pool, "LEGION-209")
			if check.WakeAt != nil {
				t.Fatalf("the check's wake = %s, want none: the wake falls at or past its bound", check.WakeAt)
			}
			if rows := checkWakeRows(t, rig.pool, "LEGION-209"); len(rows) != 0 {
				t.Fatalf("check_wake rows = %+v, want none", rows)
			}

			rig.clock.now = checkOpened.Add(checkBound)
			rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
			check = productionCheck(t, rig.pool, "LEGION-209")
			if check.Looks != 2 || !check.LookOpen || !check.Last || check.LastWakeKind != wakeBound {
				t.Fatalf("the check at its bound = %+v, want its last look 2 open, woken by the bound", check)
			}
		})
	}
}

// A wake under a minute out is moved to the floor: a look costs a pod launch and an agent turn,
// so a report naming its own moment, or one already past, starts its next look a minute after the
// report and no sooner.
func TestAWakeUnderAMinuteOutStartsTheNextLookAMinuteAfterTheReport(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{name: "ten seconds ahead", at: checkOpened.Add(10 * time.Second)},
		{name: "already past", at: checkOpened.Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			wake := tc.at
			rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "the deploy is running now", WakeAt: &wake})

			floor := checkOpened.Add(CheckWakeFloor)
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check.WakeAt == nil || !check.WakeAt.Equal(floor) {
				t.Fatalf("the check's wake = %v, want the floor at %s", check.WakeAt, floor)
			}
			if row := onlyWake(t, rig.pool, "LEGION-209"); !row.nextAt.Equal(floor) {
				t.Fatalf("the check_wake row is due %s, want the floor at %s", row.nextAt, floor)
			}

			rig.clock.now = floor
			rig.apply("wake", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Reason: "the deploy is running now"})
			if check := productionCheck(t, rig.pool, "LEGION-209"); check.Looks != 2 || !check.LookOpen {
				t.Fatalf("the check at the floor = %+v, want look 2 open a minute after the report", check)
			}
		})
	}
}

// A look nobody reported — its bound came due, the outbox met an operator's hold on its start, or
// the checking role's claim failed in it — names the next look a look bound out, with the retry
// as its reason, and that row starts it. Nothing else would: no boot and no deploy event reads
// production.
func TestAnUnreportedLookNamesTheNextLookALookBoundOutAndItStarts(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  intake.Fact
	}{
		{name: "its bound", end: intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound", OpenedAt: checkOpened}},
		{name: "an operator's hold", end: intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "operator hold", OpenedAt: checkOpened}},
		{name: "a failed claim", end: intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleTester}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			rig.clock.now = checkOpened.Add(30 * time.Minute)
			rig.apply("unreported", tc.end)

			retry := rig.clock.now.Add(CheckLookBound)
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check == nil || !check.Open() || check.LookOpen || check.WakeAt == nil || !check.WakeAt.Equal(retry) {
				t.Fatalf("the check after the look nobody reported = %+v, want it open with its wake a look bound out, at %s", check, retry)
			}
			row := onlyWake(t, rig.pool, "LEGION-209")
			if !row.nextAt.Equal(retry) {
				t.Fatalf("the check_wake row is due %s, want the retry at %s", row.nextAt, retry)
			}
			due := decodeWake(t, row)
			if due.Look != 1 || !due.OpenedAt.Equal(checkOpened) || !strings.HasPrefix(due.Reason, "the retry of a look nobody reported") {
				t.Fatalf("the check_wake row = %+v, want look 1's retry as its reason", due)
			}

			rig.clock.now = retry
			rig.apply("wake", intake.CheckWakeDue{Issue: "LEGION-209", Look: due.Look, OpenedAt: due.OpenedAt, Reason: due.Reason})
			check = productionCheck(t, rig.pool, "LEGION-209")
			if check.Looks != 2 || !check.LookOpen || check.WakeAt != nil {
				t.Fatalf("the check at its retry = %+v, want look 2 open with no wake standing", check)
			}
			starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
			if len(starts) != 2 || starts[1].CheckLook != 2 || starts[1].Role != claim.RoleTester {
				t.Fatalf("start rows = %+v, want the retry's look 2 handed to the tester", starts)
			}
		})
	}
}

// The same three ends on the check's last look close the check as not run, as a pending of that
// look does: no look follows the bound's, so no wake is named and no row is left behind.
func TestAnUnreportedLastLookClosesTheCheckAsNotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  intake.Fact
	}{
		{name: "its bound", end: intake.CheckLookEnded{Issue: "LEGION-209", Look: 2, Cause: "look bound", OpenedAt: checkOpened}},
		{name: "an operator's hold", end: intake.CheckLookEnded{Issue: "LEGION-209", Look: 2, Cause: "operator hold", OpenedAt: checkOpened}},
		{name: "a failed claim", end: intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleTester}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			wake := checkOpened.Add(time.Hour)
			rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "the deploy has not carried the merge", WakeAt: &wake})
			rig.clock.now = checkOpened.Add(checkBound)
			rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
			if check := productionCheck(t, rig.pool, "LEGION-209"); !check.Last || !check.LookOpen || check.Looks != 2 {
				t.Fatalf("the check at its bound = %+v, want its last look 2 open", check)
			}
			before := len(checkWakeRows(t, rig.pool, "LEGION-209"))

			rig.apply("unreported", tc.end)
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check.Open() || check.Outcome != record.CheckNotRun || check.LookOpen || check.WakeAt != nil {
				t.Fatalf("the check after its last look's %s = %+v, want it closed as not run with no wake", tc.name, check)
			}
			if got := len(checkWakeRows(t, rig.pool, "LEGION-209")); got != before {
				t.Fatalf("check_wake rows = %d, want the %d the last look started with: no look follows it", got, before)
			}
			if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/tester"] != 1 {
				t.Fatalf("issue_close rows = %v, want the tester's once at the close", got)
			}
		})
	}
}

// A pending on an issue back in the workflow — a root reopened inside its linger, whose
// implementer works the new run — names the wake and suspends nobody: the claims and the pod are
// the run's. The row, finding the issue in a phase when it comes due, starts no look and is
// queued again a look bound later rather than dropped, so a check whose issue re-entered the
// workflow and closed again on the same merge still holds its agent's wake.
func TestAPendingOnAnIssueBackInTheWorkflowNamesItsWakeAndSuspendsNobody(t *testing.T) {
	rig := rootCheckRig(t)
	if _, err := rig.pool.Exec(context.Background(), "update issues set phase = 'admitted', status = 'todo', linger_until = null where key = 'LEGION-208'"); err != nil {
		t.Fatal(err)
	}
	suspends := len(superviseRowsOf(t, rig.pool, "LEGION-208", "suspend"))
	suspensions := len(issueSuspensions(t, rig.pool, "LEGION-208"))

	wake := checkOpened.Add(3 * time.Hour)
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-208", Look: 1, Verdict: "pending", Summary: "the deploy has not carried the merge", WakeAt: &wake})
	check := productionCheck(t, rig.pool, "LEGION-208")
	if check == nil || !check.Open() || check.LookOpen || check.WakeAt == nil || !check.WakeAt.Equal(wake) {
		t.Fatalf("the check after the pending = %+v, want it open, the look closed, woken at %s", check, wake)
	}
	if row := onlyWake(t, rig.pool, "LEGION-208"); !row.nextAt.Equal(wake) {
		t.Fatalf("the check_wake row is due %s, want %s", row.nextAt, wake)
	}
	if got := len(superviseRowsOf(t, rig.pool, "LEGION-208", "suspend")); got != suspends {
		t.Fatalf("suspend rows = %d, want %d: the pending suspends nothing of a run", got, suspends)
	}
	if got := len(issueSuspensions(t, rig.pool, "LEGION-208")); got != suspensions {
		t.Fatalf("issue suspensions = %d, want %d: the pod is the run's", got, suspensions)
	}

	rig.clock.now = wake
	starts := len(superviseRowsOf(t, rig.pool, "LEGION-208", "start"))
	rig.apply("wake", intake.CheckWakeDue{Issue: "LEGION-208", Look: 1, OpenedAt: checkOpened, Reason: "the deploy has not carried the merge"})
	check = productionCheck(t, rig.pool, "LEGION-208")
	deferred := wake.Add(CheckLookBound)
	if check.Looks != 1 || check.LookOpen || check.WakeAt == nil || !check.WakeAt.Equal(deferred) {
		t.Fatalf("the check at its wake while the root runs again = %+v, want look 1 still ended and the wake at %s", check, deferred)
	}
	rows := checkWakeRows(t, rig.pool, "LEGION-208")
	if len(rows) != 2 || !rows[1].nextAt.Equal(deferred) {
		t.Fatalf("check_wake rows = %+v, want the wake queued again at %s", rows, deferred)
	}
	if due := decodeWake(t, rows[1]); due.Look != 1 || !due.OpenedAt.Equal(checkOpened) || due.Reason != "the deploy has not carried the merge" {
		t.Fatalf("the queued row = %+v, want the agent's own wake kept", due)
	}
	if got := len(superviseRowsOf(t, rig.pool, "LEGION-208", "start")); got != starts {
		t.Fatalf("start rows = %d, want %d: no look runs while the issue is in a phase", got, starts)
	}
}

// A wake row with no look to start changes nothing and is not queued again: a check closed since,
// a check a later merge of the same issue replaced (its look 1 is not this row's), a row of a look
// the check has left, and a row whose look is already open. Without these the queue a running
// issue's wake takes would make an orphan row immortal.
func TestAWakeRowWithNoLookToStartStartsNoneAndIsNotQueuedAgain(t *testing.T) {
	t.Run("a closed check", func(t *testing.T) {
		rig := childCheckRig(t)
		rig.apply("pass", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pass", Summary: "driven in production"})
		assertWakeStartsNothing(t, rig, "LEGION-209", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Reason: "the agent's own wake"})
	})

	t.Run("a check a later merge replaced", func(t *testing.T) {
		rig := newCheckRig(t)
		seedMergingChild(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
		first := checkOpened.Add(-24 * time.Hour)
		closed := first.Add(2 * time.Hour)
		seedRecord(t, rig.pool, func(tx pgx.Tx) error {
			return record.NewStore().PutProductionCheck(t.Context(), tx, record.ProductionCheck{
				Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion", Number: 41, MergeSHA: "merge-41",
				OpenedAt: first, BoundAt: first.Add(checkBound), Role: claim.RoleTester, Looks: 1, LookWakeAt: &first,
				LastWakeKind: wakeMerge, LastWakeDetail: "merge-41", LastWakeAt: &first, ClosedAt: &closed, Outcome: record.CheckNotRun,
			})
		})
		rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
		rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "the deploy has not carried the merge",
			WakeAt: new(checkOpened.Add(time.Hour))})
		assertWakeStartsNothing(t, rig, "LEGION-209", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: first, Reason: "the first check's wake"})
	})

	t.Run("a look the check has left", func(t *testing.T) {
		rig := childCheckRig(t)
		rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "the deploy has not carried the merge",
			WakeAt: new(checkOpened.Add(time.Hour))})
		rig.clock.now = checkOpened.Add(checkBound)
		rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
		if check := productionCheck(t, rig.pool, "LEGION-209"); check.Looks != 2 || !check.LookOpen {
			t.Fatalf("the check at its bound = %+v, want look 2 open", check)
		}
		// Look 1's own wake row, left over when the bound started look 2.
		assertWakeStartsNothing(t, rig, "LEGION-209", intake.CheckWakeDue{Issue: "LEGION-209", Look: 1, OpenedAt: checkOpened, Reason: "the agent's own wake"})
		// And a row of the look the check stands at, which is already open.
		assertWakeStartsNothing(t, rig, "LEGION-209", intake.CheckWakeDue{Issue: "LEGION-209", Look: 2, OpenedAt: checkOpened, Reason: "the agent's own wake"})
	})
}

// assertWakeStartsNothing applies due to issue and holds the record to what it was: no look
// started, and no wake row queued in its place.
func assertWakeStartsNothing(t *testing.T, rig checkRig, issue string, due intake.CheckWakeDue) {
	t.Helper()
	before := productionCheck(t, rig.pool, issue)
	starts := len(superviseRowsOf(t, rig.pool, issue, "start"))
	rows := len(checkWakeRows(t, rig.pool, issue))

	rig.apply("wake-"+issue+"-"+due.Reason+"-"+time.Now().Format(time.RFC3339Nano), due)
	after := productionCheck(t, rig.pool, issue)
	if after.Looks != before.Looks || after.LookOpen != before.LookOpen || after.Open() != before.Open() {
		t.Fatalf("the check after a wake with no look to start = %+v, want it as it was: %+v", after, before)
	}
	if got := len(superviseRowsOf(t, rig.pool, issue, "start")); got != starts {
		t.Fatalf("start rows = %d, want %d: the row starts no look", got, starts)
	}
	if got := len(checkWakeRows(t, rig.pool, issue)); got != rows {
		t.Fatalf("check_wake rows = %d, want %d: a spent row is not queued again", got, rows)
	}
}
