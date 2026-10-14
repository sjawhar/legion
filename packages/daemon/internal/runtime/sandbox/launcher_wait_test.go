package sandbox

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fetching is a pod's status while workspace-fetch runs: Pending, workspace-init not started.
func fetching(p *corev1.Pod) {
	p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
		{Name: fetchContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{Name: initContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	}}
}

// pulling is a pod's status before any init container has started: an image pull.
func pulling(p *corev1.Pod) {
	p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
		{Name: fetchContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
		{Name: initContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	}}
}

// pendingPod waits, as long as a loaded machine needs, for the pod named name that the launch
// under test makes, sets its status with status, and returns when it first saw the pod: the wait
// for the pod's launcher begins once the runtime's store holds it, so no earlier.
func (g *rig) pendingPod(name string, status func(*corev1.Pod)) time.Time {
	g.t.Helper()
	testwait.Eventually(g.t, "the new Pending pod", func() bool { return g.pod(name) != nil })
	appeared := time.Now()
	g.update(g.pod(name), status)
	return appeared
}

// A pod's role launchers start only once its init containers have finished, and workspace-fetch's
// clone, with its retries of a transient failure, may run past the boot timeout. A launch whose pod
// is still provisioning then keeps waiting for its launcher, and starts the role in that same pod
// once the launcher connects: the claim is charged no launch failure.
func TestALaunchWaitsForAPodProvisioningPastTheBootTimeout(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	spec := workerSpec(t)
	machine, _, _ := supervisedSandboxMachine(t, g, spec)
	launched := make(chan error, 1)
	go func() { launched <- machine.Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}) }()
	name := SandboxName(spec.Claim)
	g.pendingPod(name, fetching)
	uid := g.pod(name).UID

	select {
	case err := <-launched:
		t.Fatalf("the launch ended while its pod was still in workspace-fetch: %v", err)
	case <-time.After(g.opts.BootTimeout + time.Second):
	}
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = runningStatusOf(p.Spec) })
	g.launcher(spec.Claim)
	select {
	case err := <-launched:
		if err != nil {
			t.Fatalf("the launch failed once its pod had provisioned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the launch did not start the role once its launcher connected")
	}
	got := machine.Claim()
	if got.Budgets.LaunchFailures != 0 || got.Locator == nil || got.Locator.Sandbox.PodUID != string(uid) {
		t.Fatalf("claim after the launch = %+v, want it launched in pod %s with no launch failure", got, uid)
	}
	if !slices.ContainsFunc(g.sent(spec.Claim), func(frame shimwire.Frame) bool {
		start, ok := frame.(shimwire.LauncherStart)
		return ok && start.Generation == got.Generation
	}) {
		t.Fatalf("the launcher was sent %v, want the generation %d start", g.sent(spec.Claim), got.Generation)
	}
}

// The wait provisioning extends is still bounded once provisioning ends: the launcher then has the
// boot timeout to connect, and a launch whose launcher never does fails then, not at the end of
// ProvisionBound.
func TestALaunchGivesUpTheBootTimeoutAfterItsPodStopsProvisioning(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	result := make(chan error, 1)
	go func() { _, err := g.r.Spawn(g.ctx, workerSpec(t)); result <- err }()
	name := SandboxName(workerToken)
	appeared := g.pendingPod(name, fetching)
	time.Sleep(g.opts.BootTimeout + time.Second)
	provisioned := time.Now()
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = runningStatusOf(p.Spec) })

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "wait for its role launcher: timed out after") {
			t.Fatalf("Spawn = %v, want it timed out waiting for the role launcher", err)
		}
		if waited := time.Since(provisioned); waited < g.opts.BootTimeout-2*recheckInterval {
			t.Fatalf("the launch gave up %s after its pod provisioned, want about the boot timeout %s", waited, g.opts.BootTimeout)
		}
		if waited := time.Since(appeared); waited < 2*g.opts.BootTimeout {
			t.Fatalf("the launch gave up %s after its pod appeared, want the boot timeout counted from the end of provisioning", waited)
		}
	case <-time.After(g.opts.BootTimeout + 3*time.Second):
		t.Fatal("the launch still waits for a launcher the boot timeout after its pod provisioned")
	}
}

// A pod that never runs an init container, as one whose image pull never finishes, is not
// provisioning: its launch gives up at the boot timeout.
func TestALaunchWhosePodNeverProvisionsGivesUpAtTheBootTimeout(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	result := make(chan error, 1)
	go func() { _, err := g.r.Spawn(g.ctx, workerSpec(t)); result <- err }()
	name := SandboxName(workerToken)
	g.pendingPod(name, pulling)

	select {
	case err := <-result:
		want := "wait for its role launcher: timed out after " + g.opts.BootTimeout.String() + " waiting for role launcher " + string(workerToken)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Spawn = %v, want %q", err, want)
		}
	case <-time.After(g.opts.BootTimeout + 2*time.Second):
		t.Fatal("the launch waited past the boot timeout for a pod that never provisioned")
	}
}

// The launcher's deadline is the boot timeout after the pod was last seen provisioning, and never
// past the boot timeout plus ProvisionBound from the first time it was. The last row is the edge a
// cap counted from the wait's start got wrong: a 60 s image pull, a fetch that ran its whole
// 30 minutes, then a 90 s workspace-init, whose launcher connects 32 min 30 s after the wait began.
func TestTheLauncherDeadlineFollowsProvisioningUpToItsBound(t *testing.T) {
	start := time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)
	boot, bound := 2*time.Minute, 30*time.Minute
	for _, tc := range []struct {
		name        string
		first, last time.Duration
		want        time.Duration
	}{
		{"provisioning at the start", 0, 0, boot},
		{"provisioning ten minutes in", 0, 10 * time.Minute, 10*time.Minute + boot},
		{"provisioning right up to the bound", 0, bound, bound + boot},
		{"provisioning past the bound", 0, bound + time.Minute, bound + boot},
		{"a pull, a whole fetch, then workspace-init", time.Minute, 32*time.Minute + 30*time.Second, time.Minute + bound + boot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := launcherDeadline(start.Add(tc.first), start.Add(tc.last), boot, bound).Sub(start); got != tc.want {
				t.Fatalf("launcherDeadline = start + %s, want start + %s", got, tc.want)
			}
		})
	}
}

// The cap is counted from the first time the wait sees the pod provisioning, not from the wait's
// start: an image pull before workspace-fetch does not eat into the fetch's bound. With a 2 s boot
// timeout and a 3 s bound, the pod pulls for 1.5 s and provisions until 5.5 s after it appeared,
// past the 5 s a cap from the wait's start would allow; its launcher connects then, and the launch
// starts the role.
func TestALaunchCountsItsProvisioningBoundFromTheFirstInitContainer(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	g.r.provisionBound = 3 * time.Second
	spec := workerSpec(t)
	result := make(chan error, 1)
	go func() { _, err := g.r.Spawn(g.ctx, spec); result <- err }()
	name := SandboxName(spec.Claim)
	appeared := g.pendingPod(name, pulling)
	time.Sleep(time.Until(appeared.Add(1500 * time.Millisecond)))
	g.update(g.pod(name), fetching)
	time.Sleep(time.Until(appeared.Add(5500 * time.Millisecond)))
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = runningStatusOf(p.Spec) })
	g.launcher(spec.Claim)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Spawn = %v %s after its pod appeared, want the role started", err, time.Since(appeared).Round(time.Millisecond))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the launch did not start the role once its launcher connected")
	}
}
