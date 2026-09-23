package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A supervise row serves one generation of its issue. A held tree (its implementer's claim failed)
// is closed by a human, whose backlog lingers it and queues a suspend for every claim, then set
// back to todo before those rows run. Generation 2 starts the implementer again; when the
// generation-1 rows run, they must finish without acting, never suspending the new generation's
// worker (nothing would restart it: the phase waits for that worker's handoff).
func TestAnEarlierGenerationsSuperviseRowNeverActsOnTheNextGeneration(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	from := phase.Implementing
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Held, HeldFrom: &from,
		Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, root)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("put slot: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", root.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: root.Key, Issue: root.Key, Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")}, fake.SpawnResult{Err: errors.New("pane launch failed")})
	_ = machine.Handle(ctx, supervise.RequestSpawn{Claim: token})
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("implementer = %s, want failed", got)
	}
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, 2, "LEGION", quietLogger())
	handlers := []intake.Handler{engine, admission}
	client := &outboxDispatch{issue: dispatch.Issue{Key: root.Key, Status: "todo"}}
	runner := &outbox{
		pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
		dispatch: client, notices: &outboxPublisher{}, handlers: handlers, log: quietLogger(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{}, nil
		},
		remove: func(context.Context, workspace.Workspace) error { return nil },
	}

	for _, observed := range []intake.DispatchIssue{
		{Key: root.Key, Seq: 6, Type: "issue.updated", Status: "backlog", Title: root.Title, Rank: "U"},
		{Key: root.Key, Seq: 7, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: "U"},
	} {
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", "ev-"+observed.Status, observed, handlers...); err != nil {
			t.Fatalf("%s: %v", observed.Status, err)
		}
	}
	start := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Key, Role: claim.RoleImplementer, Task: "Continue. Phase: implementing.", Generation: 2}, time.Now())
	start.ID = 9001
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("start the implementer in generation 2: %v", err)
	}
	relaunched := machine.Claim()
	if err := machine.Handle(ctx, supervise.RequestRegister{Claim: token, Generation: relaunched.Generation, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := machine.Handle(ctx, supervise.RequestReady{Claim: token, Generation: relaunched.Generation, Session: "ses-impl"}); err != nil {
		t.Fatalf("ready: %v", err)
	}

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the generation-1 linger rows: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateReady {
		t.Fatalf("generation 2's implementer is %s after generation 1's linger rows ran, want ready", got)
	}
	if left := unfinishedSuperviseRows(t, pool); left != 0 {
		t.Fatalf("unfinished supervise rows = %d, want generation 1's finished without acting", left)
	}
}

// A suspend of a claim that already runs nothing (failed or retired) is done: the linger's suspend
// of a held tree's failed worker finishes, instead of being refused and retried for as long as the
// daemon runs.
func TestSuspendingAClaimThatRunsNothingIsDone(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 1, Status: "backlog", Rank: "U"})
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", "LEGION-208", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")}, fake.SpawnResult{Err: errors.New("pane launch failed")})
	_ = machine.Handle(ctx, supervise.RequestSpawn{Claim: token})
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("implementer = %s, want failed", got)
	}
	runner := &outbox{pool: pool, records: records, supervisor: sup, project: "legion", log: quietLogger(), now: time.Now}
	suspend := mustOutboxRow(t, "LEGION-208", record.SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1}, time.Now())
	if err := runner.execute(ctx, suspend); err != nil {
		t.Fatalf("suspend a failed claim = %v, want done", err)
	}
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("implementer = %s after the suspend, want still failed", got)
	}
}

func unfinishedSuperviseRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = 'supervise'").Scan(&rows); err != nil {
		t.Fatalf("count supervise rows: %v", err)
	}
	return rows
}

// A tree paused while its pull request is open (root to backlog) and resumed (todo) runs its next
// generation on the same branch and the same open pull request: GitHub allows one open pull
// request per head branch, and nothing records it again. Re-admission keeps the open pull request
// (only a merged or closed one belongs to the earlier generation), so the new generation's
// implementer completion reaches testing on it.
func TestAReadmittedTreeKeepsItsOpenPullRequest(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	const key, artifact = "LEGION-208", "0b6f7c1e-4d5a-4f0e-9f59-2b1c3d4e5f60"
	approved := 1
	root := record.Issue{Key: key, Project: "LEGION", Tree: key, Title: "Workflow", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, root)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		if err := records.PutGate(ctx, tx, record.DesignGate{Issue: key, ArtifactID: artifact, LatestVersion: 1, ApprovedVersion: &approved}); err != nil {
			return err
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "acme/widgets", Number: 86, Branch: "legion/" + key, HeadSHA: "sha-1",
			HeadUpdatedAt: time.Now(), HeadUpdatedAtSource: "webhook", Failing: []string{}, FailingStatuses: []string{}, CheckRuns: []record.AttemptRun{}, FixAttempts: 2})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, 2, "LEGION", quietLogger())
	for _, step := range []struct {
		id   string
		fact intake.Fact
	}{
		{"backlog", intake.DispatchIssue{Key: key, Seq: 6, Type: "issue.updated", Status: "backlog", Title: root.Title, Rank: "U"}},
		{"todo", intake.DispatchIssue{Key: key, Seq: 7, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: "U"}},
		{"gate", intake.GateRegistered{Issue: key, ArtifactID: artifact, Version: 2}},
		{"approve", intake.DispatchArtifact{Key: key, ArtifactID: artifact, Kind: intake.DispatchArtifactApproved, Version: 2}},
		{"plan", intake.HandoffComplete{Issue: key, Role: claim.RolePlanner, Summary: "plan", Commit: "plan-1"}},
		{"implement", intake.HandoffComplete{Issue: key, Role: claim.RoleImplementer, Summary: "impl", Commit: "impl-1"}},
	} {
		if _, err := intake.ApplyFact(ctx, pool, "test", step.id, step.fact, engine, admission); err != nil {
			t.Fatalf("apply %s: %v", step.id, err)
		}
	}
	var current phase.Phase
	var generation uint64
	if err := pool.QueryRow(ctx, "select phase, generation from issues where key = $1", key).Scan(&current, &generation); err != nil {
		t.Fatal(err)
	}
	if current != phase.Testing || generation != 2 {
		t.Fatalf("generation %d is in %s after its implementer completed, want generation 2 in testing on the open pull request #86", generation, current)
	}
	var fixAttempts int
	if err := pool.QueryRow(ctx, "select fix_attempts from pull_requests where issue = $1 and number = 86", key).Scan(&fixAttempts); err != nil {
		t.Fatalf("read the kept pull request: %v", err)
	}
	if fixAttempts != 0 {
		t.Fatalf("kept pull request fix attempts = %d, want generation 1's count reset", fixAttempts)
	}
}

// A child a human moves out of the workflow (here backlog, mid-test) leaves the table: its claims
// are suspended and its phase parked, so its tester's next completion advances nothing and no
// status write lands over the human's backlog.
func TestAChildAHumanMovesOutOfTheWorkflowStopsAndKeepsTheHumansStatus(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	parent := "LEGION-208"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Title: "child", Parent: &parent, Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "V", LastDispatchSeq: 1})
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "acme/widgets", Number: 90, Branch: "legion/LEGION-209", HeadSHA: "sha",
			HeadUpdatedAt: time.Now(), HeadUpdatedAtSource: "webhook", Failing: []string{}, FailingStatuses: []string{}, CheckRuns: []record.AttemptRun{}})
	}); err != nil {
		t.Fatal(err)
	}
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, 2, "LEGION", quietLogger())

	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-backlog", intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "child", Parent: parent, Rank: "V"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	var suspends int
	if err := pool.QueryRow(ctx, "select count(*) from outbox where kind = 'supervise' and issue = 'LEGION-209' and payload->>'op' = 'suspend'").Scan(&suspends); err != nil {
		t.Fatal(err)
	}
	if suspends != len(claim.Roles) {
		t.Fatalf("suspend rows for the backlogged child = %d, want one per role (%d)", suspends, len(claim.Roles))
	}
	if _, err := pool.Exec(ctx, "delete from outbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "api", "tester-pass", intake.HandoffComplete{Issue: "LEGION-209", Role: claim.RoleTester, Summary: "pass", Verdict: "pass", Commit: "test-1"}, engine, admission); err != nil {
		t.Fatal(err)
	}
	var childPhase phase.Phase
	if err := pool.QueryRow(ctx, "select phase from issues where key = 'LEGION-209'").Scan(&childPhase); err != nil {
		t.Fatal(err)
	}
	if childPhase != phase.Done {
		t.Fatalf("backlogged child phase = %s after its tester's completion, want parked in done", childPhase)
	}
	rows, err := pool.Query(ctx, "select id, kind, issue, payload from outbox where kind = 'dispatch_status' order by id")
	if err != nil {
		t.Fatal(err)
	}
	var statusRows []record.OutboxRow
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatal(err)
		}
		statusRows = append(statusRows, row)
	}
	rows.Close()
	client := &outboxDispatch{issue: dispatch.Issue{Key: "LEGION-209", Status: "backlog"}}
	runner := &outbox{dispatch: client}
	for _, row := range statusRows {
		if err := runner.execute(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.statuses) > 0 {
		t.Fatalf("the daemon wrote %v over a child a human moved to backlog", client.statuses)
	}
}
