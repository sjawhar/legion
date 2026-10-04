package sandbox

import (
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// next is the next observation, failing the test when none arrives within a second.
func next(t *testing.T, observations <-chan runtime.Observation) runtime.Observation {
	t.Helper()
	select {
	case obs, ok := <-observations:
		if !ok {
			t.Fatal("the observation channel closed")
		}
		return obs
	case <-time.After(time.Second):
		t.Fatal("no observation within a second")
	}
	return runtime.Observation{}
}

// quiet fails the test when an observation arrives within the window.
func quiet(t *testing.T, observations <-chan runtime.Observation, window time.Duration) {
	t.Helper()
	select {
	case obs := <-observations:
		t.Fatalf("unexpected observation %s of %s at %s: %s", obs.Kind, obs.Locator.Claim, obs.Locator.Incarnation, obs.Detail)
	case <-time.After(window):
	}
}

func runningPod(token claim.Token, uid string, role claim.Role) *corev1.Pod {
	return podObject(SandboxName(token), uid, "uid-sandbox-"+string(role), claimLabels(role), runningStatus())
}

func runningSandbox(t *testing.T, token claim.Token, role claim.Role) k8sruntime.Object {
	return sandboxObject(t, SandboxName(token), "uid-sandbox-"+string(role), modeRunning, claimLabels(role))
}

// The observations of a claim carry the incarnation the runtime recorded, whatever pod they saw:
// the supervisor fences on that incarnation, so a death stamped with any other would never reach
// the machine (B3). Once Gone or NotRecordedProcess is delivered the claim leaves the watch.
func TestAnObservationCarriesTheRecordedIncarnation(t *testing.T) {
	t.Run("gone", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{runningSandbox(t, workerToken, claim.RoleTester), runningPod(workerToken, recorded, claim.RoleTester)},
			withoutController())
		g.reportLauncher(workerToken, shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 1, PID: 42}})
		observations, err := g.r.Observe(g.ctx)
		if err != nil {
			t.Fatal(err)
		}
		loc := sandboxLocator(workerToken, recorded)
		if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &loc}}, time.Hour); err != nil {
			t.Fatal(err)
		}
		if obs := next(t, observations); obs.Kind != runtime.Alive || obs.Locator != loc {
			t.Fatalf("%s at %+v, want Alive at the recorded locator", obs.Kind, obs.Locator)
		}
		g.update(g.pod(SandboxName(workerToken)), func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{terminated(workerContainer, 137, "Error")}}
		})
		if obs := next(t, observations); obs.Kind != runtime.Gone || obs.Locator != loc {
			t.Fatalf("%s at %+v, want Gone at the recorded locator", obs.Kind, obs.Locator)
		}
		g.update(g.pod(SandboxName(workerToken)), func(p *corev1.Pod) { p.Labels["touched"] = "yes" })
		quiet(t, observations, 200*time.Millisecond)
	})
	t.Run("not the recorded process", func(t *testing.T) {
		g := newRig(t, []k8sruntime.Object{runningSandbox(t, workerToken, claim.RoleTester), runningPod(workerToken, recorded, claim.RoleTester)},
			withoutController())
		g.reportLauncher(workerToken, shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 1, PID: 42}})
		observations, err := g.r.Observe(g.ctx)
		if err != nil {
			t.Fatal(err)
		}
		loc := sandboxLocator(workerToken, recorded)
		if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &loc}}, time.Hour); err != nil {
			t.Fatal(err)
		}
		next(t, observations)
		// One write that swaps the pod under the name, as a store sees a delete and a create it
		// caught up on at once.
		if err := g.kube.Tracker().Update(podsGVR, runningPod(workerToken, "uid-pod-recreated", claim.RoleTester), testNamespace); err != nil {
			t.Fatal(err)
		}
		obs := next(t, observations)
		if obs.Kind != runtime.NotRecordedProcess || obs.Locator != loc {
			t.Fatalf("%s at %+v, want NotRecordedProcess at the recorded locator", obs.Kind, obs.Locator)
		}
	})
}

// An issue pod killed while no runtime ran is reported Gone the moment a new runtime is told of
// its claim, stamped with the incarnation recorded before, and nothing is relaunched; a claim in
// another issue pod that lived through it is Alive at its recorded incarnation (B3, re-adoption).
func TestAPodKilledWhileNoRuntimeRanIsGoneAtReadoption(t *testing.T) {
	killed := podObject(SandboxName(childToken), "uid-pod-child", "uid-sandbox-implementer", claimLabels(claim.RoleImplementer), corev1.PodStatus{
		Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{terminated(string(claim.RoleImplementer), 137, "Error")},
	})
	g := newRig(t, []k8sruntime.Object{
		runningSandbox(t, childToken, claim.RoleImplementer), killed,
		runningSandbox(t, rootToken, claim.RoleArchitect), runningPod(rootToken, "uid-pod-root", claim.RoleArchitect),
	})
	g.reportLauncher(rootToken, shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 1, PID: 42}})
	observations, err := g.r.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	child, root := sandboxLocator(childToken, "uid-pod-child"), sandboxLocator(rootToken, "uid-pod-root")
	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: childToken, Locator: &child}, {Claim: rootToken, Locator: &root}}, 0); err != nil {
		t.Fatal(err)
	}
	seen := map[claim.Token]runtime.Observation{}
	for range 2 {
		obs := next(t, observations)
		seen[obs.Locator.Claim] = obs
	}
	if obs := seen[childToken]; obs.Kind != runtime.Gone || obs.Locator != child {
		t.Errorf("the killed child issue's role: %s at %+v, want Gone at %s", obs.Kind, obs.Locator, child.Incarnation)
	}
	if obs := seen[rootToken]; obs.Kind != runtime.Alive || obs.Locator != root {
		t.Errorf("the living root: %s at %+v, want Alive at %s", obs.Kind, obs.Locator, root.Incarnation)
	}
	if writes := g.writes(); len(writes) > 0 {
		t.Fatalf("re-adoption wrote %v; it only observes", writes)
	}
}

// Suspend drops the claim from the watch, as tmux does: when it returns the watch no longer holds
// the claim, Observe delivers nothing of it evaluated after that moment while its launcher reports
// the child gone, and a Probe of the recorded locator answers Gone (P1) — while the issue pod,
// which other roles share, stays. The live suspend check in live_lifecycle_test.go applies the
// same rule.
func TestSuspendTakesTheClaimOutOfTheWatch(t *testing.T) {
	g := newRig(t, nil)
	observations, err := g.r.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	loc := g.spawn(workerSpec(t))
	if obs := next(t, observations); obs.Kind != runtime.Alive || obs.Locator != loc {
		t.Fatalf("%s at %+v, want Alive at the new locator", obs.Kind, obs.Locator)
	}
	if err := g.r.Suspend(g.ctx, loc); err != nil {
		t.Fatal(err)
	}
	returnedAt := g.r.now()
	// The rig's clock stands still; moved on, anything Observe evaluates from here is stamped later.
	g.advance(time.Second)
	if recorded, ok := g.r.recorded(workerToken); ok {
		t.Fatalf("Suspend returned with the claim still in the watch, at %s", recorded.Incarnation)
	}
	g.eventually("the launcher to report the child gone", func() bool {
		state, connected := g.r.launchers.state(workerToken, loc.Sandbox.PodUID)
		return connected && state.Child == nil
	})
	if g.pod(loc.Sandbox.Name) == nil {
		t.Fatal("suspending one role removed the issue pod")
	}
	var late []runtime.Observation
	for window := time.After(200 * time.Millisecond); ; {
		select {
		case obs := <-observations:
			late = append(late, obs)
			continue
		case <-window:
		}
		break
	}
	for _, obs := range late {
		if obs.At.After(returnedAt) {
			t.Fatalf("after Suspend: %s of %s evaluated at %s, after Suspend returned at %s", obs.Kind, obs.Locator.Claim, obs.At, returnedAt)
		}
	}
	obs, err := g.r.Probe(g.ctx, loc)
	if err != nil || obs.Kind != runtime.Gone {
		t.Fatalf("Probe after Suspend: %s %q, %v; want Gone", obs.Kind, obs.Detail, err)
	}
}

// A verdict evaluated while Suspend takes the claim out of the watch is not delivered. The Stage 4a
// run on production hit it: Suspend's shutdown frame ended the pod, Observe began evaluating the
// death — the log read is its slow step — and Suspend returned before the Gone was sent.
func TestAVerdictEvaluatedAcrossSuspendIsNotDelivered(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{runningSandbox(t, workerToken, claim.RoleTester), runningPod(workerToken, recorded, claim.RoleTester)},
		withoutController())
	g.reportLauncher(workerToken, shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 1, PID: 42}})
	observations, err := g.r.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	loc := sandboxLocator(workerToken, recorded)
	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &loc}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if obs := next(t, observations); obs.Kind != runtime.Alive {
		t.Fatalf("%s, want Alive", obs.Kind)
	}
	reading, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g.kube.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		once.Do(func() { close(reading) })
		<-release
		return false, nil, nil
	})
	g.update(g.pod(SandboxName(workerToken)), func(p *corev1.Pod) {
		p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{terminated(workerContainer, 143, "Error")}}
	})
	select {
	case <-reading:
	case <-time.After(time.Second):
		t.Fatal("Observe never began evaluating the ended pod")
	}
	if err := g.r.Suspend(g.ctx, loc); err != nil {
		t.Fatal(err)
	}
	close(release)
	quiet(t, observations, 300*time.Millisecond)
}

// Every watched claim is evaluated again each probe interval, with no change to its objects: the
// supervisor retries a waiting delivery on an Alive, as it does at each tmux sweep.
func TestEveryWatchedClaimIsEvaluatedEachProbeInterval(t *testing.T) {
	g := newRig(t, []k8sruntime.Object{runningSandbox(t, workerToken, claim.RoleTester), runningPod(workerToken, recorded, claim.RoleTester)},
		withoutController(), withOptions(func(o *Options) { o.ProbeInterval = 50 * time.Millisecond }))
	loc := sandboxLocator(workerToken, recorded)
	g.reportLauncher(workerToken, shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 1, PID: 42}})
	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &loc}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	observations, err := g.r.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if obs := next(t, observations); obs.Kind != runtime.Alive || obs.Locator != loc {
			t.Fatalf("%s at %+v, want Alive at the recorded locator", obs.Kind, obs.Locator)
		}
	}
	if _, err := g.r.Observe(g.ctx); err == nil {
		t.Fatal("a second Observe ran beside the first")
	}
}

// The daemon's sweep reads its claims unordered against the machines, so it can hand
// ReconcileOrphans a locator the claim has since relaunched past. A stale located Known never
// displaces the watch's newer incarnation: otherwise Observe reports the stale one gone and drops
// the claim, leaving the live pod unwatched, and a stale Suspend then stops the newer relaunch
// (decision 3d).
func TestAStaleKnownLocatorNeverDisplacesANewerIncarnation(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	stale := g.spawn(spec)
	if err := g.r.Suspend(g.ctx, stale); err != nil {
		t.Fatal(err)
	}
	spec.Generation, spec.BootToken = 2, "boot-g2"
	newer := g.spawn(spec)
	if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &stale}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if recorded, ok := g.r.recorded(workerToken); !ok || recorded != newer {
		t.Fatalf("the watch holds %+v after a stale sweep, want the newer incarnation %s", recorded, newer.Incarnation)
	}
	if err := g.r.Suspend(g.ctx, stale); err != nil {
		t.Fatal(err)
	}
	if obs, err := g.r.Probe(g.ctx, newer); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("a stale Suspend after a stale sweep left the newer generation %s, %v", obs.Kind, err)
	}
}
