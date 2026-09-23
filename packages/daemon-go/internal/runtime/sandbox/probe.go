package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
// daemon never came back for (sandbox_controller.go:1712-1768 at v1.0.3); the daemon reads the log
// before then and deletes the Sandbox itself when an attempt ends. It carries the project label, so
// the runtime's informers see it and its pod, and the orphan sweep deletes a crashed boot's
// leftover. The caller runs the probe as a boot step that ends before supervision starts: a probe
// Sandbox that a sweep finds is such a leftover.
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
	// StateDir holds the pass cache, <StateDir>/image-probes/<digest hex>.json.
	StateDir string
	// Budget is how long one attempt waits for the probe pod to finish
	// (slow_command_timeout_seconds). The Sandbox's shutdownTime is the budget and one API call
	// after it, so the log of a pod that finished at the budget is still there to read.
	Budget time.Duration
	// Retry waits out the attempts that say nothing about the image: bootprobe.Daemon at boot.
	Retry bootprobe.Retry
	// APIServer is the API server the runtime's client talks to (rest.Config.Host). A pass is
	// remembered for this cluster and namespace only: a devbox daemon may keep one state directory
	// for a kind cluster and for production, and a pass on one proves nothing on the other.
	APIServer string
	// Resources are the probe container's requests and limits (the TypeScript probe used the
	// `small` profile): it runs Oh My Pi three times and exits. None when zero.
	Resources corev1.ResourceRequirements
}

// probePass is a pass the cache remembers (ImageProbeCacheSchema, worker-image-probe.ts:65-75):
// the image, the contract it confirmed, and the placement it was proven on.
type probePass struct {
	Digest             string    `json:"digest"`
	GoDaemonAPIVersion int       `json:"goDaemonApiVersion"`
	Placement          string    `json:"placement"`
	ProbedAt           time.Time `json:"probedAt"`
}

// ProbeImage proves the runtime's image (Options.Image) on the cluster before any claim runs on
// it, or refuses naming why. A pass is remembered per digest, contract, and placement (the cluster,
// the namespace, and the scheduling), so a crash-restart loop never launches a second probe for an
// image that already passed there; only a pass is remembered, so the boot after a refusal proves
// the fix. An attempt's verdict is definitive —
// the API refusing what was sent (400, 401, 403, 422, or a create's 404), an image the kubelet
// cannot use, a Failed pod, a log without the OK line or confirming another contract — or
// transient: anything else, retried under p.Retry (worker-image-probe.ts:318-338, 470-508).
func (r *Runtime) ProbeImage(ctx context.Context, p ImageProbe) error {
	if p.Contract < 1 || p.StateDir == "" || p.APIServer == "" || p.Budget <= 0 || p.Retry.Initial <= 0 || p.Retry.Max < p.Retry.Initial {
		return errors.New("image probe: a contract, a state directory, an API server, a positive budget, and a positive retry wait are required")
	}
	_, hex, _ := strings.Cut(r.image, "@sha256:")
	if !digestHex.MatchString(hex) {
		return fmt.Errorf("image probe: image %q is not pinned by a sha256 digest", r.image)
	}
	digest := "sha256:" + hex
	name := probeName(r.project, hex)
	cache := filepath.Join(p.StateDir, "image-probes", hex+".json")
	placement := r.placementFingerprint(p.APIServer)
	if r.passedBefore(cache, digest, p.Contract, placement) {
		return nil
	}
	return bootprobe.Run(ctx, "worker image", p.Retry, r.log, func(ctx context.Context) bootprobe.Outcome {
		outcome := r.probeAttempt(ctx, p, name, digest)
		if !outcome.Passed {
			return outcome
		}
		if err := writePass(cache, probePass{Digest: digest, GoDaemonAPIVersion: p.Contract, Placement: placement, ProbedAt: r.now().UTC()}); err != nil {
			return bootprobe.Outcome{Refusal: fmt.Errorf("worker image %s passed its probe, but the pass could not be recorded: %w", digest, err)}
		}
		return outcome
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

// placementFingerprint is where a probe proves the image: the cluster (its API server) and the
// namespace, then the scheduling, order-insensitive as Kubernetes reads it — the node selector (a
// JSON object's keys are sorted), the tolerations sorted, and the priority class
// (schedulingFingerprint, worker-image-probe.ts:77-99).
func (r *Runtime) placementFingerprint(apiServer string) string {
	tolerations := r.tolerations()
	slices.SortFunc(tolerations, func(a, b corev1.Toleration) int {
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		return bytes.Compare(left, right)
	})
	encoded, _ := json.Marshal(struct {
		APIServer     string              `json:"apiServer"`
		Namespace     string              `json:"namespace"`
		NodeSelector  map[string]string   `json:"nodeSelector"`
		Tolerations   []corev1.Toleration `json:"tolerations"`
		PriorityClass string              `json:"priorityClass,omitempty"`
	}{apiServer, r.namespace, r.nodeSelector(), tolerations, r.scheduling.PriorityClass})
	return string(encoded)
}

// passedBefore reports whether the cache holds a pass of digest at contract on this placement. A
// file that cannot be read or decoded, or that records another image, contract, or placement, is
// no pass: the probe runs again and rewrites it, and the log says why the file was ignored.
func (r *Runtime) passedBefore(cache, digest string, contract int, placement string) bool {
	raw, err := os.ReadFile(cache)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	ignore := func(why string) bool {
		r.log.Warn("sandbox runtime: ignoring the image probe cache", "file", cache, "why", why)
		return false
	}
	if err != nil {
		return ignore(err.Error())
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var pass probePass
	if err := decoder.Decode(&pass); err != nil {
		return ignore(err.Error())
	}
	switch {
	case pass.Digest != digest:
		return ignore(fmt.Sprintf("it records image %s, this runtime runs %s", pass.Digest, digest))
	case pass.GoDaemonAPIVersion != contract:
		return ignore(fmt.Sprintf("it records Go daemon API contract %d, this daemon speaks %d", pass.GoDaemonAPIVersion, contract))
	case pass.Placement != placement:
		return ignore("its placement (cluster, namespace, or scheduling) differs from this runtime's")
	}
	r.log.Info("sandbox runtime: the worker image passed its probe before; reusing the pass",
		"image", digest, "probedAt", pass.ProbedAt, "goDaemonApiVersion", contract, "file", cache)
	return true
}

// writePass records a pass through a temporary file renamed into place, so a crash mid-write
// never leaves half a file for the next boot to ignore.
func writePass(cache string, pass probePass) error {
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(pass, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(cache), filepath.Base(cache)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), cache)
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
		return encodeProbe(r.probeManifest(name, p.Contract, p.Resources, r.now().Add(p.Budget+apiTimeout)))
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
		seen     bool
		finished *corev1.Pod
		verdict  *bootprobe.Outcome
		readErr  error
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
		if reason := probeWaiting(pod); definitiveWaiting[reason] {
			unusable := imageRefusal(digest, "pod %s %s, container %s waiting: %s", name, phaseOf(pod), probeContainer, reason)
			verdict = &unusable
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
	logTail, err := r.probeLog(ctx, name)
	if err != nil {
		return apiOutcome(digest, err, fmt.Sprintf("read probe pod %s's log", name), false)
	}
	return r.judge(name, digest, finished, logTail, p.Contract)
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

// unfinished is the detail of an attempt whose pod did not finish within the budget: the pod's
// phase, its container's waiting reason, and its events — or, with no pod, the Sandbox's Ready
// condition, where the controller reports why it made none.
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
	return detail + "; events: " + r.events(ctx, pod)
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

// judge is the verdict a finished probe pod's log gives (judgeProbeLog, worker-image-probe.ts:
// 470-508). The OK line must confirm this daemon's contract: an image whose CLI predates the Go
// contract check prints none, having checked no contract, and is refused, not waved through; one
// that confirmed another contract is refused naming both.
func (r *Runtime) judge(name, digest string, pod *corev1.Pod, logTail string, contract int) bootprobe.Outcome {
	if pod.Status.Phase == corev1.PodFailed {
		ended := ""
		for _, status := range pod.Status.ContainerStatuses {
			if t := status.State.Terminated; status.Name == probeContainer && t != nil {
				ended = fmt.Sprintf(" (container %s terminated: %s, exit code %d)", probeContainer, t.Reason, t.ExitCode)
			}
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
// pool, gVisor, the configured scheduling — with the workers' pod security, and a single container
// running the image's Go `legion probe-image` against contract.
func (r *Runtime) probeManifest(name string, contract int, resources corev1.ResourceRequirements, shutdown time.Time) probeSandbox {
	labels := map[string]string{labelProject: r.project, labelProbe: "image"}
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
					EnableServiceLinks:            new(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true), RunAsUser: new(int64(podUser)), RunAsGroup: new(int64(podUser)), FSGroup: new(int64(podUser)),
					},
					RuntimeClassName:  new(gvisor),
					NodeSelector:      r.nodeSelector(),
					Tolerations:       r.tolerations(),
					PriorityClassName: r.scheduling.PriorityClass,
					Containers: []corev1.Container{{
						Name:            probeContainer,
						Image:           r.image,
						Command:         []string{r.tools.Legion, "probe-image", "--go-daemon-api-version", strconv.Itoa(contract)},
						Resources:       resources,
						SecurityContext: restrictedContainer(),
					}},
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
