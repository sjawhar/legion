package daemon

// INFRA-484 throwaway cross-language proof. Not part of the branch: delete this file before
// reporting.
//
// What no permanent test covers is the agent's own path to the new route, end to end in one run:
// the `ready_for_update` op in `@legion/pi-shared`, its Zod request schema, the HTTP wire, the Go
// handler (internal/api's claims/ready-for-update) and the claim machine. This harness drives it
// over the rig podupdate_test.go and podupdate_fleet_test.go already build — real machines over a
// real Postgres with this branch's real migrations, the real supervision, the fake runtime as the
// cluster — with two differences: the API server is on a real TCP listener rather than httptest,
// and the signal comes from the real plugin client run under Bun, not from a Go caller.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const (
	// wireProofPod is the one issue pod both claims of the issue run in.
	wireProofPod = "4f2a18c6-5d3b-4e07-9a2c-7b8e1d6f0394"
	// wireProofRound is what the sweep names the round of that pod.
	wireProofRound = supervise.PodRoundPrefix + wireProofPod
	// wireProofDrift is what the fake runtime answers for that pod.
	wireProofDrift = "the session store volume, now postgres key=session-dsn"
	// wireProofScript is the Bun half of the proof, run from the package that depends on
	// @legion/pi-shared so the plugin's own package entry resolves.
	wireProofScript = "../../../pi-legion/infra484-wire-proof.ts"
)

// wireWorker is one claim of the pod: the launch's boot token it registers with, the secret the
// registration route issued it, and the connection its process reads tasks over.
type wireWorker struct {
	role   claim.Role
	token  claim.Token
	boot   string
	secret string
	conn   *fake.Conn
}

type wireRig struct {
	t       *testing.T
	ctx     context.Context
	s       *supervision
	sup     *supervisor
	rt      *fake.Runtime
	pool    *pgxpool.Pool
	clock   *heldClock
	workers map[claim.Role]*wireWorker
	// base is the real daemon API server's address, as the plugin's client is given it.
	base   string
	client *http.Client
}

func sessionOf(role claim.Role) string { return "ses_" + string(role) }

func newWireRig(t *testing.T, roles ...claim.Role) *wireRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	dsn := pool.Config().ConnString()

	workers := map[claim.Role]*wireWorker{}
	claims := make([]supervise.Claim, 0, len(roles))
	for _, role := range roles {
		token := mustClaimToken(t, podUpdateIssue, role)
		boot := "boot-" + string(role) + "-" + randomSuffix(t)
		workers[role] = &wireWorker{role: role, token: token, boot: boot}
		claims = append(claims, supervise.Claim{
			Token: token, Project: podUpdateProject, Tree: podUpdateIssue, Issue: podUpdateIssue, Role: role,
			Generation: 1, State: supervise.StateReady, PluginContract: api.DaemonAPIVersion,
			Session: sessionOf(role), SessionFile: "/sessions/" + string(role) + ".jsonl",
			ServingGeneration: 1, BootTokenHash: supervise.HashBootToken(boot),
			Locator: podUpdateLocator(token, podUpdateIssue, wireProofPod),
		})
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
	bootTokens := api.NewBootTokens(st)
	clock := &heldClock{at: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	rt := fake.NewRuntime()
	rt.Now = clock.Now
	conns := fake.NewConns()
	sup := newSupervisor(ctx, st, podUpdateProject, t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: conns, Store: bootTokens.Recording(st), Specs: outboxSpecs{}, Clock: clock, Log: quietLogger(),
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
	for _, w := range workers {
		w.conn = fake.NewConn()
		conns.Register(w.token, w.conn)
	}

	records := record.NewStore()
	rig := &wireRig{t: t, ctx: ctx, sup: sup, rt: rt, pool: pool, clock: clock, workers: workers,
		client: &http.Client{Timeout: 20 * time.Second}}
	rig.s = &supervision{
		cfg: config.Config{Project: "LEGION", PodUpdateRetask: podUpdateRetask, PodUpdateNotice: podUpdateNotice},
		log: quietLogger(), plan: plan{clock: clock, project: podUpdateProject, pluginRelaunchable: true, orphanSweep: orphanSweepInterval},
		runtime: rt, supervisor: sup,
		enqueue: (&workflowRuntime{pool: pool, records: records, log: quietLogger()}).enqueueOutbox,
	}

	// The daemon's own API server, on a real TCP listener rather than httptest's handler.
	srv := api.NewServer("127.0.0.1", 0, api.Options{
		Supervisor: sup, BootTokens: bootTokens, Project: podUpdateProject, DispatchProject: "LEGION",
		PluginRelaunchable: true, Pool: pool, Record: records, Grants: credential.New(nil), Log: quietLogger(),
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the daemon API: %v", err)
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	rig.base = "http://" + listener.Addr().String()
	return rig
}

// register is one claim's registration over the real route, which is what issues the secret every
// claim route then authenticates it by.
func (r *wireRig) register(role claim.Role) {
	r.t.Helper()
	w := r.workers[role]
	body, err := json.Marshal(claim.RegisterRequest{
		BootToken: w.boot, SessionID: sessionOf(role), OmpSessionFile: "/sessions/" + string(role) + ".jsonl",
		AgentID: "agent-" + string(role), PluginContract: api.DaemonAPIVersion,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	status, answer := r.post("/legion/v1/claims/register", body)
	if status != http.StatusOK {
		r.t.Fatalf("%s registered: %d %s", role, status, answer)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(answer, &registered); err != nil {
		r.t.Fatalf("read %s's registration: %v", role, err)
	}
	if registered.Secret == "" {
		r.t.Fatalf("%s's registration answered no secret: %s", role, answer)
	}
	w.secret = registered.Secret
}

func (r *wireRig) post(path string, body []byte) (int, []byte) {
	r.t.Helper()
	request, err := http.NewRequestWithContext(r.ctx, http.MethodPost, r.base+path, bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		r.t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	var read bytes.Buffer
	if _, err := read.ReadFrom(response.Body); err != nil {
		r.t.Fatalf("read the answer of POST %s: %v", path, err)
	}
	return response.StatusCode, bytes.TrimSpace(read.Bytes())
}

func (r *wireRig) drifts(reasons ...string) {
	r.t.Helper()
	for _, w := range r.workers {
		r.rt.ScriptPodDrift(w.token, reasons...)
	}
}

// tick is one orphan sweep over the claims the store holds, which is what the daemon's own tick
// puts to the round.
func (r *wireRig) tick() {
	r.t.Helper()
	claims, err := r.sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims for the sweep: %v", err)
	}
	r.s.reconcilePodUpdates(r.ctx, claims)
}

func (r *wireRig) claimOf(role claim.Role) supervise.Claim {
	r.t.Helper()
	m, ok := r.sup.Machine(r.workers[role].token)
	if !ok {
		r.t.Fatalf("%s is not supervised", role)
	}
	return m.Claim()
}

func (r *wireRig) awaitRound(role claim.Role) supervise.Claim {
	r.t.Helper()
	testwait.Eventually(r.t, fmt.Sprintf("%s to hold the update round %s", role, wireProofRound), func() bool {
		c := r.claimOf(role)
		return c.UpdateRound == wireProofRound && c.UpdateTaskedAt != nil
	})
	return r.claimOf(role)
}

// taskText is the update task as role's process received it over its connection.
func (r *wireRig) taskText(role claim.Role) string {
	r.t.Helper()
	for _, prompt := range r.workers[role].conn.Prompts() {
		if strings.HasPrefix(prompt.DeliveryID, "update:"+wireProofRound+":") {
			return prompt.Message
		}
	}
	r.t.Fatalf("%s received no task for round %s: %+v", role, wireProofRound, r.workers[role].conn.Prompts())
	return ""
}

func (r *wireRig) replacements() []fake.Call { return r.rt.CallsOf("ReplacePod") }

// answer runs the real plugin client under Bun against the listener: executePhaseOp with
// ready_for_update, which posts the route through createLegionDaemonClient over the global fetch.
// It returns the script's exit code (0 accepted, 3 refused) and everything it printed.
func (r *wireRig) answer(role claim.Role, secret, head string) (int, string) {
	r.t.Helper()
	c := r.claimOf(role)
	args, err := json.Marshal(map[string]any{
		"base": r.base, "claimToken": string(c.Token), "sessionId": c.Session, "secret": secret,
		"generation": c.Generation, "tree": c.Tree, "issue": c.Issue, "role": string(c.Role), "head": head,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	script, err := filepath.Abs(wireProofScript)
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.CommandContext(r.ctx, "bun", "run", script, string(args))
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil {
		r.t.Fatalf("run the plugin client for %s: %v\n%s", role, err, out)
	}
	return cmd.ProcessState.ExitCode(), strings.TrimRight(string(out), "\n")
}

// The whole round, in one run: two claims of one issue in one pod, registered over the real
// routes; the pod drifts; the real sweep tasks both; the real plugin client answers for each over
// the real wire; and the pod is replaced exactly once, only after both answers. Two negative
// controls on the way: the pod is not replaced while the second claim has not answered, and the
// route refuses a signal carrying a wrong secret without recording it.
func TestINFRA484ReadyForUpdateWireProof(t *testing.T) {
	rig := newWireRig(t, claim.RoleImplementer, claim.RoleTester)
	rig.register(claim.RoleImplementer)
	rig.register(claim.RoleTester)
	t.Logf("PROOF both claims registered over POST /legion/v1/claims/register at %s", rig.base)

	rig.drifts(wireProofDrift)
	rig.tick()

	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		c := rig.awaitRound(role)
		if c.UpdateReasons != wireProofDrift {
			t.Fatalf("%s was tasked for %q, want the pod's drift %q", role, c.UpdateReasons, wireProofDrift)
		}
	}
	t.Logf("PROOF the task the implementer's process received over its connection:\n%s", rig.taskText(claim.RoleImplementer))
	t.Logf("PROOF the task the tester's process received over its connection:\n%s", rig.taskText(claim.RoleTester))

	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times before any claim answered", len(calls))
	}
	t.Logf("PROOF ReplacePod calls before any answer: %d", len(rig.replacements()))

	// Negative control one: the real route refuses a signal carrying a wrong secret, and the
	// claim's UpdateSignalledAt stays unset.
	code, out := rig.answer(claim.RoleTester, "not-the-secret-the-registration-issued", "deadbee")
	t.Logf("PROOF negative control (wrong secret), the plugin client's own output:\n%s", out)
	if code != 3 {
		t.Fatalf("the plugin client with a wrong secret exited %d, want the refusal's 3", code)
	}
	if !strings.Contains(out, "PROOF response-status 403") || !strings.Contains(out, "Invalid session secret") {
		t.Fatalf("the wrong secret was not refused as an invalid secret:\n%s", out)
	}
	if c := rig.claimOf(claim.RoleTester); c.UpdateSignalledAt != nil {
		t.Fatalf("the tester's UpdateSignalledAt is %s after the refused signal, want it unset", c.UpdateSignalledAt)
	}
	t.Logf("PROOF after the refused signal the tester's UpdateSignalledAt is still unset, and its round is %q", rig.claimOf(claim.RoleTester).UpdateRound)

	// The implementer answers through the real plugin client.
	code, out = rig.answer(claim.RoleImplementer, rig.workers[claim.RoleImplementer].secret, "1f0c9ab")
	t.Logf("PROOF the implementer's answer, the plugin client's own output:\n%s", out)
	if code != 0 {
		t.Fatalf("the plugin client exited %d for the implementer, want 0", code)
	}
	if !strings.Contains(out, "PROOF response-status 204") || !strings.Contains(out, "PROOF op-answer {}") {
		t.Fatalf("the implementer's answer did not reach the route:\n%s", out)
	}
	testwait.Eventually(t, "the implementer's signal to be recorded", func() bool {
		return rig.claimOf(claim.RoleImplementer).UpdateSignalledAt != nil
	})
	if head := rig.claimOf(claim.RoleImplementer).UpdateHead; head != "1f0c9ab" {
		t.Fatalf("the implementer's recorded head is %q, want the one its client reported", head)
	}

	// Negative control two: one answer of two replaces nothing.
	rig.tick()
	if calls := rig.replacements(); len(calls) != 0 {
		t.Fatalf("ReplacePod called %d times while the tester had not answered", len(calls))
	}
	t.Logf("PROOF ReplacePod calls after one answer of two: %d (the tester's UpdateSignalledAt is %v)",
		len(rig.replacements()), rig.claimOf(claim.RoleTester).UpdateSignalledAt)

	// The tester answers through the same client, with the secret its own registration issued.
	code, out = rig.answer(claim.RoleTester, rig.workers[claim.RoleTester].secret, "1f0c9ab")
	t.Logf("PROOF the tester's answer, the plugin client's own output:\n%s", out)
	if code != 0 {
		t.Fatalf("the plugin client exited %d for the tester, want 0", code)
	}
	if !strings.Contains(out, "PROOF response-status 204") {
		t.Fatalf("the tester's answer did not reach the route:\n%s", out)
	}
	testwait.Eventually(t, "the tester's signal to be recorded", func() bool {
		return rig.claimOf(claim.RoleTester).UpdateSignalledAt != nil
	})

	rig.tick()
	testwait.Eventually(t, "the pod to be replaced once both claims answered", func() bool {
		return len(rig.replacements()) == 1
	})
	calls := rig.replacements()
	if len(calls) != 1 {
		t.Fatalf("ReplacePod called %d times once both claims answered, want once", len(calls))
	}
	if pod := podOf(calls[0].Pod.Locator); pod != wireProofPod {
		t.Fatalf("ReplacePod was called over pod %q, want the issue's own pod %q", pod, wireProofPod)
	}
	t.Logf("PROOF ReplacePod calls after both answers: %d, over pod %s", len(calls), podOf(calls[0].Pod.Locator))
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		testwait.Eventually(t, string(role)+" to leave its round with the replacement", func() bool {
			c := rig.claimOf(role)
			return c.UpdateRound == "" && c.UpdateSignalledAt == nil
		})
	}
}
