package sandbox

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// movedStreamURL and movedDaemonURL stand in for the daemon's own addresses after it moved:
// distinct from testOptions().StreamURL and testOptions().DaemonURL, the addresses every claim's
// pod is already handed when a rig spawns it.
const (
	movedStreamURL = "tcp://192.0.2.9:13371"
	movedDaemonURL = "http://192.0.2.9:13370"
)

// secondRuntime builds another Runtime over g's same fake cluster state, with testOptions edited by
// move (the addresses a restarted daemon hands its pods), on g's own context: the fake controller
// started in newRig, which goes on reconciling whichever Runtime's patches land, is this package's
// stand-in for the one real Agent Sandbox controller that ever serves a cluster, whatever daemon
// address last wrote its Sandboxes. This is how a daemon's own move to a new address is staged: the
// claims the restarted daemon re-adopts are the very Sandboxes and pods the one at the old address
// left running.
func secondRuntime(t *testing.T, g *rig, move func(*Options)) *Runtime {
	t.Helper()
	opts := testOptions()
	opts.Conns = g.conns
	opts.Now = func() time.Time { return *g.now.Load() }
	move(&opts)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.start(g.ctx, g.dyn, g.kube); err != nil {
		t.Fatal(err)
	}
	return r
}

// moveStream moves only the worker stream listener, the address the shim's --connect names.
func moveStream(o *Options) { o.StreamURL = movedStreamURL }

// readopt re-adopts loc's claim at r's address and returns its first observation.
func readopt(t *testing.T, ctx context.Context, r *Runtime, loc runtime.Locator) runtime.Observation {
	t.Helper()
	if err := r.ReconcileOrphans(ctx, []runtime.Known{{Claim: loc.Claim, Locator: &loc}}, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := r.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return next(t, observations)
}

// A pod dialing the address the daemon moved from is reported StaleAddress the moment the
// runtime at the new address is told of its claim (re-adoption, ReconcileOrphans), and the
// supervisor's relaunch of a registered claim — a Resume over the recorded locator — lands a new
// pod whose shim dials the corrected address and goes on to resume the claim's saved session
// (LEGION-592).
func TestAPodDialingAStaleAddressIsReplacedAndResumesItsSession(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	if obs, err := g.r.Probe(g.ctx, loc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe right after Spawn: %+v, %v; want Alive", obs, err)
	}

	r2 := secondRuntime(t, g, moveStream)
	obs := readopt(t, g.ctx, r2, loc)
	if obs.Kind != runtime.StaleAddress || obs.Locator != loc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
	if want := connectFlag + " " + testOptions().StreamURL + ", now " + movedStreamURL; !strings.Contains(obs.Detail, want) {
		t.Errorf("detail %q lacks %q", obs.Detail, want)
	}
	if strings.Contains(obs.Detail, "LEGION_DAEMON_URL") {
		t.Errorf("detail %q names LEGION_DAEMON_URL, which did not move", obs.Detail)
	}

	spec.ResumeSessionFile = resumeSession
	newLoc, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if newLoc.Incarnation == loc.Incarnation {
		t.Fatalf("Resume kept the same incarnation %s; the stale pod was not replaced", loc.Incarnation)
	}
	if dialed := connectOf(t, g.pod(loc.Sandbox.Name)); dialed != movedStreamURL {
		t.Fatalf("the replaced pod dials %q, want the new address %q", dialed, movedStreamURL)
	}
	if obs, err := r2.Probe(g.ctx, newLoc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe the replaced pod: %+v, %v; want Alive", obs, err)
	}
}

// connectOf is the --connect value pod's main container runs the shim with.
func connectOf(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	command := containerNamed(t, pod.Spec, mainContainer).Command
	i := slices.Index(command, connectFlag)
	if i < 0 || i+1 >= len(command) {
		t.Fatalf("pod %s's main container runs %q, with no %s value", pod.Name, command, connectFlag)
	}
	return command[i+1]
}

// A daemon whose API moved while its worker stream stayed put leaves every pod it re-adopts
// dialling the stream it still listens on, and told a LEGION_DAEMON_URL that no longer reaches it,
// so the agent's every call to the daemon's API fails: the pod is reported StaleAddress, its detail
// naming the variable with the value the pod holds and the one a new pod is handed, and the
// relaunch lands a pod told the new URL (LEGION-592).
func TestAPodToldAStaleDaemonURLIsReplaced(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)

	r2 := secondRuntime(t, g, func(o *Options) { o.DaemonURL = movedDaemonURL })
	obs := readopt(t, g.ctx, r2, loc)
	if obs.Kind != runtime.StaleAddress || obs.Locator != loc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
	if want := "LEGION_DAEMON_URL " + testOptions().DaemonURL + ", now " + movedDaemonURL; !strings.Contains(obs.Detail, want) {
		t.Errorf("detail %q lacks %q", obs.Detail, want)
	}
	if strings.Contains(obs.Detail, "--connect") {
		t.Errorf("detail %q names --connect, which did not move", obs.Detail)
	}

	spec.ResumeSessionFile = resumeSession
	newLoc, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	env := envOf(containerNamed(t, g.pod(loc.Sandbox.Name).Spec, mainContainer))
	if got := env["LEGION_DAEMON_URL"]; got != movedDaemonURL {
		t.Fatalf("the replaced pod is told LEGION_DAEMON_URL %q, want %q", got, movedDaemonURL)
	}
	if obs, err := r2.Probe(g.ctx, newLoc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe the replaced pod: %+v, %v; want Alive", obs, err)
	}
}

// A pod already dialing the runtime's current address is left alone: Observe reports it Alive,
// never StaleAddress, so nothing about it is ever replaced.
func TestAPodAtTheCurrentAddressIsNeverRepointed(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	if obs := readopt(t, g.ctx, g.r, loc); obs.Kind != runtime.Alive || obs.Locator != loc {
		t.Fatalf("%s at %+v, want Alive at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
}

// A claim mid-launch — its pod created and running, its agent not yet registered, exactly what
// the runtime itself sees of a StateLaunching claim — is reported StaleAddress the same way a
// ready claim's is: the runtime has no notion of registration at all, only of the pod it
// watches, so re-adoption catches a stale address whatever phase the supervisor's claim is in.
func TestAClaimMidLaunchWithAStaleAddressIsReported(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false) // the pod the controller creates stays Pending, unregistered
	loc := g.spawn(workerSpec(t))
	g.eventually("the pod to exist", func() bool { return g.pod(loc.Sandbox.Name) != nil })

	r2 := secondRuntime(t, g, moveStream)
	if obs := readopt(t, g.ctx, r2, loc); obs.Kind != runtime.StaleAddress || obs.Locator != loc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
}
