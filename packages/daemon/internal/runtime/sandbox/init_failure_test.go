package sandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A pod whose workspace-init ended non-zero is Gone for every role, and says so: no launcher could
// have started in it. The mapping classifies it a failed init, whatever the exit code, which is
// what keeps it out of ends() and so out of the displaced reading.
func TestRestartingInitFailureIsGoneForEveryRole(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminated", true: "backoff"}[waiting], func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(workerSpec(t))
			g.update(g.pod(loc.Sandbox.Name), func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				failed := &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}
				status := corev1.ContainerStatus{Name: initContainer}
				if waiting {
					status.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
					status.LastTerminationState.Terminated = failed
				} else {
					status.State.Terminated = failed
				}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
			})
			g.eventually("the informer to see the restarting init", func() bool {
				p := g.r.storedPod(loc.Sandbox.Name)
				return p != nil && p.Status.Phase == corev1.PodPending
			})
			observation, err := g.r.Probe(g.ctx, loc)
			if err != nil || observation.Kind != runtime.Gone || !strings.Contains(observation.Detail, initContainer) {
				t.Fatalf("restarting init = %+v, %v; want Gone with the failed init and its detail", observation, err)
			}
			if !g.initFailed(loc) {
				t.Fatalf("restarting init = %+v; want it classified a failed init", observation)
			}
		})
	}
}

func TestRelaunchReplacesAPodWhoseInitKeepsFailing(t *testing.T) {
	g := newRig(t, nil)
	old := g.spawn(workerSpec(t))
	if err := g.r.Suspend(g.ctx, old); err != nil {
		t.Fatal(err)
	}
	g.update(g.pod(old.Sandbox.Name), func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: initContainer,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}
	})
	g.eventually("the informer to see the failed init", func() bool { return g.r.storedPod(old.Sandbox.Name).Status.Phase == corev1.PodPending })
	spec := workerSpec(t)
	spec.Generation = 2
	loc := g.spawn(spec)
	if loc.Sandbox.PodUID == old.Sandbox.PodUID {
		t.Fatal("relaunch reused a pod whose init cannot start a launcher")
	}
}

func TestColdInitFailureReachesSupervisionWithoutWaitingForLauncher(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false)
	type launchResult struct {
		loc runtime.Locator
		err error
	}
	result := make(chan launchResult, 1)
	spec := workerSpec(t)
	go func() { loc, err := g.r.Spawn(g.ctx, spec); result <- launchResult{loc, err} }()
	name := SandboxName(workerToken)
	g.eventually("the new Pending pod", func() bool { return g.pod(name) != nil })
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: initContainer,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}
	})
	launched := <-result
	if launched.err != nil {
		t.Fatalf("init failure was hidden as a launcher timeout: %v", launched.err)
	}
	obs, err := g.r.Probe(g.ctx, launched.loc)
	if err != nil || obs.Kind != runtime.Gone || !g.initFailed(launched.loc) {
		t.Fatalf("supervisor cannot observe the confirmed init failure: %+v, %v", obs, err)
	}
}

// The kubelet restarts a failed init container by itself under restartPolicy Always, and a
// workspace-fetch that comes back carries on with the feed its own pod's disk already holds. So
// each of its first three restarts is the pod still provisioning — to failedInit, which an
// observation reads a pod's death out of, and to provisioning, which a launch's wait reads a pod
// coming up by, alike — and the fourth restart is the pod's death. workspace-init is given no such
// grace, and neither is a fetch in a pod that has already failed, where nothing is started again.
// Reading a restart as a death threw away a download that was still arriving and launched the
// issue again into the same wait (INFRA-495).
func TestAFetchsFirstThreeRestartsAreStartingAndItsFourthIsAFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		container  string
		restarts   int32
		terminated bool
		phase      corev1.PodPhase
		dead       bool
	}{
		{name: "a fetch that just ended, before the kubelet restarts it", container: fetchContainer, terminated: true},
		{name: "a fetch's first restart", container: fetchContainer, restarts: 1},
		{name: "a fetch's third restart", container: fetchContainer, restarts: 3},
		{name: "a fetch's fourth restart", container: fetchContainer, restarts: 4, dead: true},
		{name: "a fetch restarting in a pod that has failed", container: fetchContainer, restarts: 1, phase: corev1.PodFailed, dead: true},
		{name: "workspace-init's first failure", container: initContainer, terminated: true, dead: true},
		{name: "workspace-init restarting", container: initContainer, restarts: 1, dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase := corev1.PodPending
			if tc.phase != "" {
				phase = tc.phase
			}
			ended := &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}
			status := corev1.ContainerStatus{Name: tc.container, RestartCount: tc.restarts}
			if tc.terminated {
				status.State.Terminated = ended
			} else {
				status.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
				status.LastTerminationState.Terminated = ended
			}
			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: phase, InitContainerStatuses: []corev1.ContainerStatus{status}}}

			named, failed := failedInit(pod)
			if (failed != nil) != tc.dead {
				t.Errorf("failedInit = %q, %+v; want the pod dead %t", named, failed, tc.dead)
			}
			if tc.dead && named != tc.container {
				t.Errorf("failedInit named %q, want the pod's %s", named, tc.container)
			}
			if got := provisioning(pod); got == tc.dead {
				t.Errorf("provisioning = %t while failedInit reads the pod dead %t: the two readings of %s disagree", got, tc.dead, tc.container)
			}
		})
	}
}
