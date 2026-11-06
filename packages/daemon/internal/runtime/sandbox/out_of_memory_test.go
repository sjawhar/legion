package sandbox

// What the kubelet's OOMKilled end becomes on the observation the supervisor reads
// (runtime.Observation.OutOfMemory), which is what sends a role its task behind the note saying
// its pod ran out of memory. The mark is the issue pod's one container's end and no other's.

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// outOfMemoryEnd and erroredEnd are the two ends that tell the reason from the exit code: the
// kubelet's OOMKilled, and an ordinary error carrying the same code.
var (
	outOfMemoryEnd = corev1.ContainerStateTerminated{ExitCode: 128, Reason: "OOMKilled"}
	erroredEnd     = corev1.ContainerStateTerminated{ExitCode: 128, Reason: "Error"}
)

// endedContainer is a container as the kubelet reports it the moment it ends, and
// restartedContainer once the kubelet has started it again over that same end.
func endedContainer(name string, end corev1.ContainerStateTerminated) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &end}}
}

func restartedContainer(name string, end corev1.ContainerStateTerminated) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &end},
	}
}

func oneContainerStatus(phase corev1.PodPhase, container corev1.ContainerStatus) corev1.PodStatus {
	return corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{container}}
}

// An issue pod's one container ending for want of memory is marked on the Gone of every role
// whose turn that end cut off (Observation.OutOfMemory), which is what the daemon tells each of
// them why its turn stopped from. The kubelet's reason is the whole of what says so, read
// wherever the end is: off the ended container itself, and off the LastTerminationState of one
// the kubelet has already restarted, whose role launcher reports no child and so died with that
// instance. A launcher that reports the recorded generation's own exit has been running since
// before that generation started, so the container did not end under it and its role's process
// ended on its own: no mark. Neither does an end whose reason is Error, which carries 128, the
// very exit code an out-of-memory end carries — the reason decides and the code never does.
func TestTheRolesContainerOutOfMemoryEndIsMarkedOnTheRolesItCut(t *testing.T) {
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	ended := func(end corev1.ContainerStateTerminated) corev1.ContainerStatus {
		return endedContainer(runtime.IssuePodContainer, end)
	}
	restarted := func(end corev1.ContainerStateTerminated) corev1.ContainerStatus {
		return restartedContainer(runtime.IssuePodContainer, end)
	}
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeRunning, labels),
		podObject(name, recorded, sandboxUID, labels, oneContainerStatus(corev1.PodRunning, ended(outOfMemoryEnd)))}, withoutController())
	for _, tc := range []struct {
		name      string
		phase     corev1.PodPhase
		container corev1.ContainerStatus
		launcher  *shimwire.LauncherState
		want      bool
	}{
		{"the container was ended OOMKilled", corev1.PodRunning, ended(outOfMemoryEnd), nil, true},
		{"the kubelet restarted it and the role's launcher reports no child", corev1.PodRunning, restarted(outOfMemoryEnd), &shimwire.LauncherState{}, true},
		{"the pod itself ended with its container OOMKilled", corev1.PodFailed, ended(outOfMemoryEnd), nil, true},
		{"the role's launcher reports the recorded generation's own exit", corev1.PodRunning, restarted(outOfMemoryEnd),
			&shimwire.LauncherState{LastExit: &shimwire.LauncherExit{Generation: 1, Code: 1}}, false},
		{"the container was ended with reason Error at the same exit code", corev1.PodRunning, ended(erroredEnd), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g.update(g.pod(name), func(p *corev1.Pod) { p.Status = oneContainerStatus(tc.phase, tc.container) })
			g.eventually("the pod store to see "+tc.name, func() bool {
				pod := g.r.storedPod(name)
				if pod == nil || pod.Status.Phase != tc.phase {
					return false
				}
				status := containerStatus(pod, runtime.IssuePodContainer)
				return status != nil && reflect.DeepEqual(*status, tc.container)
			})
			if tc.launcher != nil {
				g.reportLauncher(workerToken, *tc.launcher)
			}
			obs, err := g.r.Probe(g.ctx, rolesLocator(workerToken, recorded))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if obs.Kind != runtime.Gone || obs.OutOfMemory != tc.want {
				t.Fatalf("%s OutOfMemory=%t %q, want Gone OutOfMemory=%t", obs.Kind, obs.OutOfMemory, obs.Detail, tc.want)
			}
		})
	}
}

// The mark is the issue pod's one container's end and no other's, because the note it sends is
// written for a role of an issue pod: it says the container ran every role of the issue, that
// another role's work may have filled the pod, and that an issue pod runs no heavy build. An init
// container's out-of-memory end cut no turn off, since it ended before any role's process
// started; and the project controller's own pod runs the controller alone in a container of its
// own, sharing its memory with nobody and reading no worker guide. The controller is driven
// through both verdicts that read a live pod's container: the container's own terminated state,
// and the LastTerminationState a restarted container leaves, which is where a real kubelet
// restart lands. All are Gone, and none is marked. A pod of the layout before one container is
// the third exclusion, and its own test holds it
// (TestAnEndedPodOfTheEarlierLayoutReportsTheRolesOwnContainer).
func TestAnOutOfMemoryEndOutsideTheRolesContainerIsNotMarked(t *testing.T) {
	t.Run("an init container", func(t *testing.T) {
		g := newRig(t, nil)
		loc := g.spawn(workerSpec(t))
		g.update(g.pod(loc.Sandbox.Name), func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{endedContainer(initContainer, outOfMemoryEnd)}
		})
		g.eventually("the pod store to see the failed init", func() bool {
			p := g.r.storedPod(loc.Sandbox.Name)
			if p == nil {
				return false
			}
			_, failed := failedInit(p)
			return failed != nil
		})
		obs, err := g.r.Probe(g.ctx, loc)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if obs.Kind != runtime.Gone || obs.OutOfMemory {
			t.Fatalf("%s OutOfMemory=%t %q, want Gone unmarked: the init container ran before any role's process", obs.Kind, obs.OutOfMemory, obs.Detail)
		}
	})
	for _, tc := range []struct {
		name      string
		container corev1.ContainerStatus
	}{
		{"the controller's own container, ended", endedContainer(runtime.ControllerPodContainer, outOfMemoryEnd)},
		{"the controller's own container, restarted by the kubelet", restartedContainer(runtime.ControllerPodContainer, outOfMemoryEnd)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(controllerSpec(t))
			g.update(g.pod(loc.Sandbox.Name), func(p *corev1.Pod) {
				p.Status = oneContainerStatus(corev1.PodRunning, tc.container)
			})
			g.eventually("the pod store to see "+tc.name, func() bool {
				p := g.r.storedPod(loc.Sandbox.Name)
				if p == nil {
					return false
				}
				status := containerStatus(p, runtime.ControllerPodContainer)
				return status != nil && reflect.DeepEqual(*status, tc.container)
			})
			// Its launcher reports no child, as one the kubelet started with the container does:
			// the ended case is then read off the container's own terminated state, and the
			// restarted case off the LastTerminationState, the branch a real restart lands in.
			g.reportLauncher(controllerToken, shimwire.LauncherState{})
			obs, err := g.r.Probe(g.ctx, loc)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if obs.Kind != runtime.Gone || obs.OutOfMemory {
				t.Fatalf("%s OutOfMemory=%t %q, want Gone unmarked: the controller's pod runs no role of an issue", obs.Kind, obs.OutOfMemory, obs.Detail)
			}
		})
	}
}
