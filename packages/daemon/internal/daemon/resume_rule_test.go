package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// readyClaimOf drives token's launched claim through its shim's hello, its agent's registration and
// its ready, at the claim's current generation, on the session the claim keeps under its role.
func readyClaimOf(t *testing.T, sup *supervisor, token claim.Token, role claim.Role) {
	t.Helper()
	machine, ok := sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	generation := machine.Claim().Generation
	session := "ses-" + string(role)
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: session, SessionFile: "/tmp/" + string(role) + ".jsonl"},
		supervise.RequestReady{Claim: token, Generation: generation, Session: session},
	} {
		if err := machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %T for %s: %v", ev, token, err)
		}
	}
}

// launchedClaim creates role's claim on issue of tree, launches it and makes its agent ready.
func launchedClaim(t *testing.T, sup *supervisor, tree, issue string, role claim.Role) claim.Token {
	t.Helper()
	token := mustClaimToken(t, issue, role)
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: tree, Issue: issue, Role: role, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create %s: %v", token, err)
	}
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	if err := machine.Handle(context.Background(), supervise.RequestSpawn{Claim: token}); err != nil {
		t.Fatalf("spawn %s: %v", token, err)
	}
	readyClaimOf(t, sup, token, role)
	return token
}

// exitedArchitect is LEGION-1's tree architect, ready, whose agent then reported its own exit: an
// exit suspends a tree's root (supervise's exit), which is what an architect leaves behind while it
// waits for its tree's workers.
func exitedArchitect(t *testing.T, sup *supervisor) claim.Token {
	t.Helper()
	token := launchedClaim(t, sup, "LEGION-1", "LEGION-1", claim.RoleArchitect)
	machine, _ := sup.Machine(token)
	if err := machine.Handle(context.Background(), supervise.RequestExit{Claim: token, Generation: machine.Claim().Generation,
		Session: "ses-architect", Reason: "the tree waits on its workers"}); err != nil {
		t.Fatalf("the architect's exit: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateSuspended {
		t.Fatalf("the exited architect is %s, want suspended", got)
	}
	return token
}

// resumesOf is the runtime's Resume calls for token's claim.
func resumesOf(rt *fake.Runtime, token claim.Token) []fake.Call {
	var of []fake.Call
	for _, call := range rt.CallsOf("Resume") {
		if call.Spec.Claim == token || call.Locator.Claim == token {
			of = append(of, call)
		}
	}
	return of
}

// A worker's completion reaches its architect only as a notice on the architect's role topic. An
// architect whose agent exited holds no role, so the notice waits; nothing else would resume the
// architect, and the notice would wait for ever. The notice resumes it instead: the architect
// relaunches its own session, and once it holds its role again every notice held for it arrives,
// in the order the notices were written.
func TestANoticeForAnArchitectThatExitedResumesItAndArrivesInOrder(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	architect := exitedArchitect(t, sup)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, time.Now()))
	clock := time.Now()
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}
	ctx := context.Background()

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the held notices: %v", err)
	}
	machine, _ := sup.Machine(architect)
	testwait.Eventually(t, "the exited architect to be resumed for the notice waiting on it", func() bool {
		return machine.View().State == supervise.StateLaunching && len(resumesOf(rt, architect)) == 1
	})
	if resume := resumesOf(rt, architect)[0]; resume.Spec.ResumeSessionFile != "/tmp/architect.jsonl" {
		t.Fatalf("resumed %+v, want the architect's own session", resume.Spec)
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 0 || outboxRows(t, pool) != 2 {
		t.Fatalf("before the architect holds its role: delivered %+v with %d rows, want both notices held", delivered, outboxRows(t, pool))
	}

	// The relaunched architect takes its role back.
	readyClaimOf(t, sup, architect, claim.RoleArchitect)
	publisher.setAbsent()
	for tick := range 3 {
		clock = clock.Add(2 * time.Minute)
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("tick %d after the architect returned: %v", tick, err)
		}
	}
	_, delivered := publisher.snapshot()
	if got := deliveredKinds(t, delivered); fmt.Sprint(got) != "[phase-finished to LEGION-1 pr-blocked to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left, want both notices in the order written and none left", got, outboxRows(t, pool))
	}
	if resumes := resumesOf(rt, architect); len(resumes) != 1 {
		t.Fatalf("the architect was resumed %d times, want once", len(resumes))
	}
}

// A claim the rule resumes and whose launch keeps failing is not resumed for ever: each attempt is
// charged to its launch budget as any other launch is, the claim fails once the budget is spent,
// and a failed claim is never resumed by rule. The notice waiting on it then finishes undelivered,
// and later ticks launch nothing.
func TestAnArchitectResumedForANoticeStopsAtItsLaunchBudget(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	architect := exitedArchitect(t, sup)
	unschedulable := errors.New("the pod was never scheduled")
	for range 10 {
		rt.ScriptResume(fake.SpawnResult{Err: unschedulable})
	}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	clock := time.Now()
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}
	ctx := context.Background()
	machine, _ := sup.Machine(architect)

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the held notice: %v", err)
	}
	testwait.Eventually(t, "the resumed architect to fail at its launch budget", func() bool {
		return machine.View().State == supervise.StateFailed
	})
	for tick := range 5 {
		clock = clock.Add(2 * time.Minute)
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("tick %d after the architect failed: %v", tick, err)
		}
	}
	machine.Wait()
	limit := sup.deps.Limits.LaunchFailures
	if resumes := resumesOf(rt, architect); len(resumes) != limit {
		t.Fatalf("the architect was launched %d times, want its launch budget of %d and no more", len(resumes), limit)
	}
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("the architect is %s, want failed", got)
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 0 || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %+v with %d rows left, want the notice finished undelivered", delivered, outboxRows(t, pool))
	}
}

// A lingering tree's claims stay suspended: the rule resumes nothing of a tree that has left the
// workflow, and a notice of it finishes undelivered rather than resuming the architect that
// exited. Re-admission, a person's todo, is what runs the tree again.
func TestANoticeOfALingeringTreeDoesNotResumeItsArchitect(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	architect := exitedArchitect(t, sup)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: time.Now}

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the notice: %v", err)
	}
	machine, _ := sup.Machine(architect)
	machine.Wait()
	if got := machine.Claim().State; got != supervise.StateSuspended || len(resumesOf(rt, architect)) != 0 || outboxRows(t, pool) != 0 {
		t.Fatalf("the lingering tree's architect is %s with %d resumes and %d rows left, want it suspended, never resumed, the notice finished",
			got, len(resumesOf(rt, architect)), outboxRows(t, pool))
	}
}

// The linger's suspension of a claim in a turn waits for that turn to end. An architect's sign-off,
// which moves its tree into linger, is a tool call inside a turn that goes on after it, and a
// resumed architect's turn acting on a notice is no shorter: stopping either at the stop timeout
// cuts off what the turn was doing. The linger's own expiry, which closes the tree whatever its
// claims are doing, bounds the wait.
func TestTheLingersSuspensionWaitsForTheArchitectsTurnToEnd(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	until := time.Now().Add(time.Hour)
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}
	putOutboxIssue(t, pool, records, root)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	// Real timers, so the held suspension's stop timeout runs as it does in a daemon.
	sup.deps.Clock = supervise.RealClock{}
	architect := launchedClaim(t, sup, root.Key, root.Key, claim.RoleArchitect)
	machine, _ := sup.Machine(architect)
	ctx := context.Background()
	// The architect is in a turn: the one whose sign-off closed its tree.
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: architect}); err != nil {
		t.Fatalf("the architect's turn: %v", err)
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", now: time.Now}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "suspend", Tree: root.Tree, Role: claim.RoleArchitect,
		Generation: 1, Reason: "the tree of LEGION-208 lingers"}, time.Now()))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the linger's suspend: %v", err)
	}
	time.Sleep(3 * sup.deps.Timeouts.Stop)
	if got := machine.Claim().State; got != supervise.StateWorking || len(rt.CallsOf("Suspend")) != 0 {
		t.Fatalf("three stop timeouts into the turn the architect is %s after %d suspensions, want its turn still running",
			got, len(rt.CallsOf("Suspend")))
	}
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: architect}); err != nil {
		t.Fatalf("the turn's end: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateSuspended || len(rt.CallsOf("Suspend")) != 1 {
		t.Fatalf("after its turn the architect is %s after %d suspensions, want suspended once", got, len(rt.CallsOf("Suspend")))
	}
}

// operatorSuspend is the operator's suspend route's request (api's suspendEvent) handled by token's
// machine.
func operatorSuspend(t *testing.T, sup *supervisor, token claim.Token) {
	t.Helper()
	machine, _ := sup.Machine(token)
	if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: token, Reason: "the operator suspended it", By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("the operator's suspend of %s: %v", token, err)
	}
	if got := machine.Claim().State; got != supervise.StateSuspended {
		t.Fatalf("%s is %s after the operator's suspend, want suspended", token, got)
	}
}

// An operator's suspend holds until the operator resumes the claim: a phase moving on to the role
// does not resume it. The start waits, and once the operator resumes the claim, its task is given.
func TestAStartWaitsForTheOperatorToResumeAClaimItSuspended(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	tester := launchedClaim(t, sup, issue.Tree, issue.Key, claim.RoleTester)
	operatorSuspend(t, sup, tester)
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleTester,
		Generation: 1, Phase: phase.Testing, Task: "Test it."}, clock))
	ctx := context.Background()
	machine, _ := sup.Machine(tester)

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the start: %v", err)
	}
	machine.Wait()
	if resumes := resumesOf(rt, tester); len(resumes) != 0 || machine.Claim().State != supervise.StateSuspended || outboxRows(t, pool) != 1 {
		t.Fatalf("after the start the tester is %s with %d resumes and %d rows, want it suspended, never resumed, the start waiting",
			machine.Claim().State, len(resumes), outboxRows(t, pool))
	}

	if err := machine.Handle(ctx, supervise.RequestResume{Claim: tester}); err != nil {
		t.Fatalf("the operator's resume: %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the start after the operator's resume: %v", err)
	}
	if pending := machine.Claim().Pending; outboxRows(t, pool) != 0 || pending == nil || pending.Task != "Test it." {
		t.Fatalf("after the operator's resume: %d rows, pending %+v; want the start run and its task held for the tester", outboxRows(t, pool), pending)
	}
}

// readmissionRig is a lingering tree, LEGION-208, whose child LEGION-209 was implementing when the
// tree closed: the root's architect and the child's implementer each ran, and each was suspended,
// suspend as the case says. The returned function re-admits the tree and runs what it queued.
func readmissionRig(t *testing.T, suspend func(*supervisor, claim.Token)) (rt *fake.Runtime, sup *supervisor, architect, implementer claim.Token, readmit func(), tick func(time.Duration)) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 3, Status: "done", Rank: "U",
		LingerUntil: &until, LastDispatchSeq: 5, HandedOver: true}
	putOutboxIssue(t, pool, records, root)
	parent := root.Key
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-209", Project: "LEGION", Tree: root.Key, Parent: &parent, Title: "Child",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"})
	sup, rt = newOutboxSupervisor(t, "legion", t.TempDir())
	architect = launchedClaim(t, sup, root.Key, root.Key, claim.RoleArchitect)
	implementer = launchedClaim(t, sup, root.Key, "LEGION-209", claim.RoleImplementer)
	suspend(sup, architect)
	suspend(sup, implementer)
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, engine, 2, "legion", quietLogger())
	var clock time.Time
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, root.Key), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: ghrepo.MustParse("acme/widgets"),
		githubAPI: newBranchGitHub(t, nil, branchExists).url,
		dispatch:  &outboxDispatch{issue: dispatch.Issue{Key: root.Key, Status: "todo"}}, handlers: []intake.Handler{engine, admission},
		now: func() time.Time { return clock }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
		remove: func(context.Context, workspace.Workspace) error { return nil },
	}
	tick = func(by time.Duration) {
		t.Helper()
		clock = time.Now().Add(by)
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
		for _, token := range []claim.Token{architect, implementer} {
			machine, _ := sup.Machine(token)
			machine.Wait()
		}
	}
	readmit = func() {
		t.Helper()
		if _, err := intake.ApplyFact(ctx, pool, "dispatch", "todo-again", intake.DispatchIssue{Key: root.Key, Seq: 6, Type: "issue.updated", Status: "todo",
			Title: root.Title, Rank: root.Rank, HandedOver: true}, engine, admission); err != nil {
			t.Fatalf("apply the todo: %v", err)
		}
		// The first run creates the branches the starts wait for; the second runs the starts.
		tick(0)
		tick(0)
	}
	return rt, sup, architect, implementer, readmit, tick
}

// Re-admitting a tree whose claims the daemon suspended when the tree closed resumes them: the
// root's architect, and the worker of the child's own phase, each relaunch their sessions, rather
// than the tree inheriting claims that never run again.
func TestReadmissionResumesTheClaimsTheTreesCloseSuspended(t *testing.T) {
	rt, sup, architect, implementer, readmit, _ := readmissionRig(t, func(sup *supervisor, token claim.Token) {
		machine, _ := sup.Machine(token)
		if err := machine.Handle(context.Background(), supervise.RequestSuspend{Claim: token, Reason: "the tree of LEGION-208 lingers", By: supervise.SuspendedByDaemon}); err != nil {
			t.Fatalf("the linger's suspend of %s: %v", token, err)
		}
	})
	readmit()
	for _, token := range []claim.Token{architect, implementer} {
		machine, _ := sup.Machine(token)
		if got := machine.Claim().State; got != supervise.StateLaunching || len(resumesOf(rt, token)) != 1 {
			t.Fatalf("after re-admission %s is %s with %d resumes, want its session relaunched once", token, got, len(resumesOf(rt, token)))
		}
	}
}

// Re-admission does not override an operator's suspend: the starts it queues wait until the
// operator resumes each claim, and then go on, the child's worker given the task to carry its
// phase on.
func TestReadmissionWaitsForTheOperatorToResumeTheClaimsItSuspended(t *testing.T) {
	rt, sup, architect, implementer, readmit, tick := readmissionRig(t, func(sup *supervisor, token claim.Token) { operatorSuspend(t, sup, token) })
	readmit()
	for _, token := range []claim.Token{architect, implementer} {
		machine, _ := sup.Machine(token)
		if got := machine.Claim().State; got != supervise.StateSuspended || len(resumesOf(rt, token)) != 0 {
			t.Fatalf("after re-admission %s is %s with %d resumes, want it held suspended for the operator", token, got, len(resumesOf(rt, token)))
		}
	}
	for _, token := range []claim.Token{architect, implementer} {
		machine, _ := sup.Machine(token)
		if err := machine.Handle(context.Background(), supervise.RequestResume{Claim: token}); err != nil {
			t.Fatalf("the operator's resume of %s: %v", token, err)
		}
	}
	tick(2 * time.Minute)
	machine, _ := sup.Machine(implementer)
	if pending := machine.Claim().Pending; pending == nil || pending.Phase != phase.Implementing {
		t.Fatalf("after the operator's resume the implementer holds %+v, want its phase's resume task", pending)
	}
}

// seedWorkflowTree records root as an admitted tree of project in the store at dsn — the issue, its
// admission slot and its tree's lifecycle, as admission leaves a root it promoted — and each of
// claims, bound to that lifecycle as the daemon binds a claim it admits.
func seedWorkflowTree(t *testing.T, dsn, project string, root record.Issue, claims ...supervise.Claim) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, st.Pool(), func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()})
	}); err != nil {
		t.Fatalf("record the tree of %s: %v", root.Key, err)
	}
	if _, err := st.OpenTreeLifecycle(ctx, project, root.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", root.Key, err)
	}
	for _, c := range claims {
		if _, err := st.AdmitClaim(ctx, c); err != nil {
			t.Fatalf("put %s: %v", c.Token, err)
		}
	}
}

// A daemon that boots with a suspended claim owning its issue's current phase resumes it: nothing
// else will, since the phase's start already ran before the restart. The resumed worker relaunches
// its own session and is told to carry its phase on.
func TestABootResumesASuspendedClaimThatOwnsItsIssuesPhase(t *testing.T) {
	pool := isolatedOutboxPool(t)
	cfg := workflowConfig(t, workflowNATS(t))
	cfg.PostgresDSN = pool.Config().ConnString()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.Project + "-1"
	implementer, err := claim.NewToken(project, root, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	seedWorkflowTree(t, cfg.PostgresDSN, project, record.Issue{Key: root, Project: cfg.Project, Tree: root, Title: "Workflow",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U"},
		supervise.Claim{Token: implementer, Project: project, Tree: root, Issue: root, Role: claim.RoleImplementer, Generation: 1,
			State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses_implementer", SessionFile: filepath.Join(cfg.StateDir, "implementer.jsonl")})
	writePrompt(t, cfg, implementer)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}

	startDaemon(t, cfg, o)

	testwait.Eventually(t, "the booted daemon to resume the implementer that owns its issue's phase", func() bool {
		return len(resumesOf(rt, implementer)) == 1
	})
	if resume := resumesOf(rt, implementer)[0]; resume.Spec.ResumeSessionFile != filepath.Join(cfg.StateDir, "implementer.jsonl") {
		t.Fatalf("resumed %+v, want the implementer's own session", resume.Spec)
	}
}
