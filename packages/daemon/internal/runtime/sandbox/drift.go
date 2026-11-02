package sandbox

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A running issue pod whose configuration has drifted is not replaced where it is found: every
// role of the issue runs in that one pod, and ending it ends each of their processes mid-turn and
// deletes the disk they work on, with everything unpushed on it. The daemon opens an update round
// over such a pod instead — every worker claim in it is tasked to push its work and answer
// (api's claims/ready-for-update) — and replaces the pod only once each has answered. PodDrift is
// what the round is opened on, and ReplacePod what ends the round; the round itself, its task and
// its sweep are the daemon's (internal/daemon).
//
// A launch never replaces a live pod a round covers for one of these reasons (relaunch,
// roundCovers): it keeps the pod and starts its role in it, since the round, not the launch, is
// what ends the roles already running there. A pod no round covers has no such round to wait for,
// so its launch replaces it for every kind of drift, as every launch did before the round existed.

// roundCovers is whether an update round can be opened over a pod of kind: the one predicate that
// says who replaces such a pod when it has drifted, read by both sides of that answer. PodDrift
// answers nothing for a pod it excludes, so no round is ever opened over one; a launch into a live
// pod it excludes replaces that pod for every kind of drift (relaunch, movedInPod), where a launch
// into a pod it covers keeps the pod and leaves it to its round.
//
// A round covers every issue pod and nothing else. The project controller's pod is the one kind it
// excludes: the controller holds no issue workspace and has nothing to push, so there is nobody in
// it to task and no disk to protect, and a drift neither its launch nor a round replaced would
// leave it in the pod it was created in for ever — with no broker, so the token its slack_post is
// granted never reaches it, with none of the files role_config_files.controller names, and writing
// its session against the store that pod projects, while nothing said so.
func roundCovers(kind podKind) bool {
	_, controller := kind.(controllerPod)
	return !controller
}

// podShapeAnnotation records on a pod template's metadata the configuration the pod was built
// from (podShape). It is written where the template is built (podTemplate) and reaches the cluster
// with it, so every write of a template records the shape that template has, and no later sweep
// has to rebuild a launch to learn it: a launch's inputs are an issue's state and freshly minted
// GitHub App tokens, which a sweep over every pod of the fleet must not mint.
const podShapeAnnotation = "legion.dev/pod-shape"

// podShapeRecord is what podShapeAnnotation holds: the configuration a pod is built from that no
// running pod can be changed to carry, so a pod whose own differs from the runtime's now must be
// replaced to run as a pod created now does. Each field is the operator's or the deployment's own
// value, never the daemon's own derivation of it, which the daemon changes with its own releases:
// volumes and mounts are runtime.kubernetes.pod's alone (Options.Pod), and resources the pod
// kind's one reservation.
//
// The field order is fixed and encoding/json's own: Go sorts a map's keys, so a ResourceList
// encodes the same whatever order it was built in, and a resource.Quantity marshals as its own
// canonical string. Two equal configurations must never encode differently — every pod of the
// fleet would read as drifted at once — which TestPodShapeEncodesEqualConfigurationsIdentically
// holds this to.
type podShapeRecord struct {
	Image     string                      `json:"image"`
	Resources corev1.ResourceRequirements `json:"resources"`
	Volumes   []corev1.Volume             `json:"volumes,omitempty"`
	Mounts    []corev1.VolumeMount        `json:"mounts,omitempty"`
	Disk      string                      `json:"disk"`
}

// podShape is the configuration a pod of kind is built from now, as its template's annotation
// records it: the image every container runs, the kind's own reservation (podKind.resources), the
// operator's volumes and mounts, and the size of the pod's disk. An encoding failure is
// unreachable — every field is an API object the API server itself round-trips as JSON — and is
// logged and recorded as no shape at all, which reads as a pod built before this release: compared
// by image alone, never replaced for a reservation, a volume, a mount or a disk.
//
// Every field is settled when the runtime is built and never written again, so the runtime calls
// this once per kind, as it is built, and keeps what it answers (r.podShapes): the pod template
// that records a shape and the sweep that compares one read that string.
func podShape(r *Runtime, kind podKind) string {
	encoded, err := json.Marshal(podShapeRecord{
		Image:     r.image,
		Resources: kind.resources(r),
		Volumes:   r.pod.Volumes,
		Mounts:    r.pod.VolumeMounts,
		Disk:      r.volumeSize.String(),
	})
	if err != nil {
		r.log.Error("sandbox runtime: the pod shape cannot be encoded; pods will be compared by image alone", "error", err)
		return ""
	}
	return string(encoded)
}

// podShapeDrift names what the pod of the Sandbox read as s was built from that the runtime would
// build a pod from otherwise now (want, podShape), one short phrase per key: `image`, `resources`,
// `volumes`, `mounts` or `disk`, each with what the pod holds and what it would be handed now.
// Nil means nothing drifted.
//
// It reads the stored Sandbox's template, never the pod: GKE Autopilot rewrites a pod's
// requirements at admission, so a pod need not match the template it was made from, which is why
// the repository cache's own check reads the template too (templateResized).
//
// A template carrying no shape at all was written before this release, and is compared by image
// alone, exactly as imageDrift compares a pod: such a Sandbox never reads as having drifted
// resources, volumes or mounts, whatever its pod holds, since nothing recorded what it was built
// from and a guess would replace pods the operator never changed.
//
// Equality is semantic, not textual: a quantity spelled otherwise (1024Mi for 1Gi) is the same
// reservation and the same disk, and a configuration that encodes differently but means the same
// is no drift.
func podShapeDrift(s *sandbox, want string) []string {
	held := s.Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation]
	if held == want {
		return nil
	}
	var now podShapeRecord
	if err := json.Unmarshal([]byte(want), &now); err != nil {
		return nil
	}
	if held == "" {
		return templateImageDrift(s, now.Image)
	}
	var was podShapeRecord
	if err := json.Unmarshal([]byte(held), &was); err != nil {
		return []string{fmt.Sprintf("its recorded shape %q cannot be read: %v", held, err)}
	}
	var drifted []string
	named := func(key string, inPod, handed any) {
		if equality.Semantic.DeepEqual(inPod, handed) {
			return
		}
		drifted = append(drifted, key+" "+shapeValue(inPod)+", now "+shapeValue(handed))
	}
	if was.Image != now.Image {
		drifted = append(drifted, "image "+was.Image+", now "+now.Image)
	}
	named("resources", was.Resources, now.Resources)
	named("volumes", was.Volumes, now.Volumes)
	named("mounts", was.Mounts, now.Mounts)
	if !sameQuantity(was.Disk, now.Disk) {
		drifted = append(drifted, "disk "+was.Disk+", now "+now.Disk)
	}
	return drifted
}

// shapeValue is one part of a shape as a drift reason names it: its own JSON, which is how the
// annotation holds it, so a reason names exactly what differs. An unencodable value is
// unreachable, since the value was decoded from that same JSON.
func shapeValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// sameQuantity is whether two spellings of a quantity are the same amount, so a disk written
// 1024Mi and one written 1Gi are no drift. A spelling neither Kubernetes nor this daemon can parse
// is compared as text.
func sameQuantity(held, want string) bool {
	if held == want {
		return true
	}
	heldQuantity, heldErr := resource.ParseQuantity(held)
	wantQuantity, wantErr := resource.ParseQuantity(want)
	return heldErr == nil && wantErr == nil && heldQuantity.Cmp(wantQuantity) == 0
}

// templateImageDrift is how a Sandbox written before this release is compared: the first container
// of its stored template — an init container or the pod's own — running an image other than the
// one this runtime launches every container with, as one reason. A pod is built whole from one
// image, so one container's drift is the pod's.
func templateImageDrift(s *sandbox, image string) []string {
	if other, drifted := imageOtherThan(&s.Spec.PodTemplate.Spec, image); drifted {
		return []string{"image " + other + ", now " + image}
	}
	return nil
}

// shapeDrift is why the pod of the Sandbox read as s, a pod of kind, was built from a
// configuration other than the one a pod of that kind is built from now: podShapeDrift against the
// shape the runtime computed for that kind when it was built (r.podShapes).
//
// It is the one reading of a pod's shape, which both sides of a drifted pod's replacement take:
// the round names its reasons in the task it opens with (PodDrift), and a launch into a live pod
// no round covers replaces that pod when it answers anything (relaunch, roundCovers). So the two
// can never disagree about whether a pod's shape has moved, or about which kind's shape it is
// compared with.
func (r *Runtime) shapeDrift(s *sandbox, kind podKind) []string {
	return podShapeDrift(s, r.podShapes[kind])
}

// PodDrift satisfies runtime.Runtime: why the pod k's process runs in no longer matches the
// configuration a pod created now is built from, each reason a phrase an operator reads in a task
// and in a log line, nil when nothing drifted.
//
// It is the configuration a running pod cannot be changed to carry: what its template was built
// from (podShapeDrift — the image, the kind's reservation, the operator's volumes and mounts, and
// the disk), and the three facets of the pod itself whose worker can be asked to push first — the
// operator's volume and mount holding its role's overlays, the secrets broker's enrollment, and
// the session store (driftedInPod). The two facets whose worker cannot be reached at all stay a
// launch's business and are never a round's (movedInPod).
//
// It answers nothing for a claim with no process — such a claim records no pod, so no pod of it
// is gone — and nothing for a pod no round covers (roundCovers), today the project controller's:
// there is nobody in such a pod to task, so its drift is its launch's to replace rather than a
// round's.
//
// It answers runtime.ErrPodGone where the Sandbox or the pod is absent, where the pod running
// now is not the one the locator records, and where the Sandbox is suspended for anything but
// this round's own replacement: a pod that is not there, and a pod on its way out, need no round,
// and neither has drifted from nothing. That is the whole difference between a round that ends
// over the pod it was opened on and a round whose pod went with the work on its disk, which only
// the caller can tell its workers of (runtime.ErrPodGone, internal/daemon's groupDrift).
//
// The one suspended Sandbox that is not gone is the one this round's own ReplacePod suspended: it
// names the locator's own pod under the mark only that act writes (roundDisplacedAnnotation,
// suspendDisplacing) and it is waiting that pod out. Such a pod is answered for as a pod that is
// there and has not drifted — no reasons and no error — so a sweep that reads the Sandbox between
// the suspension and the pod's end ends the round as the replacement it is. Any other suspended
// Sandbox is some other act's: an operator's suspension, a tree moved on, or a launch replacing
// the pod, which names it as displaced too (displacedAnnotation) and takes its disk with whatever
// the workers have not pushed. Nothing the round decides over such a pod is true, and the round
// that was opened on it ends as a round whose pod went.
//
// The sweep asks this of every claim of the deployment at every tick, so the steady state — a
// Sandbox running and a pod built from the configuration the runtime still has — costs one read
// of the operating mode and one of the annotation the stored template records, against the shape
// the runtime holds for that kind (r.podShapes), all strings of the object this call already
// holds. The Sandbox itself is decoded only where the two shapes differ, which is the pod a round
// is about to be opened over.
func (r *Runtime) PodDrift(_ context.Context, k runtime.Known) ([]string, error) {
	if err := k.Validate(); err != nil {
		return nil, fmt.Errorf("sandbox runtime: pod drift: %w", err)
	}
	if k.Locator == nil {
		return nil, nil
	}
	if err := r.checkLocator(*k.Locator); err != nil {
		return nil, err
	}
	role, _ := k.Locator.Claim.Role()
	kind := podKindFor(role)
	if !roundCovers(kind) {
		return nil, nil
	}
	name := k.Locator.Sandbox.Name
	u, err := r.storedObject(name)
	if err != nil {
		return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, err)
	}
	if u == nil {
		return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, runtime.ErrPodGone)
	}
	pod := r.storedPod(name)
	if !ownedBy(pod, u.GetUID()) || string(pod.UID) != k.Locator.Sandbox.PodUID {
		return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, runtime.ErrPodGone)
	}
	if mode, _, _ := unstructured.NestedString(u.Object, "spec", "operatingMode"); mode == modeSuspended {
		round, _, _ := unstructured.NestedString(u.Object, "metadata", "annotations", roundDisplacedAnnotation)
		if round != k.Locator.Sandbox.PodUID {
			return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, runtime.ErrPodGone)
		}
		// This round's own replacement, between its suspension and the pod's end: the pod is
		// there, and what it was built from is what the replacement is about to build again.
		return nil, nil
	}
	var drifted []string
	if held, _, _ := unstructured.NestedString(u.Object, "spec", "podTemplate", "metadata", "annotations", podShapeAnnotation); held != r.podShapes[kind] {
		// Only a template whose recorded shape differs is decoded, which is the pod a round is
		// about to be opened over; shapeDrift reads that same recorded shape and answers nothing
		// where the two are equal, so the skipped decode hides no reason.
		s, err := decodeSandbox(u)
		if err != nil {
			return nil, fmt.Errorf("sandbox runtime: pod drift %s: %w", k.Claim, err)
		}
		drifted = r.shapeDrift(s, kind)
	}
	for _, moved := range r.driftedInPod(pod, role) {
		drifted = append(drifted, moved.String())
	}
	return drifted, nil
}

// ReplacePod satisfies runtime.Runtime: it ends the pod k's process runs in, once its update round
// is over, and waits until the cluster no longer has it. It is what a launch does to a live pod it
// replaces, and nothing more — under the pod's launch turn, the Suspended patch that names the
// replaced pod on the Sandbox in the same write (suspendDisplacing), then the wait for the pod's
// end — so every role still running there is reported ended by the daemon's own act (Displaced)
// and resumed uncharged on its session, as it is for a pod a launch replaces. That one patch
// names the pod under the round's own mark as well (roundDisplacedAnnotation, roundReplacement),
// which no launch writes: it is what a sweep reading the Sandbox before the pod ends tells this
// replacement by, where a launch's replacement of the same pod is a pod gone (PodDrift).
//
// It creates and provisions nothing: the first of those resumes finds the Sandbox Suspended and
// provisions a fresh pod from the current configuration, which is the launch path a replacement
// has always taken. It is a no-op when the Sandbox is absent or already suspended, or when the pod
// the locator records is no longer the Sandbox's own: there is then no pod of that round to end.
//
// due is asked last of all, under the pod's launch turn and one statement before the suspension,
// and its error is returned having written nothing. That is where it belongs rather than at the
// top: taking the launch turn waits out whatever launch holds it, which under a fleet-wide roll
// is a pod boot of minutes, and a round the caller found complete before that wait is exactly a
// round a worker can start a turn in during it. Asked here, the caller reads the claims one
// statement before the write that ends their processes, and nothing it refuses is ever cut.
func (r *Runtime) ReplacePod(ctx context.Context, k runtime.Known, due func(context.Context) error) error {
	if err := k.Validate(); err != nil {
		return fmt.Errorf("sandbox runtime: replace pod: %w", err)
	}
	if k.Locator == nil {
		return nil
	}
	if err := r.checkLocator(*k.Locator); err != nil {
		return err
	}
	name := k.Locator.Sandbox.Name
	release, err := r.lockPod(ctx, name)
	if err != nil {
		return fmt.Errorf("replace %s's pod: take its pod's launch turn: %w", k.Claim, err)
	}
	defer release()
	s, err := r.storedSandbox(name)
	if err != nil {
		return fmt.Errorf("replace %s's pod: %w", k.Claim, err)
	}
	if s == nil || s.mode() == modeSuspended {
		return nil
	}
	// The pod the Sandbox owns, as every other reader of the two together takes it (viewOf): a
	// same-named pod it does not own is some earlier Sandbox's, and no pod of this round.
	v := r.viewOf(name, s)
	if v.pod == nil || string(v.pod.UID) != k.Locator.Sandbox.PodUID {
		return nil
	}
	if err := due(ctx); err != nil {
		return err
	}
	if _, err := r.suspendDisplacing(ctx, s, v.pod.UID, roundReplacement); err != nil {
		return fmt.Errorf("replace %s's pod: suspend its sandbox: %w", k.Claim, err)
	}
	if _, err := r.awaitPodGone(ctx, s); err != nil {
		return fmt.Errorf("replace %s's pod: wait out its pod: %w", k.Claim, err)
	}
	r.log.Info("sandbox runtime: the pod of a finished update round is replaced", "sandbox", name, "pod", v.pod.UID, "claim", k.Claim)
	return nil
}

// podKindFor is the kind of pod a claim of role runs in, the one decision a launch makes from its
// claim (prepare) and a round makes again from the locator it holds: the project controller's pod
// for the controller's claim, and the issue's pod for every workflow role.
func podKindFor(role claim.Role) podKind {
	if role == claim.RoleController {
		return controllerPod{}
	}
	return issuePod{}
}
