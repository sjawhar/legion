package daemon

// A process on a plugin contract this daemon does not speak — re-adopted at boot on another
// contract (or none), or registering stale after its own relaunch — is relaunched onto the current
// plugin and, for a registration, refused in words. The boot tests that pin the contract comparison
// itself (relaunched onto another contract, or left alone on the daemon's own) boot the real daemon
// (startDaemonLogging) over claims seeded in the store first (seedWorkflowTree): what they pin is
// what the daemon's own boot does with claims it loaded from the store itself, not a bare
// supervisor's. The still-launching and held-suspension boot cases drive refreshStalePlugins
// directly over a bare supervisor (no store, no HTTP) instead: the same construction outbox_test.go's
// newOutboxSupervisor uses, since what they pin needs no store round-trip. The registration tests
// drive the real HTTP route, since what they pin is the 409 body an agent and an operator read.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// newPluginSupervisor is a bare supervisor over the fake runtime, deps set for the stale-plugin
// trigger: contract is the daemon's own (supervise.Deps.PluginContract) and relaunchable its
// PluginRelaunchable — the registration path's own switch (staleRegistration); the boot pass reads
// s.plan.pluginRelaunchable instead, which the boot tests below set on the bare *supervision they
// build around this supervisor.
func newPluginSupervisor(t *testing.T, contract int, relaunchable bool) (*supervisor, *fake.Runtime) {
	t.Helper()
	rt := fake.NewRuntime()
	sup := newSupervisor(context.Background(), nil, "legion", t.TempDir(), quietLogger())
	sup.deps = supervise.Deps{
		Runtime: rt, Conns: fake.NewConns(), Store: &outboxClaimStore{}, Specs: outboxSpecs{}, Clock: stillClock{}, Log: quietLogger(),
		Limits:             supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts:           supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second, LaunchBackoff: time.Millisecond},
		PluginContract:     contract,
		PluginRelaunchable: relaunchable,
	}
	t.Cleanup(sup.stop)
	return sup, rt
}

func pluginTestLocator(token claim.Token) *runtime.Locator {
	return &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "4242:1", Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
}

// A claim re-adopted at boot on another plugin contract than the daemon's own — recorded 0, as
// every claim is at the first boot on this release, or an earlier daemon's own number — is
// relaunched at once onto the current plugin, charged nothing, through the boot pass
// (refreshStalePlugins) supervision.start runs before any unfinished launch. Driven through a real
// daemon boot over claims seeded in the store first (seedWorkflowTree), since what this pins is what
// the daemon's own boot does with claims it loaded from the store itself.
func TestRunRelaunchesAClaimOnAnotherPluginContractAtBootChargingNothing(t *testing.T) {
	pool := isolatedOutboxPool(t)
	cfg := testConfig(t)
	cfg.PostgresDSN = pool.Config().ConnString()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.Project + "-1"
	architect, err := claim.NewToken(project, root, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	implementer, err := claim.NewToken(project, root, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	sessionFiles := map[claim.Token]string{
		architect:   filepath.Join(cfg.StateDir, "architect.jsonl"),
		implementer: filepath.Join(cfg.StateDir, "implementer.jsonl"),
	}
	contracts := map[claim.Token]int{architect: 0, implementer: 23}
	seedWorkflowTree(t, cfg.PostgresDSN, project,
		record.Issue{Key: root, Project: cfg.Project, Tree: root, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U"},
		supervise.Claim{Token: architect, Project: project, Tree: root, Issue: root, Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateReady, Session: "ses_architect", SessionFile: sessionFiles[architect],
			PluginContract: contracts[architect], Locator: pluginTestLocator(architect)},
		supervise.Claim{Token: implementer, Project: project, Tree: root, Issue: root, Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateReady, Session: "ses_implementer", SessionFile: sessionFiles[implementer],
			PluginContract: contracts[implementer], Locator: pluginTestLocator(implementer)},
	)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	logs := &syncBuffer{}

	d := startDaemonLogging(t, cfg, o, jsonLogger(logs))

	for _, token := range []claim.Token{architect, implementer} {
		testwait.Eventually(t, "the stale-plugin relaunch of "+string(token), func() bool {
			return len(resumesOf(rt, token)) == 1
		})
		resume := resumesOf(rt, token)[0]
		if resume.Spec.ResumeSessionFile != sessionFiles[token] || resume.Spec.Generation != 2 {
			t.Fatalf("resumed %+v, want %s's recorded session at generation 2", resume.Spec, token)
		}
		if budgets := d.claim(token).Budgets; budgets.LaunchFailures != 0 || budgets.Deaths != 0 {
			t.Fatalf("budgets of %s after the stale-plugin relaunch = %+v, want zero", token, budgets)
		}
	}
	lines := loggedLines(t, logs.String(), "supervise: the process speaks another plugin contract; relaunching it onto the current plugin")
	if len(lines) != 2 {
		t.Fatalf("relaunch log lines = %d, want one per claim (log: %s)", len(lines), logs.String())
	}
	seen := map[claim.Token]bool{}
	for _, line := range lines {
		token := claim.Token(line["claim"].(string))
		if line["pluginContract"] != float64(contracts[token]) || line["daemonApiVersion"] != float64(api.DaemonAPIVersion) || line["trigger"] != "boot" {
			t.Fatalf("relaunch log line of %s = %+v, want pluginContract %d, daemonApiVersion %d, trigger boot",
				token, line, contracts[token], api.DaemonAPIVersion)
		}
		seen[token] = true
	}
	if !seen[architect] || !seen[implementer] {
		t.Fatalf("relaunch log lines = %+v, want one naming each claim", lines)
	}
	if calls := rt.CallsOf("Spawn"); len(calls) != 0 {
		t.Fatalf("spawned %v, want every relaunch to resume the recorded session instead", calls)
	}
}

// A boot relaunch the launch path itself refuses — a tree lifecycle cleanup already reserved, here
// — leaves the claim exactly as it found it: refreshPlugin's own checkLaunch runs before the Warn,
// the PluginRelaunchFor mark, and the turn's interruption, so the refused attempt changes nothing,
// its confirmed pending delivery included, and logs no relaunching line while the refusal lasts.
// The orphan sweep is the one periodic pass that already reads every claim, so once the refusal
// clears the claim relaunches at the next sweep without a restart. A current claim seeded beside
// it is never touched.
func TestAStalePluginRelaunchTheLaunchPathRefusesIsRetriedAtTheNextSweep(t *testing.T) {
	pool := isolatedOutboxPool(t)
	cfg := testConfig(t)
	cfg.PostgresDSN = pool.Config().ConnString()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.Project + "-1"
	stale, err := claim.NewToken(project, root, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	current, err := claim.NewToken(project, root, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(cfg.StateDir, "implementer.jsonl")
	seedWorkflowTree(t, cfg.PostgresDSN, project,
		record.Issue{Key: root, Project: cfg.Project, Tree: root, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U"},
		supervise.Claim{Token: stale, Project: project, Tree: root, Issue: root, Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateWorking, Session: "ses_implementer", SessionFile: sessionFile,
			PluginContract: 0, Locator: pluginTestLocator(stale),
			Pending: &supervise.Delivery{ID: "outbox:1", Task: "implement the plan", Generation: 1, Phase: phase.Implementing, ConfirmedAt: time.Now().Add(-time.Minute)},
		},
		supervise.Claim{Token: current, Project: project, Tree: root, Issue: root, Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateReady, Session: "ses_architect", SessionFile: filepath.Join(cfg.StateDir, "architect.jsonl"),
			PluginContract: api.DaemonAPIVersion, Locator: pluginTestLocator(current)},
	)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	var refuse atomic.Bool
	refuse.Store(true)
	var asked atomic.Int64
	o.checkLaunch = func(context.Context, supervise.Claim) error {
		asked.Add(1)
		if refuse.Load() {
			return fmt.Errorf("launch claim: %w", treelifecycle.ErrCleanupReserved)
		}
		return nil
	}
	logs := &syncBuffer{}

	startDaemonLogging(t, cfg, o, jsonLogger(logs))

	testwait.Eventually(t, "the launch gate asked more than once while refusing "+string(stale), func() bool {
		return asked.Load() > 1
	})
	if c := storedClaim(t, cfg, stale); c.PluginRelaunchFor != 0 || c.State != supervise.StateWorking || c.Generation != 1 {
		t.Fatalf("the stale claim after the refused boot relaunch = %+v, want it exactly as it was: working at generation 1, no relaunch marked", c)
	}
	if p := storedClaim(t, cfg, stale).Pending; p == nil || p.ConfirmedAt.IsZero() || p.Interrupted {
		t.Fatalf("pending after the refused relaunch = %+v, want the confirmed delivery untouched", p)
	}
	if lines := loggedLines(t, logs.String(), "relaunching it onto the current plugin"); len(lines) != 0 {
		t.Fatalf("logs %s carry a relaunching line while the refusal lasted, want none", logs.String())
	}
	if calls := resumesOf(rt, stale); len(calls) != 0 {
		t.Fatalf("resumed %v while the launch path refuses every attempt, want none", calls)
	}

	refuse.Store(false)

	testwait.Eventually(t, "the sweep's retry of "+string(stale), func() bool {
		return len(resumesOf(rt, stale)) == 1
	})
	resume := resumesOf(rt, stale)[0]
	if resume.Spec.ResumeSessionFile != sessionFile || resume.Spec.Generation != 2 {
		t.Fatalf("resumed %+v, want the recorded session at generation 2", resume.Spec)
	}
	if c := storedClaim(t, cfg, stale); c.PluginRelaunchFor != api.DaemonAPIVersion {
		t.Fatalf("PluginRelaunchFor after the sweep's relaunch = %d, want %d", c.PluginRelaunchFor, api.DaemonAPIVersion)
	}
	if p := storedClaim(t, cfg, stale).Pending; p == nil || !p.Interrupted || !p.ConfirmedAt.IsZero() {
		t.Fatalf("pending after the sweep's relaunch = %+v, want the task taken back, unconfirmed and interrupted", p)
	}
	testwait.Eventually(t, "the sweep-triggered relaunch line", func() bool {
		return len(loggedLines(t, logs.String(), `"trigger":"sweep"`)) > 0
	})
	if calls := resumesOf(rt, current); len(calls) != 0 {
		t.Fatalf("resumed the current claim %v, want it untouched across every sweep", calls)
	}
}

// A claim re-adopted at boot already on the daemon's own contract is left alone: nothing is
// relaunched, and no line is logged. Driven through a real daemon boot like the relaunch case above;
// a second seeded claim still on another contract proves the boot pass ran at all once its own
// relaunch lands, so the untouched claim's silence is not just a lucky race against a pass that
// never happened.
func TestRunLeavesAClaimAtTheDaemonsContractAloneAtBoot(t *testing.T) {
	pool := isolatedOutboxPool(t)
	cfg := testConfig(t)
	cfg.PostgresDSN = pool.Config().ConnString()
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.Project + "-1"
	current, err := claim.NewToken(project, root, claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := claim.NewToken(project, root, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	currentSessionFile := filepath.Join(cfg.StateDir, "architect.jsonl")
	seedWorkflowTree(t, cfg.PostgresDSN, project,
		record.Issue{Key: root, Project: cfg.Project, Tree: root, Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", HandedOver: true, Rank: "U"},
		supervise.Claim{Token: current, Project: project, Tree: root, Issue: root, Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateReady, Session: "ses_architect", SessionFile: currentSessionFile,
			PluginContract: api.DaemonAPIVersion, Locator: pluginTestLocator(current)},
		supervise.Claim{Token: stale, Project: project, Tree: root, Issue: root, Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateReady, Session: "ses_implementer", SessionFile: filepath.Join(cfg.StateDir, "implementer.jsonl"),
			PluginContract: 0, Locator: pluginTestLocator(stale)},
	)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	logs := &syncBuffer{}

	d := startDaemonLogging(t, cfg, o, jsonLogger(logs))

	testwait.Eventually(t, "the other claim's stale-plugin relaunch, proving the boot pass ran", func() bool {
		return len(resumesOf(rt, stale)) == 1
	})

	if resumes := resumesOf(rt, current); len(resumes) != 0 {
		t.Fatalf("resumed %v, want the current-contract claim left alone", resumes)
	}
	if calls := rt.CallsOf("Spawn"); len(calls) != 0 {
		t.Fatalf("spawned %v, want nothing relaunched for the current-contract claim", calls)
	}
	if final := d.claim(current); final.Generation != 1 || final.State != string(supervise.StateReady) {
		t.Fatalf("claim after the boot pass = %+v, want untouched", final)
	}
	if budgets := d.claim(current).Budgets; budgets.LaunchFailures != 0 || budgets.Deaths != 0 {
		t.Fatalf("budgets of the untouched claim = %+v, want zero", budgets)
	}
	lines := loggedLines(t, logs.String(), "supervise: the process speaks another plugin contract; relaunching it onto the current plugin")
	for _, line := range lines {
		if line["claim"] == string(current) {
			t.Fatalf("relaunch log line named the current-contract claim: %+v", line)
		}
	}
}

// A claim still launching, its registration not yet arrived, is left to that registration: the
// boot pass never posts it the stale-plugin event, whatever its recorded contract.
func TestBootLeavesALaunchingClaimToItsRegistration(t *testing.T) {
	sup, rt := newPluginSupervisor(t, 24, true)
	project, _ := claim.ProjectToken("legion")
	token, _ := claim.NewToken(project, "LEGION-1", claim.RoleImplementer)
	c := supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching, PluginContract: 0, Locator: pluginTestLocator(token),
	}
	if _, err := sup.restore(context.Background(), []supervise.Claim{c}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	s := &supervision{log: quietLogger(), supervisor: sup, claims: []supervise.Claim{c}, plan: plan{pluginRelaunchable: true}}

	s.refreshStalePlugins(s.claims, "boot")

	m, ok := sup.Machine(token)
	if !ok {
		t.Fatal("the claim is not supervised")
	}
	m.Wait()
	if final := m.Claim(); final.Generation != 1 || final.State != supervise.StateLaunching {
		t.Fatalf("claim after the boot pass = %+v, want untouched", final)
	}
	if calls := rt.Methods(); len(calls) > 0 {
		t.Fatalf("asked the runtime %v, want nothing", calls)
	}
}

// A stale claim found working with a suspension held for its turn (the workflow asked to suspend
// it, and the machine is waiting for the turn to end) is suspended instead of relaunched when the
// boot pass reaches it: nothing is left to hold the suspension for, so it ends there, charged
// nothing, rather than launch a pod only to suspend it on the hold's own retry.
func TestBootSuspendsAStaleClaimWhoseSuspensionIsHeld(t *testing.T) {
	sup, rt := newPluginSupervisor(t, 24, true)
	project, _ := claim.ProjectToken("legion")
	token, _ := claim.NewToken(project, "LEGION-1", claim.RoleImplementer)
	now := time.Now()
	c := supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateWorking, Session: "ses_impl", SessionFile: "/sessions/impl.jsonl",
		PluginContract: 1, Locator: pluginTestLocator(token),
		Pending: &supervise.Delivery{ID: "delivery-1", Task: "implement the plan", QueuedAt: now, DeliveredAt: now, ConfirmedAt: now},
	}
	if _, err := sup.restore(context.Background(), []supervise.Claim{c}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	m, ok := sup.Machine(token)
	if !ok {
		t.Fatal("the claim is not supervised")
	}
	if err := m.Handle(context.Background(), supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByOperator}); !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("suspend mid-turn = %v, want ErrSuspendHeld", err)
	}
	s := &supervision{log: quietLogger(), supervisor: sup, claims: []supervise.Claim{c}, plan: plan{pluginRelaunchable: true}}

	s.refreshStalePlugins(s.claims, "boot")

	testwait.Eventually(t, "the held suspension to end the stale claim", func() bool {
		return m.Claim().State == supervise.StateSuspended
	})
	if final := m.Claim(); final.Budgets != (supervise.Budgets{}) {
		t.Fatalf("budgets after the held suspension ended the claim = %+v, want zero", final.Budgets)
	}
	if calls := rt.CallsOf("Resume"); len(calls) != 0 {
		t.Fatalf("resumed %v, want the held suspension to end it instead", calls)
	}
}

// A Kubernetes daemon that knows no plugin source to relaunch a stale process onto
// (PluginRelaunchable false) posts no stale-plugin event at boot — a stale claim is left running,
// unrelaunched, with one Error line naming the gap — and a stale registration is recorded and
// accepted rather than refused: the degraded mode that keeps the first boot on a release from
// burning every live claim's launch budget before an operator can set runtime.kubernetes.plugins.
// Driven through the supervision option directly (o.pluginRelaunchable): a real Kubernetes runtime
// needs a cluster this pod has none of.
func TestAKubernetesDaemonWithNoPluginSourceRelaunchesNothing(t *testing.T) {
	cfg := testConfig(t)
	no := false
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.pluginRelaunchable = &no
	logs := &syncBuffer{}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claim.NewToken(project, "LEGION-1", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	putClaim(t, cfg, supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateReady, Session: "ses_impl", SessionFile: "/sessions/impl.jsonl",
		PluginContract: 1, Locator: pluginTestLocator(token),
	})

	d := startDaemonLogging(t, cfg, o, jsonLogger(logs))

	lines := loggedLines(t, logs.String(), "supervise: the process speaks another plugin contract, and this daemon knows no plugin versions to relaunch it onto")
	if len(lines) != 1 || lines[0]["pluginContract"] != float64(1) || lines[0]["daemonApiVersion"] != float64(api.DaemonAPIVersion) {
		t.Fatalf("the no-source log lines = %+v, want one naming pluginContract 1 and daemonApiVersion %d", lines, api.DaemonAPIVersion)
	}
	if c := d.claim(token); c.Generation != 1 || c.State != string(supervise.StateReady) || c.Budgets.LaunchFailures != 0 {
		t.Fatalf("the stale claim = %+v, want untouched and uncharged", c)
	}

	fresh := d.spawn(architect())
	launch := lastLaunch(t, rt, fresh)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_fresh", OmpSessionFile: "/sessions/fresh.jsonl", AgentID: "agent", PluginContract: 1,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("a stale registration with no plugin source = %d %s, want 200 (accepted)", status, body)
	}
	if c := d.claim(fresh); c.PluginContract != 1 || c.Budgets.LaunchFailures != 0 {
		t.Fatalf("the freshly registered stale claim = %+v, want pluginContract 1 recorded and uncharged", c)
	}
}

// jsonLogger writes JSON lines to buf for a test to grep.
func jsonLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

// A process that registers with a plugin contract the daemon does not speak is relaunched at once
// onto the current one — uncharged, the claim's first relaunch for this daemon's contract — and
// the registration itself is refused naming both contracts and the relaunch under way.
func TestAStaleRegistrationIsRelaunchedAtOnceChargingNothing(t *testing.T) {
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	token := d.spawn(architect())
	launch := lastLaunch(t, rt, token)

	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_architect", OmpSessionFile: "/sessions/architect.jsonl",
		AgentID: "agent", PluginContract: 1,
	}, false)

	wantSentence := fmt.Sprintf("pi-legion speaks daemon API contract 1; this daemon requires %d: the daemon is relaunching this process onto the current plugin", api.DaemonAPIVersion)
	if status != http.StatusConflict || !strings.Contains(string(body), wantSentence) {
		t.Fatalf("stale register = %d %s, want 409 naming %q", status, body, wantSentence)
	}
	testwait.Eventually(t, "the uncharged relaunch", func() bool {
		c := d.claim(token)
		return c.Generation == 2 && c.State == string(supervise.StateLaunching)
	})
	if c := d.claim(token); c.Budgets.LaunchFailures != 0 {
		t.Fatalf("the first stale registration was charged: %+v", c.Budgets)
	}
	if c := d.claim(token); c.PluginContract != 1 {
		t.Fatalf("recorded pluginContract = %d, want 1", c.PluginContract)
	}
}

// The daemon-launched controller's registration is covered the same way as any other claim's.
func TestAStaleControllerRegistrationIsRelaunchedAtOnce(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	rt := fake.NewRuntime()
	var record built
	d := startDaemon(t, cfg, fakeRuntime(rt, &record))
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(project)
	launch := controllerLaunched(t, rt, token)

	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_controller", OmpSessionFile: "/sessions/controller.jsonl",
		AgentID: "ses_controller", PluginContract: 1,
	}, false)

	wantSentence := fmt.Sprintf("pi-legion speaks daemon API contract 1; this daemon requires %d: the daemon is relaunching this process onto the current plugin", api.DaemonAPIVersion)
	if status != http.StatusConflict || !strings.Contains(string(body), wantSentence) {
		t.Fatalf("the controller's stale register = %d %s, want 409 naming %q", status, body, wantSentence)
	}
	testwait.Eventually(t, "the controller's uncharged relaunch", func() bool {
		c := d.claim(token)
		return c.Generation == 2 && c.State == string(supervise.StateLaunching)
	})
	if c := d.claim(token); c.Budgets.LaunchFailures != 0 {
		t.Fatalf("the controller's first stale registration was charged: %+v", c.Budgets)
	}
}

// A relaunched process that registers still stale — its last relaunch was already made for this
// daemon's contract, and it still did not land on it — is charged as any launch failure is, and
// the claim fails once its launch budget runs out, ending in the worker-died notice the architect
// hears of any failed claim's death. Driven directly over the supervisor and the workflow's own
// terminal hook (outbox_test.go's newOutboxSupervisor and
// TestAFinishedWorkerThatDiesIsRelaunchedAndItsFailureHoldsNothing's own rig), rather than through
// the HTTP route the other registration tests use: the notice is the workflow's own reaction, which
// needs its real store and handlers, not the fake-runtime daemon's.
func TestARelaunchedProcessStillStaleIsChargedUntilWorkerDied(t *testing.T) {
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
	sup.deps.PluginContract = 24
	sup.deps.PluginRelaunchable = true
	sup.deps.Limits.LaunchFailures = 2
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

	register := func() error {
		return machine.Handle(ctx, supervise.RequestRegister{
			Claim: planner, Generation: machine.Claim().Generation, Session: "ses-plan", SessionFile: "/tmp/plan.jsonl",
			CapabilityHash: []byte("planner-capability"), PluginContract: 1,
		})
	}
	wantStale := func(err error) {
		t.Helper()
		var refusal claim.Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("register = %v, want the stale-plugin refusal", err)
		}
	}

	wantStale(register())
	if got := machine.Claim().State; got != supervise.StateLaunching {
		t.Fatalf("after the first stale registration the planner is %s, want launching", got)
	}
	wantStale(register())
	if got := machine.Claim().Budgets.LaunchFailures; got != 1 {
		t.Fatalf("launchFailures after the second stale registration = %d, want 1", got)
	}
	wantStale(register())
	if got := machine.Claim().State; got != supervise.StateFailed {
		t.Fatalf("after its relaunches ran out the planner is %s, want failed", got)
	}
	if calls := rt.CallsOf("Resume"); len(calls) != 2 {
		t.Fatalf("Resume calls = %d, want 2: the first stale registration's uncharged relaunch and the second's charged one; the third's charge ends the claim in failed instead", len(calls))
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
	if len(notices) != 1 || !reflect.DeepEqual(notices[0], record.OutboxPayload(died)) {
		t.Fatalf("notices = %+v, want the one worker-died %+v", notices, died)
	}
}

// prepare() resolves plan.plugins, plan.pluginsSource and plan.pluginRelaunchable from the
// configuration alone: the file's runtime.kubernetes.plugins when set ("legion.yaml"), nil and ""
// when it is not (the daemon knows no pair, and pluginRelaunchable is false under kubernetes);
// tmux is always relaunchable and resolves no pair at all, since the tmux runtime never installs
// one.
func TestPrepareResolvesThePluginPairFromTheConfiguration(t *testing.T) {
	t.Run("kubernetes, the file names a pair", func(t *testing.T) {
		cfg := kubernetesConfig(t, "https://127.0.0.1:1")
		cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
		if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.NatsNkeySeedFile = testnats.SeedFile(t, testnats.UserSeed(t))
		cfg.Runtime.Kubernetes.Plugins = &config.PluginsConfig{PiLegion: "9.7.0", PiEnvoy: "9.6.1"}

		p, err := prepare(cfg, quietLogger(), overrides{environ: []string{}})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		want := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: api.DaemonAPIVersion}
		if p.plugins == nil || *p.plugins != *want || p.pluginsSource != "legion.yaml" || !p.pluginRelaunchable {
			t.Fatalf("plugins = %+v, source %q, relaunchable %t; want %+v, \"legion.yaml\", true", p.plugins, p.pluginsSource, p.pluginRelaunchable, want)
		}
	})
	t.Run("kubernetes, the file names none", func(t *testing.T) {
		cfg := kubernetesConfig(t, "https://127.0.0.1:1")
		cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
		if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.NatsNkeySeedFile = testnats.SeedFile(t, testnats.UserSeed(t))

		p, err := prepare(cfg, quietLogger(), overrides{environ: []string{}})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if p.plugins != nil || p.pluginsSource != "" || p.pluginRelaunchable {
			t.Fatalf("plugins = %+v, source %q, relaunchable %t; want nil, \"\", false", p.plugins, p.pluginsSource, p.pluginRelaunchable)
		}
	})
}

// ResolvedPlugins is nil, "" without the kubernetes block or without a pair, the file's pair with
// "legion.yaml", and, once main fills Plugins from the binary's linked pair (PluginsFromBinary),
// the same pair with "binary".
func TestResolvedPlugins(t *testing.T) {
	k9 := &config.PluginsConfig{PiLegion: "9.7.0", PiEnvoy: "9.6.1"}
	for _, tc := range []struct {
		name       string
		kubernetes *config.Kubernetes
		wantPair   *runtime.PluginInstall
		wantSource string
	}{
		{"tmux (no kubernetes block)", nil, nil, ""},
		{"kubernetes, no plugins", &config.Kubernetes{}, nil, ""},
		{"kubernetes, the file's pair", &config.Kubernetes{Plugins: k9}, &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: api.DaemonAPIVersion}, "legion.yaml"},
		{"kubernetes, the binary's pair", &config.Kubernetes{Plugins: k9, PluginsFromBinary: true}, &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: api.DaemonAPIVersion}, "binary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Runtime: config.Runtime{Kubernetes: tc.kubernetes}}
			pair, source := ResolvedPlugins(cfg)
			if (pair == nil) != (tc.wantPair == nil) || (pair != nil && *pair != *tc.wantPair) || source != tc.wantSource {
				t.Fatalf("ResolvedPlugins = %+v, %q; want %+v, %q", pair, source, tc.wantPair, tc.wantSource)
			}
		})
	}
}

// logPlugins is the boot's one line: Info naming the pair and its source with a pair, Warn naming
// the gap without one.
func TestLogPlugins(t *testing.T) {
	var buf syncBuffer
	logPlugins(jsonLogger(&buf), &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: api.DaemonAPIVersion}, "legion.yaml")
	if got := buf.String(); !strings.Contains(got, `"msg":"daemon: stale-plugin relaunches install"`) || !strings.Contains(got, `"piLegion":"9.7.0"`) ||
		!strings.Contains(got, `"piEnvoy":"9.6.1"`) || !strings.Contains(got, `"source":"legion.yaml"`) {
		t.Fatalf("logPlugins with a pair wrote %q", got)
	}
	buf.Reset()
	logPlugins(jsonLogger(&buf), nil, "")
	if got := buf.String(); !strings.Contains(got, `"level":"WARN"`) || !strings.Contains(got, "this daemon knows no plugin versions to relaunch a stale process onto; set runtime.kubernetes.plugins") {
		t.Fatalf("logPlugins with no pair wrote %q", got)
	}
}
