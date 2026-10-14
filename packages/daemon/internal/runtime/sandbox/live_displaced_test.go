//go:build e2e

// The Stage 4a harness's displacement check (LEGION-729): a launch that replaces an issue pod
// another role still runs in, under the Legion daemon's restricted identity and the cluster's
// admission policy for it. The rig is live_test.go.

package sandbox

import (
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// displaced: one issue pod runs the displaced claim (its tree's architect) when the operator
// deletes the launcher Secret of another role of the pod, the displacer (its implementer): the pod
// then no longer binds that role's launcher, so the displacer's Spawn replaces the pod while the
// architect still runs in it. The replacement's Suspended patch, one merge patch under the
// restricted identity, names the replaced pod on the Sandbox (displacedAnnotation); the
// architect's process ends with the pod and is observed Displaced at its recorded locator, the
// verdict the supervisor resumes uncharged (supervise's displaced); and the architect's Resume from
// its marker starts in the displacer's new pod. The fault is an operator's (the restricted
// identity may not delete a Secret), as kill-launcher's `kill 1` is; the replacement it causes is
// the runtime's alone.
func (r *liveRig) checkDisplaced() error {
	displaced, displacer := r.claim("displaced"), r.claim("displacer")
	if err := r.ensureRunning(displaced); err != nil {
		return err
	}
	name := SandboxName(displaced.token)
	old := *displaced.loc
	before, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if before.mode() != modeRunning {
		return fmt.Errorf("the issue pod Sandbox %s is %s before the fault, want Running", name, before.mode())
	}
	if named, ok := before.Annotations[displacedAnnotation]; ok {
		return fmt.Errorf("the Sandbox %s already names %s as a displaced pod before any replacement", name, named)
	}
	secret := roleSecretName(name, displacer.role)
	if _, err := r.kubectl("delete", "secret", secret); err != nil {
		return err
	}
	note("operator", "deleted Secret %s, the %s launcher's binding to pod %s, with %s running in that pod at %s",
		secret, displacer.role, short(old.Sandbox.PodUID), displaced.name, short(old.Incarnation))

	mark := r.obs.mark()
	since := time.Now()
	fresh, err := r.spawn(displacer, true)
	if err != nil {
		return fmt.Errorf("the %s's Spawn, which replaces the pod: %w", displacer.name, err)
	}
	if _, err := r.awaitRunning(displacer, since); err != nil {
		return err
	}
	if fresh.Sandbox.PodUID == old.Sandbox.PodUID {
		return fmt.Errorf("the %s's Spawn kept pod %s, whose %s launcher Secret is gone", displacer.name, old.Sandbox.PodUID, displacer.role)
	}
	after, err := r.getSandbox(name)
	if err != nil {
		return err
	}
	if named := after.Annotations[displacedAnnotation]; named != old.Sandbox.PodUID {
		return fmt.Errorf("the Sandbox %s names %q as the displaced pod, want the replaced %s", name, named, old.Sandbox.PodUID)
	}
	if string(after.UID) != string(before.UID) || after.mode() != modeRunning {
		return fmt.Errorf("the Sandbox after the replacement is uid %s, %s; want the same uid %s, Running", after.UID, after.mode(), before.UID)
	}
	note("runtime", "Spawn(%s) replaced pod %s with %s; Sandbox %s (same uid) annotated %s=%s by the Suspended merge patch",
		displacer.name, short(old.Sandbox.PodUID), short(fresh.Sandbox.PodUID), name, displacedAnnotation, after.Annotations[displacedAnnotation])

	ended, ok := r.obs.await(mark, liveGoneLimit, func(o runtime.Observation) bool {
		return o.Locator.Claim == displaced.token && (o.Kind == runtime.Gone || o.Kind == runtime.NotRecordedProcess)
	})
	if !ok {
		return fmt.Errorf("no end of %s's %s within %s", displaced.name, old.Incarnation, liveGoneLimit)
	}
	if !sameLocator(ended.Locator, old) || !ended.Displaced || ended.WorkspaceLost {
		return fmt.Errorf("%s's end is %s at %s, Displaced %t, WorkspaceLost %t: %s; want Displaced at its recorded %s",
			displaced.name, ended.Kind, ended.Locator.Incarnation, ended.Displaced, ended.WorkspaceLost, ended.Detail, old.Incarnation)
	}
	note("runtime", "Observe: %s for %s's recorded %s, Displaced — %s", ended.Kind, displaced.name, short(old.Incarnation), oneLine(firstLine(ended.Detail)))
	displaced.loc, displaced.state = nil, stateDead

	since = time.Now()
	resumed, err := r.resume(displaced, displaced.marker)
	if err != nil {
		return fmt.Errorf("the %s's Resume into the new pod: %w", displaced.name, err)
	}
	reg, err := r.awaitRunning(displaced, since)
	if err != nil {
		return err
	}
	if resumed.Sandbox.PodUID != fresh.Sandbox.PodUID || reg.hash != tokenHash(displaced.bootToken) {
		return fmt.Errorf("the %s resumed in pod %s registering %s, want the %s's new pod %s with generation %d's token",
			displaced.name, resumed.Sandbox.PodUID, short(reg.hash), displacer.name, fresh.Sandbox.PodUID, displaced.gen)
	}
	lines, err := r.markerLines(displaced)
	if err != nil {
		return err
	}
	if got := roleIncarnations(lines, displaced.role); !slices.Contains(got, old.Incarnation) || !slices.Contains(got, resumed.Incarnation) {
		return fmt.Errorf("the marker holds %s incarnations %v, want the replaced %s and the resumed %s", displaced.role, got, old.Incarnation, resumed.Incarnation)
	}
	if obs, err := r.rt.Probe(r.ctx, resumed); err != nil || obs.Kind != runtime.Alive {
		return fmt.Errorf("the resumed %s probes %s: %v", displaced.name, obs.Kind, err)
	}
	note("runtime", "Resume(%s, prev=%s) returned %s in the new pod %s; Alive", displaced.name, short(old.Incarnation), short(resumed.Incarnation), short(resumed.Sandbox.PodUID))
	note("harness", "%s: hello registered at generation %d with that generation's token; the marker holds its replaced and resumed incarnations", displaced.name, reg.gen)

	for _, c := range []*liveClaim{displacer, displaced} {
		if err := r.suspend(c); err != nil {
			return fmt.Errorf("suspend %s after the check: %w", c.name, err)
		}
	}
	pod, err := r.getPod(name)
	if err != nil {
		return err
	}
	if string(pod.UID) != fresh.Sandbox.PodUID || pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("the issue pod after both roles stopped is %s %s, want the new pod %s running", pod.UID, pod.Status.Phase, fresh.Sandbox.PodUID)
	}
	note("runtime", "%s and %s suspended; pod %s stays", displacer.name, displaced.name, short(fresh.Sandbox.PodUID))
	return nil
}
