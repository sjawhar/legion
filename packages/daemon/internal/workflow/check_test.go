package workflow

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// checkOpened is when every check rig's merge lands, and checkBound the rig's production check
// bound, short enough to drive in a test.
var checkOpened = time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)

const checkBound = 48 * time.Hour

// checkClock is the engine's clock in the check tests, moved by hand where a time matters.
type checkClock struct{ now time.Time }

func (c *checkClock) Now() time.Time { return c.now }

// checkRig is a tree whose issue's production check the test drives: the pool, the engine on a
// clock the test moves, the journal it writes, and apply, which applies a fact and fails the test
// on an error.
type checkRig struct {
	pool   *pgxpool.Pool
	engine *Engine
	clock  *checkClock
	logged *bytes.Buffer
	apply  func(id string, fact intake.Fact) intake.Result
}

func newCheckRig(t *testing.T) checkRig {
	t.Helper()
	pool := migratedPool(t)
	clock := &checkClock{now: checkOpened}
	logged := &bytes.Buffer{}
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckBound: checkBound, ProductionCheckRole: claim.RoleTester, Clock: clock.Now},
		slog.New(slog.NewTextHandler(logged, nil)))
	return checkRig{pool: pool, engine: engine, clock: clock, logged: logged, apply: applyFacts(t, pool, engine)}
}

// childCheckRig is a running root (LEGION-208, implementing) whose child LEGION-209 merged
// sjawhar/legion#43 at merge-43: the child is done with its check open at look 1 in the
// tester's live session.
func childCheckRig(t *testing.T) checkRig {
	t.Helper()
	rig := newCheckRig(t)
	seedMergingChild(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
	return rig
}

// rootCheckRig is a root LEGION-208 whose sjawhar/legion#42 merged at merge while it awaited it,
// and which a person then closed on Dispatch, with a child LEGION-209 left implementing: the tree
// lingers an hour and the root's check is open at look 1. The merge alone no longer produces that
// state — a merged root's close waits for the rest of its tree (closeMerged) — and a person's
// close of a root still lingers the tree whatever its children are doing, which is the path these
// tests are about.
func rootCheckRig(t *testing.T) checkRig {
	t.Helper()
	rig := newCheckRig(t)
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.AwaitingMerge, Generation: 7, Status: "retro", Rank: "U", LastDispatchSeq: 1})
	parent := "LEGION-208"
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Implementing, Generation: 3, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"})
	rig.apply("root-closed", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
	return rig
}

// A report on the root's check while its tree lingers is recorded: linger holds the members of a
// closed tree, and the check after the close is the one thing a member still does there.
func TestACheckReportWhileTheRootLingersIsRecorded(t *testing.T) {
	rig := rootCheckRig(t)
	if result := rig.apply("pass", intake.CheckReport{Issue: "LEGION-208", Claim: "tester", Look: 1, Verdict: "pass", Summary: "driven in production"}); result.Refusal != nil {
		t.Fatalf("the report = %+v, want it recorded", result.Refusal)
	}
	if check := productionCheck(t, rig.pool, "LEGION-208"); check == nil || check.Open() || check.Outcome != record.CheckRecorded || check.LookOpen {
		t.Fatalf("the root's check = %+v, want it closed as recorded", check)
	}
}

// A pending report ends the look with nothing to show and takes the pod: the tester's claim
// is suspended, session kept, and the issue suspended (issue_suspend), whose Sandbox goes with its
// pod and disk, both rows written after the look's
// start row so the suspension's fences find no later start. The check stays open at look 1 with
// its look closed.
func TestAPendingReportSuspendsTheTesterAfterTheLooksStart(t *testing.T) {
	rig := childCheckRig(t)
	starts := issueOutboxOf(t, rig.pool, "LEGION-209", "supervise")
	start := starts[len(starts)-1]
	if decodeSupervise(t, start).CheckLook != 1 {
		t.Fatalf("the newest supervise row before the report = %s, want the check's look 1 start", start.payload)
	}
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "not deployed yet"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check == nil || !check.Open() || check.Looks != 1 || check.LookOpen {
		t.Fatalf("the check after pending = %+v, want it open at look 1 with the look closed", check)
	}
	suspends := superviseRowsOf(t, rig.pool, "LEGION-209", "suspend")
	if len(suspends) != 1 || suspends[0].Role != claim.RoleTester || suspends[0].Generation != 7 {
		t.Fatalf("suspend rows = %+v, want the tester's alone", suspends)
	}
	suspensions := issueSuspensions(t, rig.pool, "LEGION-209")
	if len(suspensions) != 1 || suspensions[0].Generation != 7 || suspensions[0].TreeGeneration != 7 {
		t.Fatalf("issue suspensions = %+v, want one row of the look's run", suspensions)
	}
	for _, kind := range []string{"supervise", "issue_suspend"} {
		rows := issueOutboxOf(t, rig.pool, "LEGION-209", kind)
		if rows[len(rows)-1].id <= start.id {
			t.Fatalf("the newest %s row %d is not newer than the look's start %d", kind, rows[len(rows)-1].id, start.id)
		}
	}
}

// A pass closes a child's check as recorded and ends the tester's part in it: its claim is
// closed (issue_close) and the child's Sandbox deleted with the pod it ran in (an IssueSuspend),
// since its five other claims retired at the close. A fail closes it as a defect the same way.
func TestAPassOrFailOfAChildsCheckClosesItAndDeletesItsSandbox(t *testing.T) {
	for _, tc := range []struct {
		verdict string
		outcome record.CheckOutcome
	}{{"pass", record.CheckRecorded}, {"fail", record.CheckDefect}} {
		t.Run(tc.verdict, func(t *testing.T) {
			rig := childCheckRig(t)
			rig.apply("report", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: tc.verdict, Summary: "seen"})
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check == nil || check.Open() || check.Outcome != tc.outcome || check.LookOpen || !check.ClosedAt.Equal(checkOpened) {
				t.Fatalf("the check after %s = %+v, want it closed as %s", tc.verdict, check, tc.outcome)
			}
			closed := superviseRequests(t, rig.pool, "issue_close")
			for _, role := range claim.Roles {
				if closed["LEGION-209/"+string(role)] != 1 {
					t.Fatalf("issue_close rows = %v, want every role's once, the tester's by the check's close", closed)
				}
			}
			if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 1 || got[0].Generation != 7 || got[0].TreeGeneration != 7 {
				t.Fatalf("issue suspensions = %+v, want one row of the check's run", got)
			}
			assertOutboxCount(t, rig.pool, "linger_close", 0)
		})
	}
}

// A pass of a root's check closes the tester's claim and writes the suspension that deletes
// its Sandbox, as the linger's own row did: the tree's close then finds nothing of it left.
func TestAPassOfARootsCheckClosesItsTesterAndSuspendsTheIssue(t *testing.T) {
	rig := rootCheckRig(t)
	rig.apply("pass", intake.CheckReport{Issue: "LEGION-208", Look: 1, Verdict: "pass", Summary: "seen"})
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-208/tester"] != 1 || len(got) != 1 {
		t.Fatalf("issue_close rows = %v, want the tester's alone", got)
	}
	got := issueSuspensions(t, rig.pool, "LEGION-208")
	if len(got) != 2 {
		t.Fatalf("the root's issue suspensions = %+v, want the linger's and the check's", got)
	}
	// The root lingers until an hour from now, so the tree's close is not re-armed by the check.
	assertOutboxCount(t, rig.pool, "linger_close", 1)
}

// A report is refused, and changes nothing, on an issue with no open check (none, or one closed),
// on a look that is not the open one (ended by its bound or a hold, or an earlier look), and with
// a verdict other than pass, pending or fail.
func TestACheckReportIsRefusedWhereNoLookIsOpenToIt(t *testing.T) {
	rig := childCheckRig(t)
	if got := refusalCode(rig.apply("no-check", intake.CheckReport{Issue: "LEGION-208", Look: 1, Verdict: "pass"})); got != "CHECK_NOT_OPEN" {
		t.Fatalf("a report on an issue with no check = %q, want CHECK_NOT_OPEN", got)
	}
	if got := refusalCode(rig.apply("bad-verdict", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "maybe"})); got != "CHECK_VERDICT_REQUIRED" {
		t.Fatalf("a report with verdict maybe = %q, want CHECK_VERDICT_REQUIRED", got)
	}
	if got := refusalCode(rig.apply("wrong-look", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pass"})); got != "CHECK_LOOK_NOT_OPEN" {
		t.Fatalf("a report naming look 2 while look 1 is open = %q, want CHECK_LOOK_NOT_OPEN", got)
	}
	if check := productionCheck(t, rig.pool, "LEGION-209"); check == nil || !check.Open() || !check.LookOpen || check.Looks != 1 {
		t.Fatalf("the check after the refused reports = %+v, want it untouched at look 1 open", check)
	}
	rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "nothing in production yet"})
	if got := refusalCode(rig.apply("old-look", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pass"})); got != "CHECK_LOOK_NOT_OPEN" {
		t.Fatalf("a second report of look 1 = %q, want CHECK_LOOK_NOT_OPEN", got)
	}
	rig.clock.now = checkOpened.Add(checkBound)
	rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
	rig.apply("pass", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pass"})
	if got := refusalCode(rig.apply("closed", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pass"})); got != "CHECK_NOT_OPEN" {
		t.Fatalf("a report on a closed check = %q, want CHECK_NOT_OPEN", got)
	}
}

// A look nobody reports ends at its bound as a pending report would: the tester is suspended
// and the Sandbox with it. A row naming a look already ended, or an earlier look, changes nothing.
func TestCheckLookEndedEndsASilentLook(t *testing.T) {
	rig := childCheckRig(t)
	rig.apply("stale-look", intake.CheckLookEnded{Issue: "LEGION-209", Look: 2, Cause: "look bound"})
	if check := productionCheck(t, rig.pool, "LEGION-209"); !check.LookOpen {
		t.Fatalf("the check after a row of a look it has not reached = %+v, want look 1 still open", check)
	}
	rig.clock.now = checkOpened.Add(CheckLookBound)
	rig.apply("look-bound", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if !check.Open() || check.Looks != 1 || check.LookOpen {
		t.Fatalf("the check after its look's bound = %+v, want it open at look 1 with the look ended", check)
	}
	if got := superviseRequests(t, rig.pool, "suspend"); got["LEGION-209/tester"] != 1 || len(got) != 1 {
		t.Fatalf("suspend rows = %v, want the tester's", got)
	}
	if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 1 {
		t.Fatalf("issue suspensions = %+v, want one row", got)
	}
	rig.apply("look-bound-again", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound"})
	if got := superviseRequests(t, rig.pool, "suspend"); got["LEGION-209/tester"] != 1 {
		t.Fatalf("suspend rows after a second row of the ended look = %v, want the one", got)
	}
}

// A fact of a check a later merge of the same issue replaced — the first check closed as not run
// when the child was set back to todo, and the rerun's merge opened the second, which counts its
// looks from one again — changes nothing of the second: a look bound naming the first check's
// OpenedAt leaves the second's look 1 open, where the same fact naming the second check ends the
// look.
func TestAFactOfAReplacedCheckChangesNothingOfItsSuccessor(t *testing.T) {
	rig := newCheckRig(t)
	seedMergingChild(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	firstOpened := checkOpened.Add(-24 * time.Hour)
	firstClosed := firstOpened.Add(2 * time.Hour)
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(t.Context(), tx, record.ProductionCheck{
			Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion", Number: 41, MergeSHA: "merge-41",
			OpenedAt: firstOpened, BoundAt: firstOpened.Add(checkBound), Looks: 1, LookWakeAt: &firstOpened,
			LastWakeKind: "merge", LastWakeDetail: "merge-41", LastWakeAt: &firstOpened, ClosedAt: &firstClosed, Outcome: record.CheckNotRun,
		})
	})
	rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
	if check := productionCheck(t, rig.pool, "LEGION-209"); check == nil || !check.Open() || !check.OpenedAt.Equal(checkOpened) || check.Looks != 1 || !check.LookOpen {
		t.Fatalf("the check after the rerun's merge = %+v, want the second check, opened at the merge, at look 1 open", check)
	}

	rig.clock.now = checkOpened.Add(CheckLookBound)
	rig.apply("stale-bound", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound", OpenedAt: firstOpened})
	if check := productionCheck(t, rig.pool, "LEGION-209"); !check.LookOpen || check.Looks != 1 {
		t.Fatalf("the second check after the first check's look bound = %+v, want its look 1 still open", check)
	}
	if got := superviseRequests(t, rig.pool, "suspend"); len(got) != 0 {
		t.Fatalf("suspend rows after the first check's look bound = %v, want none", got)
	}
	rig.apply("bound", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	if check := productionCheck(t, rig.pool, "LEGION-209"); check.LookOpen || check.Looks != 1 || !check.Open() {
		t.Fatalf("the second check after its own look bound = %+v, want it open at look 1 with the look ended", check)
	}
	if got := superviseRequests(t, rig.pool, "suspend"); got["LEGION-209/tester"] != 1 {
		t.Fatalf("suspend rows after the second check's look bound = %v, want the tester's", got)
	}
}

// The claim of the role a check records failing inside a look ends the look as a pending report
// would and tells the architect immediately. It holds nothing: a done issue has no phase to hold,
// and the next wake starts the claim again. Another role's claim failing on the done issue
// changes nothing, as before: the implementer's, which worked the issue's phases but not its
// check.
func TestTheTestersFailedClaimEndsItsLook(t *testing.T) {
	rig := childCheckRig(t)
	rig.apply("implementer-failed", intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleImplementer})
	if check := productionCheck(t, rig.pool, "LEGION-209"); !check.LookOpen {
		t.Fatalf("the check after the implementer's claim failed = %+v, want look 1 still open", check)
	}
	rig.apply("tester-failed", intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleTester})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if !check.Open() || check.Looks != 1 || check.LookOpen {
		t.Fatalf("the check after the tester's claim failed = %+v, want it open at look 1 with the look ended", check)
	}
	if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 1 {
		t.Fatalf("issue suspensions = %+v, want one row", got)
	}
	var childPhase phase.Phase
	if err := rig.pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-209'").Scan(&childPhase); err != nil || childPhase != phase.Done {
		t.Fatalf("the child's phase = %s, %v; want done, not held", childPhase, err)
	}
	notices := noticeKinds(t, rig.pool, "LEGION-209")
	if containsNotice(notices, "held") || !containsNotice(notices, "worker-died") {
		t.Fatalf("notices %v, want one worker-died and no held notice for a check look", notices)
	}
}

// A check recorded with the implementer — the shape every check open before the upgrade has, when
// the implementer worked each of them — stays the implementer's for as long as it is open, under
// an engine whose own checks are the tester's: the look its bound starts is handed to the
// implementer, and that implementer's report of the look is taken and retires its claim.
func TestACheckRecordingTheImplementerStartsTheImplementerAndTakesItsReport(t *testing.T) {
	rig := newCheckRig(t)
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	parent := "LEGION-208"
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.Done, Generation: 7, Status: "done", Rank: "V", LastDispatchSeq: 2})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", HeadSHA: "head-43", MergeSHA: "merge-43", Failing: []string{}})
	opened := checkOpened
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		return record.NewStore().PutProductionCheck(t.Context(), tx, record.ProductionCheck{
			Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
			OpenedAt: opened, BoundAt: opened.Add(checkBound), Role: claim.RoleImplementer, Looks: 1,
			LastWakeKind: wakeMerge, LastWakeDetail: "merge-43", LastWakeAt: &opened,
		})
	})

	rig.clock.now = checkOpened.Add(checkBound)
	rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].CheckLook != 2 || !starts[0].CheckOpenedAt.Equal(checkOpened) {
		t.Fatalf("start rows = %+v, want look 2 handed to the implementer the check records", starts)
	}
	if check := productionCheck(t, rig.pool, "LEGION-209"); check.Role != claim.RoleImplementer || !check.LookOpen || check.Looks != 2 {
		t.Fatalf("the check at its bound = %+v, want the implementer's, at look 2 open", check)
	}

	rig.apply("pass", intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pass", Summary: "seen in production"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Open() || check.Outcome != record.CheckRecorded {
		t.Fatalf("the check after the implementer's pass = %+v, want it closed as recorded", check)
	}
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/implementer"] != 1 || len(got) != 1 {
		t.Fatalf("issue_close rows = %v, want the implementer's alone: the check's own role is the implementer", got)
	}
}

// A check keeps the role it opened with when the project's `production_check` changes under it:
// the setting decides the checks a close opens, never one already open, which is why the role is
// a column of the record. A check opened under `tester` is still the tester's for the rest of its
// looks once the daemon is rebuilt with the setting off, where that same daemon opens no check of
// its own.
func TestACheckOpenedUnderTheTesterKeepsItWhenTheSettingGoesOff(t *testing.T) {
	rig := childCheckRig(t)
	off := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: checkBound, Clock: rig.clock.Now}, nil)
	apply := applyFacts(t, rig.pool, off)

	apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending", Summary: "not deployed yet"})
	suspends := superviseRowsOf(t, rig.pool, "LEGION-209", "suspend")
	if len(suspends) != 1 || suspends[0].Role != claim.RoleTester {
		t.Fatalf("suspend rows = %+v, want the tester's alone: the check is the tester's", suspends)
	}
	rig.clock.now = checkOpened.Add(checkBound)
	apply("bound", intake.CheckBound{Issue: "LEGION-209"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Role != claim.RoleTester || check.Looks != 2 || !check.LookOpen || !check.Last {
		t.Fatalf("the check at its bound = %+v, want the tester's last look 2 open", check)
	}
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if len(starts) != 2 || starts[1].Role != claim.RoleTester {
		t.Fatalf("start rows = %+v, want the bound's look handed to the tester the check records", starts)
	}
}

// A root reopened inside its linger runs again from the reopen: admit's readmit stands it in
// `admitted` with the reopen's status, no role's phase, and its production check untouched. Nothing
// of the check moves the tree while it runs again: the implementer's failed claim takes the live
// phase's path rather than ending the look (the look's own bound ends it, and that end starts no
// look and suspends nothing, since the implementer and pod are the run's), and the bound closes
// the check as not run without an issue_close of the implementer the run reuses.
func TestAReopenedRootsCheckStartsNoLookWhileItRunsAgain(t *testing.T) {
	rig := rootCheckRig(t)
	if _, err := rig.pool.Exec(context.Background(), "update issues set phase = 'admitted', status = 'todo', linger_until = null where key = 'LEGION-208'"); err != nil {
		t.Fatal(err)
	}
	before := len(superviseRowsOf(t, rig.pool, "LEGION-208", "suspend"))
	suspensions := len(issueSuspensions(t, rig.pool, "LEGION-208"))

	rig.apply("implementer-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer})
	if check := productionCheck(t, rig.pool, "LEGION-208"); !check.Open() || !check.LookOpen {
		t.Fatalf("the check after the implementer's claim failed while the root runs again = %+v, want its look still open", check)
	}
	if !containsNotice(noticeKinds(t, rig.pool, "LEGION-208"), "worker-died") {
		t.Fatalf("notices %v after the implementer's claim failed, want worker-died: the failure is the run's", noticeKinds(t, rig.pool, "LEGION-208"))
	}

	rig.apply("look-bound", intake.CheckLookEnded{Issue: "LEGION-208", Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	check := productionCheck(t, rig.pool, "LEGION-208")
	if !check.Open() || check.Looks != 1 || check.LookOpen {
		t.Fatalf("the check after its look's bound = %+v, want open at look 1 with the look ended", check)
	}
	if got := len(superviseRowsOf(t, rig.pool, "LEGION-208", "suspend")); got != before {
		t.Fatalf("suspend rows = %d, want %d: the look's end suspends nothing of a run", got, before)
	}
	if got := len(issueSuspensions(t, rig.pool, "LEGION-208")); got != suspensions {
		t.Fatalf("issue suspensions = %d, want %d: the pod is the run's", got, suspensions)
	}

	rig.clock.now = checkOpened.Add(checkBound)
	rig.apply("bound", intake.CheckBound{Issue: "LEGION-208"})
	check = productionCheck(t, rig.pool, "LEGION-208")
	if check.Open() || check.Outcome != record.CheckNotRun {
		t.Fatalf("the check at its bound while the root runs again = %+v, want closed as not run", check)
	}
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-208/implementer"] != 0 {
		t.Fatalf("issue_close rows = %v, want none for the implementer the run reuses", got)
	}
}

// A done child a person parks and sets done again leaves twice more, each a close of the same
// merge: the check it has stands, recorded or open, and no second check is opened over it, no
// second bound row queued, no look started. A rerun's merge, of another pull request after a
// re-entry closed the check, still replaces it (openCheck).
func TestADoneChildSetDoneAgainKeepsItsChecks(t *testing.T) {
	rig := childCheckRig(t)
	rig.apply("report", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pass", Summary: "seen"})
	recorded := productionCheck(t, rig.pool, "LEGION-209")
	bounds := outboxKindCount(t, rig.pool, "LEGION-209", record.OutboxKindCheckBound)

	rig.apply("parked", intake.DispatchIssue{Key: "LEGION-209", Seq: 3, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"})
	// Admission, which runs after the engine on each event, records the observation the engine
	// kept its hands off (the parked status, the sequence); the rig's admission stub records
	// nothing, so the row is moved by hand to what the daemon would hold at the next event.
	if _, err := rig.pool.Exec(context.Background(), "update issues set status = 'backlog', dispatch_status = 'backlog', last_dispatch_seq = 3 where key = 'LEGION-209'"); err != nil {
		t.Fatal(err)
	}
	rig.apply("done-again", intake.DispatchIssue{Key: "LEGION-209", Seq: 4, Type: "issue.updated", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"})
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/planner"] != 2 {
		t.Fatalf("issue_close rows = %v, want the second done to have left again (two of the planner's)", got)
	}
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Open() || check.Outcome != recorded.Outcome || !check.OpenedAt.Equal(recorded.OpenedAt) || check.Looks != recorded.Looks {
		t.Fatalf("the check after the child was set done again = %+v, want the recorded one kept: %+v", check, recorded)
	}
	if got := outboxKindCount(t, rig.pool, "LEGION-209", record.OutboxKindCheckBound); got != bounds {
		t.Fatalf("check_bound rows = %d, want %d: no second check was opened", got, bounds)
	}
	if starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start"); len(starts) != 1 {
		t.Fatalf("start rows = %+v, want look 1's alone", starts)
	}
}

// The check's bound starts its last look, told that it is the last and that production did not
// carry the merge within the bound; a pending report of the last look closes the check as not run,
// and so does its silence. A look open at the bound defers the bound by the look bound, and a bound
// row that comes due before the check's bound — an earlier check's of the same issue — is deferred
// to it.
func TestTheBoundStartsTheLastLookWhoseNothingClosesTheCheckAsNotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  intake.Fact
	}{
		{name: "pending", end: intake.CheckReport{Issue: "LEGION-209", Look: 2, Verdict: "pending"}},
		{name: "silence", end: intake.CheckLookEnded{Issue: "LEGION-209", Look: 2, Cause: "look bound"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			rig.apply("early-bound", intake.CheckBound{Issue: "LEGION-209"})
			if bounds := issueOutboxOf(t, rig.pool, "LEGION-209", "check_bound"); len(bounds) != 2 || !bounds[1].nextAt.Equal(checkOpened.Add(checkBound)) {
				t.Fatalf("check_bound rows after an early bound = %+v, want a second deferred to the check's bound", bounds)
			}
			rig.clock.now = checkOpened.Add(checkBound)
			rig.apply("bound-in-look", intake.CheckBound{Issue: "LEGION-209"})
			check := productionCheck(t, rig.pool, "LEGION-209")
			if check.Looks != 1 || !check.LookOpen || check.Last {
				t.Fatalf("the check after a bound during look 1 = %+v, want look 1 still open and not last", check)
			}
			if bounds := issueOutboxOf(t, rig.pool, "LEGION-209", "check_bound"); len(bounds) != 3 || !bounds[2].nextAt.Equal(rig.clock.now.Add(CheckLookBound)) {
				t.Fatalf("check_bound rows after a bound during a look = %+v, want a third deferred two hours", bounds)
			}
			rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending"})
			rig.clock.now = rig.clock.now.Add(CheckLookBound)
			rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
			check = productionCheck(t, rig.pool, "LEGION-209")
			if check.Looks != 2 || !check.LookOpen || !check.Last || check.LastWakeKind != "bound" || check.LastWakeDetail != "production did not carry the merge within 2 days" {
				t.Fatalf("the check at its bound = %+v, want its last look 2 open, woken by the bound", check)
			}
			starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
			if len(starts) != 2 || starts[1].CheckLook != 2 || !strings.Contains(starts[1].Task, "This is the last look: no look follows.") ||
				!strings.Contains(starts[1].Task, "woken by the bound: production did not carry the merge within 2 days.") || !strings.Contains(starts[1].Task, "did not run") {
				t.Fatalf("start rows = %+v, want the last look's task saying so", starts)
			}
			rig.apply("end", tc.end)
			check = productionCheck(t, rig.pool, "LEGION-209")
			if check.Open() || check.Outcome != record.CheckNotRun || check.LookOpen {
				t.Fatalf("the check after its last look's %s = %+v, want it closed as not run", tc.name, check)
			}
			if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/tester"] != 1 {
				t.Fatalf("issue_close rows = %v, want the tester's once at the close", got)
			}
			if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 2 {
				t.Fatalf("issue suspensions = %+v, want look 1's row and the close's", got)
			}
			rig.apply("bound-after-close", intake.CheckBound{Issue: "LEGION-209"})
			if bounds := issueOutboxOf(t, rig.pool, "LEGION-209", "check_bound"); len(bounds) != 3 {
				t.Fatalf("check_bound rows after the close = %d, want no more", len(bounds))
			}
		})
	}
}

// The tree's close at the end of its linger waits for an open check: the expiry returns a wait
// naming the moment the check is bound to have ended its last look, rolls back unapplied, and the
// tree stays as it was. The check's close re-arms the linger's close, due now, and the expiry then
// applies.
func TestTheTreesCloseWaitsForAnOpenCheckAndRunsOnceItCloses(t *testing.T) {
	rig := rootCheckRig(t)
	rig.clock.now = checkOpened.Add(time.Hour)
	expiry := intake.LingerExpired{Issue: "LEGION-208", Generation: 7}
	_, err := intake.ApplyFact(context.Background(), rig.pool, "outbox", "linger:LEGION-208:7", expiry, rig.engine, admissionStub{})
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the linger's expiry with an open check = %v, want a wait", err)
	}
	if at, ok := wait.UntilAt(err); !ok || !at.Equal(checkOpened.Add(checkBound+CheckLookBound)) {
		t.Fatalf("the wait names %s (%t), want the check's bound plus the look bound", at, ok)
	}
	if !strings.Contains(err.Error(), "member LEGION-208's production check is open") {
		t.Fatalf("the wait says %q, want it to name the open check", err)
	}
	if got := superviseRequests(t, rig.pool, "tree_close"); len(got) != 0 {
		t.Fatalf("tree_close rows = %v, want none while the check is open", got)
	}
	assertOutboxCount(t, rig.pool, "linger_close", 1)

	rig.apply("pass", intake.CheckReport{Issue: "LEGION-208", Look: 1, Verdict: "pass"})
	closes := issueOutboxOf(t, rig.pool, "LEGION-208", "linger_close")
	if len(closes) != 2 || !closes[1].nextAt.Equal(rig.clock.now) {
		t.Fatalf("linger_close rows after the check's close = %+v, want a second due now", closes)
	}
	if _, err := intake.ApplyFact(context.Background(), rig.pool, "outbox", "linger:LEGION-208:7", expiry, rig.engine, admissionStub{}); err != nil {
		t.Fatalf("the linger's expiry once the check closed: %v", err)
	}
	everyClaim := map[string]int{}
	for _, issue := range []string{"LEGION-208", "LEGION-209"} {
		for _, role := range claim.Roles {
			everyClaim[issue+"/"+string(role)]++
		}
	}
	if got := superviseRequests(t, rig.pool, "tree_close"); !sameCounts(got, everyClaim) {
		t.Fatalf("tree_close rows = %v, want each tree claim once: %v", got, everyClaim)
	}
}

// A done child set back to todo closes its open check as not run before its new run starts. The
// tester's live claim stays live: the closed check rejects its old look's report.
func TestReenterChildClosesItsOpenCheckAsNotRun(t *testing.T) {
	rig := childCheckRig(t)
	seedSlot(t, rig.pool, record.Slot{Issue: "LEGION-208", Index: 0})
	rig.apply("todo", intake.DispatchIssue{Key: "LEGION-209", Seq: 3, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-208", Rank: "V"})
	check := productionCheck(t, rig.pool, "LEGION-209")
	if check.Open() || check.Outcome != record.CheckNotRun || check.LookOpen {
		t.Fatalf("the re-entered child's check = %+v, want it closed as not run", check)
	}
	var generation uint64
	var childPhase phase.Phase
	if err := rig.pool.QueryRow(context.Background(), "select generation, phase from issues where key = 'LEGION-209'").Scan(&generation, &childPhase); err != nil || generation != 8 || childPhase != phase.Admitted {
		t.Fatalf("the re-entered child = generation %d in %s, %v; want generation 8 admitted", generation, childPhase, err)
	}
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-209/tester"] != 0 {
		t.Fatalf("issue_close rows = %v, want none for the tester the rerun reuses", got)
	}
	suspends := superviseRowsOf(t, rig.pool, "LEGION-209", "suspend")
	if len(suspends) != 0 {
		t.Fatalf("suspend rows = %+v, want none while the child is back in the workflow", suspends)
	}
	if got := issueSuspensions(t, rig.pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("issue suspensions = %+v, want none: the pod is the new run's", got)
	}
}

// The boot's sweep closes a migrated issue that awaits a merge GitHub already reported, as the
// merge would have: a root with no running child, and an issue held from awaiting_merge, whose
// hold the close clears, each with a check opened on no merge commit yet. A root with a child still
// in the workflow stays, and closes when that child leaves; an issue whose pull request is open,
// and another project's, stay as they are. The boot writes its "closes" line only where it closed,
// and no notice at all: the architect was told of the wait when the close entered it.
func TestTheBootSweepClosesMigratedMergedIssuesAndLeavesARootWithARunningChild(t *testing.T) {
	rig := newCheckRig(t)
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-301", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-301", Project: "LEGION", Title: "alone", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "A"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-301", Repo: "sjawhar/legion", Number: 1, Branch: "legion/LEGION-301", HeadSHA: "h1", Failing: []string{}})
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-302", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-302", Project: "LEGION", Title: "parent", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "B", LastDispatchSeq: 1})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-302", Repo: "sjawhar/legion", Number: 2, Branch: "legion/LEGION-302", HeadSHA: "h2", MergeSHA: "m2", Failing: []string{}})
	parent := "LEGION-302"
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-303", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-302", Project: "LEGION", Title: "running child", Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "C", LastDispatchSeq: 1})
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-304", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-304", Project: "LEGION", Title: "held", Phase: phase.Held, Hold: &record.Hold{From: phase.AwaitingMerge}, Generation: 1, Status: "retro", Rank: "D"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-304", Repo: "sjawhar/legion", Number: 4, Branch: "legion/LEGION-304", HeadSHA: "h4", Failing: []string{}})
	seedIssue(t, rig.pool, record.Issue{Key: "LEGION-305", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-305", Project: "LEGION", Title: "open", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "E"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-305", Repo: "sjawhar/legion", Number: 5, Branch: "legion/LEGION-305", HeadSHA: "h5", Failing: []string{}})
	seedIssue(t, rig.pool, record.Issue{Key: "OTHER-1", Repo: "acme/other", DefaultBranch: "main", GitHubRepo: "acme/other", Tree: "OTHER-1", Project: "OTHER", Title: "elsewhere", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "F"})
	seedPR(t, rig.pool, record.PullRequest{State: record.PullRequestMerged, Issue: "OTHER-1", Repo: "acme/other", Number: 6, Branch: "legion/OTHER-1", HeadSHA: "h6", Failing: []string{}})

	rig.apply("boot", intake.DaemonBooted{Boot: 1})
	phases := map[string]phase.Phase{}
	lingering := map[string]bool{}
	rows, err := rig.pool.Query(context.Background(), "select key, phase, linger_until is not null from issues")
	if err != nil {
		t.Fatalf("read issues: %v", err)
	}
	for rows.Next() {
		var key string
		var p phase.Phase
		var lingers bool
		if err := rows.Scan(&key, &p, &lingers); err != nil {
			t.Fatalf("scan issue: %v", err)
		}
		phases[key], lingering[key] = p, lingers
	}
	rows.Close()
	for key, want := range map[string]phase.Phase{"LEGION-301": phase.Done, "LEGION-302": phase.AwaitingMerge, "LEGION-303": phase.Implementing, "LEGION-304": phase.Done, "LEGION-305": phase.AwaitingMerge, "OTHER-1": phase.AwaitingMerge} {
		if phases[key] != want || lingering[key] != (want == phase.Done) {
			t.Fatalf("%s after the boot = %s lingering %t, want %s", key, phases[key], lingering[key], want)
		}
	}
	for _, key := range []string{"LEGION-301", "LEGION-304"} {
		if check := productionCheck(t, rig.pool, key); check == nil || !check.Open() || check.Looks != 1 || check.MergeSHA != "" {
			t.Fatalf("%s's check after the boot = %+v, want it open at look 1 with no merge commit yet", key, check)
		}
		if starts := superviseRowsOf(t, rig.pool, key, "start"); len(starts) != 1 || starts[0].CheckLook != 1 || !strings.Contains(starts[0].Task, "merge commit unknown, read it from the pull request on GitHub") {
			t.Fatalf("%s's starts = %+v, want its check's look 1 telling the agent to read the merge commit from GitHub", key, starts)
		}
	}
	var held *string
	if err := rig.pool.QueryRow(context.Background(), "select held_from from issues where key = 'LEGION-304'").Scan(&held); err != nil || held != nil {
		t.Fatalf("the held issue's hold after its close = %v, %v; want cleared", held, err)
	}
	for _, key := range []string{"LEGION-302", "LEGION-305", "OTHER-1"} {
		if check := productionCheck(t, rig.pool, key); check != nil {
			t.Fatalf("%s's check after the boot = %+v, want none", key, check)
		}
	}
	// The boot writes its "closes" line only where it closed, and tells no architect anything: a
	// restart must not repeat the notice the entry into the wait wrote, and a root can now wait for
	// days across many boots.
	for _, key := range []string{"LEGION-301", "LEGION-304"} {
		if !strings.Contains(rig.logged.String(), `upgrade" issue=`+key) {
			t.Fatalf("the boot's journal holds no close of %s:\n%s", key, rig.logged.String())
		}
	}
	if strings.Contains(rig.logged.String(), `upgrade" issue=LEGION-302`) {
		t.Fatalf("the boot logged a close of the root it left waiting:\n%s", rig.logged.String())
	}
	if got := noticeKinds(t, rig.pool, "LEGION-302"); len(got) != 0 {
		t.Fatalf("the waiting root's notices after the boot = %v, want none", got)
	}

	rig.apply("child-done", intake.DispatchIssue{Key: "LEGION-303", Seq: 2, Type: "issue.closed", Status: "done", Title: "running child", Parent: "LEGION-302", Rank: "C"})
	var rootPhase phase.Phase
	var rootLingers bool
	if err := rig.pool.QueryRow(context.Background(), "select phase, linger_until is not null from issues where key = 'LEGION-302'").Scan(&rootPhase, &rootLingers); err != nil || rootPhase != phase.Done || !rootLingers {
		t.Fatalf("the parent once its last child left = %s lingering %t, %v; want done and lingering", rootPhase, rootLingers, err)
	}
	if check := productionCheck(t, rig.pool, "LEGION-302"); check == nil || !check.Open() || check.MergeSHA != "m2" {
		t.Fatalf("the parent's check = %+v, want it open on m2", check)
	}
	if got := superviseRequests(t, rig.pool, "issue_close"); got["LEGION-303/implementer"] != 1 || got["LEGION-302/implementer"] != 0 {
		t.Fatalf("issue_close rows = %v, want the child's every claim closed and the parent's none", got)
	}
}

// The check task is held to the retired phrases the composed prompts are held to
// (internal/prompts' TestNoComposedPromptTeachesAnE2ELineOrAProofArray): what a look proves is a
// result recorded against the issue's after_merge check, so each of the three verdicts asks for
// `dispatch result` at the merge commit and none asks for the retired Production: line. The task
// is prose for a pane, so it carries the phrases without their backticks.
func TestTheCheckTaskAsksForARecordedResultAndNoRetiredLine(t *testing.T) {
	issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
	check := record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43", OpenedAt: checkOpened, Looks: 1}
	for _, last := range []bool{false, true} {
		task := checkTask(issue, check, wakeMerge, "", last)
		for _, retired := range []string{"Production: line", "pending deploy of", "proof array"} {
			if strings.Contains(task, retired) {
				t.Errorf("the check task (last %t) still says %q", last, retired)
			}
		}
		for _, want := range []string{"merge commit merge-43", "dispatch result"} {
			if !strings.Contains(task, want) {
				t.Errorf("the check task (last %t) omits %q", last, want)
			}
		}
	}
}

// outboxKindCount is how many outbox rows of kind issue has, settled or not.
func outboxKindCount(t *testing.T, pool *pgxpool.Pool, issue string, kind record.OutboxKind) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where issue = $1 and kind = $2", issue, string(kind)).Scan(&count); err != nil {
		t.Fatalf("count outbox rows of kind %s for %s: %v", kind, issue, err)
	}
	return count
}
