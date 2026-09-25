package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
)

// The image probe (the in-cluster boot probe, packages/daemon/src/daemon/worker-image-probe.ts,
// as a Sandbox): a daemon off the cluster is not the image its workers run, so it proves the image
// by running the image's own Go `legion probe-image --go-daemon-api-version <N>` in a Sandbox of
// that image, placed as every worker is placed, and reading the OK line back from the pod's log.
// Only a pod on this cluster proves the cluster can pull the image, schedule it on the Legion pool
// under gVisor, and run its Oh My Pi and plugin there; only the image's CLI can read the image
// plugin's contract.
//
// The Sandbox is `shutdownPolicy: Delete` with a `shutdownTime`, so the controller deletes one the
// daemon never came back for (sandbox_controller.go:1712-1768 at v1.0.3), a crashed boot's leftover
// included; the daemon reads the log before then and deletes the Sandbox itself when an attempt
// ends. It carries the project label, so the runtime's informers see it and its pod, and the
// legion.dev/probe label, which ReconcileOrphans skips: the sweep never deletes a Sandbox whose
// legion.dev/probe label is set, so a probe in flight is never swept; a leftover ends at its
// shutdownTime, or when the next boot's probe replaces it (createProbe).
const (
	labelProbe     = "legion.dev/probe"
	probeContainer = "probe"
	// probeLogLines is how much of the probe pod's log is read and quoted (LOG_TAIL_LINES,
	// worker-image-probe.ts:59).
	probeLogLines = 50
	// probeTerminationGrace is the probe pod's terminationGracePeriodSeconds: it holds nothing to
	// save (worker-image-probe.ts:144).
	probeTerminationGrace = 5
)

// definitiveWaiting are the container waiting reasons no retry changes: the image reference itself
// is unusable (DEFINITIVE_WAITING_REASONS, worker-image-probe.ts:60-64).
var definitiveWaiting = map[string]bool{"InvalidImageName": true, "ErrImageNeverPull": true}

// digestHex is a sha256 digest's 64 hex.
var digestHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ImageProbe is how ProbeImage proves the runtime's worker image.
type ImageProbe struct {
	// Contract is the Go daemon API contract the image's plugin must declare: the daemon's own
	// GoDaemonAPIVersion.
	Contract int
	// Budget is how long one attempt waits for the probe pod to finish
	// (slow_command_timeout_seconds). The Sandbox's shutdownTime is the budget and one API call
	// after it, so the log of a pod that finished at the budget is still there to read.
	Budget time.Duration
	// Retry waits out the attempts that say nothing about the image: bootprobe.Daemon at boot.
	Retry bootprobe.Retry
	// RoleReferences are the task agents and skills the daemon's own role prompts name
	// (promptrefs.Roles), required: the prompts every worker pod is handed, so the probe resolves
	// those and their agents' models, not the image's copy (`legion probe-image --role-references`).
	RoleReferences string
	// Resources are the probe container's requests and limits (the TypeScript probe used the
	// `small` profile): it runs Oh My Pi three times (pi.agents, the plugin's load, the
	// session-storage setting) and exits. None when zero.
	Resources corev1.ResourceRequirements
}

// ProbeImage proves the runtime's image (Options.Image) on the cluster before any claim runs on
// it, or refuses naming why. Every boot probes: no pass is remembered, because the probe pod's spec
// cannot show the contents of the operator's ConfigMaps and Secrets, which decide whether the
// prompt-named agents' models resolve (the LEGION-270 plan, decision 5). An attempt's verdict is
// definitive — the API refusing what
// was sent (400, 401, 403, 422, or a create's 404), an image the kubelet cannot use, a pod whose
// probe container exited on its own and Failed, a log without the OK line or confirming another
// contract, or not resolving the agents' models, a providers Secret the pod cannot mount — or
// transient: anything else, a pod the kubelet itself failed included, retried under p.Retry
// (worker-image-probe.ts:318-338, 470-508).
func (r *Runtime) ProbeImage(ctx context.Context, p ImageProbe) error {
	if p.Contract < 1 || p.Budget <= 0 || p.Retry.Initial <= 0 || p.Retry.Max < p.Retry.Initial || p.RoleReferences == "" {
		return errors.New("image probe: a contract, a positive budget, a positive retry wait, and the role prompts' references are required")
	}
	_, hex, _ := strings.Cut(r.image, "@sha256:")
	if !digestHex.MatchString(hex) {
		return fmt.Errorf("image probe: image %q is not pinned by a sha256 digest", r.image)
	}
	digest := "sha256:" + hex
	name := probeName(r.project, hex)
	return bootprobe.Run(ctx, "worker image", p.Retry, r.log, func(ctx context.Context) bootprobe.Outcome {
		return r.probeAttempt(ctx, p, name, digest)
	})
}

// probeName is `legion-probe-<project>-<first 12 hex of the digest>`: one per project and image,
// so a crashed boot's leftover is found by name and replaced rather than accumulating, and two
// projects probing one image in a namespace never contend for one Sandbox. The project is slugged
// to fit a DNS label (probePodName, worker-image-probe.ts:108-116).
func probeName(project, hex string) string {
	prefix := "legion-probe-"
	return prefix + dnsName(project, maxNameLength-len(prefix)-1-12) + "-" + hex[:12]
}

// probeAttempt is one run of the probe Sandbox: create it (replacing this project's leftover),
// wait up to the budget for its pod to finish, read the pod's log, judge it, and delete the
// Sandbox whatever happened — with a context of its own, so a stopping daemon still cleans up.
func (r *Runtime) probeAttempt(ctx context.Context, p ImageProbe, name, digest string) bootprobe.Outcome {
	// A pod an earlier attempt's Sandbox left, whose deletion is still under way, holds the name.
	if err := r.await(ctx, p.Budget, "the pod of an earlier probe sandbox "+name+" to go", func() (bool, error) {
		pod := r.storedPod(name)
		if pod == nil {
			return true, nil
		}
		s, err := r.storedSandbox(name)
		return s != nil && ownedBy(pod, s.UID), err
	}); err != nil {
		return unlessStopped(ctx, err, "probe sandbox "+name)
	}
	// Built at each create, so the shutdown is the budget from that moment, whatever came before it.
	manifest := func() (*unstructured.Unstructured, error) {
		return encodeProbe(r.probeManifest(name, p, r.now().Add(p.Budget+apiTimeout)))
	}
	uid, outcome, created := r.createProbe(ctx, name, digest, manifest, p.Budget)
	if !created {
		return outcome
	}
	defer func() {
		deleting, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiTimeout)
		defer cancel()
		background := metav1.DeletePropagationBackground
		err := r.sandboxClient().Delete(deleting, name, metav1.DeleteOptions{
			PropagationPolicy: &background, Preconditions: &metav1.Preconditions{UID: &uid},
		})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			r.log.Warn("sandbox runtime: failed to delete the probe sandbox", "sandbox", name, "err", err)
		}
	}()

	var (
		seen      bool
		finished  *corev1.Pod
		verdict   *bootprobe.Outcome
		readErr   error
		mountRead time.Time
	)
	err := r.await(ctx, p.Budget, "probe pod "+name+" to finish", func() (bool, error) {
		s, err := r.storedSandbox(name)
		if err != nil {
			readErr = err
			return false, err
		}
		live := s != nil && s.UID == uid && s.DeletionTimestamp == nil
		switch {
		case live:
			seen = true
		case seen || (s != nil && s.UID == uid):
			vanished := bootprobe.Outcome{Detail: fmt.Sprintf("probe sandbox %s vanished before its pod finished", name)}
			verdict = &vanished
			return true, nil
		default:
			return false, nil // not in the store yet
		}
		pod := r.storedPod(name)
		if !ownedBy(pod, uid) {
			return false, nil
		}
		if terminal(pod) {
			finished = pod
			return true, nil
		}
		if stuck := r.stuck(ctx, pod, name, digest, &mountRead); stuck != nil {
			verdict = stuck
			return true, nil
		}
		return false, nil
	})
	switch {
	case ctx.Err() != nil:
		return bootprobe.Outcome{}
	case readErr != nil:
		return bootprobe.Outcome{Detail: fmt.Sprintf("probe sandbox %s: %v", name, readErr)}
	case err != nil:
		return bootprobe.Outcome{Detail: r.unfinished(ctx, name, uid, p.Budget)}
	case verdict != nil:
		return *verdict
	}
	logTail, logErr := r.probeLog(ctx, name)
	return r.judge(name, digest, finished, logTail, logErr, p.Contract)
}

// kubeletFailure is why the kubelet failed pod for reasons of its own, or "" when the pod is the
// image's answer (Succeeded, or Failed because its probe container exited on its own). Under
// `restartPolicy: Never` a node-pressure eviction, a graceful node shutdown, and a refusal at node
// admission all fail the pod: each sets the pod's reason (Evicted, Terminated, OutOfcpu,
// UnexpectedAdmissionError, …), and a disruption the kubelet or the control plane starts marks it
// a DisruptionTarget; a pod refused before its container started has no container that exited.
// None says anything about the image, and `karpenter.sh/do-not-disrupt` stops none of them.
func kubeletFailure(pod *corev1.Pod) string {
	if pod.Status.Phase != corev1.PodFailed {
		return ""
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason + ": " + pod.Status.Message
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.DisruptionTarget && condition.Status == corev1.ConditionTrue {
			return fmt.Sprintf("marked a disruption target (%s)", condition.Reason)
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == probeContainer && status.State.Terminated != nil {
			return ""
		}
	}
	return "its container " + probeContainer + " never ran to an exit"
}

// imageRefusal is a definitive verdict on the image digest: no retry changes it.
func imageRefusal(digest, format string, args ...any) bootprobe.Outcome {
	return bootprobe.Outcome{Refusal: fmt.Errorf("worker image %s failed its probe: %s", digest, fmt.Sprintf(format, args...))}
}

// apiOutcome is an API failure's verdict: the API server refusing what was sent (400, 401, 403,
// 422, or a 404 on a create: the namespace or the CRD is missing) refuses it again, so it is
// definitive; anything else is the cluster's moment, retried (worker-image-probe.ts:318-336).
func apiOutcome(digest string, err error, doing string, creating bool) bootprobe.Outcome {
	if apierrors.IsBadRequest(err) || apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) || apierrors.IsInvalid(err) ||
		(creating && apierrors.IsNotFound(err)) {
		return imageRefusal(digest, "%s: %v", doing, err)
	}
	return bootprobe.Outcome{Detail: fmt.Sprintf("%s: %v", doing, err)}
}

// unlessStopped is a wait's failure as a transient verdict, or no verdict at all when ctx ended:
// bootprobe.Run then reports the stop.
func unlessStopped(ctx context.Context, err error, detail string) bootprobe.Outcome {
	if ctx.Err() != nil {
		return bootprobe.Outcome{}
	}
	return bootprobe.Outcome{Detail: fmt.Sprintf("%s: %v", detail, err)}
}

// createProbe creates the probe Sandbox and answers its uid. A Sandbox already holding the name is
// this daemon's leftover only when it carries this project's label — the name is project-scoped,
// so anything else is another deployment's object, not this daemon's to delete — in which case it
// is deleted, its pod waited out, and the create repeated (worker-image-probe.ts:369-408).
func (r *Runtime) createProbe(ctx context.Context, name, digest string, manifest func() (*unstructured.Unstructured, error), budget time.Duration) (types.UID, bootprobe.Outcome, bool) {
	create := func() (types.UID, error) {
		object, err := manifest()
		if err != nil {
			return "", err
		}
		creating, cancel := call(ctx)
		defer cancel()
		created, err := r.sandboxClient().Create(creating, object, metav1.CreateOptions{})
		if err != nil {
			return "", err
		}
		return created.GetUID(), nil
	}
	uid, err := create()
	if err == nil {
		return uid, bootprobe.Outcome{}, true
	}
	doing := "create probe sandbox " + name
	if !apierrors.IsAlreadyExists(err) {
		return "", apiOutcome(digest, err, doing, true), false
	}
	getting, cancel := call(ctx)
	existing, err := r.sandboxClient().Get(getting, name, metav1.GetOptions{})
	cancel()
	if err != nil {
		return "", apiOutcome(digest, err, "read the existing probe sandbox "+name, false), false
	}
	if owner, ok := existing.GetLabels()[labelProject]; owner != r.project {
		belongs := "no Legion project"
		if ok {
			belongs = "project " + owner
		}
		return "", imageRefusal(digest, "probe sandbox %s already exists and belongs to %s, not %s; not deleting it", name, belongs, r.project), false
	}
	leftover := existing.GetUID()
	r.log.Info("sandbox runtime: deleting a leftover probe sandbox from an earlier boot before probing the worker image",
		"sandbox", name, "uid", leftover)
	deleting, cancel := call(ctx)
	background := metav1.DeletePropagationBackground
	err = r.sandboxClient().Delete(deleting, name, metav1.DeleteOptions{
		PropagationPolicy: &background, Preconditions: &metav1.Preconditions{UID: &leftover},
	})
	cancel()
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return "", apiOutcome(digest, err, "delete the leftover probe sandbox "+name, false), false
	}
	if err := r.await(ctx, budget, "the leftover probe sandbox "+name+" and its pod to go", func() (bool, error) {
		s, err := r.storedSandbox(name)
		return s == nil && r.storedPod(name) == nil, err
	}); err != nil {
		return "", unlessStopped(ctx, err, "leftover probe sandbox "+name), false
	}
	if uid, err = create(); err != nil {
		return "", apiOutcome(digest, err, doing, true), false
	}
	return uid, bootprobe.Outcome{}, true
}

// providersMountRecheck is how often a Pending probe pod's events are read for a providers Secret
// the kubelet cannot mount.
const providersMountRecheck = 15 * time.Second

// stuck is the verdict on a probe pod that has not finished and never will, or nil while waiting
// can still change it: an image the kubelet cannot use, or a providers Secret it cannot mount,
// which leaves the pod Pending, never Failed, and which its events say. They are read at most every
// providersMountRecheck; mountRead holds when they last were.
func (r *Runtime) stuck(ctx context.Context, pod *corev1.Pod, name, digest string, mountRead *time.Time) *bootprobe.Outcome {
	if reason := probeWaiting(pod); definitiveWaiting[reason] {
		unusable := imageRefusal(digest, "pod %s %s, container %s waiting: %s", name, phaseOf(pod), probeContainer, reason)
		return &unusable
	}
	if len(r.providerKeys) == 0 || pod.Status.Phase != corev1.PodPending || r.now().Sub(*mountRead) < providersMountRecheck {
		return nil
	}
	*mountRead = r.now()
	failure := r.providersMountFailure(ctx, pod)
	if failure == "" {
		return nil
	}
	refused := bootprobe.Outcome{Refusal: fmt.Errorf("the probe pod %s cannot mount the providers Secret %s, which provider_keys names (%s): %s",
		name, ProvidersSecretName(r.project), strings.Join(slices.Sorted(maps.Values(r.providerKeys)), ", "), failure)}
	return &refused
}

// providersMountFailure is the kubelet's FailedMount message for the providers volume when the
// Secret, or one of the keys it is asked for, is missing (`secret "<name>" not found` from the
// kubelet's watching secret manager, `secrets "<name>" not found` from the API server that one on
// the Get or Cache strategy asks, `references non-existent secret key: <key>`, pkg/volume/secret),
// or "" when the pod's events hold none. Every other FailedMount, such as `failed to sync secret
// cache: timed out waiting for the condition` while the kubelet's watch catches up, is one the
// kubelet retries, and is left to the budget.
func (r *Runtime) providersMountFailure(ctx context.Context, pod *corev1.Pod) string {
	events, err := r.podEvents(ctx, pod)
	if err != nil {
		return ""
	}
	for _, event := range events {
		if event.Reason != "FailedMount" || !strings.Contains(event.Message, `volume "`+providersVolume+`"`) {
			continue
		}
		secret := ProvidersSecretName(r.project)
		if strings.Contains(event.Message, `secret "`+secret+`" not found`) || strings.Contains(event.Message, `secrets "`+secret+`" not found`) ||
			strings.Contains(event.Message, "references non-existent secret key: ") {
			return event.Message
		}
	}
	return ""
}

// unfinished is the detail of an attempt whose pod did not finish within the budget: the pod's
// phase, its container's waiting reason, its events, and what its probe container logged so far,
// which names the probe it was in — or, with no pod, the Sandbox's Ready condition, where the
// controller reports why it made none.
func (r *Runtime) unfinished(ctx context.Context, name string, uid types.UID, budget time.Duration) string {
	pod := r.storedPod(name)
	if !ownedBy(pod, uid) {
		detail := fmt.Sprintf("probe sandbox %s has no pod after %s", name, budget)
		if s, err := r.storedSandbox(name); err == nil && s != nil {
			if ready := s.condition(conditionReady); ready != nil {
				detail += fmt.Sprintf("; sandbox Ready=%s %s: %s", ready.Status, ready.Reason, ready.Message)
			}
		}
		return detail
	}
	detail := fmt.Sprintf("probe pod %s still %s after %s", name, phaseOf(pod), budget)
	if reason := probeWaiting(pod); reason != "" {
		detail += fmt.Sprintf(" (container %s waiting: %s)", probeContainer, reason)
	}
	detail += "; events: " + r.events(ctx, pod)
	if tail, err := r.probeLog(ctx, name); err == nil && tail != "" {
		detail += "; log tail: " + tail
	}
	return detail
}

// probeWaiting is the probe container's waiting reason, when the pod reports one.
func probeWaiting(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == probeContainer && status.State.Waiting != nil {
			return status.State.Waiting.Reason
		}
	}
	return ""
}

// probeLog is the probe container's last log lines.
func (r *Runtime) probeLog(ctx context.Context, name string) (string, error) {
	reading, cancel := call(ctx)
	defer cancel()
	lines := int64(probeLogLines)
	stream, err := r.kube.CoreV1().Pods(r.namespace).GetLogs(name, &corev1.PodLogOptions{Container: probeContainer, TailLines: &lines}).Stream(reading)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	text, err := io.ReadAll(io.LimitReader(stream, 64<<10))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(text)), nil
}

// undefinedFlag is Go's flag package refusing a flag the probed CLI does not define.
var undefinedFlag = regexp.MustCompile(`flag provided but not defined: (-\S+)`)

// judge is the verdict on a finished probe pod and its log, logErr when the log could not be read
// (judgeProbeLog, worker-image-probe.ts:470-508), in this order. A pod the kubelet failed for
// reasons of its own (kubeletFailure) is transient, whatever of its log could be read, and an
// unread log is judged by the read's error. A Failed pod past those is one whose probe container
// exited on its own: the image's refusal. The OK line must confirm this daemon's contract: an
// image whose CLI predates the Go contract check prints none, having checked no contract, and is
// refused, not waved through; one that confirmed another contract is refused naming both. And it
// must say the prompt-named agents' models resolved: any other mark, or none, does not prove the
// workers run their agents on their models. An image whose CLI predates a flag the probe command
// passes stops at the flags, and its Failed pod is refused naming the flag its CLI lacks.
func (r *Runtime) judge(name, digest string, pod *corev1.Pod, logTail string, logErr error, contract int) bootprobe.Outcome {
	if why := kubeletFailure(pod); why != "" {
		// Whatever the container wrote before the kubelet ended it is quoted when it could be read.
		return bootprobe.Outcome{Detail: fmt.Sprintf("pod %s Failed: %s; the kubelet ended it, not the image — log tail: %s", name, why, logTail)}
	}
	if logErr != nil {
		return apiOutcome(digest, logErr, fmt.Sprintf("read probe pod %s's log", name), false)
	}
	if pod.Status.Phase == corev1.PodFailed {
		ended := ""
		for _, status := range pod.Status.ContainerStatuses {
			if t := status.State.Terminated; status.Name == probeContainer && t != nil {
				ended = fmt.Sprintf(" (container %s terminated: %s, exit code %d)", probeContainer, t.Reason, t.ExitCode)
			}
		}
		if flag := undefinedFlag.FindStringSubmatch(logTail); flag != nil {
			return imageRefusal(digest, "pod %s Failed%s: its legion CLI has no %s, a flag this daemon's probe passes: build the image from this daemon's commit — log tail: %s",
				name, ended, flag[1], logTail)
		}
		return imageRefusal(digest, "pod %s Failed%s — log tail: %s", name, ended, logTail)
	}
	if !strings.Contains(logTail, bootprobe.OKPrefix) {
		return imageRefusal(digest, "pod %s Succeeded without printing %s — log tail: %s", name, bootprobe.OKPrefix, logTail)
	}
	confirmed, ok := bootprobe.ConfirmedContract(logTail)
	if !ok {
		return imageRefusal(digest, "pod %s Succeeded without confirming Go daemon API contract %d (its legion CLI predates the check) — log tail: %s", name, contract, logTail)
	}
	if confirmed != contract {
		return imageRefusal(digest, "pod %s Succeeded but confirmed Go daemon API contract %d, this daemon requires %d — log tail: %s", name, confirmed, contract, logTail)
	}
	if mark := bootprobe.AgentModels(logTail); mark != bootprobe.AgentModelsResolved {
		if mark == "" {
			mark = "none"
		}
		return imageRefusal(digest, "pod %s Succeeded without resolving the prompt-named agents' models (its OK line's agent-models mark: %s, where the daemon's probe requires %s) — log tail: %s",
			name, mark, bootprobe.AgentModelsResolved, logTail)
	}
	r.log.Info("sandbox runtime: the worker image passed its probe", "image", r.image, "sandbox", name, "log", logTail)
	return bootprobe.Outcome{Passed: true}
}

// probeSandbox is a Sandbox with the lifecycle fields only the probe sets: upstream's Lifecycle,
// inlined in the spec (api/v1beta1/sandbox_types.go at v1.0.3).
type probeSandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              probeSpec `json:"spec"`
}

type probeSpec struct {
	PodTemplate    podTemplate `json:"podTemplate"`
	OperatingMode  string      `json:"operatingMode"`
	ShutdownPolicy string      `json:"shutdownPolicy"`
	ShutdownTime   metav1.Time `json:"shutdownTime"`
}

// probeManifest is the probe Sandbox: Running from the start (it holds no Secret to write first),
// deleted by the controller at shutdown, placed exactly as every worker is placed — the Legion
// pool, gVisor, the configured scheduling — with the workers' pod security and what the operator
// adds to every pod (its ServiceAccount, volumes, mounts, and variables, and the providers
// Secret's configured keys), and a single container running the image's Go `legion probe-image`
// against p's contract as a worker runs: on the pod's baseline (--pod-safety), loading the plugin
// from the root a pod loads it from (--plugin-root), with the providers Secret's keys exported as
// the worker's shim exports them (--provider-env-dir) when any are configured, and resolving the
// daemon's own role prompts' references (--role-references), which ProbeImage requires. Its
// command and env are escaped against the kubelet's expansion as every worker container's are
// (kubeletLiteral).
func (r *Runtime) probeManifest(name string, p ImageProbe, shutdown time.Time) probeSandbox {
	labels := map[string]string{labelProject: r.project, labelProbe: "image"}
	providers, providersMounts := r.providers()
	command := []string{r.tools.Legion, "probe-image", "--go-daemon-api-version", strconv.Itoa(p.Contract), "--plugin-root", legionPlugin, "--pod-safety"}
	if len(providersMounts) > 0 {
		command = append(command, "--provider-env-dir", ProvidersDir)
	}
	command = append(command, "--role-references", p.RoleReferences)
	container := corev1.Container{
		Name:            probeContainer,
		Image:           r.image,
		Command:         command,
		Env:             r.operatorEnv(),
		VolumeMounts:    slices.Concat(providersMounts, r.pod.VolumeMounts),
		Resources:       p.Resources,
		SecurityContext: restrictedContainer(),
	}
	kubeletLiteral(&container)
	return probeSandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.namespace, Labels: labels},
		Spec: probeSpec{
			OperatingMode:  modeRunning,
			ShutdownPolicy: "Delete",
			ShutdownTime:   metav1.NewTime(shutdown.UTC().Truncate(time.Second)),
			PodTemplate: podTemplate{
				Metadata: podMetadata{Labels: labels, Annotations: map[string]string{"karpenter.sh/do-not-disrupt": "true"}},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: new(int64(probeTerminationGrace)),
					AutomountServiceAccountToken:  new(false),
					ServiceAccountName:            r.pod.ServiceAccount,
					EnableServiceLinks:            new(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true), RunAsUser: new(int64(podUser)), RunAsGroup: new(int64(podUser)), FSGroup: new(int64(podUser)),
					},
					RuntimeClassName:  new(gvisor),
					NodeSelector:      r.nodeSelector(),
					Tolerations:       r.tolerations(),
					PriorityClassName: r.scheduling.PriorityClass,
					Volumes:           slices.Concat(providers, r.pod.Volumes),
					Containers:        []corev1.Container{container},
				},
			},
		},
	}
}

func encodeProbe(s probeSandbox) (*unstructured.Unstructured, error) {
	object, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&s)
	if err != nil {
		return nil, fmt.Errorf("encode probe sandbox %s: %w", s.Name, err)
	}
	return &unstructured.Unstructured{Object: object}, nil
}
