package daemon

// The update-round sweep: a running issue pod whose configuration has drifted is replaced only
// once every worker in it has pushed its work and answered. These tests drive the real supervision
// — real machines over a real Postgres, with the runtime faked — one sweep at a time, as the
// orphan sweep's tick calls it, and read what the store, the runtime and the outbox then hold.
//
// Each rig seeds one issue's claims in Postgres, restores them as a boot does, and ticks by hand:
// the sweep reads the claims the store holds, so a tick sees exactly what the previous tick's
// decisions wrote. The outbox is the daemon's own (workflowRuntime.enqueueOutbox), so a notice or
// a message the sweep writes is a real row of the real table.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	podUpdateProject = "legion"
	podUpdateIssue   = "LEGION-484"
	// podUpdatePod is the pod every role of the issue runs in, as a sandbox locator records it.
	podUpdatePod = "9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22"
	// podUpdateRound is what the sweep names the round of that pod.
	podUpdateRound = "pod:" + podUpdatePod
	// podUpdateRetask and podUpdateNotice are the two bounds under test, as legion.yaml's
	// pod_update_retask_hours and pod_update_notice_hours set them.
	podUpdateRetask = 30 * time.Minute
	podUpdateNotice = 4 * time.Hour
	// podUpdateDrift is the drift the fake runtime reports for the issue's pod.
	podUpdateDrift = "image"
)

// podUpdateRole is one role claim a test seeds: the state the store holds it in, whether an agent
// has run for it (a session), whether it records a process in the issue's pod, and the plugin
// contract its registration declared.
type podUpdateRole struct {
	role     claim.Role
	state    supervise.ClaimState
	session  bool
	inPod    bool
	contract int
}

// readyWorker is the ordinary case: a registered worker of the issue's pod, on this daemon's
// contract.
func readyWorker(role claim.Role) podUpdateRole {
	return podUpdateRole{role: role, state: supervise.StateReady, session: true, inPod: true, contract: api.DaemonAPIVersion}
}

type podUpdateRig struct {
	t       *testing.T
	ctx     context.Context
	s       *supervision
	sup     *supervisor
	rt      *fake.Runtime
	pool    *pgxpool.Pool
	records record.Store
	clock   *heldClock
	conns   *fake.Conns
	workers map[claim.Role]*fake.Conn
	tokens  map[claim.Role]claim.Token
}

func newPodUpdateRig(t *testing.T, roles ...podUpdateRole) *podUpdateRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	dsn := pool.Config().ConnString()
	tokens := map[claim.Role]claim.Token{}
	claims := make([]supervise.Claim, 0, len(roles))
	for _, role := range roles {
		token := mustClaimToken(t, podUpdateIssue, role.role)
		tokens[role.role] = token
		c := supervise.Claim{
			Token: token, Project: podUpdateProject, Tree: podUpdateIssue, Issue: podUpdateIssue, Role: role.role,
			Generation: 1, State: role.state, PluginContract: role.contract,
		}
		if role.session {
			c.Session = "ses_" + string(role.role)
			c.SessionFile = "/sessions/" + string(role.role) + ".jsonl"
		}
		if role.inPod {
			c.Locator = podUpdateLocator(token, podUpdatePod)
		}
		claims = append(claims, c)
	}
	seedWorkflowTree(t, dsn, podUpdateProject, record.Issue{
		Key: podUpdateIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: podUpdateIssue, Title: "The update round", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U",
	}, claims...)

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	clock := &heldClock{at: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
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
	records := record.NewStore()
	rig := &podUpdateRig{t: t, ctx: ctx, sup: sup, rt: rt, pool: pool, records: records, clock: clock, conns: conns,
		workers: map[claim.Role]*fake.Conn{}, tokens: tokens}
	for _, role := range roles {
		if !role.session {
			continue
		}
		conn := fake.NewConn()
		conns.Register(tokens[role.role], conn)
		rig.workers[role.role] = conn
	}
	rig.s = &supervision{
		cfg: config.Config{Project: "LEGION", PodUpdateRetask: podUpdateRetask, PodUpdateNotice: podUpdateNotice},
		log: quietLogger(), plan: plan{clock: clock, project: podUpdateProject, pluginRelaunchable: true, orphanSweep: orphanSweepInterval},
		runtime: rt, supervisor: sup,
		enqueue: (&workflowRuntime{pool: pool, records: records, log: quietLogger()}).enqueueOutbox,
	}
	return rig
}

// podUpdateLocator is one role process in an issue pod, as the Sandbox runtime records it.
func podUpdateLocator(token claim.Token, pod string) *runtime.Locator {
	return &runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(pod, 1),
		Sandbox: &runtime.SandboxLocator{
			Namespace: "legion", Name: "legion-" + strings.ToLower(podUpdateIssue), PodUID: pod,
			Container: runtime.IssuePodContainer, Generation: 1,
		},
	}
}

// drifts scripts the runtime to report reasons for every claim of the issue that records a
// process, which is what a pod built from a configuration the runtime has moved past answers.
func (r *podUpdateRig) drifts(reasons ...string) {
	r.t.Helper()
	for _, token := range r.tokens {
		r.rt.ScriptPodDrift(token, reasons...)
	}
}

// tick is one orphan sweep: the claims as the store holds them now, put to the sweep.
func (r *podUpdateRig) tick() {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims for the sweep: %v", err)
	}
	r.s.reconcilePodUpdates(r.ctx, claims)
}

// claimOf is the claim as its machine holds it, which is what the next tick reads from the store.
func (r *podUpdateRig) claimOf(role claim.Role) supervise.Claim {
	r.t.Helper()
	m, ok := r.sup.Machine(r.tokens[role])
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	return m.Claim()
}

// awaitClaim waits for role's claim to satisfy want, which the sweep's posts reach through each
// machine's own queue.
func (r *podUpdateRig) awaitClaim(role claim.Role, what string, want func(supervise.Claim) bool) supervise.Claim {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("%s: %s", role, what), func() bool { return want(r.claimOf(role)) })
	return r.claimOf(role)
}

// awaitRound waits for the round the sweep opens to be recorded on role's claim, with the task it
// sends.
func (r *podUpdateRig) awaitRound(role claim.Role, round string) supervise.Claim {
	r.t.Helper()
	return r.awaitClaim(role, "the update round "+round, func(c supervise.Claim) bool {
		return c.UpdateRound == round && c.UpdateTaskedAt != nil
	})
}

// signal is the worker answering ready_for_update, as api's claims/ready-for-update posts it.
func (r *podUpdateRig) signal(role claim.Role, head string) {
	r.t.Helper()
	c := r.claimOf(role)
	m, _ := r.sup.Machine(r.tokens[role])
	if err := m.Handle(r.ctx, supervise.PodUpdateSignalled{
		Claim: c.Token, Generation: c.Generation, Session: c.Session, Head: head,
	}); err != nil {
		r.t.Fatalf("%s answered the update round: %v", role, err)
	}
}

// handle drives one event into a claim's machine and waits for it, as the daemon's own sources do.
func (r *podUpdateRig) handle(role claim.Role, ev supervise.Event) {
	r.t.Helper()
	m, ok := r.sup.Machine(r.tokens[role])
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	if err := m.Handle(r.ctx, ev); err != nil {
		r.t.Fatalf("%s handled %T: %v", role, ev, err)
	}
}

// rows is every outbox row of kind the sweep wrote, oldest first.
func (r *podUpdateRig) rows(kind record.OutboxKind) []record.OutboxRow {
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

// notices is every update-stalled controller notice the sweep wrote, oldest first.
func (r *podUpdateRig) notices() []record.ControllerNotice {
	r.t.Helper()
	var notices []record.ControllerNotice
	for _, row := range r.rows(record.OutboxKindControllerNotice) {
		var notice record.ControllerNotice
		if err := json.Unmarshal(row.Payload, &notice); err != nil {
			r.t.Fatalf("decode controller notice %d: %v", row.ID, err)
		}
		if row.Issue != podUpdateIssue {
			r.t.Fatalf("controller notice %d is on %s, want %s", row.ID, row.Issue, podUpdateIssue)
		}
		notices = append(notices, notice)
	}
	return notices
}

// messages is every message the sweep wrote on the issue, oldest first.
func (r *podUpdateRig) messages() []string {
	r.t.Helper()
	var bodies []string
	for _, row := range r.rows(record.OutboxKindDispatchMessage) {
		var post record.MessagePost
		if err := json.Unmarshal(row.Payload, &post); err != nil {
			r.t.Fatalf("decode message %d: %v", row.ID, err)
		}
		bodies = append(bodies, post.Body)
	}
	return bodies
}

func (r *podUpdateRig) replacements() []fake.Call {
	return r.rt.CallsOf("ReplacePod")
}

// The round's whole promise: the pod is replaced only once every claim that blocks the round has
// answered. A claim with no session — a role nothing has ever launched — blocks nothing.
func TestPodUpdateReplacesThePodOnlyOnceEveryBlockingClaimHasAnswered(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		readyWorker(claim.RoleTester),
		podUpdateRole{role: claim.RolePlanner, state: supervise.StateQueued},
	)
	rig.drifts(podUpdateDrift)

	rig.tick()

	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		c := rig.awaitRound(role, podUpdateRound)
		if c.UpdateReasons != podUpdateDrift {
			t.Errorf("%s UpdateReasons = %q, want %q", role, c.UpdateReasons, podUpdateDrift)
		}
		if c.Pending == nil || c.Pending.ID != "update:"+podUpdateRound {
			t.Errorf("%s pending = %+v, want the round's task", role, c.Pending)
		}
	}
	if c := rig.claimOf(claim.RolePlanner); c.UpdateRound != "" {
		t.Errorf("the planner, which never had a worker, is in round %q; want none", c.UpdateRound)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before any worker answered", len(calls))
	}

	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times while the tester had not answered", len(calls))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateHead != "a1b2c3d" {
		t.Errorf("the implementer's reported head = %q, want a1b2c3d", c.UpdateHead)
	}

	rig.signal(claim.RoleTester, "")
	rig.tick()

	calls := rig.replacements()
	if len(calls) != 1 {
		t.Fatalf("ReplacePod called %d times once every worker answered, want once", len(calls))
	}
	if calls[0].Pod.Locator == nil || calls[0].Pod.Locator.Sandbox.PodUID != podUpdatePod {
		t.Fatalf("ReplacePod called with %+v, want a process of pod %s", calls[0].Pod, podUpdatePod)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.awaitClaim(role, "its round to end with the replacement", func(c supervise.Claim) bool {
			return c.UpdateRound == "" && c.UpdateSignalledAt == nil
		})
	}
}

// A worker answers from inside a turn, and that turn ends a moment later. The replacement waits
// for it: no role process is ended mid-turn.
func TestPodUpdateHoldsTheReplacementWhileAClaimIsWorking(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateWorking, session: true, inPod: true, contract: api.DaemonAPIVersion},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times while the implementer ran a turn", len(calls))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the implementer's round = %q, want it still open while it works", c.UpdateRound)
	}

	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: rig.tokens[claim.RoleImplementer]})
	rig.tick()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("ReplacePod called %d times once the turn ended, want once", len(calls))
	}
}

// A failed claim that has had a worker pins the round: nobody can say whether its workspace holds
// unpushed work, so the pod is not replaced, and the controller is told once pod_update_notice_hours
// after the round opened, which is what brings the operator.
func TestPodUpdateTellsTheControllerOnceAfterTheNoticeBound(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateFailed, session: true, contract: api.DaemonAPIVersion},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift, "disk")

	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times before the notice bound", len(notices))
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times with a failed claim pinning the round", len(calls))
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times at the notice bound, want once", len(notices))
	}
	notice := notices[0]
	if notice.Kind != record.UpdateStalledNotice || notice.Update == nil {
		t.Fatalf("the notice is %+v, want an update-stalled notice carrying its round", notice)
	}
	if notice.Update.Round != podUpdateRound || notice.Update.Pod != podUpdatePod {
		t.Errorf("the notice names round %q of pod %q, want %q of %q", notice.Update.Round, notice.Update.Pod, podUpdateRound, podUpdatePod)
	}
	if want := []string{podUpdateDrift, "disk"}; !slices.Equal(notice.Update.Reasons, want) {
		t.Errorf("the notice names reasons %q, want %q", notice.Update.Reasons, want)
	}
	if len(notice.Update.Waiting) != 1 || notice.Update.Waiting[0].Role != claim.RoleImplementer ||
		notice.Update.Waiting[0].State != string(supervise.StateFailed) {
		t.Fatalf("the notice waits on %+v, want the failed implementer alone", notice.Update.Waiting)
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	if notices := rig.notices(); len(notices) != 1 {
		t.Fatalf("the controller was told %d times over two bounds, want once per round", len(notices))
	}
}

// A worker that does not answer is asked again, and only once pod_update_retask_hours has passed
// since the last ask: the round's own clock never postpones the notice, and the worker is not
// asked at every tick.
func TestPodUpdateAsksAgainAfterTheRetaskBound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	first := rig.awaitRound(claim.RoleImplementer, podUpdateRound).UpdateTaskedAt

	rig.clock.advance(podUpdateRetask - time.Minute)
	rig.tick()
	rig.awaitClaim(claim.RoleImplementer, "the round to stay open", func(c supervise.Claim) bool { return c.UpdateRound == podUpdateRound })

	if asked := rig.claimOf(claim.RoleImplementer).UpdateTaskedAt; !asked.Equal(*first) {
		t.Fatalf("the worker was asked again at %s, before the re-task bound from %s", asked, first)
	}

	rig.clock.advance(2 * time.Minute)
	rig.tick()

	rig.awaitClaim(claim.RoleImplementer, "the worker to be asked again", func(c supervise.Claim) bool {
		return c.UpdateTaskedAt != nil && c.UpdateTaskedAt.After(*first)
	})
	if opened := rig.claimOf(claim.RoleImplementer).UpdateOpenedAt; !opened.Equal(*first) {
		t.Errorf("the round's own clock moved to %s with the re-task; want it still at %s", opened, first)
	}
}

// A worker whose plugin predates the contract that introduced ready_for_update can never answer.
// The round stops there: nothing is replaced, the controller is told at once rather than at the
// notice bound, and the issue carries the same words, so the operator drains the pod by hand.
func TestPodUpdateStopsForAClaimBelowTheUpdateContract(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true, contract: supervise.PodUpdateContract - 1},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times about a worker that cannot answer, want once", len(notices))
	}
	if notices[0].Update == nil || len(notices[0].Update.Waiting) != 1 || !notices[0].Update.Waiting[0].CannotAnswer {
		t.Fatalf("the notice waits on %+v, want the implementer marked as unable to answer", notices[0].Update)
	}
	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written on %d times, want once", len(messages))
	}
	if !strings.Contains(messages[0], string(claim.RoleImplementer)) {
		t.Errorf("the message does not name the implementer: %s", messages[0])
	}
	if c := rig.claimOf(claim.RoleImplementer); c.Pending != nil {
		t.Errorf("the implementer holds %+v; a claim that cannot answer is never asked", c.Pending)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times with a worker that cannot answer", len(calls))
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	if notices := rig.notices(); len(notices) != 1 {
		t.Fatalf("the controller was told %d times, want once per round", len(notices))
	}
	if messages := rig.messages(); len(messages) != 1 {
		t.Fatalf("the issue was written on %d times, want once per round", len(messages))
	}
}

// Drift that clears — the operator put the configuration back — ends the round where it stands,
// and the task the round was holding back goes to the worker that had answered.
func TestPodUpdateEndsTheRoundWhenTheDriftClears(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	// The worker's turn — the one it answered from — ends, which retires the round's own task.
	// The next task then arrives while the round holds it back.
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: "update:" + podUpdateRound})
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	rig.awaitClaim(claim.RoleImplementer, "the round's own task to retire with the turn", func(c supervise.Claim) bool {
		return c.Pending == nil
	})
	rig.handle(claim.RoleImplementer, supervise.RequestDeliver{Claim: token, ID: "after-the-round", Task: "Carry on implementing."})
	conn := rig.workers[claim.RoleImplementer]
	if prompts := promptsOf(conn, "after-the-round"); len(prompts) != 0 {
		t.Fatalf("the task was sent while the round held it: %+v", prompts)
	}

	rig.drifts()
	rig.tick()

	rig.awaitClaim(claim.RoleImplementer, "its round to end with the drift", func(c supervise.Claim) bool {
		return c.UpdateRound == ""
	})
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times for a pod that no longer drifts", len(calls))
	}
	testwait.Eventually(t, "the task the round held to be sent", func() bool {
		return len(promptsOf(conn, "after-the-round")) == 1
	})
}

// A pod can still be replaced where it stands for a failure of its own — a terminal pod, a failed
// init container, a superseded disk — and that deletes the disk with everything unpushed on it.
// The issue is told so, naming the roles that had not answered, and the round ends.
func TestPodUpdateWritesOnTheIssueWhenThePodWentWithoutItsRound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")

	// The pod is gone: neither claim records a process in it any more, which is what the next
	// sweep reads once the displaced path has released them.
	gone, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for i := range gone {
		gone[i].Locator = nil
	}
	rig.s.reconcilePodUpdates(rig.ctx, gone)

	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written on %d times, want once for the pod that went", len(messages))
	}
	if !strings.Contains(messages[0], podUpdateRound) {
		t.Errorf("the message does not name the round %s: %s", podUpdateRound, messages[0])
	}
	if !strings.Contains(messages[0], string(claim.RoleImplementer)) {
		t.Errorf("the message does not name the implementer, which had not answered: %s", messages[0])
	}
	if strings.Contains(messages[0], string(claim.RoleTester)) {
		t.Errorf("the message names the tester, which had answered and lost nothing: %s", messages[0])
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.awaitClaim(role, "its round to end with the pod", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times for a pod that is already gone", len(calls))
	}
}

// Everything a round decides is in the durable claim columns, so a daemon that restarts in the
// middle of one resumes it from the store alone: the round, its clock, the task already sent and
// the answer already given are all read back, and the next sweep replaces the pod on the answer
// the previous daemon received.
func TestPodUpdateResumesAnOpenRoundAfterARestart(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")

	next := rig.restart()

	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		c := next.claimOf(role)
		if c.UpdateRound != podUpdateRound || c.UpdateOpenedAt == nil || c.UpdateTaskedAt == nil {
			t.Fatalf("%s after the restart = %+v, want its open round read back from the store", role, c)
		}
	}
	if c := next.claimOf(claim.RoleImplementer); c.UpdateSignalledAt == nil || c.UpdateHead != "a1b2c3d" {
		t.Fatalf("the implementer's answer did not survive the restart: %+v", c)
	}

	next.drifts(podUpdateDrift)
	next.tick()
	if calls := next.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times while the tester had not answered", len(calls))
	}
	next.signal(claim.RoleTester, "a1b2c3d")
	next.tick()

	if calls := next.replacements(); len(calls) != 1 {
		t.Fatalf("ReplacePod called %d times by the daemon that resumed the round, want once", len(calls))
	}
}

// restart is a second daemon over the same Postgres: new machines, a new runtime, and nothing of
// the first daemon's memory.
func (r *podUpdateRig) restart() *podUpdateRig {
	r.t.Helper()
	r.sup.stop()
	st, err := store.Open(r.ctx, r.pool.Config().ConnString())
	if err != nil {
		r.t.Fatalf("open the store again: %v", err)
	}
	r.t.Cleanup(st.Close)
	rt := fake.NewRuntime()
	rt.Now = r.clock.Now
	conns := fake.NewConns()
	sup := newSupervisor(r.ctx, st, podUpdateProject, r.t.TempDir(), quietLogger())
	sup.deps = r.sup.deps
	sup.deps.Runtime, sup.deps.Conns, sup.deps.Store = rt, conns, st
	r.t.Cleanup(sup.stop)
	stored, err := sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	if _, err := sup.restore(r.ctx, stored); err != nil {
		r.t.Fatalf("restore the stored claims: %v", err)
	}
	workers := map[claim.Role]*fake.Conn{}
	for role, token := range r.tokens {
		if _, had := r.workers[role]; !had {
			continue
		}
		conn := fake.NewConn()
		conns.Register(token, conn)
		workers[role] = conn
	}
	next := &podUpdateRig{t: r.t, ctx: r.ctx, sup: sup, rt: rt, pool: r.pool, records: r.records, clock: r.clock,
		conns: conns, workers: workers, tokens: r.tokens}
	next.s = &supervision{
		cfg: r.s.cfg, log: quietLogger(), plan: r.s.plan, runtime: rt, supervisor: sup, enqueue: r.s.enqueue,
	}
	return next
}

// promptsOf is every prompt of one delivery the connection received.
func promptsOf(conn *fake.Conn, deliveryID string) []fake.Prompt {
	var of []fake.Prompt
	for _, prompt := range conn.Prompts() {
		if prompt.DeliveryID == deliveryID {
			of = append(of, prompt)
		}
	}
	return of
}
