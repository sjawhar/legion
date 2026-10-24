//go:build e2e

// The Stage 4a harness's lifecycle checks: every claim's launches, suspends, resumes, deaths,
// re-adoption, the orphan sweep, and the release that leaves the namespace as it was. The rig is
// live_test.go.

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// root-ready: the root claim spawns, provisions its workspace, and registers at generation 1. The
// root is the run's first issue pod, so its launch is also where the run's repository gets its
// cache: there is none before (its Sandbox recorded for the teardown first), the launch makes it,
// and the root's fetch fills its feed from that cache alone, asking GitHub only for the delta.
func (r *liveRig) checkRootReady() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	cache := CacheSandboxName(r.env.project, r.env.repo)
	if _, found := r.rt.RepositoryCache(r.env.repo); found {
		return fmt.Errorf("the repository cache %s exists before the run's first issue pod launched", cache)
	}
	if err := r.recordSandbox(cache); err != nil {
		return err
	}
	r.mapRepository(r.env.repo)
	root := r.claim("root")
	since := time.Now()
	loc, err := r.spawn(root, true)
	if err != nil {
		return err
	}
	note("runtime", "Spawn(%s) returned incarnation %s", root.token, loc.Incarnation)
	reg, err := r.awaitRunning(root, since)
	if err != nil {
		return err
	}
	name := SandboxName(root.token)
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	if string(pod.UID) != loc.Sandbox.PodUID {
		return fmt.Errorf("pod %s is uid %s, the launch returned pod uid %s in %s", name, pod.UID, loc.Sandbox.PodUID, loc.Incarnation)
	}
	note("runtime", "Sandbox %s Ready=True; pod uid %s and role container %s form returned incarnation %s", name, pod.UID, loc.Sandbox.Container, loc.Incarnation)
	log, err := r.initLog(name)
	if err != nil {
		return err
	}
	dir, _ := workspace.Location(TreeRoot, r.env.repo, root.issue)
	want := "workspace-init: " + dir.Dir + " on " + dir.Bookmark
	if !strings.Contains(log, want) {
		return fmt.Errorf("the init log (pods/log) lacks %q: %s", want, strings.TrimSpace(log))
	}
	note("runtime", "init log (pods/log): %q", want)
	if reg.gen != 1 || reg.hash != tokenHash(root.bootToken) {
		return fmt.Errorf("registered at generation %d with token hash %s, want generation 1 with %s", reg.gen, short(reg.hash), short(tokenHash(root.bootToken)))
	}
	note("harness", "hello registered %s at generation 1, token sha256 %s… (the generation-1 token's)", root.token, reg.hash[:12])
	if _, found := r.rt.RepositoryCache(r.env.repo); !found {
		return fmt.Errorf("the root's launch made no repository cache of %s", r.env.repo)
	}
	if err := r.assertCacheFed(root); err != nil {
		return err
	}
	note("runtime", "the root's launch made the repository cache %s, and the root's feed came from it alone", cache)
	return nil
}

// gvisor: the root pod runs under gVisor, on the gvisor RuntimeClass.
func (r *liveRig) checkGVisor() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	kernel, err := r.exec(root, "uname", "-r")
	if err != nil {
		return err
	}
	name := SandboxName(root.token)
	class, err := r.kubectl("get", "pod", name, "-o", "jsonpath={.spec.runtimeClassName}")
	if err != nil {
		return err
	}
	nodeName, err := r.kubectl("get", "pod", name, "-o", "jsonpath={.spec.nodeName}")
	if err != nil {
		return err
	}
	host, err := r.kubectl("get", "node", nodeName, "-o", "jsonpath={.status.nodeInfo.kernelVersion}")
	if err != nil {
		return err
	}
	note("operator", "exec %s -- uname -r: %s; node %s's kernel: %s", name, kernel, nodeName, host)
	note("operator", "pod %s .spec.runtimeClassName: %s", name, class)
	switch {
	case class != gvisor:
		return fmt.Errorf("runtimeClassName is %q, want %s", class, gvisor)
	case !strings.HasSuffix(kernel, "-gvisor"):
		return fmt.Errorf("uname -r in the pod is %q, not gVisor's emulated kernel (…-gvisor)", kernel)
	case kernel == host:
		return fmt.Errorf("the pod reports the node's own kernel %s", host)
	}
	return nil
}

// resources: every container of the root's pod, its init containers included, reserves cpu and
// memory with the request equal to the limit and equal to the issue pod's reservation the rig
// configured (liveResources: liveIssueOverride over config.DefaultResources()), so the pod is
// Guaranteed; carries the ephemeral-storage limit and request configured for it, equal, so GKE's
// clamp of a limit to its request changes nothing (gkeEphemeralStorageClamped). And carries no
// affinity but GKE Autopilot's own extended-duration one (isGKEExtendedDurationAffinity's doc
// comment) under LEGION_E2E_PLATFORM=gke-autopilot. Inside the pod, gVisor sizes the sandbox from
// the pod's cgroup, which the kubelet sets to the one container's limits once the init containers
// are done: nproc is max(2, ceil(cpu limit)) and /proc/meminfo's MemTotal is within 3% of the
// memory limit — except on GKE, where nproc reflects gVisor's own host-vCPU presentation rather
// than the pod's own cgroup (gvisorCPUFloor's doc comment), so the cgroup's own
// cpu.cfs_quota_us/cpu.cfs_period_us are read directly there instead, confirmed live to track the
// configured limit exactly.
func (r *liveRig) checkResources() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	name := SandboxName(root.token)
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	want := r.liveResources().Issue
	byContainer := map[string]corev1.ResourceRequirements{runtime.IssuePodContainer: want}
	if err := guaranteedPod(pod, byContainer, &want, resource.Quantity{}, r.gkeEphemeralStorageClamped(), r.gke()); err != nil {
		return fmt.Errorf("pod %s: %w", name, err)
	}
	var cpuSum, memorySum resource.Quantity
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		note("runtime", "container %s: cpu %s, memory %s, request = limit; ephemeral-storage %s under a limit of %s, the issue pod's reservation", c.Name, c.Resources.Limits.Cpu().String(), c.Resources.Limits.Memory().String(), c.Resources.Requests.StorageEphemeral().String(), c.Resources.Limits.StorageEphemeral().String())
	}
	for _, c := range pod.Spec.Containers {
		cpuSum.Add(*c.Resources.Limits.Cpu())
		memorySum.Add(*c.Resources.Limits.Memory())
	}
	note("operator", "pod %s: qosClass %s, no affinity but GKE Autopilot's own extended-duration one under LEGION_E2E_PLATFORM=gke-autopilot; one container carrying runtime.kubernetes.resources.issue", name, pod.Status.QOSClass)

	wantCPUs := max(2, int(math.Ceil(cpuSum.AsApproximateFloat64())))
	wantMemory := memorySum.AsApproximateFloat64()

	var memTotalKB int64
	checkMemory := func() error {
		meminfo, err := r.exec(root, "sh", "-c", "grep '^MemTotal:' /proc/meminfo")
		if err != nil {
			return err
		}
		fields := strings.Fields(meminfo)
		if len(fields) != 3 || fields[2] != "kB" {
			return fmt.Errorf("/proc/meminfo's MemTotal line is %q, want `MemTotal: <n> kB`", meminfo)
		}
		memTotalKB, err = strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return fmt.Errorf("/proc/meminfo's MemTotal %q is not a number", fields[1])
		}
		if memTotal := float64(memTotalKB) * 1024; math.Abs(memTotal-wantMemory) > 0.03*wantMemory {
			return fmt.Errorf("MemTotal in the pod is %d kB (%.2f GiB), not within 3%% of the containers' summed memory limits %s, the pod cgroup's limit as gVisor sizes the sandbox", memTotalKB, memTotal/(1<<30), memorySum.String())
		}
		return nil
	}

	if r.gke() {
		// gVisor on GKE: nproc inside the sandbox is the guest's own host-vCPU presentation, not
		// a function of the pod's cgroup quota — confirmed live, 2026-10-17, against the
		// production GKE cluster: two debug pods summing to 2.5 and to exactly 3.0 cores (one
		// container each at 1 and 2 cores) both reported nproc=2 on an 8-vCPU node with 7.91
		// allocatable, far more than either sum, ruling out node capacity as the cause; this
		// matches a documented gVisor/GKE gap (kubernetes/kubernetes#91665, "Actual CPU capacity
		// is lower than assigned CPU capacity when gVisor sandbox is enabled"). The pod's cgroup
		// cpu quota, read from `/sys/fs/cgroup/cpu/cpu.cfs_quota_us` and `cpu.cfs_period_us` —
		// identically presented by every container in the pod, since one gVisor sandbox covers
		// the whole pod — tracked the configured sum exactly in both debug pods (300000/100000 =
		// 3.0 cores; 250000/100000 = 2.5 cores) and is the direct observable of what the runtime
		// actually reserved, which is the check's real intent. AWS keeps nproc below; it is
		// proven reliable there.
		quotaOut, err := r.exec(root, "cat", "/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
		if err != nil {
			return err
		}
		periodOut, err := r.exec(root, "cat", "/sys/fs/cgroup/cpu/cpu.cfs_period_us")
		if err != nil {
			return err
		}
		quota, err := strconv.ParseFloat(strings.TrimSpace(quotaOut), 64)
		if err != nil {
			return fmt.Errorf("cpu.cfs_quota_us printed %q, not a number", quotaOut)
		}
		period, err := strconv.ParseFloat(strings.TrimSpace(periodOut), 64)
		if err != nil || period <= 0 {
			return fmt.Errorf("cpu.cfs_period_us printed %q, not a positive number", periodOut)
		}
		gotCPUs, wantCPUFloat := quota/period, cpuSum.AsApproximateFloat64()
		if math.Abs(gotCPUs-wantCPUFloat) > 0.01 {
			return fmt.Errorf("the pod's cgroup cpu quota is %.3f cores (cfs_quota_us=%s, cfs_period_us=%s), not the containers' summed cpu limits %s", gotCPUs, strings.TrimSpace(quotaOut), strings.TrimSpace(periodOut), cpuSum.String())
		}
		if err := checkMemory(); err != nil {
			return err
		}
		nprocOut, nprocErr := r.exec(root, "nproc")
		if nprocErr != nil {
			nprocOut = fmt.Sprintf("(exec failed: %v)", nprocErr)
		}
		note("operator", "pod cgroup cpu quota %.3f cores (cfs_quota_us=%s/cfs_period_us=%s) matches the containers' summed cpu limits %s; nproc reports %s, gVisor's own host-vCPU presentation, not asserted (kubernetes/kubernetes#91665); MemTotal %d kB, within 3%% of the summed memory limits %s",
			gotCPUs, strings.TrimSpace(quotaOut), strings.TrimSpace(periodOut), cpuSum.String(), strings.TrimSpace(nprocOut), memTotalKB, memorySum.String())
		return nil
	}

	nprocOut, err := r.exec(root, "nproc")
	if err != nil {
		return err
	}
	cpus, err := strconv.Atoi(strings.TrimSpace(nprocOut))
	if err != nil {
		return fmt.Errorf("nproc in the pod printed %q, not a number", nprocOut)
	}
	if cpus != wantCPUs {
		return fmt.Errorf("nproc in the pod is %d, want max(2, ceil(%s cpu)) = %d, the pod cgroup's quota as gVisor sizes the sandbox", cpus, cpuSum.String(), wantCPUs)
	}
	if err := checkMemory(); err != nil {
		return err
	}
	note("operator", "exec %s -- nproc: %d = max(2, ceil(%s)); MemTotal %d kB, within 3%% of the summed memory limits %s", name, cpus, cpuSum.String(), memTotalKB, memorySum.String())
	return nil
}

// gvisorCPUFloor is the cpu reservation a lone, small Sandbox container is admitted at on GKE,
// never under: confirmed live, 2026-10-17, against the production GKE cluster, only for the
// single-container probe Sandbox (its one container's 250m is the pod's whole cpu sum) — every
// multi-container issue pod in this same cluster, already summing well past it, was admitted at
// exactly its configured per-container cpu unchanged (checked directly against a real production
// issue pod of the layout before one container, whose six containers summed past it), and an issue
// pod's one container reserves more than the floor. So only checkImageProbe passes this;
// checkResources passes a zero Quantity, asking for no floor. AWS carries no such floor either
// way. guaranteedPod folds it into its comparison, not the daemon's own reservation
// (config.Reservation, config.DefaultResources(), probeReservation): those stay the production values
// on every runtime; only this live GKE check expects what the cluster will actually admit.
func (r *liveRig) gvisorCPUFloor() resource.Quantity {
	if r.env.runtimeIdentityKind == "kubernetes-service-account" {
		return resource.MustParse("500m")
	}
	return resource.Quantity{}
}

// gke reports whether this run's cluster is GKE Autopilot (LEGION_E2E_PLATFORM=gke-autopilot, an
// explicit input independent of runtimeIdentityKind and podReachability: a cluster authenticating
// with a ServiceAccount, or unreachable from pods directly, is not necessarily GKE — a
// self-managed cluster or an EKS cluster with IRSA could do either), the platform
// gkeEphemeralStorageClamped's and isGKEExtendedDurationAffinity's doc comments give the live
// evidence for. False means eks, which neither clamps ephemeral storage nor adds a NodeAffinity to
// any pod here.
func (r *liveRig) gke() bool {
	return r.env.platform == "gke-autopilot"
}

// gkeEphemeralStorageClamped reports whether this run's cluster admits a container's ephemeral-
// storage limit equal to its request, discarding whatever limit was configured: confirmed live,
// 2026-10-17, on both a bare debug pod and every role container of a real, already-running
// production issue pod in this namespace (every one of its six containers carries request =
// limit = 1Gi, its configured EphemeralStorageRequest, though config.DefaultResources() and
// liveOverrides ask several of them for limits of 10Gi, 20Gi or 30Gi) — a gVisor-sandboxed pod's
// ephemeral storage is never burstable on this cluster, cpu and memory floors notwithstanding.
// AWS admits the configured limit unchanged. checkImageProbe and checkResources both pass this to
// guaranteedPod, and for the same reason: checkImageProbe's reservation (probeReservation,
// manifest_test.go) is the daemon's own fixed, hardcoded value, and checkResources' is
// config.DefaultResources()'s own per-role default, for every role liveOverrides does not give a
// matching EphemeralStorageRequest of its own — in both cases a configured limit this rig cannot
// (and, for the daemon's hardcoded probe reservation, should not) simply raise the request to
// match, so the clamp is the real, admitted bound to assert, not the originally-configured one.
// liveOverrides' tester entry is the one role this rig does ask for a matching request on, which
// defeats the clamp for it alone (its own doc comment has why that cannot extend to every role at
// once): guaranteedPod's clamp only replaces a container's wantLimit with its own wantRequest, so
// a container whose want already carries request = limit sees no change from either value.
func (r *liveRig) gkeEphemeralStorageClamped() bool {
	return r.gke()
}

// isGKEExtendedDurationAffinity reports whether a carries exactly the NodeAffinity GKE Autopilot
// adds on its own to a pod carrying `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`
// (manifest.go's safeToEvictAnnotation, every issue pod's, never the probe's — probeManifest's
// own doc says why) to protect it from disruption for up to seven days (confirmed live,
// 2026-10-17: a real root issue pod on the production GKE cluster carried exactly this one
// required NodeAffinity term, keyed `cloud.google.com/extended-duration-pods`, nothing else —
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/extended-duration-pods). It is
// GKE's own addition, not a pod affinity or anti-affinity the pod spec asks for, and it still
// admits the pod to any node whose pool carries that label, never a named one — so it does not
// trip the reservation rule's intent (no pod asks for or keeps off a particular node) even though
// it is literally a NodeAffinity. AWS adds none; a pod this doesn't match is still refused.
//
// The `In` operator's own values are not a fixed, tolerable literal: confirmed live, 2026-10-17,
// on this round's own six-role root pod, GKE set them to `["2500m", "X"]` — the first exactly the
// pod's own summed regular-container cpu limit (250+250+500+500+750+250m), the second unexplained
// — where an earlier run's single-container probe pod case (never exercised here; the probe always
// passes extendedDurationAffinity=false) might plausibly carry something else again. GKE computes
// these per pod, not from a fixed set this harness could enumerate, so any non-empty `In` is
// tolerated; only the key and the operator (`In` with values, or `Exists` with none) decide
// whether this is the one shape GKE itself would still admit on its own — a `NotIn` or
// `DoesNotExist` on the same key is a refusal GKE would admit too, but meaning the opposite, and
// is never tolerated here regardless of its own values.
func isGKEExtendedDurationAffinity(a *corev1.Affinity) bool {
	if a == nil || a.PodAffinity != nil || a.PodAntiAffinity != nil || a.NodeAffinity == nil {
		return false
	}
	na := a.NodeAffinity
	if len(na.PreferredDuringSchedulingIgnoredDuringExecution) != 0 || na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	terms := na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 0 || len(terms[0].MatchExpressions) != 1 {
		return false
	}
	expr := terms[0].MatchExpressions[0]
	if expr.Key != "cloud.google.com/extended-duration-pods" {
		return false
	}
	switch expr.Operator {
	case corev1.NodeSelectorOpIn:
		return len(expr.Values) > 0
	case corev1.NodeSelectorOpExists:
		return len(expr.Values) == 0
	default:
		return false
	}
}

// guaranteedPod is the reservation rule the rig's pods are held to, since the rig configures no
// cpu or memory request below a limit (liveOverrides): it carries no affinity but GKE's own
// extended-duration one when extendedDurationAffinity allows it (isGKEExtendedDurationAffinity's
// doc has the evidence; the probe never carries it and always passes false), its qosClass is
// Guaranteed, and every container — the init containers included — reserves cpu and memory with
// the request equal to the limit and at least the reservation configured for it (cpu floored at
// cpuFloor, zero off GKE and off every multi-container pod on it), and carries the
// ephemeral-storage request and limit configured for it, the request under the limit, except that
// ephemeralStorageClamped asks for the limit to equal the request instead of the configured limit
// (GKE's own admission, gkeEphemeralStorageClamped's doc has the evidence): want's entry for each
// regular container by name, and init for every init container (nil when the pod has none to
// hold). A container want does not name is a refusal: nothing a Legion pod runs is unreserved.
func guaranteedPod(pod *corev1.Pod, want map[string]corev1.ResourceRequirements, init *corev1.ResourceRequirements, cpuFloor resource.Quantity, ephemeralStorageClamped, extendedDurationAffinity bool) error {
	if pod.Spec.Affinity != nil && !(extendedDurationAffinity && isGKEExtendedDurationAffinity(pod.Spec.Affinity)) {
		return fmt.Errorf("carries an affinity, which no Legion pod asks for: %+v", pod.Spec.Affinity)
	}
	if pod.Status.QOSClass != corev1.PodQOSGuaranteed {
		return fmt.Errorf("is %q, want %s", pod.Status.QOSClass, corev1.PodQOSGuaranteed)
	}
	check := func(c corev1.Container, expected corev1.ResourceRequirements, source string) error {
		for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			request, limit := c.Resources.Requests[resourceName], c.Resources.Limits[resourceName]
			reserved := expected.Requests[resourceName]
			if resourceName == corev1.ResourceCPU && reserved.Cmp(cpuFloor) < 0 {
				reserved = cpuFloor
			}
			if request.IsZero() || limit.IsZero() {
				return fmt.Errorf("container %s reserves no %s: requests %v, limits %v", c.Name, resourceName, c.Resources.Requests, c.Resources.Limits)
			}
			if request.Cmp(limit) != 0 {
				return fmt.Errorf("container %s requests %s %s but is limited to %s; this run configures request = limit", c.Name, resourceName, request.String(), limit.String())
			}
			if request.Cmp(reserved) != 0 {
				return fmt.Errorf("container %s reserves %s %s, not the %s configured for %s", c.Name, resourceName, request.String(), reserved.String(), source)
			}
		}
		request, limit := c.Resources.Requests[corev1.ResourceEphemeralStorage], c.Resources.Limits[corev1.ResourceEphemeralStorage]
		wantRequest, wantLimit := expected.Requests[corev1.ResourceEphemeralStorage], expected.Limits[corev1.ResourceEphemeralStorage]
		if ephemeralStorageClamped {
			wantLimit = wantRequest
		}
		switch {
		case request.IsZero() || limit.IsZero():
			return fmt.Errorf("container %s bounds no ephemeral-storage: requests %v, limits %v", c.Name, c.Resources.Requests, c.Resources.Limits)
		case request.Cmp(limit) > 0:
			return fmt.Errorf("container %s requests ephemeral-storage %s past its limit %s", c.Name, request.String(), limit.String())
		case request.Cmp(wantRequest) != 0 || limit.Cmp(wantLimit) != 0:
			return fmt.Errorf("container %s bounds ephemeral-storage at %s under a limit of %s, not the %s under %s configured for %s", c.Name, request.String(), limit.String(), wantRequest.String(), wantLimit.String(), source)
		}
		return nil
	}
	for _, c := range pod.Spec.InitContainers {
		if init == nil {
			return fmt.Errorf("init container %s: the pod is expected to run none", c.Name)
		}
		if err := check(c, *init, "the launching role"); err != nil {
			return fmt.Errorf("init container: %w", err)
		}
	}
	for _, c := range pod.Spec.Containers {
		expected, ok := want[c.Name]
		if !ok {
			return fmt.Errorf("container %s names no role a reservation is configured for", c.Name)
		}
		if err := check(c, expected, "the "+c.Name); err != nil {
			return err
		}
	}
	return nil
}

// adopt-working-copy: the root's shim sets its working copy's author, in the pod's workspace.
func (r *liveRig) checkAdoptWorkingCopy() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.rt.AdoptWorkingCopy(r.ctx, *root.loc, r.identity); err != nil {
		return err
	}
	note("runtime", "AdoptWorkingCopy(%s, %s <%s>): nil", root.token, r.identity.Name, r.identity.Email)
	author, err := r.exec(root, "sh", "-c", `cd "$LEGION_WORKSPACE" && jj log -r @ --no-graph -T author`)
	if err != nil {
		return err
	}
	note("operator", "exec: cd \"$LEGION_WORKSPACE\" && jj log -r @ --no-graph -T author: %s", author)
	if !strings.Contains(author, r.identity.Name) || !strings.Contains(author, r.identity.Email) {
		return fmt.Errorf("the working copy's author is %q, not %s <%s>", author, r.identity.Name, r.identity.Email)
	}
	return nil
}

// worker-shares-issue-pod: another role of the same issue starts in the root's existing issue pod.
// Its process locator has the same pod UID and the same one container, and no init container runs
// again.
func (r *liveRig) checkWorkerSharesIssuePod() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	before, err := r.getPod(SandboxName(root.token))
	if err != nil {
		return err
	}
	init := append([]corev1.ContainerStatus(nil), before.Status.InitContainerStatuses...)
	since := time.Now()
	if _, err := r.spawn(worker, true); err != nil {
		return err
	}
	if _, err := r.awaitRunning(worker, since); err != nil {
		return err
	}
	pod, err := r.getPod(SandboxName(worker.token))
	if err != nil {
		return err
	}
	if pod.Name != before.Name || string(pod.UID) != string(before.UID) || worker.loc.Sandbox.PodUID != root.loc.Sandbox.PodUID {
		return fmt.Errorf("worker locator %+v and root locator %+v do not share one issue pod %s/%s", worker.loc.Sandbox, root.loc.Sandbox, before.Name, before.UID)
	}
	if worker.loc.Sandbox.Container != runtime.IssuePodContainer || root.loc.Sandbox.Container != runtime.IssuePodContainer {
		return fmt.Errorf("worker/root containers are %q/%q, want %q for both", worker.loc.Sandbox.Container, root.loc.Sandbox.Container, runtime.IssuePodContainer)
	}
	if !slices.EqualFunc(init, pod.Status.InitContainerStatuses, func(a, b corev1.ContainerStatus) bool {
		return a.Name == b.Name && a.ContainerID == b.ContainerID && a.State.Terminated != nil && b.State.Terminated != nil &&
			a.State.Terminated.StartedAt.Equal(&b.State.Terminated.StartedAt) && a.State.Terminated.FinishedAt.Equal(&b.State.Terminated.FinishedAt)
	}) {
		return fmt.Errorf("starting %s reran issue-pod init containers: before %+v, after %+v", worker.role, init, pod.Status.InitContainerStatuses)
	}
	note("runtime", "worker role %s started in root issue pod %s (uid %s), container %s; both init-container identities stayed unchanged", worker.token, pod.Name, pod.UID, worker.loc.Sandbox.Container)
	return nil
}

// suspend: stopping the worker's process leaves its issue Sandbox, pod, root role and the issue's
// PVC intact. The stopped locator probes Gone; the root remains Alive in its separate role container.
func (r *liveRig) checkSuspend() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.ensureRunning(worker); err != nil {
		return err
	}
	loc := *worker.loc
	name := SandboxName(worker.token)
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	if err := r.rt.Suspend(r.ctx, loc); err != nil {
		return err
	}
	if recorded, ok := r.rt.recorded(worker.token); ok {
		return fmt.Errorf("Suspend returned with the claim still in the watch, at %s", recorded.Incarnation)
	}
	s, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if s.mode() != modeRunning {
		return fmt.Errorf("Suspend(%s) changed shared Sandbox %s to %s", worker.token, name, s.mode())
	}
	current, err := r.getPod(name)
	if err != nil || string(current.UID) != string(pod.UID) {
		return fmt.Errorf("Suspend(%s) changed its issue pod from %s: %v", worker.token, pod.UID, err)
	}
	obs, err := r.rt.Probe(r.ctx, loc)
	if err != nil {
		return err
	}
	if obs.Kind != runtime.Gone || !sameLocator(obs.Locator, loc) {
		return fmt.Errorf("Probe(the stopped locator) answered %s for %s: %s", obs.Kind, obs.Locator.Incarnation, obs.Detail)
	}
	if rootObs, err := r.rt.Probe(r.ctx, *root.loc); err != nil || rootObs.Kind != runtime.Alive {
		return fmt.Errorf("the root process after Suspend(%s) is %s: %v", worker.token, rootObs.Kind, err)
	}
	worker.loc, worker.state = nil, stateSuspended
	// The worker and the root are claims of one issue, so either token names the issue's PVC.
	pvc := IssueClaimName(worker.token)
	phase, err := r.kubectl("get", "pvc", pvc, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return err
	}
	if phase != "Bound" {
		return fmt.Errorf("the issue's PVC %s is %q", pvc, phase)
	}
	note("runtime", "Suspend(%s) stopped only its role process; issue Sandbox %s and root process stayed running in pod uid %s", worker.token, name, pod.UID)
	note("operator", "PVC %s: Bound; Probe(stopped %s): Gone", pvc, short(loc.Incarnation))
	return nil
}

// role-container-isolation: a second role of the issue starts in the existing pod's own container,
// keeps the root process alive, and can stop without changing the root container or the pod.
func (r *liveRig) checkRoleContainerIsolation() error {
	root, second := r.claim("root"), r.claim("second")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	since := time.Now()
	if _, err := r.spawn(second, true); err != nil {
		return err
	}
	if _, err := r.awaitRunning(second, since); err != nil {
		return err
	}
	if second.loc.Sandbox.PodUID != root.loc.Sandbox.PodUID || second.loc.Sandbox.Container != root.loc.Sandbox.Container {
		return fmt.Errorf("second role locator %+v and root locator %+v are not processes of one issue pod's container", second.loc.Sandbox, root.loc.Sandbox)
	}
	if rootObs, err := r.rt.Probe(r.ctx, *root.loc); err != nil || rootObs.Kind != runtime.Alive {
		return fmt.Errorf("root process after starting %s is %s: %v", second.token, rootObs.Kind, err)
	}
	if err := r.suspend(second); err != nil {
		return err
	}
	if rootObs, err := r.rt.Probe(r.ctx, *root.loc); err != nil || rootObs.Kind != runtime.Alive {
		return fmt.Errorf("root process after suspending %s is %s: %v", second.token, rootObs.Kind, err)
	}
	note("runtime", "role %s ran then stopped in container %s; root %s stayed Alive in container %s of pod uid %s", second.token, second.last.Sandbox.Container, root.token, root.loc.Sandbox.Container, root.loc.Sandbox.PodUID)
	return nil
}

// resume: the first worker comes back as a new process generation of the same agent in the same
// issue pod beside the root.
func (r *liveRig) checkResume() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureSuspended(worker); err != nil {
		return err
	}
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	old := *worker.last
	since := time.Now()
	loc, err := r.resume(worker, worker.marker)
	if err != nil {
		return err
	}
	reg, err := r.awaitRunning(worker, since)
	if err != nil {
		return err
	}
	if loc.Incarnation == old.Incarnation || loc.Sandbox.PodUID != old.Sandbox.PodUID || loc.Sandbox.PodUID != root.loc.Sandbox.PodUID {
		return fmt.Errorf("Resume(%s) returned %+v after %+v, want a new process generation in the unchanged issue pod %s", worker.token, loc.Sandbox, old.Sandbox, root.loc.Sandbox.PodUID)
	}
	note("runtime", "Resume(prev %s) returned %s in the same issue pod uid %s", short(old.Incarnation), short(loc.Incarnation), loc.Sandbox.PodUID)
	if reg.hash != tokenHash(worker.bootToken) {
		return fmt.Errorf("the registration's token hash %s is not generation %d's", short(reg.hash), worker.gen)
	}
	note("harness", "hello registered %s at generation %d, token sha256 %s… (that generation's)", worker.token, reg.gen, reg.hash[:12])
	lines, err := r.markerLines(worker)
	if err != nil {
		return err
	}
	note("operator", "exec cat %s: %v", worker.marker, lines)
	// The marker is the issue pod's (SandboxName), shared by every resident role that started in
	// it — the architect's own root-ready launch and any sibling role's first launch are also in
	// there — so only worker's own "implementer:" lines are its to check.
	own := roleIncarnations(lines, worker.role)
	if len(own) != 2 || own[0] != old.Incarnation || own[1] != loc.Incarnation {
		return fmt.Errorf("the marker's %s lines are %v (of %v shared by the issue pod's resident roles), want exactly [%s %s]", worker.role, own, lines, old.Incarnation, loc.Incarnation)
	}
	return nil
}

// stalePluginBlocked reports why stale-plugin-install cannot run: the script packs the two
// checkout tarballs and names them in these four variables together, so one set or none. Its
// negative control needs no tarball of its own: it names the published pi-legion@9.6.1 by exact
// version, which the install script resolves through the registry inside the pod.
func stalePluginBlocked(r *liveRig) string {
	if r.env.piLegionTarball != "" && r.env.piEnvoyTarball != "" && r.env.piLegionVersion != "" &&
		r.env.piEnvoyVersion != "" {
		return ""
	}
	return "LEGION_E2E_PI_LEGION_TARBALL/PI_ENVOY_TARBALL/PI_LEGION_VERSION/PI_ENVOY_VERSION are unset; an older invocation of scripts/e2e/stage4a-sandbox-runtime.sh packs no plugins"
}

// pluginPackageInfo reads an installed plugin's package.json through node, as the wrapper's own
// install check does. version is pkg.version; contract is pkg.legion.daemonApiVersion as a
// string, or "" when the package names none, as pi-envoy's does.
func (r *liveRig) pluginPackageInfo(c *liveClaim, path string) (version, contract string, err error) {
	const script = `const pkg = require(process.argv[1]); console.log(pkg.version); console.log(pkg.legion && typeof pkg.legion.daemonApiVersion === 'number' ? pkg.legion.daemonApiVersion : '');`
	out, err := r.exec(c, "node", "-e", script, path)
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", path, err)
	}
	lines := strings.SplitN(out, "\n", 2)
	version = strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		contract = strings.TrimSpace(lines[1])
	}
	return version, contract, nil
}

// containsConsecutive reports whether needle occurs, in order and contiguously, somewhere in
// haystack.
func containsConsecutive(haystack, needle []string) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

// stale-plugin-install (LEGION-164): a relaunch the supervisor marks as installing a stale-plugin
// pair keeps the worker's issue pod and installs the daemon-named pi-legion and pi-envoy under the
// generation's own directory, rather than replacing the pod or loading the image's copies: the
// pod's uid and creation time are unchanged, its Sandbox is Running (never suspended for a
// replacement — the unchanged pod uid already proves that, and with it that nothing was
// provisioned again), the installed pi-legion declares this run's contract, and the shim that
// forked the agent — the one process of the launch chain whose own cmdline the agent's own execs
// (into the install script, then the stub, then `sleep infinity`) never touch — names the two
// installed --extension roots. Its negative control relaunches again naming the published
// pi-legion@9.6.1 by exact version — the daemon resolves it to @sjawhar/pi-legion@9.6.1 before
// `bun add` ever sees it (LEGION-164), and bun fetches it from the registry inside the pod, so
// a devbox run proves the version→package mapping end to end, not just a tarball install. That
// release declares another contract: the install still lands (so the earlier generation's own
// directory is gone, replaced by this one's `rm -rf .../g*`) but the wrapper's own check ends the
// child before `exec`, which the runtime reports Gone, and the pod is still the one this check
// started with. kill-launcher, the next check to touch worker, resumes it with ensureRunning on a
// plain launch, so this check leaves no resume of its own after the negative control.
func (r *liveRig) checkStalePluginInstall() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureSuspended(worker); err != nil {
		return err
	}
	if err := r.ensureRunning(root); err != nil {
		return err
	}

	name := SandboxName(worker.token)
	before, err := r.getPod(name)
	if err != nil {
		return err
	}

	dir := roleStateHome(worker.role) + "/stage4a-plugin-tarballs"
	if _, err := r.exec(worker, "mkdir", "-p", dir); err != nil {
		return err
	}
	legionTar, envoyTar := dir+"/pi-legion.tgz", dir+"/pi-envoy.tgz"
	for _, cp := range []struct{ local, remote string }{
		{r.env.piLegionTarball, legionTar},
		{r.env.piEnvoyTarball, envoyTar},
	} {
		if _, err := r.kubectl("cp", cp.local, r.env.namespace+"/"+name+":"+cp.remote, "-c", string(worker.role)); err != nil {
			return fmt.Errorf("kubectl cp %s %s: %w", cp.local, cp.remote, err)
		}
	}
	note("operator", "kubectl cp the checkout's packed pi-legion@%s and pi-envoy@%s into %s:%s (-c %s)",
		r.env.piLegionVersion, r.env.piEnvoyVersion, name, dir, worker.role)

	old := *worker.last
	since := time.Now()
	loc, err := r.resumeWithPlugins(worker, worker.marker, &runtime.PluginInstall{PiLegion: legionTar, PiEnvoy: envoyTar, Contract: api.DaemonAPIVersion})
	if err != nil {
		return err
	}
	installedGen := worker.gen
	if _, err := r.awaitRunning(worker, since); err != nil {
		return err
	}
	if loc.Incarnation == old.Incarnation || loc.Sandbox.PodUID != old.Sandbox.PodUID || loc.Sandbox.PodUID != string(before.UID) {
		return fmt.Errorf("the stale-plugin Resume(%s) returned %+v, want a new process generation in the unchanged issue pod %s", worker.token, loc.Sandbox, before.UID)
	}
	after, err := r.getPod(name)
	if err != nil {
		return err
	}
	if after.UID != before.UID || !after.CreationTimestamp.Time.Equal(before.CreationTimestamp.Time) {
		return fmt.Errorf("the pod was replaced: uid %s→%s, created %s→%s", before.UID, after.UID, before.CreationTimestamp, after.CreationTimestamp)
	}
	s, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if s.mode() != modeRunning {
		return fmt.Errorf("the issue Sandbox is %s after an installing Resume, want Running: the pod's unchanged uid %s already proves it was never suspended for a replacement", s.mode(), after.UID)
	}
	note("runtime", "Resume(prev %s, Plugins) returned %s in the unchanged issue pod uid %s (created %s), Sandbox Running",
		short(old.Incarnation), short(loc.Incarnation), after.UID, after.CreationTimestamp)

	genDir := pluginGenDir(roleStateHome(worker.role), installedGen)
	envoyRoot, legionRoot := pluginRoots(genDir)
	legionVersion, legionContract, err := r.pluginPackageInfo(worker, legionRoot+"/package.json")
	if err != nil {
		return err
	}
	if legionVersion != r.env.piLegionVersion || legionContract != strconv.Itoa(api.DaemonAPIVersion) {
		return fmt.Errorf("%s/package.json: version %q contract %q, want version %q and contract %d", legionRoot, legionVersion, legionContract, r.env.piLegionVersion, api.DaemonAPIVersion)
	}
	envoyVersion, _, err := r.pluginPackageInfo(worker, envoyRoot+"/package.json")
	if err != nil {
		return err
	}
	if envoyVersion != r.env.piEnvoyVersion {
		return fmt.Errorf("%s/package.json: version %q, want %q", envoyRoot, envoyVersion, r.env.piEnvoyVersion)
	}
	note("operator", "%s/node_modules: @sjawhar/pi-legion@%s (contract %s), @sjawhar/pi-envoy@%s", genDir, legionVersion, legionContract, envoyVersion)

	agent, err := r.agentPid(worker)
	if err != nil {
		return err
	}
	shim, err := r.shimPid(worker, agent)
	if err != nil {
		return err
	}
	cmdline, err := r.procCmdline(worker, shim)
	if err != nil {
		return err
	}
	want := []string{"--extension", envoyRoot, "--extension", legionRoot}
	if !containsConsecutive(cmdline, want) {
		return fmt.Errorf("the shim's (pid %s) own cmdline is %q, want it to carry %v: the agent's own argv is lost once it execs through the install script into the stub and then into sleep infinity, but the shim never execs again after the launcher forked it with this generation's start command", shim, cmdline, want)
	}
	note("operator", "/proc/%s/cmdline (the shim, never replaced by the agent's own execs): carries %v", shim, want)

	// The negative control: the published pi-legion@9.6.1 by exact version. The daemon's loader
	// (config.readPluginSource) and the release's linked bare versions admit only an exact
	// version, an absolute tarball path, or an http(s) URL — never a dist-tag or a range — and
	// installBlock's bunSpec turns that version into @sjawhar/pi-legion@9.6.1 before bun ever
	// sees it (LEGION-164), so bun fetches it from the registry inside the pod: a devbox run
	// proves the version→package mapping end to end, not just a tarball install. The install
	// still lands — so the earlier generation's own directory is gone once this one's own `rm -rf
	// .../g*` has run — but the wrapper's own check ends the child before `exec`, and the runtime
	// observes it Gone.
	mark := r.obs.mark()
	negLoc, err := r.resumeWithPlugins(worker, worker.marker, &runtime.PluginInstall{PiLegion: "9.6.1", PiEnvoy: envoyTar, Contract: api.DaemonAPIVersion})
	if err != nil {
		return err
	}
	gone, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == worker.token && o.Locator.Incarnation == negLoc.Incarnation && o.Kind == runtime.Gone
	})
	if !ok {
		return fmt.Errorf("no Gone for the negative control's %s within %s", negLoc.Incarnation, liveGoneLimit)
	}
	note("runtime", "negative control: Observe gone for %s (the published pi-legion@9.6.1 declares contract 23, not %d) — %s", short(negLoc.Incarnation), api.DaemonAPIVersion, oneLine(firstLine(gone.Detail)))
	worker.loc, worker.state = nil, stateDead

	final, err := r.getPod(name)
	if err != nil {
		return err
	}
	if final.UID != before.UID {
		return fmt.Errorf("the negative control replaced the pod: uid %s→%s", before.UID, final.UID)
	}
	note("operator", "pod %s is still uid %s after the negative control", name, final.UID)

	oldGenDir := pluginGenDir(roleStateHome(worker.role), installedGen)
	if _, err := r.exec(worker, "sh", "-c", `test ! -e "$1"`, "sh", oldGenDir); err != nil {
		return fmt.Errorf("%s still exists after the negative control's own install ran rm -rf .../g* first: %w", oldGenDir, err)
	}
	note("operator", "the earlier generation's %s is gone", oldGenDir)
	return nil
}

// same-agent-negative: a resume naming a session the issue's volume does not hold never starts a
// fresh agent. The role launcher's start refuses before any child runs (manager.start's stat of
// ResumeFile, packages/daemon/internal/launcher/launcher.go), so Resume fails synchronously —
// there is no process to observe Gone — and the shared pod stays available to peers.
func (r *liveRig) checkSameAgentNegative() error {
	root, second := r.claim("root"), r.claim("second")
	if err := r.ensureSuspended(second); err != nil {
		return err
	}
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	// The marker is the issue pod's (SandboxName), shared by every resident role that started in
	// it; second's own "tester:" lines include role-container-isolation's gen-1 line only once
	// its stub agent's write has actually landed on the issue's volume — a race against how quickly
	// that check moved on to suspend it (it waits only for the hello, never the write). Snapshot
	// second's own lines now, before this check's own writes, so what follows asserts only what
	// this check itself causes.
	before, err := r.markerLines(second)
	if err != nil {
		return err
	}
	beforeOwn := len(roleIncarnations(before, second.role))
	absent := ompSessionsDir + "/absent-" + SandboxName(second.token) + ".marker"
	_, err = r.resume(second, absent)
	if err == nil {
		return fmt.Errorf("Resume naming %s started a fresh agent, want the role launcher's refusal before any child runs", absent)
	}
	want := "resume session file " + absent + ": stat " + absent + ": no such file or directory"
	if !strings.Contains(err.Error(), want) {
		return fmt.Errorf("Resume naming %s failed with %q, want it to quote the missing session file %q", absent, err, want)
	}
	note("runtime", "Resume naming %s refused before any child started: %s", absent, oneLine(err.Error()))
	if regs := r.reg.registrations(second.token); len(regs) > 0 && regs[len(regs)-1].gen == second.gen {
		return errors.New("the refused generation registered a hello")
	}

	since := time.Now()
	fixed, err := r.resume(second, second.marker)
	if err != nil {
		return err
	}
	if _, err := r.awaitRunning(second, since); err != nil {
		return err
	}
	if fixed.Sandbox.PodUID != root.loc.Sandbox.PodUID {
		return fmt.Errorf("the fixed resume runs in pod %s, not the root's unchanged issue pod %s: the refusal must have disturbed the shared pod", fixed.Sandbox.PodUID, root.loc.Sandbox.PodUID)
	}
	lines, err := r.markerLines(second)
	if err != nil {
		return err
	}
	note("operator", "resumed correctly as %s; exec cat %s: %v", short(fixed.Incarnation), second.marker, lines)
	// Only second's own "tester:" lines are its to check (the marker is shared by every resident
	// role of the issue pod); the refused generation above never started a child, so it never
	// wrote one — this asserts only the one new line this check's own correct resume caused,
	// never assuming role-container-isolation's earlier gen-1 line had already landed.
	own := roleIncarnations(lines, second.role)
	if len(own) != beforeOwn+1 || own[len(own)-1] != fixed.Incarnation {
		return fmt.Errorf("the marker's %s lines went from %v to %v (of %v shared by the issue pod's resident roles): want exactly one more, the last %s", second.role, roleIncarnations(before, second.role), own, lines, fixed.Incarnation)
	}
	if err := r.suspend(second); err != nil {
		return err
	}
	note("runtime", "second worker suspended again, for the checks that need a suspended claim")
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// kill-launcher: a worker role's launcher PID 1 ends in place, is reported Gone for that role
// process, and Resume(prev=dead) starts a new generation in the same issue pod.
func (r *liveRig) checkKillLauncher() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.ensureRunning(worker); err != nil {
		return err
	}
	old := *worker.loc
	name := SandboxName(worker.token)
	mark := r.obs.mark()
	if _, err := r.exec(worker, "sh", "-c", "kill 1"); err != nil {
		return err
	}
	note("operator", "exec %s -c %s -- sh -c 'kill 1'", name, worker.role)
	gone, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == worker.token && o.Kind == runtime.Gone
	})
	if !ok {
		return fmt.Errorf("no gone for %s within %s", old.Incarnation, liveGoneLimit)
	}
	if !sameLocator(gone.Locator, old) || !strings.Contains(gone.Detail, string(old.Sandbox.PodUID)) || !strings.Contains(gone.Detail, "container "+old.Sandbox.Container+" terminated") {
		return fmt.Errorf("the gone carries %s, want the old %s with the container's exit: %s", gone.Locator.Incarnation, old.Incarnation, gone.Detail)
	}
	exit := regexp.MustCompile(`exit code (-?\d+)`).FindStringSubmatch(gone.Detail)
	if exit == nil {
		return fmt.Errorf("the gone names no exit code: %s", gone.Detail)
	}
	note("runtime", "Observe: gone for old %s, role container %s exit code %s — %s", short(old.Incarnation), old.Sandbox.Container, exit[1], oneLine(firstLine(gone.Detail)))
	worker.loc, worker.state = nil, stateDead
	before, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if before.mode() != modeRunning {
		return fmt.Errorf("the issue pod Sandbox is %s before the relaunch", before.mode())
	}
	since := time.Now()
	fresh, err := r.resume(worker, worker.marker)
	if err != nil {
		return err
	}
	if _, err := r.awaitRunning(worker, since); err != nil {
		return err
	}
	after, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if fresh.Incarnation == old.Incarnation || fresh.Sandbox.PodUID != old.Sandbox.PodUID || after.Generation != before.Generation {
		return fmt.Errorf("Resume(prev=dead) returned %+v over Sandbox generation %d → %d, want a new process in unchanged pod %s and Sandbox generation", fresh.Sandbox, before.Generation, after.Generation, old.Sandbox.PodUID)
	}
	if rootObs, err := r.rt.Probe(r.ctx, *root.loc); err != nil || rootObs.Kind != runtime.Alive {
		return fmt.Errorf("root after worker launcher restart is %s: %v", rootObs.Kind, err)
	}
	note("runtime", "Resume(prev=dead %s) returned %s in the same pod uid %s; root stayed Alive", short(old.Incarnation), short(fresh.Incarnation), fresh.Sandbox.PodUID)
	r.killed.old, r.killed.fresh, r.killed.gone = old, fresh, gone
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// lineContaining returns the first line of text that contains want, or "" when none does; the
// callers assert want is there first, so "" never reaches a note.
func lineContaining(text, want string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}

// stale-incarnation: across the kill and the relaunch, nothing about the old process reaches the
// new one.
func (r *liveRig) checkStaleIncarnation() error {
	if r.killed.fresh.Incarnation == "" {
		return errors.New("kill-launcher did not run; stale-incarnation rides its relaunch")
	}
	worker := r.claim("worker")
	old, fresh := r.killed.old, r.killed.fresh
	time.Sleep(liveSettle)
	seen := 0
	for _, o := range r.obs.since(0) {
		if o.Locator.Incarnation != fresh.Incarnation {
			continue
		}
		seen++
		if o.Kind != runtime.Alive && o.Kind != runtime.Uncertain {
			return fmt.Errorf("an observation carrying the new %s is %s: %s", fresh.Incarnation, o.Kind, o.Detail)
		}
	}
	if seen == 0 {
		return fmt.Errorf("no observation carries the new %s after %s, so none could be judged", fresh.Incarnation, liveSettle)
	}
	note("runtime", "%d observations carry the new %s, every one alive or uncertain", seen, short(fresh.Incarnation))
	if r.killed.gone.Locator.Incarnation != old.Incarnation {
		return fmt.Errorf("the gone carried %s, not the old %s", r.killed.gone.Locator.Incarnation, old.Incarnation)
	}
	note("runtime", "the gone carried the old incarnation %s", short(old.Incarnation))
	if err := r.rt.Suspend(r.ctx, old); err != nil {
		return fmt.Errorf("Suspend(the old locator): %w", err)
	}
	name := SandboxName(worker.token)
	s, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	uid, err := r.kubectl("get", "pod", name, "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		return err
	}
	if s.mode() != modeRunning || string(pod.UID) != fresh.Sandbox.PodUID || uid != fresh.Sandbox.PodUID {
		return fmt.Errorf("Suspend(old) acted: Sandbox %s, pod uid %s (operator: %s), want Running and recorded pod uid %s", s.mode(), pod.UID, uid, fresh.Sandbox.PodUID)
	}
	note("runtime", "Suspend(old %s): nil; Sandbox still Running, pod still %s", short(old.Incarnation), short(string(pod.UID)))
	note("operator", "pod %s uid %s", name, uid)
	return nil
}

// respawn-before-register: a role stopped before its first hello starts again in the existing issue
// pod at a new process generation. The new hello carries only the second generation's boot token.
func (r *liveRig) checkRespawnBeforeRegister() error {
	fresh := r.claim("fresh")
	if err := r.ensureRunning(r.claim("root")); err != nil {
		return err
	}
	first, err := r.spawn(fresh, false)
	if err != nil {
		return err
	}
	if err := r.suspend(fresh); err != nil {
		return err
	}
	if regs := r.reg.registrations(fresh.token); len(regs) > 0 {
		return fmt.Errorf("the claim registered before its first suspend: %+v", regs)
	}
	note("runtime", "Spawn returned %s; suspended before any hello (its token is withheld from the resolver: %d hellos refused, none registered)", short(first.Incarnation), r.reg.refusals(fresh.token))
	fresh.state = stateNone // the machine's view: never registered, so the next launch is a Spawn
	since := time.Now()
	second, err := r.spawn(fresh, true)
	if err != nil {
		return fmt.Errorf("the second Spawn in the existing issue pod: %w", err)
	}
	reg, err := r.awaitRunning(fresh, since)
	if err != nil {
		return err
	}
	if second.Incarnation == first.Incarnation || second.Sandbox.PodUID != first.Sandbox.PodUID || second.Sandbox.Container != first.Sandbox.Container {
		return fmt.Errorf("the second Spawn returned %+v after %+v, want a new role process in the same issue pod and container", second.Sandbox, first.Sandbox)
	}
	if reg.hash != tokenHash(fresh.bootToken) || reg.gen != 2 {
		return fmt.Errorf("registered at generation %d with %s, want 2 with the second generation token %s", reg.gen, short(reg.hash), short(tokenHash(fresh.bootToken)))
	}
	note("runtime", "second Spawn returned %s in the same issue pod %s", short(second.Incarnation), second.Sandbox.PodUID)
	note("harness", "hello registered at generation 2 with that token")
	return nil
}

// independent-provision: a new tree's root and its child, spawned at once, each get an issue pod of
// their own: a Sandbox with one `issue` claim template, a PersistentVolumeClaim of its own selected
// by the issue's label and Bound, a clone with no lock beside it and a jj workspace of that issue
// alone, standing at the repository's default branch as GitHub names it (its init log names the
// branch, and the workspace's @- is the clone's <default branch>@origin), each clone passing git
// fsck. Nothing orders the two provisions: each works on its own volume. Its negative control is
// the other issue's workspace: the root's pod cannot list the child's workspace directory, which
// exists in the child's pod, and the child's pod cannot list the root's. Which node each pod lands
// on is not checked: no pod asks for or keeps off another pod's node (LEGION-632), and what a pod
// asks of its node is the `resources` check's.
func (r *liveRig) checkIndependentProvision() error {
	root := r.claim("root")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	root2, child := r.claim("root2"), r.claim("child2")
	since := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []*liveClaim{root2, child} {
		wg.Go(func() { _, errs[i] = r.spawn(c, true) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	note("runtime", "Spawn(%s) and Spawn(%s) called at once", root2.token, child.token)
	owner, repo := r.env.repo.Owner(), r.env.repo.Name()
	clone := TreeRoot + "/repos/github.com/" + owner + "/" + repo
	pvcs := map[string]string{}
	for _, c := range []*liveClaim{root2, child} {
		if _, err := r.awaitRunning(c, since); err != nil {
			return err
		}
		name := SandboxName(c.token)
		log, err := r.initLog(name)
		if err != nil {
			return err
		}
		dir, _ := workspace.Location(TreeRoot, r.env.repo, c.issue)
		want := "workspace-init: " + dir.Dir + " on " + dir.Bookmark + " from " + r.defaultBranch + "\n"
		if !strings.Contains(log, want) {
			return fmt.Errorf("%s's init log lacks %q: %s", c.name, strings.TrimSpace(want), strings.TrimSpace(log))
		}
		s, err := r.getSandbox(name)
		if err != nil {
			return err
		}
		if len(s.Spec.VolumeClaimTemplates) != 1 || s.Spec.VolumeClaimTemplates[0].Metadata.Name != issueVolume {
			return fmt.Errorf("Sandbox %s carries %d volume claim templates %v, want one named %s", name, len(s.Spec.VolumeClaimTemplates), templateNames(s), issueVolume)
		}
		bound, err := r.issuePVCs(c.issue)
		if err != nil {
			return err
		}
		if want := IssueClaimName(c.token) + " Bound"; !slices.Equal(bound, []string{want}) {
			return fmt.Errorf("the PVCs labelled %s=%s are %v, want exactly [%s]", labelIssue, labelValue(c.issue), bound, want)
		}
		pvcs[c.name] = IssueClaimName(c.token)
		listing, err := r.exec(c, "ls", "-A", filepath.Dir(clone))
		if err != nil {
			return err
		}
		entries := strings.Fields(listing)
		if !slices.Equal(entries, []string{repo}) {
			return fmt.Errorf("%s's %s holds %v, want the clone %s alone and no lock beside it", c.name, filepath.Dir(clone), entries, repo)
		}
		workspaces, err := r.exec(c, "jj", "-R", clone, "workspace", "list")
		if err != nil {
			return err
		}
		other := root2
		if c == root2 {
			other = child
		}
		if !strings.Contains(workspaces, strings.ToLower(c.issue)+":") || strings.Contains(workspaces, strings.ToLower(other.issue)+":") {
			return fmt.Errorf("%s's clone lists workspaces %q, want %s's own and not %s's", c.name, oneLine(workspaces), strings.ToLower(c.issue), strings.ToLower(other.issue))
		}
		parent, err := r.exec(c, "jj", "-R", dir.Dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy")
		if err != nil {
			return err
		}
		// git reads a ref name literally, where a jj revset would parse it: the colocated clone's
		// refs/remotes/origin/<default branch> is the commit jj's <default branch>@origin names.
		head, err := r.exec(c, "git", "--git-dir="+clone+"/.git", "rev-parse", "--verify", "refs/remotes/origin/"+r.defaultBranch+"^{commit}")
		if err != nil {
			return err
		}
		if parent, head = strings.TrimSpace(parent), strings.TrimSpace(head); parent == "" || parent != head {
			return fmt.Errorf("%s's workspace @- is %q, want the default branch %s's head %q", c.name, parent, r.defaultBranch, head)
		}
		note("operator", "%s: workspace @- %s = default_branch %s @ %s", c.name, parent, r.defaultBranch, head)
		fsck, err := r.exec(c, "git", "--git-dir="+clone+"/.git", "fsck", "--connectivity-only", "--no-progress")
		if err != nil {
			return fmt.Errorf("git fsck of %s's clone: %w", c.name, err)
		}
		note("runtime", "%s: init log %q; Sandbox %s owns one claim template %s", c.name, strings.TrimSpace(want), name, issueVolume)
		note("operator", "%s: PVC %s (by %s=%s): Bound; exec ls -A %s: %v; jj workspace list: %s; git fsck --connectivity-only: ok %s",
			c.name, IssueClaimName(c.token), labelIssue, labelValue(c.issue), filepath.Dir(clone), entries, oneLine(workspaces), oneLine(fsck))
	}
	if pvcs["root2"] == pvcs["child2"] {
		return fmt.Errorf("root2 and child2 share PVC %s", pvcs["root2"])
	}
	// The negative control: each pod holds its own issue's workspace and cannot see the other's.
	for _, pair := range [][2]*liveClaim{{root2, child}, {child, root2}} {
		own, other := pair[0], pair[1]
		theirs, _ := workspace.Location(TreeRoot, r.env.repo, other.issue)
		if _, err := r.exec(other, "ls", theirs.Dir); err != nil {
			return fmt.Errorf("%s's own workspace %s is not in its pod: %w", other.name, theirs.Dir, err)
		}
		out, err := r.exec(own, "ls", theirs.Dir)
		if err == nil {
			return fmt.Errorf("%s's pod lists %s's workspace %s: %s", own.name, other.name, theirs.Dir, oneLine(out))
		}
		if !strings.Contains(err.Error(), "No such file or directory") {
			return fmt.Errorf("%s's pod failed to list %s's workspace %s for another reason than its absence: %w", own.name, other.name, theirs.Dir, err)
		}
		note("operator", "exec %s -- ls %s: No such file or directory; the same path exists in %s's pod", SandboxName(own.token), theirs.Dir, other.name)
	}
	return nil
}

// second-owner: an issue on a repository of another owner (LEGION_E2E_REPO_SECOND_OWNER) gets an
// issue pod whose workspace stands at that repository's default branch as GitHub names it, and
// whose gh bundle holds a directory for each owner of the run's repositories: the agent is told the
// bundle's root (LEGION_GH_OWNERS_DIR), its own owner's directory (GH_CONFIG_DIR) and the real gh
// (LEGION_GH_PATH). Under the agent's own environment the image's gh, the `legion gh` wrapper,
// reads LEGION_E2E_REPO from that owner's read-only directory, while the real gh with the issue
// owner's directory cannot (404: the negative control that proves the wrapper's pick, since
// LEGION_E2E_REPO is private to its owner); git, through the clone's `legion git-credential`
// helper, lists LEGION_E2E_REPO's refs and is refused a push there (403: the token is read-only);
// and GitHub, read with the operator's gh, holds no branch of that push. Nothing is pushed to the
// second owner's repository. That repository's cache is its own: the issue's launch makes it (its
// Sandbox recorded for the teardown first), and the issue's feed comes from it alone.
func (r *liveRig) checkSecondOwner() error {
	c := r.claim("second-owner")
	cache := CacheSandboxName(r.env.project, c.repo)
	if err := r.recordSandbox(cache); err != nil {
		return err
	}
	r.mapRepository(c.repo)
	since := time.Now()
	if _, err := r.spawn(c, true); err != nil {
		return err
	}
	if _, err := r.awaitRunning(c, since); err != nil {
		return err
	}
	log, err := r.initLog(SandboxName(c.token))
	if err != nil {
		return err
	}
	dir, _ := workspace.Location(TreeRoot, c.repo, c.issue)
	if want := "workspace-init: " + dir.Dir + " on " + dir.Bookmark + " from " + r.secondDefaultBranch + "\n"; !strings.Contains(log, want) {
		return fmt.Errorf("%s's init log lacks %q: %s", c.name, strings.TrimSpace(want), strings.TrimSpace(log))
	}
	note("runtime", "%s on %s: init log names %s from %s", c.name, c.repo, dir.Bookmark, r.secondDefaultBranch)
	if err := r.assertCacheFed(c); err != nil {
		return err
	}
	agent, err := r.agentEnviron(c)
	if err != nil {
		return err
	}
	own, other := strings.ToLower(c.repo.Owner()), strings.ToLower(r.env.repo.Owner())
	for name, want := range map[string]string{
		"LEGION_GH_OWNERS_DIR": GHConfigDir, "GH_CONFIG_DIR": GHConfigDir + "/" + own, "LEGION_GH_PATH": "/usr/local/bin/gh",
	} {
		if agent[name] != want {
			return fmt.Errorf("%s's agent is told %s=%q, want %q", c.name, name, agent[name], want)
		}
	}
	owners, err := r.asAgent(c, `ls "$LEGION_GH_OWNERS_DIR"`)
	if err != nil {
		return err
	}
	if got, want := strings.Fields(owners), []string{min(own, other), max(own, other)}; !slices.Equal(got, want) {
		return fmt.Errorf("%s's LEGION_GH_OWNERS_DIR lists %v, want %v", c.name, got, want)
	}
	note("operator", "%s's agent: LEGION_GH_OWNERS_DIR=%s lists %v; GH_CONFIG_DIR=%s", c.name, GHConfigDir, strings.Fields(owners), agent["GH_CONFIG_DIR"])
	read, err := r.asAgent(c, "gh api repos/"+r.env.repo.String()+" --jq .full_name")
	if err != nil {
		return fmt.Errorf("%s's gh could not read %s: %w", c.name, r.env.repo, err)
	}
	if read != r.env.repo.String() {
		return fmt.Errorf("%s's gh api repos/%s answered %q, want %s", c.name, r.env.repo, read, r.env.repo)
	}
	note("operator", "%s's agent: gh api repos/%s --jq .full_name = %s (the wrapper picked %s's read-only directory)", c.name, r.env.repo, read, other)
	out, err := r.asAgent(c, `env GH_CONFIG_DIR="$LEGION_GH_OWNERS_DIR/`+own+`" "$LEGION_GH_PATH" api repos/`+r.env.repo.String()+` 2>&1`)
	if err == nil || !strings.Contains(out+err.Error(), "404") {
		return fmt.Errorf("the real gh with %s's own directory read %s (%v): %s; want a 404, or the wrapper's pick proves nothing", c.name, r.env.repo, err, oneLine(out))
	}
	note("operator", "negative control: the real gh with GH_CONFIG_DIR=%s/%s on repos/%s: 404", GHConfigDir, own, r.env.repo)
	clone := TreeRoot + "/repos/github.com/" + c.repo.Owner() + "/" + c.repo.Name()
	url := "https://github.com/" + r.env.repo.String()
	refs, err := r.asAgent(c, "git --git-dir="+clone+"/.git ls-remote "+url+" HEAD")
	if err != nil || !strings.Contains(refs, "HEAD") {
		return fmt.Errorf("%s's git ls-remote %s through the clone's helper = %q, %v; want its HEAD", c.name, url, refs, err)
	}
	note("operator", "%s's agent: git ls-remote %s HEAD: %s", c.name, url, oneLine(refs))
	// The run's own name: if the refusal ever broke, the push would land on the repository every run
	// shares, and only this run's "want none" below should fail for it.
	branch := "legion/" + strings.ToLower(c.issue) + "-must-not-land-" + r.env.claimProject
	out, err = r.asAgent(c, "git --git-dir="+clone+"/.git push "+url+" HEAD:refs/heads/"+branch+" 2>&1")
	if err == nil || !strings.Contains(out+err.Error(), "403") {
		return fmt.Errorf("%s's git push to %s was not refused 403 (%v): %s", c.name, url, err, oneLine(out))
	}
	note("operator", "%s's agent: git push %s HEAD:refs/heads/%s: 403", c.name, url, branch)
	ctx, cancel := context.WithTimeout(r.ctx, time.Minute)
	defer cancel()
	check := exec.CommandContext(ctx, "gh", "api", "repos/"+r.env.repo.String()+"/branches/"+branch)
	if answer, err := check.CombinedOutput(); err == nil || !strings.Contains(string(answer), "404") {
		return fmt.Errorf("the operator's gh reads branch %s on %s (%v): %s; want none", branch, r.env.repo, err, oneLine(string(answer)))
	}
	note("operator", "gh api repos/%s/branches/%s (the operator's gh): 404: no branch %s on %s", r.env.repo, branch, branch, r.env.repo)
	return nil
}

// asAgent runs script with sh in the claim's container under its stub agent's own environment,
// read from /proc/<pid>/environ, as the agent's bash tool would run it: an exec carries none of
// the variables the agent's launcher start command told it.
func (r *liveRig) asAgent(c *liveClaim, script string) (string, error) {
	pid, err := r.agentPid(c)
	if err != nil {
		return "", err
	}
	return r.exec(c, "sh", "-c", `xargs -0 sh -c 'script=$1; shift; exec env -i "$@" sh -c "$script"' agent-env "$2" <"/proc/$1/environ"`, "as-agent", pid, script)
}

// templateNames are the names of a Sandbox's volume claim templates.
func templateNames(s *sandbox) []string {
	names := make([]string, 0, len(s.Spec.VolumeClaimTemplates))
	for _, t := range s.Spec.VolumeClaimTemplates {
		names = append(names, t.Metadata.Name)
	}
	return names
}

// re-adopt: a fresh runtime and listener take over every live claim, with nothing relaunched; a
// separate issue pod killed while no runtime ran is reported dead with its recorded incarnation.
// The Agent Sandbox controller (sandbox_controller.go's reconcilePod, v1.0.3) recreates a Running
// Sandbox's pod under the identical name the moment the old one is gone — the name is
// deterministic (resolvePodName) and the Pod is Owns()-watched — so only the killed pod's own uid
// ever actually ends; the daemon's own evaluate() reports a differently-uid'd pod at that name as
// NotRecordedProcess, never Gone, and the supervisor treats the two identically (machine.go).
func (r *liveRig) checkReAdopt() error {
	victim := r.claim("orphan")
	for _, name := range []string{"root", "worker", "fresh", "root2", "child2", "orphan"} {
		if err := r.ensureRunning(r.claim(name)); err != nil {
			return err
		}
	}
	if err := r.ensureSuspended(r.claim("second")); err != nil {
		return err
	}
	generations := map[string]int64{}
	for _, c := range r.live() {
		s, err := r.getSandbox(SandboxName(c.token))
		if err != nil {
			return err
		}
		generations[c.name] = s.Generation
	}
	r.stopRuntime()
	note("runtime", "listener and runtime closed")
	name := SandboxName(victim.token)
	oldUID := victim.loc.Sandbox.PodUID
	if _, err := r.kubectl("delete", "pod", name, "--wait=false"); err != nil {
		return err
	}
	if err := r.poll(liveGoneLimit, "orphan issue pod "+name+"'s killed uid "+short(oldUID)+" to end", func() (bool, error) {
		pod, err := r.getPod(name)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return string(pod.UID) != oldUID, nil
	}); err != nil {
		return err
	}
	note("operator", "deleted pod %s (uid %s) while no runtime ran; the controller may already have replaced it under the same name before anything watched", name, short(oldUID))

	restarted := time.Now()
	mark := r.obs.mark()
	if err := r.startRuntime(); err != nil {
		return err
	}
	known := r.known(nil)
	if err := r.rt.ReconcileOrphans(r.ctx, known, time.Hour); err != nil {
		return err
	}
	note("runtime", "fresh listener and sandbox.New; ReconcileOrphans with %d known claims", len(known))

	gone, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == victim.token && o.Kind != runtime.Alive
	})
	if !ok || (gone.Kind != runtime.Gone && gone.Kind != runtime.NotRecordedProcess) || !sameLocator(gone.Locator, *victim.loc) {
		return fmt.Errorf("the killed claim was not reported dead with its recorded %s: %+v", victim.loc.Incarnation, gone)
	}
	note("runtime", "killed claim: %s, stamped %s — %s", gone.Kind, short(gone.Locator.Incarnation), oneLine(firstLine(gone.Detail)))
	victim.loc, victim.state = nil, stateDead

	for _, c := range r.live() {
		loc := *c.loc
		alive, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
			return o.Locator.Claim == c.token && o.Kind == runtime.Alive
		})
		if !ok || !sameLocator(alive.Locator, loc) {
			return fmt.Errorf("%s never reached alive at its recorded %s within %s", c.name, loc.Incarnation, liveGoneLimit)
		}
		// A role launcher that has not yet redialed the fresh listener — every real launcher
		// retries every second (internal/launcher/launcher.go's reconnectDelay), and a fresh
		// sweep can run before that — is legitimately Uncertain for a probe interval or two first
		// (observe.go's "disconnected" branch); the supervisor's judge() only counts the streak
		// and re-arms the same probe, never a deadline (TestADisconnectedLauncherAtReadoptionIs-
		// UncertainThenAliveNeverRelaunched). Anything else before alive — a different locator (something
		// was relaunched) or a death verdict — is a bug, not a race.
		for _, o := range r.obs.since(mark) {
			if o.Locator.Claim != c.token || o.Kind == runtime.Alive {
				continue
			}
			if o.Kind != runtime.Uncertain || !sameLocator(o.Locator, loc) {
				return fmt.Errorf("%s's observation before reaching alive is %+v, want only launcher-disconnected Uncertain at its recorded %s", c.name, o, loc.Incarnation)
			}
		}
		reg, err := r.awaitHelloAgain(c, restarted)
		if err != nil {
			return err
		}
		uid, err := r.kubectl("get", "pod", SandboxName(c.token), "-o", "jsonpath={.metadata.uid}")
		if err != nil {
			return err
		}
		s, err := r.getSandbox(SandboxName(c.token))
		if err != nil {
			return err
		}
		if uid != loc.Sandbox.PodUID || s.Generation != generations[c.name] {
			return fmt.Errorf("%s was relaunched: pod uid %s (recorded %s), Sandbox generation %d → %d", c.name, uid, loc.Sandbox.PodUID, generations[c.name], s.Generation)
		}
		note("runtime", "%s: alive with recorded %s; Sandbox generation %d unchanged", c.name, short(loc.Incarnation), s.Generation)
		note("harness", "%s: hello again at generation %d, token sha256 %s…", c.name, reg.gen, reg.hash[:12])
		note("operator", "%s: pod uid %s", c.name, short(uid))
	}
	for _, o := range r.obs.since(mark) {
		if o.Locator.Claim != victim.token && (o.Kind == runtime.Gone || o.Kind == runtime.NotRecordedProcess) {
			return fmt.Errorf("re-adoption observed %s for %s: %s", o.Kind, o.Locator.Claim, o.Detail)
		}
	}
	return nil
}

// orphan-sweep: a Sandbox of the project that no claim records survives a sweep inside the grace;
// past it, it survives while its tree's lifecycle is open (a claim launched after the daemon read
// the claims it sweeps with), and is deleted once its tree's cleanup confirmed, by a fresh runtime
// as by any: what a launch that reached the API after its tree's cleanup listed it leaves. The
// issue's volume goes with its Sandbox, which owns it (the sweep's delete propagates in the
// background, so the PVC follows the Sandbox through its owner reference). A suspended claim's
// Sandbox, of a live tree, survives every sweep.
func (r *liveRig) checkOrphanSweep() error {
	orphan, suspended := r.claim("orphan"), r.claim("second")
	if err := r.ensureRunning(r.claim("root")); err != nil {
		return err
	}
	if err := r.ensureSuspended(suspended); err != nil {
		return err
	}
	if err := r.ensureRunning(orphan); err != nil {
		return err
	}
	name, keep := SandboxName(orphan.token), SandboxName(suspended.token)
	if err := r.rt.ReconcileOrphans(r.ctx, r.known(orphan), time.Hour); err != nil {
		return err
	}
	for _, n := range []string{name, keep} {
		s, err := r.getSandbox(n)
		if err != nil || s.DeletionTimestamp != nil {
			return fmt.Errorf("Sandbox %s did not survive a sweep inside the grace: %v", n, err)
		}
	}
	note("runtime", "sweep with grace 1h, the orphan unrecorded: %s and the suspended %s survive", name, keep)
	time.Sleep(2 * time.Second)
	if err := r.rt.ReconcileOrphans(r.ctx, r.known(orphan), time.Second); err != nil {
		return err
	}
	if s, err := r.getSandbox(name); err != nil || s.DeletionTimestamp != nil {
		return fmt.Errorf("unrecorded %s of the live tree %s did not survive a sweep past the grace: %v", name, orphan.tree, err)
	}
	note("runtime", "sweep with grace 1s while its tree %s is live: %s survives", orphan.tree, name)
	r.trees.close(orphan.tree)
	r.stopRuntime()
	restarted := time.Now()
	if err := r.startRuntime(); err != nil {
		return err
	}
	note("runtime", "tree %s's cleanup recorded confirmed; listener and runtime replaced", orphan.tree)
	if err := r.rt.ReconcileOrphans(r.ctx, r.known(orphan), time.Second); err != nil {
		return err
	}
	if err := r.poll(liveGoneLimit, "orphan Sandbox "+name+" to be deleted", func() (bool, error) {
		_, err := r.getSandbox(name)
		return apierrors.IsNotFound(err), ignoreNotFound(err)
	}); err != nil {
		return err
	}
	for _, c := range r.claims {
		if c == orphan || c.state == stateNone || c.state == stateReleased {
			continue
		}
		s, err := r.getSandbox(SandboxName(c.token))
		if err != nil || s.DeletionTimestamp != nil {
			return fmt.Errorf("the sweep past the grace took known claim %s's Sandbox: %v", c.name, err)
		}
	}
	note("runtime", "sweep with grace 1s: %s deleted; every known claim's Sandbox, the suspended %s included, survives", name, keep)
	if err := r.poll(liveGoneLimit, "the swept issue "+orphan.issue+"'s PVC to be gone", func() (bool, error) {
		pvcs, err := r.issuePVCs(orphan.issue)
		return len(pvcs) == 0, err
	}); err != nil {
		return err
	}
	note("operator", "get pvc -l %s=%s: none; the swept Sandbox took the issue's volume %s with it", labelIssue, labelValue(orphan.issue), IssueClaimName(orphan.token))
	orphan.loc, orphan.state = nil, stateReleased
	for _, c := range r.live() {
		if _, err := r.awaitHelloAgain(c, restarted); err != nil {
			return err
		}
	}
	return nil
}

// release-preserves-issue: release ends only the named role process. The shared issue Sandbox,
// root process and the issue's PVC stay until the daemon's durable whole-issue cleanup effect runs;
// this direct runtime harness deliberately has no store/outbox and never substitutes a local cleanup.
func (r *liveRig) checkReleasePreservesIssue() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.ensureRunning(worker); err != nil {
		return err
	}
	loc := *worker.loc
	name := SandboxName(worker.token)
	if err := r.rt.Release(r.ctx, runtime.Known{Claim: worker.token, Locator: &loc}); err != nil {
		return fmt.Errorf("Release(%s): %w", worker.name, err)
	}
	worker.loc, worker.state = nil, stateReleased
	s, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if s.mode() != modeRunning {
		return fmt.Errorf("Release(%s) changed issue Sandbox %s to %s", worker.name, name, s.mode())
	}
	// The root must not die with its sibling's release. Its launcher can still be redialling the
	// listener the previous check restarted (orphan-sweep waits for every agent's hello, not for
	// its launcher, whose hello the listener authenticates with a read of the role's Secret the
	// client may throttle), and an unconnected launcher reads Uncertain; so the root is polled
	// until Alive, any other verdict failing at once, then probed again after liveSettle, so a root
	// that reads Alive and dies moments later fails too.
	var last runtime.Observation
	var verdict error
	err = r.poll(liveGoneLimit, "root "+string(root.token)+" to read Alive after Release("+worker.name+")", func() (bool, error) {
		obs, err := r.rt.Probe(r.ctx, *root.loc)
		if err != nil {
			verdict = fmt.Errorf("probe root %s after Release(%s): %w", root.token, worker.name, err)
			return false, verdict
		}
		last = obs
		switch obs.Kind {
		case runtime.Alive:
			return true, nil
		case runtime.Uncertain:
			return false, nil
		}
		verdict = fmt.Errorf("root %s after Release(%s) is %s: %s", root.token, worker.name, obs.Kind, obs.Detail)
		return false, verdict
	})
	switch {
	case err == nil:
	case verdict != nil:
		return verdict
	case last.Kind != "":
		return fmt.Errorf("%w; last observation %s: %s", err, last.Kind, last.Detail)
	default:
		return err
	}
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	case <-time.After(liveSettle):
	}
	settled, err := r.rt.Probe(r.ctx, *root.loc)
	if err != nil {
		return fmt.Errorf("probe root %s %s after it read Alive following Release(%s): %w", root.token, liveSettle, worker.name, err)
	}
	if settled.Kind != runtime.Alive {
		return fmt.Errorf("root %s read Alive after Release(%s), and %s later is %s: %s", root.token, worker.name, liveSettle, settled.Kind, settled.Detail)
	}
	// The released worker and the root are claims of one issue, so either token names its PVC.
	pvc := IssueClaimName(worker.token)
	phase, err := r.kubectl("get", "pvc", pvc, "-o", "jsonpath={.status.phase}")
	if err != nil || phase != "Bound" {
		return fmt.Errorf("the issue's PVC %s after Release(%s) is %q: %v", pvc, worker.name, phase, err)
	}
	note("runtime", "Release(%s) ended only that role; issue Sandbox %s and root %s stayed Alive", worker.name, name, root.token)
	note("operator", "PVC %s: Bound", pvc)
	return nil
}

// TestIsGKEExtendedDurationAffinity covers the matcher's whole class of mistake: checking only the
// key of the one tolerated NodeAffinity shape, not its operator, would accept a refusal
// (NotIn/DoesNotExist on the same key) wearing the one shape meant to be tolerated. The `In`
// operator's own values are GKE's to pick per pod (confirmed live, 2026-10-17: a six-role root pod
// carried `["2500m", "X"]`, not a fixed literal), so any non-empty value list is tolerated.
func TestIsGKEExtendedDurationAffinity(t *testing.T) {
	key := "cloud.google.com/extended-duration-pods"
	required := func(exprs ...corev1.NodeSelectorRequirement) *corev1.Affinity {
		return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: exprs}},
			},
		}}
	}
	for _, tc := range []struct {
		name string
		a    *corev1.Affinity
		want bool
	}{
		{"nil affinity", nil, false},
		{"In [true], an earlier observed GKE shape", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}), true},
		{"In [2500m, X], this round's own live GKE shape, the pod's summed cpu beside an unexplained token", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"2500m", "X"}}), true},
		{"Exists, no values", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpExists}), true},
		{"NotIn [true] on the same key is a refusal, not a tolerance", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}}), false},
		{"DoesNotExist on the same key is a refusal, not a tolerance", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpDoesNotExist}), false},
		{"In with no values at all is not GKE's own shape", required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn}), false},
		{"a different key entirely", required(corev1.NodeSelectorRequirement{Key: "some.other/key", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}), false},
		{"a second term is not the one tolerated shape", &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}}},
				{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}}},
			}},
		}}, false},
		{"a pod affinity alongside is not the one tolerated shape", &corev1.Affinity{
			NodeAffinity: required(corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}).NodeAffinity,
			PodAffinity:  &corev1.PodAffinity{},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGKEExtendedDurationAffinity(tc.a); got != tc.want {
				t.Errorf("isGKEExtendedDurationAffinity(%+v) = %v, want %v", tc.a, got, tc.want)
			}
		})
	}
}

// TestGuaranteedPodAffinity covers guaranteedPod's own use of the matcher above: the
// extendedDurationAffinity parameter must gate on the matcher's real verdict, not merely on
// whether the pod carries any NodeAffinity at all.
func TestGuaranteedPodAffinity(t *testing.T) {
	extended := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: "cloud.google.com/extended-duration-pods", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}},
			}}},
		},
	}}
	refusal := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: "cloud.google.com/extended-duration-pods", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"true"}},
			}}},
		},
	}}
	quantity := resource.MustParse("1")
	podWith := func(affinity *corev1.Affinity) *corev1.Pod {
		return &corev1.Pod{
			Spec: corev1.PodSpec{
				Affinity: affinity,
				Containers: []corev1.Container{{Name: "only", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: quantity, corev1.ResourceMemory: quantity, corev1.ResourceEphemeralStorage: quantity},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: quantity, corev1.ResourceMemory: quantity, corev1.ResourceEphemeralStorage: quantity},
				}}},
			},
			Status: corev1.PodStatus{QOSClass: corev1.PodQOSGuaranteed},
		}
	}
	want := map[string]corev1.ResourceRequirements{"only": {
		Requests: corev1.ResourceList{corev1.ResourceCPU: quantity, corev1.ResourceMemory: quantity, corev1.ResourceEphemeralStorage: quantity},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: quantity, corev1.ResourceMemory: quantity, corev1.ResourceEphemeralStorage: quantity},
	}}
	for _, tc := range []struct {
		name                     string
		affinity                 *corev1.Affinity
		extendedDurationAffinity bool
		wantErr                  bool
	}{
		{"no affinity, flag false: fine", nil, false, false},
		{"no affinity, flag true: still fine", nil, true, false},
		{"the real shape, flag true: tolerated", extended, true, false},
		{"the real shape, flag false: refused — the probe's own call site passes false", extended, false, true},
		{"a refusal wearing the same key, flag true: still refused, the matcher's own job", refusal, true, true},
		{"some other affinity entirely, flag true: refused", &corev1.Affinity{PodAffinity: &corev1.PodAffinity{}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := guaranteedPod(podWith(tc.affinity), want, nil, resource.Quantity{}, false, tc.extendedDurationAffinity)
			if (err != nil) != tc.wantErr {
				t.Errorf("guaranteedPod(affinity=%+v, extendedDurationAffinity=%v) = %v, wantErr %v", tc.affinity, tc.extendedDurationAffinity, err, tc.wantErr)
			}
		})
	}
}
