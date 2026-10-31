package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A tester's turn outlives its phase: the daemon ends no turn when a phase ends, so when CI goes
// red the implementer starts at once and the tester goes on working. Its issue comes back to
// testing on the implementer's round, where a second testing phase hands the tester a second
// task — and the completion the first task's turn reports is of work the issue has moved past. The
// run cannot tell the two apart, since the issue never left run 1, and neither can the phase, which
// is testing both times. The assignment can: the tester's claim still serves the first task's,
// because the machine refuses a second delivery while one is pending, while the phases row already
// names the second, written in the transition's own transaction. The completion is refused
// HANDOFF_STALE_ASSIGNMENT before any fact is applied, and the refusal sends the tester to the
// new task's own work: that task's turn reports a handoff of its own, which is taken.
func TestAStaleAssignmentsCompletionDoesNotTakeTheNextTestingPhases(t *testing.T) {
	h, engine, facts := newAssignmentHarness(t)
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}, Required: []string{"pytest"}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}

	// The implementer's round ends and the issue enters testing: the tester's first assignment.
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "handoff-1"}))
	assertIssuePhase(t, h, phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	firstTesting := takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)

	// CI goes red at the head while the tester works. The issue goes back to implementing, the
	// implementer starts at once, and the tester's turn, its process and its claim are untouched:
	// its delivery is still pending and confirmed.
	if result, err := intake.ApplyFact(ctx, h.store.Pool(), "github", "red", intake.PullRequestChecks{Repo: "acme/widgets", Number: 42,
		HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "pytest", ID: 7}}, Generation: 1, Snapshot: "red-head",
		Failing: []string{"pytest"}}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("apply the red settlement = %+v, %v", result.Refusal, err)
	}
	assertIssuePhase(t, h, phase.Implementing)
	testerMachine, ok := h.supervisor.Machine(tester.token)
	if !ok {
		t.Fatal("no machine for the tester")
	}
	if p := testerMachine.Claim().Pending; p == nil || p.ConfirmedAt.IsZero() || p.Assignment != firstTesting {
		t.Fatalf("the tester's delivery after the red is %+v, want its first assignment still confirmed and pending", p)
	}

	// The implementer's second round ends and the issue is in testing again: the tester's second
	// assignment, which its first task's turn knows nothing of.
	takeStart(t, h, implementer.token, "LEGION-208", claim.RoleImplementer)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "fixed the red", Commit: "handoff-2"}))
	assertIssuePhase(t, h, phase.Testing)
	_, secondStart := queuedStart(t, h, "LEGION-208", claim.RoleTester)
	if secondStart.Assignment == "" || secondStart.Assignment == firstTesting {
		t.Fatalf("the second testing start carries assignment %q, want a new one beside %q", secondStart.Assignment, firstTesting)
	}
	if run := issueGeneration(t, h, "LEGION-208"); run != 1 {
		t.Fatalf("the issue is on run %d, want 1: the run never moved, which is why it cannot refuse this completion", run)
	}

	// The old turn reports its completion. Refused, with nothing recorded, and the call made again
	// at the same commit is refused the same way rather than answered ALREADY_RECORDED.
	complete := func() string {
		t.Helper()
		recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "test-1"})
		return refused(t, recorder, http.StatusConflict, "HANDOFF_STALE_ASSIGNMENT")
	}
	before := recordedCompletions(t, h, claim.RoleTester)
	message := complete()
	if !strings.Contains(message, "testing") || !strings.Contains(message, "nothing was recorded") {
		t.Fatalf("the refusal is %q, want it to name the phase handed out again and say nothing was recorded", message)
	}
	// What the refusal tells the worker to do: the second testing assignment is for the head the
	// implementer's fix pushed, so this turn's verdict on the head before it is no answer for it.
	// The worker does that task's work and writes a handoff of its own, and the refusal never
	// sends this completion's commit again.
	if !strings.Contains(message, "a handoff of its own") {
		t.Fatalf("the refusal is %q, want it to tell the worker to complete the new task with a handoff of its own", message)
	}
	if strings.Contains(message, "at the same commit") {
		t.Fatalf("the refusal is %q, want it never to offer this completion's commit again: the new assignment's work is its own", message)
	}
	complete()
	if got := recordedCompletions(t, h, claim.RoleTester); got != before {
		t.Fatalf("the refused completions left %d recorded tester completions, want the %d there were: a refusal before the fact records nothing", got, before)
	}
	for _, fact := range facts.recorded() {
		if completed, ok := fact.(intake.HandoffComplete); ok && completed.Role == claim.RoleTester {
			t.Fatalf("the workflow saw the tester's completion %#v, want the route to have refused it", completed)
		}
	}
	assertIssuePhase(t, h, phase.Testing)

	// The second assignment's task arrives; the tester tests the fixed head, writes the handoff
	// that task's work calls for, and completes in its turn, which is applied and moves the issue
	// on to reviewing.
	second := takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)
	if second != secondStart.Assignment {
		t.Fatalf("the tester took assignment %q, want the start's %q", second, secondStart.Assignment)
	}
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass on the fix", Verdict: "pass", Commit: "test-2"}))
	assertIssuePhase(t, h, phase.Reviewing)
	testers := 0
	for _, fact := range facts.recorded() {
		if completed, ok := fact.(intake.HandoffComplete); ok && completed.Role == claim.RoleTester {
			testers++
		}
	}
	if testers != 1 {
		t.Fatalf("the workflow saw %d tester completions, want the one the new assignment reported", testers)
	}
}

// The comparison is made only when both sides name an assignment, which is what carries a daemon
// upgraded in place: every worker live at the release has an empty value on both sides, and the
// first start each role is given stamps both at once. A row a re-admission cleared
// (record.ClearGeneration) and a claim that has taken no task carrying one are the one-sided cases,
// and each is accepted — the fence is the pair, never one half of it.
func TestACompletionIsHeldToItsAssignmentOnlyWhenBothSidesNameOne(t *testing.T) {
	for _, tc := range []struct {
		name    string
		row     string
		serving string
		refused bool
	}{
		{name: "a daemon upgraded in place: neither side names one"},
		{name: "the row names one, the claim's task was given before the upgrade", row: "A2"},
		{name: "a re-admission cleared the row, the claim serves its task's", serving: "A1"},
		{name: "both sides name the assignment the phase handed out", row: "A1", serving: "A1"},
		{name: "the claim serves the assignment before this phase's", row: "A2", serving: "A1", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			h, facts := newHandoffHarness(t, github, nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Testing)
			if tc.row != "" {
				seedAssignment(t, h, "LEGION-208", claim.RoleTester, tc.row)
			}
			tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
			if tc.serving != "" {
				takeTask(t, h, tester.token, "outbox:77", "test it again", tc.serving, 1)
			}
			recorder := h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: headSHA})
			if tc.refused {
				refused(t, recorder, http.StatusConflict, "HANDOFF_STALE_ASSIGNMENT")
				if got := facts.recorded(); len(got) != 0 {
					t.Fatalf("facts after the refusal = %#v, want none applied", got)
				}
				return
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("the completion = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
		})
	}
}

// A held phase's retry is the phase entry the worker is already on, resumed: the issue never left
// it, and the work is the work it had. The tester's claim fails, the issue is held with that
// claim still holding the task of its testing assignment, and the architect's retry puts the
// issue back in testing. The outbox hands a relaunched claim no second copy of the task it still
// works (daemon's outbox, "not delivered again"), so the worker goes on serving the assignment
// the held phase gave it: the retry's start carries that same assignment, the row keeps it, and
// the completion the worker reports is applied. A retry that minted a new one would name an
// assignment no start ever delivers, and every completion of that phase would be refused
// HANDOFF_STALE_ASSIGNMENT until something outside the workflow moved the issue.
func TestAHeldPhasesRetryKeepsTheAssignmentItsWorkerStillServes(t *testing.T) {
	h, engine, facts := newAssignmentHarness(t)
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}, Required: []string{"pytest"}})
	}); err != nil {
		t.Fatalf("record the pull request: %v", err)
	}

	// The implementer's round ends and the tester takes the task of the testing phase's assignment.
	implementer := newLiveClaim(t, h, "LEGION-208", claim.RoleImplementer)
	completion(t, h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: "handoff-1"}))
	assertIssuePhase(t, h, phase.Testing)
	tester := newLiveClaim(t, h, "LEGION-208", claim.RoleTester)
	testing := takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)

	// The tester's claim fails — a budget ran out — and the issue is held. The claim keeps the task
	// it was working, and the turn it is in goes on serving that task's assignment.
	applyAssignmentFact(t, h, engine, "claim-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleTester, Reason: "the launch budget ran out"})
	assertIssuePhase(t, h, phase.Held)

	// The architect retries the hold: testing again, with the same work, so the start hands the
	// tester the assignment its row already names rather than minting one nothing would deliver.
	applyAssignmentFact(t, h, engine, "retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision})
	assertIssuePhase(t, h, phase.Testing)
	_, retry := queuedStart(t, h, "LEGION-208", claim.RoleTester)
	if retry.Assignment != testing {
		t.Fatalf("the retry's start carries assignment %q, want the %q the held phase handed the tester: a new one reaches the worker that still holds the old task", retry.Assignment, testing)
	}
	if got := rowAssignment(t, h, "LEGION-208", claim.RoleTester); got != testing {
		t.Fatalf("after the retry the tester's row names assignment %q, want the %q its phase handed out", got, testing)
	}

	// The relaunched worker finishes the task it held, and its completion is applied: the issue
	// moves on to reviewing, and the workflow saw exactly that one tester completion.
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "test-1"}))
	assertIssuePhase(t, h, phase.Reviewing)
	testers := 0
	for _, fact := range facts.recorded() {
		if completed, ok := fact.(intake.HandoffComplete); ok && completed.Role == claim.RoleTester {
			testers++
		}
	}
	if testers != 1 {
		t.Fatalf("the workflow saw %d tester completions, want the one the retried phase reported", testers)
	}
}

// newAssignmentHarness serves the daemon's routes with the workflow deciding every fact they
// apply, over a real Postgres and the GitHub stand-in a completing implementer reads. It answers
// the engine the test applies its own facts with — a settlement, a failed claim, an architect's
// retry — and the recorder of every fact the routes applied.
func newAssignmentHarness(t *testing.T) (*harness, *workflow.Engine, *factRecorder) {
	t.Helper()
	h := newHarness(t)
	github := newHandoffGitHub(t)
	facts := &factRecorder{}
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, ReviewRoundCap: 3, MaxFixAttempts: 3},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: record.NewStore(),
		Handlers: []intake.Handler{engine, facts}, Dispatch: &statusRecorder{},
		Tokens: handoffTokens{}, GitHubAPI: github.url,
	}).Handler
	return h, engine, facts
}

// applyAssignmentFact applies fact to the workflow under id, as the route that carries it would,
// and fails the test when the engine refuses it: every fact these tests apply is one it takes.
func applyAssignmentFact(t *testing.T, h *harness, engine *workflow.Engine, id string, fact intake.Fact) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), h.store.Pool(), "test", id, fact, engine)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply %s (%T) = %+v, %v; want it taken", id, fact, result.Refusal, err)
	}
}

// rowAssignment is the assignment role's phases row on issue names now.
func rowAssignment(t *testing.T, h *harness, issue string, role claim.Role) string {
	t.Helper()
	var assignment string
	if err := h.store.Pool().QueryRow(context.Background(),
		"select assignment from phases where issue = $1 and role = $2", issue, string(role)).Scan(&assignment); err != nil {
		t.Fatalf("read the %s's assignment on %s: %v", role, issue, err)
	}
	return assignment
}

// assertIssuePhase asserts where LEGION-208's record stands now.
func assertIssuePhase(t *testing.T, h *harness, want phase.Phase) {
	t.Helper()
	var got phase.Phase
	if err := h.store.Pool().QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&got); err != nil {
		t.Fatalf("read the phase: %v", err)
	}
	if got != want {
		t.Fatalf("the issue is in %s, want %s", got, want)
	}
}

// seedAssignment stamps role's phases row on issue with assignment, as the start that handed that
// role its task does.
func seedAssignment(t *testing.T, h *harness, issue string, role claim.Role, assignment string) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return record.NewStore().PutPhase(ctx, tx, record.PhaseRow{Issue: issue, Role: role, Claim: claim.Token(string(role) + "-claim"), Assignment: assignment})
	}); err != nil {
		t.Fatalf("stamp the %s's assignment on %s: %v", role, issue, err)
	}
}

// queuedStart is the newest start the outbox holds for role on issue, with its row id: the row the
// outbox runner would deliver next.
func queuedStart(t *testing.T, h *harness, issue string, role claim.Role) (int64, record.SuperviseRequest) {
	t.Helper()
	var id int64
	var payload []byte
	err := h.store.Pool().QueryRow(context.Background(), `select id, payload from outbox
		where kind = 'supervise' and issue = $1 and payload->>'op' = 'start' and payload->>'role' = $2
		order by id desc limit 1`, issue, string(role)).Scan(&id, &payload)
	if err != nil {
		t.Fatalf("read the %s's queued start on %s: %v", role, issue, err)
	}
	var request record.SuperviseRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("decode the %s's start: %v", role, err)
	}
	return id, request
}

// takeStart hands the claim the newest start queued for its role, as the outbox runner does, and
// answers the assignment that start carries. The turn the claim is in ends first, which retires
// the task it held: the outbox's delivery waits for that anyway, since a machine refuses a second
// delivery while one is pending.
func takeStart(t *testing.T, h *harness, token claim.Token, issue string, role claim.Role) string {
	t.Helper()
	row, start := queuedStart(t, h, issue, role)
	if start.Assignment == "" {
		t.Fatalf("the %s's start on %s carries no assignment: %+v", role, issue, start)
	}
	takeTask(t, h, token, fmt.Sprintf("outbox:%d", row), start.Task, start.Assignment, start.Generation)
	return start.Assignment
}

// takeTask ends whatever turn the claim is in, which retires the task it held, and gives it task
// under assignment in a turn of its own: a worker taking the next phase's work.
func takeTask(t *testing.T, h *harness, token claim.Token, id, task, assignment string, generation uint64) {
	t.Helper()
	ctx := context.Background()
	machine, ok := h.supervisor.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if machine.Claim().State == supervise.StateWorking {
		if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
			t.Fatalf("end the turn of %s: %v", token, err)
		}
	}
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: task, ID: id,
		Generation: generation, Assignment: assignment}); err != nil {
		t.Fatalf("deliver %s to %s: %v", id, token, err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token, DeliveryID: id}); err != nil {
		t.Fatalf("start the turn of %s: %v", token, err)
	}
}

// recordedCompletions is how many completions of role on LEGION-208 are recorded as processed.
func recordedCompletions(t *testing.T, h *harness, role claim.Role) int {
	t.Helper()
	var stored int
	if err := h.store.Pool().QueryRow(context.Background(),
		"select count(*) from processed_events where event_id like $1", "handoff:LEGION-208:%:"+string(role)+":%").Scan(&stored); err != nil {
		t.Fatalf("count the recorded %s completions: %v", role, err)
	}
	return stored
}
