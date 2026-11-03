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
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
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
// already holds on the claim, as an earlier tick or an earlier release left it; left is the round
// the claim left when the daemon started it again in another pod than that round's own, as the
// launch records it (supervise's leftPodRoundAt); pod names a pod of its own, for a claim that
// records one the rest of the issue has left behind; suspendedBy is who suspended a claim seeded
// suspended.
type podUpdateRole struct {
	role        claim.Role
	state       supervise.ClaimState
	session     bool
	inPod       bool
	contract    int
	round       string
	reasons     string
	signalled   bool
	left        string
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
	// logs is what the sweep itself wrote, which is where a round the store refused to take a
	// notice for is reported (tellUpdateStalled). The machines keep their own quiet logger.
	logs *syncBuffer
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
			c.UpdateTaskedAt, c.UpdateLeftRound = &opened, role.left
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
	logs := &syncBuffer{}
	s := &supervision{
		// The rig's claims run in an issue pod, so its configuration is a Kubernetes
		// deployment's: that is what the Claude Code role's exemption from the stale-plugin
		// lane is read from, beside the role's backend (noPluginInstall).
		cfg: config.Config{Project: "LEGION", Runtime: config.Runtime{Name: "kubernetes"},
			PodUpdateRetask: podUpdateRetask, PodUpdateNotice: podUpdateNotice},
		log:     slog.New(slog.NewJSONHandler(logs, nil)),
		plan:    plan{clock: clock, project: podUpdateProject, pluginRelaunchable: true, orphanSweep: orphanSweepInterval},
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
		workers: map[claim.Role]*fake.Conn{}, tokens: tokens, logs: logs}
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

// awaitSwept waits for role's claim to satisfy want, sweeping between the polls, and is how
// every wait for something only a sweep brings about is written: in this rig no sweep happens
// unless a test calls for one, so a condition the sweep alone decides never arrives while a
// wait merely polls for it. One tick is not enough on its own, as it is not for the replacement
// (awaitReplacement): the sweep reads the claims live and through the deciding mask
// (roundGroup), so a claim whose machine is still settling a decision at that read counts as not
// having answered, and what the round would have decided is left to the next sweep. Nothing is
// given up by the sweeps this makes: the rig's clock is held, so no re-task bound and no notice
// bound is crossed by sweeping again.
//
// The first poll sweeps nothing. A decision the last sweep made is posted to each claim's
// machine and handled there (supervisor.post), so it is in flight rather than undecided, and a
// sweep that read it half landed would decide the round a second time: it would replace the pod
// again, or ask every worker of the round again over the claims the end has already reached
// (openPodUpdate posts to every blocking claim). A wait for what a sweep has already decided is
// awaitClaim's, as is one for what a posted event or a direct call brings about.
//
// A settleAll before each tick was tried here in place of that skipped poll, as the more
// deterministic shape, and it is not: it makes this rig's own
// TestPodUpdateTellsItsWorkerNothingHoweverTheRoundEnds time out in every subtest, where the
// skipped poll passes it 100 times out of 100. settleAll nests its own bounded wait per machine
// inside this one, and a sweep that reads only fully settled machines does not reach the
// decision these waits are for.
func (r *podUpdateRig) awaitSwept(role claim.Role, what string, want func(supervise.Claim) bool) supervise.Claim {
	r.t.Helper()
	// landing is the first poll, which leaves a decision the last sweep posted the time to land.
	landing := true
	testwait.Eventually(r.t, fmt.Sprintf("%s: %s", role, what), func() bool {
		if want(r.claimOf(role)) {
			return true
		}
		if landing {
			landing = false
			return false
		}
		r.tick()
		return want(r.claimOf(role))
	})
	return r.claimOf(role)
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

// endTurn is the worker's turn over as Oh My Pi reports it: the agent_end, and behind it the
// result of the prompt whose turn it was, which is what retires that task (promptResulted). A
// turn whose end leaves the prompt outstanding retires nothing, so a test that waits for the
// round's own ask to go ends the turn this way.
func (r *podUpdateRig) endTurn(role claim.Role, delivery string) {
	r.t.Helper()
	token := r.tokens[role]
	r.handle(role, supervise.StreamTurnEnd{Claim: token})
	r.handle(role, supervise.StreamPromptResult{Claim: token, DeliveryID: delivery, AgentInvoked: true})
}

// startInPod is the daemon starting a role into pod: with the issue's own pod, the launch keeps
// the drifted pod a round holds rather than replacing it where it stands, so the process it
// returns is in that same pod and the role is a worker of the round all the same, running
// degraded there — the pod carries neither the overlay nor the enrollment a pod created now is
// built with. With the pod that replaced the issue's own, it is the next role the workflow
// starts after the pod went, which was never in the round's pod and never in its round. The
// role registers and readies from whichever it is, as any other worker does.
func (r *podUpdateRig) startInPod(role claim.Role, pod string) {
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
	r.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, pod)})
	r.handle(role, supervise.RequestSpawn{Claim: token})
	r.registerInto(role, pod)
}

// displacedInto is the role's process ending with the pod it ran in and the daemon starting the
// role again in pod: the runtime's verdict that the process ended with a pod that is gone
// (runtime.Observation), which relaunches the claim on its own session wherever the runtime
// builds the pod. It is how a role of a dead pod really comes back in the one that replaced it,
// and so the one path that writes what that claim left behind in its round
// (supervise's leftPodRoundAt).
func (r *podUpdateRig) displacedInto(role claim.Role, pod string) {
	r.t.Helper()
	token := r.tokens[role]
	ended := r.claimOf(role).Locator
	if ended == nil {
		r.t.Fatalf("the %s records no process to end with its pod", role)
	}
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	r.workers[role] = conn
	r.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, pod)})
	r.handle(role, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: r.clock.Now(), Displaced: true,
	}})
	r.registerInto(role, pod)
}

// registerInto is the walk from a started process to a ready worker, which every launch of a
// role in these tests takes, and the check that it came up in the pod the caller started it in.
func (r *podUpdateRig) registerInto(role claim.Role, pod string) {
	r.t.Helper()
	token := r.tokens[role]
	generation := r.claimOf(role).Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses_" + string(role),
			SessionFile: "/sessions/" + string(role) + ".jsonl", CapabilityHash: []byte(role), PluginContract: api.DaemonAPIVersion},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses_" + string(role)},
	} {
		r.handle(role, ev)
	}
	if c := r.claimOf(role); c.State != supervise.StateReady || podOf(c.Locator) != pod {
		r.t.Fatalf("the %s is %s at %+v, want it ready in pod %s", role, c.State, c.Locator, pod)
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

// awaitReplacement sweeps until the pod has been replaced n times, and is how every assertion
// that a replacement happened reads the count. One tick is not enough on its own:
// completePodUpdate reads the group live and through the deciding mask (roundGroup), so a claim
// whose machine is still settling a decision at that read counts as not having answered, the
// replacement is deferred, and the round is simply decided by the next sweep instead
// (podupdate.go's completePodUpdate). Nothing is lost by that deferral and the round stays open,
// so a test that reads the count once, immediately after the tick that follows the last answer,
// asserts a race rather than the rule: it passes only when no machine happens to be deciding at
// that moment, which CI run 41340112838 is the reproduction of. Sweeping until the count is
// reached asks the question the rule answers — the pod is replaced once every worker has
// answered — without asking it of one particular sweep. An assertion that a replacement has
// *not* happened needs no such wait and keeps reading the count once, since no deferral can turn
// nothing-yet into a replacement. Either failure names the count and what the caller waited for:
// the wait's, the number of replacements it swept for, and the check below it, the number read
// against the number wanted.
func (r *podUpdateRig) awaitReplacement(n int, what string) []fake.Call {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("%d replacement(s) of the pod %s", n, what), func() bool {
		if len(r.replacements()) >= n {
			return true
		}
		r.tick()
		return len(r.replacements()) >= n
	})
	calls := r.replacements()
	if len(calls) != n {
		r.t.Fatalf("the pod was replaced %d times %s, want %d", len(calls), what, n)
	}
	return calls
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

// A claim that left an *earlier* round is no claim of this round that has left. Its own pod died
// while a round was open over it, the daemon started it again in the pod this issue runs in now,
// and this round's open has not reached it yet: it still records the round it left, and it holds
// this round's pod exactly as any claim the round has not asked does. So the replacement waits
// for it, the next sweep's post asks it, and the pod goes only once it has answered.
//
// The predicate is asked about the round being decided for this case alone (supervise's
// Claim.LeftPodRound, read here by waitingClaims). Asked about the claim's own round instead —
// "has it left the round it records" — such a claim waits for nothing, and this sweep deletes the
// disk of a pod holding work the round asked nobody to push: the claim had been told nothing, so
// it had pushed nothing.
//
// The group's two claims are the issue's implementer and tester because the sweep reads the
// issue's open round from the first claim in token order (openPodRound), and the implementer's
// token sorts first: the round being decided is the one over the pod both claims run in, which
// is what makes the tester's recorded round the earlier one.
func TestPodUpdateWaitsForAClaimThatLeftAnEarlierRound(t *testing.T) {
	previous := supervise.PodRound(podUpdatePreviousPod)
	rig := newPodUpdateRig(t,
		// The implementer is a worker of this pod's round and has answered it.
		podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
			contract: api.DaemonAPIVersion, round: podUpdateRound, reasons: podUpdateDrift, signalled: true},
		// The tester came up in this pod out of the round over the pod before it, which it left
		// unanswered, and no post of this round has reached it.
		podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, inPod: true,
			contract: api.DaemonAPIVersion, round: previous, reasons: podUpdateDrift, left: previous},
	)
	rig.drifts(podUpdateDrift)

	rig.tick()

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times while the round had asked the tester nothing; the disk it holds carries work no worker was told to push",
			len(calls))
	}
	c := rig.awaitRound(claim.RoleTester, podUpdateRound)
	if c.UpdateLeftRound != "" {
		t.Errorf("the tester in this round left %q, want the earlier round's record cleared with it", c.UpdateLeftRound)
	}
	if c.Pending == nil || !strings.HasPrefix(c.Pending.ID, "update:"+podUpdateRound+":") {
		t.Fatalf("the tester's pending = %+v, want this round's own ask", c.Pending)
	}

	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.tick()

	calls := rig.awaitReplacement(1, "once the claim the round had not reached answered")
	if calls[0].Pod.Locator == nil || calls[0].Pod.Locator.Sandbox.PodUID != podUpdatePod {
		t.Fatalf("ReplacePod called with %+v, want a process of pod %s", calls[0].Pod, podUpdatePod)
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
	rig.startInPod(claim.RoleArchitect, podUpdatePod)
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

	rig.awaitReplacement(1, "once the turn ended")
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
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	rig.endTurn(claim.RoleImplementer, asked.Pending.ID)
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

	again := rig.awaitSwept(claim.RoleImplementer, "the worker to be asked again", func(c supervise.Claim) bool {
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
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})

	answered := rig.claimOf(claim.RoleImplementer)
	if answered.UpdateSignalledAt == nil {
		t.Fatal("the end of the update task's turn recorded no answer for a worker that has no op to answer with")
	}
	if answered.UpdateHead != "" {
		t.Errorf("the answer records head %q; a turn's end reports none", answered.UpdateHead)
	}

	rig.tick()

	rig.awaitReplacement(1, "once every worker answered")
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v; a round that finishes by itself needs no one", notices)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q; no drain is asked of anybody", messages)
	}
}

// Drift that clears while a round is open — the operator put the configuration back — ends
// nothing. A round, once opened, ends only by replacing its pod or because that pod is gone: it
// has already asked every worker in the pod to push its work and to start no new work, each has
// answered or is about to, and no path is left that lets one carry on in a pod it was told would
// be replaced. So the pod is replaced here too, which is one restart of each worker in its own
// session.
func TestPodUpdateReplacesThePodWhenTheDriftClearsUnderAnOpenRound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	// The worker's turn — the one it answered from — ends, which retires the round's own task,
	// so nothing of the round is left in its hands when the next tick reads it.
	token := rig.tokens[claim.RoleImplementer]
	answered := rig.claimOf(claim.RoleImplementer).Pending.ID
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	rig.endTurn(claim.RoleImplementer, answered)
	rig.awaitClaim(claim.RoleImplementer, "the round's own task to retire with the turn", func(c supervise.Claim) bool {
		return c.Pending == nil
	})

	rig.drifts()
	rig.tick()

	rig.awaitReplacement(1, "once the drift had gone away: the one replacement the round was opened for")
	rig.awaitClaim(claim.RoleImplementer, "its round to end with the replacement", func(c supervise.Claim) bool {
		return c.UpdateRound == ""
	})
}

// A pod can still be replaced where it stands for a failure of its own — a terminal pod, a failed
// init container, a superseded disk — and that deletes the disk with everything unpushed on it.
// The issue is told so, naming the roles that had not answered, and the round ends.
//
// Two things say the pod went, and this is the second of them: the pod the claims record now is
// another one, which the displaced path brought each role back into. The first is the runtime
// reading the pod gone, which ends the round at the sweep that reads it
// (TestPodUpdateEndsTheRoundAtTheFirstReadOfAGonePod). Which of them the sweep reaches first
// decides nothing but the log line: the report is read from the round's own record of the
// claims it asked and what each had done when it left (supervise's Claim.UpdateLeftRound), so
// it is the same either way. A claim that records no pod at all says nothing of the kind.
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

// podGone scripts the runtime to answer that the pod the claims of the issue record is not there
// at all (runtime.ErrPodGone), as the Sandbox runtime answers a pod replaced where it stood for
// a failure of its own: the pod is gone from the cluster while the claims still record it, which
// is what every sweep between that pod's end and the relaunch of each role reads.
func (r *podUpdateRig) podGone() {
	r.t.Helper()
	for _, token := range r.tokens {
		r.rt.ScriptPodGone(token, true)
	}
}

// The pod of an open round can go before any claim records another one: it is replaced where it
// stands for a failure of its own, and the claims record it for as long as the displaced path
// takes to relaunch each role. The first sweep that reads it gone is where the round ends and
// where what was lost is decided — the roles that had not answered by then — and the issue is
// told so there and then.
//
// Nothing a worker does afterwards changes that report. Each role is relaunched into the pod
// that replaced this one inside the window a round left open would span, and an answer from one
// of those processes is an answer about a pod the round never covered: counted, it would say the
// issue lost nothing where the implementer's unpushed work went with the disk.
func TestPodUpdateEndsTheRoundAtTheFirstReadOfAGonePod(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	rig.signal(claim.RoleTester, "a1b2c3d")
	// The tester's turn — the one it answered from — ends, which retires the round's own ask, so
	// from here it holds nothing and the next thing it is sent is the next thing the daemon says.
	token := rig.tokens[claim.RoleTester]
	answered := rig.claimOf(claim.RoleTester).Pending.ID
	rig.handle(claim.RoleTester, supervise.StreamTurnStart{Claim: token})
	rig.endTurn(claim.RoleTester, answered)
	rig.awaitClaim(claim.RoleTester, "the round's own ask to retire with its turn",
		func(c supervise.Claim) bool { return c.Pending == nil })
	conn := rig.workers[claim.RoleTester]
	before := len(conn.Prompts())

	rig.podGone()
	rig.tick()

	// The message is written inside the sweep, so it is there when the tick returns; the round
	// ends through each claim's own machine, which is what the wait below is for.
	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written on %d times at the sweep that read the pod gone, want once for the pod that went", len(messages))
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
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times for a pod that is not there", len(calls))
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.awaitClaim(role, "its round to end at the sweep that read its pod gone",
			func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	rig.settle(claim.RoleTester)
	if sent := conn.Prompts()[before:]; len(sent) != 0 {
		t.Fatalf("the worker that had answered was sent %+v, want nothing: its pod went, and its launch is what starts it again", sent)
	}

	// The implementer comes back in the pod that replaced the old one and answers the round its
	// task named. The round ended with the pod it was opened over, so that answer is refused and
	// the report the gone read decided stands.
	back := rig.claimOf(claim.RoleImplementer)
	m, ok := rig.sup.Machine(back.Token)
	if !ok {
		t.Fatal("the implementer is not supervised")
	}
	if err := m.Handle(rig.ctx, supervise.PodUpdateSignalled{
		Claim: back.Token, Generation: back.Generation, Session: back.Session, Round: podUpdateRound, Head: "e4f5a6b",
	}); err == nil {
		t.Fatal("an answer naming the round of the pod that went was accepted; that round ended with its pod")
	}
	rig.tickBackIn(podUpdateReplacementPod)

	if after := rig.messages(); len(after) != 1 {
		t.Fatalf("the issue was written on %d times once a worker answered in the pod that replaced the round's own, want the one report the gone read decided", len(after))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateSignalledAt != nil {
		t.Errorf("the implementer records an answer at %s to a round that is over", c.UpdateSignalledAt)
	}
}

// The same first read of a gone pod, over a round every worker had already answered: the workers
// pushed everything they wanted kept before the pod went, so nothing was lost and the issue is
// told nothing. The round still ends there — its pod is gone, and no replacement it could make
// would be of that pod — and the operator's side is told nothing either: a round that ends with
// nothing lost is nobody's business.
func TestPodUpdateEndsAGonePodsRoundWithNoMessageWhenEveryWorkerAnswered(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
		// Each answering turn ends, so no claim of the round is working when the pod is read
		// gone: what ends the round here is the pod, not the completeness the next branch acts
		// on, and a turn still running would leave the two indistinguishable.
		token := rig.tokens[role]
		answered := rig.claimOf(role).Pending.ID
		rig.handle(role, supervise.StreamTurnStart{Claim: token})
		rig.endTurn(role, answered)
		rig.awaitClaim(role, "the round's own ask to retire with its turn",
			func(c supervise.Claim) bool { return c.Pending == nil })
	}

	rig.podGone()
	rig.tick()

	for _, role := range roles {
		rig.awaitClaim(role, "its round to end at the sweep that read its pod gone",
			func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q; every worker of the round had answered, so the pod took nothing unpushed with it", messages)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round that ended with nothing lost", notices)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times for a pod that is not there", len(calls))
	}
}

// A round every worker answered whose claims stop recording its pod between the sweep's
// snapshot and the live read the replacement is decided from: an operator suspended the one
// answered worker, and the suspension let its process go (letGo clears the locator). The pod is
// still there and still drifted, so nothing is replaced, nothing is logged as replaced, and the
// round stays open on the claims with their answers standing. Ended here, the round would leave
// a drifted pod nothing has replaced, the task it was holding would go out, and the next tick
// would open the same round over the same pod again.
//
// The claim records the pod again when its operator resumes it, and the sweep that reads it
// there replaces the pod and ends the round, which is the only end the round had left.
func TestPodUpdateKeepsARoundWhoseLiveClaimStopsRecordingItsLivePod(t *testing.T) {
	const replaced = "supervise: every worker of this update round has answered; the pod is replaced and the round is over"
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.settleAll()

	// The group this tick carries, read while the worker still records the round's pod. Minutes
	// later in that same tick its operator suspends it, so the live read finds no pod at all.
	group := rig.snapshot()
	rig.suspendByOperator(claim.RoleImplementer)

	rig.s.reconcilePodUpdates(rig.ctx, group)

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("the pod was replaced %d times with no claim recording a process in it", len(calls))
	}
	if lines := loggedLines(t, rig.logs.String(), replaced); len(lines) != 0 {
		t.Fatalf("the sweep logged the pod replaced %d times having replaced nothing: %+v", len(lines), lines)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q of a pod that is still there", messages)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round one sweep away from its replacement", notices)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateRound != podUpdateRound || c.UpdateSignalledAt == nil {
		t.Fatalf("the claim after the sweep = round %q answered=%t, want the round open on it and its answer standing",
			c.UpdateRound, c.UpdateSignalledAt != nil)
	}

	rig.resumeInPod(claim.RoleImplementer)

	rig.awaitReplacement(1, "at the sweep that read the round's own pod recorded again")
	rig.awaitClaim(claim.RoleImplementer, "its round to end with the replacement",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })
}

// suspendByOperator is the operator suspending a claim, settled: the process goes with the
// suspension (letGo clears the locator), and the machine is deciding nothing by the time it
// returns, since a sweep's live read counts a claim whose machine is deciding as one that has
// not answered (roundGroup's mask) and would decide something else entirely.
func (r *podUpdateRig) suspendByOperator(role claim.Role) {
	r.t.Helper()
	r.handle(role, supervise.RequestSuspend{Claim: r.tokens[role], By: supervise.SuspendedByOperator})
	r.awaitClaim(role, "the operator's suspension to let its process go", func(c supervise.Claim) bool {
		return c.State == supervise.StateSuspended && c.Locator == nil
	})
	r.settleAll()
}

// resumeInPod is the operator resuming a suspended claim of an open round: the session is
// relaunched, and a launch into a pod that has merely drifted keeps that pod, so the process
// comes up in the very pod the round is open over. The claim is a worker of that round again
// with its answer standing (Claim.LeftPodRound), and it registers and readies there as
// startInPod's role does.
func (r *podUpdateRig) resumeInPod(role claim.Role) {
	r.t.Helper()
	token := r.tokens[role]
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	r.workers[role] = conn
	r.rt.ScriptResume(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, podUpdatePod)})
	r.handle(role, supervise.RequestResume{Claim: token})
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
		r.t.Fatalf("the %s its operator resumed is %s at %+v, want it ready in the round's own pod %s",
			role, c.State, c.Locator, podUpdatePod)
	}
}

// The same round whose live claims record no process in its pod, with the pod really gone: it
// was replaced where it stood for a failure of its own while the sweep ran. There is nothing to
// replace it through and nothing to wait for, so the round ends there, through the one path
// every reading of a gone pod ends a round by (endRoundOverAGonePod), and the issue is written
// what that path writes: nothing, because every worker of this round had answered and the pod
// took nothing unpushed with it.
func TestPodUpdateEndsTheRoundWhenTheRuntimeSaysItsUnrecordedPodIsGone(t *testing.T) {
	const overAGonePod = "supervise: the pod of this issue's update round is not there, and every claim of the round had answered; nothing was lost and the round is over"
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.signal(claim.RoleImplementer, "a1b2c3d")
	rig.settleAll()

	group := rig.snapshot()
	rig.suspendByOperator(claim.RoleImplementer)
	rig.s.runtime = &podGoneAfterTheDriftRead{Runtime: rig.rt}

	rig.s.reconcilePodUpdates(rig.ctx, group)

	rig.awaitClaim(claim.RoleImplementer, "its round to end with the pod that is not there",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })
	if lines := loggedLines(t, rig.logs.String(), overAGonePod); len(lines) != 1 {
		t.Fatalf("the sweep ended the round over a gone pod %d times, want once: %+v", len(lines), lines)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q; every worker of the round had answered, so the pod took nothing unpushed with it", messages)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Fatalf("the controller was told %+v of a round that ended with nothing lost", notices)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times for a pod that is not there", len(calls))
	}
}

// podGoneAfterTheDriftRead is the runtime with the round's pod going while one sweep runs: the
// first read of a pod answers what the test scripted, and every read after it answers
// runtime.ErrPodGone, as a runtime answers a pod replaced where it stood a moment before. The
// sweep reads the drift of the pod the claims record first and asks about the round's own pod
// last, so the pod is there when the round is found complete and gone when the round asks what
// became of it — the window this round can only end in as a gone pod's round.
type podGoneAfterTheDriftRead struct {
	runtime.Runtime
	reads atomic.Int64
}

func (r *podGoneAfterTheDriftRead) PodDrift(ctx context.Context, k runtime.Known) ([]string, error) {
	if r.reads.Add(1) == 1 {
		return r.Runtime.PodDrift(ctx, k)
	}
	return nil, runtime.ErrPodGone
}

// A round ends in three ways, and none of them leaves its worker a word from the daemon. The
// worker here has answered and its turn is over, so it holds nothing: the pod round's own ask
// retired with that turn, and the round held every task since. The pod the round was opened over
// is gone whichever way the round ended — the replacement this round made, a pod that went
// without its round, or a pod that went for a failure of its own and left the round's last tick
// reading no drift over the pod that replaced it — so the worker comes back on a new session and
// a new disk, where its launch is what starts it.
func TestPodUpdateTellsItsWorkerNothingHoweverTheRoundEnds(t *testing.T) {
	for name, tc := range map[string]struct {
		roles        []podUpdateRole
		end          func(*podUpdateRig)
		replacements int
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
	} {
		t.Run(name, func(t *testing.T) {
			rig := newPodUpdateRig(t, tc.roles...)
			rig.drifts(podUpdateDrift)
			rig.tick()
			// Every worker of the pod is in the round before the end under test is driven. The
			// sweep reads the round from the claims the store holds, and a claim whose round is
			// still being written is no member of it: the pod read gone under such a claim has
			// lost nobody, so the sweep opens a round over the pod that replaced this one
			// instead of ending the round over the pod that went.
			for _, role := range tc.roles {
				rig.awaitRound(role.role, podUpdateRound)
			}
			rig.signal(claim.RoleImplementer, "a1b2c3d")
			// The turn the implementer answered from ends, which retires the round's own ask:
			// from here it holds nothing at all.
			token := rig.tokens[claim.RoleImplementer]
			answered := rig.claimOf(claim.RoleImplementer).Pending.ID
			rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
			rig.endTurn(claim.RoleImplementer, answered)
			rig.awaitClaim(claim.RoleImplementer, "the round's own ask to retire with its turn",
				func(c supervise.Claim) bool { return c.Pending == nil })
			conn := rig.workers[claim.RoleImplementer]
			before := len(conn.Prompts())

			tc.end(rig)

			rig.awaitSwept(claim.RoleImplementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
			rig.settle(claim.RoleImplementer)
			if calls := rig.replacements(); len(calls) != tc.replacements {
				t.Fatalf("ReplacePod called %d times, want %d: this is not the end under test", len(calls), tc.replacements)
			}
			if sent := conn.Prompts()[before:]; len(sent) != 0 {
				t.Fatalf("the worker was sent %+v after the round ended, want nothing: its pod is gone, and its launch is what starts it", sent)
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

	next.awaitReplacement(1, "by the daemon that resumed the round")
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

	rig.awaitReplacement(1, "once the live worker answered")
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

	ended := rig.awaitSwept(claim.RoleImplementer, "its plugin round to end", func(c supervise.Claim) bool {
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

	revived := rig.awaitSwept(claim.RoleTester, "the suspended claim to be revived for the round", func(c supervise.Claim) bool {
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

	rig.awaitReplacement(1, "for the one attempt that failed")
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
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	// The implementer answers from the turn of the round's own ask, and that turn ends.
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
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
// launch installs. Under Kubernetes a Claude Code role's plugin is its pod image's own
// claude-envoy: the relaunch installs nothing and keeps the pod, so the process comes back on
// the same contract, its registration is charged, and the claim walks to failed. It is left
// running, and the round over its pod is what carries it. A claim whose launch does install one
// is relaunched: that is what puts it on a plugin with the op, with its pod and its disk kept.
//
// Under tmux the same Claude Code role is exempt from nothing. No tmux launch installs a plugin
// for any backend: a pane resumes on the plugin the daemon's host holds, a Claude Code pane on
// the claude-envoy at LEGION_CLAUDE_PLUGIN_DIR, which its launch reads again. So the relaunch
// does there what it does for an Oh My Pi pane, and the lane relaunches both workers.
func TestStalePluginLeavesAClaudeWorkerAloneUnderKubernetesAndRelaunchesItUnderTmux(t *testing.T) {
	for _, tc := range []struct {
		runtimeName string
		// relaunched is every role the lane started a process for, and left is the role it
		// posted nothing to, which is still running the process it had.
		relaunched []claim.Role
		left       []claim.Role
	}{
		{runtimeName: "kubernetes", relaunched: []claim.Role{claim.RoleTester}, left: []claim.Role{claim.RoleImplementer}},
		{runtimeName: "tmux", relaunched: []claim.Role{claim.RoleImplementer, claim.RoleTester}},
	} {
		t.Run(tc.runtimeName, func(t *testing.T) {
			rig := newPodUpdateRig(t,
				podUpdateRole{role: claim.RoleImplementer, state: supervise.StateReady, session: true, inPod: true,
					contract: supervise.PodUpdateContract - 1},
				podUpdateRole{role: claim.RoleTester, state: supervise.StateReady, session: true, inPod: true,
					contract: supervise.PodUpdateContract - 1},
			)
			rig.s.cfg.Runtime = config.Runtime{Name: tc.runtimeName}
			rig.s.cfg.WorkerBackends = map[claim.Role]runtime.Backend{claim.RoleImplementer: runtime.BackendClaude}
			claims, err := rig.sup.Claims(rig.ctx)
			if err != nil {
				t.Fatalf("read the claims for the sweep: %v", err)
			}

			rig.s.refreshStalePlugins(claims, "sweep")

			for _, role := range tc.relaunched {
				rig.awaitClaim(role, "the worker a relaunch can carry past the contract to be relaunched", func(c supervise.Claim) bool {
					return c.State == supervise.StateLaunching
				})
			}
			for _, role := range tc.left {
				if c := rig.claimOf(role); c.State != supervise.StateReady {
					t.Errorf("the Claude Code worker is %s, want it left running for the round over its pod", c.State)
				}
			}
			if resumes := rig.rt.CallsOf("Resume"); len(resumes) != len(tc.relaunched) {
				t.Errorf("the sweep started %d processes under %s, want %d: %v",
					len(resumes), tc.runtimeName, len(tc.relaunched), tc.relaunched)
			}
		})
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
	rig.startInPod(claim.RoleTester, podUpdatePod)
	// Every machine settles before the sweep: the first tick's ask send can still be in flight,
	// and a sweep that reads the implementer deciding its outcome masks the answer (roundGroup),
	// waits for it and calls no ReplacePod at all, so the refusal below would be a coin toss.
	rig.settleAll()

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

	rig.awaitReplacement(1, "once every worker of the issue answered")
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
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	// Each worker answers from the turn of the round's own ask, and the implementer's ends: a
	// turn it starts after that is one the daemon sent nothing for.
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
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

	rig.awaitReplacement(1, "once the worker answered again")
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
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	rig.tick()

	rig.awaitReplacement(1, "once the Claude Code worker's turn ended")
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
	// Both machines settle before the hold, so the one claim the read finds deciding is the one
	// this test holds there: a sweep's own ask send still in flight would mask another claim's
	// answer and make the assertion below a coin toss.
	rig.settleAll()

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

// A round past its notice bound whose only unanswered worker is one the sweep's own read masked
// — its machine was deciding at that moment — is not stalled, and the operator's side is told
// nothing of it. The notice is built from a second, unmasked read (noticeGroup), which finds
// every claim of the round answered, so the row would name no claim at all: the store refuses
// such a row, an update-stalled notice requiring its round and the claims it waits on, and the
// refusal leaves the round unmarked, so every tick until that decision ended would build the
// same row, be refused again and log it at Error. Such a round is one decision away from
// complete, and the next sweep decides it.
//
// The control is the same round past the same bound with a worker that really has not answered:
// there the unmasked read finds it waiting, and the operator's side is told once, naming it.
func TestPodUpdateTellsNobodyOfARoundWaitingOnlyOnAClaimMidDecision(t *testing.T) {
	// refusedNotice is what the sweep logs when the store will not take the notice's row.
	const refusedNotice = "supervise: tell the operator's side that an update round is stalled; telling again at the next sweep"
	for _, tc := range []struct {
		name string
		// answers is whether the tester answers the round, and held whether its machine is
		// inside a decision for the whole of the sweep that follows — which is what makes the
		// sweep's own read count an answered claim as one the round waits on. Only an answer
		// the mask hides leaves the unmasked read with nothing to name.
		answers bool
		held    bool
		notices int
	}{
		{name: "the claim the round waits on answered, and its machine is deciding", answers: true, held: true, notices: 0},
		{name: "the claim the round waits on has not answered", answers: false, notices: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
			rig.drifts(podUpdateDrift)
			rig.tick()
			rig.awaitRound(claim.RoleImplementer, podUpdateRound)
			rig.awaitRound(claim.RoleTester, podUpdateRound)
			rig.signal(claim.RoleImplementer, "a1b2c3d")
			if tc.answers {
				rig.signal(claim.RoleTester, "a1b2c3d")
			}
			// Every machine settles first, so the one claim the sweep finds deciding is the one
			// this test holds there.
			rig.settleAll()

			if tc.held {
				// The tester's machine is held inside a Resume for the whole of the sweep
				// below, as a relaunch into a pod being built holds it: the sweep reads that
				// machine's published view and counts it as not having answered.
				pod := *rig.claimOf(claim.RoleTester).Locator
				entered, release := make(chan struct{}), make(chan struct{})
				rig.rt.HoldResume(func() {
					rig.rt.HoldResume(nil)
					close(entered)
					<-release
				})
				defer close(release)
				go func() {
					m, _ := rig.sup.Machine(rig.tokens[claim.RoleTester])
					_ = m.Handle(rig.ctx, supervise.RuntimeObservation{
						Observation: runtime.Observation{Locator: pod, Kind: runtime.Gone, Detail: "the process ended"},
					})
				}()
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("the tester's machine never reached the Resume this test holds it in")
				}
			}

			rig.clock.advance(podUpdateNotice)
			rig.tick()

			notices := rig.notices()
			if len(notices) != tc.notices {
				t.Fatalf("the controller was told %d times, want %d: %+v", len(notices), tc.notices, notices)
			}
			if refused := loggedLines(t, rig.logs.String(), refusedNotice); len(refused) != 0 {
				t.Fatalf("the sweep logged %d refused notices, want none: %+v", len(refused), refused)
			}
			if tc.notices == 0 {
				return
			}
			if waiting := notices[0].Update.Waiting; len(waiting) != 1 ||
				waiting[0].Role != claim.RoleTester || waiting[0].Answered {
				t.Fatalf("the notice waits on %+v, want the tester alone, unanswered", waiting)
			}
		})
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

// settleAll waits for every machine of the rig to be deciding nothing, which is what a tick
// between sweeps leaves.
func (r *podUpdateRig) settleAll() {
	r.t.Helper()
	for role, token := range r.tokens {
		m, ok := r.sup.Machine(token)
		if !ok {
			continue
		}
		m.Wait()
		testwait.Eventually(r.t, string(role)+" to be deciding nothing", func() bool { return m.Deciding() == nil })
	}
}

// The report names the members of the round that lost work, and nothing else of the issue. Two
// claims of the issue are no members of it at all, and each was named before the round kept a
// record of its own.
//
// The first is the claim an earlier sweep's end has already landed on. The end is posted to each
// machine and handled there, so a machine held in a launch handles it a minute after its
// neighbour does: the sweep that runs in between reads one claim out of the round and one still
// in it, takes the round from the one still in it, and read as "has not answered this round" the
// claim that is out of it is a worker that lost its work — though it answered before the pod
// went.
//
// The second is the role the workflow first started after the pod went. It never ran in the pod
// the round was opened over, it is in no round, and it has answered nothing, which is the same
// reading again.
//
// What each really left is the round's own record, written where the launch records the pod the
// claim came up in: the round it left, and whether it had answered it by then.
func TestPodUpdateNamesOnlyTheRoundsOwnMembersAsHavingLostWork(t *testing.T) {
	rig := newPodUpdateRig(t,
		readyWorker(claim.RoleImplementer),
		readyWorker(claim.RoleTester),
		podUpdateRole{role: claim.RoleReviewer, state: supervise.StateQueued},
	)
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.awaitRound(claim.RoleTester, podUpdateRound)
	// The tester pushes its work and answers; the implementer never does.
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.settleAll()

	// The pod goes for a failure of its own, and the displaced path brings both workers back in
	// the pod that replaced it. Each leaves the round where that pod left it.
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.displacedInto(role, podUpdateReplacementPod)
	}
	if c := rig.claimOf(claim.RoleTester); c.UpdateLeftRound != podUpdateRound || c.UpdateSignalledAt == nil {
		t.Fatalf("the tester left %q with answered=%t, want the round left with its answer standing",
			c.UpdateLeftRound, c.UpdateSignalledAt != nil)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateLeftRound != podUpdateRound || c.UpdateSignalledAt != nil {
		t.Fatalf("the implementer left %q with answered=%t, want the round left unanswered",
			c.UpdateLeftRound, c.UpdateSignalledAt != nil)
	}

	// An earlier sweep ended the round, and only the tester's machine has handled that end so
	// far: the implementer's is still to come.
	rig.handle(claim.RoleTester, supervise.PodUpdateEnded{Claim: rig.tokens[claim.RoleTester], Round: podUpdateRound})
	rig.awaitClaim(claim.RoleTester, "the end of its round to land", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	// The workflow starts the next phase's role, which comes up in the pod that replaced the
	// round's own and has answered nothing.
	rig.startInPod(claim.RoleReviewer, podUpdateReplacementPod)
	rig.settleAll()

	rig.tick()

	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written on %d times, want once for the pod that went: %q", len(messages), messages)
	}
	if !strings.Contains(messages[0], string(claim.RoleImplementer)) {
		t.Errorf("the message does not name the implementer, which had not answered when the pod went: %s", messages[0])
	}
	if strings.Contains(messages[0], string(claim.RoleTester)) {
		t.Errorf("the message names the tester, which answered before the pod went and whose round has already ended: %s", messages[0])
	}
	if strings.Contains(messages[0], string(claim.RoleReviewer)) {
		t.Errorf("the message names the reviewer, which was first started in the pod that replaced the round's own: %s", messages[0])
	}
	rig.awaitClaim(claim.RoleImplementer, "its round to end with the report",
		func(c supervise.Claim) bool { return c.UpdateRound == "" })
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times for a pod that is already gone", len(calls))
	}
}

// A worker suspended in the middle of a turn in the pod that replaced its round's own, and then
// answering that round. The suspension lets the process go and clears the claim's locator
// (letGo), which is where the claim's part in the round used to thaw: read from the locator, the
// claim stopped counting as one that had left, and this answer was taken for the round. It names
// work the dead pod's disk held, which no process of this claim has seen, so it is refused.
func TestPodUpdateRefusesALateAnswerFromAClaimSuspendedAfterItLeftTheRound(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)

	// The pod goes, and the implementer comes back in the pod that replaced it, having answered
	// nothing.
	rig.displacedInto(claim.RoleImplementer, podUpdateReplacementPod)
	// The worker is in a turn of its own in that new pod when its operator suspends it, so the
	// suspension waits for the turn and takes the process when the turn ends.
	token := rig.tokens[claim.RoleImplementer]
	rig.handle(claim.RoleImplementer, supervise.StreamTurnStart{Claim: token})
	held, _ := rig.sup.Machine(token)
	// A suspension asked for during a turn answers ErrSuspendHeld: it is a wait, and the end of
	// the turn is what runs it (holdSuspension).
	if err := held.Handle(rig.ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByOperator}); !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("the operator's suspension during the turn returned %v, want it held for that turn", err)
	}
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	suspended := rig.awaitClaim(claim.RoleImplementer, "the suspension to take its process",
		func(c supervise.Claim) bool { return c.State == supervise.StateSuspended && c.Locator == nil })
	if suspended.UpdateLeftRound != podUpdateRound || suspended.UpdateSignalledAt != nil {
		t.Fatalf("the suspended claim left %q with answered=%t, want the round it left standing unanswered",
			suspended.UpdateLeftRound, suspended.UpdateSignalledAt != nil)
	}

	m, ok := rig.sup.Machine(token)
	if !ok {
		t.Fatal("the implementer is not supervised")
	}
	err := m.Handle(rig.ctx, supervise.PodUpdateSignalled{
		Claim: token, Generation: suspended.Generation, Session: suspended.Session,
		Round: podUpdateRound, Head: "e4f5a6b",
	})

	if !errors.Is(err, supervise.ErrLeftPodRound) {
		t.Fatalf("the late answer from the suspended claim returned %v, want the refusal of a claim that left the round", err)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Fatalf("the claim after the refused answer = answered %v head %q, want nothing of that answer recorded",
			c.UpdateSignalledAt, c.UpdateHead)
	}
}

// envoyTurn is a turn the daemon sent nothing for, started in the pod that replaced the round's
// own: an Envoy notice delivered into the relaunched session, or a person's steer. A claim that
// has left its round keeps its answer through such a turn (takeBackSignal), because the answer
// was given in the pod the round is about and no turn of the pod that replaced it can move the
// branch that pod held.
func (r *podUpdateRig) envoyTurn(role claim.Role) {
	r.t.Helper()
	r.handle(role, supervise.StreamTurnStart{Claim: r.tokens[role]})
	if c := r.claimOf(role); c.State != supervise.StateWorking {
		r.t.Fatalf("the %s after a turn of its own is %s, want it %s", role, c.State, supervise.StateWorking)
	}
}

// Whether this round's pod is still there is asked before the round waits on a turn, because
// every such wait in the round's completion is a wait on that pod. A round whose pod no live
// claim records has nothing in that pod to cut and nothing there to replace: the turn the sweep
// would wait for runs in the
// pod that replaced this one, waiting for it would hold every answered worker's next task behind
// a round that can never end, and the stalled-round notice would name a pod that is gone.
//
// So the round is decided here rather than held: it ends by the one path every reading of a gone
// pod ends a round by (endRoundOverAGonePod), the turn in the new pod is left running, and the
// operator's side is told nothing, however long past the notice bound the sweep runs.
func TestPodUpdateDecidesAGonePodsRoundRatherThanWaitForATurnInThePodThatReplacedIt(t *testing.T) {
	const held = "supervise: every worker of this update round has answered; the replacement waits for the turns still running"
	const overAGonePod = "supervise: the pod of this issue's update round is not there, and every claim of the round had answered; nothing was lost and the round is over"
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
	}
	rig.settleAll()

	// The pod goes, and the displaced path brings both workers back in the pod that replaced
	// it; one of them is in a turn of its own there when the sweep runs.
	for _, role := range roles {
		rig.displacedInto(role, podUpdateReplacementPod)
	}
	rig.envoyTurn(claim.RoleImplementer)
	rig.settleAll()
	// Long past the bound the round's own stall is told at, so a sweep that waited for that
	// turn would tell the operator's side of a pod that is gone.
	rig.clock.advance(podUpdateNotice + time.Hour)

	rig.tick()

	// What the sweep itself did, read before the round's end is waited for: a sweep that waited
	// on the turn instead says so in these three, and leaves the round open behind them.
	if lines := loggedLines(t, rig.logs.String(), held); len(lines) != 0 {
		t.Errorf("the sweep waited for the turns still running %d times, and that turn is in the pod that replaced this round's own: %+v",
			len(lines), lines)
	}
	if lines := loggedLines(t, rig.logs.String(), overAGonePod); len(lines) != 1 {
		t.Errorf("the sweep ended the round over a gone pod %d times, want once: %+v", len(lines), lines)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Errorf("the controller was told %q, over %+v: the round it names is of a pod that is gone, and nothing of that round waits on a turn",
			notices[0].Reason, notices[0].Update)
	}
	if messages := rig.messages(); len(messages) != 0 {
		t.Errorf("the issue was written %q; every worker of the round had answered before its pod went", messages)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Errorf("ReplacePod called %d times for a pod that is already gone", len(calls))
	}
	if c := rig.claimOf(claim.RoleImplementer); c.State != supervise.StateWorking {
		t.Errorf("the worker whose turn runs in the pod that replaced the round's own is %s, want it still %s: that turn was never this round's to wait for",
			c.State, supervise.StateWorking)
	}
	for _, role := range roles {
		rig.awaitClaim(role, "its round to end over the pod that is gone",
			func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
}

// Every worker answered, the pod went, and one of them takes a turn of its own in the pod that
// replaced it. The issue is told nothing: what the round's pod took with it is read from the
// round's own record of each member (UpdateLeftRound, and the answer a claim that has left never
// loses), and that member had answered when it left. Read from "has not answered now", the turn
// would have taken the answer back (takeBackSignal) and the issue would be told a worker lost
// work that worker had pushed.
func TestPodUpdateTellsTheIssueNothingWhenAnAnsweredWorkerTakesATurnInTheNewPod(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
	}
	rig.settleAll()

	for _, role := range roles {
		rig.displacedInto(role, podUpdateReplacementPod)
	}
	// An Envoy notice starts a turn of the implementer's own in the new pod, and that turn ends.
	token := rig.tokens[claim.RoleImplementer]
	rig.envoyTurn(claim.RoleImplementer)
	rig.handle(claim.RoleImplementer, supervise.StreamTurnEnd{Claim: token})
	rig.settleAll()
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateSignalledAt == nil || c.UpdateLeftRound != podUpdateRound {
		t.Fatalf("the relaunched worker after a turn of its own = answered=%t, left %q; want its answer to the dead pod's round standing",
			c.UpdateSignalledAt != nil, c.UpdateLeftRound)
	}

	rig.tick()

	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q; every worker of the round had answered before its pod went", messages)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Errorf("the controller was told %+v of a round that ended with nothing lost", notices)
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Errorf("ReplacePod called %d times for a pod that is already gone", len(calls))
	}
	for _, role := range roles {
		rig.awaitClaim(role, "its round to end over the pod that is gone",
			func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
}

// The same report, where the pod went by this round's own replacement rather than by a failure
// of its own: ReplacePod suspended the Sandbox, which displaces the pod, and then ran out of the
// time the sweep bounds it to while it waited for that pod to go. The daemon is told a failure,
// the round stays open, and the pod goes all the same — the suspension had landed — which brings
// every worker back in the pod built in its place.
//
// The round ends at the sweep that reads them there, the pod it could replace is gone, and the
// issue is told nothing: each worker had answered when it left the round.
func TestPodUpdateTellsTheIssueNothingWhenTheReplacementTimesOutAfterItsSuspensionLanded(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
		rig.signal(role, "a1b2c3d")
	}
	rig.rt.FailReplacePod(fmt.Errorf("wait for the pod of the suspended sandbox to go: %w", context.DeadlineExceeded))

	rig.awaitReplacement(1, "once every worker answered")

	for _, role := range roles {
		if c := rig.claimOf(role); c.UpdateRound != podUpdateRound || c.UpdateSignalledAt == nil {
			t.Fatalf("the %s after the replacement failed = round %q answered=%t, want the round open with its answer standing",
				role, c.UpdateRound, c.UpdateSignalledAt != nil)
		}
		rig.displacedInto(role, podUpdateReplacementPod)
	}
	token := rig.tokens[claim.RoleTester]
	rig.envoyTurn(claim.RoleTester)
	rig.handle(claim.RoleTester, supervise.StreamTurnEnd{Claim: token})
	rig.settleAll()

	rig.tick()

	if messages := rig.messages(); len(messages) != 0 {
		t.Fatalf("the issue was written %q; every worker of the round had answered before the replacement took its pod", messages)
	}
	if notices := rig.notices(); len(notices) != 0 {
		t.Errorf("the controller was told %+v of a round whose pod the replacement took", notices)
	}
	if calls := rig.replacements(); len(calls) != 1 {
		t.Errorf("the pod was replaced %d times, want the one call that timed out: the pod it names is gone", len(calls))
	}
	for _, role := range roles {
		rig.awaitClaim(role, "its round to end over the pod the replacement took",
			func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
}

// One worker had not answered when the pod went, and it answers from the process the daemon
// started in the pod that replaced it. The issue is told once that this worker lost work: the
// report is the round's own record of what each member left with, and this member left having
// answered nothing.
//
// The late answer changes none of it. It names work the dead pod's disk held, which no process
// of this claim has seen, so it is refused (ErrLeftPodRound) and nothing of it is recorded; and
// the round ends with the report, so no later sweep writes the message again.
func TestPodUpdateTellsTheIssueOnceOfAWorkerThatAnswersOnlyInTheNewPod(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.drifts(podUpdateDrift)

	rig.tick()
	roles := []claim.Role{claim.RoleImplementer, claim.RoleTester}
	for _, role := range roles {
		rig.awaitRound(role, podUpdateRound)
	}
	// The tester pushes its work and answers; the implementer never does.
	rig.signal(claim.RoleTester, "a1b2c3d")
	rig.settleAll()

	for _, role := range roles {
		rig.displacedInto(role, podUpdateReplacementPod)
	}
	back := rig.claimOf(claim.RoleImplementer)
	m, ok := rig.sup.Machine(back.Token)
	if !ok {
		t.Fatal("the implementer is not supervised")
	}
	err := m.Handle(rig.ctx, supervise.PodUpdateSignalled{
		Claim: back.Token, Generation: back.Generation, Session: back.Session, Round: podUpdateRound, Head: "e4f5a6b",
	})
	if !errors.Is(err, supervise.ErrLeftPodRound) {
		t.Fatalf("the answer from the pod that replaced the round's own returned %v, want the refusal of a claim that left the round", err)
	}
	if c := rig.claimOf(claim.RoleImplementer); c.UpdateSignalledAt != nil || c.UpdateHead != "" {
		t.Fatalf("the claim after the refused answer = answered %v head %q, want nothing of that answer recorded",
			c.UpdateSignalledAt, c.UpdateHead)
	}
	rig.settleAll()

	rig.tick()

	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written on %d times, want once for the pod that went: %q", len(messages), messages)
	}
	if !strings.Contains(messages[0], string(claim.RoleImplementer)) {
		t.Errorf("the message does not name the implementer, which had answered nothing when the pod went: %s", messages[0])
	}
	if strings.Contains(messages[0], string(claim.RoleTester)) {
		t.Errorf("the message names the tester, which answered before the pod went: %s", messages[0])
	}
	if calls := rig.replacements(); len(calls) != 0 {
		t.Errorf("ReplacePod called %d times for a pod that is already gone", len(calls))
	}
	for _, role := range roles {
		rig.awaitClaim(role, "its round to end with the report",
			func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}

	// The sweep that follows the round's end finds no round of this pod at all, and writes
	// nothing more: the issue is told once.
	rig.tick()

	if after := rig.messages(); len(after) != 1 {
		t.Fatalf("the issue was written on %d times over two sweeps, want the one report the round's end decided: %q", len(after), after)
	}
}
