package daemon

import (
	"context"
	"errors"
	"fmt"
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
	runner := &outbox{dispatch: client}

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
	runner := &outbox{dispatch: client}

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

	if err := (&outbox{pool: pool, records: records, notices: publisher}).execute(context.Background(), row); err != nil {
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

	if err := (&outbox{pool: pool, records: records, notices: publisher}).execute(context.Background(), row); err == nil || !strings.Contains(err.Error(), "listener unavailable") {
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
	runner := &outbox{pool: pool, records: records, dispatch: client, handlers: []intake.Handler{handler}}

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
			runner := &outbox{pool: pool, records: record.NewStore(), dispatch: &outboxDispatch{approval: tc.approval}, handlers: []intake.Handler{handler}}
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
	runner := &outbox{pool: pool, handlers: []intake.Handler{handler}}

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
	row := mustOutboxRow(t, "LEGION-208", record.WorkspaceRemove{}, time.Now())
	var removed workspace.Workspace
	runner := &outbox{
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
	runner := &outbox{
		pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
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
	if err := runner.execute(context.Background(), mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "stop", Tree: issue.Tree, Role: claim.RolePlanner, Generation: issue.Generation}, time.Now())); err != nil {
		t.Fatalf("stop planner: %v", err)
	}
	if got := machine.Claim().State; got != supervise.StateRetired {
		t.Fatalf("claim state after stop = %s, want retired", got)
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
	runner := &outbox{pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets"}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer, Task: "Continue. Reason: retry held phase.", Generation: issue.Generation}, time.Now())
	row.ID = 91

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("start the failed claim: %v", err)
	}
	if got := machine.Claim(); got.State != supervise.StateLaunching || got.Budgets != (supervise.Budgets{}) || got.Pending == nil || got.Pending.ID != "outbox:91" {
		t.Fatalf("retried claim = %+v, want launching with fresh budgets and the retry task pending", got)
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
		pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
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

func (s *outboxClaimStore) RetireDelivery(_ context.Context, token claim.Token, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deliveries, token)
	return nil
}

func newOutboxSupervisor(t *testing.T, project, stateDir string) (*supervisor, *fake.Runtime) {
	t.Helper()
	rt := fake.NewRuntime()
	sup := newSupervisor(context.Background(), nil, project, stateDir, quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: &outboxClaimStore{}, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:   supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, StopGrace: time.Second},
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
		pool: pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), repo: "acme/widgets",
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
