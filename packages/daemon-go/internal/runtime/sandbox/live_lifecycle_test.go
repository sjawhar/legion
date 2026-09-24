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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	if string(pod.UID) != loc.Incarnation {
		return fmt.Errorf("pod %s is uid %s, the launch returned %s", name, pod.UID, loc.Incarnation)
	}
	note("runtime", "Sandbox %s Ready=True; pod uid %s equals the returned incarnation", name, pod.UID)
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

// worker-colocated: a worker spawned while the root runs requires, and gets, the root's node.
func (r *liveRig) checkWorkerColocated() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
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
	rootPod, err := r.getPod(SandboxName(root.token))
	if err != nil {
		return err
	}
	if !treeAffinity(pod, worker.tree) {
		return fmt.Errorf("the worker pod carries no required podAffinity on %s=%s with topology %s: %+v", labelTree, worker.tree, corev1.LabelHostname, pod.Spec.Affinity)
	}
	if pod.Spec.NodeName != rootPod.Spec.NodeName {
		return fmt.Errorf("the worker runs on %s, the root on %s", pod.Spec.NodeName, rootPod.Spec.NodeName)
	}
	note("runtime", "worker pod %s: required podAffinity %s=%s, topology %s; node %s, the root's", pod.Name, labelTree, worker.tree, corev1.LabelHostname, pod.Spec.NodeName)
	return nil
}

// suspend: the worker's pod goes and its Sandbox and the tree volume stay; the runtime reports
// the recorded process Gone when asked, and Observe says nothing more about it that the supervisor
// would act on.
func (r *liveRig) checkSuspend() error {
	root, worker := r.claim("root"), r.claim("worker")
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.ensureRunning(worker); err != nil {
		return err
	}
	loc := *worker.loc
	if err := r.rt.Suspend(r.ctx, loc); err != nil {
		return err
	}
	returned, returnedAt := r.obs.mark(), time.Now()
	if recorded, ok := r.rt.recorded(worker.token); ok {
		return fmt.Errorf("Suspend returned with the claim still in the watch, at %s", recorded.Incarnation)
	}
	note("runtime", "Suspend returned with the claim out of the watch")
	name := SandboxName(worker.token)
	s, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if s.mode() != modeSuspended {
		return fmt.Errorf("Sandbox %s is %s after Suspend returned", name, s.mode())
	}
	if err := r.awaitPodGone(worker); err != nil {
		return err
	}
	worker.loc, worker.state = nil, stateSuspended
	note("runtime", "Sandbox %s operatingMode Suspended; pod %s gone", name, name)
	pvc := TreeClaimName(root.token)
	phase, err := r.kubectl("get", "pvc", pvc, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return err
	}
	if phase != "Bound" {
		return fmt.Errorf("the tree PVC %s is %q", pvc, phase)
	}
	note("operator", "PVC %s: Bound", pvc)
	obs, err := r.rt.Probe(r.ctx, loc)
	if err != nil {
		return err
	}
	if obs.Kind != runtime.Gone || !sameLocator(obs.Locator, loc) {
		return fmt.Errorf("Probe(the recorded locator) answered %s for %s: %s", obs.Kind, obs.Locator.Incarnation, obs.Detail)
	}
	note("runtime", "Probe(recorded %s): gone — %s", short(loc.Incarnation), obs.Detail)
	time.Sleep(liveSettle)
	// Observe re-reads the recorded incarnation after each evaluation (an observation's At is
	// stamped as its evaluation ends), so one it delivers was evaluated before Suspend dropped the
	// entry. Anything evaluated after Suspend returned means Observe went on reporting the claim.
	late := 0
	for _, o := range r.obs.since(returned) {
		if o.Locator.Claim != worker.token {
			continue
		}
		if o.At.After(returnedAt) {
			return fmt.Errorf("Observe delivered %s for the suspended worker, evaluated at %s, after Suspend returned at %s: %s",
				o.Kind, o.At.Format(time.RFC3339Nano), returnedAt.Format(time.RFC3339Nano), o.Detail)
		}
		late++
	}
	note("runtime", "Observe after Suspend returned, over %s: nothing evaluated after it returned (%d delivered late, each evaluated before)", liveSettle, late)
	return nil
}

// no-affinity: with no pod of the tree scheduled, a worker and then the resumed root carry no
// affinity, schedule anywhere, and mount the tree volume.
func (r *liveRig) checkNoAffinity() error {
	root, second := r.claim("root"), r.claim("second")
	if err := r.ensureSuspended(r.claim("worker")); err != nil {
		return err
	}
	if err := r.ensureRunning(root); err != nil {
		return err
	}
	if err := r.suspend(root); err != nil {
		return err
	}
	note("runtime", "root suspended; its pod gone")
	pods, err := r.kube.CoreV1().Pods(r.env.namespace).List(r.ctx, metav1.ListOptions{LabelSelector: labelProject + "=" + r.env.project + "," + labelTree + "=" + root.tree})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName != "" && !terminal(&p) && p.DeletionTimestamp == nil {
			return fmt.Errorf("pod %s of the tree is still scheduled on %s", p.Name, p.Spec.NodeName)
		}
	}
	note("runtime", "no pod of tree %s is scheduled", root.tree)

	since := time.Now()
	if _, err := r.spawn(second, true); err != nil {
		return err
	}
	if _, err := r.awaitRunning(second, since); err != nil {
		return err
	}
	pod, err := r.getPod(SandboxName(second.token))
	if err != nil {
		return err
	}
	if treeAffinity(pod, second.tree) || (pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAffinity != nil) {
		return fmt.Errorf("the worker spawned with no tree pod scheduled carries an affinity: %+v", pod.Spec.Affinity)
	}
	claimName := ""
	for _, v := range pod.Spec.Volumes {
		if v.Name == treeVolume && v.PersistentVolumeClaim != nil {
			claimName = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claimName != TreeClaimName(root.token) {
		return fmt.Errorf("the worker mounts claim %q, not the tree's %s", claimName, TreeClaimName(root.token))
	}
	phase, err := r.kubectl("get", "pvc", claimName, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return err
	}
	note("runtime", "second worker pod %s: no affinity, node %s, Ready, mounts %s", pod.Name, pod.Spec.NodeName, claimName)
	note("operator", "PVC %s: %s", claimName, phase)
	if phase != "Bound" {
		return fmt.Errorf("the tree PVC is %q", phase)
	}
	if err := r.suspend(second); err != nil {
		return err
	}
	note("runtime", "second worker suspended; its pod gone")

	since = time.Now()
	if _, err := r.resume(root, root.marker); err != nil {
		return err
	}
	if _, err := r.awaitRunning(root, since); err != nil {
		return err
	}
	rootPod, err := r.getPod(SandboxName(root.token))
	if err != nil {
		return err
	}
	if rootPod.Spec.Affinity != nil && rootPod.Spec.Affinity.PodAffinity != nil {
		return fmt.Errorf("the resumed root carries an affinity although no other tree pod is scheduled: %+v", rootPod.Spec.Affinity)
	}
	note("runtime", "root resumed as %s: no affinity, node %s, Ready", short(string(rootPod.UID)), rootPod.Spec.NodeName)
	return nil
}

// resume: the first worker comes back as a new incarnation of the same agent, beside the root.
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
	if loc.Incarnation == old.Incarnation {
		return fmt.Errorf("the resumed incarnation %s is the old one", loc.Incarnation)
	}
	note("runtime", "Resume(prev %s) returned %s", short(old.Incarnation), short(loc.Incarnation))
	pod, err := r.getPod(SandboxName(worker.token))
	if err != nil {
		return err
	}
	if !treeAffinity(pod, worker.tree) {
		return fmt.Errorf("the resumed worker carries no tree affinity although the root runs: %+v", pod.Spec.Affinity)
	}
	note("runtime", "resumed pod requires the tree's node again (the root is Ready); node %s", pod.Spec.NodeName)
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

// same-agent-negative: a resume naming a session the volume does not hold never starts a fresh
// agent; the init container refuses, and the runtime reports it Gone with the refusal.
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
	if gone.Kind != runtime.Gone || !strings.Contains(gone.Detail, want) || !strings.Contains(gone.Detail, "init container "+initContainer) {
		return fmt.Errorf("observed %s, want gone quoting the init container's %q: %s", gone.Kind, want, gone.Detail)
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

// kill-pod: a worker killed in place is reported Gone with its exit, and Resume(prev=dead)
// relaunches it through Suspended.
func (r *liveRig) checkKillPod() error {
	worker := r.claim("worker")
	if err := r.ensureRunning(r.claim("root")); err != nil {
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
	note("operator", "exec %s -- sh -c 'kill 1'", name)
	gone, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == worker.token && o.Kind == runtime.Gone
	})
	if !ok {
		return fmt.Errorf("no gone for %s within %s", name, liveGoneLimit)
	}
	if !sameLocator(gone.Locator, old) || !strings.Contains(gone.Detail, string(old.Incarnation)) || !strings.Contains(gone.Detail, "main container "+mainContainer+" terminated") {
		return fmt.Errorf("the gone carries %s, want the old %s with the main container's exit: %s", gone.Locator.Incarnation, old.Incarnation, gone.Detail)
	}
	exit := regexp.MustCompile(`exit code (-?\d+)`).FindStringSubmatch(gone.Detail)
	if exit == nil {
		return fmt.Errorf("the gone names no exit code: %s", gone.Detail)
	}
	note("runtime", "Observe: gone for old %s, main container exit code %s — %s", short(old.Incarnation), exit[1], oneLine(firstLine(gone.Detail)))
	worker.loc, worker.state = nil, stateDead
	before, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if before.mode() != modeRunning {
		return fmt.Errorf("the dead pod's Sandbox is %s before the relaunch", before.mode())
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
	if after.Generation-before.Generation != 2 {
		return fmt.Errorf("the Sandbox went from generation %d to %d; a relaunch through Suspended is two writes", before.Generation, after.Generation)
	}
	note("runtime", "Resume(prev=dead %s) returned %s; Sandbox generation %d → %d (Suspended, then the template and Running), the dead pod gone", short(old.Incarnation), short(fresh.Incarnation), before.Generation, after.Generation)
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
		return errors.New("kill-pod did not run; stale-incarnation rides its relaunch")
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
	if s.mode() != modeRunning || string(pod.UID) != fresh.Incarnation || uid != fresh.Incarnation {
		return fmt.Errorf("Suspend(old) acted: Sandbox %s, pod uid %s (operator: %s), want Running and %s", s.mode(), pod.UID, uid, fresh.Incarnation)
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
// workspace, one after the other, from one clone.
func (r *liveRig) checkConcurrentProvision() error {
	if err := r.startRuntimeOnce(); err != nil {
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
	a, b := windows["root2"], windows["child2"]
	if a.start.IsZero() || b.start.IsZero() {
		return fmt.Errorf("an init container's terminated state is missing: %+v", windows)
	}
	if a.start.Before(b.end) && b.start.Before(a.end) {
		return fmt.Errorf("the two workspace-init runs overlapped: root2 %v–%v, child2 %v–%v", a.start, a.end, b.start, b.end)
	}
	note("runtime", "the two workspace-init runs did not overlap: the runtime serialized them")
	owner, repo, _ := strings.Cut(r.env.repo, "/")
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
		reg, ok := r.reg.await(c.token, c.gen, restarted, 2*time.Minute)
		if !ok || reg.hash != tokenHash(c.bootToken) {
			return fmt.Errorf("%s's shim did not say hello again with its generation-%d token", c.name, c.gen)
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

// orphan-sweep: a Sandbox of the project that no claim records survives a sweep inside the grace
// and is deleted by one past it; a suspended claim's Sandbox survives both.
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
	return nil
}

// release-tree: releasing every claim, a suspended one with no locator included, leaves nothing
// of the run: every Sandbox, every -boot Secret, and each tree volume go.
func (r *liveRig) checkReleaseTree() error {
	if err := r.startRuntimeOnce(); err != nil {
		return err
	}
	var released []string
	// The roster in reverse, so each tree's root, whose Sandbox owns the tree volume, goes last.
	for _, c := range slices.Backward(r.claims) {
		if c.state == stateNone || c.state == stateReleased {
			continue
		}
		loc := c.loc
		if err := r.rt.Release(r.ctx, runtime.Known{Claim: c.token, Locator: loc}); err != nil {
			return fmt.Errorf("Release(%s): %w", c.name, err)
		}
		how := "nil locator"
		if loc != nil {
			how = "locator " + short(loc.Incarnation)
		}
		released = append(released, c.name+" ("+how+")")
		c.loc, c.state = nil, stateReleased
	}
	note("runtime", "Released %s", strings.Join(released, ", "))
	selector := labelProject + "=" + r.env.project
	var left string
	err := r.poll(liveGoneLimit, "every object of the run to go", func() (bool, error) {
		out, err := r.kubectl("get", "sandboxes,secrets,pvc,pods", "-l", selector, "-o", "name")
		left = strings.TrimSpace(out)
		return left == "", err
	})
	if err != nil {
		return fmt.Errorf("%w; left: %s", err, oneLine(left))
	}
	note("operator", "kubectl get sandboxes,secrets,pvc,pods -l %s: none — every Sandbox, -boot Secret, and tree PVC (%s, %s) gone",
		selector, TreeClaimName(r.claim("root").token), TreeClaimName(r.claim("root2").token))
	return nil
}
