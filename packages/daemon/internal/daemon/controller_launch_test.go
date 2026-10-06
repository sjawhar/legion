package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// controllerLaunched waits for the daemon's first launch of its controller and returns it.
func controllerLaunched(t *testing.T, rt *fake.Runtime, token claim.Token) runtime.SpawnSpec {
	t.Helper()
	testwait.Eventually(t, "the daemon to launch its controller", func() bool {
		for _, call := range rt.CallsOf("Spawn") {
			if call.Spec.Claim == token {
				return true
			}
		}
		return false
	})
	return lastLaunch(t, rt, token)
}

// Under `controller: daemon` the daemon launches the project's controller itself, with nobody
// asking: a claim on the controller role, on no issue, with no repository. Its agent registers with
// the launch's boot token — the credential the daemon minted for that launch, never one fetched
// over the operator's bearer — and is answered as the operator's controller is, then recorded as
// the project's controller (the state's controllerLocator, its grants). Once it says it is ready
// the daemon hands it the start message, so every launch runs the skill's start procedure. The
// operator's secret route refuses meanwhile: one controller runs per project.
func TestTheDaemonLaunchesItsControllerAndHandsItTheStartMessage(t *testing.T) {
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
	if launch.Role != claim.RoleController || launch.Issue != "" || launch.Tree != "" || !launch.Repository.IsZero() || launch.BootToken == "" {
		t.Fatalf("the controller was launched as %+v, want the controller role on no issue, no repository, with a boot token", launch)
	}

	status, body := d.request(http.MethodPost, "/legion/v1/controller/secret", api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion}, true)
	if status != http.StatusConflict || !strings.Contains(string(body), "controller: daemon") {
		t.Fatalf("the operator's controller secret = %d %s, want 409 naming controller: daemon", status, body)
	}

	sh := dialShim(t, record.address, launch.BootToken)
	testwait.Eventually(t, "the hello to reach the controller's machine", func() bool {
		return d.claim(token).State == string(supervise.StateShimConnected)
	})
	status, body = d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_controller", OmpSessionFile: "/sessions/controller.jsonl",
		AgentID: "ses_controller", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the controller with its boot token = %d; body %s", status, body)
	}
	var registration api.ControllerRegisterResponse
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&registration); err != nil {
		t.Fatalf("decode %s as the controller's registration: %v", body, err)
	}
	if registration.ClaimToken != token || registration.Role != api.ControllerRole || registration.Generation != launch.Generation || registration.Secret == "" {
		t.Fatalf("the controller's registration = %+v, want %s, role controller, generation %d, a secret", registration, token, launch.Generation)
	}
	if locator := d.state().ControllerLocator; locator == nil || locator.SessionID != "ses_controller" {
		t.Fatalf("controllerLocator = %+v, want the daemon's controller's session", locator)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: "ses_controller", Secret: registration.Secret}, false); status != http.StatusOK {
		t.Fatalf("the controller's grant = %d; body %s", status, body)
	}

	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: "ses_controller", Secret: registration.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", status, body)
	}
	prompt := sh.prompt()
	if prompt.Message != ControllerStartMessage {
		t.Fatalf("the controller was prompted %q, want the start message %q", prompt.Message, ControllerStartMessage)
	}
	sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true})
	sh.send(shimwire.AgentStart{})
	sh.send(shimwire.AgentEnd{})
	testwait.Eventually(t, "the start turn to end", func() bool {
		c := d.claim(token)
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})
}

// A daemon whose operator launches the controller (the default) launches none, and its secret
// route still mints the operator's capability.
func TestTheOperatorsDaemonLaunchesNoController(t *testing.T) {
	cfg := testConfig(t)
	rt := fake.NewRuntime()
	d := startDaemon(t, cfg, fakeRuntime(rt, &built{}))
	if status, body := d.request(http.MethodPost, "/legion/v1/controller/secret", api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion}, true); status != http.StatusOK {
		t.Fatalf("the operator's controller secret = %d %s, want 200", status, body)
	}
	for _, call := range rt.CallsOf("Spawn") {
		if call.Spec.Role == claim.RoleController {
			t.Fatalf("the operator's daemon launched a controller: %+v", call.Spec)
		}
	}
}

// controllerSpawns counts the runtime's spawns of the controller's claim.
func controllerSpawns(rt *fake.Runtime, token claim.Token) int {
	spawns := 0
	for _, call := range rt.CallsOf("Spawn") {
		if call.Spec.Claim == token {
			spawns++
		}
	}
	return spawns
}

// A controller whose launches run out of their budget fails, as any claim does, and nothing in the
// machine relaunches a failed claim. The keeper does: after its wait it retries the claim with fresh
// budgets, so the project is never left without a controller for good.
func TestTheKeeperRetriesAControllerWhoseLaunchesRanOut(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	rt := fake.NewRuntime()
	refused := errors.New("the cluster refused the Sandbox")
	rt.ScriptSpawn(fake.SpawnResult{Err: refused}, fake.SpawnResult{Err: refused}, fake.SpawnResult{Err: refused})
	o := fakeRuntime(rt, &built{})
	o.orphanSweep, o.controllerRetry = 50*time.Millisecond, 200*time.Millisecond
	d := startDaemon(t, cfg, o)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(project)
	testwait.Eventually(t, "the controller's refused launches to fail its claim", func() bool {
		return controllerSpawns(rt, token) == cfg.LaunchFailureLimit && d.claim(token).State == string(supervise.StateFailed)
	})
	testwait.Eventually(t, "the keeper to retry the failed controller on fresh budgets", func() bool {
		c := d.claim(token)
		return controllerSpawns(rt, token) == cfg.LaunchFailureLimit+1 && c.State == string(supervise.StateLaunching) &&
			c.Budgets.LaunchFailures == 0
	})
}

// A daemon that restarts finds its controller's claim in the store with the process it launched and
// re-adopts it: the keeper launches no second controller over the one still running.
func TestARestartedDaemonReadoptsItsControllerAndLaunchesNoSecond(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	rt := fake.NewRuntime()
	o := fakeRuntime(rt, &built{})
	o.orphanSweep = 50 * time.Millisecond
	d := startDaemon(t, cfg, o)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(project)
	controllerLaunched(t, rt, token)
	d.stop()

	rebindHeldPorts(t, &cfg)
	d = startDaemon(t, cfg, o)
	testwait.Eventually(t, "the restarted daemon to supervise its controller's claim", func() bool {
		return d.claim(token).State == string(supervise.StateLaunching)
	})
	time.Sleep(5 * o.orphanSweep)
	if spawns := controllerSpawns(rt, token); spawns != 1 {
		t.Fatalf("the controller was spawned %d times across the restart, want once", spawns)
	}
}
