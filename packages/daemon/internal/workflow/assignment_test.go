package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every start that hands a role a task mints that role an assignment, writes it on the role's
// phases row and carries it on the start's payload, both in the transition's own transaction. The
// issue's run cannot do this work: one run holds every phase entry, so the testing phase the
// tester is given after a red and an implementing round has to be told from the testing phase
// before it by something the run does not carry (api/handoff.go, HANDOFF_STALE_ASSIGNMENT).
func TestEveryPhaseStartMintsTheRoleAnAssignmentOnItsRowAndItsPayload(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}, Required: []string{"pytest"}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	engine := testEngine(config.DesignGateRootIssues, nil)
	apply := func(eventID string, fact intake.Fact) {
		t.Helper()
		result, err := intake.ApplyFact(ctx, pool, "fact", eventID, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
		if result.Refusal != nil {
			t.Fatalf("ApplyFact %s refused: %+v", eventID, *result.Refusal)
		}
	}
	red := func(eventID, head string) {
		t.Helper()
		apply(eventID, intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
			CheckRuns: []record.AttemptRun{{Name: "pytest", ID: 7}}, Generation: 1, Snapshot: eventID, Failing: []string{"pytest"}})
	}

	// The red ends testing and starts the implementer, which is the first start of this test.
	red("red-1", "head")
	assertPhase(t, pool, phase.Implementing)
	firstImplementing := assignmentOfStart(t, pool, claim.RoleImplementer, 1)

	// The implementer's completion sends the issue to testing and starts the tester.
	apply("implement-1", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "fix-1"})
	assertPhase(t, pool, phase.Testing)
	firstTesting := assignmentOfStart(t, pool, claim.RoleTester, 1)
	if firstTesting == firstImplementing {
		t.Fatalf("the tester's assignment is the implementer's, %q: each role's start mints its own", firstTesting)
	}
	// The implementer's own assignment is its row's still: a completion keeps it, and only its next
	// start replaces it.
	if got := assignmentOfRow(t, pool, claim.RoleImplementer); got != firstImplementing {
		t.Fatalf("after its completion the implementer's row carries %q, want the %q its start wrote", got, firstImplementing)
	}

	// The tester fails this round and the issue goes back to implementing, where the implementer's
	// second start mints an assignment of its own; its completion gives the tester a second one.
	apply("test-1", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Verdict: "fail", Commit: "test-1"})
	assertPhase(t, pool, phase.Implementing)
	secondImplementing := assignmentOfStart(t, pool, claim.RoleImplementer, 2)
	if secondImplementing == firstImplementing {
		t.Fatalf("the implementer's second start carries its first assignment %q, want a new one: nothing would tell the two implementing phases apart", secondImplementing)
	}
	apply("implement-2", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "fix-2"})
	assertPhase(t, pool, phase.Testing)
	secondTesting := assignmentOfStart(t, pool, claim.RoleTester, 2)
	if secondTesting == firstTesting {
		t.Fatalf("the tester's second start carries its first assignment %q, want a new one: nothing would tell the two testing phases apart", secondTesting)
	}
	// The issue never left run 1, which is why the run alone cannot tell these two testing phases
	// apart.
	var generation uint64
	if err := pool.QueryRow(ctx, "select generation from issues where key = 'LEGION-208'").Scan(&generation); err != nil {
		t.Fatalf("read the issue's run: %v", err)
	}
	if generation != 1 {
		t.Fatalf("the issue is on run %d, want 1: the test only means something while the run stands still", generation)
	}
}

// A resume hands no new task: it is the task the worker already holds, said again for a session
// that may have forgotten it (ResumePhaseTask). It mints no assignment, so the row keeps the one
// the phase's own start wrote and the resumed worker's in-flight completion is never refused.
func TestTheBootsResumeMintsNoAssignment(t *testing.T) {
	pool := migratedPool(t)
	ctx := t.Context()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Assignment: "A1"})
	token, err := claim.NewToken(project, "LEGION-208", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, 'LEGION-208', 'LEGION-208', 'implementer', 1, 'ses', '/sessions/ses.jsonl', 'suspended', 0, 0, 0, 0, 'daemon')`,
		string(token), project); err != nil {
		t.Fatalf("seed the suspended implementer: %v", err)
	}

	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	starts := superviseRowsOf(t, pool, "LEGION-208", "start")
	if len(starts) != 1 || !starts[0].ResumeTask {
		t.Fatalf("the boot queued %+v, want the one resume start", starts)
	}
	if starts[0].Assignment != "" {
		t.Fatalf("the resume start carries assignment %q, want none: it hands the worker no new task", starts[0].Assignment)
	}
	if got := assignmentOfRow(t, pool, claim.RoleImplementer); got != "A1" {
		t.Fatalf("after the resume the implementer's row carries %q, want the A1 its phase's start wrote", got)
	}
}

// A production check's look starts the implementer after the close, on work that is no phase: the
// issue has no phase to hand out, the report is held to the check and its look instead, and the
// start mints no assignment. So the implementer's claim goes on serving whatever assignment its
// last phase gave it, and nothing of the check is weighed against a phases row.
func TestAProductionChecksLookMintsNoAssignment(t *testing.T) {
	rig := childCheckRig(t, "")
	starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
	if len(starts) == 0 {
		t.Fatal("no start for the check's first look")
	}
	look := starts[len(starts)-1]
	if look.CheckLook != 1 {
		t.Fatalf("the newest start is %+v, want the check's look 1", look)
	}
	if look.Assignment != "" {
		t.Fatalf("the look's start carries assignment %q, want none: a look is no phase", look.Assignment)
	}
}

// assignmentOfStart is the assignment the nth start of role carries, counted from the oldest.
func assignmentOfStart(t *testing.T, pool *pgxpool.Pool, role claim.Role, nth int) string {
	t.Helper()
	var of []record.SuperviseRequest
	for _, request := range superviseRowsOf(t, pool, "LEGION-208", "start") {
		if request.Role == role {
			of = append(of, request)
		}
	}
	if len(of) < nth {
		t.Fatalf("the outbox holds %d starts of the %s, want at least %d", len(of), role, nth)
	}
	start := of[nth-1]
	if start.Assignment == "" {
		t.Fatalf("start %d of the %s carries no assignment: %+v", nth, role, start)
	}
	if row := assignmentOfRow(t, pool, role); row != start.Assignment {
		t.Fatalf("start %d of the %s carries assignment %q while its row carries %q; the two are written in one transaction and must agree",
			nth, role, start.Assignment, row)
	}
	return start.Assignment
}

// assignmentOfRow is the assignment role's phases row carries on LEGION-208.
func assignmentOfRow(t *testing.T, pool *pgxpool.Pool, role claim.Role) string {
	t.Helper()
	var assignment string
	if err := pool.QueryRow(context.Background(),
		"select assignment from phases where issue = 'LEGION-208' and role = $1", string(role)).Scan(&assignment); err != nil {
		t.Fatalf("read the %s's assignment: %v", role, err)
	}
	return assignment
}
