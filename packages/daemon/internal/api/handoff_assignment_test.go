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
// HANDOFF_STALE_ASSIGNMENT before any fact is applied, and the completion of the second assignment,
// in that task's own turn, is taken.
func TestAStaleAssignmentsCompletionDoesNotTakeTheNextTestingPhases(t *testing.T) {
	h := newHarness(t)
	github := newHandoffGitHub(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: testProject, Repositories: testRepositories, ReviewRoundCap: 3, MaxFixAttempts: 3},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	facts := &factRecorder{}
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Grants: credential.New(nil), Pool: h.store.Pool(), Record: records,
		Handlers: []intake.Handler{engine, facts}, Dispatch: &statusRecorder{},
		Tokens: handoffTokens{}, GitHubAPI: github.url,
	}).Handler
	ctx := context.Background()
	h.recordIssue(record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: testProject, Title: "LEGION-208", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets",
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

	// The old turn reports its completion. Refused, with nothing recorded, and the retry at the
	// same commit is refused the same way rather than answered ALREADY_RECORDED.
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

	// The second assignment's task arrives; the same report in its turn is applied, and the issue
	// moves on to reviewing.
	second := takeStart(t, h, tester.token, "LEGION-208", claim.RoleTester)
	if second != secondStart.Assignment {
		t.Fatalf("the tester took assignment %q, want the start's %q", second, secondStart.Assignment)
	}
	completion(t, h.complete(HandoffCompleteRequest{GrantID: tester.grant(t), Summary: "tests pass", Verdict: "pass", Commit: "test-1"}))
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
