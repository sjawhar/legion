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
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
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
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
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
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", log: quietLogger(), now: time.Now}
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
		{"plan", intake.HandoffComplete{Generation: 2, Issue: key, Role: claim.RolePlanner, Summary: "plan", Commit: "plan-1"}},
		{"implement", intake.HandoffComplete{Generation: 2, Issue: key, Role: claim.RoleImplementer, Summary: "impl", Commit: "impl-1"}},
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
	if _, err := intake.ApplyFact(ctx, pool, "api", "tester-pass", intake.HandoffComplete{Generation: 1, Issue: "LEGION-209", Role: claim.RoleTester, Summary: "pass", Verdict: "pass", Commit: "test-1"}, engine, admission); err != nil {
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

// A backward move between two of one role's phases (the implementer's retro back to implementing)
// hands the running worker the new phase. No suspend may stop that worker once it has the new
// phase: a stopped worker's new task is retired with it, and nothing would ever resume it, since
// the phase waits for that worker's handoff. Here the runtime cannot stop a pane at first, so a
// suspend queued by the move would fail and be retried; the worker finishes its retro turn and
// takes whatever task it is sent; then the runtime recovers and every row runs to its end, and the
// worker is still running with the implementing task.
func TestASameRoleBackwardMoveNeverStopsTheWorkerInItsNewPhase(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Retro, Generation: 1, Status: "retro", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RoleImplementer, Claim: "claim"})
	}); err != nil {
		t.Fatal(err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds // exactly what the daemon wires
	clock := time.Now()
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "retro"}}, notices: &outboxPublisher{}, handlers: []intake.Handler{engine},
		log: quietLogger(), now: func() time.Time { return clock },
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	// takeTask is the agent starting a turn on a task it was sent and has not yet taken.
	takeTask := func() {
		t.Helper()
		machine, _ := sup.Machine(token)
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if p := machine.Claim().Pending; p != nil && !p.DeliveredAt.IsZero() && p.ConfirmedAt.IsZero() {
				if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token, DeliveryID: p.ID}); err != nil {
					t.Fatalf("start the turn: %v", err)
				}
				return
			}
		}
	}
	// boot is a launched process's agent coming up: its shim's hello, its registration, ready.
	boot := func() {
		t.Helper()
		machine, _ := sup.Machine(token)
		generation := machine.Claim().Generation
		for _, ev := range []supervise.Event{
			supervise.StreamHello{Claim: token, Generation: generation},
			supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl"},
			supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-impl"},
		} {
			if err := machine.Handle(ctx, ev); err != nil {
				t.Fatalf("handle %T: %v", ev, err)
			}
		}
	}

	// The implementer works on its retro.
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
		Generation: 1, Phase: phase.Retro, Task: "Continue Workflow. Issue: LEGION-208. Phase: retro."}, clock)); err != nil {
		t.Fatalf("start the implementer's retro: %v", err)
	}
	boot()
	takeTask()
	machine, _ := sup.Machine(token)
	if got := machine.Claim().State; got != supervise.StateWorking {
		t.Fatalf("implementer = %s, want working on its retro", got)
	}

	// It asks to go back to implementing while the runtime cannot stop its pane.
	rt.FailSuspend(errors.New("the pane did not stop"))
	if _, err := intake.ApplyFact(ctx, pool, "api", "retro-back", intake.BackwardMove{Issue: issue.Key, Requester: claim.RoleImplementer, To: phase.Implementing, Reason: "rework"}, engine); err != nil {
		t.Fatalf("move back to implementing: %v", err)
	}
	run := func() {
		t.Helper()
		clock = clock.Add(10 * time.Minute)
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
		takeTask()
	}
	run()
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("end the retro turn: %v", err)
	}
	run()

	// The runtime recovers, and every row runs to its end.
	rt.FailSuspend(nil)
	run()
	if state := machine.Claim().State; state == supervise.StateLaunching {
		boot()
		takeTask()
	}
	run()

	if left := unfinishedSuperviseRows(t, pool); left != 0 {
		t.Fatalf("unfinished supervise rows = %d, want every row run to its end", left)
	}
	final := machine.Claim()
	if final.State == supervise.StateSuspended || final.Pending == nil || final.Pending.Phase != phase.Implementing {
		t.Fatalf("implementer = %s holding %+v in implementing, want it running with the implementing task: a suspended worker holding no task is never resumed", final.State, final.Pending)
	}
}

// A transition's suspend, stamped with the phase it ends, is stale once the issue is back in a phase
// its role works: the role was handed its work again, and a suspend retried from before that
// return must not stop it. Here the implementer's suspend from the move to testing failed and backed
// off; the tester failed the change, and the implementer is ready in implementing again when the old
// suspend comes due.
func TestASuspendFromBeforeItsRoleWasHandedWorkAgainNeverActs(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatal(err)
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl"},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-impl"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", log: quietLogger(), now: time.Now}

	stale := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "suspend", Tree: issue.Tree, Role: claim.RoleImplementer, Generation: 1, Leaves: phase.Implementing}, time.Now())
	if err := runner.execute(ctx, stale); err != nil {
		t.Fatalf("the stale suspend = %v, want it finished", err)
	}
	if got := machine.Claim().State; got != supervise.StateReady {
		t.Fatalf("implementer = %s after a suspend from before it was handed implementing again, want still ready", got)
	}
}

// A stop ends the run it was written for. It is retried until the runtime takes it — a pane that
// will not stop can refuse for minutes — so a retry can land long after the start that replaced
// that run has delivered its task. Acting then suspends the run the start began and retires its
// task, leaving the phase with nobody in it. The claim records the newest start run against it,
// so the stop knows it is superseded whatever the timing was: no ordering, no window.
func TestAStopSupersededByANewerStartNeverActs(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 2, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RolePlanner, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatal(err)
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-plan", SessionFile: "/tmp/plan.jsonl"},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-plan"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	now := time.Now().UTC()
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", log: quietLogger(), now: func() time.Time { return now }}

	// The re-entry's stop, refused by the runtime for as long as it likes, and then the start of
	// the run that replaces it.
	rt.FailSuspend(errors.New("the pane will not stop"))
	stop := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "suspend", Tree: issue.Tree, Role: claim.RolePlanner, Generation: 2}, now)
	stop.ID = 41
	if err := runner.execute(ctx, stop); err == nil {
		t.Fatal("the stop's first attempt succeeded, want the runtime's refusal")
	}
	start := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RolePlanner, Generation: 2, Phase: phase.Planning, Task: "plan it again"}, now)
	start.ID = stop.ID + 1
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("the new run's start: %v", err)
	}

	// The runtime recovers and the stop is retried. It is the old run's, and the old run is over.
	rt.FailSuspend(nil)
	if err := runner.execute(ctx, stop); err != nil {
		t.Fatalf("the retried stop = %v, want it finished as superseded", err)
	}

	if got := machine.Claim().State; got == supervise.StateSuspended {
		t.Error("the superseded stop suspended the run that replaced it")
	}
	if p := machine.Claim().Pending; p == nil || p.Task != "plan it again" {
		t.Fatalf("pending after the retried stop = %+v, want the new run's task kept", p)
	}
	if suspends := len(rt.CallsOf("Suspend")); suspends != 1 {
		t.Errorf("Suspend calls = %d, want the one refused attempt", suspends)
	}

	// The process that survived is the one launched before the re-entry, and it is now serving the
	// new run's task: Spawn once, Resume never. So the completion it reports for that task belongs
	// to the new run, and the one it would have reported for the task it held before does not —
	// which is what the delivery's generation says, and the pane's own environment cannot.
	if spawns, resumes := len(rt.CallsOf("Spawn")), len(rt.CallsOf("Resume")); spawns != 1 || resumes != 0 {
		t.Fatalf("spawns=%d resumes=%d, want the pane launched before the re-entry still serving", spawns, resumes)
	}
	if pending := machine.Claim().Pending; pending.Generation != 2 {
		t.Fatalf("the task being served names generation %d, want the new run's 2", pending.Generation)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token, DeliveryID: machine.Claim().Pending.ID}); err != nil {
		t.Fatalf("start the new task's turn: %v", err)
	}
	if pending := machine.Claim().Pending; pending == nil || pending.ConfirmedAt.IsZero() || pending.Generation != 2 {
		t.Fatalf("the confirmed delivery is %+v, want the new run's task: a completion now is generation 2's", pending)
	}
}

// A root a human moves out of the workflow lingers: only the root is parked in done, and every
// claim of every tree member is suspended, the root's own worker and its children's. A child keeps
// its phase, so its running worker is one whose role works the child's phase, and its suspend must
// act all the same: nothing else stops that worker before the linger's tree close, and a completion
// from it would still advance the child inside a tree the human closed. The root's worker is
// suspended with the root in done, a phase no role works. Found by #1347's review, whose test this
// extends.
func TestARootLeavingTheWorkflowSuspendsEveryWorkerOfItsTree(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	parent := root.Key
	child := record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Parent: &parent, Title: "Child", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, root)
	putOutboxIssue(t, pool, records, child)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	// ready is a worker of issue running and ready, as it is between two of its turns.
	ready := func(issue string) *supervise.Machine {
		t.Helper()
		token, err := claim.NewToken("legion", issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: root.Tree, Issue: issue, Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
			t.Fatal(err)
		}
		generation := machine.Claim().Generation
		for _, ev := range []supervise.Event{
			supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-" + issue, SessionFile: "/tmp/" + issue + ".jsonl"},
			supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-" + issue},
		} {
			if err := machine.Handle(ctx, ev); err != nil {
				t.Fatalf("handle %T: %v", ev, err)
			}
		}
		return machine
	}
	rootWorker, childWorker := ready(root.Key), ready(child.Key)
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", log: quietLogger(), now: time.Now,
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: root.Key, Status: "backlog"}}, notices: &outboxPublisher{}, handlers: []intake.Handler{engine}}

	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "ev-backlog", intake.DispatchIssue{Key: root.Key, Seq: 6, Type: "issue.updated", Status: "backlog", Title: root.Title, Rank: "U"}, engine); err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := childWorker.Claim().State; got != supervise.StateSuspended {
		t.Errorf("child implementer = %s after its root moved to backlog, want suspended", got)
	}
	if got := rootWorker.Claim().State; got != supervise.StateSuspended {
		t.Errorf("root implementer = %s after its root moved to backlog, want suspended", got)
	}
}

// heldImplementer is a claim held after its agent kept dying in a turn of its task: failed, its
// session kept, and the task it was given still pending, marked interrupted.
func heldImplementer(t *testing.T, sup *supervisor, issue record.Issue, kept supervise.Delivery) *supervise.Machine {
	t.Helper()
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateFailed,
		Generation: 3, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl", Budgets: supervise.Budgets{Deaths: 3}, Pending: &kept,
	}, "")
	if err != nil {
		t.Fatalf("create the held implementer: %v", err)
	}
	return machine
}

// retryHeldPhase is the start the architect's retry of the held phase writes for the implementer.
func retryHeldPhase(t *testing.T, issue record.Issue) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
		Task: "Continue Workflow. Reason: retry held phase.", Phase: phase.Implementing, Generation: issue.Generation}, time.Now())
	row.ID = 92
	return row
}

// A claim held after its agent kept dying in a turn of its task keeps that task, and the retry's
// relaunch sends it, behind the sentence saying its turn was interrupted. The architect's retry
// writes a start of its own for the same phase and run: its task would come behind the kept one as
// a second prompt for work already under way, so the row relaunches the claim and delivers nothing.
func TestARetryOfAClaimThatKeptItsPhasesTaskDeliversNothingMore(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	kept := supervise.Delivery{ID: "kept-task", Task: "Continue Workflow. Issue: LEGION-208. Phase: implementing.",
		Phase: phase.Implementing, Generation: issue.Generation, QueuedAt: time.Now(), Interrupted: true}
	machine := heldImplementer(t, sup, issue, kept)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets")}

	if err := runner.execute(context.Background(), retryHeldPhase(t, issue)); err != nil {
		t.Fatalf("start the held claim: %v", err)
	}
	got := machine.Claim()
	if got.State != supervise.StateLaunching || got.Budgets != (supervise.Budgets{}) || len(rt.CallsOf("Resume")) != 1 {
		t.Fatalf("retried claim = %+v after %d resumes, want its session relaunched once with fresh budgets", got, len(rt.CallsOf("Resume")))
	}
	if got.Pending == nil || got.Pending.ID != kept.ID || got.Pending.Task != kept.Task {
		t.Fatalf("retried claim pending %+v, want the kept task alone", got.Pending)
	}
}

// A held claim can keep a confirmed task only when the write retiring it failed: its turn is over,
// and the relaunch's first decision retires it. The retry's own task then goes in its place, where
// a check made before the relaunch, seeing the task still held, would relaunch the claim with
// nothing to do.
func TestARetryOfAClaimWhoseKeptTasksTurnWasOverDeliversItsTask(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	kept := supervise.Delivery{ID: "kept-task", Task: "Continue Workflow. Issue: LEGION-208. Phase: implementing.",
		Phase: phase.Implementing, Generation: issue.Generation, QueuedAt: time.Now(), DeliveredAt: time.Now(), ConfirmedAt: time.Now()}
	machine := heldImplementer(t, sup, issue, kept)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets")}

	if err := runner.execute(context.Background(), retryHeldPhase(t, issue)); err != nil {
		t.Fatalf("start the held claim: %v", err)
	}
	if got := machine.Claim().Pending; got == nil || got.ID != "outbox:92" {
		t.Fatalf("retried claim pending %+v, want the retry's own task", got)
	}
}

// A kept task of another phase or another run is not the retry's work, so the start still hands
// the claim its own task: the delivery waits behind the kept task (the claim refuses a second
// pending one), and the row is retried until it goes.
func TestARetryOfAClaimThatKeptOtherWorkStillDeliversItsTask(t *testing.T) {
	for name, edit := range map[string]func(*supervise.Delivery){
		"another phase": func(d *supervise.Delivery) { d.Phase = phase.Planning },
		"another run":   func(d *supervise.Delivery) { d.Generation = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 2, Status: "in_progress"}
			putOutboxIssue(t, pool, records, issue)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			kept := supervise.Delivery{ID: "kept-task", Task: "an earlier task", Phase: phase.Implementing, Generation: issue.Generation, QueuedAt: time.Now()}
			edit(&kept)
			heldImplementer(t, sup, issue, kept)
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets")}

			err := runner.execute(context.Background(), retryHeldPhase(t, issue))

			if !errors.Is(err, supervise.ErrDeliveryPending) {
				t.Fatalf("start the held claim = %v, want its own task waiting behind the kept one (%v)", err, supervise.ErrDeliveryPending)
			}
		})
	}
}
