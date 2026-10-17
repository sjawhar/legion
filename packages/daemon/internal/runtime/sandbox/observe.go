package sandbox

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// logTailLines is how much of a failed container's log a Gone quotes (runtime-kubernetes.ts:678-689).
const logTailLines = 20

// workspaceLostExitCode is workspace-init's exit code for an issue's volume that lost its clone and
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
	if err != nil {
		return view{}, err
	}
	return r.viewOf(name, s), nil
}

// viewOf is the view of s, the Sandbox named name, beside the pod the pod store holds by that name.
func (r *Runtime) viewOf(name string, s *sandbox) view {
	if s == nil {
		return view{}
	}
	v := view{sandbox: s, pod: r.storedPod(name)}
	if v.pod != nil && !ownedBy(v.pod, s.UID) {
		v.stranger, v.pod = v.pod, nil
	}
	return v
}

// Probe is one observation of loc now, from the stores. A Sandbox the store holds but the runtime
// cannot decode is an Uncertain observation, never an error, and never a death.
func (r *Runtime) Probe(ctx context.Context, loc runtime.Locator) (runtime.Observation, error) {
	if err := r.checkLocator(loc); err != nil {
		return runtime.Observation{}, err
	}
	return r.evaluate(ctx, loc), nil
}

// evaluate is one observation of loc's process from the stores: Uncertain while they cannot answer
// (view), otherwise the verdict the mapping reads from them, said Displaced when the daemon's own
// replacement of the recorded pod is what ended the process (displaced).
//
// The pod store and the Sandbox store are fed by two informers nothing orders, so a pod's end can
// reach the runtime before the Suspended patch that named it does. A final verdict (ends) whose
// cached Sandbox does not name the recorded pod is therefore confirmed with one read of the
// Sandbox from the API server: a store that has not seen a write proves nothing about it, the rule
// awaitPodGone keeps. The patch is applied before the controller deletes the pod, so a read made
// once the pod store shows the end sees the name. When that read names the pod, the verdict is
// read again from it, its detail included; when the read fails the observation is Uncertain, so the
// claim stays watched and is evaluated again.
func (r *Runtime) evaluate(ctx context.Context, loc runtime.Locator) runtime.Observation {
	uncertain := func(format string, args ...any) runtime.Observation {
		return runtime.Observation{Locator: loc, Kind: runtime.Uncertain, At: r.now(), Detail: fmt.Sprintf(format, args...)}
	}
	name := loc.Sandbox.Name
	v, err := r.view(name)
	if err != nil {
		return uncertain("%v", err)
	}
	obs := r.verdict(ctx, loc, v)
	if ends(obs) && !names(v.sandbox, loc) {
		current, err := r.readSandbox(ctx, name)
		if err != nil {
			return uncertain("the stores report %s (%s); reading sandbox %s to learn whether the daemon replaced pod %s failed: %v",
				obs.Kind, obs.Detail, name, loc.Sandbox.PodUID, err)
		}
		if names(current, loc) {
			v = r.viewOf(name, current)
			obs = r.verdict(ctx, loc, v)
		}
	}
	if displaced(v, loc, obs) {
		obs.Displaced = true
		obs.Detail += fmt.Sprintf("; the daemon replaced pod %s to launch another role there", loc.Sandbox.PodUID)
	}
	return obs
}

// readSandbox is the Sandbox named name as the API server holds it now, or nil when it holds none.
func (r *Runtime) readSandbox(ctx context.Context, name string) (*sandbox, error) {
	u, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeSandbox(u)
}

// ends reports whether obs is a final verdict the daemon's own replacement of a pod can account
// for: a Gone or NotRecordedProcess, never a lost workspace, which no replacement causes.
func ends(obs runtime.Observation) bool {
	return (obs.Kind == runtime.Gone || obs.Kind == runtime.NotRecordedProcess) && !obs.WorkspaceLost
}

// names reports whether s names loc's recorded pod as the live pod a relaunch replaced
// (displacedAnnotation).
func names(s *sandbox, loc runtime.Locator) bool {
	if s == nil {
		return false
	}
	named, ok := s.Annotations[displacedAnnotation]
	return ok && named == loc.Sandbox.PodUID
}

// displaced reports whether obs, the verdict on loc's process from v, is an end the daemon made:
// a final verdict (ends) on a process whose recorded pod the Sandbox names as the live pod a
// relaunch replaced, once that pod is being deleted or is gone. A role that ends while its pod
// still runs undeleted ended on its own, and so did one whose pod the Sandbox does not name: the
// name is written in the patch that suspends the pod, so a suspension that never applied names
// nothing.
func displaced(v view, loc runtime.Locator, obs runtime.Observation) bool {
	if !ends(obs) || !names(v.sandbox, loc) {
		return false
	}
	return v.pod == nil || string(v.pod.UID) != loc.Sandbox.PodUID || v.pod.DeletionTimestamp != nil
}

// verdict is the observation mapping for one recorded role process, from the stores' view of its
// Sandbox and pod. Pod loss applies to every role in the issue, but a live pod is judged through
// the recorded role container: Ready changes during a neighbour's container restart and is never a
// whole-pod death verdict.
//
// A live pod that holds, for the recorded role, something fixed for its life that a pod created now
// is not handed (movedInPod: the stream its launcher dials, or the secrets broker's enrollment the
// role's next generation runs against) is StaleAddress, before its launcher's state is read: a
// launcher on a stale stream never reaches this daemon, so its state would only ever read
// disconnected, and a pod lacking the enrollment cannot run the role's next generation at all. Every
// role of such a pod reads so, so each is relaunched uncharged and they move into one new pod
// together, rather than the first relaunch for some other reason replacing the pod under the rest.
// A role running its recorded generation whose Sandbox records it was started with other addresses
// than a generation started now is handed (movedEnvironment) is StaleAddress too. Either names each
// moved address, never its credentials.
func (r *Runtime) verdict(ctx context.Context, loc runtime.Locator, v view) runtime.Observation {
	observe := func(kind runtime.ObservationKind, format string, args ...any) runtime.Observation {
		return runtime.Observation{Locator: loc, Kind: kind, At: r.now(), Detail: fmt.Sprintf(format, args...)}
	}
	name := loc.Sandbox.Name
	switch {
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
	case corev1.PodPending, "", corev1.PodRunning:
	default:
		return observe(runtime.Uncertain, "pod %s (uid %s) phase %s", name, pod.UID, pod.Status.Phase)
	}
	if moved := r.movedInPod(pod, claim.Role(loc.Sandbox.Container)); len(moved) > 0 {
		return observe(runtime.StaleAddress, "pod %s (uid %s) holds addresses a pod launched now is not handed: %s", name, pod.UID, describeMoved(moved))
	}
	if pod.Status.Phase != corev1.PodRunning {
		return observe(runtime.Alive, "pod %s (uid %s) %s", name, pod.UID, phaseOf(pod))
	}
	status := containerStatus(pod, loc.Sandbox.Container)
	if status == nil {
		return observe(runtime.Uncertain, "pod %s (uid %s) Running has no status for role container %s", name, pod.UID, loc.Sandbox.Container)
	}
	// A connected launcher running the recorded generation is the role container's current
	// instance. The pod status can still show the previous instance terminated for a moment after
	// Kubernetes restarts the container.
	state, connected := r.launchers.state(loc.Claim, loc.Sandbox.PodUID)
	if connected && state.Child != nil && state.Child.Generation == loc.Sandbox.Generation {
		moved, err := r.movedEnvironment(v.sandbox, claim.Role(loc.Sandbox.Container), loc.Sandbox.Generation)
		if err != nil {
			return observe(runtime.StaleAddress, "pod %s (uid %s) role container %s runs generation %d, whose addresses cannot be compared: %v",
				name, pod.UID, loc.Sandbox.Container, state.Child.Generation, err)
		}
		if len(moved) > 0 {
			return observe(runtime.StaleAddress, "pod %s (uid %s) role container %s runs generation %d, started with addresses a generation started now is not handed: %s",
				name, pod.UID, loc.Sandbox.Container, state.Child.Generation, describeMoved(moved))
		}
		return observe(runtime.Alive, "pod %s (uid %s) role container %s runs generation %d", name, pod.UID, loc.Sandbox.Container, state.Child.Generation)
	}
	if ended := status.State.Terminated; ended != nil {
		return observe(runtime.Gone, "pod %s (uid %s) role container %s terminated (%s, exit code %d); last lines:\n%s",
			name, pod.UID, loc.Sandbox.Container, ended.Reason, ended.ExitCode, r.logTail(ctx, name, loc.Sandbox.Container))
	}
	if !connected {
		return observe(runtime.Uncertain, "pod %s (uid %s) role launcher %s is disconnected", name, pod.UID, loc.Sandbox.Container)
	}
	switch {
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
		detail.WriteString("the issue's volume was lost: ")
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
	if kind == "init" && container == fetchContainer {
		detail.WriteString(r.fetchCacheState(pod))
	}
	fmt.Fprintf(&detail, "; last lines of %s:\n%s", container, r.logTail(ctx, pod.Name, container))
	return detail.String(), workspaceLost
}

// fetchCacheState is what a failed workspace-fetch's detail says of the repository cache it fills
// a fresh feed from: that cache's state, with the kubelet's reason while it is failing, or that the
// repository has none. The repository is the one the pod's fetch container names
// (fetchRepository); a pod whose fetch names none says nothing.
func (r *Runtime) fetchCacheState(pod *corev1.Pod) string {
	repo, ok := fetchRepository(pod)
	if !ok {
		return ""
	}
	status, found := r.RepositoryCache(repo)
	if !found {
		return fmt.Sprintf("; %s has no repository cache, which a fresh feed comes from alone", repo)
	}
	state := fmt.Sprintf("; the repository cache of %s, which a fresh feed comes from alone, is %s", repo, status.State)
	if status.Reason != "" {
		state += ": " + status.Reason
	}
	return state
}

// fetchRepository is the repository pod's fetch container names (--repo), which the runtime wrote
// there; false for a pod whose fetch names none that parses.
func fetchRepository(pod *corev1.Pod) (ghrepo.Repository, bool) {
	for _, c := range pod.Spec.InitContainers {
		if at := slices.Index(c.Command, "--repo"); c.Name == fetchContainer && at >= 0 && at+1 < len(c.Command) {
			repo, err := ghrepo.Parse("--repo", c.Command[at+1])
			return repo, err == nil
		}
	}
	return ghrepo.Repository{}, false
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

// initContainerStatus is containerStatus for an init container: the status of pod's init
// container name, or nil when the kubelet reports none for it yet.
func initContainerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == name {
			return &pod.Status.InitContainerStatuses[i]
		}
	}
	return nil
}

// logTail is a container's last log lines, or why they could not be read: a detail's quote never
// changes its verdict, so the read is bounded whole (call).
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
	selector := fields.Set{"involvedObject.kind": "Pod", "involvedObject.name": pod.Name, "involvedObject.uid": string(pod.UID)}
	list, err := r.kube.CoreV1().Events(r.namespace).List(ctx, metav1.ListOptions{FieldSelector: selector.String()})
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
