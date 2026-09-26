package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
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

func TestOutboxDropsSupersededStatusWrite(t *testing.T) {
	row, err := record.NewOutboxRow("LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, time.Now())
	if err != nil {
		t.Fatalf("new outbox row: %v", err)
	}
	row.ID = 42
	client := &outboxDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "backlog"}}
	runner := &outbox{log: quietLogger(), dispatch: client}

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(client.statuses) != 0 {
		t.Fatalf("status writes = %#v, want superseded row dropped", client.statuses)
	}
}

func TestOutboxFinishesMessageAlreadyPostedBeforeCrash(t *testing.T) {
	row, err := record.NewOutboxRow("LEGION-208", record.MessagePost{Body: "Pull request checks are blocked."}, time.Now())
	if err != nil {
		t.Fatalf("new outbox row: %v", err)
	}
	row.ID = 44
	client := &outboxDispatch{bodies: []string{"Pull request checks are blocked.\n\n<!-- legion-outbox:44 -->"}}
	runner := &outbox{log: quietLogger(), dispatch: client}

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(client.messages) != 0 {
		t.Fatalf("posts = %#v, want marker to suppress duplicate", client.messages)
	}
}

type outboxDispatch struct {
	issue          dispatch.Issue
	statuses       []string
	bodies         []string
	messages       []string
	getErr         error
	statusErr      error
	messageReadErr error
	postErr        error
	approval       dispatch.Approval
	approvalErr    error
}

func (d *outboxDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return nil, nil
}
func (d *outboxDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return d.issue, d.getErr
}
func (d *outboxDispatch) SetStatus(_ context.Context, _ string, status string) error {
	d.statuses = append(d.statuses, status)
	return d.statusErr
}
func (d *outboxDispatch) PostMessage(_ context.Context, _ string, body string) error {
	d.messages = append(d.messages, body)
	return d.postErr
}
func (d *outboxDispatch) MessageBodiesSince(context.Context, string, time.Time) ([]string, error) {
	return d.bodies, d.messageReadErr
}
func (d *outboxDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return d.approval, d.approvalErr
}

func TestOutboxStatusWriteCallsDispatchOnlyForItsObservedStatus(t *testing.T) {
	row := mustOutboxRow(t, "LEGION-208", record.StatusWrite{ObservedStatus: "todo", Status: "in_progress"}, time.Now())
	client := &outboxDispatch{issue: dispatch.Issue{Key: row.Issue, Status: "todo"}}

	if err := (&outbox{dispatch: client}).execute(context.Background(), row); err != nil {
		t.Fatalf("execute status write: %v", err)
	}
	if got := client.statuses; len(got) != 1 || got[0] != "in_progress" {
		t.Fatalf("status writes = %v, want one in_progress write", got)
	}

	client.getErr = errors.New("Dispatch unavailable")
	if err := (&outbox{dispatch: client}).execute(context.Background(), row); err == nil || !strings.Contains(err.Error(), "Dispatch unavailable") {
		t.Fatalf("status write failure = %v, want Dispatch read failure", err)
	}
}

func TestOutboxMessagePostsItsMarkerAndReturnsReadFailure(t *testing.T) {
	row := mustOutboxRow(t, "LEGION-208", record.MessagePost{Body: "Dispatch message."}, time.Now())
	row.ID = 55
	client := &outboxDispatch{}

	if err := (&outbox{dispatch: client}).execute(context.Background(), row); err != nil {
		t.Fatalf("execute message post: %v", err)
	}
	if got := client.messages; len(got) != 1 || got[0] != "Dispatch message.\n\n<!-- legion-outbox:55 -->" {
		t.Fatalf("messages = %#v, want marker-appended post", got)
	}

	client.messageReadErr = errors.New("event history unavailable")
	if err := (&outbox{dispatch: client}).execute(context.Background(), row); err == nil || !strings.Contains(err.Error(), "event history unavailable") {
		t.Fatalf("message read failure = %v, want event history failure", err)
	}
}

func TestOutboxNoticePublishesIssueAndTreeTopics(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-1", Title: "Child", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	row := mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now())
	row.ID = 56
	publisher := &outboxPublisher{}

	if err := (&outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher}).execute(context.Background(), row); err != nil {
		t.Fatalf("execute notice: %v", err)
	}
	if got := publisher.topics(); fmt.Sprint(got) != "[notifications.legion.legion.LEGION-2 notifications.legion.legion.LEGION-1]" { // the LEGION_PROJECT token panes subscribe under
		t.Fatalf("notice topics = %v, want issue then root topics", got)
	}
	for _, dedupe := range publisher.keys() {
		if dedupe != "legion-outbox:56" {
			t.Fatalf("notice dedupe key = %q, want outbox row key", dedupe)
		}
	}
}

func TestOutboxNoticeReturnsPublisherFailure(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Project: "LEGION", Tree: "LEGION-2", Title: "Root", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	row := mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now())
	publisher := &outboxPublisher{err: errors.New("listener unavailable")}

	if err := (&outbox{pool: pool, dispatchProject: "LEGION", records: records, notices: publisher}).execute(context.Background(), row); err == nil || !strings.Contains(err.Error(), "listener unavailable") {
		t.Fatalf("notice failure = %v, want listener failure", err)
	}
}

func TestOutboxGateSeedAppliesApprovedFactAndReturnsApprovalFailure(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	handler := &outboxFactHandler{}
	row := mustOutboxRow(t, "LEGION-208", record.GateSeed{ArtifactID: "artifact-208", Version: 3}, time.Now())
	version := new(int)
	*version = 3
	client := &outboxDispatch{approval: dispatch.Approval{State: "approved", LatestVersion: 3, Version: version}}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, handlers: []intake.Handler{handler}}

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute gate seed: %v", err)
	}
	if got := handler.facts(); len(got) != 1 || got[0] != (intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 3}) {
		t.Fatalf("gate facts = %#v, want approved artifact", got)
	}

	client.approvalErr = errors.New("Dispatch unavailable")
	if err := runner.execute(context.Background(), row); err == nil || !strings.Contains(err.Error(), "Dispatch unavailable") {
		t.Fatalf("gate seed failure = %v, want approval failure", err)
	}
}

// The seed reads the version Dispatch holds, which may be later than the one the architect
// registered: a later version it did not see is still the current one, as the shipped daemon
// raises the gate to Dispatch's latest version. Approved at that latest version, the gate opens
// there; approved only at an earlier one, the gate moves to the latest version and stays closed.
func TestOutboxGateSeedRaisesTheGateToTheVersionDispatchHolds(t *testing.T) {
	two, one := 2, 1
	for _, tc := range []struct {
		name     string
		approval dispatch.Approval
		want     intake.DispatchArtifact
	}{
		{name: "approved at the later version", approval: dispatch.Approval{State: "approved", LatestVersion: 2, Version: &two},
			want: intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 2}},
		{name: "approved at the registered version only", approval: dispatch.Approval{State: "stale", LatestVersion: 2, Version: &one},
			want: intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactVersion, Version: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			handler := &outboxFactHandler{}
			row := mustOutboxRow(t, "LEGION-208", record.GateSeed{ArtifactID: "artifact-208", Version: 1}, time.Now())
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: record.NewStore(), dispatch: &outboxDispatch{approval: tc.approval}, handlers: []intake.Handler{handler}}
			if err := runner.execute(context.Background(), row); err != nil {
				t.Fatalf("execute gate seed: %v", err)
			}
			if got := handler.facts(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("gate facts = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestOutboxLingerAppliesExpirationAndReturnsHandlerFailure(t *testing.T) {
	pool := isolatedOutboxPool(t)
	handler := &outboxFactHandler{}
	row := mustOutboxRow(t, "LEGION-208", record.LingerClose{Generation: 7}, time.Now())
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", handlers: []intake.Handler{handler}}

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute linger close: %v", err)
	}
	if got := handler.facts(); len(got) != 1 || got[0] != (intake.LingerExpired{Issue: "LEGION-208", Generation: 7}) {
		t.Fatalf("linger facts = %#v, want expiration", got)
	}

	runner.handlers = []intake.Handler{&outboxFactHandler{err: errors.New("record unavailable")}}
	failingRow := mustOutboxRow(t, "LEGION-208", record.LingerClose{Generation: 8}, time.Now())
	if err := runner.execute(context.Background(), failingRow); err == nil || !strings.Contains(err.Error(), "record unavailable") {
		t.Fatalf("linger failure = %v, want handler failure", err)
	}
}

func TestOutboxWorkspaceRemovalUsesDeterministicLocationAndReturnsFailure(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 2, Status: "done"}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	row := mustOutboxRow(t, issue.Key, record.WorkspaceRemove{Generation: issue.Generation}, time.Now())
	var removed workspace.Workspace
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, log: quietLogger(),
		stateDir: t.TempDir(),
		repo:     "acme/widgets",
		remove: func(_ context.Context, got workspace.Workspace) error {
			removed = got
			return nil
		},
	}

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute workspace removal: %v", err)
	}
	if want, _ := workspace.Location(runner.stateDir, runner.repo, row.Issue); removed != want {
		t.Fatalf("removed workspace = %#v, want %#v", removed, want)
	}

	runner.remove = func(context.Context, workspace.Workspace) error { return errors.New("remove failed") }
	if err := runner.execute(context.Background(), row); err == nil || !strings.Contains(err.Error(), "remove failed") {
		t.Fatalf("workspace removal failure = %v, want remove failure", err)
	}
}

func TestOutboxSuperviseStartsResumesSuspendsStopsAndDeduplicatesDelivery(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	provisioned := 0
	runner := &outbox{log: quietLogger(),
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			provisioned++
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RolePlanner, Task: "Plan it.", Generation: issue.Generation}, time.Now())
	row.ID = 57

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("start claim: %v", err)
	}
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("repeat start delivery: %v", err)
	}
	token, err := claim.NewToken("legion", issue.Key, claim.RolePlanner)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, found := sup.Machine(token)
	if !found {
		t.Fatal("supervisor has no created planner claim")
	}
	if got := machine.Claim(); got.State != supervise.StateLaunching || got.Pending == nil || got.Pending.ID != "outbox:57" || provisioned != 1 || len(runtime.CallsOf("Spawn")) != 1 {
		t.Fatalf("started claim = %+v, provisions=%d spawns=%d", got, provisioned, len(runtime.CallsOf("Spawn")))
	}
	if err := machine.Handle(context.Background(), supervise.RequestRegister{Claim: token, Generation: 1, Session: "ses-plan", SessionFile: "/tmp/plan.jsonl"}); err != nil {
		t.Fatalf("register planner: %v", err)
	}
	if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: token, Generation: 1, Session: "ses-plan"}); err != nil {
		t.Fatalf("ready planner: %v", err)
	}
	if err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "suspend", Tree: issue.Tree, Role: claim.RolePlanner, Generation: issue.Generation}, time.Now())); err != nil {
		t.Fatalf("suspend planner: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateSuspended {
		t.Fatalf("claim state after suspend = %s, want suspended", got)
	}
	if err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RolePlanner, Generation: issue.Generation}, time.Now())); err != nil {
		t.Fatalf("resume planner: %v", err)
	}
	if len(runtime.CallsOf("Resume")) != 1 {
		t.Fatalf("resume calls = %d, want one", len(runtime.CallsOf("Resume")))
	}
	if err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "tree_close", Tree: issue.Tree, Role: claim.RolePlanner, Generation: issue.Generation}, time.Now())); err != nil {
		t.Fatalf("close the planner's tree: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Fatalf("claim state after its tree's close = %s, want retired", got)
	}
}

// The architect's retry of a held phase enqueues a start for the claim that failed. The executor
// relaunches it; before, it went straight to the delivery, which a failed claim refuses, and the row
// retried forever while the issue sat in its phase with no worker.
func TestOutboxStartRelaunchesAFailedClaim(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create implementer claim: %v", err)
	}
	runtime.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")}, fake.SpawnResult{Err: errors.New("pane launch failed")})
	_ = machine.Handle(context.Background(), supervise.RequestSpawn{Claim: token})
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("claim state = %s, want failed", got)
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets"}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Task: "Continue. Reason: retry held phase.", Generation: issue.Generation}, time.Now())
	row.ID = 91

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("start the failed claim: %v", err)
	}
	if got := machine.Claim(); got.State != supervise.StateLaunching || got.Budgets != (supervise.Budgets{}) || got.Pending == nil || got.Pending.ID != "outbox:91" {
		t.Fatalf("retried claim = %+v, want launching with fresh budgets and the retry task pending", got)
	}
}

// The architect's retry of a held phase writes a start carrying the retry task. The relaunched
// claim still holds the task it was started with, so the retry task waits behind it and its row
// is retried until that task's turn is over. That turn can finish the phase: the implementer
// completes, and the transition suspends it and starts the tester. The waiting start was written
// for a phase the issue has left, and it neither relaunches the implementer nor hands it that
// phase, which would have it push to the pull request its tester is testing.
func TestOutboxRetryStartForAPhaseTheIssueLeftRelaunchesNothing(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	heldFrom := phase.Implementing
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Held, HeldFrom: &heldFrom, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{Issue: issue.Key, Repo: "acme/widgets", Number: 114, Branch: "legion/LEGION-208", HeadSHA: "f8f30933", Failing: []string{}, FailingStatuses: []string{}, State: record.PullRequestOpen})
	}); err != nil {
		t.Fatalf("put the pull request: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	// The held implementer: its launch budget spent, its session kept, and the task it was started
	// with still pending.
	machine, _, err := sup.Create(ctx, supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleImplementer, State: supervise.StateFailed,
		Generation: 3, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl",
		Pending: &supervise.Delivery{ID: "outbox:60", Task: "Continue Workflow. Issue: LEGION-208. Phase: implementing.", QueuedAt: time.Now()},
	}, "")
	if err != nil {
		t.Fatalf("create the held implementer: %v", err)
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	clock := time.Now().Add(time.Minute)
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "in_progress"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine}, now: func() time.Time { return clock }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}

	// The architect retries the held phase, and the outbox relaunches the implementer's session.
	if _, err := intake.ApplyFact(ctx, pool, "api", "retry:LEGION-208", intake.RetryOrEscalate{Issue: issue.Key, Decision: intake.RetryDecision}, engine); err != nil {
		t.Fatalf("apply the retry: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the retry's start: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateLaunching {
		t.Fatalf("the retried implementer is %s, want launching", got)
	}

	// The relaunched implementer works the task it is handed and completes the phase within that
	// turn; the issue moves to testing.
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
	testwait.Eventually(t, "the relaunched implementer's task to be acknowledged", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start the turn: %v", err)
	}
	result, err := intake.ApplyFact(ctx, pool, "api", "handoff:LEGION-208:implementer:implementing", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RoleImplementer, Claim: token, Commit: "952c3514"}, engine)
	if err != nil || result.Refusal != nil {
		t.Fatalf("complete implementing = %+v, %v", result.Refusal, err)
	}
	if err := machine.Handle(ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("end the turn: %v", err)
	}
	implementerLaunches := func() int {
		n := 0
		for _, call := range rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
				n++
			}
		}
		return n
	}
	launches := implementerLaunches()
	prompts := len(conn.Prompts())

	// The transition's effects run first — the implementer is suspended and the tester started —
	// and then every row still waiting comes due.
	clock = time.Now().Add(10 * time.Second)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the transition's effects: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateSuspended {
		t.Fatalf("after the move to testing the implementer is %s, want suspended", got)
	}
	clock = time.Now().Add(time.Hour)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the waiting rows: %v", err)
	}

	got := machine.Claim()
	if relaunched := implementerLaunches() - launches; relaunched != 0 || got.State != supervise.StateSuspended {
		t.Fatalf("after its phase ended the implementer was launched %d more times and is %s, want no launch and suspended", relaunched, got.State)
	}
	if got.Pending != nil || len(conn.Prompts()) != prompts {
		t.Fatalf("after its phase ended the implementer holds %+v and was prompted %d more times, want no task", got.Pending, len(conn.Prompts())-prompts)
	}
	if remaining := outboxRows(t, pool); remaining != 0 {
		t.Fatalf("outbox rows left = %d, want the start written for the left phase finished, not retried", remaining)
	}
}

// A task handed to a claim waits as its pending delivery until a turn confirms it. An agent that
// refuses the prompt after acknowledging it — it was already in the turn a human's steer started —
// has the delivery taken back, and it can finish the phase inside that turn: the transition then
// suspends the claim with the task still pending. The suspension retires that task, so the
// claim's next start, for the issue's next phase or round, resumes it with the new start's own
// task rather than the finished phase's — for the next round, or for retro, never "Phase:
// implementing" again.
func TestAResumedWorkerIsHandedItsNewPhaseNotATaskLeftPendingFromTheLast(t *testing.T) {
	for _, tc := range []struct {
		name   string
		review intake.PullRequestReview
		want   string
	}{
		{name: "approved into retro", review: intake.PullRequestReview{State: "APPROVED"}, want: "Phase: retro."},
		{name: "changes requested into the next round", review: intake.PullRequestReview{State: "CHANGES_REQUESTED", Body: "scripted changes requested, round 3"}, want: "round 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			ctx := context.Background()
			issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Reviewing, Generation: 1, Status: "needs_review"}
			putOutboxIssue(t, pool, records, issue)
			const head = "16973163"
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return records.PutPullRequest(ctx, tx, record.PullRequest{Issue: issue.Key, Repo: "acme/widgets", Number: 118, Branch: "legion/LEGION-208", HeadSHA: head, Verdict: "green", Failing: []string{}, FailingStatuses: []string{}, State: record.PullRequestOpen})
			}); err != nil {
				t.Fatalf("put the pull request: %v", err)
			}
			sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
			engine := workflow.New(records, workflow.Config{Project: "legion", ReviewRoundCap: 10}, quietLogger())
			clock := time.Now()
			runner := &outbox{
				dispatchProject: "LEGION",
				pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
				dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "needs_review"}}, notices: &outboxPublisher{},
				handlers: []intake.Handler{engine}, now: func() time.Time { return clock }, log: quietLogger(),
				provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
					return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
				},
			}
			apply := func(id string, fact intake.Fact) {
				t.Helper()
				result, err := intake.ApplyFact(ctx, pool, "api", id, fact, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("apply %s = %+v, %v", id, result.Refusal, err)
				}
			}
			due := func(what string) {
				t.Helper()
				clock = clock.Add(time.Hour)
				if err := runner.RunOnce(ctx); err != nil {
					t.Fatalf("run %s: %v", what, err)
				}
			}
			implementer, err := claim.NewToken("legion", issue.Key, claim.RoleImplementer)
			if err != nil {
				t.Fatalf("implementer claim token: %v", err)
			}
			tester, err := claim.NewToken("legion", issue.Key, claim.RoleTester)
			if err != nil {
				t.Fatalf("tester claim token: %v", err)
			}
			conn := fake.NewConn()
			sup.deps.Conns.(*fake.Conns).Register(implementer, conn)

			// The reviewer asks for round 2, and the implementer starts on its task.
			apply("review:round-2", intake.PullRequestReview{Repo: "acme/widgets", Number: 118, State: "CHANGES_REQUESTED", CommitID: head, HeadSHA: head, Body: "scripted changes requested, round 2"})
			// A review ends when its reviewer completes it: its handoff is part of the phase.
			apply("handoff:reviewer:reviewing:1", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RoleReviewer,
				Claim: "reviewer", Commit: "review-round-1"})
			due("the round-2 start")
			machine, found := sup.Machine(implementer)
			if !found {
				t.Fatal("the round-2 start created no implementer claim")
			}
			ready := func() {
				t.Helper()
				generation := machine.Claim().Generation
				for _, ev := range []supervise.Event{
					supervise.StreamHello{Claim: implementer, Generation: generation},
					supervise.RequestRegister{Claim: implementer, Generation: generation, Session: "ses-impl", SessionFile: "/tmp/impl.jsonl"},
					supervise.RequestReady{Claim: implementer, Generation: generation, Session: "ses-impl"},
				} {
					if err := machine.Handle(ctx, ev); err != nil {
						t.Fatalf("handle %T: %v", ev, err)
					}
				}
			}
			ready()
			testwait.Eventually(t, "the round-2 task to be acknowledged", func() bool {
				p := machine.Claim().Pending
				return p != nil && !p.DeliveredAt.IsZero()
			})

			// The agent is already in the turn a human's steer started: that turn confirms the task,
			// and the agent then refuses the acknowledged prompt, which takes the task back.
			if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: implementer}); err != nil {
				t.Fatalf("start the steer's turn: %v", err)
			}
			if err := machine.Handle(ctx, supervise.StreamLateRefusal{Claim: implementer, DeliveryID: machine.Claim().Pending.ID,
				Error: "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."}); err != nil {
				t.Fatalf("refuse the acknowledged prompt: %v", err)
			}

			// Inside that turn the implementer finishes round 2, and the transition suspends it.
			apply("handoff:implementer:implementing:2", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RoleImplementer, Claim: implementer, Commit: "impl-round-2"})
			due("the move to testing")
			if got := machine.Claim().State; got != supervise.StateSuspended {
				t.Fatalf("after round 2 the implementer is %s, want suspended", got)
			}

			// The tester passes, and the reviewer decides.
			apply("handoff:tester:testing:2", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RoleTester, Claim: tester, Commit: "test-round-2", Verdict: "pass"})
			due("the move to reviewing")
			review := tc.review
			review.Repo, review.Number, review.CommitID, review.HeadSHA = "acme/widgets", 118, head, head
			apply("review:after-round-2", review)
			apply("handoff:reviewer:reviewing:2", intake.HandoffComplete{Generation: 1, Issue: issue.Key, Role: claim.RoleReviewer,
				Claim: "reviewer", Commit: "review-round-2"})
			resumes := len(rt.CallsOf("Resume"))
			prompted := len(conn.Prompts())
			due("the implementer's next start")
			if got := len(rt.CallsOf("Resume")) - resumes; got != 1 {
				t.Fatalf("the implementer's next start resumed it %d times, want once", got)
			}
			ready()
			due("the rows still waiting")
			testwait.Eventually(t, "the resumed implementer to be handed a task", func() bool { return len(conn.Prompts()) > prompted })

			handed := conn.Prompts()[prompted:]
			if !strings.Contains(handed[0].Message, tc.want) {
				t.Fatalf("resumed for its next start, the implementer was first handed %q, want the task naming %q", handed[0].Message, tc.want)
			}
			for _, p := range handed {
				if strings.Contains(p.Message, "round 2") {
					t.Fatalf("resumed for its next start, the implementer was handed the finished round's task %q", p.Message)
				}
			}
		})
	}
}

func TestOutboxSuperviseReturnsProvisioningFailure(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RolePlanner, Generation: issue.Generation}, time.Now())
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{}, errors.New("provision failed")
		},
	}

	if err := runner.execute(context.Background(), row); err == nil || !strings.Contains(err.Error(), "provision failed") {
		t.Fatalf("supervise failure = %v, want provisioning failure", err)
	}
}

func TestTerminalReplayAfterPersistedFailureHoldsOnceAndEmitsOneWorkerDiedNotice(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Planner failure", Phase: phase.Planning, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Limits.LaunchFailures = 1
	token, err := claim.NewToken("legion", issue.Key, claim.RolePlanner)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	runtime.ScriptSpawn(fake.SpawnResult{Err: errors.New("pane launch failed")})
	machine, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RolePlanner, State: supervise.StateQueued,
	}, "")
	if err != nil {
		t.Fatalf("create planner claim: %v", err)
	}
	if err := machine.Handle(context.Background(), supervise.RequestSpawn{Claim: token}); err == nil {
		t.Fatal("launch with exhausted budget returned no error")
	}
	persisted := machine.Claim()
	if persisted.State != supervise.StateFailed {
		t.Fatalf("persisted claim state = %s, want failed before the terminal hook", persisted.State)
	}

	engine := workflow.New(records, workflow.Config{Project: issue.Project}, quietLogger())
	restarted := &workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger()}
	if err := restarted.replayTerminal(context.Background(), []supervise.Claim{persisted}); err != nil {
		t.Fatalf("replay persisted failure: %v", err)
	}
	if err := restarted.replayTerminal(context.Background(), []supervise.Claim{persisted}); err != nil {
		t.Fatalf("repeat replay persisted failure: %v", err)
	}

	var held string
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		stored, err := records.Issue(context.Background(), tx, issue.Key)
		if err != nil {
			return err
		}
		if stored == nil {
			return errors.New("workflow issue missing")
		}
		held = string(stored.Phase)
		return nil
	}); err != nil {
		t.Fatalf("read held workflow issue: %v", err)
	}
	if held != string(phase.Held) {
		t.Fatalf("workflow phase after replay = %s, want held", held)
	}
	rows, err := pool.Query(context.Background(), "select payload->>'kind' from outbox where kind = $1 order by id", string(record.OutboxKindNotice))
	if err != nil {
		t.Fatalf("read failure notices: %v", err)
	}
	defer rows.Close()
	var notices []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan failure notice: %v", err)
		}
		notices = append(notices, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate failure notices: %v", err)
	}
	if fmt.Sprint(notices) != "[held worker-died]" {
		t.Fatalf("failure notices = %v, want exactly held and worker-died once", notices)
	}
}

func TestTerminalReplayAppliesPersistedReadyFactOnce(t *testing.T) {
	pool := isolatedOutboxPool(t)
	handler := &outboxFactHandler{}
	restarted := &workflowRuntime{pool: pool, handlers: []intake.Handler{handler}, log: quietLogger()}
	persisted := supervise.Claim{
		Token: "legion-LEGION-208-planner", Issue: "LEGION-208", Role: claim.RolePlanner, Generation: 4, State: supervise.StateReady,
	}

	if err := restarted.replayTerminal(context.Background(), []supervise.Claim{persisted}); err != nil {
		t.Fatalf("replay persisted ready claim: %v", err)
	}
	if err := restarted.replayTerminal(context.Background(), []supervise.Claim{persisted}); err != nil {
		t.Fatalf("repeat replay persisted ready claim: %v", err)
	}
	if got := handler.facts(); len(got) != 1 || got[0] != (intake.ClaimReady{Issue: persisted.Issue, Role: persisted.Role}) {
		t.Fatalf("ready facts = %#v, want exactly one replayed ClaimReady", got)
	}
}

// The answer supervise asks for before it sends a queued task: is the issue still in the phase
// the task was queued for. The role is not what is compared — one role runs several phases — and
// a task of no phase never reaches this answer at all, which is supervise's own rule.
func TestPhaseHoldsAnswersFromTheIssuesCurrentPhase(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{
		Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "phase holds",
		Phase: phase.Implementing, Status: "in_progress", Rank: "00000",
	})
	holds := (&workflowRuntime{pool: pool, records: records}).phaseHolds

	for _, tc := range []struct {
		name      string
		issue     string
		queuedFor phase.Phase
		want      bool
	}{
		{name: "the issue's own phase", issue: "LEGION-208", queuedFor: phase.Implementing, want: true},
		{name: "a phase the issue has not reached", issue: "LEGION-208", queuedFor: phase.Testing, want: false},
		// The implementer works implementing, retro and the production check: a task written for
		// one of them is not the work of another, which the role alone cannot tell.
		{name: "another phase of the same role", issue: "LEGION-208", queuedFor: phase.Retro, want: false},
		// The workflow never deletes an issue, so no record means the claim is not the workflow's:
		// an operator spawn, whose task the workflow has no standing to drop.
		{name: "an issue the workflow never recorded", issue: "LEGION-999", queuedFor: phase.Implementing, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := holds(context.Background(), tc.issue, tc.queuedFor)
			if err != nil {
				t.Fatalf("phaseHolds: %v", err)
			}
			if got != tc.want {
				t.Fatalf("phaseHolds(%s, %s) = %v, want %v", tc.issue, tc.queuedFor, got, tc.want)
			}
		})
	}

	putOutboxIssue(t, pool, records, record.Issue{
		Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "phase holds",
		Phase: phase.Testing, Status: "testing", Rank: "00000",
	})
	implementing, err := holds(context.Background(), "LEGION-208", phase.Implementing)
	if err != nil {
		t.Fatalf("phaseHolds after the phase moved: %v", err)
	}
	if implementing {
		t.Fatal("the implementing task still holds after the issue reached testing")
	}
}

func putOutboxIssue(t *testing.T, pool *pgxpool.Pool, records record.Store, issue record.Issue) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutIssue(context.Background(), tx, issue)
	}); err != nil {
		t.Fatalf("put workflow issue: %v", err)
	}
}

type outboxPublisher struct {
	mu      sync.Mutex
	publish []outboxPublish
	err     error
}

type outboxPublish struct {
	topic, key string
}

func (p *outboxPublisher) Publish(_ context.Context, topic, _ string, _ any, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publish = append(p.publish, outboxPublish{topic: topic, key: key})
	return p.err
}

func (p *outboxPublisher) topics() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	topics := make([]string, len(p.publish))
	for i, published := range p.publish {
		topics[i] = published.topic
	}
	return topics
}

func (p *outboxPublisher) keys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, len(p.publish))
	for i, published := range p.publish {
		keys[i] = published.key
	}
	return keys
}

type outboxFactHandler struct {
	mu       sync.Mutex
	recorded []intake.Fact
	err      error
}

func (h *outboxFactHandler) Apply(_ context.Context, _ pgx.Tx, fact intake.Fact) (intake.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recorded = append(h.recorded, fact)
	return intake.Result{}, h.err
}

func (h *outboxFactHandler) facts() []intake.Fact {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]intake.Fact(nil), h.recorded...)
}

type outboxTokens struct{}

func (outboxTokens) Token(context.Context, appauth.AppRole, string) (appauth.Lease, error) {
	return appauth.Lease{Token: "outbox-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type outboxSpecs struct{}

func (outboxSpecs) SpawnSpec(context.Context, supervise.Claim) (runtime.SpawnSpec, error) {
	return runtime.SpawnSpec{}, nil
}

type outboxClaimStore struct {
	mu         sync.Mutex
	claims     map[claim.Token]supervise.Claim
	deliveries map[claim.Token]supervise.Delivery
}

func (s *outboxClaimStore) PutClaim(_ context.Context, c supervise.Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claims == nil {
		s.claims = map[claim.Token]supervise.Claim{}
	}
	s.claims[c.Token] = c
	return nil
}

func (s *outboxClaimStore) PutDelivery(_ context.Context, token claim.Token, delivery supervise.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deliveries == nil {
		s.deliveries = map[claim.Token]supervise.Delivery{}
	}
	s.deliveries[token] = delivery
	return nil
}

func (s *outboxClaimStore) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, d supervise.Delivery) error {
	if err := s.PutClaim(ctx, c); err != nil {
		return err
	}
	return s.PutDelivery(ctx, c.Token, d)
}

func (s *outboxClaimStore) RetireDelivery(ctx context.Context, c supervise.Claim, _ string) error {
	if err := s.PutClaim(ctx, c); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deliveries, c.Token)
	return nil
}

func newOutboxSupervisor(t *testing.T, project, stateDir string) (*supervisor, *fake.Runtime) {
	t.Helper()
	rt := fake.NewRuntime()
	sup := newSupervisor(context.Background(), nil, project, stateDir, quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: &outboxClaimStore{}, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:   supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second},
	}
	t.Cleanup(sup.stop)
	return sup, rt
}

// A closed tree set back to todo runs again. Its linger expiry retired every claim and removed the
// workspaces; admission re-admits the root, and the architect's start must provision the workspace
// again and relaunch the kept session. Before, the start met a retired claim and finished having
// done nothing: the tree held a slot, in_progress, with no architect.
func TestAClosedTreeSetBackToTodoRelaunchesItsArchitect(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	until := time.Now().Add(-time.Minute)
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 3, Status: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, root)
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	provisioned, removed := 0, 0
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, 2, "legion", quietLogger())
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: root.Key, Status: "todo"}}, handlers: []intake.Handler{engine, admission},
		now: func() time.Time { return time.Now().Add(time.Hour) }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			provisioned++
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
		remove: func(context.Context, workspace.Workspace) error { removed++; return nil },
	}

	// The tree's architect ran on a session before the tree closed.
	if err := runner.execute(ctx, mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect, Generation: root.Generation}, time.Now())); err != nil {
		t.Fatalf("start the architect: %v", err)
	}
	token, err := claim.NewToken("legion", root.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _ := sup.Machine(token)
	if err := machine.Handle(ctx, supervise.RequestRegister{Claim: token, Generation: machine.Claim().Generation, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"}); err != nil {
		t.Fatalf("register the architect: %v", err)
	}

	// Linger expires: every claim stops and the workspace goes.
	if _, err := intake.ApplyFact(ctx, pool, "outbox", "linger:LEGION-208:3", intake.LingerExpired{Issue: root.Key, Generation: 3}, engine, admission); err != nil {
		t.Fatalf("apply linger expiry: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the linger effects: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateRetired || removed != 1 {
		t.Fatalf("after linger expiry the architect is %s with %d workspace removals, want retired and one", got, removed)
	}

	// The human sets the closed root back to todo.
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "todo-again", intake.DispatchIssue{Key: root.Key, Seq: 6, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank}, engine, admission); err != nil {
		t.Fatalf("apply the todo: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the re-admission effects: %v", err)
	}
	got := machine.Claim()
	if got.State != supervise.StateLaunching || provisioned != 2 {
		t.Fatalf("re-admitted architect is %s after %d provisions, want launching on a provisioned workspace", got.State, provisioned)
	}
	resumes := runtime.CallsOf("Resume")
	if len(resumes) != 1 || resumes[0].Spec.ResumeSessionFile != "/tmp/architect.jsonl" {
		t.Fatalf("resume calls = %+v, want the kept session relaunched once", resumes)
	}
}

// A tree's workspace removal serves the generation whose linger expired. One still waiting out its
// backoff when the tree is re-admitted finishes without acting: the workspace there now is the next
// generation's, just provisioned for the relaunched architect.
func TestAnEarlierGenerationsWorkspaceRemovalLeavesTheReadmittedTreesWorkspace(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	until := time.Now().Add(-time.Minute)
	root := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Done, Generation: 3, Status: "done", Rank: "U", LingerUntil: &until, LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, root)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	engine := workflow.New(records, workflow.Config{Project: "legion"}, quietLogger())
	admission := admit.New(records, 2, "legion", quietLogger())
	clock := time.Now().Add(time.Hour)
	removals, busy := 0, true
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: root.Key, Status: "todo"}}, handlers: []intake.Handler{engine, admission},
		now: func() time.Time { return clock }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
		remove: func(context.Context, workspace.Workspace) error {
			removals++
			if busy {
				return errors.New("jj workspace forget: the repository is locked")
			}
			return nil
		},
	}
	if _, err := intake.ApplyFact(ctx, pool, "outbox", "linger:LEGION-208:3", intake.LingerExpired{Issue: root.Key, Generation: 3}, engine, admission); err != nil {
		t.Fatalf("apply linger expiry: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the linger effects: %v", err)
	}
	if removals != 1 || workspaceRemovals(t, pool) != 1 {
		t.Fatalf("after a failed removal: %d attempts, %d rows, want one attempt and its row waiting", removals, workspaceRemovals(t, pool))
	}

	busy = false
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "todo-again", intake.DispatchIssue{Key: root.Key, Seq: 6, Type: "issue.updated", Status: "todo", Title: root.Title, Rank: root.Rank}, engine, admission); err != nil {
		t.Fatalf("apply the todo: %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the re-admission effects: %v", err)
	}
	if removals != 1 || workspaceRemovals(t, pool) != 0 {
		t.Fatalf("after re-admission: %d removal attempts, %d rows, want the first generation's row finished without removing", removals, workspaceRemovals(t, pool))
	}
}

// Under a runtime that provisions each claim's workspace in its own pod (C4), the daemon provisions
// none and removes none: a start spawns the claim with no workspace on the host, a retired claim's
// relaunch provisions nothing, and a tree's removal row finishes without acting.
func TestARuntimeThatProvisionsInItsPodsLeavesTheHostWithoutWorkspaces(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	rt.InPod = true
	provisions, removals := 0, 0
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets", log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			provisions++
			return workspace.Workspace{}, nil
		},
		remove: func(context.Context, workspace.Workspace) error { removals++; return nil },
	}

	start := record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: issue.Generation}
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, start, time.Now())); err != nil {
		t.Fatalf("start the architect: %v", err)
	}
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "tree_close", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: issue.Generation}, time.Now())); err != nil {
		t.Fatalf("close the tree: %v", err)
	}
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, record.WorkspaceRemove{Generation: issue.Generation}, time.Now())); err != nil {
		t.Fatalf("remove the workspace: %v", err)
	}
	if err := runner.execute(ctx, mustOutboxRow(t, issue.Key, start, time.Now())); err != nil {
		t.Fatalf("relaunch the retired architect: %v", err)
	}
	if launches := len(rt.CallsOf("Spawn")) + len(rt.CallsOf("Resume")); launches != 2 {
		t.Errorf("the runtime saw %d launches, want the start and the relaunch", launches)
	}
	if provisions != 0 || removals != 0 {
		t.Errorf("the daemon provisioned %d and removed %d host workspaces, want none", provisions, removals)
	}
	if _, err := os.Stat(filepath.Join(runner.stateDir, "workspaces")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the state directory has a workspaces directory (%v), want none", err)
	}
}

func workspaceRemovals(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'workspace_remove'`).Scan(&count); err != nil {
		t.Fatalf("count workspace removal rows: %v", err)
	}
	return count
}
