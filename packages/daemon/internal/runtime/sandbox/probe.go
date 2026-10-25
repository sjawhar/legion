package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
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
	"k8s.io/client-go/rest"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// The image probe, as a Sandbox: a daemon off the cluster is not the image its workers run, so it
// proves the image by running the image's own `legion probe-image --daemon-api-version <N>` in a
// Sandbox of that image, placed as every worker is placed, and reading the OK line back from the
// pod's log.
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
	// probeLogLines is how much of the probe pod's log is read and quoted. The capability table
	// (internal/capabilities) is one line per row, about twenty, printed before the OK line, so a
	// 50-line tail would leave a refused image's own messages, printed before the table, out of the
	// tail the refusal quotes. The
	// tail probeLog keeps (probeLogBytes) is kept from its end (lastBytes), so the OK line, the
	// tail's last line, is in reach whatever a line's length: a capability line is its prefix,
	// the row's own words, and a command's quoted output cut at 300 characters
	// (capabilities.maxDetail), about 500 bytes at the longest, so a hundred such lines stay near
	// 50 KiB, and a passing pod prints about twenty. PodLogOptions.LimitBytes is not used for the
	// bound: it counts from the start of the returned tail too, the same cutoff lastBytes avoids.
	probeLogLines = 100
	// probeLogReadBytes is the most probeLog reads from the API: 100 lines of up to 10 KiB apiece,
	// comfortably past what probeLogLines ever holds, bounding the read whatever a line's length
	// turned out to be (io.LimitReader on the stream).
	probeLogReadBytes = 1 << 20
	// probeLogBytes is what probeLog keeps of that read, and what a refusal quotes: the last
	// probeLogBytes of the tail (lastBytes), so a long line early in the tail never pushes the OK
	// line, the tail's last line, out of reach.
	probeLogBytes = 64 << 10
	// probeTerminationGrace is the probe pod's terminationGracePeriodSeconds: it holds nothing to
	// save.
	probeTerminationGrace = 5
)

// definitiveWaiting are the container waiting reasons no retry changes: the image reference itself
// is unusable.
var definitiveWaiting = map[string]bool{"InvalidImageName": true, "ErrImageNeverPull": true}

// pullWaiting are the waiting reasons of a pull that failed (ErrImagePull) and of the back-off
// after it (ImagePullBackOff), whose message carries the failed pull's error after `Back-off
// pulling image "<image>": ErrImagePull: ` (pkg/kubelet/images/image_manager.go, EnsureImageExists,
// prevPullErrMsg). Either reason alone also covers a registry that refused the node's credentials,
// one the node could not reach, and one that rate-limited it, so only the message decides
// (registryHasNoImage).
var pullWaiting = map[string]bool{"ErrImagePull": true, "ImagePullBackOff": true}

// imageNotFound is containerd's answer to a pull whose reference the registry does not have: its
// CRI image service wraps the pull's error as `failed to pull and unpack image "<ref>": …`
// (internal/cri/server/images/image_pull.go at v2, pkg/cri/server/image_pull.go at v1.7). A local
// pull wraps a resolution's as `failed to resolve reference "<ref>": …` (client/pull.go at v2,
// pull.go at v1.7); a pull through containerd's transfer service, the CRI's default from 2.1 unless
// its configuration asks for something the service lacks (internal/cri/config/config.go,
// CheckLocalImagePullConfigs), wraps it as `failed to resolve image: …` (core/transfer/local/
// pull.go). The failure reaches the kubelet as gRPC NotFound only when it is errdefs.ErrNotFound.
// The resolver's Resolve (core/remotes/docker/resolver.go at v2, remotes/docker/resolver.go at
// v1.7) answers `<ref>: not found` when the registry answered 404. An unauthorized pull (`pull
// access denied, …`), a rate limit and a server error carry another code or none. The resolver's
// other ErrNotFound answers say nothing of the registry's contents — `no resolve hosts: not found`
// (the node's registry configuration) and `rejecting <n> byte manifest for <ref>: not found` (a
// manifest past containerd's size limit) — nor does a NotFound past resolution, `failed to copy: …
// not found`, a manifest or layer the registry named and then did not serve; so the whole message
// must be the one grammar, every reference in it the same (namesOneReference).
//
// One answer is not enough. containerd 2 resolves a digest as a manifest and then as a blob, and
// ranks a 404 above a request that failed, so a manifest request the network cut followed by the
// blob request's 404 — which a registry answers for a manifest it holds, GHCR among them — reads as
// this answer too. The probe therefore refuses only once the kubelet has met it on two pulls
// (registryHasNoImage): each pull resolves afresh, and the kubelet's second comes about ten seconds
// after the first (its image back-off's first step). On a node configured with several hosts for
// one registry (a mirror in containerd's hosts.toml), containerd 2 likewise reports one host's 404
// over another host's failed request — a refused connection, a timeout, or credentials the host
// refused (`pull access denied`, `authorization failed`, `failed to authorize`) — so a mirror that
// lacks the image while the registry behind it stays unreachable or keeps refusing the node's
// credentials is refused as a missing image; the refusal quotes the kubelet, which names the
// reference, and a mirror that lags its registry is the node's to fix.
//
// Measured on kind with containerd 2.0.2 (Kubernetes v1.32.2), 2.1.3 (v1.34.0) and 2.3.4 (v1.37.0),
// whose kind configuration (`discard_unpacked_layers`) keeps the local pull, against public
// registries, and on 2.0.2 against a stub answering each status: `rpc error: code = NotFound desc =
// failed to pull and unpack image "<ref>": failed to resolve reference "<ref>": <ref>: not found`
// for the 404 on each, the wording a GKE node's Failed event quotes for a digest its registry
// lacks; and on 2.3.4 with that setting removed, so the transfer service pulls: `rpc error: code =
// NotFound desc = failed to pull and unpack image "<ref>": failed to resolve image: <ref>: not
// found`, while a repository Docker Hub does not have read `… failed to resolve image: pull access
// denied, …` with no code. Docker Hub answers such a repository with 401, read as credentials and
// retried; another runtime's wording (CRI-O's `manifest unknown`) falls to the retry too.
const imageNotFound = `rpc error: code = NotFound desc = failed to pull and unpack image "([^"]+)": (?:failed to resolve reference "([^"]+)"|failed to resolve image): (.+): not found$`

// pullNotFound matches imageNotFound as a pull's waiting message, alone or after the back-off's
// `Back-off pulling image "<ref>": ErrImagePull: ` (pkg/kubelet/images/image_manager.go,
// EnsureImageExists, prevPullErrMsg); pullFailedNotFound matches it as the kubelet's Failed event
// for the pull, `Failed to pull image "<ref>": <error>` (the same function's FailedToPullImage).
var (
	pullNotFound       = regexp.MustCompile(`^(?:Back-off pulling image "[^"]+": ErrImagePull: )?` + imageNotFound)
	pullFailedNotFound = regexp.MustCompile(`^Failed to pull image "[^"]+": ` + imageNotFound)
)

// namesOneReference reports whether message matches pattern with every reference imageNotFound
// captures the same: the image's, the resolved reference's on a local pull (none on the transfer
// service's), and the one the resolver says it did not find.
func namesOneReference(pattern *regexp.Regexp, message string) bool {
	m := pattern.FindStringSubmatch(message)
	return m != nil && m[1] == m[3] && (m[2] == "" || m[2] == m[1])
}

// registryHasNoImage reports whether the probe container's waiting state w is a pull the registry
// refused because it does not have the image (pullNotFound), and the pod's events show the kubelet
// met that answer on two pulls or more: the Failed events whose message is it, counted as the
// kubelet folds a repeat (eventCount). The first answer alone is no proof (imageNotFound).
func registryHasNoImage(w *corev1.ContainerStateWaiting, events []corev1.Event) bool {
	if w == nil || !pullWaiting[w.Reason] || !namesOneReference(pullNotFound, w.Message) {
		return false
	}
	var pulls int32
	for _, event := range events {
		if event.Reason == "Failed" && namesOneReference(pullFailedNotFound, event.Message) {
			pulls += eventCount(event)
		}
	}
	return pulls >= 2
}

// digestHex is a sha256 digest's 64 hex.
var digestHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ImageProbe is how ProbeImage proves the runtime's worker image.
type ImageProbe struct {
	// Contract is the daemon API contract the image's plugin must declare: the daemon's own
	// DaemonAPIVersion.
	Contract int
	// Budget is how long one attempt waits for the probe pod to finish: slow_command_timeout_seconds,
	// or, when worker_boot_timeout_seconds leaves it less than a minute of room, worker_boot_timeout_seconds
	// plus that minute instead (internal/daemon's ProbeBudget), so the capacity stop below (stuck)
	// can always see a probe pod's Unschedulable age pass the boot timeout within one attempt. The
	// Sandbox's shutdownTime is the budget and one API call after it, so the log of a pod that
	// finished at the budget is still there to read.
	Budget time.Duration
	// Retry waits out the attempts that say nothing about the image: bootprobe.Daemon at boot.
	Retry bootprobe.Retry
	// RoleReferences are the task agents and skills the daemon's own role prompts name, required:
	// the prompts every worker pod is handed, so the probe resolves those and their agents' models,
	// not the image's copy. probeManifest encodes them for `legion probe-image --role-references`,
	// and ProbeImage refuses, before any pod runs, a value whose encoding the image's Decode would
	// refuse (the zero Names, a kind left nil, a name no prompt can write): a probe pod handed one
	// would fail, and the failure would be blamed on the image.
	RoleReferences promptrefs.Names
	// Resources are the probe container's requirements, as the daemon hands them: the probe's own
	// fixed reservation (internal/daemon/kubernetes.go, probeReservation), its cpu and memory the
	// request and the limit alike, so the probe pod is Guaranteed whatever the roles request. It
	// runs Oh My Pi three times, one at a time (pi.agents, the plugin's load, the session-storage
	// setting), and exits, so it is sized for that and not for a role's work.
	Resources corev1.ResourceRequirements
}

// ProbeImage proves the runtime's image (Options.Image) on the cluster before any claim runs on
// it, answering what the probe's OK line reported of the image and how the run went
// (bootprobe.ImageReport), or refuses naming why. Every boot probes: no pass is remembered, because
// the probe pod's spec cannot show the contents of the operator's ConfigMaps and Secrets, which
// decide whether the prompt-named agents' models resolve (the LEGION-270 plan, decision 5). An
// attempt's verdict is definitive — the API refusing what was sent (400, 401, 403, 422, or a
// create's 404), an image the kubelet cannot use, a pod whose probe container exited on its own
// and Failed, a log without the OK line or confirming another contract, or not resolving the
// agents' models, or not checking the capability list, a ConfigMap or Secret the pod cannot mount,
// or its container cannot be configured for — or waiting: a pod the pool has no room for (stuck),
// which counts against no attempt budget, since a full pool says nothing about the image and the
// daemon must stay up until it has room — or transient: anything else, a pod the kubelet itself
// failed included, retried under p.Retry. The report counts the attempts that waited on capacity
// and keeps the last one's reason, for the
// pool-capacity row of the capability report.
func (r *Runtime) ProbeImage(ctx context.Context, p ImageProbe) (bootprobe.ImageReport, error) {
	if p.Contract < 1 || p.Budget <= 0 || p.Retry.Initial <= 0 || p.Retry.Max < p.Retry.Initial {
		return bootprobe.ImageReport{}, errors.New("image probe: a contract, a positive budget, and a positive retry wait are required")
	}
	if p.RoleReferences.Zero() {
		return bootprobe.ImageReport{}, errors.New("image probe: ImageProbe.RoleReferences is required: the references of the role prompts a pod is handed")
	}
	if _, err := promptrefs.Decode(p.RoleReferences.Encode()); err != nil {
		return bootprobe.ImageReport{}, fmt.Errorf("image probe: ImageProbe.RoleReferences: %w", err)
	}
	_, hex, _ := strings.Cut(r.image, "@sha256:")
	if !digestHex.MatchString(hex) {
		return bootprobe.ImageReport{}, fmt.Errorf("image probe: image %q is not pinned by a sha256 digest", r.image)
	}
	digest := "sha256:" + hex
	name := probeName(r.project, hex, r.probeRun)
	// The report is the passing attempt's (Outcome.Report): a transient attempt reports nothing,
	// and a refusal ends the run. The capacity waits are the run's own count.
	var report bootprobe.ImageReport
	waits, reason := 0, ""
	err := bootprobe.Run(ctx, "worker image", p.Retry, r.log, func(ctx context.Context) bootprobe.Outcome {
		outcome := r.probeAttempt(ctx, p, name, digest)
		switch {
		case outcome.Passed:
			report = outcome.Report
		case outcome.Waiting != "":
			waits, reason = waits+1, outcome.Waiting
		}
		return outcome
	})
	if err != nil {
		return bootprobe.ImageReport{}, err
	}
	report.CapacityWaits, report.CapacityReason = waits, reason
	return report, nil
}

// probeName is `legion-probe-<project>-<first 12 hex of the digest>`, with `-<run>` after it when
// run is set. The daemon's runtime sets no run: its probe is one per project and image, so a
// crashed boot's leftover is found by name and replaced rather than accumulating, and two projects
// probing one image in a namespace never contend for one Sandbox. A Prober sets a run of its own
// (newProber), so its Sandbox is never a booting daemon's or another probe-boot's, and neither
// takes the other's for a leftover (createProbe); a probe-boot leftover ends at its shutdownTime.
// The project is slugged to fit a DNS label (probePodName), shorter by what the run takes.
func probeName(project, hex, run string) string {
	prefix, suffix := "legion-probe-", "-"+hex[:12]
	if run != "" {
		suffix += "-" + run
	}
	return prefix + dnsName(project, maxNameLength-len(prefix)-len(suffix)) + suffix
}

// Prober is the runtime for the image probe alone: `legion probe-boot` builds it to run the probe a
// booting daemon runs (ProbeImage) from the daemon's own configuration, under the caller's
// credentials, with no workflow behind it. Its probe Sandbox is named for its own run (probeName),
// so it may run while that project's daemon boots, or beside another probe-boot.
type Prober struct{ r *Runtime }

// NewProber builds a Prober over rc's clients (clients, newProber).
func NewProber(ctx context.Context, rc *rest.Config, opts Options) (*Prober, error) {
	c, err := clients(rc)
	if err != nil {
		return nil, err
	}
	return newProber(ctx, c, opts)
}

// newProber builds a Prober from opts, which need none of what only supervision reads
// (configureProber), draws its probe run, and starts its Sandbox and pod informers for ctx's
// lifetime as New does, and nothing else: no refresher of the roles' gh files, since it writes no
// role's Secret.
func newProber(ctx context.Context, c runtimeClients, opts Options) (*Prober, error) {
	r, err := configureProber(opts)
	if err != nil {
		return nil, err
	}
	run := make([]byte, 4)
	if _, err := rand.Read(run); err != nil {
		return nil, fmt.Errorf("sandbox runtime: draw the probe run: %w", err)
	}
	r.probeRun = fmt.Sprintf("%x", run)
	if err := r.startInformers(ctx, c); err != nil {
		return nil, err
	}
	return &Prober{r: r}, nil
}

// ProbeImage is Runtime.ProbeImage: the same probe Sandbox, under the Prober's own name, judged the
// same way.
func (p *Prober) ProbeImage(ctx context.Context, probe ImageProbe) (bootprobe.ImageReport, error) {
	return p.r.ProbeImage(ctx, probe)
}

// probeAttempt is one run of the probe Sandbox: create it (replacing this project's leftover),
// wait up to the budget for its pod to finish, read the pod's log, judge it, and delete the
// Sandbox whatever happened — with a context of its own, so a stopping daemon still cleans up.
func (r *Runtime) probeAttempt(ctx context.Context, p ImageProbe, name, digest string) bootprobe.Outcome {
	// A pod an earlier attempt's Sandbox left, whose deletion is still under way, holds the name.
	if err := r.await(ctx, p.Budget, "the pod of an earlier probe sandbox "+name+" to go", func(context.Context) (bool, error) {
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
		// Detached from ctx, which may be what ended the probe, so nothing but this bound ends the
		// delete; it covers the wait on the client's limiter too. A delete it cuts short leaves a
		// Sandbox the next probe replaces (createProbe), and the Sandbox's shutdown time ends it
		// before then.
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
	err := r.await(ctx, p.Budget, "probe pod "+name+" to finish", func(context.Context) (bool, error) {
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
// definitive; anything else is the cluster's moment, retried.
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
// a leftover of this runtime's own probe only when it carries this project's label — the name is
// project-scoped, so anything else is another deployment's object, not this runtime's to delete —
// in which case it is deleted, its pod waited out, and the create repeated. The name is the
// daemon's for every boot of the project and image, and a Prober's for its run alone (probeName),
// so neither ever meets the other's Sandbox here.
func (r *Runtime) createProbe(ctx context.Context, name, digest string, manifest func() (*unstructured.Unstructured, error), budget time.Duration) (types.UID, bootprobe.Outcome, bool) {
	create := func() (types.UID, error) {
		object, err := manifest()
		if err != nil {
			return "", err
		}
		created, err := r.sandboxClient().Create(ctx, object, metav1.CreateOptions{})
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
	existing, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
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
	background := metav1.DeletePropagationBackground
	err = r.sandboxClient().Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &background, Preconditions: &metav1.Preconditions{UID: &leftover},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return "", apiOutcome(digest, err, "delete the leftover probe sandbox "+name, false), false
	}
	if err := r.await(ctx, budget, "the leftover probe sandbox "+name+" and its pod to go", func(context.Context) (bool, error) {
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

// mountRecheck is how often a scheduled Pending probe pod's events are read for a mount the kubelet
// cannot make: the providers Secret, or an operator volume (a ConfigMap, Secret or share
// runtime.kubernetes.pod.volumes gives the pod). Each read is an event list against the API
// server, so they are not made on every poll of the store; fifteen seconds still finds the
// kubelet's first FailedMount, which it records within seconds of binding the pod, early in a
// budget minutes long.
const mountRecheck = 15 * time.Second

// stuck is the verdict on a probe pod that has not finished and never will, or nil while waiting
// can still change it. It stops on an image the kubelet cannot use (definitiveWaiting), the
// image's own refusal; on an image the registry does not have, from the pod's status and events
// (registryHasNoImage), which no retry of the pull changes; on a container the kubelet cannot
// configure, waiting
// CreateContainerConfigError with a message saying a ConfigMap or Secret the container reads, or a
// key of one, is missing (missingObject) — the kubelet builds the container's env from those
// objects at each sync (pkg/kubelet/kubelet_pods.go, makeEnvironmentVariables) and fails the same
// way until the operator creates what is missing, so this is a refusal quoting the message and not
// the image's fault, while a CreateContainerConfigError saying anything else is left to the budget;
// on a pod the pool has no room for, which the scheduler leaves Pending with PodScheduled=False
// Unschedulable, waited on (bootprobe.Outcome.Waiting) once it has been so for longer than the boot
// timeout — the same bound the liveness rules give an issue pod (observe.go), long enough for
// Karpenter to add a node under the pool's limits when it can, so a cold pool scaling up is not
// mistaken for a full one — rather than counted against the attempt budget, since a full pool says
// nothing about the image — reachable within the attempt by construction, since the attempt's
// budget (ImageProbe.Budget, internal/daemon's ProbeBudget) always clears the boot timeout by at
// least a minute; and on a mount the kubelet cannot make, which leaves the pod Pending, never
// Failed, and which only its events say: the providers Secret (providersMountFailure) or an
// operator volume (missingVolume), each a refusal naming the object and quoting the kubelet.
//
// Events are read only once the pod is scheduled (pod.Spec.NodeName != ""): the kubelet mounts
// only a pod bound to it, so an unscheduled pod has no FailedMount to find, and the capacity stop,
// whose pod holds no node, costs no read. They are read for every scheduled Pending probe pod at
// most every mountRecheck — mountRead holds when they last were — one podEvents call per read
// feeding the missing-image rule first (a pull's Failed events), then the providers matcher, when
// the pod mounts the providers Secret, then the general matcher over the same list. A FailedMount
// of any other form (the kubelet's ConfigMap or Secret watch not yet synced, the object not yet
// registered with its manager, the pod's volumes not all mounted yet) is one the kubelet retries,
// and is left to the budget.
//
// Ending the attempt on an Unschedulable pod deletes its Sandbox (probeAttempt's deferred delete)
// and the next attempt, after the retry's longest wait, creates it again: a probe pod that could
// not be placed holds no node, so recreating it costs the pool nothing, and the new pod asks the
// scheduler afresh rather than keeping one object Pending across the daemon's whole wait.
func (r *Runtime) stuck(ctx context.Context, pod *corev1.Pod, name, digest string, mountRead *time.Time) *bootprobe.Outcome {
	w := probeWaiting(pod)
	if w != nil && definitiveWaiting[w.Reason] {
		unusable := imageRefusal(digest, "pod %s %s, container %s waiting: %s", name, phaseOf(pod), probeContainer, w.Reason)
		return &unusable
	}
	if w != nil && w.Reason == "CreateContainerConfigError" && missingObject(w.Message) {
		refused := bootprobe.Outcome{Refusal: fmt.Errorf("the probe pod %s cannot configure its container %s (waiting: %s), a ConfigMap or Secret it reads being missing: %s",
			name, probeContainer, w.Reason, w.Message)}
		return &refused
	}
	if pod.Status.Phase != corev1.PodPending {
		return nil
	}
	if scheduled := podCondition(pod, corev1.PodScheduled); scheduled != nil &&
		scheduled.Status == corev1.ConditionFalse && scheduled.Reason == corev1.PodReasonUnschedulable {
		if claim, message := r.missingClaim(scheduled.Message); claim != "" {
			return r.volumeRefusal(name, claim, message)
		}
		if strings.Contains(scheduled.Message, unboundClaims) && slices.ContainsFunc(r.pod.Volumes, func(v corev1.Volume) bool { return v.PersistentVolumeClaim != nil }) {
			return nil
		}
		if strings.Contains(scheduled.Message, readWriteOncePodInUse) {
			if claims := r.blockClaims(); len(claims) != 0 {
				refused := bootprobe.Outcome{Refusal: fmt.Errorf("the probe pod %s cannot be placed: a ReadWriteOncePod claim it mounts is held by another pod, and every Legion pod mounts the claims runtime.kubernetes.pod.volumes gives volumes %s from, so they must be ReadWriteMany: %s",
					name, strings.Join(claims, ", "), scheduled.Message)}
				return &refused
			}
		}
		since := scheduled.LastTransitionTime.Time
		if since.IsZero() {
			since = pod.CreationTimestamp.Time
		}
		if r.now().Sub(since) > r.bootTimeout {
			waiting := bootprobe.Outcome{Waiting: fmt.Sprintf("probe pod %s is Unschedulable: %s", name, scheduled.Message)}
			return &waiting
		}
	}
	if pod.Spec.NodeName == "" || r.now().Sub(*mountRead) < mountRecheck {
		return nil
	}
	*mountRead = r.now()
	events, err := r.podEvents(ctx, pod)
	if err != nil {
		return nil
	}
	if registryHasNoImage(w, events) {
		missing := imageRefusal(digest, "the registry does not have image %s: pod %s %s, container %s waiting: %s: %s",
			r.image, name, phaseOf(pod), probeContainer, w.Reason, w.Message)
		return &missing
	}
	// Every pod mounts the providers Secret, the probe's included: the session database's URL key
	// is required, so a Secret the kubelet cannot mount refuses the boot here.
	secret := ProvidersSecretName(r.project)
	if failure := providersMountFailure(events, secret); failure != "" {
		refused := bootprobe.Outcome{Refusal: fmt.Errorf("the probe pod %s cannot mount the providers Secret %s, whose keys every pod mounts (%s): %s",
			name, secret, strings.Join(r.providersKeys(), ", "), failure)}
		return &refused
	}
	if volume, message := missingVolume(events); message != "" {
		return r.blockVolumeRefusal(pod, name, volume, message)
	}
	if refused := r.shareRefusal(pod, name, events); refused != nil {
		return refused
	}
	return nil
}

// providersMountFailure is the kubelet's FailedMount message, among events, for the providers
// volume when secret (the providers Secret), or one of the keys it is asked for, is missing
// (`secret "<name>" not found` from the kubelet's watching secret manager, `secrets "<name>" not
// found` from the API server that one on the Get or Cache strategy asks, `references non-existent
// secret key: <key>`, pkg/volume/secret), or "" when the events hold none. Every other FailedMount,
// such as `failed to sync secret cache: timed out waiting for the condition` while the kubelet's
// watch catches up, is one the kubelet retries, and is left to the budget.
func providersMountFailure(events []corev1.Event, secret string) string {
	for _, event := range events {
		if event.Reason != "FailedMount" || !strings.Contains(event.Message, `volume "`+providersVolume+`"`) {
			continue
		}
		if strings.Contains(event.Message, `secret "`+secret+`" not found`) || strings.Contains(event.Message, `secrets "`+secret+`" not found`) ||
			strings.Contains(event.Message, "references non-existent secret key: ") {
			return event.Message
		}
	}
	return ""
}

// mountedVolume is the volume a kubelet FailedMount event names, `MountVolume.SetUp failed for
// volume "<name>" : <error>` (pkg/volume/util/operationexecutor's GenerateMsgDetailed, quoting the
// volume's name as the pod spec gives it).
var mountedVolume = regexp.MustCompile(`volume "([^"]+)"`)

// missingVolume is the first FailedMount among events naming a volume other than the providers
// one, whose message says a ConfigMap or Secret, or a key of one, is missing (missingObject), as
// its volume's name and the message whole; or "" and "" when the events hold none. A FailedMount
// naming no volume, such as the kubelet's `Unable to attach or mount volumes` summary of a pod's
// still-pending mounts, is left to the budget, and one on the providers volume to the providers
// rule (providersMountFailure).
func missingVolume(events []corev1.Event) (volume, message string) {
	for _, event := range events {
		if event.Reason != "FailedMount" {
			continue
		}
		named := mountedVolume.FindStringSubmatch(event.Message)
		if named == nil || named[1] == providersVolume || !missingObject(event.Message) {
			continue
		}
		return named[1], event.Message
	}
	return "", ""
}

// missingObjectForm is the kubelet's wording for a ConfigMap or Secret that is not there, with the
// object's name — the one piece that varies — a wildcard: `configmap "<name>" not found` and
// `secret "<name>" not found` from its watching object manager (pkg/kubelet/util/manager, each a
// NotFound on the singular resource), `configmaps "<name>" not found` and `secrets "<name>" not
// found` from the API server when the manager is on the Get or Cache strategy and asks it, and
// `couldn't find key <key> in ConfigMap <namespace>/<name>` or `… in Secret <namespace>/<name>`
// from the container's env (pkg/kubelet/kubelet_pods.go, makeEnvironmentVariables). The other
// pieces are literal, so a reworded message falls back to the retry.
var missingObjectForm = regexp.MustCompile(`(configmaps?|secrets?) "[^"]*" not found|couldn't find key \S+ in (ConfigMap|Secret) `)

// missingObject reports whether message, a FailedMount event's or a CreateContainerConfigError
// waiting state's, says a ConfigMap or Secret, or a key the pod asks of one, is missing: one of
// missingObjectForm's, or a volume's key that is not in the object it mounts (`configmap references
// non-existent config key: <key>`, pkg/volume/configmap; `references non-existent secret key:
// <key>`, pkg/volume/secret). Each is one the kubelet repeats at every sync until the operator
// creates what is missing, so none is worth the budget.
func missingObject(message string) bool {
	return missingObjectForm.MatchString(message) ||
		strings.Contains(message, "configmap references non-existent config key: ") ||
		strings.Contains(message, "references non-existent secret key: ")
}

// missingClaimForm is the scheduler's wording for a pod whose PersistentVolumeClaim is not there,
// is going, or has lost its volume, with the claim's name the one piece that varies:
// `persistentvolumeclaim "<name>" not found` (the claim lister's NotFound, which the volumebinding
// plugin's PreFilter returns as it is), `persistentvolumeclaim "<name>" is being deleted` and
// `persistentvolumeclaim "<name>" bound to non-existent persistentvolume "<pv>"`
// (pkg/scheduler/framework/plugins/volumebinding/volume_binding.go, podHasPVCs). The scheduler
// repeats each at every attempt until the operator fixes the claim, so none is worth the budget,
// nor a capacity wait: no node the pool adds can place the pod.
var missingClaimForm = regexp.MustCompile(`persistentvolumeclaim "([^"]+)" (not found|is being deleted|bound to non-existent persistentvolume)`)

// missingClaim is the operator volume, and the scheduler's message whole, when message — an
// Unschedulable probe pod's PodScheduled condition — says a claim one of the pod block's
// persistent_volume_claim volumes names is missing or being deleted; "" and "" otherwise, so a
// claim the pod block does not name (one Legion made) is never blamed on the operator, and any
// other wording is left to the capacity rule. The scheduler names the claim, not the volume, so
// the volume is found by its claim.
func (r *Runtime) missingClaim(message string) (volume, quoted string) {
	named := missingClaimForm.FindStringSubmatch(message)
	if named == nil {
		return "", ""
	}
	for _, v := range r.pod.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == named[1] {
			return v.Name, message
		}
	}
	return "", ""
}

// unboundClaims is the scheduler's wording for a pod one of whose PersistentVolumeClaims exists
// but is bound to no volume yet (volume_binding.go, PreFilter: `pod has unbound immediate
// PersistentVolumeClaims`). No node the pool adds places such a pod, so it is no capacity wait;
// the claim may still bind, so it is no refusal either, and the attempt waits on it to its budget.
const unboundClaims = "pod has unbound immediate PersistentVolumeClaims"

// readWriteOncePodInUse is the scheduler's wording for a pod whose ReadWriteOncePod claim another
// pod holds (pkg/scheduler/framework/plugins/volumerestrictions/volume_restrictions.go,
// ErrReasonReadWriteOncePodConflict). Every Legion pod mounts the pod block's claims at once, so
// while any issue pod runs, the probe pod waits on it forever: a refusal, not a capacity wait.
const readWriteOncePodInUse = "PersistentVolumeClaim with ReadWriteOncePod access mode already in-use by another pod"

// blockClaims names the pod block's persistent_volume_claim volumes, in its order.
func (r *Runtime) blockClaims() []string {
	var claims []string
	for _, v := range r.pod.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims = append(claims, v.Name)
		}
	}
	return claims
}

// podHasVolume reports whether the admitted pod's spec carries a volume of that name: one the
// runtime sent, or one an admission webhook added, which is never a PersistentVolume's name.
func podHasVolume(pod *corev1.Pod, volume string) bool {
	return slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == volume })
}

// shareRefusal is the refusal for the newest FailedMount among events whose mount of a share the
// pod block gives the pod (an nfs volume, or a persistent_volume_claim one) ran and failed for good
// (shareMountFailed) and is still the pod's state, or nil when the events hold none. A share's
// mount is the network's and the server's — an export that does not exist, a server that refuses
// the node or never answers, a CSI driver's final error — and the kubelet repeats it at every sync
// with the same outcome; no image fixes it, so it is said once, naming the volume and quoting the
// kubelet, rather than retried to the budget.
//
// The events are the pod's history, not its state: a failure the kubelet's next try cleared stays
// listed until the event expires. The kubelet pulls a pod's images only once every volume is
// mounted (pkg/kubelet/kubelet.go, SyncPod: volumeManager.WaitForAttachAndMount before
// containerRuntime.SyncPod), so a share failure older than the pod's last Pulling or Pulled event
// is one a later try fixed, and is not read (sharesMountedSince).
//
// The kubelet names a volume as its spec does (volume.Spec.Name, pkg/volume/plugins.go): an nfs
// volume by the pod's own name for it, a claim's by the PersistentVolume it is bound to, which
// blockVolumeRefusal maps back to the pod block's claim. A name the admitted pod's spec carries but
// the block does not (a volume an admission webhook injected) is no share of the block's, nor is
// any name when the block names no claim; both are left to the budget.
func (r *Runtime) shareRefusal(pod *corev1.Pod, name string, events []corev1.Event) *bootprobe.Outcome {
	mounted := sharesMountedSince(events)
	claims := len(r.blockClaims())
	var newest *corev1.Event
	for i := range events {
		event := &events[i]
		if event.Reason != "FailedMount" || !shareMountFailed(event.Message) || !eventTime(*event).After(mounted) ||
			(csiFailure(event.Message) && eventCount(*event) < 2) {
			continue
		}
		named := mountedVolume.FindStringSubmatch(event.Message)
		if named == nil || named[1] == providersVolume {
			continue
		}
		v, ok := r.blockVolumeNamed(named[1])
		switch {
		case ok && v.NFS == nil && v.PersistentVolumeClaim == nil:
			continue // a ConfigMap, Secret or projected volume: missingVolume's to judge
		case !ok && (claims == 0 || podHasVolume(pod, named[1])):
			continue
		}
		if newest == nil || eventTime(*event).After(eventTime(*newest)) {
			newest = event
		}
	}
	if newest == nil {
		return nil
	}
	return r.blockVolumeRefusal(pod, name, mountedVolume.FindStringSubmatch(newest.Message)[1], newest.Message)
}

// sharesMountedSince is when the kubelet last began or finished pulling one of the pod's images
// (a Pulling or Pulled event), which it does only once every volume is mounted, or the zero time
// when it has not yet.
func sharesMountedSince(events []corev1.Event) time.Time {
	var since time.Time
	for _, event := range events {
		if (event.Reason == "Pulling" || event.Reason == "Pulled") && eventTime(event).After(since) {
			since = eventTime(event)
		}
	}
	return since
}

// eventCount is how many times the kubelet recorded event: it folds a repeat of the same message
// into one event and counts it (Count, or the series' count).
func eventCount(event corev1.Event) int32 {
	if event.Series != nil && event.Series.Count > event.Count {
		return event.Series.Count
	}
	return event.Count
}

// csiFailure reports whether a share's failed mount is a CSI driver's gRPC error. An in-tree NFS
// `mount failed` is refused on its first event. For a server that never answers or refuses the
// connection, that event already stands for about two minutes of mount.nfs's own retries
// (ECONNREFUSED, ETIMEDOUT, EHOSTUNREACH, EAGAIN), and the kubelet's next try spends those two
// minutes again, so its report would arrive after a probe attempt's budget (each attempt is a new
// pod, whose events count afresh). Other mount.nfs errors (an export denied or absent) exit at once
// and are refused on that one try as well, since the export is the operator's to fix. A CSI final
// code may be the driver's moment (a Filestore maintenance blip), so a CSI failure is refused only
// once the kubelet's retry, about half a second later, has met it again; a CSI driver's own
// two-minute deadline (`DeadlineExceeded`) is not final and is left to the budget.
func csiFailure(message string) bool {
	return strings.Contains(message, `" : rpc error: code = `) && !strings.Contains(message, `" : mount failed: `)
}

// eventTime is when event last happened: the series' last observation, else its last timestamp,
// else when it was first recorded, else when the API server made it.
func eventTime(event corev1.Event) time.Time {
	switch {
	case event.Series != nil && !event.Series.LastObservedTime.IsZero():
		return event.Series.LastObservedTime.Time
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	}
	return event.CreationTimestamp.Time
}

// retryableCSICode are the gRPC codes Kubernetes' CSI client does not treat as final, an
// operation that may still be under way on the driver (pkg/volume/csi/csi_client.go,
// isFinalError; the kubelet retries every failed mount with backoff either way): an Aborted call (the
// Filestore driver's answer while another operation on the volume runs, pkg/csi_driver/node.go), a
// deadline the kubelet's 2-minute CSI timeout cut (a slow first Filestore mount), an Unavailable
// driver (its node plugin restarting), a Canceled call and an exhausted resource.
var retryableCSICode = regexp.MustCompile(`" : rpc error: code = (Canceled|DeadlineExceeded|Unavailable|ResourceExhausted|Aborted) `)

// shareMountFailed reports whether a FailedMount message says a share's mount ran and failed for
// good, in the kubelet's literal grammar after the volume's name: `MountVolume.SetUp failed for
// volume "<name>" : mount failed: …` (generateVolumeMsg's ` : ` before mount-utils' mount_linux.go
// doMount error, which every NFS mount, in-tree or a claim's PersistentVolume, goes through), or a
// CSI driver's final gRPC error, `… : rpc error: code = …` (MountDevice and SetUp alike) with a code
// retryableCSICode does not hold. Any other FailedMount — a retried CSI code, or the kubelet's
// `Unable to attach or mount volumes` summary while it still waits on a volume — is left to the
// budget.
func shareMountFailed(message string) bool {
	if strings.Contains(message, `" : mount failed: `) {
		return true
	}
	return strings.Contains(message, `" : rpc error: code = `) && !retryableCSICode.MatchString(message)
}

// blockVolumeNamed is the pod block's volume a kubelet event's volume name names, and whether the
// block names it.
func (r *Runtime) blockVolumeNamed(volume string) (corev1.Volume, bool) {
	i := slices.IndexFunc(r.pod.Volumes, func(v corev1.Volume) bool { return v.Name == volume })
	if i < 0 {
		return corev1.Volume{}, false
	}
	return r.pod.Volumes[i], true
}

// blockVolumeRefusal is volumeRefusal for a volume as a kubelet event names it. A name the pod
// block holds, or one the admitted pod's spec carries (a volume an admission webhook added, which
// volumeRefusal says the block does not name), is that volume. Any other name is the
// PersistentVolume one of the block's claims is bound to (volume.Spec.Name): the one claim's volume
// when the block names one claim, and, when it names several, a refusal naming the
// PersistentVolume and every claim it may belong to, since the runtime reads no claim (the
// daemon's Role grants none) to tell which.
func (r *Runtime) blockVolumeRefusal(pod *corev1.Pod, name, volume, message string) *bootprobe.Outcome {
	if _, ok := r.blockVolumeNamed(volume); ok || podHasVolume(pod, volume) {
		return r.volumeRefusal(name, volume, message)
	}
	claims := r.blockClaims()
	switch len(claims) {
	case 0:
		return r.volumeRefusal(name, volume, message)
	case 1:
		return r.volumeRefusal(name, claims[0], message)
	}
	refused := bootprobe.Outcome{Refusal: fmt.Errorf("the probe pod %s cannot mount PersistentVolume %q, bound to one of the claims runtime.kubernetes.pod.volumes gives volumes %s from: %s",
		name, volume, strings.Join(claims, ", "), message)}
	return &refused
}

// volumeRefusal is the refusal for an operator volume the kubelet cannot mount, for the reason
// message gives: naming what the pod block (runtime.kubernetes.pod.volumes) gives the volume from
// when the block holds it, so the operator reads which object to create, which key to add, or
// which share to fix, or saying the block does not name it, since a volume of that name reached
// the pod from elsewhere.
func (r *Runtime) volumeRefusal(name, volume, message string) *bootprobe.Outcome {
	from := "which runtime.kubernetes.pod.volumes does not name"
	if i := slices.IndexFunc(r.pod.Volumes, func(v corev1.Volume) bool { return v.Name == volume }); i >= 0 {
		from = "which runtime.kubernetes.pod.volumes gives it from " + volumeSources(r.pod.Volumes[i])
	}
	refused := bootprobe.Outcome{Refusal: fmt.Errorf("the probe pod %s cannot mount volume %q, %s: %s", name, volume, from, message)}
	return &refused
}

// volumeSources names what a pod-block volume is filled from: its ConfigMap or Secret, for a
// projected volume each ConfigMap and Secret source in the block's order joined by " and ", its
// ServiceAccount token sources not among them, since the kubelet mints those and no object can be
// missing for one, its PersistentVolumeClaim, or its NFS export. The loader
// (config/kubernetes.go, readPodVolumes) admits exactly one of those five kinds, so the last return
// is for a volume no file can produce.
func volumeSources(v corev1.Volume) string {
	switch {
	case v.ConfigMap != nil:
		return fmt.Sprintf("ConfigMap %q", v.ConfigMap.Name)
	case v.Secret != nil:
		return fmt.Sprintf("Secret %q", v.Secret.SecretName)
	case v.Projected != nil:
		var sources []string
		for _, source := range v.Projected.Sources {
			switch {
			case source.ConfigMap != nil:
				sources = append(sources, fmt.Sprintf("ConfigMap %q", source.ConfigMap.Name))
			case source.Secret != nil:
				sources = append(sources, fmt.Sprintf("Secret %q", source.Secret.Name))
			}
		}
		if len(sources) == 0 {
			return "a projected volume with no ConfigMap or Secret source"
		}
		return strings.Join(sources, " and ")
	case v.PersistentVolumeClaim != nil:
		return fmt.Sprintf("PersistentVolumeClaim %q", v.PersistentVolumeClaim.ClaimName)
	case v.NFS != nil:
		return fmt.Sprintf("NFS export %s:%s", v.NFS.Server, v.NFS.Path)
	}
	return "a source of another kind"
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
	if w := probeWaiting(pod); w != nil && w.Reason != "" {
		detail += fmt.Sprintf(" (container %s waiting: %s)", probeContainer, w.Reason)
	}
	detail += "; events: " + r.events(ctx, pod)
	if tail, err := r.probeLog(ctx, name); err == nil && tail != "" {
		detail += "; log tail: " + tail
	}
	return detail
}

// probeWaiting is the probe container's waiting state, or nil when the pod reports none: its
// Reason is what stuck and unfinished judge and quote, its Message the kubelet's own words, which
// only a CreateContainerConfigError (missingObject) and a failed pull (registryHasNoImage) are read
// for.
func probeWaiting(pod *corev1.Pod) *corev1.ContainerStateWaiting {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == probeContainer && status.State.Waiting != nil {
			return status.State.Waiting
		}
	}
	return nil
}

// probeLog is the probe container's last log lines, kept to probeLogBytes from the tail's end
// (lastBytes), so a refusal's quote reaches the OK line whatever a line's length.
func (r *Runtime) probeLog(ctx context.Context, name string) (string, error) {
	reading, cancel := call(ctx)
	defer cancel()
	lines := int64(probeLogLines)
	stream, err := r.kube.CoreV1().Pods(r.namespace).GetLogs(name, &corev1.PodLogOptions{Container: probeContainer, TailLines: &lines}).Stream(reading)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	text, err := lastBytes(io.LimitReader(stream, probeLogReadBytes), probeLogBytes)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

// lastBytes reads r to its end, keeping only the last keep bytes (one buffer of 2×keep, compacted
// in place), and answers that tail: with a leading … line (everything up to and including the
// first \n dropped, so what remains starts on its own line) when anything of r was cut, unchanged
// when r was keep bytes or shorter.
func lastBytes(r io.Reader, keep int) (string, error) {
	buf := make([]byte, 0, 2*keep)
	chunk := make([]byte, keep)
	dropped := false
	for {
		n, err := io.ReadFull(r, chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > keep {
			dropped = true
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if !dropped {
		return string(buf), nil
	}
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	return "…\n" + string(buf), nil
}

// undefinedFlag is Go's flag package refusing a flag the probed CLI does not define.
var undefinedFlag = regexp.MustCompile(`flag provided but not defined: (-\S+)`)

// judge is the verdict on a finished probe pod and its log, logErr when the log could not be read
// (judgeProbeLog), in this order, carrying on a pass what the OK line reported of the image, in
// Outcome.Report. A pod the kubelet failed for
// reasons of its own (kubeletFailure) is transient, whatever of its log could be read, and an
// unread log is judged by the read's error. A Failed pod past those is one whose probe container
// exited on its own: the image's refusal. The OK line must confirm this daemon's contract: an
// image whose CLI predates the contract check prints none, having checked no contract, and is
// refused, not waved through; one that confirmed another contract is refused naming both. And it
// must say the prompt-named agents' models resolved: any other mark, or none, does not prove the
// workers run their agents on their models. It must carry the extensions mark
// (bootprobe.ExtensionsDiscovered): a CLI that predates the discovery-on pod lane probed the plugin
// roots with Oh My Pi's extension discovery disabled, and the image it certified links the plugins
// into its profile, which a pod of this daemon — discovery on, the two roots explicit — would load
// twice. It must carry the capabilities mark too
// (bootprobe.CapabilitiesChecked): a CLI that predates the capability check prints none, having
// checked no capability, and a current one that found a capability missing exits 1 before the OK
// line, so the mark's absence on a Succeeded pod is the image's CLI, refused as a rebuild. When the
// daemon has a pane NATS nkey seed (Options.NATSUser),
// the probe must also name the same user as the seed its pointer read (bootprobe.NATSUser), a
// refusal otherwise. Naming none is the image's: a current CLI whose pointer holds a blank or
// invalid seed exits 1 (natsauth.Seed), and a key the kubelet cannot mount never starts the
// container, so only a CLI that predates the user line succeeds without one. Naming another is the
// providers Secret holding another seed. An image whose CLI predates a flag the probe command
// passes stops at the flags, and its Failed pod is refused naming the flag its CLI lacks. The
// passing outcome's Report carries the line's model-fallback mark, which the daemon keeps.
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
		if refused := r.unreadableShares(name, pod); refused != nil {
			return *refused
		}
		return imageRefusal(digest, "pod %s Failed%s — log tail: %s", name, ended, logTail)
	}
	if !strings.Contains(logTail, bootprobe.OKPrefix) {
		return imageRefusal(digest, "pod %s Succeeded without printing %s — log tail: %s", name, bootprobe.OKPrefix, logTail)
	}
	confirmed, ok := bootprobe.ConfirmedContract(logTail)
	if !ok {
		return imageRefusal(digest, "pod %s Succeeded without confirming daemon API contract %d (its legion CLI predates the check) — log tail: %s", name, contract, logTail)
	}
	if confirmed != contract {
		return imageRefusal(digest, "pod %s Succeeded but confirmed daemon API contract %d, this daemon requires %d — log tail: %s", name, confirmed, contract, logTail)
	}
	if mark := bootprobe.AgentModels(logTail); mark != bootprobe.AgentModelsResolved {
		if mark == "" {
			mark = "none"
		}
		return imageRefusal(digest, "pod %s Succeeded without resolving the prompt-named agents' models (its OK line's agent-models mark: %s, where the daemon's probe requires %s) — log tail: %s",
			name, mark, bootprobe.AgentModelsResolved, logTail)
	}
	if !bootprobe.ExtensionsDiscovered(logTail) {
		return imageRefusal(digest, "pod %s Succeeded without the extensions=discovered mark: its legion CLI predates the discovery-on pod lane and its profile links the plugins a pod names explicitly, which a pod of this daemon would load twice — build the image from this daemon's commit — log tail: %s",
			name, logTail)
	}
	if !bootprobe.CapabilitiesChecked(logTail) {
		return imageRefusal(digest, "pod %s Succeeded without checking the capability list (its legion CLI predates the check): build the image from this daemon's commit — log tail: %s", name, logTail)
	}
	if r.natsUser != "" {
		switch got := bootprobe.NATSUser(logTail); got {
		case r.natsUser:
		case "":
			return imageRefusal(digest, "pod %s named no nkey user, where the pane seed the daemon hands every pod is user %s: its legion CLI predates the probe's nats-nkey-user line: build the image from this daemon's commit — log tail: %s",
				name, r.natsUser, logTail)
		default:
			return bootprobe.Outcome{Refusal: fmt.Errorf("the probe pod %s read nkey user %s through its NATS_NKEY_SEED_FILE, the providers Secret %s's NATS_NKEY_SEED, where the pane seed the daemon hands every pod is user %s: put that seed in that key — log tail: %s",
				name, got, ProvidersSecretName(r.project), r.natsUser, logTail)}
		}
	}
	report := bootprobe.ImageReport{ModelFallback: bootprobe.ModelFallback(logTail)}
	r.log.Info("sandbox runtime: the worker image passed its probe", "image", r.image, "sandbox", name, "model-fallback", report.ModelFallback, "log", logTail)
	return bootprobe.Outcome{Passed: true, Report: report}
}

// unreadableMounts is gVisor's wording when the container's mounts cannot be handed to the
// sandbox: runsc's start reads the mount list its gofer wrote and fails `reading mounts file: …`
// (runsc/container/container.go, Container.Start) when the gofer could not open one, which is
// what a share the kubelet mounted on the node but whose root the node cannot read (an NFS
// server answering the mount and failing every read after it, `Remote I/O error`) does to a
// gVisor container. The kubelet reports it as the container's StartError, exit code 128.
const unreadableMounts = "reading mounts file: "

// unreadableShares is the refusal for a probe pod Failed because gVisor could not start its
// container over its mounts (unreadableMounts), when the pod block gives the pod a share: the
// image is not at fault, and the message names no volume, so the refusal names every share the
// pod mounts. Nil otherwise, so any other failed start stays the image's.
func (r *Runtime) unreadableShares(name string, pod *corev1.Pod) *bootprobe.Outcome {
	var shares []string
	for _, v := range r.pod.Volumes {
		if v.NFS != nil || v.PersistentVolumeClaim != nil {
			shares = append(shares, fmt.Sprintf("%q (%s)", v.Name, volumeSources(v)))
		}
	}
	if len(shares) == 0 {
		return nil
	}
	for _, status := range pod.Status.ContainerStatuses {
		t := status.State.Terminated
		if status.Name != probeContainer || t == nil || t.Reason != "StartError" || !strings.Contains(t.Message, unreadableMounts) {
			continue
		}
		refused := bootprobe.Outcome{Refusal: fmt.Errorf("the probe pod %s could not start its container %s over its mounts, which gVisor could not read: check the shares runtime.kubernetes.pod.volumes gives it, %s, are readable from the node: %s",
			name, probeContainer, strings.Join(shares, ", "), t.Message)}
		return &refused
	}
	return nil
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
// against p's contract as a worker runs: on the pod's baseline (--pod-safety), loading the plugins
// from the roots a pod loads them from (--plugin-root, --envoy-plugin-root), with the providers
// Secret's keys exported as the worker's shim exports them (--provider-env-dir) when any are
// configured, each providers
// secret's `<NAME>_FILE` pointing at its file there as a worker's does (so neither exports it),
// and resolving the daemon's own role prompts' references (--role-references), which ProbeImage
// requires. Its command and env are escaped against the kubelet's expansion as every worker
// container's are (kubeletLiteral). It carries Karpenter's `do-not-disrupt` alone, not the Cluster
// Autoscaler's `safe-to-evict` (podTemplate sets both): GKE Autopilot places a pod carrying that
// one on a node of its own whenever possible, so each boot's probe would ask for a node, and a
// probe minutes long and already retried on failure (kubeletFailure, ImageProbe.Retry) gains nothing
// from a seven-day eviction window.
func (r *Runtime) probeManifest(name string, p ImageProbe, shutdown time.Time) probeSandbox {
	labels := map[string]string{labelProject: r.project, labelProbe: "image"}
	providers, providersMounts := r.providers()
	// The probe reads the providers directory as a worker's shim does, and every pod mounts it:
	// the session database's URL key is required, so the Secret is never absent (providers).
	command := []string{r.tools.Legion, "probe-image", "--daemon-api-version", strconv.Itoa(p.Contract), "--plugin-root", legionPlugin, "--envoy-plugin-root", envoyPlugin, "--pod-safety",
		"--provider-env-dir", ProvidersDir, "--role-references", p.RoleReferences.Encode()}
	container := corev1.Container{
		Name:            probeContainer,
		Image:           r.image,
		Command:         command,
		Env:             slices.Concat(r.operatorEnv(), r.providersPointers(), r.sessionDSNPointer()),
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
				Metadata: podMetadata{Labels: labels, Annotations: map[string]string{doNotDisruptAnnotation: "true"}},
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
