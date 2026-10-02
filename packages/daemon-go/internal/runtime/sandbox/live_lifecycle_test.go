//go:build e2e

// The Stage 4a harness's lifecycle checks: every claim's launches, suspends, resumes, deaths,
// re-adoption, the orphan sweep, and the release that leaves the namespace as it was. The rig is
// live_test.go.

package sandbox

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

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

// treeAffinity reports whether the pod requires the node of another pod of its tree.
func treeAffinity(p *corev1.Pod, tree string) bool {
	if p.Spec.Affinity == nil || p.Spec.Affinity.PodAffinity == nil {
		return false
	}
	for _, term := range p.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		if term.TopologyKey == corev1.LabelHostname && term.LabelSelector != nil && term.LabelSelector.MatchLabels[labelTree] == tree {
			return true
		}
	}
	return false
}

// worker-colocated: another role of the same issue starts in the root's existing issue pod. Its
// process locator has the same pod UID but its own role container, and no init container runs again.
func (r *liveRig) checkWorkerColocated() error {
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

// suspend: stopping the worker's process leaves its issue Sandbox, pod, root role and tree PVC
// intact. The stopped locator probes Gone; the root remains Alive in its separate role container.
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
	pvc := TreeClaimName(root.token)
	phase, err := r.kubectl("get", "pvc", pvc, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return err
	}
	if phase != "Bound" {
		return fmt.Errorf("the tree PVC %s is %q", pvc, phase)
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
	if len(lines) != 2 || lines[0] != old.Incarnation || lines[1] != loc.Incarnation {
		return fmt.Errorf("the marker holds %v, want exactly [%s %s]", lines, old.Incarnation, loc.Incarnation)
	}
	return nil
}

// same-agent-negative: a resume naming a session the tree volume does not hold never starts a
// fresh agent. The role launcher reports the refusal; the shared pod stays available to peers.
func (r *liveRig) checkSameAgentNegative() error {
	second := r.claim("second")
	if err := r.ensureSuspended(second); err != nil {
		return err
	}
	if err := r.ensureRunning(r.claim("root")); err != nil {
		return err
	}
	absent := ompSessionsDir + "/absent-" + SandboxName(second.token) + ".marker"
	mark := r.obs.mark()
	loc, err := r.resume(second, absent)
	if err != nil {
		return err
	}
	note("runtime", "Resume naming %s returned %s", absent, short(loc.Incarnation))
	gone, ok := r.obs.await(mark, liveRunningLimit, func(o runtime.Observation) bool {
		return sameLocator(o.Locator, loc) && o.Kind != runtime.Alive && o.Kind != runtime.Uncertain
	})
	if !ok {
		return fmt.Errorf("no final observation of %s within %s", loc.Incarnation, liveRunningLimit)
	}
	want := "Refusing to start " + second.issue + " fresh"
	if gone.Kind != runtime.Gone || !strings.Contains(gone.Detail, want) || !strings.Contains(gone.Detail, "role container "+string(second.role)) {
		return fmt.Errorf("observed %s, want gone quoting the role container's refusal %q: %s", gone.Kind, want, gone.Detail)
	}
	note("runtime", "Observe: gone for %s — %s", short(loc.Incarnation), oneLine(gone.Detail))
	if regs := r.reg.registrations(second.token); len(regs) > 0 && regs[len(regs)-1].gen == second.gen {
		return errors.New("the refused incarnation registered a hello")
	}
	second.loc, second.state = nil, stateDead

	since := time.Now()
	fixed, err := r.resume(second, second.marker)
	if err != nil {
		return err
	}
	if _, err := r.awaitRunning(second, since); err != nil {
		return err
	}
	lines, err := r.markerLines(second)
	if err != nil {
		return err
	}
	note("operator", "resumed correctly as %s; exec cat %s: %v", short(fixed.Incarnation), second.marker, lines)
	if len(lines) != 2 || lines[1] != fixed.Incarnation || slices.Contains(lines, loc.Incarnation) {
		return fmt.Errorf("the marker holds %v: want two agents, the last %s, and never the refused %s", lines, fixed.Incarnation, loc.Incarnation)
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

// respawn-before-register: a claim suspended before its first hello spawns again over its
// existing Sandbox, as a new incarnation on a rotated Secret.
func (r *liveRig) checkRespawnBeforeRegister() error {
	fresh := r.claim("fresh")
	if err := r.ensureRunning(r.claim("root")); err != nil {
		return err
	}
	first, err := r.spawn(fresh, false)
	if err != nil {
		return err
	}
	firstHash := tokenHash(fresh.bootToken)
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
		return fmt.Errorf("the second Spawn over the existing Sandbox: %w", err)
	}
	reg, err := r.awaitRunning(fresh, since)
	if err != nil {
		return err
	}
	if second.Incarnation == first.Incarnation {
		return errors.New("the second Spawn returned the first incarnation")
	}
	out, err := r.kubectl("get", "secret", secretName(SandboxName(fresh.token)), "-o", "jsonpath={.data."+bootTokenKey+"}")
	if err != nil {
		return err
	}
	stored, err := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
	if err != nil {
		return err
	}
	storedHash := tokenHash(string(stored))
	switch {
	case storedHash == firstHash:
		return errors.New("the Secret still holds the first generation's boot token")
	case storedHash != tokenHash(fresh.bootToken):
		return errors.New("the Secret's boot token is neither generation's")
	case reg.hash != storedHash || reg.gen != 2:
		return fmt.Errorf("registered at generation %d with %s, want 2 with the Secret's %s", reg.gen, short(reg.hash), short(storedHash))
	}
	note("runtime", "second Spawn returned %s (new uid)", short(second.Incarnation))
	note("operator", "Secret %s: boot token sha256 %s… (generation 2's; generation 1's was %s…)", secretName(SandboxName(fresh.token)), storedHash[:12], firstHash[:12])
	note("harness", "hello registered at generation 2 with that token")
	return nil
}

// concurrent-provision: two claims of a new tree launched at once each provision their
// workspace, one after the other, from one clone, on a node the running tree has no pod on.
func (r *liveRig) checkConcurrentProvision() error {
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
	type window struct{ start, end time.Time }
	windows := map[string]window{}
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
		pod, err := r.getPod(name)
		if err != nil {
			return err
		}
		for _, s := range pod.Status.InitContainerStatuses {
			if s.Name == initContainer && s.State.Terminated != nil {
				windows[c.name] = window{s.State.Terminated.StartedAt.Time, s.State.Terminated.FinishedAt.Time}
			}
		}
		w := windows[c.name]
		note("runtime", "%s: init log %q; workspace-init ran %s → %s", c.name, want, w.start.UTC().Format(time.TimeOnly), w.end.UTC().Format(time.TimeOnly))
	}
	rootPod, err := r.getPod(SandboxName(root.token))
	if err != nil {
		return err
	}
	for _, c := range []*liveClaim{root2, child} {
		pod, err := r.getPod(SandboxName(c.token))
		if err != nil {
			return err
		}
		if pod.Spec.NodeName == rootPod.Spec.NodeName {
			return fmt.Errorf("tree %s's %s runs on node %s beside tree %s's root", c.tree, c.name, pod.Spec.NodeName, root.tree)
		}
		note("runtime", "tree %s's %s on node %s; tree %s's root on %s", c.tree, c.name, pod.Spec.NodeName, root.tree, rootPod.Spec.NodeName)
	}
	a, b := windows["root2"], windows["child2"]
	if a.start.IsZero() || b.start.IsZero() {
		return fmt.Errorf("an init container's terminated state is missing: %+v", windows)
	}
	if a.start.Before(b.end) && b.start.Before(a.end) {
		return fmt.Errorf("the two workspace-init runs overlapped: root2 %v–%v, child2 %v–%v", a.start, a.end, b.start, b.end)
	}
	note("runtime", "the two workspace-init runs did not overlap: the runtime serialized them")
	owner, repo := r.env.repo.Owner(), r.env.repo.Name()
	clone := TreeRoot + "/repos/github.com/" + owner + "/" + repo
	listing, err := r.exec(root2, "ls", "-A", filepath.Dir(clone))
	if err != nil {
		return err
	}
	entries := strings.Fields(listing)
	slices.Sort(entries)
	if !slices.Equal(entries, []string{repo, repo + ".lock"}) {
		return fmt.Errorf("%s holds %v, want one clone and its lock", filepath.Dir(clone), entries)
	}
	workspaces, err := r.exec(root2, "jj", "-R", clone, "workspace", "list")
	if err != nil {
		return err
	}
	for _, issue := range []string{root2.issue, child.issue} {
		if !strings.Contains(workspaces, strings.ToLower(issue)+":") {
			return fmt.Errorf("jj workspace list lacks %s: %s", strings.ToLower(issue), workspaces)
		}
	}
	fsck, err := r.exec(root2, "git", "--git-dir="+clone+"/.git", "fsck", "--connectivity-only", "--no-progress")
	if err != nil {
		return fmt.Errorf("git fsck of the clone: %w", err)
	}
	note("operator", "exec ls -A %s: %v", filepath.Dir(clone), entries)
	note("operator", "exec jj -R %s workspace list: %s", clone, oneLine(workspaces))
	note("operator", "exec git fsck --connectivity-only: ok %s", oneLine(fsck))
	return nil
}

// re-adopt: a fresh runtime and listener take over every live claim, with nothing relaunched; a
// pod killed while no runtime ran is reported Gone with its recorded incarnation.
func (r *liveRig) checkReAdopt() error {
	victim := r.claim("fresh")
	for _, name := range []string{"root", "worker", "fresh", "root2", "child2"} {
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
	if _, err := r.exec(victim, "sh", "-c", "kill 1"); err != nil {
		return err
	}
	if err := r.poll(liveGoneLimit, "pod "+name+" to end", func() (bool, error) {
		phase, err := r.kubectl("get", "pod", name, "-o", "jsonpath={.status.phase}")
		return phase == string(corev1.PodFailed) || phase == string(corev1.PodSucceeded), err
	}); err != nil {
		return err
	}
	note("operator", "exec %s -- sh -c 'kill 1' while no runtime ran; the pod ended", name)

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
	if !ok || gone.Kind != runtime.Gone || !sameLocator(gone.Locator, *victim.loc) {
		return fmt.Errorf("the killed claim was not reported gone with its recorded %s: %+v", victim.loc.Incarnation, gone)
	}
	note("runtime", "killed claim: gone, stamped %s — %s", short(gone.Locator.Incarnation), oneLine(firstLine(gone.Detail)))
	victim.loc, victim.state = nil, stateDead

	for _, c := range r.live() {
		loc := *c.loc
		alive, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool { return o.Locator.Claim == c.token })
		if !ok || alive.Kind != runtime.Alive || !sameLocator(alive.Locator, loc) {
			return fmt.Errorf("%s's first observation after re-adoption is %+v, want alive with its recorded %s", c.name, alive, loc.Incarnation)
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
		if uid != loc.Incarnation || s.Generation != generations[c.name] {
			return fmt.Errorf("%s was relaunched: pod uid %s (recorded %s), Sandbox generation %d → %d", c.name, uid, loc.Incarnation, generations[c.name], s.Generation)
		}
		note("runtime", "%s: alive with recorded %s; Sandbox generation %d unchanged", c.name, short(loc.Incarnation), s.Generation)
		note("harness", "%s: hello again at generation %d, token sha256 %s…", c.name, reg.gen, reg.hash[:12])
		note("operator", "%s: pod uid %s", c.name, short(uid))
	}
	for _, o := range r.obs.since(mark) {
		if o.Kind == runtime.NotRecordedProcess || (o.Kind == runtime.Gone && o.Locator.Claim != victim.token) {
			return fmt.Errorf("re-adoption observed %s for %s: %s", o.Kind, o.Locator.Claim, o.Detail)
		}
	}
	return nil
}

// orphan-sweep: a Sandbox of the project that no claim records survives a sweep inside the grace;
// past it, it survives while it is this runtime's own unreleased launch (a claim launched after
// the daemon read the claims it sweeps with), and is deleted once a fresh runtime, which never
// launched it, sweeps: what a crash between creating it and persisting its claim leaves. A
// suspended claim's Sandbox survives every sweep.
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
		return fmt.Errorf("the runtime's own unreleased launch %s did not survive a sweep past the grace: %v", name, err)
	}
	note("runtime", "sweep with grace 1s by the runtime that launched it: %s survives", name)
	r.stopRuntime()
	restarted := time.Now()
	if err := r.startRuntime(); err != nil {
		return err
	}
	note("runtime", "listener and runtime replaced, as a crash before the claim was persisted would")
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
	orphan.loc, orphan.state = nil, stateReleased
	for _, c := range r.live() {
		if _, err := r.awaitHelloAgain(c, restarted); err != nil {
			return err
		}
	}
	return nil
}

// release-preserves-issue: release ends only the named role process. The shared issue Sandbox,
// root process and tree PVC stay until the daemon's durable whole-issue cleanup effect runs; this
// direct runtime harness deliberately has no store/outbox and never substitutes a local cleanup.
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
	if rootObs, err := r.rt.Probe(r.ctx, *root.loc); err != nil || rootObs.Kind != runtime.Alive {
		return fmt.Errorf("root after Release(%s) is %s: %v", worker.name, rootObs.Kind, err)
	}
	pvc := TreeClaimName(root.token)
	phase, err := r.kubectl("get", "pvc", pvc, "-o", "jsonpath={.status.phase}")
	if err != nil || phase != "Bound" {
		return fmt.Errorf("tree PVC %s after Release(%s) is %q: %v", pvc, worker.name, phase, err)
	}
	note("runtime", "Release(%s) ended only that role; issue Sandbox %s and root %s stayed Alive", worker.name, name, root.token)
	note("operator", "PVC %s: Bound", pvc)
	return nil
}
