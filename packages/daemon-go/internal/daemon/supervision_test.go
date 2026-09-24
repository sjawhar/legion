package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	recordpkg "github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func architect() api.SpawnRequest {
	return api.SpawnRequest{Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}
}

// Everything the daemon refuses on is refused before it touches the store, naming the key the
// operator has to change: nothing is recorded for a daemon that could never have served.
func TestRunRefusesAConfigurationItCannotSuperviseUnder(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*config.Config, *overrides)
		want   string
	}{
		{"no operator token file", func(c *config.Config, _ *overrides) { c.OperatorTokenFile = "" }, "operator_token_file is required"},
		{"an operator token file that is not there", func(c *config.Config, _ *overrides) {
			c.OperatorTokenFile = filepath.Join(c.StateDir, "absent")
		}, "operator_token_file names"},
		{"an Envoy token file that is not there", func(c *config.Config, _ *overrides) {
			c.EnvoyTokenFile = filepath.Join(c.StateDir, "absent")
		}, "envoy_token_file names"},
		{"a runtime Stage 2 does not supervise under", func(c *config.Config, _ *overrides) {
			c.Runtime = config.Runtime{Name: "kubernetes"}
		}, "runtime kubernetes"},
		{"no omp_invocation and no LEGION_OMP_PATH", func(c *config.Config, o *overrides) {
			c.OmpInvocation = ""
			o.runtime = nil
			o.getenv = func(string) string { return "" }
		}, "omp_invocation is not set"},
		{"a provider key that cannot be resolved", func(c *config.Config, o *overrides) {
			c.ProviderKeys = []config.ProviderKey{{Env: "ABSENT_ENV", Secret: "ABSENT_KEY"}}
			o.environ = []string{"PATH=" + t.TempDir()}
		}, "provider_keys.ABSENT_ENV: the secrets command is not on PATH, so ABSENT_KEY cannot be read"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := testConfig(t)
			o := fakeRuntime(fake.NewRuntime(), &built{})
			testCase.change(&cfg, &o)

			err := run(context.Background(), cfg, quietLogger(), o)

			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("run = %v, want a refusal naming %q", err, testCase.want)
			}
			if count, _ := boots(t, cfg); count != 0 {
				t.Fatalf("boots after a refused start = %d, want 0", count)
			}
		})
	}
}

// The worker stream is a unix socket in the state directory, and the runtime is built over it:
// the listener is the runtime's connection directory, and its address is what every pane's shim
// is told to dial (`--connect`).
func TestRunBuildsTheRuntimeOverTheWorkerStream(t *testing.T) {
	cfg := testConfig(t)
	var record built
	startDaemon(t, cfg, fakeRuntime(fake.NewRuntime(), &record))

	record.mu.Lock()
	defer record.mu.Unlock()
	if want := "unix://" + filepath.Join(cfg.StateDir, "worker-stream.sock"); record.address != want {
		t.Errorf("the runtime was told to have shims dial %q, want %q", record.address, want)
	}
	if _, ok := record.conns.(*stream.Listener); !ok {
		t.Errorf("the runtime's connection directory is %T, want the worker stream listener", record.conns)
	}
}

// Every launch the daemon asks for carries what a pane needs and the claim does not: the role
// prompt the operator gave, the claim's addressing, the operator's deployment instructions as
// boot wrote them, the Envoy bearer as a secret, and a workspace that exists.
func TestRunLaunchesWithThePromptInstructionsAndSecretsItWasGiven(t *testing.T) {
	cfg := testConfig(t)
	cfg.InstructionsPath = filepath.Join(t.TempDir(), "instructions.md")
	if err := os.WriteFile(cfg.InstructionsPath, []byte("Never push to main.\n"), 0o600); err != nil {
		t.Fatalf("write the instructions: %v", err)
	}
	cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
	if err := os.WriteFile(cfg.EnvoyTokenFile, []byte("envoy-bearer\n"), 0o600); err != nil {
		t.Fatalf("write the Envoy token: %v", err)
	}
	rt := fake.NewRuntime()
	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))

	materialized := filepath.Join(cfg.StateDir, "deployment-instructions.md")
	written, err := os.ReadFile(materialized)
	if err != nil {
		t.Fatalf("boot wrote no deployment instructions: %v", err)
	}
	if want := "# Deployment instructions (" + cfg.Project + ")\n\nNever push to main.\n"; string(written) != want {
		t.Fatalf("deployment instructions = %q, want %q", written, want)
	}

	token := d.spawn(architect())
	spec := lastLaunch(t, rt, token)

	if spec.Prompt.DeploymentInstructionsPath != materialized {
		t.Errorf("the launch reads instructions from %q, want %q", spec.Prompt.DeploymentInstructionsPath, materialized)
	}
	if len(spec.Prompt.RolePromptPaths) != 1 {
		t.Fatalf("role prompts = %v, want one", spec.Prompt.RolePromptPaths)
	}
	if prompt, err := os.ReadFile(spec.Prompt.RolePromptPaths[0]); err != nil || string(prompt) != "Reply ready and wait." {
		t.Errorf("the role prompt file holds %q (%v), want the operator's prompt", prompt, err)
	}
	if !strings.HasPrefix(spec.Prompt.RolePromptPaths[0], cfg.StateDir+string(filepath.Separator)) {
		t.Errorf("the role prompt %s is outside the state directory", spec.Prompt.RolePromptPaths[0])
	}
	project, _ := claim.ProjectToken(cfg.Project)
	if want := "`notifications.role." + string(token) + "`"; !strings.Contains(spec.Prompt.Addressing, want) {
		t.Errorf("the addressing %q does not name the claim's role topic %s", spec.Prompt.Addressing, want)
	}
	if want := "`notifications.role.legion-" + project + "-controller`"; !strings.Contains(spec.Prompt.Addressing, want) {
		t.Errorf("the addressing %q does not name the controller %s", spec.Prompt.Addressing, want)
	}
	if !reflect.DeepEqual(spec.Secrets, map[string]string{"ENVOY_TOKEN": "envoy-bearer"}) {
		t.Errorf("the launch's secrets = %v, want the Envoy bearer", spec.Secrets)
	}
	if info, err := os.Stat(spec.Workspace); err != nil || !info.IsDir() || !filepath.IsAbs(spec.Workspace) {
		t.Errorf("the launch's workspace %q is not an absolute directory (%v)", spec.Workspace, err)
	}
	if spec.Project != project || spec.Tree != "LEGION-1" || spec.Issue != "LEGION-1" || spec.Role != claim.RoleArchitect {
		t.Errorf("the launch is for %s/%s/%s/%s, want the spawned claim", spec.Project, spec.Tree, spec.Issue, spec.Role)
	}
}

// A default operator claim has no arbitrary prompt file. It must receive the shared role prompt
// followed by the Go daemon part, so tmux can concatenate both before addressing and deployment
// instructions into its single OMP flag.
func TestRunLaunchesTheComposedGoRolePromptWhenSpawnHasNoPromptFile(t *testing.T) {
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))

	token := d.spawn(api.SpawnRequest{Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect})
	spec := lastLaunch(t, rt, token)
	rolesDir, err := prompts.ResolveRolePromptsDir(nil)
	if err != nil {
		t.Fatalf("ResolveRolePromptsDir: %v", err)
	}
	want := []string{
		filepath.Join(rolesDir, "architect-root.md"),
		filepath.Join(cfg.StateDir, "prompts", "go", "architect-root.md"),
	}
	if !reflect.DeepEqual(spec.Prompt.RolePromptPaths, want) {
		t.Fatalf("RolePromptPaths = %q, want %q", spec.Prompt.RolePromptPaths, want)
	}
}

// One claim from its spawn to a relaunch, through every source that feeds its machine: the
// operator's routes, the agent's routes, the worker stream's events (the hello, the turn, the
// late refusal), and the runtime's sweep.
func TestRunFeedsTheStreamAndTheSweepIntoTheClaimsMachine(t *testing.T) {
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	var record built
	d := startDaemon(t, cfg, fakeRuntime(rt, &record))
	spawn := architect()
	spawn.Task = "Say hello."
	token := d.spawn(spawn)
	launch := lastLaunch(t, rt, token)

	// State is now a durable issue record joined to live claims; the claim alone does not create an
	// issue in the projection. The workflow will write this pair in one transaction at admission.
	stateStore, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	t.Cleanup(stateStore.Close)
	records := recordpkg.NewStore()
	if err := stateStore.Tx(context.Background(), func(tx pgx.Tx) error {
		issue := recordpkg.Issue{
			Key: spawn.Issue, Tree: spawn.Issue, Project: cfg.Project, Title: "Stream lifecycle", Phase: phase.Admitted,
			Generation: 1, Status: "in_progress",
		}
		if err := records.PutIssue(context.Background(), tx, issue); err != nil {
			return err
		}
		return records.PutPhase(context.Background(), tx, recordpkg.PhaseRow{
			Issue: issue.Key, Role: claim.RoleArchitect, Claim: token,
		})
	}); err != nil {
		t.Fatalf("write state record: %v", err)
	}

	sh := dialShim(t, record.address, launch.BootToken)
	eventually(t, "the hello to reach the machine", func() bool {
		return d.claim(token).State == string(supervise.StateShimConnected)
	})

	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_architect", OmpSessionFile: "/sessions/architect.jsonl",
		AgentID: "agent", PluginContract: 1,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register = %d; body %s", status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration: %v", err)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret, Generation: 1,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", status, body)
	}

	// The task queued before ready goes once the agent is ready, over the claim's connection.
	prompt := sh.prompt()
	if prompt.Message != "Say hello." || prompt.DeliveryID == "" {
		t.Fatalf("the daemon prompted %+v, want the queued task with its delivery id", prompt)
	}
	sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true})
	sh.send(shimwire.AgentStart{})
	eventually(t, "the turn to start", func() bool { return d.claim(token).State == string(supervise.StateWorking) })
	sh.send(shimwire.AgentEnd{})
	eventually(t, "the turn to end", func() bool {
		c := d.claim(token)
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})

	architectView := d.state().Issues["LEGION-1"].Architect
	if architectView == nil || architectView.State != "idle" || architectView.Session != "ses_architect" ||
		architectView.Locator == nil || architectView.Locator.Tmux == nil || architectView.Locator.Claim != token {
		t.Fatalf("the state shows the architect as %+v, want the idle claim with its nested locator", architectView)
	}

	// OMP taking back a prompt it acknowledged is a stream event of its own, and it costs the
	// claim a prompt failure.
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: "Say it again."}, true); status != http.StatusOK {
		t.Fatalf("deliver = %d; body %s", status, body)
	}
	second := sh.prompt()
	sh.send(shimwire.Response{ID: second.ID, Command: shimwire.TypePrompt, Success: true})
	eventually(t, "the acknowledgement to be recorded", func() bool {
		c := d.claim(token)
		return c.Pending != nil && c.Pending.DeliveredAt != nil
	})
	sh.send(shimwire.Response{ID: second.ID, Command: shimwire.TypePrompt, Success: false, Error: "Agent is busy"})
	eventually(t, "the late refusal to be charged", func() bool { return d.claim(token).Budgets.PromptFailures == 1 })

	// The sweep finding the process gone relaunches the recorded session.
	loc := d.claim(token).Locator
	rt.Emit(runtime.Observation{Locator: *loc, Kind: runtime.Gone, Detail: "pane gone"})
	eventually(t, "the relaunch", func() bool {
		c := d.claim(token)
		return c.Generation == 2 && c.State == string(supervise.StateLaunching) && len(rt.CallsOf("Resume")) == 1
	})
	if resumed := rt.CallsOf("Resume")[0]; resumed.Spec.ResumeSessionFile != "/sessions/architect.jsonl" ||
		!reflect.DeepEqual(resumed.Locator, *loc) {
		t.Errorf("resumed %+v, want the recorded session after the dead incarnation", resumed)
	}
}

// A restart re-adopts every claim with a live locator: the runtime is told of it, nothing is
// launched again, the claim keeps its state and incarnation, and the shim's reconnect hello — with
// the boot token it has held since its launch — is accepted.
func TestRunReadoptsEveryClaimWithALiveLocatorOnRestart(t *testing.T) {
	cfg := testConfig(t)
	first := fake.NewRuntime()
	var record built
	d := startDaemon(t, cfg, fakeRuntime(first, &record))
	token := d.spawn(architect())
	launch := lastLaunch(t, first, token)
	sh := dialShim(t, record.address, launch.BootToken)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_architect", OmpSessionFile: "/sessions/architect.jsonl",
		AgentID: "agent", PluginContract: 1,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register = %d; body %s", status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration: %v", err)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_architect", Secret: registered.Secret, Generation: 1,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", status, body)
	}
	before := d.claim(token)
	d.stop()
	sh.closed()

	second := fake.NewRuntime()
	var again built
	restarted := startDaemon(t, cfg, fakeRuntime(second, &again))

	reconciled := second.CallsOf("ReconcileOrphans")
	if len(reconciled) == 0 || reconciled[0].Grace != 0 ||
		!reflect.DeepEqual(reconciled[0].Known, []runtime.Known{{Claim: token, Locator: before.Locator}}) {
		t.Fatalf("the restart reconciled %+v, want the live claim known with its locator, at grace 0", reconciled)
	}
	for _, method := range []string{"Spawn", "Resume", "Release", "Suspend"} {
		if calls := second.CallsOf(method); len(calls) != 0 {
			t.Fatalf("the restart called %s %d times; a live claim is re-observed, not relaunched", method, len(calls))
		}
	}
	after := restarted.claim(token)
	if after.State != string(supervise.StateReady) || after.Generation != 1 || !reflect.DeepEqual(after.Locator, before.Locator) {
		t.Fatalf("after the restart the claim is %+v, want it as it was: %+v", after, before)
	}
	if boots := restarted.state().Daemon.Boots; boots != 2 {
		t.Fatalf("boots = %d, want 2", boots)
	}

	dialShim(t, again.address, launch.BootToken)
	second.Emit(runtime.Observation{Locator: *before.Locator, Kind: runtime.Alive})
	time.Sleep(100 * time.Millisecond)
	if c := restarted.claim(token); c.State != string(supervise.StateReady) || c.Generation != 1 {
		t.Fatalf("after the reconnect and a live sweep the claim is %s at generation %d, want ready at 1", c.State, c.Generation)
	}
}

// A launch the previous daemon persisted and never finished — its locator not yet recorded when
// it died — has no process to re-adopt; boot launches it again rather than leave a claim that
// waits on a pane nothing knows.
func TestRunLaunchesAgainALaunchThePreviousDaemonDidNotFinish(t *testing.T) {
	cfg := testConfig(t)
	project, _ := claim.ProjectToken(cfg.Project)
	token, _ := claim.NewToken(project, "LEGION-3", claim.RolePlanner)
	putClaim(t, cfg, supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-3", Role: claim.RolePlanner,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	})
	writePrompt(t, cfg, token)
	rt := fake.NewRuntime()

	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))

	eventually(t, "the unfinished launch to be launched again", func() bool {
		c := d.claim(token)
		return c.State == string(supervise.StateLaunching) && c.Locator != nil
	})
	spawns := rt.CallsOf("Spawn")
	if len(spawns) != 1 || spawns[0].Spec.Generation != 2 {
		t.Fatalf("spawns = %+v, want one launch at generation 2", spawns)
	}
}

// An unrecorded launching claim may still have a pane the previous daemon opened before it
// persisted its locator. A failed orphan listing leaves that process unknown, not gone, so boot
// must not open another pane. Once reconciliation succeeds, the queued launch can proceed.
func TestRunWaitsToRelaunchAnUnfinishedClaimUntilOrphanReconciliationSucceeds(t *testing.T) {
	cfg := testConfig(t)
	project, _ := claim.ProjectToken(cfg.Project)
	token, _ := claim.NewToken(project, "LEGION-4", claim.RoleReviewer)
	bootToken := "interrupted-" + randomSuffix(t)
	oldSecret := "old-secret"
	oldCapability := sha256.Sum256([]byte(oldSecret))
	putClaim(t, cfg, supervise.Claim{
		Token: token, Project: project, Tree: "LEGION-1", Issue: "LEGION-4", Role: claim.RoleReviewer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken(bootToken), CapabilityHash: oldCapability[:],
	})
	writePrompt(t, cfg, token)
	rt := fake.NewRuntime()
	rt.FailReconcileOrphans(errors.New("tmux list-panes timed out"))
	var record built
	o := fakeRuntime(rt, &record)
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)

	eventually(t, "the failed boot reconciliation", func() bool { return len(rt.CallsOf("ReconcileOrphans")) >= 1 })
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 0 {
		t.Fatalf("spawns after a failed boot reconciliation = %+v, want none while the old pane is unknown", spawns)
	}
	if c := d.claim(token); c.State != "launch_uncertain" || c.Locator != nil {
		t.Fatalf("claim after a failed boot reconciliation = %+v, want persisted launch_uncertain with no locator", c)
	}
	oldShim := dialShim(t, record.address, bootToken)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: "old-session", OmpSessionFile: "/sessions/old.jsonl", AgentID: "old-agent", PluginContract: 1,
	}, false)
	if status != http.StatusConflict || !strings.Contains(string(body), "previous launch") {
		t.Fatalf("old shim register while the pane is uncertain = %d %s, want a named 409", status, body)
	}
	status, body = d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "old-session", Secret: oldSecret, Generation: 1,
	}, false)
	if status != http.StatusConflict || !strings.Contains(string(body), "previous launch") {
		t.Fatalf("old shim ready while the pane is uncertain = %d %s, want a named 409", status, body)
	}
	if c := d.claim(token); c.State != "launch_uncertain" || c.Locator != nil {
		t.Fatalf("old shim hello, register, and ready produced %+v, want no live state without a locator", c)
	}
	_ = oldShim.conn.Close()
	eventually(t, "the old shim's disconnect to leave the claim locator-less", func() bool {
		c := d.claim(token)
		return c.State == "launch_uncertain" && c.Locator == nil
	})
	status, body = d.request(http.MethodPost, "/legion/v1/operator/claims", api.SpawnRequest{
		Tree: "LEGION-1", Issue: "LEGION-4", Role: claim.RoleReviewer, Prompt: "Retry the launch.",
	}, true)
	if status != http.StatusConflict || !strings.Contains(string(body), "previous launch") {
		t.Fatalf("operator spawn while the old pane is uncertain = %d %s, want a named 409", status, body)
	}
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 0 {
		t.Fatalf("operator spawn while the old pane is uncertain opened %+v, want none", spawns)
	}

	d.stop()
	second := fake.NewRuntime()
	second.FailReconcileOrphans(errors.New("tmux list-panes timed out again"))
	secondOverrides := fakeRuntime(second, &built{})
	secondOverrides.orphanSweep = 20 * time.Millisecond
	restarted := startDaemon(t, cfg, secondOverrides)
	eventually(t, "the second boot's failed reconciliation", func() bool { return len(second.CallsOf("ReconcileOrphans")) >= 1 })
	if c := restarted.claim(token); c.State != "launch_uncertain" || c.Locator != nil {
		t.Fatalf("claim after a second failed boot reconciliation = %+v, want persisted launch_uncertain with no locator", c)
	}
	status, body = restarted.request(http.MethodPost, "/legion/v1/operator/claims", api.SpawnRequest{
		Tree: "LEGION-1", Issue: "LEGION-4", Role: claim.RoleReviewer, Prompt: "Retry the launch.",
	}, true)
	if status != http.StatusConflict || !strings.Contains(string(body), "previous launch") {
		t.Fatalf("operator spawn after a second failed reconciliation = %d %s, want a named 409", status, body)
	}
	if spawns := second.CallsOf("Spawn"); len(spawns) != 0 {
		t.Fatalf("operator spawn after a second failed reconciliation opened %+v, want none", spawns)
	}

	restarted.stop()
	third := fake.NewRuntime()
	thirdOverrides := fakeRuntime(third, &built{})
	thirdOverrides.orphanSweep = 20 * time.Millisecond
	final := startDaemon(t, cfg, thirdOverrides)
	eventually(t, "the third boot to launch after reconciliation succeeds", func() bool { return len(third.CallsOf("Spawn")) == 1 })
	if spawned := third.CallsOf("Spawn")[0]; spawned.Spec.Generation != 2 {
		t.Fatalf("the third boot spawned generation %d, want 2", spawned.Spec.Generation)
	}
	if c := final.claim(token); c.State != string(supervise.StateLaunching) || c.Locator == nil {
		t.Fatalf("claim after successful reconciliation = %+v, want the one launching incarnation", c)
	}
}

// A boot reconciliation retried after boot reads the claims as they are at the retry, never as
// the boot read them. Under the sandbox runtime the sweep deletes every Sandbox no known claim
// owns, with no grace at boot, so a retry told the boot's snapshot would delete the Sandbox — and
// with a root's, the tree volume — of a claim launched and suspended since.
func TestRunRetriesTheBootReconciliationWithTheClaimsAsTheyAreNow(t *testing.T) {
	cfg := testConfig(t)
	project, _ := claim.ProjectToken(cfg.Project)
	unfinished, _ := claim.NewToken(project, "LEGION-4", claim.RoleReviewer)
	putClaim(t, cfg, supervise.Claim{
		Token: unfinished, Project: project, Tree: "LEGION-1", Issue: "LEGION-4", Role: claim.RoleReviewer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	})
	writePrompt(t, cfg, unfinished)
	rt := fake.NewRuntime()
	rt.FailReconcileOrphans(errors.New("sandboxes list timed out"))
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)
	eventually(t, "the failed boot reconciliation", func() bool { return len(rt.CallsOf("ReconcileOrphans")) >= 1 })

	worker := architect()
	worker.Issue, worker.Role = "LEGION-2", claim.RoleImplementer
	suspended := d.spawn(worker)
	readyClaim(t, d, rt, suspended)
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(suspended)+"/suspend", nil, true); status != http.StatusOK {
		t.Fatalf("suspend = %d; body %s", status, body)
	}
	if c := d.claim(suspended); c.State != string(supervise.StateSuspended) || c.Locator != nil {
		t.Fatalf("the claim launched after boot is %s with locator %+v, want suspended with none", c.State, c.Locator)
	}
	cleared := len(rt.CallsOf("ReconcileOrphans"))
	rt.FailReconcileOrphans(nil)

	eventually(t, "the unfinished launch to be released by a successful retry", func() bool { return len(rt.CallsOf("Spawn")) == 2 })
	var retry *fake.Call
	for _, call := range rt.CallsOf("ReconcileOrphans")[cleared:] {
		if call.Grace == 0 {
			retry = &call
			break
		}
	}
	if retry == nil {
		t.Fatal("no boot reconciliation ran after the failure cleared")
	}
	known := map[claim.Token]*runtime.Locator{}
	for _, entry := range retry.Known {
		known[entry.Claim] = entry.Locator
	}
	if locator, ok := known[suspended]; !ok || locator != nil {
		t.Fatalf("the retried boot reconciliation knows %v; want the claim suspended since boot, with no locator", retry.Known)
	}
}

// A pane's secret files live exactly as long as its process: boot removes every file no live
// locator names, and a claim whose process ends — here, stopped — loses its own.
func TestRunPrunesTheSecretFilesOfClaimsWithNoProcess(t *testing.T) {
	cfg := testConfig(t)
	project, _ := claim.ProjectToken(cfg.Project)
	live, _ := claim.NewToken(project, "LEGION-1", claim.RoleArchitect)
	suspended, _ := claim.NewToken(project, "LEGION-2", claim.RoleArchitect)
	putClaim(t, cfg, supervise.Claim{
		Token: live, Project: project, Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect, Generation: 1,
		State: supervise.StateReady, Session: "ses_live", SessionFile: "/sessions/live.jsonl",
		Locator: &runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: live, Incarnation: "4242:1", Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
		},
	})
	putClaim(t, cfg, supervise.Claim{
		Token: suspended, Project: project, Tree: "LEGION-2", Issue: "LEGION-2", Role: claim.RoleArchitect, Generation: 1,
		State: supervise.StateSuspended, Session: "ses_suspended", SessionFile: "/sessions/suspended.jsonl",
	})
	secrets := filepath.Join(cfg.StateDir, "secrets")
	for _, name := range []string{string(live), string(live) + "-envoy_token", string(suspended), string(suspended) + "-envoy_token", "left-by-a-crash"} {
		writeFile(t, filepath.Join(secrets, name))
	}
	rt := fake.NewRuntime()

	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))

	if got := secretFiles(t, secrets); !slices.Equal(got, []string{string(live), string(live) + "-envoy_token"}) {
		t.Fatalf("after boot the secrets are %v, want only the live claim's", got)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(live)+"/stop", nil, true); status != http.StatusOK {
		t.Fatalf("stop = %d; body %s", status, body)
	}
	if got := secretFiles(t, secrets); len(got) != 0 {
		t.Fatalf("after the stop the secrets are %v, want none", got)
	}
}

// Provider keys are resolved at boot, before anything can launch, into the daemon-held directory
// every pane's shim is pointed at — and boot's pruning of the pane secret files, which runs after,
// leaves that directory alone: it is the daemon's, not any claim's.
func TestRunResolvesProviderKeysAtBootAndKeepsThemThroughThePrune(t *testing.T) {
	cfg := testConfig(t)
	cfg.ProviderKeys = []config.ProviderKey{{Env: "AGENT_ENV", Secret: "AGENT_KEY"}}
	bin := t.TempDir()
	stub := "#!/bin/sh\ncase \"$2:$3\" in\n" +
		"  AGENT_KEY:--no-request) echo '{\"key\":\"AGENT_KEY\",\"tier\":\"agent\"}' ;;\n" +
		"  AGENT_KEY:--value) echo 'agent-value' ;;\n" +
		"  *) exit 9 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "secrets"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	o := fakeRuntime(fake.NewRuntime(), &built{})
	o.environ = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	writeFile(t, filepath.Join(cfg.StateDir, "secrets", "left-by-a-crash"))

	startDaemon(t, cfg, o)

	key := filepath.Join(cfg.StateDir, "secrets", "provider-env", "AGENT_ENV")
	if got, err := os.ReadFile(key); err != nil || string(got) != "agent-value" {
		t.Fatalf("the provider key after boot: %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "secrets", "left-by-a-crash")); !os.IsNotExist(err) {
		t.Errorf("boot's prune left a file no claim names (stat: %v)", err)
	}
}

// The shipped daemon writes the one Dispatch bearer panes share before any pane can launch, and
// boot's claim-secret prune leaves it alone because it belongs to the daemon rather than a claim.
func TestPrepareWritesTheDispatchTokenFileAndBootPruneKeepsIt(t *testing.T) {
	cfg := testConfig(t)
	cfg.DispatchURL = "http://127.0.0.1:18766"
	cfg.DispatchTokenFile = filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(" dispatch-test-token \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := prepare(cfg, quietLogger(), fakeRuntime(fake.NewRuntime(), &built{}))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want := filepath.Join(cfg.StateDir, "secrets", "dispatch-token")
	if p.dispatchTokenFile != want {
		t.Fatalf("Dispatch token file = %q, want %q", p.dispatchTokenFile, want)
	}
	if info, err := os.Stat(want); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("Dispatch token file stat = %v, %v; want mode 0600", info, err)
	}
	if got, err := os.ReadFile(want); err != nil || string(got) != "dispatch-test-token" {
		t.Fatalf("Dispatch token file = %q (%v), want the trimmed configured token", got, err)
	}
	pruneAllBut(filepath.Join(cfg.StateDir, "secrets"), nil, quietLogger())
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("boot's prune removed the daemon's Dispatch token file: %v", err)
	}
}

// The daemon keeps reconciling orphans while it runs, with a grace, so a pane opened by hand on
// the private server is reaped once it has idled past it; the processes the claims record are
// always known.
func TestRunReconcilesOrphansWhileItRuns(t *testing.T) {
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)
	token := d.spawn(architect())
	loc := d.claim(token).Locator

	eventually(t, "a reconciliation that knows the claim's process", func() bool {
		for _, call := range rt.CallsOf("ReconcileOrphans") {
			if call.Grace == orphanGrace && slices.ContainsFunc(call.Known, func(known runtime.Known) bool {
				return known.Claim == token && known.Locator != nil && reflect.DeepEqual(*known.Locator, *loc)
			}) {
				return true
			}
		}
		return false
	})
	if orphanGrace != 2*time.Minute {
		t.Errorf("orphan grace = %s, want the shipped two minutes", orphanGrace)
	}
}

// The orphan sweep is told of every claim that is not retired, each with its locator or none. A
// suspended claim, a tree's root whose agent exited, and a failed claim keep their tokens in the
// known set with no locator; a retired claim leaves it. A runtime that deletes what no known claim owns — a sandbox's
// Sandboxes, a root's tree volume — therefore never deletes a claim that can still resume.
func TestRunKnowsEverySuspendedClaimToTheOrphanSweep(t *testing.T) {
	cfg := testConfig(t)
	cfg.LaunchFailureLimit = 1
	rt := fake.NewRuntime()
	rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("tmux refused")})
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 20 * time.Millisecond
	d := startDaemon(t, cfg, o)
	planner := architect()
	planner.Issue, planner.Role = "LEGION-4", claim.RolePlanner
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", planner, true); status != http.StatusInternalServerError {
		t.Fatalf("spawn of the claim meant to fail = %d; body %s", status, body)
	}
	project, _ := claim.ProjectToken(cfg.Project)
	failed, _ := claim.NewToken(project, "LEGION-4", claim.RolePlanner)
	root := d.spawn(architect())
	worker := architect()
	worker.Issue, worker.Role = "LEGION-2", claim.RoleImplementer
	suspended := d.spawn(worker)
	reviewer := architect()
	reviewer.Issue, reviewer.Role = "LEGION-3", claim.RoleReviewer
	retired := d.spawn(reviewer)
	secret := readyClaim(t, d, rt, root)
	readyClaim(t, d, rt, suspended)

	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(suspended)+"/suspend", nil, true); status != http.StatusOK {
		t.Fatalf("suspend = %d; body %s", status, body)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/exit", claim.ExitRequest{
		ClaimToken: root, SessionID: "ses_" + string(root), Secret: secret, Generation: 1, Reason: "the tree waits on review",
	}, false); status != http.StatusNoContent {
		t.Fatalf("root exit = %d; body %s", status, body)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(retired)+"/stop", nil, true); status != http.StatusOK {
		t.Fatalf("stop = %d; body %s", status, body)
	}
	for token, want := range map[claim.Token]supervise.ClaimState{
		root: supervise.StateSuspended, suspended: supervise.StateSuspended, failed: supervise.StateFailed,
		retired: supervise.StateRetired,
	} {
		if c := d.claim(token); c.State != string(want) || c.Locator != nil {
			t.Fatalf("%s is %s with locator %+v, want %s with none", token, c.State, c.Locator, want)
		}
	}
	sweeps := len(rt.CallsOf("ReconcileOrphans"))

	eventually(t, "a sweep that knows the suspended and failed claims and not the retired one", func() bool {
		for _, call := range rt.CallsOf("ReconcileOrphans")[sweeps:] {
			locators := map[claim.Token]*runtime.Locator{}
			for _, known := range call.Known {
				locators[known.Claim] = known.Locator
			}
			rootLocator, rootKnown := locators[root]
			suspendedLocator, suspendedKnown := locators[suspended]
			failedLocator, failedKnown := locators[failed]
			_, retiredKnown := locators[retired]
			if rootKnown && rootLocator == nil && suspendedKnown && suspendedLocator == nil &&
				failedKnown && failedLocator == nil && !retiredKnown {
				return true
			}
		}
		return false
	})
}

// readyClaim registers the claim's latest launch and reports it ready, as its agent would, and
// returns the secret the registration issued.
func readyClaim(t *testing.T, d *daemon, rt *fake.Runtime, token claim.Token) string {
	t.Helper()
	launch := lastLaunch(t, rt, token)
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_" + string(token), OmpSessionFile: "/sessions/" + string(token) + ".jsonl",
		AgentID: "agent", PluginContract: 1,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d; body %s", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration: %v", err)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_" + string(token), Secret: registered.Secret, Generation: 1,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d; body %s", token, status, body)
	}
	return registered.Secret
}

// The budgets and the waits a machine runs on are the configuration's, not defaults of its own.
func TestRunSupervisesWithTheConfiguredLimitsAndTimeouts(t *testing.T) {
	cfg := testConfig(t)
	cfg.LaunchFailureLimit = 2
	cfg.WorkerStopTimeout = 17 * time.Second
	rt := fake.NewRuntime()
	rt.ScriptSpawn(fake.SpawnResult{Err: errors.New("tmux refused")}, fake.SpawnResult{Err: errors.New("tmux refused")})
	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))

	status, body := d.request(http.MethodPost, "/legion/v1/operator/claims", architect(), true)
	if status != http.StatusInternalServerError || !strings.Contains(string(body), "tmux refused") {
		t.Fatalf("spawn = %d %s, want 500 naming the runtime's refusal", status, body)
	}
	project, _ := claim.ProjectToken(cfg.Project)
	failed, _ := claim.NewToken(project, "LEGION-1", claim.RoleArchitect)
	if c := d.claim(failed); c.State != string(supervise.StateFailed) || c.Budgets.LaunchFailures != 2 {
		t.Fatalf("the claim is %s after %d launch failures, want failed after the configured 2", c.State, c.Budgets.LaunchFailures)
	}
	if spawns := rt.CallsOf("Spawn"); len(spawns) != 2 {
		t.Fatalf("spawns = %d, want the configured 2", len(spawns))
	}

	worker := architect()
	worker.Issue, worker.Role = "LEGION-2", claim.RoleImplementer
	token := d.spawn(worker)
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil, true); status != http.StatusOK {
		t.Fatalf("stop = %d; body %s", status, body)
	}
	if releases := rt.CallsOf("Release"); len(releases) != 1 || releases[0].Grace != 17*time.Second {
		t.Fatalf("releases = %+v, want one with the configured 17s grace", releases)
	}
}

// Every agent fact the worker stream reports reaches the claim's machine as its own event — the
// late refusal included, which the stream added after the contract was first written.
func TestEveryStreamEventMapsToItsSuperviseEvent(t *testing.T) {
	const token = claim.Token("legion-legion-legion-1-tester")
	cases := []struct {
		in   stream.Event
		want supervise.Event
	}{
		{stream.Hello{Claim: token, Generation: 3}, supervise.StreamHello{Claim: token, Generation: 3}},
		{stream.TurnStart{Claim: token, DeliveryID: "d1"}, supervise.StreamTurnStart{Claim: token, DeliveryID: "d1"}},
		{stream.TurnEnd{Claim: token}, supervise.StreamTurnEnd{Claim: token}},
		{stream.LateRefusal{Claim: token, DeliveryID: "d1", Error: "Agent is busy"},
			supervise.StreamLateRefusal{Claim: token, DeliveryID: "d1", Error: "Agent is busy"}},
		{stream.Closed{Claim: token}, supervise.StreamClosed{Claim: token}},
	}
	for _, testCase := range cases {
		got, err := superviseEvent(testCase.in)
		if err != nil || !reflect.DeepEqual(got, testCase.want) {
			t.Errorf("superviseEvent(%#v) = %#v, %v; want %#v", testCase.in, got, err, testCase.want)
		}
	}
	if sealed := streamEventTypes(t); len(sealed) != len(cases) {
		t.Fatalf("internal/stream seals %d event types %v; this test maps %d — map the new one", len(sealed), sealed, len(cases))
	}
}

// streamEventTypes is every type in internal/stream with an isEvent method: the sealed set the
// pump has to map.
func streamEventTypes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "stream", "*.go"))
	if err != nil {
		t.Fatalf("list internal/stream: %v", err)
	}
	var sealed []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "isEvent" || fn.Recv == nil {
				continue
			}
			if ident, ok := fn.Recv.List[0].Type.(*ast.Ident); ok {
				sealed = append(sealed, ident.Name)
			}
		}
	}
	return sealed
}

func putClaim(t *testing.T, cfg config.Config, c supervise.Claim) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.PutClaim(ctx, c); err != nil {
		t.Fatalf("put %s: %v", c.Token, err)
	}
}

func writePrompt(t *testing.T, cfg config.Config, token claim.Token) {
	t.Helper()
	path := filepath.Join(cfg.StateDir, "prompts", string(token)+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("make the prompts directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("Plan it."), 0o600); err != nil {
		t.Fatalf("write the role prompt: %v", err)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("make %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func secretFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
