//go:build e2e

// The Stage 4a harness's lifecycle checks: every claim's launches, suspends, resumes, deaths,
// re-adoption, the orphan sweep, and the release that leaves the namespace as it was. The rig is
// live_test.go.

package sandbox

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// root-ready: the root claim spawns, provisions its workspace, and registers at generation 1.
func (r *liveRig) checkRootReady() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
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
// memory with the request equal to the limit and equal to what the rig configured for the
// container's role (liveResources: some roles from liveOverrides, the rest from
// config.DefaultResources(), and the root pod carries both), so the pod is Guaranteed; carries the
// ephemeral-storage limit and request configured for it, the request under the limit (the tester's
// limit from liveOverrides, every other role's bound the default); and carries no affinity. The
// init containers take the reservation of the role whose launch created the pod
// (issuePod.initContainers): the root's own Spawn at root-ready, which no later launch of the
// issue replaces. Inside the pod, gVisor sizes the sandbox from the pod's cgroup, which the kubelet
// sets to the regular containers' summed limits once the init containers are done (the pod's
// effective request is max(the largest init container, the sum of the containers), and no init
// container's reservation exceeds the sum): nproc is max(2, ceil(Σ cpu limits)) and /proc/meminfo's
// MemTotal is within 3% of Σ memory limits. The per-container values are read from the API, since
// a gVisor pod exposes no cgroup files.
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
	want := liveResources()
	byContainer := make(map[string]corev1.ResourceRequirements, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		byContainer[c.Name] = want[claim.Role(c.Name)]
	}
	init := want[root.role]
	if err := guaranteedPod(pod, byContainer, &init); err != nil {
		return fmt.Errorf("pod %s: %w", name, err)
	}
	var cpuSum, memorySum resource.Quantity
	overrides, defaults := 0, 0
	for _, c := range pod.Spec.InitContainers {
		note("runtime", "init container %s: cpu %s, memory %s, the %s's reservation, request = limit; ephemeral-storage %s under a limit of %s", c.Name, c.Resources.Limits.Cpu().String(), c.Resources.Limits.Memory().String(), root.role, c.Resources.Requests.StorageEphemeral().String(), c.Resources.Limits.StorageEphemeral().String())
	}
	for _, c := range pod.Spec.Containers {
		role := claim.Role(c.Name)
		cpuSum.Add(*c.Resources.Limits.Cpu())
		memorySum.Add(*c.Resources.Limits.Memory())
		source := []string{}
		for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
			if overridden(role, resourceName) {
				overrides++
				source = append(source, string(resourceName)+" overridden")
			} else {
				defaults++
				source = append(source, string(resourceName)+" default")
			}
		}
		note("runtime", "container %s: cpu %s, memory %s, request = limit; ephemeral-storage %s under a limit of %s (%s)", c.Name, c.Resources.Limits.Cpu().String(), c.Resources.Limits.Memory().String(), c.Resources.Requests.StorageEphemeral().String(), c.Resources.Limits.StorageEphemeral().String(), strings.Join(source, ", "))
	}
	if overrides == 0 || defaults == 0 {
		return fmt.Errorf("the root pod's reservations come from %d overrides and %d defaults; the rig must configure both paths (liveOverrides)", overrides, defaults)
	}
	note("operator", "pod %s: qosClass %s, no affinity; %d reservation fields from liveOverrides and %d from config.DefaultResources()", name, pod.Status.QOSClass, overrides, defaults)

	wantCPUs := max(2, int(math.Ceil(cpuSum.AsApproximateFloat64())))
	nproc, err := r.exec(root, "nproc")
	if err != nil {
		return err
	}
	cpus, err := strconv.Atoi(strings.TrimSpace(nproc))
	if err != nil {
		return fmt.Errorf("nproc in the pod printed %q, not a number", nproc)
	}
	if cpus != wantCPUs {
		return fmt.Errorf("nproc in the pod is %d, want max(2, ceil(%s cpu)) = %d, the pod cgroup's quota as gVisor sizes the sandbox", cpus, cpuSum.String(), wantCPUs)
	}
	meminfo, err := r.exec(root, "sh", "-c", "grep '^MemTotal:' /proc/meminfo")
	if err != nil {
		return err
	}
	fields := strings.Fields(meminfo)
	if len(fields) != 3 || fields[2] != "kB" {
		return fmt.Errorf("/proc/meminfo's MemTotal line is %q, want `MemTotal: <n> kB`", meminfo)
	}
	memTotalKB, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return fmt.Errorf("/proc/meminfo's MemTotal %q is not a number", fields[1])
	}
	memTotal, wantMemory := float64(memTotalKB)*1024, memorySum.AsApproximateFloat64()
	if math.Abs(memTotal-wantMemory) > 0.03*wantMemory {
		return fmt.Errorf("MemTotal in the pod is %d kB (%.2f GiB), not within 3%% of the containers' summed memory limits %s, the pod cgroup's limit as gVisor sizes the sandbox", memTotalKB, memTotal/(1<<30), memorySum.String())
	}
	note("operator", "exec %s -- nproc: %d = max(2, ceil(%s)); MemTotal %d kB, within 3%% of the summed memory limits %s", name, cpus, cpuSum.String(), memTotalKB, memorySum.String())
	return nil
}

// guaranteedPod is the reservation rule every Legion pod is held to: it carries no affinity, its
// qosClass is Guaranteed, and every container — the init containers included — reserves cpu and
// memory with the request equal to the limit and equal to the reservation configured for it, and
// carries the ephemeral-storage request and limit configured for it, the request under the limit:
// want's entry for each regular container by name, and init for every init container (nil when the
// pod has none to hold). A container want does not name is a refusal: nothing a Legion pod runs is
// unreserved.
func guaranteedPod(pod *corev1.Pod, want map[string]corev1.ResourceRequirements, init *corev1.ResourceRequirements) error {
	if pod.Spec.Affinity != nil {
		return fmt.Errorf("carries an affinity, which no Legion pod asks for: %+v", pod.Spec.Affinity)
	}
	if pod.Status.QOSClass != corev1.PodQOSGuaranteed {
		return fmt.Errorf("is %q, want %s", pod.Status.QOSClass, corev1.PodQOSGuaranteed)
	}
	check := func(c corev1.Container, expected corev1.ResourceRequirements, source string) error {
		for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			request, limit := c.Resources.Requests[resourceName], c.Resources.Limits[resourceName]
			reserved := expected.Requests[resourceName]
			if request.IsZero() || limit.IsZero() {
				return fmt.Errorf("container %s reserves no %s: requests %v, limits %v", c.Name, resourceName, c.Resources.Requests, c.Resources.Limits)
			}
			if request.Cmp(limit) != 0 {
				return fmt.Errorf("container %s requests %s %s but is limited to %s; a reservation is one value as both", c.Name, resourceName, request.String(), limit.String())
			}
			if request.Cmp(reserved) != 0 {
				return fmt.Errorf("container %s reserves %s %s, not the %s configured for %s", c.Name, resourceName, request.String(), reserved.String(), source)
			}
		}
		request, limit := c.Resources.Requests[corev1.ResourceEphemeralStorage], c.Resources.Limits[corev1.ResourceEphemeralStorage]
		wantRequest, wantLimit := expected.Requests[corev1.ResourceEphemeralStorage], expected.Limits[corev1.ResourceEphemeralStorage]
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
// Its process locator has the same pod UID but its own role container, and no init container runs
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
	if worker.loc.Sandbox.Container != string(worker.role) || root.loc.Sandbox.Container != string(root.role) {
		return fmt.Errorf("worker/root role containers are %q/%q, want %q/%q", worker.loc.Sandbox.Container, root.loc.Sandbox.Container, worker.role, root.role)
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
	if second.loc.Sandbox.PodUID != root.loc.Sandbox.PodUID || second.loc.Sandbox.Container == root.loc.Sandbox.Container {
		return fmt.Errorf("second role locator %+v and root locator %+v are not separate containers of one issue pod", second.loc.Sandbox, root.loc.Sandbox)
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
	if !sameLocator(gone.Locator, old) || !strings.Contains(gone.Detail, string(old.Sandbox.PodUID)) || !strings.Contains(gone.Detail, "role container "+old.Sandbox.Container+" terminated") {
		return fmt.Errorf("the gone carries %s, want the old %s with the role container's exit: %s", gone.Locator.Incarnation, old.Incarnation, gone.Detail)
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
// alone, each clone passing git fsck. Nothing orders the two provisions: each works on its own
// volume. Its negative control is the other issue's workspace: the root's pod cannot list the
// child's workspace directory, which exists in the child's pod, and the child's pod cannot list the
// root's. Which node each pod lands on is not checked: no pod asks for or keeps off another pod's
// node (LEGION-632), and what a pod asks of its node is the `resources` check's.
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
		want := "workspace-init: " + dir.Dir + " on " + dir.Bookmark
		if !strings.Contains(log, want) {
			return fmt.Errorf("%s's init log lacks %q: %s", c.name, want, strings.TrimSpace(log))
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
		fsck, err := r.exec(c, "git", "--git-dir="+clone+"/.git", "fsck", "--connectivity-only", "--no-progress")
		if err != nil {
			return fmt.Errorf("git fsck of %s's clone: %w", c.name, err)
		}
		note("runtime", "%s: init log %q; Sandbox %s owns one claim template %s", c.name, want, name, issueVolume)
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
