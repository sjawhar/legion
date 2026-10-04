package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
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
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Held, Hold: &record.Hold{From: phase.Implementing},
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
	admission := admit.New(records, engine, 2, "LEGION", quietLogger())
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
		{Key: root.Key, Seq: 7, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: "U", HandedOver: true},
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

// A worker reports its phase complete from a tool call inside its turn, and it stays resident:
// the transition that records the report starts the next role while that turn still runs, and
// stops nobody. Once the turn ends the planner keeps its process, its launch, its session and the
// capability its registration was issued. A later question reaches it — one an Envoy message
// starts a turn for, and one its architect delivers — and it answers with the issue still in
// implementing and nothing relaunched; a completion it reports for a phase it no longer works is
// refused. Only the issue's close stops it, and its session is kept.
func TestAPhaseCompletionLeavesTheWorkerResidentUntilItsIssueCloses(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds // exactly what the daemon wires
	planner, err := claim.NewToken("legion", issue.Key, claim.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RolePlanner, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(planner, conn)
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: planner}); err != nil {
		t.Fatalf("spawn the planner: %v", err)
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: planner, Generation: generation},
		supervise.RequestRegister{Claim: planner, Generation: generation, Session: "ses-plan", SessionFile: "/tmp/plan.jsonl", CapabilityHash: []byte("planner-capability")},
		supervise.RequestReady{Claim: planner, Generation: generation, Session: "ses-plan"},
		supervise.RequestDeliver{Claim: planner, Task: "Continue Workflow. Issue: LEGION-208. Phase: planning.", Phase: phase.Planning, Generation: 1},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	machine.Wait()
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: planner}); err != nil {
		t.Fatalf("start the planning turn: %v", err)
	}
	// The linger outlasts the test's clock, so the close's suspends are all the close does here.
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour}, quietLogger())
	clock := time.Now()
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "in_progress"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine}, now: func() time.Time { return clock }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	run := func(what string) {
		t.Helper()
		clock = clock.Add(time.Hour)
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run %s: %v", what, err)
		}
	}
	currentPhase := func() phase.Phase {
		t.Helper()
		var got phase.Phase
		if err := pool.QueryRow(ctx, "select phase from issues where key = $1", issue.Key).Scan(&got); err != nil {
			t.Fatalf("read the issue's phase: %v", err)
		}
		return got
	}
	// stops counts the runtime's stops and relaunches of the planner's process.
	stops := func() (suspends, releases, launches int) {
		t.Helper()
		for _, call := range rt.Calls() {
			switch {
			case call.Method == "Suspend" && call.Locator.Claim == planner:
				suspends++
			case call.Method == "Release" && call.Locator.Claim == planner:
				releases++
			case (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == planner:
				launches++
			}
		}
		return suspends, releases, launches
	}
	before := machine.Claim()
	if before.State != supervise.StateWorking || before.Locator == nil || len(before.CapabilityHash) == 0 {
		t.Fatalf("the planner before its completion is %+v, want working on a live process with its capability", before)
	}

	// Inside that turn the planner reports planning complete, and the issue moves to implementing:
	// the implementer is started while the planner's turn still runs, and no stop is queued.
	if result, err := intake.ApplyFact(ctx, pool, "api", "handoff:planner:planning", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RolePlanner, Claim: planner, Commit: "plan-1"}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("complete planning = %+v, %v", result.Refusal, err)
	}
	var queuedStops int
	if err := pool.QueryRow(ctx, "select count(*) from outbox where kind = 'supervise' and payload->>'op' <> 'start'").Scan(&queuedStops); err != nil {
		t.Fatalf("count the queued stops: %v", err)
	}
	if queuedStops != 0 {
		t.Fatalf("the completion queued %d stops, want none: the planner's assignment ends, not its process", queuedStops)
	}
	run("the transition's effects")
	implementer, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if _, started := sup.Machine(implementer); !started || currentPhase() != phase.Implementing {
		t.Fatalf("after the completion the issue is in %s with the implementer started %t, want implementing with the implementer started", currentPhase(), started)
	}
	if got := machine.Claim().State; got != supervise.StateWorking {
		t.Fatalf("while its turn still runs the planner is %s, want working", got)
	}

	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: planner}); err != nil {
		t.Fatalf("end the planning turn: %v", err)
	}
	run("the rows due after the turn")
	after := machine.Claim()
	if after.State != supervise.StateIdle || after.Generation != before.Generation || after.Session != before.Session || after.SessionFile != before.SessionFile ||
		after.Locator == nil || *after.Locator != *before.Locator || !bytes.Equal(after.CapabilityHash, before.CapabilityHash) || after.Pending != nil {
		t.Fatalf("phase completion ended or replaced the resident planner: before %+v, after %+v", before, after)
	}
	if suspends, releases, launches := stops(); suspends != 0 || releases != 0 || launches != 1 {
		t.Fatalf("the runtime suspended the planner %d times, released it %d times and launched it %d times, want once launched and never stopped", suspends, releases, launches)
	}

	// Another role's question arrives through Envoy, which starts a turn of the planner's own with no
	// delivery behind it; then its architect asks one through the daemon, a task of no phase.
	for _, ev := range []supervise.Event{supervise.StreamTurnStart{Claim: planner}, supervise.StreamTurnEnd{Claim: planner}} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("answer the Envoy question: handle %T: %v", ev, err)
		}
	}
	const question = "Why does the plan keep the old API?"
	prompted := len(conn.Prompts())
	if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: planner, Task: question}); err != nil {
		t.Fatalf("deliver the architect's question: %v", err)
	}
	testwait.Eventually(t, "the architect's question to reach the planner", func() bool { return len(conn.Prompts()) > prompted })
	if sent := conn.Prompts()[prompted]; sent.Message != question {
		t.Fatalf("the planner was sent %q, want the architect's question", sent.Message)
	}
	for _, ev := range []supervise.Event{supervise.StreamTurnStart{Claim: planner, DeliveryID: machine.Claim().Pending.ID}, supervise.StreamTurnEnd{Claim: planner}} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("answer the architect's question: handle %T: %v", ev, err)
		}
	}
	// Answering, it completes again, and the completion changes nothing.
	result, err := intake.ApplyFact(ctx, pool, "api", "handoff:planner:after", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RolePlanner, Claim: planner, Commit: "plan-2"}, engine)
	if err != nil || result.Refusal == nil || result.Refusal.Code != "HANDOFF_NOT_CURRENT_PHASE" {
		t.Fatalf("the finished planner's later completion = %+v, %v; want HANDOFF_NOT_CURRENT_PHASE", result.Refusal, err)
	}
	run("the rows due after the questions")
	answered := machine.Claim()
	if answered.State != supervise.StateIdle || answered.Generation != before.Generation || answered.Session != before.Session ||
		answered.Locator == nil || *answered.Locator != *before.Locator || answered.Pending != nil {
		t.Fatalf("after answering, the planner is %+v; want it idle on the same process and session, holding nothing", answered)
	}
	if got := currentPhase(); got != phase.Implementing {
		t.Fatalf("after the planner answered, the issue is in %s, want implementing", got)
	}
	if suspends, releases, launches := stops(); suspends != 0 || releases != 0 || launches != 1 {
		t.Fatalf("answering suspended the planner %d times, released it %d times and launched it %d times, want once launched and never stopped", suspends, releases, launches)
	}

	// A person closes the issue: its tree lingers, and the close suspends the planner, keeping its
	// session for a re-admission to resume.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "ev-done", intake.DispatchIssue{Key: issue.Key, Seq: 6, Type: "issue.closed", Status: "done", Title: issue.Title, Rank: "U"}, engine); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	run("the close's suspends")
	closed := machine.Claim()
	if suspends, _, _ := stops(); closed.State != supervise.StateSuspended || closed.Session != before.Session || closed.SessionFile != before.SessionFile || suspends != 1 {
		t.Fatalf("after the close the planner is %+v with %d suspensions, want suspended once with its session kept", closed, suspends)
	}
}

// An issue's close suspends its workers, and a worker the close finds in a turn is suspended once
// that turn ends rather than in the middle of a tool call (LEGION-283): the suspend row runs, the
// claim answers that the suspension is held, the row is retried and the worker stays working with
// no stop; once the turn ends the worker is suspended once with its session kept, and the row's
// retry finishes it.
func TestAnIssuesCloseWaitsForTheWorkersTurnToEnd(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	planner, err := claim.NewToken("legion", issue.Key, claim.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RolePlanner, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	sup.deps.Conns.(*fake.Conns).Register(planner, fake.NewConn())
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: planner}); err != nil {
		t.Fatalf("spawn the planner: %v", err)
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: planner, Generation: generation},
		supervise.RequestRegister{Claim: planner, Generation: generation, Session: "ses-plan", SessionFile: "/tmp/plan.jsonl"},
		supervise.RequestReady{Claim: planner, Generation: generation, Session: "ses-plan"},
		supervise.RequestDeliver{Claim: planner, Task: "Continue Workflow. Issue: LEGION-208. Phase: planning.", Phase: phase.Planning, Generation: 1},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	machine.Wait()
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: planner}); err != nil {
		t.Fatalf("start the planning turn: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour}, quietLogger())
	clock := time.Now()
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "done"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine}, now: func() time.Time { return clock }, log: quietLogger(),
	}
	// suspendRow is the planner's suspend rows left unfinished, and the most attempts one has had.
	suspendRow := func() (rows, attempts int) {
		t.Helper()
		if err := pool.QueryRow(ctx, "select count(*), coalesce(max(attempts), 0) from outbox where kind = 'supervise' and payload->>'op' = 'suspend' and payload->>'role' = 'planner'").
			Scan(&rows, &attempts); err != nil {
			t.Fatalf("read the planner's suspend rows: %v", err)
		}
		return rows, attempts
	}

	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "ev-done", intake.DispatchIssue{Key: issue.Key, Seq: 6, Type: "issue.closed", Status: "done", Title: issue.Title, Rank: "U"}, engine); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	clock = clock.Add(time.Second)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the close's suspends: %v", err)
	}
	if rows, attempts := suspendRow(); machine.Claim().State != supervise.StateWorking || rows != 1 || attempts != 1 || len(rt.CallsOf("Suspend")) != 0 {
		t.Fatalf("while its turn runs the planner is %s with %d suspend rows run %d times and %d suspensions, want working, the row run once and waiting, none",
			machine.Claim().State, rows, attempts, len(rt.CallsOf("Suspend")))
	}

	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: planner}); err != nil {
		t.Fatalf("end the planning turn: %v", err)
	}
	if got := machine.Claim(); got.State != supervise.StateSuspended || got.Session != "ses-plan" || got.SessionFile != "/tmp/plan.jsonl" || len(rt.CallsOf("Suspend")) != 1 {
		t.Fatalf("after its turn the planner is %s on %q with %d suspensions, want suspended once with its session kept", got.State, got.Session, len(rt.CallsOf("Suspend")))
	}
	clock = clock.Add(time.Hour)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the waiting suspend: %v", err)
	}
	if rows, _ := suspendRow(); rows != 0 || len(rt.CallsOf("Suspend")) != 1 {
		t.Fatalf("after the retry the planner's suspend rows left = %d and suspensions %d, want the row finished and one suspension",
			rows, len(rt.CallsOf("Suspend")))
	}
}

// CI settling red while the tester is in a turn — running a tool, its phase not completed — takes
// the phase back to implementing with nobody's completion. The tester's turn is interrupted, not its
// process: the implementer's start waits, sending the implementer nothing, until the tester's turn
// has ended, and then hands the implementer the CI-red task. The tester keeps its launch, its
// session and its capability, and nothing stops it.
func TestACIRedSendBackInterruptsTheTestersTurnBeforeTheImplementerStarts(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: issue.Key, Repo: "acme/widgets", Number: 86, Branch: "legion/LEGION-208", HeadSHA: "sha-1",
			HeadUpdatedAt: time.Now(), Failing: []string{}, CheckRuns: []record.AttemptRun{}, Required: []string{"ci"}})
	}); err != nil {
		t.Fatalf("seed the pull request: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	boot := func(role claim.Role, task string, p phase.Phase) (*supervise.Machine, *fake.Conn) {
		t.Helper()
		token, err := claim.NewToken("legion", issue.Key, role)
		if err != nil {
			t.Fatal(err)
		}
		machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: role, State: supervise.StateQueued}, "")
		if err != nil {
			t.Fatal(err)
		}
		conn := fake.NewConn()
		sup.deps.Conns.(*fake.Conns).Register(token, conn)
		if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
			t.Fatalf("spawn the %s: %v", role, err)
		}
		generation := machine.Claim().Generation
		for _, ev := range []supervise.Event{
			supervise.StreamHello{Claim: token, Generation: generation},
			supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-" + string(role), SessionFile: "/tmp/" + string(role) + ".jsonl", CapabilityHash: []byte(role)},
			supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-" + string(role)},
		} {
			if err := machine.Handle(ctx, ev); err != nil {
				t.Fatalf("boot the %s: handle %T: %v", role, ev, err)
			}
		}
		if task != "" {
			if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: task, Phase: p, Generation: 1}); err != nil {
				t.Fatalf("deliver the %s's task: %v", role, err)
			}
			machine.Wait()
			if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("start the %s's turn: %v", role, err)
			}
		}
		return machine, conn
	}
	// The implementer finished implementing and is idle; the tester is in its testing turn.
	implementer, implementerConn := boot(claim.RoleImplementer, "", "")
	tester, testerConn := boot(claim.RoleTester, "Continue Workflow. Issue: LEGION-208. Phase: testing.", phase.Testing)
	before := tester.Claim()
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	clock := time.Now()
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "testing"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine}, now: func() time.Time { return clock }, log: quietLogger(),
	}
	run := func(what string) {
		t.Helper()
		clock = clock.Add(time.Hour)
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run %s: %v", what, err)
		}
		implementer.Wait()
	}
	ciRedTasks := func() int {
		t.Helper()
		count := 0
		for _, prompt := range implementerConn.Prompts() {
			if strings.Contains(prompt.Message, "Phase: implementing.") && strings.Contains(prompt.Message, "CI is red at sha-1: ci") {
				count++
			}
		}
		return count
	}

	if result, err := intake.ApplyFact(ctx, pool, "github", "red", intake.PullRequestChecks{Repo: "acme/widgets", Number: 86, HeadSHA: "sha-1",
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "red-1", Failing: []string{"ci"}}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("apply the red verdict = %+v, %v", result, err)
	}
	run("the send-back's effects")
	run("the start's first retry, the tester's turn still running")
	if got := tester.Claim(); got.State != supervise.StateWorking || testerConn.Aborts() != 1 || ciRedTasks() != 0 || len(implementerConn.Prompts()) != 0 {
		t.Fatalf("while the tester's turn runs it is %s with %d aborts and the implementer got %d prompts; want working, interrupted once, and nothing sent to the implementer",
			got.State, testerConn.Aborts(), len(implementerConn.Prompts()))
	}

	if err := tester.Handle(ctx, supervise.StreamTurnEnd{Claim: before.Token}); err != nil {
		t.Fatalf("end the tester's interrupted turn: %v", err)
	}
	run("the start once the tester's turn is over")
	testwait.Eventually(t, "the implementer to be handed its CI-red task", func() bool { return ciRedTasks() == 1 })
	after := tester.Claim()
	if after.State != supervise.StateIdle || after.Generation != before.Generation || after.Session != before.Session || after.Locator == nil ||
		*after.Locator != *before.Locator || !bytes.Equal(after.CapabilityHash, before.CapabilityHash) || testerConn.Aborts() != 1 {
		t.Fatalf("after the takeover the tester is %+v with %d aborts, want idle on its launch, session and capability, interrupted once", after, testerConn.Aborts())
	}
	for _, method := range []string{"Suspend", "Release"} {
		for _, call := range rt.CallsOf(method) {
			if call.Locator.Claim == before.Token {
				t.Fatalf("the runtime was asked to %s the tester: %+v", method, call)
			}
		}
	}
	// The tester's next turn, a question asked through Envoy, is not interrupted for the start it
	// already let go.
	if err := tester.Handle(ctx, supervise.StreamTurnStart{Claim: before.Token}); err != nil {
		t.Fatalf("start the tester's later turn: %v", err)
	}
	run("the rows due after the tester's later turn starts")
	if testerConn.Aborts() != 1 || ciRedTasks() != 1 {
		t.Fatalf("the tester's later turn: %d aborts and %d CI-red tasks, want the one abort and the one task", testerConn.Aborts(), ciRedTasks())
	}
}

// A role that finished its phase stays resident, so its process dying is recovered like any other:
// the same session is relaunched, it is handed nothing, and the issue stays in the phase another role
// works. When its relaunches run out, the claim fails, and the architect is told in a worker-died
// that says the phase is not that role's; the issue is not held, since the phase's own worker is fine.
func TestAFinishedWorkerThatDiesIsRelaunchedAndItsFailureHoldsNothing(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, pool, records, issue)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: issue.Key, Role: claim.RolePlanner, Claim: "planner", HandoffCommit: "plan-1", LastHandoff: "plan-1"})
	}); err != nil {
		t.Fatal(err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	sup.OnTerminal((&workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	planner, err := claim.NewToken("legion", issue.Key, claim.RolePlanner)
	if err != nil {
		t.Fatal(err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: planner, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RolePlanner, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatal(err)
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(planner, conn)
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: planner}); err != nil {
		t.Fatalf("spawn the planner: %v", err)
	}
	boot := func() {
		t.Helper()
		generation := machine.Claim().Generation
		for _, ev := range []supervise.Event{
			supervise.StreamHello{Claim: planner, Generation: generation},
			supervise.RequestRegister{Claim: planner, Generation: generation, Session: "ses-plan", SessionFile: "/tmp/plan.jsonl", CapabilityHash: []byte("planner-capability")},
			supervise.RequestReady{Claim: planner, Generation: generation, Session: "ses-plan"},
		} {
			if err := machine.Handle(ctx, ev); err != nil {
				t.Fatalf("boot the planner: handle %T: %v", ev, err)
			}
		}
	}
	die := func() error {
		t.Helper()
		return machine.Handle(ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now()}})
	}
	boot()
	launched := machine.Claim()

	// The finished planner's process dies while it is idle: the same session is relaunched.
	if err := die(); err != nil {
		t.Fatalf("the planner's process dies: %v", err)
	}
	resumes := rt.CallsOf("Resume")
	if len(resumes) != 1 || resumes[0].Spec.ResumeSessionFile != "/tmp/plan.jsonl" {
		t.Fatalf("after its death the planner was resumed %+v, want once from its session file", resumes)
	}
	boot()
	relaunched := machine.Claim()
	if relaunched.State != supervise.StateReady || relaunched.Session != "ses-plan" || relaunched.Generation == launched.Generation || relaunched.Pending != nil || len(conn.Prompts()) != 0 {
		t.Fatalf("the relaunched planner is %+v with %d prompts, want ready on its session in a new launch, handed nothing", relaunched, len(conn.Prompts()))
	}

	// It dies again, and every relaunch is refused: the claim fails.
	rt.ScriptResume(fake.SpawnResult{Err: errors.New("the pod did not start")}, fake.SpawnResult{Err: errors.New("the pod did not start")})
	if err := die(); err == nil {
		t.Fatal("the death whose relaunch the runtime refused answered nil, want the refusal")
	}
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("after its relaunches ran out the planner is %s, want failed", got)
	}
	died := record.Notice{Kind: "worker-died", Role: claim.RolePlanner, Phase: phase.Implementing,
		Reason: "the planner does not work LEGION-208's phase implementing; nothing is held"}
	var notices []record.OutboxPayload
	testwait.Eventually(t, "the architect to be told of the finished planner's failure", func() bool {
		notices = nil
		rows, err := pool.Query(ctx, "select id, kind, issue, payload from outbox where kind in ('notice', 'controller_notice') order by id")
		if err != nil {
			t.Fatalf("read the notice rows: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row record.OutboxRow
			if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
				t.Fatalf("scan a notice row: %v", err)
			}
			payload, err := record.DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("decode notice row %d: %v", row.ID, err)
			}
			notices = append(notices, payload)
		}
		return len(notices) > 0
	})
	if len(notices) != 1 || notices[0] != record.OutboxPayload(died) {
		t.Fatalf("notices = %+v, want the one worker-died %+v", notices, died)
	}
	var current phase.Phase
	var held bool
	if err := pool.QueryRow(ctx, "select phase, held_from is not null from issues where key = $1", issue.Key).Scan(&current, &held); err != nil {
		t.Fatal(err)
	}
	if current != phase.Implementing || held {
		t.Fatalf("after the finished planner failed the issue is in %s, held %t; want implementing and not held", current, held)
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
			HeadUpdatedAt: time.Now(), Failing: []string{}, CheckRuns: []record.AttemptRun{}, FixAttempts: 2})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, engine, 2, "LEGION", quietLogger())
	for _, step := range []struct {
		id   string
		fact intake.Fact
	}{
		{"backlog", intake.DispatchIssue{Key: key, Seq: 6, Type: "issue.updated", Status: "backlog", Title: root.Title, Rank: "U"}},
		{"todo", intake.DispatchIssue{Key: key, Seq: 7, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: "U", HandedOver: true}},
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
			HeadUpdatedAt: time.Now(), Failing: []string{}, CheckRuns: []record.AttemptRun{}})
	}); err != nil {
		t.Fatal(err)
	}
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, engine, 2, "LEGION", quietLogger())

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
