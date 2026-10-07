package daemon

import (
	"context"
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

// registeredLaunch connects launch's shim to the daemon and registers its agent as session, as the
// controller's pod does, and returns the registration the agent is answered with.
func registeredLaunch(t *testing.T, d *daemon, record *built, launch runtime.SpawnSpec, session string) api.ControllerRegisterResponse {
	t.Helper()
	dialShim(t, record.address, launch.BootToken)
	testwait.Eventually(t, "the hello to reach the controller's machine", func() bool {
		return d.claim(launch.Claim).State == string(supervise.StateShimConnected)
	})
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the controller's launch as %s = %d; body %s", session, status, body)
	}
	var registration api.ControllerRegisterResponse
	if err := json.Unmarshal(body, &registration); err != nil {
		t.Fatalf("decode %s as the controller's registration: %v", body, err)
	}
	return registration
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
	if registration.ClaimToken != token || registration.Role != claim.RoleController || registration.Generation != launch.Generation || registration.Secret == "" {
		t.Fatalf("the controller's registration = %+v, want %s, role controller, generation %d, a secret", registration, token, launch.Generation)
	}
	state := d.state()
	if locator := state.ControllerLocator; locator == nil || locator.SessionID != "ses_controller" {
		t.Fatalf("controllerLocator = %+v, want the daemon's controller's session", locator)
	}
	// The controller is the project's, never an issue's worker; the strict client's own parse of
	// this shape is packages/contracts' state-controller-claim fixture.
	if view, listed := state.Issues[""]; listed {
		t.Fatalf("the state lists the controller's claim as an issue keyed \"\": %+v", view)
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

// A daemon restarted with `controller: operator` after a period under `controller: daemon` stops
// the controller's claim it finds in the store, at boot and before anything relaunches it: its
// process is released and the claim retires, so no pod of the daemon's contends with the operator's
// `legion controller start` for the one controller record, and the operator's secret route mints.
func TestADaemonSwitchedBackToTheOperatorStopsItsControllersClaim(t *testing.T) {
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
	launch := controllerLaunched(t, rt, token)
	d.stop()

	rebindHeldPorts(t, &cfg)
	cfg.ControllerLaunch = config.ControllerLaunchOperator
	d = startDaemon(t, cfg, o)
	if state := d.claim(token).State; state != string(supervise.StateRetired) {
		t.Fatalf("the controller's claim is %s once the operator's daemon booted, want retired", state)
	}
	released := false
	for _, call := range rt.CallsOf("Release") {
		released = released || call.Released.Claim == token
	}
	if !released {
		t.Fatalf("the operator's daemon never released the controller's process; runtime calls %+v", rt.Calls())
	}
	time.Sleep(5 * o.orphanSweep)
	if spawns := controllerSpawns(rt, token); spawns != 1 {
		t.Fatalf("the controller was spawned %d times across the switch, want once", spawns)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: "ses_pod", OmpSessionFile: "/sessions/pod.jsonl",
		AgentID: "ses_pod", PluginContract: api.DaemonAPIVersion,
	}, false); status == http.StatusOK {
		t.Fatalf("the stopped controller's launch registered = %d; body %s", status, body)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/controller/secret", api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion}, true); status != http.StatusOK {
		t.Fatalf("the operator's controller secret = %d %s, want 200", status, body)
	}
}

// A daemon switched back to `controller: operator` registers no earlier launch of the controller it
// stopped. Once the claim has relaunched, a restart forgets the earlier launch's boot token, which
// then resolves to no launch and reaches the operator's capability path; registered there it would
// sit outside the claim's generation fence and mint controller grants. Neither the capability the
// launch registered under nor the one the stop mints in its place is a token anyone holds, and the
// stop ends the record's registration, so the state names no dead pod's session.
func TestASwitchedBackDaemonRegistersNoEarlierLaunchOfItsController(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	rt := fake.NewRuntime()
	var record built
	o := fakeRuntime(rt, &record)
	o.orphanSweep = 50 * time.Millisecond
	d := startDaemon(t, cfg, o)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(project)
	first := controllerLaunched(t, rt, token)
	dialShim(t, record.address, first.BootToken)
	testwait.Eventually(t, "the hello to reach the controller's machine", func() bool {
		return d.claim(token).State == string(supervise.StateShimConnected)
	})
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: first.BootToken, SessionID: "ses_first", OmpSessionFile: "/sessions/controller.jsonl",
		AgentID: "ses_first", PluginContract: api.DaemonAPIVersion,
	}, false); status != http.StatusOK {
		t.Fatalf("register the controller's first launch = %d; body %s", status, body)
	}
	// The pod dies and the machine resumes its session as a second launch, which has not
	// registered when the daemon restarts.
	rt.Emit(runtime.Observation{Locator: *d.claim(token).Locator, Kind: runtime.Gone, Detail: "pod gone"})
	testwait.Eventually(t, "the controller's relaunch", func() bool {
		c := d.claim(token)
		return c.Generation == first.Generation+1 && c.State == string(supervise.StateLaunching)
	})
	d.stop()

	rebindHeldPorts(t, &cfg)
	cfg.ControllerLaunch = config.ControllerLaunchOperator
	d = startDaemon(t, cfg, o)
	if state := d.claim(token).State; state != string(supervise.StateRetired) {
		t.Fatalf("the controller's claim is %s once the operator's daemon booted, want retired", state)
	}
	if locator := d.state().ControllerLocator; locator != nil {
		t.Errorf("controllerLocator = %+v once the operator's daemon stopped the controller, want none", locator)
	}
	status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: first.BootToken, SessionID: "ses_stale", OmpSessionFile: "/sessions/stale.jsonl",
		AgentID: "ses_stale", PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != claim.InvalidBootToken.Status || !strings.Contains(string(body), claim.InvalidBootToken.Message) {
		t.Fatalf("the first launch's boot token registered on the operator's daemon = %d %s, want %d %q",
			status, body, claim.InvalidBootToken.Status, claim.InvalidBootToken.Message)
	}
}

// A daemon switched back to `controller: operator` whose stop of the controller fails refuses to
// boot with the controller record as it was, so a daemon switched to `controller: daemon` again
// re-adopts a controller whose registration still mints its grants, and an operator's daemon whose
// stop then succeeds completes the switch. Ending the registration ahead of a stop that then failed
// would leave that pod running, re-adopted with its registration gone: every grant refused and
// every wake dropped, with nothing to heal it until the pod died.
func TestASwitchedBackDaemonWhoseStopFailsLeavesItsControllerRegistered(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	rt := fake.NewRuntime()
	var record built
	o := fakeRuntime(rt, &record)
	o.orphanSweep = 50 * time.Millisecond
	d := startDaemon(t, cfg, o)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(project)
	registration := registeredLaunch(t, d, &record, controllerLaunched(t, rt, token), "ses_pod")
	d.stop()

	rebindHeldPorts(t, &cfg)
	cfg.ControllerLaunch = config.ControllerLaunchOperator
	refused := errors.New("the cluster refused the Sandbox delete")
	rt.FailReleaseOf(token, refused)
	err = run(context.Background(), cfg, quietLogger(), o)
	if err == nil || !strings.Contains(err.Error(), "stop "+string(token)) || !strings.Contains(err.Error(), refused.Error()) {
		t.Fatalf("the operator's daemon whose stop of the controller failed booted with %v, want a refusal naming the stop of %s", err, token)
	}

	rebindHeldPorts(t, &cfg)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	rt.FailReleaseOf(token, nil)
	d = startDaemon(t, cfg, o)
	if locator := d.state().ControllerLocator; locator == nil || locator.SessionID != "ses_pod" {
		t.Errorf("controllerLocator = %+v after the refused switch back, want the re-adopted pod's session ses_pod", locator)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: "ses_pod", Secret: registration.Secret}, false); status != http.StatusOK {
		t.Errorf("the re-adopted controller's grant = %d %s, want 200", status, body)
	}
	if state := d.claim(token).State; state == string(supervise.StateRetired) {
		t.Errorf("the controller's claim is %s after the refused switch back, want it re-adopted", state)
	}
	d.stop()

	rebindHeldPorts(t, &cfg)
	cfg.ControllerLaunch = config.ControllerLaunchOperator
	d = startDaemon(t, cfg, o)
	if state := d.claim(token).State; state != string(supervise.StateRetired) {
		t.Errorf("the controller's claim is %s once the operator's daemon stopped it, want retired", state)
	}
	if locator := d.state().ControllerLocator; locator != nil {
		t.Errorf("controllerLocator = %+v once the operator's daemon stopped the controller, want none", locator)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: "ses_pod", Secret: registration.Secret}, false); status != http.StatusForbidden {
		t.Errorf("the stopped controller's grant = %d %s, want 403", status, body)
	}
}

// A daemon switched back to `controller: operator` ends the registration the controller it stopped
// still holds, and only that one. A claim `legion claims stop` retired under `controller: daemon`
// keeps its session, which the record still names, so the switch back ends that registration even
// though it stops nothing; once the operator's controller has registered, a later boot leaves the
// operator's registration alone.
func TestASwitchedBackDaemonEndsOnlyItsStoppedControllersRegistration(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	rt := fake.NewRuntime()
	var record built
	o := fakeRuntime(rt, &record)
	o.orphanSweep = 50 * time.Millisecond
	d := startDaemon(t, cfg, o)
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(project)
	registration := registeredLaunch(t, d, &record, controllerLaunched(t, rt, token), "ses_pod")
	if status, body := d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/stop", nil, true); status != http.StatusOK {
		t.Fatalf("stop the controller's claim = %d; body %s", status, body)
	}
	d.stop()

	rebindHeldPorts(t, &cfg)
	cfg.ControllerLaunch = config.ControllerLaunchOperator
	d = startDaemon(t, cfg, o)
	if locator := d.state().ControllerLocator; locator != nil {
		t.Errorf("controllerLocator = %+v once the operator's daemon booted, want the stopped controller's registration ended", locator)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: "ses_pod", Secret: registration.Secret}, false); status != http.StatusForbidden {
		t.Errorf("the stopped controller's grant = %d %s, want 403", status, body)
	}
	status, body := d.request(http.MethodPost, "/legion/v1/controller/secret", api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion}, true)
	if status != http.StatusOK {
		t.Fatalf("the operator's controller secret = %d %s, want 200", status, body)
	}
	var secret api.ControllerSecretResponse
	if err := json.Unmarshal(body, &secret); err != nil {
		t.Fatal(err)
	}
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: secret.Secret, SessionID: "ses_operator", OmpSessionFile: "/sessions/operator.jsonl",
		AgentID: "ses_operator", PluginContract: api.DaemonAPIVersion,
	}, false); status != http.StatusOK {
		t.Fatalf("register the operator's controller = %d; body %s", status, body)
	}
	d.stop()

	rebindHeldPorts(t, &cfg)
	d = startDaemon(t, cfg, o)
	if locator := d.state().ControllerLocator; locator == nil || locator.SessionID != "ses_operator" {
		t.Errorf("controllerLocator = %+v after another boot, want the operator's controller ses_operator", locator)
	}
}
