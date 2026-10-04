package sandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func TestRestartingInitFailureIsGoneForEveryRole(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminated", true: "backoff"}[waiting], func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(workerSpec(t))
			g.update(g.pod(loc.Sandbox.Name), func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				failed := &corev1.ContainerStateTerminated{ExitCode: 3, Reason: "Error"}
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
			if err != nil || observation.Kind != runtime.Gone || !observation.WorkspaceLost || !strings.Contains(observation.Detail, initContainer) {
				t.Fatalf("restarting init = %+v, %v; want Gone with workspace loss and init detail", observation, err)
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
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, Reason: "Error"}},
		}}
	})
	launched := <-result
	if launched.err != nil {
		t.Fatalf("init failure was hidden as a launcher timeout: %v", launched.err)
	}
	obs, err := g.r.Probe(g.ctx, launched.loc)
	if err != nil || obs.Kind != runtime.Gone || !obs.WorkspaceLost {
		t.Fatalf("supervisor cannot observe confirmed storage loss: %+v, %v", obs, err)
	}
}
