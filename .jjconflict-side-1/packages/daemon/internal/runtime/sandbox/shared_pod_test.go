package sandbox

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// implementerToken is the implementer of the rig's tree root issue: a role of the architect's own
// issue pod.
var implementerToken = claim.Token("legion-legion-legion-208-implementer")

// endOf reads observations until the first that ends loc's process, a Gone or NotRecordedProcess
// of its claim, skipping every other, each read bounded as next bounds it.
func endOf(t *testing.T, observations <-chan runtime.Observation, loc runtime.Locator) runtime.Observation {
	t.Helper()
	for range 20 {
		obs := next(t, observations)
		if obs.Locator.Claim == loc.Claim && (obs.Kind == runtime.Gone || obs.Kind == runtime.NotRecordedProcess) {
			return obs
		}
	}
	t.Fatalf("no observation ended %s's process", loc.Claim)
	return runtime.Observation{}
}

// A daemon restarted onto a release pinning another image re-adopts a tree's architect still
// running in its issue pod, built from the earlier image, and launches again the implementer's
// launch its predecessor did not finish (LEGION-729). The launch replaces the drifted pod, which
// ends the architect's process with it. That end is the daemon's own act: the runtime reports it
// Displaced, and the architect's Machine resumes its kept session in the new pod at once, charging
// nothing, and registers and is ready there, its process alive.
func TestALaunchIntoADriftedPodResumesItsOtherLiveRoleOnItsSessionUncharged(t *testing.T) {
	g := newRig(t, nil)
	architect := rootSpec(t)
	before, _, _ := supervisedSandboxMachine(t, g, architect)
	architectLoc := *startSupervisedSandboxRole(t, g, before, architect, resumeSession)
	implementer := testSpec(t, implementerToken, claim.RoleImplementer, testTree)
	implementerLoc := g.spawn(implementer)
	if architectLoc.Sandbox.PodUID != implementerLoc.Sandbox.PodUID {
		t.Fatalf("the architect and implementer run in pods %s and %s, want their issue's one pod", architectLoc.Sandbox.PodUID, implementerLoc.Sandbox.PodUID)
	}

	r2 := secondRuntime(t, g, func(o *Options) { o.Image = nextImage })()
	g.connectRunning(rootToken, architectLoc.Sandbox.Generation)
	g.connectRunning(implementerToken, implementerLoc.Sandbox.Generation)
	g.eventually("both launchers to report their child to the restarted daemon", func() bool {
		architectState, architectConnected := r2.launchers.state(rootToken, architectLoc.Sandbox.PodUID)
		implementerState, implementerConnected := r2.launchers.state(implementerToken, implementerLoc.Sandbox.PodUID)
		return architectConnected && architectState.Child != nil && implementerConnected && implementerState.Child != nil
	})
	machine, _, _ := restoredSandboxMachine(t, g, architect, before.Claim())
	// The architect is re-adopted at its recorded locator; the unfinished launch recorded none.
	if err := r2.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: rootToken, Locator: &architectLoc}, {Claim: implementerToken}}, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := r2.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if obs := next(t, observations); obs.Locator != architectLoc || obs.Kind != runtime.Alive {
		t.Fatalf("the re-adopted architect: %s at %+v (%s), want alive at its recorded locator", obs.Kind, obs.Locator, obs.Detail)
	}

	implementer.Generation, implementer.BootToken = 2, "boot-implementer-g2"
	relaunched, err := r2.Spawn(g.ctx, implementer)
	if err != nil {
		t.Fatalf("Spawn the implementer again: %v", err)
	}
	if relaunched.Sandbox.PodUID == architectLoc.Sandbox.PodUID {
		t.Fatalf("the implementer's launch kept the drifted pod %s", architectLoc.Sandbox.PodUID)
	}

	ended := endOf(t, observations, architectLoc)
	if ended.Locator != architectLoc || !ended.Displaced {
		t.Errorf("the architect's end: %s, Displaced %t, at %+v (%s); want it displaced at its recorded locator", ended.Kind, ended.Displaced, ended.Locator, ended.Detail)
	}
	if err := machine.Handle(g.ctx, supervise.RuntimeObservation{Observation: ended}); err != nil {
		t.Fatalf("the architect's Machine on its end: %v", err)
	}
	got := machine.Claim()
	if got.Budgets != (supervise.Budgets{}) {
		t.Fatalf("the architect's budgets after the replacement = %+v, want none charged", got.Budgets)
	}
	if got.State != supervise.StateLaunching || got.Generation != 2 || got.Locator == nil || got.Locator.Sandbox.PodUID != relaunched.Sandbox.PodUID {
		t.Fatalf("the architect after the replacement = %+v, want generation 2 launching in the new pod %s", got, relaunched.Sandbox.PodUID)
	}
	if start := lastStart(t, g, rootToken); start.Generation != 2 || start.ResumeFile != resumeSession {
		t.Fatalf("the architect's last start is generation %d resuming %q, want generation 2 resuming its session", start.Generation, start.ResumeFile)
	}
	for _, event := range []supervise.Event{
		supervise.StreamHello{Claim: rootToken, Generation: 2},
		supervise.RequestRegister{Claim: rootToken, Generation: 2, Session: "ses-tester", SessionFile: resumeSession, PluginContract: 24},
		supervise.RequestReady{Claim: rootToken, Generation: 2, Session: "ses-tester"},
	} {
		if err := machine.Handle(g.ctx, event); err != nil {
			t.Fatalf("%T for generation 2: %v", event, err)
		}
	}
	resumed := machine.Claim()
	if resumed.State != supervise.StateReady || resumed.Session != "ses-tester" || resumed.SessionFile != resumeSession {
		t.Fatalf("the architect after its resume = %+v, want ready on its kept session", resumed)
	}
	if obs, err := r2.Probe(g.ctx, *resumed.Locator); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("the resumed architect: %+v, %v; want alive in the new pod", obs, err)
	}
}

// A pod replaced for a reason other than its image — here the reviewer's launcher Secret is gone,
// so the reviewer's launcher could never authenticate in it — ends the architect's process with it
// just the same. The Sandbox names the replaced pod, the runtime reports the architect Displaced,
// and its Machine resumes its session in the new pod at once, charging no launch failure and no
// death.
func TestARoleWhosePodTheDaemonReplacedIsResumedUncharged(t *testing.T) {
	g := newRig(t, nil)
	architect := rootSpec(t)
	machine, _, _ := supervisedSandboxMachine(t, g, architect)
	first := startSupervisedSandboxRole(t, g, machine, architect, resumeSession)
	reviewerSecret := roleSecretName(first.Sandbox.Name, claim.RoleReviewer)
	if err := g.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, reviewerSecret); err != nil {
		t.Fatal(err)
	}
	reviewerLoc := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	if reviewerLoc.Sandbox.PodUID == first.Sandbox.PodUID {
		t.Fatalf("the reviewer's launch kept pod %s, whose reviewer Secret is gone", first.Sandbox.PodUID)
	}
	if named := g.sandbox(first.Sandbox.Name).Annotations[displacedAnnotation]; named != first.Sandbox.PodUID {
		t.Fatalf("the Sandbox names %q as the pod the daemon replaced, want %s", named, first.Sandbox.PodUID)
	}

	obs, err := g.r.Probe(g.ctx, *first)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Kind != runtime.Gone && obs.Kind != runtime.NotRecordedProcess || !obs.Displaced {
		t.Fatalf("the architect in the replaced pod: %s, Displaced %t (%s); want its process reported displaced", obs.Kind, obs.Displaced, obs.Detail)
	}
	if err := machine.Handle(g.ctx, supervise.RuntimeObservation{Observation: obs}); err != nil {
		t.Fatalf("the architect's Machine on %s: %v", obs.Kind, err)
	}
	got := machine.Claim()
	if got.Budgets != (supervise.Budgets{}) {
		t.Fatalf("the architect's budgets after the daemon replaced its pod = %+v, want none charged", got.Budgets)
	}
	if got.State != supervise.StateLaunching || got.Generation != 2 || got.Locator == nil || got.Locator.Sandbox.PodUID != reviewerLoc.Sandbox.PodUID {
		t.Fatalf("the architect after the daemon replaced its pod = %+v, want generation 2 launching in the new pod %s", got, reviewerLoc.Sandbox.PodUID)
	}
	if start := lastStart(t, g, rootToken); start.Generation != 2 || start.ResumeFile != resumeSession {
		t.Fatalf("the architect's last start is generation %d resuming %q, want generation 2 resuming its session", start.Generation, start.ResumeFile)
	}
}

// A Sandbox carrying no annotations when the launch reads it — its pod launched, but no generation
// of it recorded its addresses yet — gets the name of the live pod replaced in it, and an
// annotation another writer adds between that read and the suspension (the Agent Sandbox
// controller writes one of its own) stands beside it: the suspension is a merge patch of its one
// annotation key, never a write of the whole map.
func TestReplacingALivePodNamesItBesideEveryOtherAnnotation(t *testing.T) {
	const between = "example.io/written-between"
	g := newRig(t, nil)
	testerLoc := g.spawn(workerSpec(t))
	name := testerLoc.Sandbox.Name
	stored, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
	if err != nil {
		t.Fatal(err)
	}
	bare := stored.(*unstructured.Unstructured).DeepCopy()
	bare.SetAnnotations(nil)
	if err := g.dyn.Tracker().Update(sandboxGVR, bare, testNamespace); err != nil {
		t.Fatal(err)
	}
	if err := g.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, roleSecretName(name, claim.RoleReviewer)); err != nil {
		t.Fatal(err)
	}
	var wrote atomic.Bool
	g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if !modePatched(t, string(a.(k8stesting.PatchAction).GetPatch()), modeSuspended) || !wrote.CompareAndSwap(false, true) {
			return false, nil, nil
		}
		current, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
		if err != nil {
			return true, nil, err
		}
		annotated := current.(*unstructured.Unstructured).DeepCopy()
		annotated.SetAnnotations(map[string]string{between: "kept"})
		if err := g.dyn.Tracker().Update(sandboxGVR, annotated, testNamespace); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})

	reviewerLoc := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	if reviewerLoc.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("the reviewer's launch kept pod %s, whose reviewer Secret is gone", testerLoc.Sandbox.PodUID)
	}
	annotations := g.sandbox(name).Annotations
	if named := annotations[displacedAnnotation]; named != testerLoc.Sandbox.PodUID {
		t.Fatalf("the Sandbox names %q as the pod the daemon replaced, want %s", named, testerLoc.Sandbox.PodUID)
	}
	if !wrote.Load() || annotations[between] != "kept" {
		t.Fatalf("the Sandbox's annotations after the suspension = %v, want %s, written before it, kept", annotations, between)
	}
	if obs, err := g.r.Probe(g.ctx, testerLoc); err != nil || !obs.Displaced {
		t.Fatalf("the tester in the replaced pod: %+v, %v; want it displaced", obs, err)
	}
}

// A final verdict whose cause cannot be confirmed — the cached Sandbox names no pod, and reading
// the Sandbox itself fails — is Uncertain, never a death: the claim stays watched and is evaluated
// again once the read can answer.
func TestAnEndWhoseSandboxCannotBeReadIsUncertain(t *testing.T) {
	name := SandboxName(workerToken)
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeSuspended, claimLabels(claim.RoleTester))}, withoutController())
	g.dyn.PrependReactor("get", "sandboxes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})
	obs, err := g.r.Probe(g.ctx, sandboxLocator(workerToken, recorded))
	if err != nil {
		t.Fatal(err)
	}
	if obs.Kind != runtime.Uncertain || !strings.Contains(obs.Detail, "etcdserver: request timed out") || !strings.Contains(obs.Detail, "the stores report gone") {
		t.Fatalf("an end whose Sandbox cannot be read: %s (%s), want Uncertain naming the verdict and the failed read", obs.Kind, obs.Detail)
	}
}

// The pod store and the Sandbox store are fed by two informers that nothing orders, so the pod's
// end can reach the runtime before the Suspended patch that names it does. A final verdict on a
// role whose cached Sandbox does not name its pod is confirmed with one read of the Sandbox
// itself before it is reported: with the Sandbox store 300 ms behind, the tester whose pod the
// reviewer's launch replaced is still reported Displaced, never a death to charge.
func TestADisplacementIsConfirmedPastALaggingSandboxStore(t *testing.T) {
	g := newRig(t, nil, withLaggingSandboxInformer(300*time.Millisecond))
	testerLoc := g.spawn(workerSpec(t))
	if err := g.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, roleSecretName(testerLoc.Sandbox.Name, claim.RoleReviewer)); err != nil {
		t.Fatal(err)
	}
	observations, err := g.r.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := testSpec(t, otherToken, claim.RoleReviewer, testTree)
	g.launcher(reviewer.Claim)
	spawned := make(chan error, 1)
	go func() {
		_, err := g.r.Spawn(g.ctx, reviewer)
		spawned <- err
	}()

	ended := endOf(t, observations, testerLoc)
	if !ended.Displaced || ended.Locator != testerLoc {
		t.Errorf("the tester's end behind a lagging Sandbox store: %s, Displaced %t, at %+v (%s); want it displaced at its recorded locator", ended.Kind, ended.Displaced, ended.Locator, ended.Detail)
	}
	if err := <-spawned; err != nil {
		t.Fatalf("Spawn the reviewer: %v", err)
	}
}

// A pod the daemon set out to replace whose suspension the API server refused runs on, undeleted:
// the suspension and the name of the pod it replaces travel in one write, so the Sandbox names no
// pod. A role that dies there later is a death like any other, not Displaced, so a role that keeps
// dying in a pod that is never replaced still spends its launch budget.
func TestARefusedReplacementNamesNoPod(t *testing.T) {
	g := newRig(t, nil)
	architectLoc := g.spawn(rootSpec(t))
	reviewerSecret := roleSecretName(architectLoc.Sandbox.Name, claim.RoleReviewer)
	if err := g.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("secrets"), testNamespace, reviewerSecret); err != nil {
		t.Fatal(err)
	}
	g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if modePatched(t, string(a.(k8stesting.PatchAction).GetPatch()), modeSuspended) {
			return true, nil, errors.New("the server is currently unable to handle the request")
		}
		return false, nil, nil
	})
	reviewer := testSpec(t, otherToken, claim.RoleReviewer, testTree)
	g.launcher(reviewer.Claim)
	if err := failedLaunch(t, g, reviewer); err == nil || !strings.Contains(err.Error(), "suspend its sandbox") {
		t.Fatalf("Spawn the reviewer = %v, want the refused suspension", err)
	}
	if pod := g.pod(architectLoc.Sandbox.Name); pod == nil || string(pod.UID) != architectLoc.Sandbox.PodUID || pod.DeletionTimestamp != nil {
		t.Fatalf("the pod after the refused suspension = %+v, want the architect's pod %s running on", pod, architectLoc.Sandbox.PodUID)
	}
	if named, ok := g.sandbox(architectLoc.Sandbox.Name).Annotations[displacedAnnotation]; ok {
		t.Fatalf("the refused suspension left the Sandbox naming %q as a replaced pod", named)
	}

	g.reportLauncher(rootToken, shimwire.LauncherState{LastExit: &shimwire.LauncherExit{Generation: architectLoc.Sandbox.Generation, Code: 1}})
	obs, err := g.r.Probe(g.ctx, architectLoc)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Kind != runtime.Gone || obs.Displaced {
		t.Fatalf("the architect's own exit in the pod that ran on: %s, Displaced %t (%s); want a death", obs.Kind, obs.Displaced, obs.Detail)
	}
}

// Replacing a pod that already ended — it failed, or its workspace-init did — ends no process of
// the daemon's: whatever ran there ended with the pod. The Sandbox names no replaced pod, so the
// roles recorded in it are reported as the pod's own ends, never Displaced.
func TestReplacingAnEndedPodNamesNothing(t *testing.T) {
	for name, end := range map[string]func(*corev1.Pod){
		"a failed pod": func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{terminated(workerContainer, 137, "Error")}}
		},
		"a pod whose workspace-init failed": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{terminated(initContainer, 1, "Error")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			testerLoc := g.spawn(workerSpec(t))
			sandboxName := testerLoc.Sandbox.Name
			g.update(g.pod(sandboxName), end)
			g.eventually("the pod's end to reach the store", func() bool {
				pod := g.r.storedPod(sandboxName)
				_, initExit := failedInit(pod)
				return pod != nil && (terminal(pod) || initExit != nil)
			})

			reviewerLoc := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
			if reviewerLoc.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
				t.Fatalf("the reviewer's launch kept the ended pod %s", testerLoc.Sandbox.PodUID)
			}
			if named, ok := g.sandbox(sandboxName).Annotations[displacedAnnotation]; ok {
				t.Fatalf("replacing an ended pod named %q as a pod the daemon displaced roles from", named)
			}
			obs, err := g.r.Probe(g.ctx, testerLoc)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Displaced {
				t.Fatalf("the tester recorded in the ended pod: %s, Displaced (%s); want the pod's own end", obs.Kind, obs.Detail)
			}
		})
	}
}
