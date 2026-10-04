package sandbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// logTailLines is how much of a failed container's log a Gone quotes (runtime-kubernetes.ts:678-689).
const logTailLines = 20

// workspaceLostExitCode is workspace-init's exit code for a tree volume that lost its clone and
// the session being resumed (decision 11): a Gone that says so carries Observation.WorkspaceLost.
const workspaceLostExitCode = 3

// view is what the stores hold for one claim's name: its Sandbox, the pod the Sandbox owns, and a
// same-named pod it does not own, which is no process of the claim's.
type view struct {
	sandbox  *sandbox
	pod      *corev1.Pod
	stranger *corev1.Pod
}

// view reads the claim's Sandbox and pod from the stores. New synced both before it returned, and
// they stay current while New's context lives and each informer's latest list or watch request
// succeeded; while either's failed, the store holds what it last heard, so view refuses to answer
// from it and evaluate answers Uncertain.
func (r *Runtime) view(name string) (view, error) {
	for _, f := range []*feed{&r.sandboxFeed, &r.podFeed} {
		if err := f.check(); err != nil {
			return view{}, err
		}
	}
	s, err := r.storedSandbox(name)
	if err != nil || s == nil {
		return view{}, err
	}
	v := view{sandbox: s, pod: r.storedPod(name)}
	if v.pod != nil && !ownedBy(v.pod, s.UID) {
		v.stranger, v.pod = v.pod, nil
	}
	return v, nil
}

// Probe is one observation of loc now, from the stores. A Sandbox the store holds but the runtime
// cannot decode is an Uncertain observation, never an error, and never a death.
func (r *Runtime) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	if err := r.checkLocator(loc); err != nil {
		return runtime.Observation{}, err
	}
	return r.evaluate(ctx, loc), nil
}

// evaluate is the observation mapping for one recorded role process. Pod loss applies to every
// role in the issue, but a live pod is judged through the recorded role container: Ready changes
// during a neighbour's container restart and is never a whole-pod death verdict.
func (r *Runtime) evaluate(ctx context.Context, loc runtime.Locator) runtime.Observation {
	observe := func(kind runtime.ObservationKind, format string, args ...any) runtime.Observation {
		return runtime.Observation{Locator: loc, Kind: kind, At: r.now(), Detail: fmt.Sprintf(format, args...)}
	}
	name := loc.Sandbox.Name
	v, err := r.view(name)
	switch {
	case err != nil:
		return observe(runtime.Uncertain, "%v", err)
	case v.sandbox == nil:
		return observe(runtime.Gone, "sandbox %s/%s is absent", r.namespace, name)
	case v.pod == nil:
		return observe(runtime.Gone, "%s", podAbsent(v))
	case string(v.pod.UID) != loc.Sandbox.PodUID:
		return observe(runtime.NotRecordedProcess, "pod %s is uid %s, not the recorded pod UID %s", name, v.pod.UID, loc.Sandbox.PodUID)
	}
	pod := v.pod
	initName, initExit := failedInit(pod)
	if terminal(pod) || initExit != nil {
		detail, workspaceLost := r.ended(ctx, v, loc.Sandbox.Container, initName, initExit)
		ended := observe(runtime.Gone, "%s", detail)
		ended.WorkspaceLost = workspaceLost
		return ended
	}
	if scheduled := podCondition(pod, corev1.PodScheduled); pod.Status.Phase == corev1.PodPending && scheduled != nil &&
		scheduled.Status == corev1.ConditionFalse {
		since := scheduled.LastTransitionTime.Time
		if since.IsZero() {
			since = pod.CreationTimestamp.Time
		}
		if unscheduled := r.now().Sub(since); unscheduled > r.bootTimeout {
			return observe(runtime.Gone, "pod %s (uid %s) unscheduled for %s, past the boot timeout %s (PodScheduled=False %s: %s); events: %s",
				name, pod.UID, unscheduled.Round(time.Second), r.bootTimeout, scheduled.Reason, scheduled.Message, r.events(ctx, pod))
		}
	}
	if ready := v.sandbox.condition(conditionReady); ready != nil &&
		(ready.Reason == reasonMultiplePods || ready.Reason == reasonReconcilerError) {
		return observe(runtime.Uncertain, "sandbox %s Ready=%s %s: %s (pod uid %s)", name, ready.Status, ready.Reason, ready.Message, pod.UID)
	}
	switch pod.Status.Phase {
	case corev1.PodPending, "":
		return observe(runtime.Alive, "pod %s (uid %s) %s", name, pod.UID, phaseOf(pod))
	case corev1.PodRunning:
	default:
		return observe(runtime.Uncertain, "pod %s (uid %s) phase %s", name, pod.UID, pod.Status.Phase)
	}
	status := containerStatus(pod, loc.Sandbox.Container)
	if status == nil {
		return observe(runtime.Uncertain, "pod %s (uid %s) Running has no status for role container %s", name, pod.UID, loc.Sandbox.Container)
	}
	if ended := status.State.Terminated; ended != nil {
		return observe(runtime.Gone, "pod %s (uid %s) role container %s terminated (%s, exit code %d); last lines:\n%s",
			name, pod.UID, loc.Sandbox.Container, ended.Reason, ended.ExitCode, r.logTail(ctx, name, loc.Sandbox.Container))
	}
	state, connected := r.launchers.state(loc.Claim, loc.Sandbox.PodUID)
	if !connected {
		return observe(runtime.Uncertain, "pod %s (uid %s) role launcher %s is disconnected", name, pod.UID, loc.Sandbox.Container)
	}
	switch {
	case state.Child != nil && state.Child.Generation == loc.Sandbox.Generation:
		return observe(runtime.Alive, "pod %s (uid %s) role container %s runs generation %d", name, pod.UID, loc.Sandbox.Container, state.Child.Generation)
	case state.Child != nil:
		return observe(runtime.NotRecordedProcess, "pod %s (uid %s) role container %s runs generation %d, not the recorded %d",
			name, pod.UID, loc.Sandbox.Container, state.Child.Generation, loc.Sandbox.Generation)
	case state.LastExit != nil && state.LastExit.Generation == loc.Sandbox.Generation:
		return observe(runtime.Gone, "pod %s (uid %s) role container %s reports generation %d exited (code %d, signal %s)",
			name, pod.UID, loc.Sandbox.Container, state.LastExit.Generation, state.LastExit.Code, state.LastExit.Signal)
	case state.LastExit != nil:
		return observe(runtime.NotRecordedProcess, "pod %s (uid %s) role container %s last exited generation %d, not the recorded %d",
			name, pod.UID, loc.Sandbox.Container, state.LastExit.Generation, loc.Sandbox.Generation)
	default:
		return observe(runtime.Gone, "pod %s (uid %s) role launcher %s reports no child", name, pod.UID, loc.Sandbox.Container)
	}
}

// podAbsent describes the Sandbox mode, its current Suspended condition, and any same-named
// pod the Sandbox does not own.
func podAbsent(v view) string {
	s := v.sandbox
	detail := fmt.Sprintf("no pod of sandbox %s (operatingMode %s", s.Name, s.mode())
	if suspended := s.condition(conditionSuspended); suspended != nil {
		detail += fmt.Sprintf("; Suspended=%s %s", suspended.Status, suspended.Reason)
	}
	detail += ")"
	if v.stranger != nil {
		detail += fmt.Sprintf("; pod %s (uid %s) holds the name but is not controller-owned by the sandbox (uid %s)",
			v.stranger.Name, v.stranger.UID, s.UID)
	}
	return detail
}

// ended is the terminal pod verdict. A failed init container applies to every role; otherwise the
// locator's role container is the process that ended.
func (r *Runtime) ended(ctx context.Context, v view, roleContainer, initName string, initExit *corev1.ContainerStateTerminated) (string, bool) {
	pod := v.pod
	kind, container, state := "role", roleContainer, initExit
	if initExit != nil {
		kind, container = "init", initName
	}
	if state == nil {
		if status := containerStatus(pod, roleContainer); status != nil {
			state = status.State.Terminated
		}
	}
	var detail strings.Builder
	workspaceLost := kind == "init" && container == initContainer && state != nil && state.ExitCode == workspaceLostExitCode
	if workspaceLost {
		detail.WriteString("the tree volume was lost: ")
	}
	fmt.Fprintf(&detail, "pod %s (uid %s) %s", pod.Name, pod.UID, pod.Status.Phase)
	if state != nil {
		fmt.Fprintf(&detail, ": %s container %s terminated (%s, exit code %d)", kind, container, state.Reason, state.ExitCode)
	} else if pod.Status.Reason != "" {
		fmt.Fprintf(&detail, ": %s: %s", pod.Status.Reason, pod.Status.Message)
	}
	if finished := v.sandbox.condition(conditionFinished); finished != nil {
		fmt.Fprintf(&detail, "; sandbox Finished=%s %s", finished.Status, finished.Reason)
	}
	fmt.Fprintf(&detail, "; last lines of %s:\n%s", container, r.logTail(ctx, pod.Name, container))
	return detail.String(), workspaceLost
}

// failedInit includes a failed attempt the kubelet is restarting under restartPolicy Always.
// A later successful termination wins over LastTerminationState.
func failedInit(pod *corev1.Pod) (string, *corev1.ContainerStateTerminated) {
	if pod == nil {
		return "", nil
	}
	for _, status := range pod.Status.InitContainerStatuses {
		ended := status.State.Terminated
		if ended == nil {
			ended = status.LastTerminationState.Terminated
		}
		if ended != nil && ended.ExitCode != 0 {
			return status.Name, ended
		}
	}
	return "", nil
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// logTail is a container's last log lines, or why they could not be read: a detail's quote never
// changes its verdict.
func (r *Runtime) logTail(ctx context.Context, pod, container string) string {
	reading, cancel := call(ctx)
	defer cancel()
	lines := int64(logTailLines)
	stream, err := r.kube.CoreV1().Pods(r.namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &lines}).Stream(reading)
	if err != nil {
		return fmt.Sprintf("(the log could not be read: %v)", err)
	}
	defer stream.Close()
	text, err := io.ReadAll(io.LimitReader(stream, 64<<10))
	if err != nil {
		return fmt.Sprintf("(the log could not be read: %v)", err)
	}
	return strings.TrimRight(string(text), "\n")
}

// events are the pod's events, "<type> <reason>: <message>" joined by " | ", or why they could
// not be read.
func (r *Runtime) events(ctx context.Context, pod *corev1.Pod) string {
	events, err := r.podEvents(ctx, pod)
	if err != nil {
		return fmt.Sprintf("(the events could not be read: %v)", err)
	}
	parts := make([]string, 0, len(events))
	for _, event := range events {
		parts = append(parts, fmt.Sprintf("%s %s: %s", event.Type, event.Reason, event.Message))
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, " | ")
}

// podEvents lists the events the API server holds for pod, this incarnation's only.
func (r *Runtime) podEvents(ctx context.Context, pod *corev1.Pod) ([]corev1.Event, error) {
	reading, cancel := call(ctx)
	defer cancel()
	selector := fields.Set{"involvedObject.kind": "Pod", "involvedObject.name": pod.Name, "involvedObject.uid": string(pod.UID)}
	list, err := r.kube.CoreV1().Events(r.namespace).List(reading, metav1.ListOptions{FieldSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func podCondition(pod *corev1.Pod, kind corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == kind {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func phaseOf(pod *corev1.Pod) corev1.PodPhase {
	if pod.Status.Phase == "" {
		return corev1.PodPending
	}
	return pod.Status.Phase
}
