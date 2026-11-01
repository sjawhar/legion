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
	"errors"
	"fmt"
	"strconv"
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
	// podUpdateKeptDrift is a drift of the kind a launch keeps the pod for and starts its role
	// degraded in: the operator's volume holding that role's overlay moved, so a pod built before
	// the move cannot carry the file the role's Oh My Pi would be told to read. The launch starts
	// the role as the pod was built instead (the Sandbox runtime's relaunch), so the role runs
	// without that overlay until this round replaces its pod — and can still push and answer.
	podUpdateKeptDrift = "role_config_files.architect /etc/legion-operator-extra/fallback.yml (unset), now volume operator-roles from ConfigMap legion-operator-roles items []"
	// podUpdateReplacementPod is the pod the issue's roles come back in once something replaced
	// the pod a round was open over: a different uid, which is the one evidence that the pod went.
	podUpdateReplacementPod = "4c7e1b60-8d92-4f03-a1c5-73b0e2d9f481"
	// podUpdatePreviousPod is a pod of this issue that is already gone: the one a claim retired
	// before the live ones still records a round over.
	podUpdatePreviousPod = "f1a9c3d7-5e84-4b20-9c61-0d8a7f26b4e3"
	// podUpdatePluginReasons is what a plugin round records, which carries ", " inside it.
	podUpdatePluginReasons = "the process speaks plugin contract 42, and this daemon speaks 43"
)

// podUpdateStart is where the rig's clock stands when a test begins, and what a round seeded on a
// claim records as its own clock.
var podUpdateStart = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

// podUpdateRole is one role claim a test seeds: the state the store holds it in, whether an agent
// has run for it (a session), whether it records a process in the issue's pod, and the plugin
// contract its registration declared. round, reasons and signalled seed an update round the store
// already holds on the claim, as an earlier tick or an earlier release left it; pod names a pod of
// its own, for a claim that records one the rest of the issue has left behind; suspendedBy is who
// suspended a claim seeded suspended.
type podUpdateRole struct {
	role        claim.Role
	state       supervise.ClaimState
	session     bool
	inPod       bool
	contract    int
	round       string
	reasons     string
	signalled   bool
	pod         string
	suspendedBy supervise.SuspendSource
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
			Generation: 1, State: role.state, PluginContract: role.contract, SuspendedBy: role.suspendedBy,
		}
		if role.session {
			c.Session = "ses_" + string(role.role)
			c.SessionFile = "/sessions/" + string(role.role) + ".jsonl"
		}
		if pod := role.pod; pod != "" || role.inPod {
			if pod == "" {
				pod = podUpdatePod
			}
			c.Locator = podUpdateLocator(token, podUpdateIssue, pod)
		}
		if role.round != "" {
			opened := podUpdateStart
			c.UpdateRound, c.UpdateReasons, c.UpdateOpenedAt = role.round, role.reasons, &opened
			c.UpdateTaskedAt = &opened
			if role.signalled {
				signalled := podUpdateStart
				c.UpdateSignalledAt = &signalled
			}
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
	clock := &heldClock{at: podUpdateStart}
	rt := fake.NewRuntime()
	rt.Now = clock.Now
	conns := fake.NewConns()
	records := record.NewStore()
	sup := newSupervisor(ctx, st, podUpdateProject, t.TempDir(), quietLogger())
	// The supervision is built before the machines are, because the machines are handed one of
	// its answers: which roles a launch installs no plugin for, which only the daemon's own
	// configuration knows (supervise.Deps.NoPluginInstall). A test that moves a role onto another
	// backend writes rig.s.cfg, and the closure reads it when a registration asks.
	s := &supervision{
		cfg: config.Config{Project: "LEGION", PodUpdateRetask: podUpdateRetask, PodUpdateNotice: podUpdateNotice},
		log: quietLogger(), plan: plan{clock: clock, project: podUpdateProject, pluginRelaunchable: true, orphanSweep: orphanSweepInterval},
		runtime: rt, supervisor: sup,
		enqueue: (&workflowRuntime{pool: pool, records: records, log: quietLogger()}).enqueueOutbox,
	}
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: conns, Store: st, Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
		Limits:         supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:       supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract: api.DaemonAPIVersion,
		// This daemon can relaunch a stale process onto a plugin, as the supervision's own plan
		// says it can: a registration's relaunch and its refusal are decided by this field alone
		// (staleRegistration), and a rig that left it false would accept every stale registration
		// whatever the rest of the rule says.
		PluginRelaunchable: true,
		NoPluginInstall:    func(c supervise.Claim) bool { return noPluginInstall(s.cfg, c) },
		PodUpdateRetask:    podUpdateRetask,
	}
	t.Cleanup(sup.stop)
	stored, err := sup.Claims(ctx)
	if err != nil {
		t.Fatalf("read the seeded claims: %v", err)
	}
	if _, err := sup.restore(ctx, stored); err != nil {
		t.Fatalf("restore the seeded claims: %v", err)
	}
	rig := &podUpdateRig{t: t, ctx: ctx, s: s, sup: sup, rt: rt, pool: pool, records: records, clock: clock, conns: conns,
		workers: map[claim.Role]*fake.Conn{}, tokens: tokens}
	for _, role := range roles {
		if !role.session {
			continue
		}
		conn := fake.NewConn()
		conns.Register(tokens[role.role], conn)
		rig.workers[role.role] = conn
	}
	return rig
}

// podUpdateLocator is one role process in an issue pod, as the Sandbox runtime records it. The
// issue names the pod's Sandbox, which every role claim of that issue shares.
func podUpdateLocator(token claim.Token, issue, pod string) *runtime.Locator {
	return &runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(pod, 1),
		Sandbox: &runtime.SandboxLocator{
			Namespace: "legion", Name: "legion-" + strings.ToLower(issue), PodUID: pod,
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
		Claim: c.Token, Generation: c.Generation, Session: c.Session, Round: c.UpdateRound, Head: head,
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

// startInPod is the daemon starting a role into the issue's pod while a round holds that pod: the
// launch keeps the drifted pod rather than replacing it where it stands, so the process it returns
// is in that same pod, and the role registers and readies from it as any other worker does. The
// role runs degraded there — the pod carries neither the overlay nor the enrollment a pod created
// now is built with — and is a worker of the round all the same.
func (r *podUpdateRig) startInPod(role claim.Role) {
	r.t.Helper()
	token := r.tokens[role]
	m, ok := r.sup.Machine(token)
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	r.workers[role] = conn
	// A claim the boot restored with no process of its own is launch_uncertain until its release
	// settles it, which is what the daemon's own unfinished-launch path does before it launches
	// (launchUnfinishedClaim).
	if r.claimOf(role).State == supervise.StateLaunchUncertain {
		if _, err := m.ReleaseUncertainLaunch(r.ctx); err != nil {
			r.t.Fatalf("release the %s's unfinished launch: %v", role, err)
		}
	}
	r.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdatePod)})
	r.handle(role, supervise.RequestSpawn{Claim: token})
	generation := r.claimOf(role).Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses_" + string(role),
			SessionFile: "/sessions/" + string(role) + ".jsonl", CapabilityHash: []byte(role), PluginContract: api.DaemonAPIVersion},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses_" + string(role)},
	} {
		r.handle(role, ev)
	}
	if c := r.claimOf(role); c.State != supervise.StateReady || podOf(c.Locator) != podUpdatePod {
		r.t.Fatalf("the %s started into the kept pod is %s at %+v, want it ready in pod %s", role, c.State, c.Locator, podUpdatePod)
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
		if c.Pending == nil || !strings.HasPrefix(c.Pending.ID, "update:"+podUpdateRound+":") {
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

// A drift the kept-pod rule leaves a role degraded for — here the operator's volume holding that
// role's overlay, moved — is still a drift the round replaces the pod for, and the role the daemon
// starts into that kept pod is still a worker of the round. It registers from the pod it was
// started in, is tasked with the round like every other worker, holds the replacement back until
// it answers, and ends the round when it does. A role that never registered would leave the round
// waiting on the very pod it was opened to replace.
func TestPodUpdateEndsTheRoundOnceTheRoleStartedIntoTheKeptPodAnswers(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		podUpdateRole{role: claim.RoleArchitect, state: supervise.StateQueued},
	)
	rig.drifts(podUpdateKeptDrift)

	rig.tick()

	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	if c := rig.claimOf(claim.RoleArchitect); c.UpdateRound != "" {
		t.Fatalf("the architect, which no launch has started, is in round %q; want none", c.UpdateRound)
	}

	// The launch keeps the drifted pod and starts the architect in it, degraded: the pod carries
	// neither the overlay the role's configuration now names nor what a pod created now is built
	// with, and the role runs there until this round replaces the pod.
	rig.startInPod(claim.RoleArchitect)
	rig.tick()

	degraded := rig.awaitRound(claim.RoleArchitect, podUpdateRound)
	if degraded.Pending == nil || !strings.HasPrefix(degraded.Pending.ID, "update:"+podUpdateRound+":") {
		t.Fatalf("the degraded architect holds %+v, want the round's own task", degraded.Pending)
	}
	if degraded.UpdateReasons != podUpdateKeptDrift {
		t.Errorf("the degraded architect was tasked for %q, want the drift its pod was kept for %q", degraded.UpdateReasons, podUpdateKeptDrift)
	}

	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times while the degraded architect had not answered", len(calls))
	}

	rig.signal(claim.RoleArchitect, "e4f5a6b")
	rig.tick()

	calls := rig.replacements()
	if len(calls) != 1 {
		t.Fatalf("ReplacePod called %d times once the degraded architect answered, want once", len(calls))
	}
	if calls[0].Pod.Locator == nil || calls[0].Pod.Locator.Sandbox.PodUID != podUpdatePod {
		t.Fatalf("ReplacePod called with %+v, want a process of the kept pod %s", calls[0].Pod, podUpdatePod)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleArchitect} {
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
	if want := podUpdateDrift + ", disk"; notice.Update.Reasons != want {
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

// A worker that read the round's ask and ended its turn without answering is asked again, and
// only once pod_update_retask_hours has passed since the last ask: the round's own clock never
// postpones the notice, and the worker is not asked at every tick. Each ask is a delivery of its
// own, so the second one reaches a worker the shim would answer for under the first ask's id.
func TestPodUpdateAsksAgainAfterTheRetaskBound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	asked := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	first := asked.UpdateTaskedAt
	// The ask's own turn starts and ends with no answer, which is what leaves the claim holding
	// no task for the next ask to be.
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: asked.Pending.ID})
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	rig.awaitClaim(claim.RoleImplementer, "the unanswered ask to end with its turn", func(c supervise.Claim) bool {
		return c.Pending == nil
	})

	rig.clock.advance(podUpdateRetask - time.Minute)
	rig.tick()
	rig.awaitClaim(claim.RoleImplementer, "the round to stay open", func(c supervise.Claim) bool { return c.UpdateRound == podUpdateRound })

	if at := rig.claimOf(claim.RoleImplementer).UpdateTaskedAt; !at.Equal(*first) {
		t.Fatalf("the worker was asked again at %s, before the re-task bound from %s", at, first)
	}

	rig.clock.advance(2 * time.Minute)
	rig.tick()

	again := rig.awaitClaim(claim.RoleImplementer, "the worker to be asked again", func(c supervise.Claim) bool {
		return c.UpdateTaskedAt != nil && c.UpdateTaskedAt.After(*first)
	})
	if again.Pending == nil || again.Pending.ID == asked.Pending.ID {
		t.Errorf("the second ask = %+v, want a delivery of its own: the shim answers the first ask's id itself", again.Pending)
	}
	if opened := rig.claimOf(claim.RoleImplementer).UpdateOpenedAt; !opened.Equal(*first) {
		t.Errorf("the round's own clock moved to %s with the re-task; want it still at %s", opened, first)
	}
}

// A worker whose plugin predates the contract that introduced ready_for_update has no op to
// answer with. It is asked all the same, and the end of the turn its task starts is its answer,
// so the round completes, the pod is replaced and the controller is told nothing: a contract
// change rolls with no drain and no operator.
func TestPodUpdateTakesATurnsEndAsTheAnswerOfAClaimBelowTheUpdateContract(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true, contract: supervise.PodUpdateContract - 1},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	asked := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	if asked.Pending == nil {
		t.Fatal("the worker below the contract holds no task; a claim with no op is asked like any other")
	}
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before the worker below the contract answered", len(calls))
	}

	// It reads the task, pushes, and ends its turn. That end is the answer.
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: asked.Pending.ID})
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})

	answered := rig.claimOf(claim.RoleImplementer)
	if answered.UpdateSignalledAt == nil {
		t.Fatal("the end of the update task's turn recorded no answer for a worker that has no op to answer with")
	}
	if answered.UpdateHead != "" {
		t.Errorf("the answer records head %q; a turn's end reports none", answered.UpdateHead)
	}

	rig.tick()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("the pod was replaced %d times once every worker answered, want once", len(calls))
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v; a round that finishes by itself needs no one", notices)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q; no drain is asked of anybody", messages)
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
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: rig.claimOf(claim.RoleImplementer).Pending.ID})
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
//
// What says the pod went is the pod the claims record now: another one, which the displaced path
// brought each role back into. A claim that records no pod at all says nothing of the kind.
func TestPodUpdateWritesOnTheIssueWhenThePodWentWithoutItsRound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")

	// The pod the round was opened over is gone, and each role is back in the pod that replaced
	// it, which is what the next sweep reads once the displaced path has relaunched them.
	rig.tickBackIn(podUpdateReplacementPod)

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

// tickBackIn is one sweep over claims that record pod rather than the one their round was opened
// over: the pod went, and the displaced path brought each role back into the pod that replaced
// it. The claims the store holds still record the old pod, so what the sweep reads is the whole
// of the difference.
func (r *podUpdateRig) tickBackIn(pod string) {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims for the sweep: %v", err)
	}
	for i := range claims {
		if claims[i].Locator != nil {
			claims[i].Locator = podUpdateLocator(claims[i].Token, podUpdateIssue, pod)
		}
	}
	r.s.reconcilePodUpdates(r.ctx, claims)
}

// settle waits for every send role's machine started to have had its outcome handled, so a test
// that reads what the worker was sent reads all of it.
func (r *podUpdateRig) settle(role claim.Role) {
	r.t.Helper()
	m, ok := r.sup.Machine(r.tokens[role])
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	m.Wait()
}

// A round can end in four ways, and only one of them leaves its worker needing a word from the
// daemon. The worker here has answered and its turn is over, so it holds nothing: the pod round's
// own ask retired with that turn, and the round held every task since. Where the pod the round
// was opened over is gone — the replacement this round made, a pod that went without its round,
// or a pod that went for a failure of its own and left the round's last tick reading no drift
// over the pod that replaced it — the worker comes back on a new session and a new disk and its
// launch is what starts it, so the sweep's end says the pod went and the machine tells it
// nothing. Only the round whose own pod is still running ends with that worker holding nothing
// and no launch coming, and only there is it told to carry on.
func TestPodUpdateTellsTheWorkerToCarryOnOnlyWhereItsPodStayed(t *testing.T) {
	for name, tc := range map[string]struct {
		roles        []podUpdateRole
		end          func(*podUpdateRig)
		replacements int
		want         int
	}{
		"the round replaced the pod": {
			roles:        []podUpdateRole{readyWorker(claim.RoleImplementer)},
			end:          func(r *podUpdateRig) { r.tick() },
			replacements: 1,
		},
		"the pod went without its round": {
			roles: []podUpdateRole{readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester)},
			end:   func(r *podUpdateRig) { r.tickBackIn(podUpdateReplacementPod) },
		},
		"the drift cleared over the pod that replaced the round's own": {
			roles: []podUpdateRole{readyWorker(claim.RoleImplementer)},
			end:   func(r *podUpdateRig) { r.drifts(); r.tickBackIn(podUpdateReplacementPod) },
		},
		"the drift cleared over the round's own pod": {
			roles: []podUpdateRole{readyWorker(claim.RoleImplementer)},
			end:   func(r *podUpdateRig) { r.drifts(); r.tick() },
			want:  1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newPodUpdateRig(t, tc.roles...)
			rig.drifts(podUpdateDrift)
			rig.tick()
			rig.awaitRound(claim.RoleImplementer, podUpdateRound)
			rig.signal(claim.RoleImplementer, "a1b2c3d")
			// The turn the implementer answered from ends, which retires the round's own ask:
			// from here it holds nothing at all.
			token := rig.tokens[claim.RoleImplementer]
			rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: rig.claimOf(claim.RoleImplementer).Pending.ID})
			rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
			rig.awaitClaim(claim.RoleImplementer, "the round's own ask to retire with its turn",
				func(c supervise.Claim) bool { return c.Pending == nil })
			conn := rig.workers[claim.RoleImplementer]
			before := len(conn.Prompts())

			tc.end(rig)

			rig.awaitClaim(claim.RoleImplementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
			rig.settle(claim.RoleImplementer)
			if calls := rig.replacements(); len(calls) != tc.replacements {
				t.Fatalf("ReplacePod called %d times, want %d: this is not the end under test", len(calls), tc.replacements)
			}
			sent := conn.Prompts()[before:]
			if len(sent) != tc.want {
				t.Fatalf("the worker was sent %+v after the round ended, want %d tasks", sent, tc.want)
			}
			if tc.want == 0 {
				return
			}
			if strings.HasPrefix(sent[0].DeliveryID, "update:") {
				t.Errorf("the task after the round = %+v, want the daemon's own word rather than another ask of a round", sent[0])
			}
			if c := rig.claimOf(claim.RoleImplementer); c.Pending == nil || c.Pending.ID != sent[0].DeliveryID {
				t.Errorf("pending after the round = %+v, want the task just sent", c.Pending)
			}
		})
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

// A claim that retired while a round was open — or one an earlier release retired before a
// retirement cleared the round at all — holds a round naming a pod that is long gone. The sweep
// reads no round from it: the live claims' own round is the issue's, it opens, completes and
// replaces the pod, and nothing is ever written on the issue. Read against the pod the live
// claims run in, that leftover round would say "the pod is gone, work is lost" at every tick for
// the rest of the tree's life.
func TestPodUpdateIgnoresARetiredClaimsLeftoverRound(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		podUpdateRole{role: claim.RoleTester, state: supervise.StateRetired, session: true,
			contract: api.DaemonAPIVersion, round: supervise.PodRound(podUpdatePreviousPod), reasons: podUpdateDrift},
	)
	rig.drifts(podUpdateDrift)

	rig.tick()

	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written on %d times for a retired claim's leftover round: %q", len(messages), messages)
	}

	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("ReplacePod called %d times once the live worker answered, want once", len(calls))
	}
	rig.awaitClaim(claim.RoleImplementer, "its round to end with the replacement", func(c supervise.Claim) bool {
		return c.UpdateRound == ""
	})

	// The leftover round is still on the retired claim, and every later tick reads it the same
	// way: not at all.
	for range 3 {
		rig.clock.advance(podUpdateNotice)
		rig.tick()
	}

	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written on %d times over four ticks: %q", len(messages), messages)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times about a retired claim's leftover round", len(notices))
	}
}

// An operator suspends the only worker of an open round. No claim of the issue records a pod then
// — a suspension lets the process go — and that is no evidence that the pod went: the round waits
// where it stands, the issue is told nothing, and the claim an operator holds goes on blocking the
// round until the operator acts. Reading an empty pod as "the pod went" would write a false
// data-loss message on the issue and end a round no worker had answered.
func TestPodUpdateWaitsWhileNoClaimRecordsThePod(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)

	rig.handle(claim.RoleImplementer, supervise.RequestSuspend{
		Claim: rig.tokens[claim.RoleImplementer], By: supervise.SuspendedByOperator,
	})
	rig.awaitClaim(claim.RoleImplementer, "the operator's suspension to let its process go", func(c supervise.Claim) bool {
		return c.State == supervise.StateSuspended && c.Locator == nil
	})

	rig.tick()
	rig.clock.advance(podUpdateNotice)
	rig.tick()

	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written on %d times while no claim recorded a pod: %q", len(messages), messages)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times with no pod recorded", len(calls))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the suspended claim's round = %q, want it still open: only the operator's resume ends this wait", c.UpdateRound)
	}
}

// A plugin round whose worker has answered ends when the sweep reads that answer. Where the
// claim's process is gone by then — an operator suspended it — the round ends and launches
// nothing: starting that process again is the operator's to do, and the claim's next launch
// installs this daemon's plugin anyway.
func TestPodUpdatePluginRoundEndDoesNotRelaunchAnOperatorSuspendedClaim(t *testing.T) {
	round := supervise.PluginRound(api.DaemonAPIVersion)
	rig := newPodUpdateRig(t, podUpdateRole{
		role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
		contract: api.DaemonAPIVersion - 1, round: round, reasons: podUpdatePluginReasons, signalled: true,
	})

	rig.handle(claim.RoleImplementer, supervise.RequestSuspend{
		Claim: rig.tokens[claim.RoleImplementer], By: supervise.SuspendedByOperator,
	})
	rig.awaitClaim(claim.RoleImplementer, "the operator's suspension", func(c supervise.Claim) bool {
		return c.State == supervise.StateSuspended
	})

	rig.tick()

	ended := rig.awaitClaim(claim.RoleImplementer, "its plugin round to end", func(c supervise.Claim) bool {
		return c.UpdateRound == ""
	})
	if ended.State != supervise.StateSuspended || ended.SuspendedBy != supervise.SuspendedByOperator {
		t.Fatalf("the claim after its plugin round ended = %s suspended by %q, want it left as the operator suspended it",
			ended.State, ended.SuspendedBy)
	}
	if resumes, spawns := rig.rt.CallsOf("Resume"), rig.rt.CallsOf("Spawn"); len(resumes)+len(spawns) != 0 {
		t.Fatalf("the round's end started %d processes, want none: the operator's suspension holds this claim",
			len(resumes)+len(spawns))
	}
}

// A claim still launching has no process to judge: the contract it records is the one its last
// process declared, and the one its launch is bringing up is this daemon's. The round waits for
// it rather than telling the operator to drain the pod, which at this release's own roll would
// fire for every issue a boot relaunched a moment before.
func TestPodUpdateDoesNotCallALaunchingClaimUnableToAnswer(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		podUpdateRole{role: claim.RoleTester, state: supervise.StateLaunching, session: true,
			contract: supervise.PodUpdateContract - 1},
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times about a claim that is still launching: %+v", len(notices), notices)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was told to drain the pod for a claim that is still launching: %q", messages)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times while the tester had not answered", len(calls))
	}
}

// A suspended claim is revived for the round's task whatever contract it last recorded: the
// revive launches it onto this daemon's plugin, and the process that comes up answers. A claim
// left suspended for the contract of a process that has ended would block every round this
// deployment ever opens over its pod.
func TestPodUpdateRevivesASuspendedClaimBelowTheUpdateContract(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		podUpdateRole{role: claim.RoleTester, state: supervise.StateSuspended, session: true,
			contract: supervise.PodUpdateContract - 1, suspendedBy: supervise.SuspendedByDaemon},
	)
	rig.drifts(podUpdateDrift)

	rig.tick()

	revived := rig.awaitClaim(claim.RoleTester, "the suspended claim to be revived for the round", func(c supervise.Claim) bool {
		return c.State == supervise.StateLaunching
	})
	if revived.UpdateRound != podUpdateRound || revived.UpdateTaskedAt == nil {
		t.Fatalf("the revived claim = %+v, want it in the round and asked", revived)
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 1 {
		t.Fatalf("the revive resumed %d processes, want the one that installs this daemon's plugin", len(resumes))
	}
}

// The stale-plugin lane and the round divide the claims of a pod between them, and the round
// takes every claim it covers. A claim in a round is left to it whatever contract it records:
// one whose worker can call ready_for_update answers with the op, and one whose plugin has no op
// answers by ending the turn the round's task starts, so neither needs a relaunch and a relaunch
// would cut a turn the round is waiting on.
func TestStalePluginLeavesEveryClaimInARoundToThatRound(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
			contract: supervise.PodUpdateContract - 1, round: podUpdateRound, reasons: podUpdateDrift},
		// A claim whose recorded contract is ahead of this daemon's — its pod runs an image the
		// operator pinned ahead of the daemon — can answer, so its round carries it and the stale
		// lane leaves it alone.
		podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, inPod: true,
			contract: api.DaemonAPIVersion + 1, round: podUpdateRound, reasons: podUpdateDrift},
	)
	claims, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims for the sweep: %v", err)
	}

	rig.s.refreshStalePlugins(claims, "sweep")
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		m, ok := rig.sup.Machine(rig.tokens[role])
		if !ok {
			t.Fatalf("%s is not supervised", role)
		}
		m.Wait()
	}

	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if c := rig.claimOf(role); c.State != supervise.StateReady {
			t.Errorf("%s is %s, want every claim of the round left to it", role, c.State)
		}
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 0 {
		t.Fatalf("the sweep started %d processes for claims a round already carries, want none", len(resumes))
	}
}

// A round every worker answered still replaces nothing while a turn runs. That wait is held to
// the same bound as a worker that never answers: past pod_update_notice_hours the controller is
// told, naming the turns the round waits on. Without it a turn that never ends holds every
// signalled worker's next task for ever, with only a log line to show for it.
func TestPodUpdateTellsTheControllerWhenTheTurnsNeverEnd(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateWorking, session: true, inPod: true,
			contract: api.DaemonAPIVersion},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times before the notice bound", len(notices))
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times about a round whose turn never ends, want once", len(notices))
	}
	if !strings.Contains(notices[0].Reason, string(claim.RoleImplementer)) {
		t.Errorf("the notice says %q, want it to name the turn the round waits on", notices[0].Reason)
	}
	if notices[0].Update == nil || notices[0].Update.Round != podUpdateRound {
		t.Fatalf("the notice carries %+v, want the round it is of", notices[0].Update)
	}
	// Every claim the notice lists has answered, and it says so: the controller's playbook
	// publishes the update task to a claim that has not answered, and that publish starts a turn,
	// which takes the answer back.
	wantAnswered(t, notices[0], claim.RoleImplementer, claim.RoleTester)
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times while a turn ran", len(calls))
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	if notices := rig.notices(); len(notices) != 1 {
		t.Fatalf("the controller was told %d times over two bounds, want once per round", len(notices))
	}
}

// A ReplacePod that keeps failing is the same stall seen from the runtime's side: every worker has
// answered, the pod is still there, and every one of them is holding its next task. Past the
// notice bound the controller is told, and the error the runtime gave is the reason.
func TestPodUpdateTellsTheControllerWhenTheReplacementKeepsFailing(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.rt.FailReplacePod(errors.New("the api server refused the suspension"))
	rig.tick()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("ReplacePod called %d times, want the one attempt that failed", len(calls))
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times before the notice bound", len(notices))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the round = %q after a failed replacement, want it still open", c.UpdateRound)
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times about a replacement that keeps failing, want once", len(notices))
	}
	if !strings.Contains(notices[0].Reason, "the api server refused the suspension") {
		t.Errorf("the notice says %q, want the error the runtime gave", notices[0].Reason)
	}
	wantAnswered(t, notices[0], claim.RoleImplementer)
}

// Every reason carries ", " inside it already — a moved address, a pod-shape drift, the two
// contracts of a plugin round — so the notice carries what the round records, whole. A list built
// by splitting that string cuts one reason into pieces that each read as a reason of their own.
func TestPodUpdateNoticeKeepsTheReasonsWhole(t *testing.T) {
	rig := newPodUpdateRig(t, podUpdateRole{
		role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
		contract: api.DaemonAPIVersion, round: supervise.PluginRound(api.DaemonAPIVersion),
		reasons: podUpdatePluginReasons,
	})

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	rows := rig.rows(record.OutboxKindControllerNotice)
	if len(rows) != 1 {
		t.Fatalf("the controller was told %d times, want once past the notice bound", len(rows))
	}
	var payload struct {
		Update struct {
			Reasons json.RawMessage `json:"reasons"`
		} `json:"update"`
	}
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("decode the notice: %v", err)
	}
	if got, want := string(payload.Update.Reasons), strconv.Quote(podUpdatePluginReasons); got != want {
		t.Fatalf("the notice carries reasons %s, want the one string the round records, %s", got, want)
	}
}

// wantAnswered holds a notice's waiting list to exactly roles, each marked as having answered the
// round: a round stuck on a turn or on a failing replacement waits on claims that have every one
// of them answered, and the controller must send none of them the update task again.
func wantAnswered(t *testing.T, notice record.ControllerNotice, roles ...claim.Role) {
	t.Helper()
	if notice.Update == nil || len(notice.Update.Waiting) != len(roles) {
		t.Fatalf("the notice waits on %+v, want %v", notice.Update, roles)
	}
	for i, role := range roles {
		if got := notice.Update.Waiting[i]; got.Role != role || !got.Answered {
			t.Errorf("the notice lists %+v, want the %s marked as having answered", got, role)
		}
	}
}

// The sweep decides the replacement from the claims as their machines hold them, never from the
// snapshot the orphan sweep read at the start of the tick. Each earlier issue's replacement in
// that tick can take a whole sweep interval, so after a fleet-wide roll that snapshot is minutes
// old here. A turn that starts inside that window takes the worker's answer back in its machine
// alone (takeBackSignal), and a sweep deciding from the snapshot would see neither the turn nor
// the answer going: it would end that turn and delete the disk the turn's work is on.
func TestPodUpdateReadsTheClaimsAgainBeforeItReplacesThePod(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	asked := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	// The implementer answers from the turn of the round's own ask, and that turn ends.
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: asked.Pending.ID})
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	rig.signal(claim.RoleTester, "e4f5a6b")

	// The snapshot this tick began with: every worker has answered, and none is in a turn.
	snapshot, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims for the sweep: %v", err)
	}
	for _, c := range snapshot {
		if c.UpdateRound != podUpdateRound || c.UpdateSignalledAt == nil || c.State == supervise.StateWorking {
			t.Fatalf("the snapshot holds %s as %s in round %q answered=%t; want every worker answered and idle",
				c.Role, c.State, c.UpdateRound, c.UpdateSignalledAt != nil)
		}
	}
	// Minutes later in that same tick, an Envoy notice starts a turn of the implementer's own,
	// which takes its answer back — in the machine alone.
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	if c := rig.claimOf(claim.RoleImplementer); c.State != supervise.StateWorking || c.UpdateSignalledAt != nil {
		t.Fatalf("the implementer after a turn of its own = %s answered=%t, want it working with its answer taken back",
			c.State, c.UpdateSignalledAt != nil)
	}

	rig.s.reconcilePodUpdates(rig.ctx, snapshot)

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times from a snapshot the implementer's turn has outrun", len(calls))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the implementer's round = %q, want it still open: it must be asked again", c.UpdateRound)
	}
}

// A round that waits on a turn or on a failing replacement is told once, and the bound it is told
// at runs from the last answer rather than from the round's opening. Measured from the opening, a
// round answered just before the bound would spend its one notice on a turn that ends seconds
// later, and the stall that followed would be told nothing at all.
func TestPodUpdateMeasuresTheLateNoticeFromTheLastAnswer(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateWorking, session: true, inPod: true,
			contract: api.DaemonAPIVersion},
		readyWorker(claim.RoleTester),
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)

	// The round all but reaches its bound with nobody having answered, and then both workers
	// answer. The implementer's turn runs on.
	rig.clock.advance(podUpdateNotice - time.Minute)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times, want none: the answers are a minute old", len(notices))
	}

	// Past the round's opening bound, and a minute past its last answer: still nothing.
	rig.clock.advance(2 * time.Minute)
	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times at the opening's bound, want the bound measured from the last answer",
			len(notices))
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times a bound after the last answer, want once", len(notices))
	}
	if !strings.Contains(notices[0].Reason, string(claim.RoleImplementer)) {
		t.Errorf("the notice says %q, want it to name the turn the round waits on", notices[0].Reason)
	}
}

// A round whose issue records no pod at all can move nowhere: no drift is read, no pod is
// replaced, and only the operator who suspended the one worker of the issue can end the wait. It
// is told at the same bound as every other stall, naming the claim it waits on; without that, a
// lone operator-suspended worker's operator is never told.
func TestPodUpdateTellsTheControllerWhenNoClaimRecordsThePod(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.handle(claim.RoleImplementer, supervise.RequestSuspend{
		Claim: rig.tokens[claim.RoleImplementer], By: supervise.SuspendedByOperator,
	})
	rig.awaitClaim(claim.RoleImplementer, "the operator's suspension to let its process go", func(c supervise.Claim) bool {
		return c.State == supervise.StateSuspended && c.Locator == nil
	})

	rig.tick()

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times before the notice bound", len(notices))
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times about a round whose issue records no pod, want once", len(notices))
	}
	if notices[0].Update == nil || notices[0].Update.Round != podUpdateRound {
		t.Fatalf("the notice carries %+v, want the round it is of", notices[0].Update)
	}
	if waiting := notices[0].Update.Waiting; len(waiting) != 1 ||
		waiting[0].Role != claim.RoleImplementer || waiting[0].State != string(supervise.StateSuspended) {
		t.Fatalf("the notice waits on %+v, want the suspended implementer alone", notices[0].Update.Waiting)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written on %d times: %q. The operator reads this round through the notice", len(messages), messages)
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	if notices := rig.notices(); len(notices) != 1 {
		t.Fatalf("the controller was told %d times over two bounds, want once per round", len(notices))
	}
}

// Outside a round the stale-plugin lane divides the claims below the contract by what their
// launch installs. A Claude Code role's plugin is its pod image's own claude-envoy: the relaunch
// installs nothing and keeps the pod, so the process comes back on the same contract, its
// registration is charged, and the claim walks to failed. It is left running, and the round over
// its pod is what carries it. A claim whose launch does install one is relaunched: that is what
// puts it on a plugin with the op, with its pod and its disk kept.
func TestStalePluginLeavesAClaudeWorkerAloneAndRelaunchesOneAnInstallCanCarry(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
			contract: supervise.PodUpdateContract - 1},
		podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, inPod: true,
			contract: supervise.PodUpdateContract - 1},
	)
	rig.s.cfg.WorkerBackends = map[claim.Role]runtime.Backend{claim.RoleImplementer: runtime.BackendClaude}
	claims, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims for the sweep: %v", err)
	}

	rig.s.refreshStalePlugins(claims, "sweep")

	rig.awaitClaim(claim.RoleTester, "the worker whose launch installs a plugin to be relaunched", func(c supervise.Claim) bool {
		return c.State == supervise.StateLaunching
	})
	if c := rig.claimOf(claim.RoleImplementer); c.State != supervise.StateReady {
		t.Errorf("the Claude Code worker is %s, want it left running for the round over its pod", c.State)
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 1 {
		t.Errorf("the sweep started %d processes, want the one whose launch installs a plugin", len(resumes))
	}
}

// snapshotWithout is the claims the orphan sweep puts to the round, less one role's: how a test
// holds a claim out of the group a tick carries, as a claim admitted after that read is held out
// of it. The store still has it, and everything that reads the store still sees it.
func (r *podUpdateRig) snapshotWithout(role claim.Role) []supervise.Claim {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims for the sweep: %v", err)
	}
	var kept []supervise.Claim
	for _, c := range claims {
		if c.Role != role {
			kept = append(kept, c)
		}
	}
	return kept
}

// notDue is every time the runtime asked the daemon whether the replacement was still due and was
// told it was not.
func (r *podUpdateRig) notDue() []fake.Call { return r.rt.CallsOf("ReplacePodNotDue") }

// The replacement's last word is taken inside ReplacePod, from the issue's claims as the store
// holds them then, and not from the group this tick carries. The group is one read of the
// claims, and a claim admitted after it — the next phase's role, launched into the very pod the
// round keeps — is in no group of this tick and in no re-read of it either: every claim the
// group holds is re-read, and a claim it never held is re-read nowhere. Deciding the replacement
// from the group alone would end that role's turn and delete the issue's disk under it.
func TestPodUpdateAbortsTheReplacementForAClaimTheGroupNeverHeld(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		podUpdateRole{role: claim.RoleTester, state: supervise.StateQueued},
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")

	// The group this tick carries: the one worker the issue had when the sweep read its claims,
	// and it has answered.
	group := rig.snapshotWithout(claim.RoleTester)
	// Minutes later in that same tick, the next phase's role is started into the kept pod. It
	// registers there, holds a session from that moment, and has answered nothing.
	rig.startInPod(claim.RoleTester)

	rig.s.reconcilePodUpdates(rig.ctx, group)

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times under a role the group never held", len(calls))
	}
	if refused := rig.notDue(); len(refused) != 1 {
		t.Fatalf("the runtime was told the replacement was no longer due %d times, want once", len(refused))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound {
		t.Fatalf("the implementer's round = %q, want it left open for the next sweep", c.UpdateRound)
	}
	if c := rig.claimOf(claim.RoleTester); c.State != supervise.StateReady || podOf(c.Locator) != podUpdatePod {
		t.Fatalf("the role started into the kept pod is %s at %+v, want it running there", c.State, c.Locator)
	}

	// The next sweep reads the claim the group never held, tasks it with the round, and replaces
	// the pod only once it has answered too.
	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times while the role of the next phase had not answered", len(calls))
	}

	rig.signal(claim.RoleTester, "e4f5a6b")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("the pod was replaced %d times once every worker of the issue answered, want once", len(calls))
	}
}

// A turn that starts while the replacement waits is not cut. The Sandbox runtime takes the pod's
// launch turn before it suspends anything, and that wait is a pod boot long: a round every
// worker of which had answered when the call began is a round a worker can start a turn in while
// it runs, and that turn takes the worker's answer back (takeBackSignal). The runtime asks the
// daemon one statement before the write that ends every process in the pod, the daemon reads the
// claims again there, and the replacement is refused rather than made.
func TestPodUpdateDoesNotCutATurnThatStartsWhileTheReplacementWaits(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	asked := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	// Each worker answers from the turn of the round's own ask, and the implementer's ends: a
	// turn it starts after that is one the daemon sent nothing for.
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: asked.Pending.ID})
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	rig.signal(claim.RoleTester, "e4f5a6b")

	// The runtime holds where it waits for the pod's launch turn, and an Envoy notice starts a
	// turn of the implementer's own inside that wait, which takes its answer back. One hold only:
	// the next call finds the pod as the sweep left it.
	rig.rt.HoldReplacePod(func() {
		rig.rt.HoldReplacePod(nil)
		rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	})

	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times with a turn running in it", len(calls))
	}
	if refused := rig.notDue(); len(refused) != 1 {
		t.Fatalf("the runtime was told the replacement was no longer due %d times, want once", len(refused))
	}
	cut := rig.claimOf(claim.RoleImplementer)
	if cut.State != supervise.StateWorking {
		t.Fatalf("the worker whose turn started inside the wait is %s, want it still %s: its turn was not this round's to cut",
			cut.State, supervise.StateWorking)
	}
	if cut.UpdateRound != podUpdateRound || cut.UpdateSignalledAt != nil {
		t.Fatalf("the worker after its own turn started = round %q answered=%t, want the round open and its answer taken back",
			cut.UpdateRound, cut.UpdateSignalledAt != nil)
	}

	// The round carries on where it stood: the turn ends, the worker is asked again, and only its
	// next answer replaces the pod.
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times before the worker answered again", len(calls))
	}

	rig.signal(claim.RoleImplementer, "b2c3d4e")
	rig.tick()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("the pod was replaced %d times once the worker answered again, want once", len(calls))
	}
}

// A Claude Code worker below the contract that introduced ready_for_update is never posted the
// stale-plugin relaunch, at boot or at a sweep, whether or not a round is open over it: its
// launch installs no plugin, so the relaunch would keep the pod, bring the same plugin back and
// charge the claim toward failed. Its registration is accepted for the same reason, charged
// nothing and refused nothing. What carries such a claim is the round over its pod, which asks
// it, takes the end of its turn as the answer, and replaces the pod onto the new image.
func TestStalePluginLeavesAClaudeWorkerBelowTheContractToItsRound(t *testing.T) {
	rig := newPodUpdateRig(t,
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
			contract: supervise.PodUpdateContract - 1},
		readyWorker(claim.RoleTester),
	)
	rig.s.cfg.WorkerBackends = map[claim.Role]runtime.Backend{claim.RoleImplementer: runtime.BackendClaude}
	claims, err := rig.sup.Claims(rig.ctx)
	if err != nil {
		t.Fatalf("read the claims for the sweep: %v", err)
	}

	// No round is open yet: this is the boot pass, and the sweep that follows it.
	rig.s.refreshStalePlugins(claims, "boot")
	rig.s.refreshStalePlugins(claims, "sweep")
	m, ok := rig.sup.Machine(rig.tokens[claim.RoleImplementer])
	if !ok {
		t.Fatal("the Claude Code worker is not supervised")
	}
	m.Wait()

	if c := rig.claimOf(claim.RoleImplementer); c.State != supervise.StateReady {
		t.Fatalf("the Claude Code worker is %s outside every round, want it left running for the round over its pod", c.State)
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 0 {
		t.Fatalf("the boot pass and the sweep started %d processes, want none: no launch of this deployment installs its plugin", len(resumes))
	}

	// Its process registers again, on the same old contract. The registration stands, and nothing
	// is charged for it: charging it is what walks the claim to failed, one relaunch at a time.
	before := rig.claimOf(claim.RoleImplementer)
	if err := m.Handle(rig.ctx, supervise.RequestRegister{
		Claim: before.Token, Generation: before.Generation, Session: before.Session, SessionFile: before.SessionFile,
		CapabilityHash: []byte(claim.RoleImplementer), PluginContract: supervise.PodUpdateContract - 1,
	}); err != nil {
		t.Fatalf("the registration of a worker no launch can take past the contract = %v, want it accepted", err)
	}
	after := rig.claimOf(claim.RoleImplementer)
	if after.State != supervise.StateReady {
		t.Fatalf("the claim after its stale registration is %s, want it left running", after.State)
	}
	if after.Budgets != (supervise.Budgets{}) || after.PluginRelaunchFor != 0 {
		t.Fatalf("the stale registration left budgets %+v and a relaunch mark for %d, want neither charged",
			after.Budgets, after.PluginRelaunchFor)
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 0 {
		t.Fatalf("the stale registration started %d processes, want none", len(resumes))
	}

	// The round over the pod is what carries such a claim: it asks it like any other worker, the
	// end of that turn is its answer, and the replacement brings the new image its plugin needs.
	// Nobody is told and nothing is written on the issue.
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	asked := rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	if asked.Pending == nil {
		t.Fatal("the Claude Code worker holds no task; the round asks it like any other")
	}
	rig.signal(claim.RoleTester, "a1b2c3d")
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token, DeliveryID: asked.Pending.ID})
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	rig.tick()

	if calls := rig.replacements(); len(calls) != 1 {
		t.Fatalf("the pod was replaced %d times once the Claude Code worker's turn ended, want once", len(calls))
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v about a worker the round carries by itself", notices)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q, want no drain asked of anybody", messages)
	}
	if resumes := rig.rt.CallsOf("Resume"); len(resumes) != 0 {
		t.Fatalf("%d processes were started for a claim no launch of this deployment can take past the contract, want none",
			len(resumes))
	}
}

// A round whose claims record no pod and that every worker has answered is the ordinary end of a
// phase: each pushed, each answered, and the daemon suspended each, which let its process and its
// locator go. Nothing is stalled there and nobody has anything to do about it, so the controller
// is told nothing — a notice would open a finding ask over a round that needs no one.
func TestPodUpdateTellsNobodyOfAPodlessRoundEveryWorkerHasAnswered(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.handle(claim.RoleImplementer, supervise.RequestSuspend{
		Claim: rig.tokens[claim.RoleImplementer], By: supervise.SuspendedByDaemon,
	})
	rig.awaitClaim(claim.RoleImplementer, "the suspension to let its process go", func(c supervise.Claim) bool {
		return c.State == supervise.StateSuspended && c.Locator == nil
	})

	for range 2 {
		rig.clock.advance(podUpdateNotice)
		rig.tick()
	}

	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %d times about a round every worker answered: %+v", len(notices), notices)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written on %d times: %q", len(messages), messages)
	}
}

// The no-pod notice names the workers it waits on, and only those. A round where one worker
// answered and another did not is a round an operator must act on — but only for the one that
// has not answered, and the controller's playbook must be told nothing of the other: publishing
// the update task to a claim that has answered starts a turn, which takes that answer back.
func TestPodUpdatePodlessNoticeListsOnlyTheWorkersThatHaveNotAnswered(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.handle(role, supervise.RequestSuspend{Claim: rig.tokens[role], By: supervise.SuspendedByOperator})
		rig.awaitClaim(role, "the operator's suspension to let its process go", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.Locator == nil
		})
	}

	rig.clock.advance(podUpdateNotice)
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 {
		t.Fatalf("the controller was told %d times about a round one worker has not answered, want once", len(notices))
	}
	waiting := notices[0].Update.Waiting
	if len(waiting) != 1 || waiting[0].Role != claim.RoleTester || waiting[0].Answered {
		t.Fatalf("the notice waits on %+v, want the tester alone, unanswered", waiting)
	}
	if !strings.Contains(notices[0].Reason, string(claim.RoleTester)) {
		t.Errorf("the notice says %q, want it to name the worker it waits on", notices[0].Reason)
	}
}

// The sweep reads every claim of a drifted issue on every tick, and it must never wait on one.
// A claim's machine holds its own lock for the whole of a runtime call, and a Sandbox relaunch
// waits minutes for its pod, so a read that waited would stall the tick behind one relaunching
// claim: the orphan reaping, the stale-plugin posts and every later issue's round with it.
//
// What the sweep reads of such a claim is the machine's published view, and it reads it as not
// having answered whatever that view says: the decision in flight can be the turn that takes the
// answer back, and counting the answer would delete a disk under work that turn can still
// commit. The cost is one sweep.
func TestPodUpdateReadsAClaimMidRelaunchWithoutWaitingAndCountsItUnanswered(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)
	rig.tick()
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.signal(claim.RoleTester, "a1b2c3d")

	// The tester's machine is held inside a Resume: it holds its own lock for the whole call, as
	// a relaunch into a pod being built does.
	released := make(chan struct{})
	read := make(chan []supervise.Claim, 1)
	rig.rt.HoldResume(func() {
		rig.rt.HoldResume(nil)
		read <- rig.s.liveGroup(rig.snapshot())
		<-released
	})
	relaunching := rig.tokens[claim.RoleTester]
	go func() {
		m, _ := rig.sup.Machine(relaunching)
		_ = m.Handle(rig.ctx, supervise.RuntimeObservation{
			Observation: runtime.Observation{Locator: *rig.claimOf(claim.RoleTester).Locator, Kind: runtime.Gone, Detail: "the process ended"},
		})
	}()

	var live []supervise.Claim
	select {
	case live = <-read:
	case <-time.After(10 * time.Second):
		close(released)
		t.Fatal("the live read did not answer while a claim's machine was inside a Resume; it waited on the decision")
	}
	close(released)

	var answered []claim.Role
	for _, c := range live {
		if c.UpdateSignalledAt != nil {
			answered = append(answered, c.Role)
		}
	}
	if len(answered) != 1 || answered[0] != claim.RoleImplementer {
		t.Fatalf("the live read counts %v as answered, want the implementer alone: a claim whose machine is deciding has not answered", answered)
	}
}

// snapshot is the claims the sweep puts to a round, as a tick reads them.
func (r *podUpdateRig) snapshot() []supervise.Claim {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims for the sweep: %v", err)
	}
	var group []supervise.Claim
	for _, c := range claims {
		if c.Issue == podUpdateIssue {
			group = append(group, c)
		}
	}
	return group
}
