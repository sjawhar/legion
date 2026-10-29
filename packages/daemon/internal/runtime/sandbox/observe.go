package sandbox

import (
	"bytes"
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
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// logTailLines is how much of a failed container's log a Gone quotes (runtime-kubernetes.ts:678-689).
const logTailLines = 20

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
//
// When that read finds no Sandbox at all, the name may be in the gap a migrating launch opens
// between deleting the issue's Sandbox of the superseded disk shape and creating its replacement
// (replaceSupersededSandbox), and then the runtime's own keeping of the pod the deleted one named
// (stranded) stands in for the annotation. It is taken before the confirming read: it is kept from
// before the delete is sent until after the Create is answered, so taken first it misses the gap
// only if a whole foreground delete, garbage collection included, ran between the two, where taken
// after it would miss a Create landing between them. That one window is closed by the record's
// generation (strandedGen), taken with it and again once the read answers: a GET the API server is
// slow to answer can span the whole delete, and then the record taken before it is not the one the
// absence it reports belongs to, so the observation is Uncertain and the claim is evaluated again a
// tick later, when a Sandbox exists to answer for itself or the record has settled. Only an absence
// the API server confirmed under a record that did not move lets it answer; a Sandbox that exists
// answers for itself, as it does for a restarted daemon. It names a pod by uid, so it can only call
// displaced a process of the pod the suspension ended.
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
	stranded := false
	if ends(obs) && !names(v.sandbox, loc) {
		held, heldAt := r.strandedPod(name)
		current, err := r.readSandbox(ctx, name)
		if err != nil {
			return uncertain("the stores report %s (%s); reading sandbox %s to learn whether the daemon replaced pod %s failed: %v",
				obs.Kind, obs.Detail, name, loc.Sandbox.PodUID, err)
		}
		switch {
		case names(current, loc):
			v = r.viewOf(name, current)
			obs = r.verdict(ctx, loc, v)
		case current == nil:
			if now, nowAt := r.strandedPod(name); nowAt != heldAt {
				return uncertain("the stores report %s (%s); the API server holds no sandbox %s, but the daemon's record of the pods its sandbox replacements displaced moved while that read was out (pod %q held for it before, %q now), so whether the daemon replaced pod %s is read again",
					obs.Kind, obs.Detail, name, held, now, loc.Sandbox.PodUID)
			}
			stranded = held != "" && held == loc.Sandbox.PodUID
		}
	}
	if stranded || displaced(v, loc, obs) {
		obs.Displaced = true
		obs.Detail += fmt.Sprintf("; the daemon replaced pod %s to launch another role there", loc.Sandbox.PodUID)
	}
	return obs.Observation
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

// observation is one verdict of the mapping: the runtime.Observation the caller receives, beside
// the one classification only this package acts on. No runtime consumer reads it — the supervisor
// reads Kind and Displaced alone — so it stays here rather than on the cross-runtime contract.
type observation struct {
	runtime.Observation
	// initFailed is a Gone whose process never started because an init container of its pod ended
	// non-zero: the pod provisioned nothing a launcher could run in. No replacement the daemon made
	// can account for such an end, which is what has a relaunch replace a pod whose init keeps
	// failing rather than read its roles as displaced into it (ends). A pod the daemon is itself
	// deleting is never one of these, however its init container ended: a pod still inside its init
	// containers is live to a relaunch (relaunch's live), so the daemon displaces it for image
	// drift, a moved address or the disk migration, and the kubelet then kills the init container it
	// was running — a non-zero termination of the daemon's own making, which read as a failed init
	// would charge every role of that pod a death.
	initFailed bool
}

// ends reports whether obs is a final verdict the daemon's own replacement of a pod can account
// for: a Gone or NotRecordedProcess, never one a failed init container made.
func ends(obs observation) bool {
	return (obs.Kind == runtime.Gone || obs.Kind == runtime.NotRecordedProcess) && !obs.initFailed
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
func displaced(v view, loc runtime.Locator, obs observation) bool {
	if !ends(obs) || !names(v.sandbox, loc) {
		return false
	}
	return v.pod == nil || string(v.pod.UID) != loc.Sandbox.PodUID || v.pod.DeletionTimestamp != nil
}

// verdict is the observation mapping for one recorded role process, from the stores' view of its
// Sandbox and pod. The process's role is its claim's (loc.Claim.Role, which Validate holds); its
// container is its pod's one container, `roles` in an issue pod, whose launcher runs every role of
// the issue, and `controller` in the controller's. Pod loss applies to every role in the pod, and so
// does the one container's end: a launcher that exits takes every role's process with it. A live
// pod is judged through that role's own launcher state: Ready changes during the container's
// restart and is never a whole-pod death verdict, and a role's own process ending is that role's
// alone.
//
// A live pod that holds, for the recorded role, something fixed for its life that a pod created now
// is not handed (movedInPod: the layout before one container, the stream its launcher dials, or the
// secrets broker's enrollment the role's next generation runs against) is StaleAddress, before its
// launcher's state is read: a launcher on a stale stream never reaches this daemon, so its state
// would only ever read disconnected, and a pod lacking the enrollment cannot run the role's next
// generation at all. Every role of such a pod reads so, so each is relaunched uncharged and they
// move into one new pod together, rather than the first relaunch for some other reason replacing
// the pod under the rest. A role running its recorded generation whose Sandbox records it was
// started with other addresses than a generation started now is handed (movedEnvironment) is
// StaleAddress too. Either names each moved address, never its credentials.
func (r *Runtime) verdict(ctx context.Context, loc runtime.Locator, v view) observation {
	observe := func(kind runtime.ObservationKind, format string, args ...any) observation {
		return observation{Observation: runtime.Observation{Locator: loc, Kind: kind, At: r.now(), Detail: fmt.Sprintf(format, args...)}}
	}
	name, container := loc.Sandbox.Name, loc.Sandbox.Container
	role, _ := loc.Claim.Role()
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
		ended := observe(runtime.Gone, "%s", r.ended(ctx, v, container, role, initName, initExit))
		ended.initFailed = initExit != nil && pod.DeletionTimestamp == nil
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
	if moved := r.movedInPod(pod, role); len(moved) > 0 {
		return observe(runtime.StaleAddress, "pod %s (uid %s) holds addresses a pod launched now is not handed: %s", name, pod.UID, describeMoved(moved))
	}
	if pod.Status.Phase != corev1.PodRunning {
		return observe(runtime.Alive, "pod %s (uid %s) %s", name, pod.UID, phaseOf(pod))
	}
	status := containerStatus(pod, container)
	if status == nil {
		return observe(runtime.Uncertain, "pod %s (uid %s) Running has no status for container %s", name, pod.UID, container)
	}
	// A connected launcher running the recorded generation is the role's current process. The pod
	// status can still show the container's previous instance terminated for a moment after
	// Kubernetes restarts it.
	state, connected := r.launchers.state(loc.Claim, loc.Sandbox.PodUID)
	if connected && state.Child != nil && state.Child.Generation == loc.Sandbox.Generation {
		moved, err := r.movedEnvironment(v.sandbox, role, loc.Sandbox.Generation)
		if err != nil {
			return observe(runtime.StaleAddress, "pod %s (uid %s) role %s runs generation %d, whose addresses cannot be compared: %v",
				name, pod.UID, role, state.Child.Generation, err)
		}
		if len(moved) > 0 {
			return observe(runtime.StaleAddress, "pod %s (uid %s) role %s runs generation %d, started with addresses a generation started now is not handed: %s",
				name, pod.UID, role, state.Child.Generation, describeMoved(moved))
		}
		return observe(runtime.Alive, "pod %s (uid %s) role %s runs generation %d", name, pod.UID, role, state.Child.Generation)
	}
	if ended := status.State.Terminated; ended != nil {
		return observe(runtime.Gone, "pod %s (uid %s) container %s terminated (%s, exit code %d); last lines of %s:\n%s",
			name, pod.UID, container, ended.Reason, ended.ExitCode, role, r.roleLog(ctx, name, container, role))
	}
	if !connected {
		return observe(runtime.Uncertain, "pod %s (uid %s) role %s's launcher is disconnected%s", name, pod.UID, role, containerRestarts(status))
	}
	switch {
	case state.Child != nil:
		return observe(runtime.NotRecordedProcess, "pod %s (uid %s) role %s runs generation %d, not the recorded %d",
			name, pod.UID, role, state.Child.Generation, loc.Sandbox.Generation)
	case state.LastExit != nil && state.LastExit.Generation == loc.Sandbox.Generation && state.LastExit.Code == shimwire.LauncherExitSetupFailed:
		return observe(runtime.Gone, "pod %s (uid %s) role %s reports generation %d failed setting up its namespaces before its shim started; last lines of %s:\n%s",
			name, pod.UID, role, state.LastExit.Generation, role, r.roleLog(ctx, name, container, role))
	case state.LastExit != nil && state.LastExit.Generation == loc.Sandbox.Generation:
		return observe(runtime.Gone, "pod %s (uid %s) role %s reports generation %d exited (code %d, signal %s)",
			name, pod.UID, role, state.LastExit.Generation, state.LastExit.Code, state.LastExit.Signal)
	case state.LastExit != nil:
		return observe(runtime.NotRecordedProcess, "pod %s (uid %s) role %s last exited generation %d, not the recorded %d",
			name, pod.UID, role, state.LastExit.Generation, loc.Sandbox.Generation)
	default:
		return observe(runtime.Gone, "pod %s (uid %s) role %s's launcher reports no child", name, pod.UID, role)
	}
}

// containerRestarts is what the kubelet says of the pod's one container while no launcher of it
// is connected: the reason it waits, and, once it has ended at least once, its restart count and
// how its last instance ended. That container runs every role's launcher, so a container that
// crash-loops reads as every role's launcher disconnected, and this is the why each of them
// carries. It is "" for a container that is running and never ended.
func containerRestarts(status *corev1.ContainerStatus) string {
	var detail strings.Builder
	if waiting := status.State.Waiting; waiting != nil {
		fmt.Fprintf(&detail, "; container %s waiting (%s", status.Name, waiting.Reason)
		if waiting.Message != "" {
			fmt.Fprintf(&detail, ": %s", waiting.Message)
		}
		detail.WriteString(")")
	}
	if last := status.LastTerminationState.Terminated; last != nil {
		fmt.Fprintf(&detail, "; container %s restarted %d times, its last instance terminated (%s, exit code %d", status.Name, status.RestartCount, last.Reason, last.ExitCode)
		if !last.FinishedAt.IsZero() {
			fmt.Fprintf(&detail, " at %s", last.FinishedAt.UTC().Format(time.RFC3339))
		}
		detail.WriteString(")")
	}
	return detail.String()
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
// pod's one container, container, is what ended, and with it every role it ran. The quote is the
// init container's log, or role's own lines of the container's (roleLog). A pod of the layout
// before one container (movedLayout) that ended before this daemon replaced it has no `roles`
// container: role's own container there is what ended, and its log is role's alone.
func (r *Runtime) ended(ctx context.Context, v view, container string, role claim.Role, initName string, initExit *corev1.ContainerStateTerminated) string {
	pod := v.pod
	kind, state := "container", initExit
	if initExit != nil {
		kind, container = "init container", initName
	} else if _, earlier := movedLayout(pod); earlier {
		container = string(role)
	}
	if state == nil {
		if status := containerStatus(pod, container); status != nil {
			state = status.State.Terminated
		}
	}
	var detail strings.Builder

	fmt.Fprintf(&detail, "pod %s (uid %s) %s", pod.Name, pod.UID, pod.Status.Phase)
	if state != nil {
		fmt.Fprintf(&detail, ": %s %s terminated (%s, exit code %d)", kind, container, state.Reason, state.ExitCode)
	} else if pod.Status.Reason != "" {
		fmt.Fprintf(&detail, ": %s: %s", pod.Status.Reason, pod.Status.Message)
	}
	if finished := v.sandbox.condition(conditionFinished); finished != nil {
		fmt.Fprintf(&detail, "; sandbox Finished=%s %s", finished.Status, finished.Reason)
	}
	if initExit != nil {
		if container == fetchContainer {
			detail.WriteString(r.fetchCacheState(pod))
		}
		fmt.Fprintf(&detail, "; last lines of %s:\n%s", container, r.logTail(ctx, pod.Name, container))
	} else {
		fmt.Fprintf(&detail, "; last lines of %s:\n%s", role, r.roleLog(ctx, pod.Name, container, role))
	}
	return detail.String()
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
// changes its verdict, so the read is bounded whole (call), in time and in bytes off the API
// (readLog).
func (r *Runtime) logTail(ctx context.Context, pod, container string) string {
	text, _, _, bounded, err := r.readLog(ctx, pod, container, logTailLines, logTailBytes, logTailReadBytes)
	if err != nil {
		return fmt.Sprintf("(the log could not be read: %v)", err)
	}
	if bounded {
		text += "\n" + logBoundNote(logTailReadBytes)
	}
	return text
}

// sharedLogLines and sharedLogBytes bound how much of an issue pod's one container's log roleLog
// reads to find one role's last lines among every role's, and sharedLogReadBytes how much of the
// stream that read takes off the API. logTailBytes and logTailReadBytes are the same pair for one
// container's own tail. A line is bounded at 64 KiB where the launcher writes it
// (launcher.maxLogLine) and nowhere else, so 2000 lines are up to 128 MiB on the wire, read once
// per role of a terminal pod: the read bound holds that to sharedLogReadBytes, enough for 2000
// lines of 4 KiB, past what any quote keeps. A read the bound cut is read again for fewer lines
// (readLog); one cut even so keeps the oldest of its tail, since a stream is read from its start,
// so the quote says so (logBoundNote) rather than passing off older lines as the container's last.
const (
	sharedLogLines     = 2000
	sharedLogBytes     = 1 << 20
	sharedLogReadBytes = 8 << 20
	logTailBytes       = 64 << 10
	logTailReadBytes   = 1 << 20
)

// logBoundNote says a quote is short of the container's newest lines because the read stopped at
// its byte bound.
func logBoundNote(read int) string {
	return fmt.Sprintf("(the log was read to its %d-byte bound: these are not the container's newest lines)", read)
}

// containerLinesHeader introduces, in a role's quote of an issue pod's one container, the lines
// no role wrote (containerLines).
const containerLinesHeader = "the container's own last lines, no role's:"

// roleLog is role's last lines of container's log, quoted when that container has ended or role's
// generation failed setting up its namespaces (shimwire.LauncherExitSetupFailed). The
// controller's container runs its role alone, so its log is the role's whole. An issue pod's one
// container interleaves every role's lines, each prefixed `<role>: ` by the launcher, so its quote
// is role's own (roleLines), never a sibling's: a sibling's agent may have written anything there.
// The container's own lines follow it (containerLines): what the launcher, PID 1, wrote itself
// rather than for a role, such as a panic, is why every role of the container ended.
//
// The first line of the read is attributed to no one when the line bound alone cut it: the
// kubelet's tail counts a long line's partial runtime entries as lines of their own, so a read can
// start inside a role's line, at a fragment that role's agent chose, which could read as another
// role's line or the launcher's. Dropping it costs the read's oldest line. A read the byte bound cut
// already starts after a newline the log holds (readLog), on a whole line, so nothing more is
// dropped from it.
func (r *Runtime) roleLog(ctx context.Context, pod, container string, role claim.Role) string {
	if container != runtime.IssuePodContainer {
		return r.logTail(ctx, pod, container)
	}
	text, tail, whole, bounded, err := r.readLog(ctx, pod, container, sharedLogLines, sharedLogBytes, sharedLogReadBytes)
	if err != nil {
		return fmt.Sprintf("(the log could not be read: %v)", err)
	}
	if !whole {
		_, text, _ = strings.Cut(text, "\n")
	}
	quote := roleLines(text, role, logTailLines)
	if quote == "" {
		quote = fmt.Sprintf("(none of the container's last %d lines is %s's)", tail, role)
	}
	if own := containerLines(text, logTailLines); own != "" {
		quote += "\n" + containerLinesHeader + "\n" + own
	}
	if bounded {
		quote += "\n" + logBoundNote(sharedLogReadBytes)
	}
	return quote
}

// readLog is container's last lines of log, at most limit bytes of them, read under one bound
// (call) and at most read bytes off the API, as probeLog reads the probe pod's log through an
// io.LimitReader. TailLines bounds the read to lines, and the stream is read through tailBytes,
// which holds only its newest limit bytes, so a log's size never decides how much the daemon
// holds, and a quote never ends before the lines nearest the container's end. The read bound is
// what a log's size cannot make the daemon pull: a line is bounded only where the launcher writes
// it (64 KiB), so lines alone leave the stream in the hundreds of megabytes. A read the bound cut
// ends before the container's newest lines, since a stream is read from its start, so readLog
// reads once more, asking for half the whole lines the cut read held: the newest lines are then
// what it reads, unless they are longer still, when that read is cut too and bounded says so, for
// the caller to say so in its quote. A second read that fails (its stream refused, or the shared
// bound expiring) gives back the first read, still bounded, rather than losing what it held. tail
// is the TailLines of the read text came from. When the byte bound dropped older bytes, the read
// starts from the first whole line it kept, and whole says so: the text then starts just after a
// newline of the log. Otherwise it starts where the kubelet's line count did, which may be inside
// a line.
func (r *Runtime) readLog(ctx context.Context, pod, container string, lines int64, limit, read int) (text string, tail int64, whole, bounded bool, err error) {
	reading, cancel := call(ctx)
	defer cancel()
	text, whole, seen, bounded, err := r.readLogOnce(reading, pod, container, lines, limit, read)
	if err != nil || !bounded {
		return text, lines, whole, bounded, err
	}
	fewer := max(1, seen/2)
	if fewer >= lines {
		return text, lines, whole, bounded, nil
	}
	again, againWhole, _, againBounded, err := r.readLogOnce(reading, pod, container, fewer, limit, read)
	if err != nil {
		return text, lines, whole, bounded, nil
	}
	return again, fewer, againWhole, againBounded, nil
}

// readLogOnce is one read of readLog's: the text, whether it starts on a whole line, how many
// newlines the read took off the API, and whether it stopped at read bytes.
func (r *Runtime) readLogOnce(ctx context.Context, pod, container string, lines int64, limit, read int) (text string, whole bool, seen int64, bounded bool, err error) {
	stream, err := r.kube.CoreV1().Pods(r.namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &lines}).Stream(ctx)
	if err != nil {
		return "", false, 0, false, err
	}
	defer stream.Close()
	counted := &logByteCounter{r: io.LimitReader(stream, int64(read)+1)}
	kept, cut, err := tailBytes(counted, limit)
	if err != nil {
		return "", false, 0, false, err
	}
	if cut {
		if newline := bytes.IndexByte(kept, '\n'); newline >= 0 {
			kept, whole = kept[newline+1:], true
		}
	}
	return strings.TrimRight(string(kept), "\n"), whole, counted.newlines, counted.n > int64(read), nil
}

// logByteCounter counts the bytes and newlines read through it, so a read that stopped at an
// io.LimitReader's bound is told from one that reached the stream's end, and how many lines it got.
type logByteCounter struct {
	r        io.Reader
	n        int64
	newlines int64
}

func (c *logByteCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.newlines += int64(bytes.Count(p[:n], []byte{'\n'}))
	return n, err
}

// roleLines is role's last n lines of log, each with the launcher's `<role>: ` prefix dropped: the
// lines of an issue pod's one container that are role's own, none of a sibling's (whose name
// another's may begin with) or the launcher's unprefixed ones.
func roleLines(log string, role claim.Role, n int) string {
	prefix := string(role) + ": "
	var own []string
	for line := range strings.Lines(log) {
		if rest, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), prefix); ok {
			own = append(own, rest)
		}
	}
	return strings.Join(own[max(0, len(own)-n):], "\n")
}

// containerLines is the last n lines of an issue pod's one container's log that no role wrote:
// every role's output reaches the log behind its `<role>: ` prefix, so a line with none of them is
// the launcher's own, PID 1's.
func containerLines(log string, n int) string {
	var own []string
	for line := range strings.Lines(log) {
		if line = strings.TrimRight(line, "\n"); !rolePrefixed(line) {
			own = append(own, line)
		}
	}
	return strings.Join(own[max(0, len(own)-n):], "\n")
}

// rolePrefixed is whether line begins with a workflow role's `<role>: ` prefix.
func rolePrefixed(line string) bool {
	return slices.ContainsFunc(claim.Roles, func(role claim.Role) bool { return strings.HasPrefix(line, string(role)+": ") })
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
