package sandbox

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// movedStreamURL and movedDaemonURL stand in for the daemon's own addresses after it moved:
// distinct from testOptions().StreamURL and testOptions().DaemonURL, the addresses every role
// process is already handed when a rig spawns it.
const (
	movedStreamURL = "tcp://192.0.2.9:13371"
	movedDaemonURL = "http://192.0.2.9:13370"
)

// secondRuntime builds another Runtime over g's same fake cluster state, with testOptions edited by
// move (the addresses a restarted daemon hands its role processes), on g's own context and store:
// the fake controller started in newRig, which goes on reconciling whichever Runtime's patches
// land, is this package's stand-in for the one real Agent Sandbox controller that ever serves a
// cluster, whatever daemon address last wrote its Sandboxes. This is how a daemon's own move to a
// new address is staged: the claims the restarted daemon re-adopts are the very Sandboxes and pods
// the one at the old address left running. restarted makes it g's runtime, so the fake launchers of
// every pod created from then on connect to it, as real ones dial the daemon at the new address.
func secondRuntime(t *testing.T, g *rig, move func(*Options)) (restarted func() *Runtime) {
	t.Helper()
	opts := testOptions()
	opts.Conns = g.conns
	opts.Now = func() time.Time { return *g.now.Load() }
	if g.store != nil {
		opts.Store = g.store
	}
	move(&opts)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.start(g.ctx, g.dyn, g.kube); err != nil {
		t.Fatal(err)
	}
	return func() *Runtime {
		g.r = r
		return r
	}
}

// moveStream moves only the worker stream listener, the address every role launcher's --connect
// names.
func moveStream(o *Options) { o.StreamURL = movedStreamURL }

// readopt re-adopts the claims of locs at r's address and returns their first observations, in the
// order the runtime emits them.
func readopt(t *testing.T, ctx context.Context, r *Runtime, locs ...runtime.Locator) []runtime.Observation {
	t.Helper()
	var known []runtime.Known
	for _, loc := range locs {
		known = append(known, runtime.Known{Claim: loc.Claim, Locator: &loc})
	}
	if err := r.ReconcileOrphans(ctx, known, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := r.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen []runtime.Observation
	for range locs {
		seen = append(seen, next(t, observations))
	}
	return seen
}

// observationOf is the one observation of seen about loc's claim.
func observationOf(t *testing.T, seen []runtime.Observation, loc runtime.Locator) runtime.Observation {
	t.Helper()
	i := slices.IndexFunc(seen, func(obs runtime.Observation) bool { return obs.Locator.Claim == loc.Claim })
	if i < 0 {
		t.Fatalf("no observation of %s among %+v", loc.Claim, seen)
	}
	return seen[i]
}

// connectOf is the --connect value the named role container's launcher dials.
func connectOf(t *testing.T, pod *corev1.Pod, container string) string {
	t.Helper()
	command := containerNamed(t, pod.Spec, container).Command
	i := slices.Index(command, connectFlag)
	if i < 0 || i+1 >= len(command) {
		t.Fatalf("pod %s's %s container runs %q, with no %s value", pod.Name, container, command, connectFlag)
	}
	return command[i+1]
}

// lastStart is the last start command token's fake launcher received.
func lastStart(t *testing.T, g *rig, token claim.Token) shimwire.LauncherStart {
	t.Helper()
	frames := g.sent(token)
	for i := len(frames) - 1; i >= 0; i-- {
		if start, ok := frames[i].(shimwire.LauncherStart); ok {
			return start
		}
	}
	t.Fatalf("%s's launcher was sent no start command", token)
	return shimwire.LauncherStart{}
}

// startEnv is a start command's environment as a map.
func startEnv(start shimwire.LauncherStart) map[string]string {
	env := map[string]string{}
	for _, pair := range start.Env {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	return env
}

// An issue pod whose launchers dial the stream the daemon moved from is reported StaleAddress for
// each of its roles the moment the runtime at the new address is told of their claims
// (re-adoption, ReconcileOrphans): a launcher never redials elsewhere, so it would otherwise read as
// disconnected for ever. The first role the supervisor relaunches — a Resume over its recorded
// locator — replaces the issue pod, whose launchers dial the corrected stream, and resumes the
// role's saved session there; a sibling relaunched after it resumes into that same new pod
// (LEGION-592).
func TestAnIssuePodDialingAStaleStreamIsReplacedAndEveryRoleResumesIntoTheNewPod(t *testing.T) {
	g := newRig(t, nil)
	tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
	testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)
	if testerLoc.Sandbox.PodUID != reviewerLoc.Sandbox.PodUID {
		t.Fatalf("the tester and reviewer run in pods %s and %s, want their issue's one pod", testerLoc.Sandbox.PodUID, reviewerLoc.Sandbox.PodUID)
	}

	restarted := secondRuntime(t, g, moveStream)
	r2 := restarted()
	seen := readopt(t, g.ctx, r2, testerLoc, reviewerLoc)
	for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
		obs := observationOf(t, seen, loc)
		if obs.Kind != runtime.StaleAddress || obs.Locator != loc {
			t.Fatalf("%s: %s at %+v, want StaleAddress at the recorded locator: %s", loc.Claim, obs.Kind, obs.Locator, obs.Detail)
		}
		if want := connectFlag + " " + testOptions().StreamURL + ", now " + movedStreamURL; !strings.Contains(obs.Detail, want) {
			t.Errorf("%s's detail %q lacks %q", loc.Claim, obs.Detail, want)
		}
		if strings.Contains(obs.Detail, "LEGION_DAEMON_URL") {
			t.Errorf("%s's detail %q names LEGION_DAEMON_URL, which did not move", loc.Claim, obs.Detail)
		}
	}

	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	newTester, err := r2.Resume(g.ctx, &testerLoc, tester)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if newTester.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("Resume kept the pod %s; the pod dialing the stale stream was not replaced", testerLoc.Sandbox.PodUID)
	}
	pod := g.pod(testerLoc.Sandbox.Name)
	for _, role := range claim.Roles {
		if dialed := connectOf(t, pod, string(role)); dialed != movedStreamURL {
			t.Errorf("the replaced pod's %s launcher dials %q, want the new stream %q", role, dialed, movedStreamURL)
		}
	}
	if start := lastStart(t, g, workerToken); start.ResumeFile != resumeSession || start.Generation != 2 {
		t.Errorf("the tester started generation %d resuming %q, want generation 2 resuming its saved session", start.Generation, start.ResumeFile)
	}
	if obs, err := r2.Probe(g.ctx, newTester); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe the replaced pod's tester: %+v, %v; want Alive", obs, err)
	}

	reviewer.Generation, reviewer.BootToken, reviewer.ResumeSessionFile = 2, "boot-reviewer-g2", resumeSession
	newReviewer, err := r2.Resume(g.ctx, &reviewerLoc, reviewer)
	if err != nil {
		t.Fatalf("Resume the reviewer: %v", err)
	}
	if newReviewer.Sandbox.PodUID != newTester.Sandbox.PodUID {
		t.Fatalf("the reviewer resumed into pod %s, want the tester's new pod %s", newReviewer.Sandbox.PodUID, newTester.Sandbox.PodUID)
	}
	if obs, err := r2.Probe(g.ctx, newReviewer); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe the reviewer in the new pod: %+v, %v; want Alive", obs, err)
	}
}

// A daemon whose API moved while its worker stream stayed put leaves every role it re-adopts
// dialling the stream it still listens on, and running a generation told a LEGION_DAEMON_URL that
// no longer reaches it, so the agent's every call to the daemon's API fails. The Sandbox's record of
// that generation says so: the role is reported StaleAddress, its detail naming the variable with
// the value the generation was started with and the one a new generation is handed, and the
// relaunch starts a new generation in the same pod, told the new URL (LEGION-592).
func TestARoleToldAStaleDaemonURLGetsANewGenerationInTheSamePod(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)

	restarted := secondRuntime(t, g, func(o *Options) { o.DaemonURL = movedDaemonURL })
	r2 := restarted()
	g.connectRunning(workerToken, loc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	obs := readopt(t, g.ctx, r2, loc)[0]
	if obs.Kind != runtime.StaleAddress || obs.Locator != loc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
	if want := "LEGION_DAEMON_URL " + testOptions().DaemonURL + ", now " + movedDaemonURL; !strings.Contains(obs.Detail, want) {
		t.Errorf("detail %q lacks %q", obs.Detail, want)
	}
	if strings.Contains(obs.Detail, connectFlag) {
		t.Errorf("detail %q names %s, which did not move", obs.Detail, connectFlag)
	}

	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	newLoc, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if newLoc.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod %s with %s; only the generation's environment moved", loc.Sandbox.PodUID, newLoc.Sandbox.PodUID)
	}
	start := lastStart(t, g, workerToken)
	if got := startEnv(start)["LEGION_DAEMON_URL"]; start.Generation != 2 || got != movedDaemonURL {
		t.Fatalf("the new generation %d is told LEGION_DAEMON_URL %q, want generation 2 told %q", start.Generation, got, movedDaemonURL)
	}
	g.eventually("the record of generation 2 to reach the store", func() bool {
		obs, err := r2.Probe(g.ctx, newLoc)
		return err == nil && obs.Kind == runtime.Alive
	})
}

// A role already running with every address the runtime hands now is left alone: Observe reports it
// Alive, never StaleAddress, so nothing about it is ever replaced.
func TestARoleAtTheCurrentAddressesIsNeverRepointed(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	if obs := readopt(t, g.ctx, g.r, loc)[0]; obs.Kind != runtime.Alive || obs.Locator != loc {
		t.Fatalf("%s at %+v, want Alive at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
}

// A claim mid-launch — its issue pod created and Pending, exactly what the runtime itself sees of a
// StateLaunching claim — is reported StaleAddress the same way a ready claim's is: the runtime has
// no notion of registration at all, only of the pod it watches, so re-adoption catches a stale
// stream whatever phase the supervisor's claim is in.
func TestAClaimMidLaunchWithAStaleStreamIsReported(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false) // the pod the controller creates stays Pending
	loc := g.spawn(workerSpec(t))
	if pod := g.pod(loc.Sandbox.Name); pod == nil || pod.Status.Phase != corev1.PodPending {
		t.Fatalf("the issue pod is %+v, want it Pending", pod)
	}

	restarted := secondRuntime(t, g, moveStream)
	if obs := readopt(t, g.ctx, restarted(), loc)[0]; obs.Kind != runtime.StaleAddress || obs.Locator != loc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
}

// Recording a generation's addresses costs one API write per role start: a role started in a
// running issue pod sends the Sandbox one patch, of that role's annotation alone, and nothing else.
func TestARoleStartedInARunningPodWritesOnlyItsAddressRecord(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(rootSpec(t))
	g.clearActions()
	g.spawn(workerSpec(t))
	writes := g.writes()
	if len(writes) != 1 || writes[0].verb != "patch" || writes[0].resource != "sandboxes" || writes[0].name != SandboxName(workerToken) {
		t.Fatalf("a start in a running pod wrote %v, want one patch of its Sandbox", writes)
	}
	if body := writes[0].patch; !strings.Contains(body, addressesAnnotation(claim.RoleTester)) || strings.Contains(body, addressesAnnotation(claim.RoleArchitect)) {
		t.Errorf("the patch %s is not the tester's record alone", body)
	}
	t.Logf("one role start: %d write, %d bytes of patch body", len(writes), len(writes[0].patch))
}
