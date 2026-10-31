package daemon

// The fleet-wide update round (INFRA-419, folded into INFRA-484): one change to the pod template —
// the deployment's session store here — drifts the pod of every issue of the project at once. Each
// issue's pod is a round of its own: it is replaced only once its own workers have answered, and
// an answer in one issue's pod replaces no other issue's. Nothing of the round suspends a root
// architect, parks an issue or sends a root to await a person, and the daemon goes on serving every
// claim in an open round — a worker tasked to push its work completes its phase through the same
// route it would on any other day (api's handoff/complete).
//
// The rig is podupdate_test.go's over two issues rather than one: real machines over a real
// Postgres with the runtime faked, ticked by hand one sweep at a time. The completion is driven
// through the daemon's own HTTP handler over that same store, with the workflow engine behind it,
// so what accepts it is the route the `legion` tool posts to and not a test's stand-in.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

const (
	// The two issues the one template change drifts at once: each a root of its own, each with its
	// own architect, its own phase workers and its own pod.
	fleetIssueA = "LEGION-419"
	fleetIssueB = "LEGION-420"
	fleetPodA   = "6b1d0c4e-7a92-4c1f-8d35-2f6b9c0ad417"
	fleetPodB   = "c8f52a71-04b3-4de6-9a7c-15d8e3b6027f"
	// fleetDrift is what the runtime answers for every pod of the fleet after the one change: the
	// deployment moved the session store its pods are created under, which no running pod can be
	// changed to carry (the Sandbox runtime's movedSessionStore).
	fleetDrift = "the session store volume, now postgres key=session-dsn"
)

// fleetIssue is one issue the fleet round covers: its key, the phase its record stands in, the pod
// every role of it runs in, and the roles that run there. Every role is a registered worker on
// this daemon's contract, which is what a fleet-wide drift finds.
type fleetIssue struct {
	key   string
	phase phase.Phase
	pod   string
	roles []claim.Role
}

// fleetClaim names one claim of the fleet: the issue it is of and the role it runs.
type fleetClaim struct {
	issue string
	role  claim.Role
}

type fleetRig struct {
	t       *testing.T
	ctx     context.Context
	s       *supervision
	sup     *supervisor
	rt      *fake.Runtime
	pool    *pgxpool.Pool
	records record.Store
	clock   *heldClock
	grants  *credential.Grants
	// api is the daemon's own HTTP handler over the same store: the completion route under test.
	api    http.Handler
	tokens map[fleetClaim]claim.Token
	pods   map[string]string
}

func newFleetRig(t *testing.T, issues ...fleetIssue) *fleetRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	dsn := pool.Config().ConnString()
	tokens := map[fleetClaim]claim.Token{}
	pods := map[string]string{}
	for _, issue := range issues {
		pods[issue.key] = issue.pod
		claims := make([]supervise.Claim, 0, len(issue.roles))
		for _, role := range issue.roles {
			token := mustClaimToken(t, issue.key, role)
			tokens[fleetClaim{issue: issue.key, role: role}] = token
			claims = append(claims, supervise.Claim{
				Token: token, Project: podUpdateProject, Tree: issue.key, Issue: issue.key, Role: role,
				Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion,
				Session: "ses_" + issue.key + "_" + string(role), SessionFile: "/sessions/" + issue.key + "-" + string(role) + ".jsonl",
				CapabilityHash: []byte(issue.key + ":" + string(role)), ServingGeneration: 1,
				Locator: podUpdateLocator(token, issue.key, issue.pod),
			})
		}
		seedWorkflowTree(t, dsn, podUpdateProject, record.Issue{
			Key: issue.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "LEGION", Tree: issue.key, Title: "The fleet round", Phase: issue.phase,
			Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
		}, claims...)
	}

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	clock := &heldClock{at: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	rt := fake.NewRuntime()
	rt.Now = clock.Now
	conns := fake.NewConns()
	sup := newSupervisor(ctx, st, podUpdateProject, t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: conns, Store: st, Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
		Limits:          supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:        supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract:  api.DaemonAPIVersion,
		PodUpdateRetask: podUpdateRetask,
	}
	t.Cleanup(sup.stop)
	stored, err := sup.Claims(ctx)
	if err != nil {
		t.Fatalf("read the seeded claims: %v", err)
	}
	if _, err := sup.restore(ctx, stored); err != nil {
		t.Fatalf("restore the seeded claims: %v", err)
	}
	for _, token := range tokens {
		conns.Register(token, fake.NewConn())
	}
	records := record.NewStore()
	rig := &fleetRig{t: t, ctx: ctx, sup: sup, rt: rt, pool: pool, records: records, clock: clock,
		grants: credential.New(nil), tokens: tokens, pods: pods}
	rig.s = &supervision{
		cfg: config.Config{Project: "LEGION", PodUpdateRetask: podUpdateRetask, PodUpdateNotice: podUpdateNotice},
		log: quietLogger(), plan: plan{clock: clock, project: podUpdateProject, pluginRelaunchable: true, orphanSweep: orphanSweepInterval},
		runtime: rt, supervisor: sup,
		enqueue: (&workflowRuntime{pool: pool, records: records, log: quietLogger()}).enqueueOutbox,
	}
	engine := workflow.New(records, workflow.Config{Project: podUpdateProject, Repositories: testRepositories}, quietLogger())
	rig.api = api.NewServer("127.0.0.1", 0, api.Options{
		Supervisor: sup, Project: podUpdateProject, DispatchProject: "LEGION", PluginRelaunchable: true,
		Pool: pool, Handlers: []intake.Handler{engine}, Record: records, Grants: rig.grants, Log: quietLogger(),
	}).Handler
	return rig
}

// drifts scripts the runtime to answer reasons for every claim of issue, which is what one
// template change leaves on every pod built before it.
func (r *fleetRig) drifts(issue string, reasons ...string) {
	r.t.Helper()
	for key, token := range r.tokens {
		if key.issue == issue {
			r.rt.ScriptPodDrift(token, reasons...)
		}
	}
}

// tick is one orphan sweep over every claim the store holds, which is what the daemon's own tick
// puts to the round.
func (r *fleetRig) tick() {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims for the sweep: %v", err)
	}
	r.s.reconcilePodUpdates(r.ctx, claims)
}

func (r *fleetRig) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, ok := r.tokens[fleetClaim{issue: issue, role: role}]
	if !ok {
		r.t.Fatalf("%s has no %s claim in this rig", issue, role)
	}
	return token
}

// claimOf is the claim as its machine holds it, which is what the next tick reads from the store.
func (r *fleetRig) claimOf(issue string, role claim.Role) supervise.Claim {
	r.t.Helper()
	m, ok := r.sup.Machine(r.token(issue, role))
	if !ok {
		r.t.Fatalf("%s's %s is not supervised", issue, role)
	}
	return m.Claim()
}

// round is the pod round the sweep opens over issue.
func (r *fleetRig) round(issue string) string {
	r.t.Helper()
	return supervise.PodRound(r.pods[issue])
}

// awaitRound waits for the round of issue to be recorded on role's claim with the task it sends,
// which the sweep's posts reach through each machine's own queue.
func (r *fleetRig) awaitRound(issue string, role claim.Role) supervise.Claim {
	r.t.Helper()
	want := r.round(issue)
	testwait.Eventually(r.t, fmt.Sprintf("%s's %s to hold the update round %s", issue, role, want), func() bool {
		c := r.claimOf(issue, role)
		return c.UpdateRound == want && c.UpdateTaskedAt != nil
	})
	return r.claimOf(issue, role)
}

// signal is the worker answering ready_for_update, as api's claims/ready-for-update posts it.
func (r *fleetRig) signal(issue string, role claim.Role, head string) {
	r.t.Helper()
	c := r.claimOf(issue, role)
	m, _ := r.sup.Machine(c.Token)
	if err := m.Handle(r.ctx, supervise.PodUpdateSignalled{
		Claim: c.Token, Generation: c.Generation, Session: c.Session, Round: c.UpdateRound, Head: head,
	}); err != nil {
		r.t.Fatalf("%s's %s answered the update round: %v", issue, role, err)
	}
}

// complete is one worker's phase completion through the daemon's own route, with a grant minted
// for its claim as the grants route mints one. It returns the status and the body.
func (r *fleetRig) complete(issue string, role claim.Role, summary, commit string) (int, string) {
	r.t.Helper()
	grant, err := r.grants.Mint(r.claimOf(issue, role))
	if err != nil {
		r.t.Fatalf("mint a grant for %s's %s: %v", issue, role, err)
	}
	body, err := json.Marshal(api.HandoffCompleteRequest{GrantID: grant.ID, Summary: summary, Commit: commit})
	if err != nil {
		r.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/legion/v1/handoff/complete", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	r.api.ServeHTTP(recorder, request)
	return recorder.Code, strings.TrimSpace(recorder.Body.String())
}

// replacementsOf is every ReplacePod the sweep made over pod, in order.
func (r *fleetRig) replacementsOf(pod string) []fake.Call {
	r.t.Helper()
	var of []fake.Call
	for _, call := range r.rt.CallsOf("ReplacePod") {
		if call.Pod.Locator == nil || call.Pod.Locator.Sandbox == nil {
			r.t.Fatalf("ReplacePod was called with %+v, want a process of a pod", call.Pod)
		}
		if call.Pod.Locator.Sandbox.PodUID == pod {
			of = append(of, call)
		}
	}
	return of
}

// replacements is every ReplacePod the sweep made, over any pod.
func (r *fleetRig) replacements() []fake.Call { return r.rt.CallsOf("ReplacePod") }

// controllerNotices is every controller notice the sweep and the workflow wrote, oldest first.
func (r *fleetRig) controllerNotices() []record.ControllerNotice {
	r.t.Helper()
	var notices []record.ControllerNotice
	for _, row := range r.rows(record.OutboxKindControllerNotice) {
		var notice record.ControllerNotice
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			r.t.Fatalf("decode controller notice %d: %v", row.ID, err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// statusWrites is every Dispatch status the workflow queued, oldest first, with the issue each is
// of: a park is one of them, the backlog a leaving issue is written to.
func (r *fleetRig) statusWrites() map[string]string {
	r.t.Helper()
	writes := map[string]string{}
	for _, row := range r.rows(record.OutboxKindDispatchStatus) {
		var write record.StatusWrite
		if err := json.Unmarshal(row.Payload, &write); err != nil {
			r.t.Fatalf("decode status write %d: %v", row.ID, err)
		}
		writes[row.Issue] = write.Status
	}
	return writes
}

// rows is every outbox row of kind, oldest first, whatever issue it is of.
func (r *fleetRig) rows(kind record.OutboxKind) []record.OutboxRow {
	r.t.Helper()
	query, err := r.pool.Query(r.ctx, "select id, kind, issue, payload from outbox where kind = $1 order by id", string(kind))
	if err != nil {
		r.t.Fatalf("read the %s rows: %v", kind, err)
	}
	defer query.Close()
	var rows []record.OutboxRow
	for query.Next() {
		var row record.OutboxRow
		if err := query.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("read a %s row: %v", kind, err)
		}
		rows = append(rows, row)
	}
	if err := query.Err(); err != nil {
		r.t.Fatalf("read the %s rows: %v", kind, err)
	}
	return rows
}

// assertNoRootLeftTheWorkflow is the fleet round's second promise: the round moves no root out of
// the workflow. No root waits on a person, none is parked — its phase done at backlog, or a
// Dispatch status write that would park it — and no architect of either issue is suspended or
// holds a suspension for its turn. The controller is told of no root waiting on a person either.
func (r *fleetRig) assertNoRootLeftTheWorkflow(issues ...string) {
	r.t.Helper()
	writes := r.statusWrites()
	for _, issue := range issues {
		got := readIssue(r.t, r.pool, issue)
		if got == nil {
			r.t.Fatalf("%s is not recorded", issue)
		}
		if got.Phase == phase.AwaitingPerson || got.Awaiting != nil {
			r.t.Errorf("%s is at phase %s waiting on %+v; the round sends no root to a person", issue, got.Phase, got.Awaiting)
		}
		if got.Phase == phase.Done || got.Status == "backlog" {
			r.t.Errorf("%s is parked: phase %s, status %s", issue, got.Phase, got.Status)
		}
		if status, written := writes[issue]; written && status != "in_progress" {
			r.t.Errorf("the round queued the Dispatch status %q for %s; it parks no issue", status, issue)
		}
		architect := r.claimOf(issue, claim.RoleArchitect)
		if architect.State == supervise.StateSuspended || architect.SuspendedBy != "" || architect.SuspensionHeld {
			r.t.Errorf("%s's architect is %s, suspended by %q (held %t); the round suspends no root",
				issue, architect.State, architect.SuspendedBy, architect.SuspensionHeld)
		}
	}
	for _, notice := range r.controllerNotices() {
		if notice.Kind == record.AwaitingPersonNotice {
			r.t.Errorf("the controller was told %+v; no root of the round waits on a person", notice)
		}
	}
}

// One template change drifts every pod of the fleet at once, and each issue's pod is still its own
// round: the pod of the issue whose workers have answered is replaced, and no other. The claims of
// the issue that has not finished answering keep their round, their pod and their processes, and
// their pod is replaced only once the last of them answers.
//
// The round refuses no completion while it waits, either: a worker tasked to push its work and
// answer completes its phase through the daemon's own route, the issue moves on, and the round it
// is in stays open — the worker pushes, reports and is replaced after, which is what the round is
// for. And nothing of any of it moves a root out of the workflow: no root waits on a person, none
// is parked, and no architect is suspended.
func TestPodUpdateReplacesEachIssuesPodOnlyAfterItsOwnWorkersAnswer(t *testing.T) {
	rig := newFleetRig(t,
		fleetIssue{key: fleetIssueA, phase: phase.Implementing, pod: fleetPodA,
			roles: []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester}},
		fleetIssue{key: fleetIssueB, phase: phase.Planning, pod: fleetPodB,
			roles: []claim.Role{claim.RoleArchitect, claim.RolePlanner}},
	)
	rig.drifts(fleetIssueA, fleetDrift)
	rig.drifts(fleetIssueB, fleetDrift)

	rig.tick()

	for _, c := range []fleetClaim{
		{fleetIssueA, claim.RoleArchitect}, {fleetIssueA, claim.RoleImplementer}, {fleetIssueA, claim.RoleTester},
		{fleetIssueB, claim.RoleArchitect}, {fleetIssueB, claim.RolePlanner},
	} {
		got := rig.awaitRound(c.issue, c.role)
		if got.UpdateReasons != fleetDrift {
			t.Errorf("%s's %s was tasked for %q, want the one template change %q", c.issue, c.role, got.UpdateReasons, fleetDrift)
		}
		if got.Pending == nil || !strings.HasPrefix(got.Pending.ID, "update:"+rig.round(c.issue)+":") {
			t.Errorf("%s's %s holds %+v, want its own round's task", c.issue, c.role, got.Pending)
		}
	}
	if a, b := rig.round(fleetIssueA), rig.round(fleetIssueB); a == b {
		t.Fatalf("both issues are in round %s; each issue's pod is a round of its own", a)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before any worker answered", len(calls))
	}

	// A worker tasked by the round completes its phase through the daemon's own HTTP route, with
	// the workflow engine behind it, while both rounds wait.
	status, body := rig.complete(fleetIssueB, claim.RolePlanner, "planned; pushed before the pod goes", "b1b2c3d")
	if status != http.StatusOK {
		t.Fatalf("the planner's completion in an open round = %d %s, want 200: the round refuses no completion", status, body)
	}
	if moved := readIssue(t, rig.pool, fleetIssueB); moved == nil || moved.Phase != phase.Implementing {
		t.Fatalf("%s after its planner's completion = %+v, want its phase moved on to implementing", fleetIssueB, moved)
	}
	if c := rig.claimOf(fleetIssueB, claim.RolePlanner); c.UpdateRound != rig.round(fleetIssueB) {
		t.Fatalf("the planner's round = %q after its completion, want it still open: the completion ends no round", c.UpdateRound)
	}
	rig.assertNoRootLeftTheWorkflow(fleetIssueA, fleetIssueB)

	// One worker of the second issue answers. Neither pod may go: its own round still waits on its
	// architect, and the first issue's workers have answered nothing.
	rig.signal(fleetIssueB, claim.RolePlanner, "b1b1b1b")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times on one worker of %s answering", len(calls), fleetIssueB)
	}

	// The first issue's workers all answer. Its pod is replaced; the second issue's is not, and its
	// claims keep the round they are in.
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester} {
		rig.signal(fleetIssueA, role, "a1a1a1a")
	}
	rig.tick()

	if calls := rig.replacementsOf(fleetPodA); len(calls) != 1 {
		t.Fatalf("ReplacePod over %s's pod called %d times once its workers answered, want once", fleetIssueA, len(calls))
	}
	if calls := rig.replacementsOf(fleetPodB); len(calls) != 0 {
		t.Fatalf("answering in %s's pod replaced %s's pod %d times", fleetIssueA, fleetIssueB, len(calls))
	}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleImplementer, claim.RoleTester} {
		testwait.Eventually(t, fmt.Sprintf("%s's %s to leave its round with the replacement", fleetIssueA, role), func() bool {
			c := rig.claimOf(fleetIssueA, role)
			return c.UpdateRound == "" && c.UpdateSignalledAt == nil
		})
	}
	if c := rig.claimOf(fleetIssueB, claim.RoleArchitect); c.UpdateRound != rig.round(fleetIssueB) || c.UpdateSignalledAt != nil {
		t.Fatalf("%s's architect = %+v, want it still waiting in its own round", fleetIssueB, c)
	}
	// The replacement carries the configuration a pod made now is built from, so the first issue's
	// new pod has drifted from nothing.
	rig.drifts(fleetIssueA)

	// The second issue's last worker answers: its pod is replaced too, and the first issue's is not
	// replaced a second time.
	rig.signal(fleetIssueB, claim.RoleArchitect, "b2b2b2b")
	rig.tick()

	if calls := rig.replacementsOf(fleetPodB); len(calls) != 1 {
		t.Fatalf("ReplacePod over %s's pod called %d times once its workers answered, want once", fleetIssueB, len(calls))
	}
	if calls := rig.replacementsOf(fleetPodA); len(calls) != 1 {
		t.Fatalf("answering in %s's pod replaced %s's pod %d times, want the one replacement its own round made",
			fleetIssueB, fleetIssueA, len(calls))
	}
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RolePlanner} {
		testwait.Eventually(t, fmt.Sprintf("%s's %s to leave its round with the replacement", fleetIssueB, role), func() bool {
			return rig.claimOf(fleetIssueB, role).UpdateRound == ""
		})
	}
	rig.assertNoRootLeftTheWorkflow(fleetIssueA, fleetIssueB)
}

// The round serves a worker on the plugin contract it registered with, whatever this daemon's own
// is: the whole point of the round is that the process pushes its work before it is replaced, and
// it cannot push without the daemon it already speaks to (api's staleClaim). With no round open
// the same claim is refused the stale-caller 409 — the negative control — and the round is what
// turns that refusal into the 200 a completion needs.
func TestPodUpdateRoundServesAWorkerOnItsPreviousContract(t *testing.T) {
	rig := newFleetRig(t,
		fleetIssue{key: fleetIssueB, phase: phase.Planning, pod: fleetPodB,
			roles: []claim.Role{claim.RoleArchitect, claim.RolePlanner}},
	)
	before := rig.claimOf(fleetIssueB, claim.RolePlanner)
	m, ok := rig.sup.Machine(before.Token)
	if !ok {
		t.Fatalf("%s's planner is not supervised", fleetIssueB)
	}
	// The planner's process registered on the contract before this daemon's, which is what a
	// deployment's own release leaves running until each pod is replaced.
	if err := m.Handle(rig.ctx, supervise.RequestRegister{
		Claim: before.Token, Generation: before.Generation, Session: before.Session, SessionFile: before.SessionFile,
		CapabilityHash: []byte("previous-contract"), PluginContract: api.DaemonAPIVersion - 1,
	}); err != nil {
		t.Fatalf("register the planner on the previous contract: %v", err)
	}

	status, body := rig.complete(fleetIssueB, claim.RolePlanner, "planned", "b1b2c3d")
	if status != http.StatusConflict || !strings.Contains(body, "STALE_PLUGIN_CONTRACT") {
		t.Fatalf("the completion of a claim on the previous contract with no round open = %d %s, want 409 STALE_PLUGIN_CONTRACT", status, body)
	}
	if got := readIssue(t, rig.pool, fleetIssueB); got.Phase != phase.Planning {
		t.Fatalf("%s moved to %s on a refused completion, want nothing recorded by it", fleetIssueB, got.Phase)
	}

	rig.drifts(fleetIssueB, fleetDrift)
	rig.tick()
	rig.awaitRound(fleetIssueB, claim.RoleArchitect)
	testwait.Eventually(t, "the planner to be in the round of "+fleetIssueB, func() bool {
		return rig.claimOf(fleetIssueB, claim.RolePlanner).UpdateRound == rig.round(fleetIssueB)
	})

	status, body = rig.complete(fleetIssueB, claim.RolePlanner, "planned; pushed for the round", "b1b2c3d")
	if status != http.StatusOK {
		t.Fatalf("the same completion while the round is open = %d %s, want 200: the round serves the claim on its previous contract", status, body)
	}
	if got := readIssue(t, rig.pool, fleetIssueB); got.Phase != phase.Implementing {
		t.Fatalf("%s after the completion the round let through = %s, want its phase moved on to implementing", fleetIssueB, got.Phase)
	}
	rig.assertNoRootLeftTheWorkflow(fleetIssueB)
}
