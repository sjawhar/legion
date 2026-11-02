package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// freshSessionRig is one issue implementing, its implementer running on the session oldSession,
// and the workflow behind it: the engine the facts are applied through, the outbox that runs what
// the workflow queues, and the runtime the launches go to.
type freshSessionRig struct {
	t           *testing.T
	pool        *pgxpool.Pool
	records     record.Store
	issue       record.Issue
	sup         *supervisor
	rt          *fake.Runtime
	engine      *workflow.Engine
	runner      *outbox
	operatorAPI http.Handler
	implementer claim.Token
	machine     *supervise.Machine
	conn        *fake.Conn
}

const (
	oldSession     = "ses-refused"
	oldSessionFile = "/tmp/ses-refused.jsonl"
	newSession     = "ses-fresh"
	newSessionFile = "/tmp/ses-fresh.jsonl"
	phaseTask      = "Continue Workflow. Issue: LEGION-208. Phase: implementing."
	// freshSessionOperatorToken is the bearer `legion claims` sends, which the operator routes
	// compare against.
	freshSessionOperatorToken = "operator-token"
)

func newFreshSessionRig(t *testing.T) *freshSessionRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, pool, records, issue)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.PhaseHolds = (&workflowRuntime{pool: pool, records: records}).phaseHolds
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories}, quietLogger())
	sup.OnTerminal((&workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	rig := &freshSessionRig{t: t, pool: pool, records: records, issue: issue, sup: sup, rt: rt, engine: engine}
	trees := outboxTreeStore(t, pool, issue.Key)
	workspaceDir := t.TempDir()
	rig.runner = &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
		stateDir: t.TempDir(), tokens: outboxTokens{}, notices: &outboxPublisher{}, handlers: []intake.Handler{engine}, now: time.Now,
		trees: trees,
		// A retired claim's start provisions its workspace again, as it does for a re-admitted tree.
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: workspaceDir, Bookmark: "legion/" + issue.Key}, nil
		}}
	// The operator's own routes, wired as the daemon wires them: what `legion claims` talks to.
	rig.operatorAPI = api.NewServer("127.0.0.1", 8437, api.Options{
		Supervisor: sup, BootTokens: api.NewBootTokens(trees), Project: "legion", DispatchProject: "LEGION",
		OperatorToken: freshSessionOperatorToken, Controller: trees, Trees: trees, Releaser: rt, Pool: pool, Record: records,
		Handlers: []intake.Handler{engine}, RoleFor: workflow.RoleFor, Log: quietLogger(),
	}).Handler
	rig.implementer = mustClaimToken(t, issue.Key, claim.RoleImplementer)
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: rig.implementer, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the implementer: %v", err)
	}
	rig.machine = machine
	rig.conn = fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(rig.implementer, rig.conn)
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: rig.implementer}); err != nil {
		t.Fatalf("spawn the implementer: %v", err)
	}
	rig.register(oldSession, oldSessionFile)
	rig.must(supervise.RequestDeliver{Claim: rig.implementer, Task: phaseTask, ID: "outbox:7", Phase: phase.Implementing, Generation: issue.Generation})
	return rig
}

// register drives the launched claim through its shim's hello, its agent's registration on session
// and its ready, at the claim's current generation, and waits out the send its ready starts.
func (r *freshSessionRig) register(session, file string) {
	r.t.Helper()
	generation := r.machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.implementer, Generation: generation},
		supervise.RequestRegister{Claim: r.implementer, Generation: generation, Session: session, SessionFile: file},
		supervise.RequestReady{Claim: r.implementer, Generation: generation, Session: session},
	} {
		if err := r.machine.Handle(context.Background(), ev); err != nil {
			r.t.Fatalf("boot the implementer: handle %T: %v", ev, err)
		}
	}
	r.machine.Wait()
}

func (r *freshSessionRig) must(ev supervise.Event) {
	r.t.Helper()
	if err := r.machine.Handle(context.Background(), ev); err != nil {
		r.t.Fatalf("handle %T: %v", ev, err)
	}
}

// operator is one `legion claims` request against the operator routes: the command's own HTTP
// call, with its bearer. A nil body sends none, as a subcommand with no flags of its own does.
func (r *freshSessionRig) operator(method, target string, body any) *httptest.ResponseRecorder {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Authorization", "Bearer "+freshSessionOperatorToken)
	recorder := httptest.NewRecorder()
	r.operatorAPI.ServeHTTP(recorder, request)
	return recorder
}

// refuseEveryTurn works the claim's pending task through turns the provider refuses, until the
// trailing run of refusals reaches the launch failure limit and the claim fails.
func (r *freshSessionRig) refuseEveryTurn(message string) {
	r.t.Helper()
	for turn := 1; turn <= r.sup.deps.Limits.LaunchFailures; turn++ {
		generation := r.machine.Claim().Generation
		r.must(supervise.StreamTurnStart{Claim: r.implementer})
		r.must(supervise.RequestTurnOutcome{Claim: r.implementer, Generation: generation, Sequence: uint64(turn), Refused: true, Message: message})
		if r.machine.Claim().State == supervise.StateFailed {
			return
		}
		r.must(supervise.StreamTurnEnd{Claim: r.implementer})
	}
}

// told is one notice the workflow queued, whether for the issue's architect or for the
// controller: its kind and the reason it carries.
type told struct{ kind, reason string }

// notices are the notices the workflow has queued, in the order it queued them.
func (r *freshSessionRig) notices() []told {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), "select id, kind, issue, payload from outbox where kind in ('notice', 'controller_notice') order by id")
	if err != nil {
		r.t.Fatalf("read the notice rows: %v", err)
	}
	defer rows.Close()
	var notices []told
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("scan a notice row: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		switch notice := payload.(type) {
		case record.Notice:
			notices = append(notices, told{kind: string(notice.Kind), reason: notice.Reason})
		case record.ControllerNotice:
			notices = append(notices, told{kind: string(notice.Kind) + " (controller)", reason: notice.Reason})
		default:
			r.t.Fatalf("outbox row %d is no notice: %#v", row.ID, payload)
		}
	}
	return notices
}

// recordedPhase is the issue's phase and the phase it is held from, as the record holds them.
func (r *freshSessionRig) recordedPhase() (phase.Phase, phase.Phase) {
	r.t.Helper()
	var current phase.Phase
	var from *phase.Phase
	if err := r.pool.QueryRow(context.Background(), "select phase, held_from from issues where key = $1", r.issue.Key).Scan(&current, &from); err != nil {
		r.t.Fatalf("read the issue's phase: %v", err)
	}
	if from == nil {
		return current, ""
	}
	return current, *from
}

// A claim the provider refused out of its budget fails; its issue is held, so the tree's architect
// can retry it. That one retry must reach a working agent: a new session, because every turn of
// the recorded one was refused, and the phase's task in it.
func TestAnArchitectsRetryOfARefusedClaimStartsANewSessionWithThePhaseTask(t *testing.T) {
	rig := newFreshSessionRig(t)
	ctx := context.Background()
	refusal := "Refusal (reasoning_extraction): this request was blocked"

	rig.refuseEveryTurn(refusal)

	if got := rig.machine.Claim(); got.State != supervise.StateFailed || got.Session != oldSession {
		t.Fatalf("after its refused turns the implementer is %s on session %q, want failed with the refused session still recorded", got.State, got.Session)
	}
	current, from := rig.recordedPhase()
	if current != phase.Held || from != phase.Implementing {
		t.Fatalf("after the refusals the issue is in %s held from %q, want held from implementing", current, from)
	}
	notices := rig.notices()
	var kinds []string
	for _, notice := range notices {
		kinds = append(kinds, notice.kind)
		if !strings.Contains(notice.reason, refusal) {
			t.Errorf("the %s notice says %q, want the provider's refusal in it", notice.kind, notice.reason)
		}
	}
	if want := []string{"held", "held (controller)", "worker-died"}; !slices.Equal(kinds, want) {
		t.Fatalf("the failure queued %v, want %v", kinds, want)
	}

	// The one command: the architect's retry of the held phase.
	if _, err := intake.ApplyFact(ctx, rig.pool, "api", "retry:"+rig.issue.Key,
		intake.RetryOrEscalate{Issue: rig.issue.Key, Decision: intake.RetryDecision}, rig.engine); err != nil {
		t.Fatalf("the architect's retry: %v", err)
	}
	if err := rig.runner.RunOnce(ctx); err != nil {
		t.Fatalf("run what the retry queued: %v", err)
	}
	rig.machine.Wait()

	if got := rig.machine.Claim(); got.State != supervise.StateLaunching || got.Session != "" || got.SessionFile != "" {
		t.Fatalf("after the retry the implementer is %s on session %q (%s), want it launching with no session to resume", got.State, got.Session, got.SessionFile)
	}
	if resumes := resumesOf(rig.rt, rig.implementer); len(resumes) != 0 {
		t.Fatalf("the retry resumed %+v, want the refused session left alone", resumes)
	}
	spawns := rig.rt.CallsOf("Spawn")
	if len(spawns) != 2 || spawns[1].Spec.ResumeSessionFile != "" {
		t.Fatalf("the retry's launch = %+v, want a spawn resuming no session file", spawns)
	}
	rig.register(newSession, newSessionFile)
	if got := rig.machine.Claim(); got.Session != newSession || got.Session == oldSession {
		t.Fatalf("the retried implementer registered session %q, want the new %q", got.Session, newSession)
	}
	// The task of the retried phase, behind the sentence that says this session replaced one the
	// provider refused: a worker reading an empty transcript would otherwise take the task for a
	// fresh start and do again what the abandoned session already pushed.
	prompts := rig.conn.Prompts()
	task := "Continue Workflow. Issue: LEGION-208. Phase: implementing. Reason: retry held phase."
	if len(prompts) != 2 || !strings.HasSuffix(prompts[1].Message, task) {
		t.Fatalf("the new session was prompted %+v, want it to end with the retried phase's task %q", prompts, task)
	}
	wantsAbandonment(t, prompts[1].Message, refusal)
}

// An operator who stops a worker whose agent can do nothing — a provider that refuses its session,
// an agent wedged in a way no budget counts — retires that claim, and a stop is not a death: the
// issue is left in its phase, holding nothing. From there `legion claims retry --fresh-session` is
// the one command that must give that phase a working agent: the stopped claim forgets the session
// it recorded, the workflow starts its phase again, and the new session is given the phase's task.
func TestTheOperatorsFreshSessionRetryOfAStoppedWorkerStartsANewSessionWithThePhaseTask(t *testing.T) {
	rig := newFreshSessionRig(t)
	ctx := context.Background()
	route := "/legion/v1/operator/claims/" + string(rig.implementer)

	if recorder := rig.operator(http.MethodPost, route+"/stop", nil); recorder.Code != http.StatusOK {
		t.Fatalf("the operator's stop = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateRetired || got.Session != oldSession {
		t.Fatalf("after the operator's stop the implementer is %s on session %q, want it retired with its session still recorded", got.State, got.Session)
	}
	if current, from := rig.recordedPhase(); current != phase.Implementing || from != "" {
		t.Fatalf("after the operator's stop the issue is in %s held from %q, want implementing and not held: a stop is not a death", current, from)
	}

	// The one command: the operator's retry in a new session.
	recorder := rig.operator(http.MethodPost, route+"/retry", api.RetryRequest{FreshSession: true})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("retry --fresh-session of the stopped implementer = %d, want 202; body %s", recorder.Code, recorder.Body)
	}
	if err := rig.runner.RunOnce(ctx); err != nil {
		t.Fatalf("run what the retry queued: %v", err)
	}
	rig.machine.Wait()

	if got := rig.machine.Claim(); got.State != supervise.StateLaunching || got.Session != "" || got.SessionFile != "" {
		t.Fatalf("after the retry the implementer is %s on session %q (%s), want it launching with no session to resume", got.State, got.Session, got.SessionFile)
	}
	if resumes := resumesOf(rig.rt, rig.implementer); len(resumes) != 0 {
		t.Fatalf("the retry resumed %+v, want the stopped session left alone", resumes)
	}
	spawns := rig.rt.CallsOf("Spawn")
	if len(spawns) != 2 || spawns[1].Spec.ResumeSessionFile != "" {
		t.Fatalf("the retry's launch = %+v, want a spawn resuming no session file", spawns)
	}
	rig.register(newSession, newSessionFile)
	if got := rig.machine.Claim(); got.Session != newSession || got.Session == oldSession {
		t.Fatalf("the retried implementer registered session %q, want the new %q", got.Session, newSession)
	}
	prompts := rig.conn.Prompts()
	if len(prompts) != 2 || !strings.HasSuffix(prompts[1].Message, phaseTask) {
		t.Fatalf("the new session was prompted %+v, want it to end with its phase's task", prompts)
	}
	wantsAbandonment(t, prompts[1].Message, "the retry asked for a new session")
}

// `--fresh-session` is the one retry, not two steps: the claim forgets its session inside the
// retry the workflow's start makes, so a restart the workflow refuses leaves the session where
// the operator still has it. Here the tree lingers, which holds every member where it stood, so
// the restart refuses and the stopped worker keeps the session it recorded — where forgetting it
// before applying the fact would have discarded it for a retry that never happened.
func TestAFreshSessionRetryTheWorkflowRefusesLeavesTheSession(t *testing.T) {
	rig := newFreshSessionRig(t)
	route := "/legion/v1/operator/claims/" + string(rig.implementer)

	if recorder := rig.operator(http.MethodPost, route+"/stop", nil); recorder.Code != http.StatusOK {
		t.Fatalf("the operator's stop = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	lingering := rig.issue
	until := time.Now().Add(time.Hour)
	lingering.LingerUntil = &until
	putOutboxIssue(t, rig.pool, rig.records, lingering)
	spawns := len(rig.rt.CallsOf("Spawn"))

	recorder := rig.operator(http.MethodPost, route+"/retry", api.RetryRequest{FreshSession: true})

	refused := "the tree LEGION-208 is lingering after it left the workflow, so the operator's restart of the implementer of LEGION-208 changed nothing"
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), refused) {
		t.Fatalf("retry --fresh-session while the tree lingers = %d, want 409 %q; body %s", recorder.Code, refused, recorder.Body)
	}
	got := rig.machine.Claim()
	if got.Session != oldSession || got.SessionFile != oldSessionFile {
		t.Fatalf("after the refused retry the claim records session %q (%s), want the session the retry never started kept", got.Session, got.SessionFile)
	}
	if got.Abandoned.Session != "" {
		t.Fatalf("the claim records %+v as abandoned, want nothing abandoned by a retry the workflow refused", got.Abandoned)
	}
	if after := rig.rt.CallsOf("Spawn"); len(after) != spawns {
		t.Fatalf("the refused retry launched %+v, want nothing", after[spawns:])
	}
}

// wantsAbandonment is the check both retries make of the fresh session's first prompt: it names
// the session that was abandoned and why — the provider's own words for a refused one, the
// context size among them where the provider gave one — and the two things that outlived that
// session, the issue's branch and the handoffs committed on it.
func wantsAbandonment(t *testing.T, prompt, why string) {
	t.Helper()
	for _, want := range []string{
		oldSession, "abandoned", why,
		"legion/" + "LEGION-208", ".legion/LEGION-208/", "plan.json", "implement.json", "test.json", "review.json",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the fresh session's first prompt does not name %q:\n%s", want, prompt)
		}
	}
}
