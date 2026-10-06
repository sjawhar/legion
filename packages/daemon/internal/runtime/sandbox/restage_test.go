package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// secondRuntime builds another Runtime over g's same fake cluster state, at a stream address of
// its own: the fake controller started in newRig, which goes on reconciling whichever Runtime's
// patches land, is this package's stand-in for the one real Agent Sandbox controller that ever
// serves a cluster, whatever daemon address last wrote its Sandboxes. This is how a daemon's own
// move to a new address is staged: the claims the restarted daemon re-adopts are the very
// Sandboxes and pods the one at the old address left running.
func secondRuntime(t *testing.T, g *rig, streamURL string) (*Runtime, context.Context) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	opts := testOptions()
	opts.Conns = g.conns
	opts.Now = func() time.Time { return *g.now.Load() }
	opts.StreamURL = streamURL
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.start(ctx, g.dyn, g.kube); err != nil {
		t.Fatal(err)
	}
	return r, ctx
}

// A pod dialing the address the daemon moved from is reported Stale the moment the runtime at
// the new address is told of its claim (re-adoption, ReconcileOrphans), and the supervisor's own
// replace for it — Resume over the same recorded locator, exactly what a suspend-then-resume
// already does — lands a new pod whose shim dials the corrected address and goes on to resume
// the claim's saved session (LEGION-592).
func TestAPodDialingAStaleAddressIsReplacedAndResumesItsSession(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	if obs, err := g.r.Probe(g.ctx, loc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe right after Spawn: %+v, %v; want Alive", obs, err)
	}

	const movedTo = "tcp://192.0.2.9:13371"
	r2, ctx2 := secondRuntime(t, g, movedTo)
	if err := r2.ReconcileOrphans(ctx2, []runtime.Known{{Claim: workerToken, Locator: &loc}}, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := r2.Observe(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	obs := next(t, observations)
	if obs.Kind != runtime.Stale || obs.Locator != loc {
		t.Fatalf("%s at %+v, want Stale at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}

	spec.ResumeSessionFile = resumeSession
	newLoc, err := r2.Resume(ctx2, &loc, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if newLoc.Incarnation == loc.Incarnation {
		t.Fatalf("Resume kept the same incarnation %s; the stale pod was not replaced", loc.Incarnation)
	}
	pod := g.pod(loc.Sandbox.Name)
	if dialed, ok := connectAddress(pod); !ok || dialed != movedTo {
		t.Fatalf("the replaced pod dials %q (found=%t), want the new address %q", dialed, ok, movedTo)
	}
	if obs, err := r2.Probe(ctx2, newLoc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe the replaced pod: %+v, %v; want Alive", obs, err)
	}
}

// A pod already dialing the runtime's current address is left alone: Observe reports it Alive,
// never Stale, so nothing about it is ever replaced.
func TestAPodAtTheCurrentAddressIsNeverRepointed(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &loc}}, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := g.r.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs := next(t, observations); obs.Kind != runtime.Alive || obs.Locator != loc {
		t.Fatalf("%s at %+v, want Alive at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
}

// A claim mid-launch — its pod created and running, its agent not yet registered, exactly what
// the runtime itself sees of a StateLaunching claim — is reported Stale the same way a ready
// claim's is: the runtime has no notion of registration at all, only of the pod it watches, so
// re-adoption catches a stale address whatever phase the supervisor's claim is in.
func TestAClaimMidLaunchWithAStaleAddressIsReported(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false) // the pod the controller creates stays Pending, unregistered
	spec := workerSpec(t)
	loc := g.spawn(spec)
	g.eventually("the pod to exist", func() bool { return g.pod(loc.Sandbox.Name) != nil })

	const movedTo = "tcp://192.0.2.9:13371"
	r2, ctx2 := secondRuntime(t, g, movedTo)
	if err := r2.ReconcileOrphans(ctx2, []runtime.Known{{Claim: workerToken, Locator: &loc}}, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := r2.Observe(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	obs := next(t, observations)
	if obs.Kind != runtime.Stale || obs.Locator != loc {
		t.Fatalf("%s at %+v, want Stale at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
}
