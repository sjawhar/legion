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

// The reviewer's trace, which the issue's phase cannot tell apart from a check report and the
// caller's own assignment can. A child's pull request merges before awaiting_merge and a person
// sets the issue done from Dispatch while the tester's testing turn is still running: the close
// spares the check's role (workflow's linger.go, `except = e.checkRole()`), opens the production
// check and starts look 1 behind that running turn. The tester then ends its testing turn with a
// verdict of pass — which is also a check verdict.
//
// Routed by the issue's phase, that completion was recorded as the production check passing with
// nobody having looked at production. Routed by the claim's own live assignment, which is still
// the testing task's, it is a phase completion, and the phase path answers it as it answers any
// completion of a phase the issue has left: here HANDOFF_RECORD_MISMATCH, since phase done ends
// with no handoff record and this completion names its testing one. What matters is what every
// refusal of that path shares — it is written before the fact is applied — so the check is
// untouched: still open at look 1, with the look open, and no report of any kind recorded.
func TestATestingVerdictAfterTheMergedCloseIsNoReportOnTheProductionCheck(t *testing.T) {
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
	if p := machine.Claim().Pending; p == nil || p.ConfirmedAt.IsZero() || p.Assignment != "testing-1" {
		t.Fatalf("the tester's delivery after the close is %+v, want the testing task still confirmed and pending: the look's start waits for it", p)
	}
	if look := machine.Claim().ServingCheck(); look.Look != 0 {
		t.Fatalf("the tester serves check look %+v, want none: its live assignment is the testing task's", look)
	}

	// The testing turn ends with the verdict a check report also names.
	message := refused(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass",
		HandoffID: tester.writeHandoff("test", 1), HandoffVersion: 1}), http.StatusConflict, "HANDOFF_RECORD_MISMATCH")
	if !strings.Contains(message, "this completion changed nothing") {
		t.Fatalf("the refusal is %q, want it to say the completion changed nothing", message)
	}
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
