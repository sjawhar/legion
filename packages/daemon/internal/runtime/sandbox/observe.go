package sandbox

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"slices"
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
// from it and evaluate answers Uncertain (row 1).
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

// evaluate is the observation mapping (the Stage 4 plan's interfaces block), for R the recorded
// incarnation loc carries, S the claim's Sandbox, and P the pod S owns. The rows apply in order,
// every observation carries loc, and the uid observed goes in the detail:
//
//  1. S cannot be read from the store, or a store's
//     list or watch is failing                       → Uncertain
//  2. S absent                                       → Gone
//  3. P absent (either mode)                         → Gone
//  4. P's uid ≠ R                                    → NotRecordedProcess
//  5. P Failed or Succeeded                          → Gone, quoting the container that ended
//  6. P Pending, PodScheduled=False past BootTimeout → Gone, quoting the pod's events
//  7. S's current Ready reason MultiplePods or ReconcilerError → Uncertain
//  8. P Pending (any sub-state) or Running, and it holds every address a pod
//     launched now is handed                          → Alive
//  9. P Pending or Running, and it holds an address a pod launched now is not
//     handed — the daemon's configuration moved since the pod was launched → StaleAddress
//
// Terminal state is read from the pod, whose phase and container states belong to its one uid; a
// Sandbox condition is quoted only when written for the Sandbox's current generation, so one left
// by the previous pod is never read as this one's (B5). A pod being deleted stays Alive until it
// is gone. The registration deadline bounds rows 6 and 8.
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
	case string(v.pod.UID) != loc.Incarnation:
		return observe(runtime.NotRecordedProcess, "pod %s is uid %s, not the recorded %s", name, v.pod.UID, loc.Incarnation)
	}
	pod := v.pod
	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodSucceeded:
		detail, workspaceLost := r.ended(ctx, v)
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
	case corev1.PodPending, corev1.PodRunning, "":
		if moved := r.movedAddresses(pod); len(moved) > 0 {
			return observe(runtime.StaleAddress, "pod %s (uid %s) holds addresses a pod launched now is not handed: %s",
				name, pod.UID, strings.Join(moved, "; "))
		}
		return observe(runtime.Alive, "pod %s (uid %s) %s", name, pod.UID, phaseOf(pod))
	}
	return observe(runtime.Uncertain, "pod %s (uid %s) phase %s", name, pod.UID, pod.Status.Phase)
}

// movedAddresses are row 9's comparison: each address pod's main container holds that differs from
// the one a pod launched now is handed (handedAddresses), as "<name> <held>, now <handed>", both
// named without putting URL userinfo in the detail. A pod is compared only when its main container
// runs the shim with connectFlag, as every pod this runtime builds does; one without (never one this
// runtime built) has nothing to compare, and row 9 must never read it as stale for want of it. A
// variable the container lacks holds "", as mainEnvironment leaves unset an address the runtime
// hands none of. A handed value is compared as the pod spec carries it, escaped against the
// kubelet's expansion (kubeletLiteral).
func (r *Runtime) movedAddresses(pod *corev1.Pod) []string {
	i := slices.IndexFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == mainContainer })
	if i < 0 {
		return nil
	}
	main := pod.Spec.Containers[i]
	flag := slices.Index(main.Command, connectFlag)
	if flag < 0 || flag+1 >= len(main.Command) {
		return nil
	}
	held := map[string]string{connectFlag: main.Command[flag+1]}
	for _, v := range main.Env {
		held[v.Name] = v.Value
	}
	var moved []string
	for _, a := range r.handedAddresses() {
		if handed := kubeletEscape(a.value); held[a.name] != handed {
			heldName, handedName := namedURL(held[a.name]), namedURL(handed)
			if a.name == "ENVOY_NATS_URL" {
				heldName, handedName = namedNATSPodValue(held[a.name]), namedNATSURLs(r.natsURLs)
			}
			moved = append(moved, fmt.Sprintf("%s %s, now %s", a.name, heldName, handedName))
		}
	}
	return moved
}

// namedURL is the log boundary for one endpoint value: an existing pod can hold a value a previous
// daemon accepted, or a direct Options caller supplied, even when this daemon's current loader no
// longer accepts it. It names "(unset)" for none; otherwise it constructs the detail from exactly
// the scheme, host and port it accepts to show, adding xxxxx@ when the URL carries userinfo. Path,
// query and fragment never enter the detail. An endpoint that cannot yield a scheme and host is
// xxxxx whole, so unexpected input cannot make the detail a credential reader.
func namedURL(address string) string {
	if address == "" {
		return "(unset)"
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "xxxxx"
	}
	name := u.Scheme + "://"
	if u.User != nil {
		name += "xxxxx@"
	}
	return name + u.Host
}

// namedNATSURLs names the current ENVOY_NATS_URL from the distinct values r.natsURLs holds: the
// entries readNatsURLs accepted, before mainEnvironment comma-joined them. Each is an endpoint, so
// namedURL may parse its kubelet-escaped value whole.
func namedNATSURLs(urls []string) string {
	if len(urls) == 0 {
		return "(unset)"
	}
	named := make([]string, len(urls))
	for i, raw := range urls {
		named[i] = namedURL(kubeletEscape(raw))
	}
	return strings.Join(named, ",")
}

// namedNATSPodValue names the ENVOY_NATS_URL an existing pod holds, which can be from the same
// older grammar namedURL handles. mainEnvironment comma-joined the old daemon's list, but this
// formatter never splits it: raw commas are valid in userinfo, so splitting
// nats://user:pa,ss@host:4222 at the comma puts `user:pa` in a fragment's host position, which an
// allowlist would print. url.Parse reads the whole string, and namedURL allowlists only its scheme,
// host and port. A path holding a later URL or a credential-shaped value never prints. A parse
// failure is the one case whose components are unknown, so it is xxxxx whole.
func namedNATSPodValue(address string) string {
	if address == "" {
		return "(unset)"
	}
	return namedURL(address)
}

// podAbsent is row 3's detail: the Sandbox's mode, its Suspended condition when current, and any
// same-named pod the Sandbox does not own.
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

// ended is row 5's detail: the container that ended — the first init container that failed, else
// the main container — with its reason and exit code, the Sandbox's Finished condition when
// current, and the container's last log lines. workspace-init's exit code 3 is the tree volume
// lost (decision 11), which it reports as the second return rather than as a shape of the detail.
func (r *Runtime) ended(ctx context.Context, v view) (string, bool) {
	pod := v.pod
	kind, container, state := "main", mainContainer, (*corev1.ContainerStateTerminated)(nil)
	for _, status := range pod.Status.InitContainerStatuses {
		if t := status.State.Terminated; t != nil && t.ExitCode != 0 {
			kind, container, state = "init", status.Name, t
			break
		}
	}
	if state == nil {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == mainContainer {
				state = status.State.Terminated
			}
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
