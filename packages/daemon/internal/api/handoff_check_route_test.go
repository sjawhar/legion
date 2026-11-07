package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// newWorkflowHarness serves the daemon's routes with the workflow of cfg deciding every fact they
// apply, over a real Postgres and the GitHub stand-in a completing implementer reads. It answers
// the engine the test applies its own facts with — a settlement, a status a person set, a failed
// claim — and the recorder of every fact the routes applied.
func newWorkflowHarness(t *testing.T, cfg workflow.Config) (*harness, *workflow.Engine, *factRecorder) {
	t.Helper()
	h := newHarness(t)
	h.handoffs = &statusRecorder{}
	github := newHandoffGitHub(t)
	facts := &factRecorder{}
	engine := workflow.New(record.NewStore(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: []intake.Handler{engine, facts}, Dispatch: h.handoffs,
		Tokens: handoffTokens{}, GitHubAPI: github.url,
	}).Handler
	return h, engine, facts
}

// worksLook hands c's claim the task of look of the production check opened at openedAt and starts
// its turn, as the look's own start row does (workflow's startLook: a task of no phase carrying
// the look and the check instance). The turn the claim is in ends first, which retires the task it
// held: a machine refuses a second delivery while one is pending, so the outbox's delivery waits
// for that anyway.
func (c liveClaim) worksLook(t *testing.T, look int, openedAt time.Time) {
	t.Helper()
	machine, ok := c.h.supervisor.Machine(c.token)
	if !ok {
		t.Fatalf("no machine for %s", c.token)
	}
	ctx := context.Background()
	if machine.Claim().State == supervise.StateWorking {
		if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: c.token}); err != nil {
			t.Fatalf("end the turn of %s: %v", c.token, err)
		}
	}
	id := fmt.Sprintf("outbox:look:%s:%d:%d", c.token, openedAt.UnixMicro(), look)
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: c.token, Task: "Production check of " + c.issue + ".", ID: id,
		Generation: issueGeneration(t, c.h, c.issue), CheckLook: supervise.CheckLook{Look: look, OpenedAt: openedAt}}); err != nil {
		t.Fatalf("deliver look %d to %s: %v", look, c.token, err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: c.token, DeliveryID: id}); err != nil {
		t.Fatalf("start look %d's turn on %s: %v", look, c.token, err)
	}
	if got := machine.Claim().ServingCheck(); got.Look != look {
		t.Fatalf("the claim serves look %+v, want look %d of the check opened %s", got, look, openedAt)
	}
}

// newCheckLookClaim is the live claim of role on key whose worker took look of key's production
// check, the check seedCheck records: what the look's start leaves, and what the completion route
// reads to tell a report on that look from a phase completion.
func newCheckLookClaim(t *testing.T, h *harness, key string, role claim.Role, look int) liveClaim {
	t.Helper()
	c := newLiveClaim(t, h, key, role)
	c.worksLook(t, look, checkOpenedAt)
	return c
}

// mergedCloseBehindATestingTurn drives the reviewer's trace as far as the close. A child's pull
// request merges before awaiting_merge and a person sets the issue done from Dispatch while the
// tester's testing turn is still running: the close spares the check's role (workflow's
// linger.go, `except = e.checkRole()`), opens the tester's production check and starts look 1
// behind that running turn. It answers the harness, its record of every fact the routes applied,
// and the tester's claim with the machine that holds it.
func mergedCloseBehindATestingTurn(t *testing.T) (*harness, *factRecorder, liveClaim, *supervise.Machine) {
	t.Helper()
	now := time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	h, engine, facts := newWorkflowHarness(t, workflow.Config{Project: testProject, Repositories: testRepositories,
		ReviewRoundCap: 3, MaxFixAttempts: 3, ProductionCheckRole: claim.RoleTester, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }})
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Accepting, Generation: 1, Status: "in_progress"})
	h.recordIssue(record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-209", Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress"})
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-209", Repo: "acme/widgets",
			Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head", MergeSHA: "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0", Failing: []string{}, Required: []string{"pytest"}})
	}); err != nil {
		t.Fatalf("record the merged pull request: %v", err)
	}
	seedAssignment(t, h, "LEGION-209", claim.RoleTester, "testing-1")
	tester := newLiveClaimIn(t, h, "LEGION-208", "LEGION-209", claim.RoleTester)
	takeTask(t, h, tester.token, "outbox:testing-1", "Continue Workflow. Issue: LEGION-209. Phase: testing.", "testing-1", 1)

	// The person's done reaches the daemon while that turn runs.
	applyAssignmentFact(t, h, engine, "done", intake.DispatchIssue{Key: "LEGION-209", Status: "done", Title: "LEGION-209", ResolvedRepo: "acme/widgets"})
	opened := recordedCheck(t, h, "LEGION-209")
	if !opened.Open() || opened.Role != claim.RoleTester || opened.Looks != 1 || !opened.LookOpen {
		t.Fatalf("the check the merged close opened = %+v, want the tester's, open at look 1 with the look open", opened)
	}
	machine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	return h, facts, tester, machine
}

// theCheckIsUntouched is what a refusal of a completion that works no look leaves behind,
// whichever refusal it is: the refusal is written before the fact is applied, so no report of
// any kind reached the workflow, no check event is recorded as processed, and the check still
// stands open at look 1 with the look open.
func theCheckIsUntouched(t *testing.T, h *harness, facts *factRecorder) {
	t.Helper()
	for _, fact := range facts.recorded() {
		if report, isReport := fact.(intake.CheckReport); isReport {
			t.Fatalf("the testing verdict reached the workflow as %#v, want no report on production: nobody looked at it", report)
		}
	}
	if got := checkEvents(t, h, "LEGION-209"); got != 0 {
		t.Fatalf("processed check events = %d, want none", got)
	}
	after := recordedCheck(t, h, "LEGION-209")
	if !after.Open() || after.Looks != 1 || !after.LookOpen || after.Outcome != "" {
		t.Fatalf("the check after the testing verdict = %+v, want it still open at look 1 with the look open", after)
	}
}

// The reviewer's trace, which the issue's phase cannot tell apart from a check report and the
// caller's own assignment can. The tester ends the testing turn the close ran behind with a
// verdict of pass — which is also a check verdict.
//
// Routed by the issue's phase, that completion was recorded as the production check passing with
// nobody having looked at production. Routed by the claim's own live assignment, which is still
// the testing task's, it is no report on the check — and no phase completion either, since a done
// issue runs no phase, so the route names the cause (CHECK_LOOK_NOT_SERVED) rather than holding
// an answer the agent cannot give to a field. Nothing is recorded, and the check is untouched.
func TestATestingVerdictAfterTheMergedCloseIsNoReportOnTheProductionCheck(t *testing.T) {
	h, facts, tester, machine := mergedCloseBehindATestingTurn(t)
	if p := machine.Claim().Pending; p == nil || p.ConfirmedAt.IsZero() || p.Assignment != "testing-1" {
		t.Fatalf("the tester's delivery after the close is %+v, want the testing task still confirmed and pending: the look's start waits for it", p)
	}
	if look := machine.Claim().ServingCheck(); look.Look != 0 {
		t.Fatalf("the tester serves check look %+v, want none: its live assignment is the testing task's", look)
	}

	// The testing turn ends with the verdict a check report also names.
	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, "CHECK_LOOK_NOT_SERVED")
	if !strings.Contains(message, "works no look") || !strings.Contains(message, "recorded nothing") {
		t.Fatalf("the refusal is %q, want it to say this session works no look and recorded nothing", message)
	}
	theCheckIsUntouched(t, h, facts)
}

// The same trace one turn later, which the claim's own look field cannot tell apart on its own.
// The tester's testing turn ends, which retires its task and leaves the claim serving run 1 and
// assignment testing-1; look 1's task is then sent and acknowledged, and no turn starts within
// the daemon's wait for it (the wait and its take-back are driven in internal/supervise, where
// the clock moves: TestOnlyTheTurnBoundLeavesTheTaskMarkedAsRead). An Envoy notice — a CI answer —
// then starts a turn of the tester's own, which confirms nothing, and the tester reports the
// testing verdict it still owes in it.
//
// A look the claim has only had sent to it is no look it serves: read as one, this verdict would
// close the production check as a pass with nobody having looked at production, which is the
// whole of what the fallback for an unread task must not do. So the completion reports on no
// look, and a done issue runs no phase for it to complete either: it is refused
// CHECK_LOOK_NOT_SERVED, before the phase path, with nothing recorded and the check left open at
// its look.
func TestATestingVerdictBehindAnUnreadLooksTaskIsNoReportOnTheProductionCheck(t *testing.T) {
	h, facts, tester, machine := mergedCloseBehindATestingTurn(t)
	ctx := context.Background()
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: tester.token}); err != nil {
		t.Fatalf("end the tester's testing turn: %v", err)
	}
	look := supervise.CheckLook{Look: 1, OpenedAt: recordedCheck(t, h, "LEGION-209").OpenedAt}
	id := "outbox:look:1"
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: tester.token, Task: "Production check of LEGION-209.", ID: id,
		Generation: 1, CheckLook: look}); err != nil {
		t.Fatalf("deliver look 1 to the tester: %v", err)
	}
	if err := machine.Handle(ctx, supervise.PromptAcked{Claim: tester.token, Generation: machine.Claim().Generation, DeliveryID: id}); err != nil {
		t.Fatalf("acknowledge the look's prompt: %v", err)
	}
	c := machine.Claim()
	if p := c.Pending; p == nil || p.DeliveredAt.IsZero() || !p.ConfirmedAt.IsZero() || p.CheckLook != look {
		t.Fatalf("the tester's delivery is %+v, want look 1's task read and unconfirmed", p)
	}
	if c.ServingRun() != 1 || c.ServingAssignment() != "testing-1" || c.ServingCheck() != (supervise.CheckLook{}) {
		t.Fatalf("the tester serves run %d, assignment %q, look %+v; want run 1, testing-1 and no look: one serving, not three",
			c.ServingRun(), c.ServingAssignment(), c.ServingCheck())
	}

	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, "CHECK_LOOK_NOT_SERVED")
	if !strings.Contains(message, "works no look") || !strings.Contains(message, "recorded nothing") {
		t.Fatalf("the refusal is %q, want it to say this session works no look and recorded nothing", message)
	}
	theCheckIsUntouched(t, h, facts)
}

// A session that works no look of a done issue's production check completes nothing: the issue
// runs no phase. Held to the phase path, an implementer's and a merger's completion is refused
// for its commit (MISSING_FIELD), which the plugin's done branch never sends — it sends what a
// check report sends (pi-shared's legion-phase-ops.ts) — so that answer names a field no turn of
// theirs can fill. The route names the cause instead and says what the agent does next. A late
// implementer or merger turn after the close lands here, and so does a look a restart left
// unrecorded on its claim (the 20261105231916 migration backfills none). Nothing is recorded
// either way, so the answer is the whole of it.
func TestACompletionOfADoneIssueFromASessionWorkingNoLookIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  HandoffCompleteRequest
	}{
		{name: "the plugin's done branch, which sends no commit", req: HandoffCompleteRequest{Summary: "production carries it", Verdict: "pass"}},
		{name: "a phase completion at its commit", req: HandoffCompleteRequest{Summary: "merged", Commit: headSHA}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			seedCheck(t, h, "LEGION-208", claim.RoleTester, false, false)
			implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
			req := tc.req
			req.GrantID = implementer.grant(t)

			message := refused(t, h.complete(req), http.StatusConflict, "CHECK_LOOK_NOT_SERVED")
			for _, want := range []string{"LEGION-208 is done", "works no look", "recorded nothing", "pass, pending or fail"} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal is %q, want it to say %q", message, want)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts = %#v, want none: the refusal is written before any fact is applied", got)
			}
			if got := checkEvents(t, h, "LEGION-208"); got != 0 {
				t.Fatalf("processed check events = %d, want none", got)
			}
		})
	}
}

// The other arm of the same refusal, which the sibling above does not reach: a done issue whose
// production check has closed, or that never had one, has no look to send anybody. So the role
// that completes here is told it has nothing to report, rather than being pointed at a look task
// of its own that will never arrive — the answer the open arm gives, where the check names one
// role and only a turn of that role's look reports on it. Both ways of having no open check read
// alike: a check its last report closed, and an issue with no check row at all. Nothing of the
// call is recorded either way.
func TestACompletionOfADoneIssueWithNoOpenProductionCheckIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		check func(t *testing.T, h *harness)
	}{
		{name: "a production check that opened and has since closed", check: func(t *testing.T, h *harness) {
			seedCheck(t, h, "LEGION-208", claim.RoleTester, true, false)
		}},
		{name: "an issue that never had a production check", check: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Done)
			if tc.check != nil {
				tc.check(t, h)
			}
			implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)

			message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t),
				Summary: "production carries it", Verdict: "pass"}), http.StatusConflict, "CHECK_LOOK_NOT_SERVED")
			for _, want := range []string{"LEGION-208 is done", "works no look", "recorded nothing",
				"no open production check", "nothing here for any role to report"} {
				if !strings.Contains(message, want) {
					t.Fatalf("the refusal is %q, want it to say %q", message, want)
				}
			}
			if strings.Contains(message, "pass, pending or fail") {
				t.Fatalf("the refusal is %q, want none of the open check's arm: no look of this issue reaches any role, so there is no verdict to name", message)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("facts = %#v, want none: the refusal is written before any fact is applied", got)
			}
			if got := checkEvents(t, h, "LEGION-208"); got != 0 {
				t.Fatalf("processed check events = %d, want none", got)
			}
		})
	}
}

// A phase completion carries no wakeAt: the field names when a production check's next look is
// due, and a phase ends with no look to follow it. The contract says so for every client, so the
// daemon refuses one rather than dropping the field unread, before any fact is applied, and the
// same completion without a time is taken.
func TestAPhaseCompletionNamingAWakeIsRefused(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)

	handoff := tester.writeHandoff("test", 1)
	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: handoff, HandoffVersion: 1, WakeAt: &checkWakeAt}), http.StatusBadRequest, "CHECK_WAKE_UNEXPECTED")
	if !strings.Contains(message, "wakeAt") || !strings.Contains(message, "pending report") || !strings.Contains(message, "nothing was recorded") {
		t.Fatalf("the refusal is %q, want it to name the field, say a wake belongs to a check's pending report, and say nothing was recorded", message)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts = %#v, want none: the refusal is written before the fact is applied", got)
	}
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: handoff, HandoffVersion: 1}))
	if got := facts.recorded(); len(got) != 1 {
		t.Fatalf("facts after the completion without a time = %#v, want the one completion", got)
	}
}

// A worker whose live assignment is a check look reports on that look, not on whichever look the
// check has reached: a report from a turn that outlived its look names the look it was given, and
// the engine's own fence refuses it (CHECK_LOOK_NOT_OPEN) instead of recording it as the current
// look's.
func TestACheckReportCarriesTheLookTheClaimWasGivenNotTheChecksCurrentOne(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", claim.RoleImplementer, false, false)
	implementer := newCheckLookClaim(t, h, "LEGION-208", claim.RoleImplementer, 1)

	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked", Verdict: "pass"}))
	want := intake.CheckReport{Issue: "LEGION-208", Claim: implementer.token, Look: 1, Verdict: "pass", Summary: "looked"}
	if got := lastCheckReport(t, facts); !sameCheckReport(got, want) {
		t.Fatalf("check report = %+v, want %+v: the record is at look 2, and this worker's look is 1", got, want)
	}
}

// A look of a production check a later merge of the same issue replaced is no look of the check
// the issue holds now, whose looks count from one again: its report is refused CHECK_NOT_OPEN,
// with nothing recorded, rather than taken for the new check's look of the same number.
func TestACheckReportOfAReplacedChecksLookIsRefused(t *testing.T) {
	h, facts := newHandoffHarness(t, newHandoffGitHub(t), nil, nil)
	seedIssueAt(t, h, "LEGION-208", phase.Done)
	seedCheck(t, h, "LEGION-208", claim.RoleImplementer, false, false)
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	implementer.worksLook(t, 2, checkOpenedAt.Add(-24*time.Hour))

	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "looked", Verdict: "pass"}),
		http.StatusConflict, "CHECK_NOT_OPEN")
	if !strings.Contains(message, "replaced") {
		t.Fatalf("the refusal is %q, want it to say a later merge replaced the check this look belongs to", message)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("facts = %#v, want none", got)
	}
}
