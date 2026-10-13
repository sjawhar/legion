package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// A daemon restarted onto another release finds an issue pod of the earlier image with its
// architect live in it, and launches another role of the issue there: a launch the previous daemon
// did not finish, here the implementer's (LEGION-729). The pod is not replaced while the architect
// runs in it, since replacing it would end the architect's process with it: the implementer starts
// in the pod as it is, and the architect is never stopped, never reported gone or not the recorded
// process, and is still alive at its recorded locator. The replacement waits for a launch that
// finds no other role live: once both are suspended, the architect's resume replaces the pod, onto
// the configured image, and resumes the architect's session there.
func TestALaunchIntoADriftedPodLeavesItsOtherLiveRoleRunning(t *testing.T) {
	g := newRig(t, nil)
	architect := rootSpec(t)
	implementer := testSpec(t, implementerToken, claim.RoleImplementer, testTree)
	architectLoc, implementerLoc := g.spawn(architect), g.spawn(implementer)
	name := architectLoc.Sandbox.Name
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
	// The architect is re-adopted at its recorded locator; the implementer's unfinished launch
	// recorded none.
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
	var mu sync.Mutex
	var later []runtime.Observation
	collecting, stop := context.WithCancel(g.ctx)
	defer stop()
	go func() {
		for {
			select {
			case obs := <-observations:
				mu.Lock()
				later = append(later, obs)
				mu.Unlock()
			case <-collecting.Done():
				return
			}
		}
	}()

	implementer.Generation, implementer.BootToken = 2, "boot-implementer-g2"
	relaunched, err := r2.Spawn(g.ctx, implementer)
	if err != nil {
		t.Fatalf("Spawn the implementer again: %v", err)
	}
	if relaunched.Sandbox.PodUID != architectLoc.Sandbox.PodUID {
		t.Fatalf("the implementer's launch replaced pod %s with %s while the architect was live in it", architectLoc.Sandbox.PodUID, relaunched.Sandbox.PodUID)
	}
	if start := lastStart(t, g, implementerToken); start.Generation != 2 {
		t.Fatalf("the implementer's last start is generation %d, want 2", start.Generation)
	}
	if obs, err := r2.Probe(g.ctx, architectLoc); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("the architect after its sibling's launch: %+v, %v; want alive at its recorded locator", obs, err)
	}
	for _, frame := range g.sent(rootToken) {
		if command, ok := frame.(shimwire.LauncherStop); ok {
			t.Fatalf("the architect's launcher was sent %+v, want its process left running", command)
		}
	}
	time.Sleep(50 * time.Millisecond)
	stop()
	mu.Lock()
	for _, obs := range later {
		if obs.Kind == runtime.Gone || obs.Kind == runtime.NotRecordedProcess {
			t.Errorf("%s was reported %s (%s)", obs.Locator.Claim, obs.Kind, obs.Detail)
		}
	}
	mu.Unlock()

	if err := r2.Suspend(g.ctx, relaunched); err != nil {
		t.Fatalf("Suspend the implementer: %v", err)
	}
	if err := r2.Suspend(g.ctx, architectLoc); err != nil {
		t.Fatalf("Suspend the architect: %v", err)
	}
	architect.Generation, architect.BootToken, architect.ResumeSessionFile = 2, "boot-architect-g2", resumeSession
	resumed, err := r2.Resume(g.ctx, &architectLoc, architect)
	if err != nil {
		t.Fatalf("Resume the architect: %v", err)
	}
	if resumed.Sandbox.PodUID == architectLoc.Sandbox.PodUID {
		t.Fatalf("the architect's resume, with no other role live, kept the drifted pod %s", architectLoc.Sandbox.PodUID)
	}
	if other, drifted := imageDrift(g.pod(name), nextImage); drifted {
		t.Fatalf("the replaced pod has a container on image %s, want every container on the configured %s", other, nextImage)
	}
	if start := lastStart(t, g, rootToken); start.Generation != 2 || start.ResumeFile != resumeSession {
		t.Fatalf("the architect's last start is generation %d resuming %q, want generation 2 resuming its session", start.Generation, start.ResumeFile)
	}
}

// A pod the daemon replaces to launch one role, for a reason no other role's life defers — here
// the reviewer's launcher Secret is gone, so the reviewer's launcher could never authenticate in
// it — ends the architect's process with it. That is no failure of the architect's: the runtime
// reports the process ended with a pod the daemon replaced, and the architect's Machine resumes its
// session in the new pod at once, charging no launch failure and no death.
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

	obs, err := g.r.Probe(g.ctx, *first)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Kind != runtime.Gone && obs.Kind != runtime.NotRecordedProcess {
		t.Fatalf("the architect in the replaced pod: %s (%s), want its process reported ended", obs.Kind, obs.Detail)
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

// A pod the daemon set out to replace whose suspension the API server refused runs on, undeleted:
// nothing of the daemon's ended the roles in it. A role that dies there later is a death like any
// other, reported without PodReplaced, so a role that keeps dying in a pod that is never replaced
// still spends its launch budget rather than being relaunched uncharged for ever.
func TestAPodWhoseReplacingSuspensionFailedReportsALaterDeathAsADeath(t *testing.T) {
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

	g.reportLauncher(rootToken, shimwire.LauncherState{LastExit: &shimwire.LauncherExit{Generation: architectLoc.Sandbox.Generation, Code: 1}})
	obs, err := g.r.Probe(g.ctx, architectLoc)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Kind != runtime.Gone || obs.PodReplaced {
		t.Fatalf("the architect's own exit in the pod that ran on: %s, PodReplaced %t (%s); want a death", obs.Kind, obs.PodReplaced, obs.Detail)
	}
}
