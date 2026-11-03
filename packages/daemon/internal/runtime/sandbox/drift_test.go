package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// driftOf is why loc's pod has drifted from the configuration r would build one from now
// (PodDrift). A runtime that cannot be asked fails the test: an error is never a statement that
// something drifted.
func driftOf(t *testing.T, ctx context.Context, r *Runtime, loc runtime.Locator) []string {
	t.Helper()
	drifted, err := r.PodDrift(ctx, runtime.Known{Claim: loc.Claim, Locator: &loc})
	if err != nil {
		t.Fatalf("PodDrift %s: %v", loc.Claim, err)
	}
	return drifted
}

// operatorVolume is an operator's own volume (runtime.kubernetes.pod.volumes), and operatorMount a
// mount of it: what an operator adds to every pod Legion runs, and two of the five keys a pod's
// recorded shape holds.
var (
	operatorVolume = corev1.Volume{Name: "operator-extra", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-extra"},
	}}}
	operatorMount = corev1.VolumeMount{Name: operatorVolume.Name, MountPath: "/etc/legion-operator-extra", ReadOnly: true}
)

// withOperatorVolume adds the operator's volume, and withOperatorMount its mount as well: a mount
// is only ever configured beside the volume it names.
func withOperatorVolume(o *Options) { o.Pod.Volumes = append(o.Pod.Volumes, operatorVolume) }

func withOperatorMount(o *Options) {
	withOperatorVolume(o)
	o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, operatorMount)
}

// movedReservation is the issue pod's reservation with another CPU request, the kind's own and
// nothing the daemon derives.
func movedReservation(o *Options) {
	resources := reservations()
	resources.Issue.Requests[corev1.ResourceCPU] = resource.MustParse("5")
	resources.Issue.Limits[corev1.ResourceCPU] = resource.MustParse("5")
	o.Resources = resources
}

// A pod's recorded shape is the configuration it was built from, in a fixed field order, each
// value as the operator wrote it: the image, the kind's own reservation, the operator's volumes and
// mounts, and the disk. It is pinned whole because it is what every later daemon compares a running
// pod against: a change of spelling here reads as drift on every pod of the fleet at once.
func TestPodShapeRecordsTheConfigurationAPodIsBuiltFrom(t *testing.T) {
	opts := testOptions()
	opts.Resources = PodResources{
		Issue: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("6Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi"),
				corev1.ResourceEphemeralStorage: resource.MustParse("80Gi"),
			},
		},
		Controller: reservations().Controller,
	}
	opts.IssueVolume = resource.MustParse("200Gi")
	opts.Pod = Pod{Volumes: []corev1.Volume{operatorVolume}, VolumeMounts: []corev1.VolumeMount{operatorMount}}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"image":"` + testImage + `",` +
		`"resources":{"limits":{"cpu":"2","ephemeral-storage":"80Gi","memory":"8Gi"},"requests":{"cpu":"2","ephemeral-storage":"6Gi","memory":"8Gi"}},` +
		`"volumes":[{"name":"operator-extra","configMap":{"name":"legion-operator-extra"}}],` +
		`"mounts":[{"name":"operator-extra","readOnly":true,"mountPath":"/etc/legion-operator-extra"}],` +
		`"disk":"200Gi"}`
	if got := podShape(r, issuePod{}); got != want {
		t.Errorf("podShape =\n%s\nwant\n%s", got, want)
	}
}

// Two configurations that mean the same must never encode differently, or every pod of the fleet
// reads as drifted the moment a daemon restarts: a reservation's keys written in another order
// encode the same, since Go sorts a map's keys; a quantity the operator spelled 8192Mi encodes as
// the 8Gi another deployment wrote, since a resource.Quantity carries its own canonical spelling;
// and one configuration encodes the same every time it is asked. Equal shapes are read as no
// drift, whichever spelling each side was written in.
func TestPodShapeEncodesEqualConfigurationsIdentically(t *testing.T) {
	spelled := func(cpu, memory, storage, disk string, keys ...corev1.ResourceName) *Runtime {
		t.Helper()
		opts := testOptions()
		requests, limits := corev1.ResourceList{}, corev1.ResourceList{}
		// The keys are set in the order the caller names them, which a map does not keep.
		for _, key := range keys {
			request, limit := cpu, cpu
			switch key {
			case corev1.ResourceMemory:
				request, limit = memory, memory
			case corev1.ResourceEphemeralStorage:
				request, limit = storage, storage
			}
			requests[key], limits[key] = resource.MustParse(request), resource.MustParse(limit)
		}
		opts.Resources = PodResources{
			Issue:      corev1.ResourceRequirements{Requests: requests, Limits: limits},
			Controller: reservations().Controller,
		}
		opts.IssueVolume = resource.MustParse(disk)
		opts.Pod = Pod{Volumes: []corev1.Volume{operatorVolume}, VolumeMounts: []corev1.VolumeMount{operatorMount}}
		r, err := configure(opts)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	keys := []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage}
	first := spelled("2", "8Gi", "6Gi", "200Gi", keys...)
	again := spelled("2", "8Gi", "6Gi", "200Gi", keys[2], keys[1], keys[0])
	if podShape(first, issuePod{}) != podShape(again, issuePod{}) {
		t.Fatalf("two spellings of one configuration encode differently:\n%s\n%s", podShape(first, issuePod{}), podShape(again, issuePod{}))
	}
	if got, repeated := podShape(first, issuePod{}), podShape(first, issuePod{}); got != repeated {
		t.Fatalf("one configuration encodes differently twice:\n%s\n%s", got, repeated)
	}

	// The same amounts, spelled otherwise: the quantities are canonical, so the text is the same
	// and the pod has drifted from nothing.
	otherwise := spelled("2", "8192Mi", "6144Mi", "204800Mi", keys...)
	if podShape(first, issuePod{}) != podShape(otherwise, issuePod{}) {
		t.Fatalf("the same amounts spelled otherwise encode differently:\n%s\n%s", podShape(first, issuePod{}), podShape(otherwise, issuePod{}))
	}
	built := &sandbox{Spec: sandboxSpec{PodTemplate: podTemplate{
		Metadata: podMetadata{Annotations: map[string]string{podShapeAnnotation: podShape(first, issuePod{})}},
	}}}
	if drifted := podShapeDrift(built, podShape(otherwise, issuePod{})); len(drifted) > 0 {
		t.Errorf("podShapeDrift = %q, want nothing: the same amounts spelled otherwise are the same configuration", drifted)
	}
}

// A pod a launch just created has drifted from nothing: the single highest-consequence failure of
// this rule is a false positive, which would open an update round over every pod of the fleet at
// once. It holds for a pod of either kind, and for an issue pod with the operator's own volumes and
// mounts on it.
func TestPodDriftIsNothingForAPodALaunchJustCreated(t *testing.T) {
	for name, options := range map[string][]rigOption{
		"a bare deployment":                 nil,
		"with the operator's volume":        {withOptions(withOperatorVolume)},
		"with the operator's volume mounts": {withOptions(withOperatorMount)},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil, options...)
			for _, spec := range []runtime.SpawnSpec{workerSpec(t), controllerSpec(t)} {
				loc := g.spawn(spec)
				if drifted := driftOf(t, g.ctx, g.r, loc); len(drifted) > 0 {
					t.Errorf("%s: PodDrift = %q, want nothing of a pod its own launch just created", spec.Claim, drifted)
				}
			}
		})
	}
}

// Each key of a pod's shape reads as drifted on its own, and the reason names that key: a daemon
// restarted onto another image, another reservation for the issue pod, an operator volume or mount
// added, or another disk size, over the pod the release before it left running. A deployment
// restarted on the configuration it already ran has drifted from nothing.
func TestPodDriftNamesEachConfigurationKeyThatMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		before, now func(*Options)
		key         string
	}{
		"the same configuration":   {func(*Options) {}, func(*Options) {}, ""},
		"another image":            {func(*Options) {}, func(o *Options) { o.Image = nextImage }, "image"},
		"another reservation":      {func(*Options) {}, movedReservation, "resources"},
		"an operator volume added": {func(*Options) {}, withOperatorVolume, "volumes"},
		"an operator mount added":  {withOperatorVolume, withOperatorMount, "mounts"},
		"another disk size":        {func(*Options) {}, func(o *Options) { o.IssueVolume = resource.MustParse("40Gi") }, "disk"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(tc.before))
			loc := g.spawn(workerSpec(t))
			r2 := secondRuntime(t, g, tc.now)()
			drifted := driftOf(t, g.ctx, r2, loc)
			switch {
			case tc.key == "" && len(drifted) > 0:
				t.Fatalf("PodDrift = %q, want nothing: the configuration did not move", drifted)
			case tc.key == "":
			case len(drifted) != 1 || !strings.HasPrefix(drifted[0], tc.key+" "):
				t.Fatalf("PodDrift = %q, want exactly one reason naming %s", drifted, tc.key)
			case !strings.Contains(drifted[0], ", now "):
				t.Fatalf("PodDrift = %q, want it to name what the pod holds and what it would be handed now", drifted)
			}
		})
	}
}

// A Sandbox whose stored template records no shape was written before this release, so there is
// nothing to say what its pod was built from: it is compared by image alone, exactly as a pod is.
// A moved reservation, operator volume or mount, or disk size on it is no drift — a guess there
// would replace pods no operator changed — and a moved image is.
func TestPodDriftComparesASandboxWithoutAPodShapeByImageAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		before, now func(*Options)
		drifted     bool
	}{
		"the same configuration":   {func(*Options) {}, func(*Options) {}, false},
		"another reservation":      {func(*Options) {}, movedReservation, false},
		"an operator volume added": {func(*Options) {}, withOperatorVolume, false},
		"an operator mount added":  {withOperatorVolume, withOperatorMount, false},
		"another disk size":        {func(*Options) {}, func(o *Options) { o.IssueVolume = resource.MustParse("40Gi") }, false},
		"another image":            {func(*Options) {}, func(o *Options) { o.Image = nextImage }, true},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(tc.before))
			loc := g.spawn(workerSpec(t))
			r2 := secondRuntime(t, g, tc.now)()
			forgetPodShape(t, g, r2, loc.Sandbox.Name)
			drifted := driftOf(t, g.ctx, r2, loc)
			if (len(drifted) > 0) != tc.drifted {
				t.Fatalf("PodDrift = %q, want drifted %t of a sandbox that records no shape", drifted, tc.drifted)
			}
			if tc.drifted && !strings.HasPrefix(drifted[0], "image ") {
				t.Fatalf("PodDrift = %q, want its one reason to name the image", drifted)
			}
		})
	}
}

// forgetPodShape drops the shape annotation from the stored Sandbox's pod template, leaving the
// Sandbox a release before this one wrote, and waits for the store the runtime reads to hold it.
func forgetPodShape(t *testing.T, g *rig, r *Runtime, name string) {
	t.Helper()
	s := g.sandbox(name)
	if s == nil {
		t.Fatalf("sandbox %s is absent", name)
	}
	annotations := map[string]string{}
	for key, value := range s.Spec.PodTemplate.Metadata.Annotations {
		if key != podShapeAnnotation {
			annotations[key] = value
		}
	}
	s.Spec.PodTemplate.Metadata.Annotations = annotations
	u, err := encodeSandbox(*s)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
		t.Fatal(err)
	}
	g.eventually("the sandbox store to see "+name+" without a recorded shape", func() bool {
		stored, err := r.storedSandbox(name)
		return err == nil && stored != nil && stored.Spec.PodTemplate.Metadata.Annotations[podShapeAnnotation] == ""
	})
}

// The three facets a round's workers can be asked about — the operator's volume and mount holding a
// named role's overlays, the secrets broker's enrollment, and the session store — are read by
// PodDrift, and no observation of the pod names one any more: a role whose pod holds one keeps
// running until it has answered its round, where a StaleAddress naming it would have the
// supervisor repoint it mid-turn. A deployment that enrolls with the broker only now moves no
// address of the running generation either: that generation runs in a pod carrying no enrollment,
// so it was handed no AGENT_SECRETS_URL and is handed none now (Runtime.enrollsIn,
// movedEnvironment). Each role of such a pod is Alive until its round replaces the pod.
func TestPodDriftReadsTheFacetsThatAreNoLongerStaleAddresses(t *testing.T) {
	const overlay = "/etc/legion-operator-extra/fallback.yml"
	for name, tc := range map[string]struct {
		now  func(*Options)
		says string
	}{
		"a role's overlays": {now: func(o *Options) {
			withOperatorMount(o)
			o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {overlay}}
		}, says: "role_config_files.tester " + overlay},
		"the broker's enrollment": {now: func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
		}, says: agentSecretsTokenVolume},
		"the session store": {now: func(o *Options) { o.SessionStore = cloudSQLStore }, says: "the session store"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(workerSpec(t))
			r2 := secondRuntime(t, g, tc.now)()
			g.connectRunning(workerToken, loc.Sandbox.Generation)
			g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			drifted := strings.Join(driftOf(t, g.ctx, r2, loc), "; ")
			if !strings.Contains(drifted, tc.says+" ") {
				t.Errorf("PodDrift = %q, want a reason naming %s", drifted, tc.says)
			}
			obs := readopt(t, g.ctx, r2, loc)[0]
			if strings.Contains(obs.Detail, tc.says) {
				t.Errorf("%s (%s) names %s, which is its round's to act on and no repoint's", obs.Kind, obs.Detail, tc.says)
			}
			if obs.Kind != runtime.Alive {
				t.Errorf("%s (%s), want the role alive: a drifted pod is its round's to replace, never a repoint's", obs.Kind, obs.Detail)
			}
		})
	}
}

// No round covers the project controller's pod (roundCovers), so its pod has drifted from nothing
// whatever the configuration says now: the controller holds no issue workspace and has nothing to
// push, so there is nobody in that pod to task and no round to open over it. What this answers
// nothing for, its next launch replaces instead, for every kind of drift and as it always did
// (TestALaunchReplacesAPodNoRoundCoversWhoseShapeHasDrifted,
// TestTheControllersPodIsNotLeftBehindByAConfigurationItCannotCarry): a pod neither covers would
// run on for ever against a configuration it cannot carry.
func TestPodDriftIsNothingForTheControllersPod(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(controllerSpec(t))
	r2 := secondRuntime(t, g, func(o *Options) {
		o.Image = nextImage
		o.IssueVolume = resource.MustParse("40Gi")
		withOperatorMount(o)
	})()
	if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) > 0 {
		t.Errorf("PodDrift = %q of the controller's pod, want nothing", drifted)
	}
}

// The pod the locator records not being there is said, and never read as a pod that has drifted
// from nothing: a Sandbox gone and a pod that is not the one the locator records each answer
// runtime.ErrPodGone, however far the configuration has moved. The daemon's round turns on the
// difference — a pod that drifted from nothing is still running, and the round open over it
// replaces it as it promised its workers it would, where a pod that is not there took whatever
// was unpushed on its disk with it, and the workers of its round must be told
// (internal/daemon's groupDrift).
//
// A claim running nowhere records no pod at all, so nothing of it is gone and nothing of it has
// drifted.
func TestPodDriftSaysThePodTheLocatorRecordsIsNotThere(t *testing.T) {
	moved := func(o *Options) { o.Image = nextImage }

	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	r2 := secondRuntime(t, g, moved)()
	if drifted, err := r2.PodDrift(g.ctx, runtime.Known{Claim: workerToken}); err != nil || len(drifted) > 0 {
		t.Errorf("PodDrift of a claim running nowhere = %q, %v; want nothing", drifted, err)
	}

	// Another pod of the same Sandbox: the one the locator records is gone.
	name := loc.Sandbox.Name
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	g.eventually("the controller to make another pod", func() bool {
		pod := r2.storedPod(name)
		return pod != nil && string(pod.UID) != loc.Sandbox.PodUID
	})
	if drifted, err := r2.PodDrift(g.ctx, runtime.Known{Claim: loc.Claim, Locator: &loc}); !errors.Is(err, runtime.ErrPodGone) || len(drifted) > 0 {
		t.Errorf("PodDrift of a locator's replaced pod = %q, %v; want runtime.ErrPodGone", drifted, err)
	}

	gone := newRig(t, nil)
	goneLoc := gone.spawn(workerSpec(t))
	r3 := secondRuntime(t, gone, moved)()
	if err := gone.dyn.Tracker().Delete(sandboxGVR, testNamespace, goneLoc.Sandbox.Name); err != nil {
		t.Fatal(err)
	}
	gone.eventually("the sandbox store to see it gone", func() bool {
		s, err := r3.storedSandbox(goneLoc.Sandbox.Name)
		return err == nil && s == nil
	})
	if drifted, err := r3.PodDrift(gone.ctx, runtime.Known{Claim: goneLoc.Claim, Locator: &goneLoc}); !errors.Is(err, runtime.ErrPodGone) || len(drifted) > 0 {
		t.Errorf("PodDrift of an absent sandbox = %q, %v; want runtime.ErrPodGone", drifted, err)
	}
}

// A pod under a suspended Sandbox is on its way out, and the round over it decides nothing true
// — unless the suspension is that round's own replacement. ReplacePod suspends the Sandbox and
// names the displaced pod on it under the round's own mark in one write (roundDisplacedAnnotation,
// suspendDisplacing) and then waits the pod out, so between those two the Sandbox is suspended,
// names this very pod for the round, and the pod is still there: that is the round's own
// replacement in flight, and it reads as a pod that is there and has not drifted, which is what
// ends the round as the replacement it is. The same Sandbox suspended by anything else — an
// operator, a tree moved on, a launch replacing the pod — carries no such mark, or one naming
// another pod, and reads runtime.ErrPodGone. A launch's replacement names the pod it displaced
// too (displacedAnnotation), and is read as a pod gone all the same: it is taking the pod's disk
// with it, which is the end the round's workers must be told of.
//
// The configuration has moved here, so a pod answered for at all answers the moved image: an
// empty answer is this reading and nothing else.
func TestPodDriftReadsASuspendedSandboxAsItsOwnReplacementOrAsAGonePod(t *testing.T) {
	g := newRig(t, nil)
	// The pod of a suspended Sandbox stays terminating, which is the window every reading below
	// is taken in: the suspension has landed and the pod has not ended.
	g.suspendDelay = time.Minute
	loc := g.spawn(workerSpec(t))
	name := loc.Sandbox.Name
	r2 := secondRuntime(t, g, func(o *Options) { o.Image = nextImage })()
	if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
		t.Fatal("PodDrift of the running pod is empty, so this case proves nothing: nothing below could be read as the drift that cleared")
	}

	suspended := func(what string, ok func(*sandbox) bool) {
		t.Helper()
		g.eventually("the sandbox store to see "+what, func() bool {
			s, err := r2.storedSandbox(name)
			return err == nil && s != nil && s.mode() == modeSuspended && ok(s)
		})
		if pod := g.pod(name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
			t.Fatalf("the pod of %s went while it was %s, so this case reads a pod that is gone rather than one on its way out", name, what)
		}
	}
	gone := func(what string) {
		t.Helper()
		if drifted, err := r2.PodDrift(g.ctx, runtime.Known{Claim: loc.Claim, Locator: &loc}); !errors.Is(err, runtime.ErrPodGone) || len(drifted) > 0 {
			t.Errorf("PodDrift of %s = %q, %v; want runtime.ErrPodGone", what, drifted, err)
		}
	}

	if err := g.r.setMode(g.ctx, g.sandbox(name), modeSuspended); err != nil {
		t.Fatalf("suspend the sandbox: %v", err)
	}
	suspended("it suspended naming no replaced pod", func(s *sandbox) bool { return s.Annotations[displacedAnnotation] == "" })
	gone("a suspended sandbox naming no replaced pod")

	if _, err := g.r.suspendDisplacing(g.ctx, g.sandbox(name), types.UID(loc.Sandbox.PodUID), launchReplacement); err != nil {
		t.Fatalf("name the locator's own pod as the one a launch replaced: %v", err)
	}
	suspended("a launch's replacement of the locator's own pod", func(s *sandbox) bool {
		_, forRound := s.Annotations[roundDisplacedAnnotation]
		return s.Annotations[displacedAnnotation] == loc.Sandbox.PodUID && !forRound
	})
	gone("a pod a launch's replacement suspended the sandbox over")

	if _, err := g.r.suspendDisplacing(g.ctx, g.sandbox(name), "uid-pod-earlier", roundReplacement); err != nil {
		t.Fatalf("name an earlier pod as the one the daemon replaced: %v", err)
	}
	suspended("it naming an earlier replaced pod", func(s *sandbox) bool {
		return s.Annotations[roundDisplacedAnnotation] == "uid-pod-earlier"
	})
	gone("a suspended sandbox naming another pod as the one its round replaced")

	if _, err := g.r.suspendDisplacing(g.ctx, g.sandbox(name), types.UID(loc.Sandbox.PodUID), roundReplacement); err != nil {
		t.Fatalf("name the locator's own pod as the one this round replaced: %v", err)
	}
	suspended("it naming the locator's own pod for its round", func(s *sandbox) bool {
		return s.Annotations[roundDisplacedAnnotation] == loc.Sandbox.PodUID
	})
	if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) > 0 {
		t.Errorf("PodDrift of this round's own replacement in flight = %q, want nothing: the pod is there and the round ends on it", drifted)
	}
}

// The two acts that replace a live pod leave the same Sandbox behind — suspended, naming that pod
// as the one the daemon displaced (displacedAnnotation), the pod still terminating — and a sweep
// that reads it between the suspension and the pod's end must tell them apart, because what it
// answers is what the round's workers are told. The round's own ReplacePod is a pod that is there
// and has not drifted, so the round ends as the replacement it is; a launch replacing the same pod
// is runtime.ErrPodGone, since that launch takes the pod's disk with whatever a worker has not
// pushed, and a round that ended as its own replacement would report nothing lost.
//
// Both are driven through the callers themselves, never by writing the annotations here: a future
// caller of suspendDisplacing that forgets which kind of replacement it is making fails this.
// The launch is a Resume into a pod whose launcher dials the stream the daemon has left, the kind
// of drift no round answers — its workers cannot be reached to be asked anything — and so the one
// that makes a launch replace an issue pod where it stands (movedInPod). The operator's volume
// moved in both cases, which is drift a round does answer: a pod read for at all answers it, so
// the empty answer the round's own replacement reads as is that reading and nothing else.
func TestPodDriftTellsTheRoundsOwnReplacementFromALaunchs(t *testing.T) {
	for name, tc := range map[string]struct {
		moved   func(*Options)
		replace func(*rig, *Runtime, runtime.Locator, runtime.SpawnSpec) error
		gone    bool
	}{
		"the round's own replacement in flight": {
			moved: withOperatorVolume,
			replace: func(g *rig, r *Runtime, loc runtime.Locator, _ runtime.SpawnSpec) error {
				return r.ReplacePod(g.ctx, runtime.Known{Claim: loc.Claim, Locator: &loc}, stillDue)
			},
		},
		"a launch replacing the same pod": {
			moved: func(o *Options) { withOperatorVolume(o); moveStream(o) },
			replace: func(g *rig, r *Runtime, loc runtime.Locator, spec runtime.SpawnSpec) error {
				spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-g2", resumeSession
				_, err := r.Resume(g.ctx, &loc, spec)
				return err
			},
			gone: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			// The pod stays terminating until this test ends it, which is the window the reading
			// is taken in: the suspension has landed and the pod has not ended.
			g.suspendDelay = time.Minute
			spec := workerSpec(t)
			loc := g.spawn(spec)
			sandboxName := loc.Sandbox.Name
			r2 := secondRuntime(t, g, tc.moved)()
			g.connectRunning(spec.Claim, loc.Sandbox.Generation)
			g.eventually("the role's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(spec.Claim, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) == 0 {
				t.Fatal("PodDrift of the running pod is empty, so this case proves nothing: the reading below could not be the drift that cleared")
			}

			replaced := make(chan error, 1)
			go func() { replaced <- tc.replace(g, r2, loc, spec) }()
			g.eventually("the replacement to suspend the sandbox naming this pod", func() bool {
				s, err := r2.storedSandbox(sandboxName)
				return err == nil && s != nil && s.mode() == modeSuspended && s.Annotations[displacedAnnotation] == loc.Sandbox.PodUID
			})
			if pod := g.pod(sandboxName); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
				t.Fatalf("the pod of %s went before the reading, so this case reads a pod that is gone rather than one on its way out", sandboxName)
			}
			drifted, err := r2.PodDrift(g.ctx, runtime.Known{Claim: loc.Claim, Locator: &loc})
			switch {
			case tc.gone && !errors.Is(err, runtime.ErrPodGone):
				t.Errorf("PodDrift of a pod a launch is replacing = %q, %v; want runtime.ErrPodGone: the launch takes its disk with whatever its workers have not pushed", drifted, err)
			case !tc.gone && (err != nil || len(drifted) > 0):
				t.Errorf("PodDrift of this round's own replacement in flight = %q, %v; want nothing and no error: the pod is there and the round ends on it", drifted, err)
			}

			// End the pod the replacement is waiting out, as the cluster does once it has
			// terminated, so the act under test finishes as it would there.
			if err := g.kube.Tracker().Delete(podsGVR, testNamespace, sandboxName); err != nil {
				t.Fatal(err)
			}
			if err := <-replaced; err != nil {
				t.Fatalf("the replacement: %v", err)
			}
		})
	}
}

// ReplacePod is what ends a pod whose update round is over: it names the pod on its Sandbox as the
// one the daemon replaced, and as the one this round replaced, suspends the Sandbox in that same
// write, and returns only once the cluster no longer has the pod. It creates nothing in its stead
// — the role that resumes next provisions a pod from the current configuration — and every role
// still running in the pod is reported ended by the daemon's own act, never dead.
func TestReplacePodNamesTheDisplacedPodAndWaitsItOut(t *testing.T) {
	g := newRig(t, nil)
	tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
	testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)
	name := testerLoc.Sandbox.Name

	if err := g.r.ReplacePod(g.ctx, runtime.Known{Claim: workerToken, Locator: &testerLoc}, stillDue); err != nil {
		t.Fatalf("ReplacePod: %v", err)
	}
	if named := g.sandbox(name).Annotations[displacedAnnotation]; named != testerLoc.Sandbox.PodUID {
		t.Fatalf("the Sandbox names %q as the pod the daemon replaced, want %s", named, testerLoc.Sandbox.PodUID)
	}
	if named := g.sandbox(name).Annotations[roundDisplacedAnnotation]; named != testerLoc.Sandbox.PodUID {
		t.Fatalf("the Sandbox names %q as the pod this round replaced, want %s: it is the mark that tells this replacement from a launch's (PodDrift)",
			named, testerLoc.Sandbox.PodUID)
	}
	if mode := g.sandbox(name).mode(); mode != modeSuspended {
		t.Fatalf("the Sandbox is %s after its pod was replaced, want it suspended", mode)
	}
	if pod := g.pod(name); pod != nil && string(pod.UID) == testerLoc.Sandbox.PodUID {
		t.Fatalf("ReplacePod returned with pod %s still in the cluster", pod.UID)
	}
	for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
		obs, err := g.r.Probe(g.ctx, loc)
		if err != nil {
			t.Fatal(err)
		}
		if obs.Kind != runtime.Gone && obs.Kind != runtime.NotRecordedProcess || !obs.Displaced {
			t.Fatalf("%s in the replaced pod: %s, Displaced %t (%s); want its process reported displaced",
				loc.Claim, obs.Kind, obs.Displaced, obs.Detail)
		}
	}

	// Nothing was created in its stead: the next launch provisions the new pod.
	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	resumed, err := g.r.Resume(g.ctx, &testerLoc, tester)
	if err != nil {
		t.Fatalf("Resume after the replacement: %v", err)
	}
	if resumed.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("the tester resumed into the replaced pod %s", testerLoc.Sandbox.PodUID)
	}
}

// The caller's last word is taken under the pod's launch turn and one statement before the
// suspension: the Sandbox is still as the caller left it when due is asked, and a due that
// refuses leaves the pod running and the Sandbox unwritten, with its own error handed back. That
// ordering is the whole point of asking here — the launch turn is where this call waits minutes,
// and a round that completed before the wait is a round a worker can start a turn in during it.
func TestReplacePodAsksWhetherItIsStillDueBeforeItSuspends(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	name := loc.Sandbox.Name
	known := runtime.Known{Claim: workerToken, Locator: &loc}

	refused := errors.New("a worker of the round started a turn")
	asked := 0
	if err := g.r.ReplacePod(g.ctx, known, func(context.Context) error {
		asked++
		if mode := g.sandbox(name).mode(); mode == modeSuspended {
			t.Errorf("the Sandbox is already %s when the replacement is asked whether it is due", mode)
		}
		return refused
	}); !errors.Is(err, refused) {
		t.Fatalf("ReplacePod of a replacement no longer due = %v, want %v", err, refused)
	}
	if asked != 1 {
		t.Fatalf("the replacement asked whether it was due %d times, want once", asked)
	}
	if mode := g.sandbox(name).mode(); mode == modeSuspended {
		t.Fatalf("the Sandbox is %s after a replacement that was no longer due", mode)
	}
	if named := g.sandbox(name).Annotations[displacedAnnotation]; named != "" {
		t.Fatalf("the Sandbox names %q as a pod the daemon replaced, want none", named)
	}
	if pod := g.pod(name); pod == nil || string(pod.UID) != loc.Sandbox.PodUID {
		t.Fatalf("the pod of a replacement that was no longer due is %+v, want %s still running", pod, loc.Sandbox.PodUID)
	}

	// A due that answers nil replaces the pod as every other caller's does.
	if err := g.r.ReplacePod(g.ctx, known, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("ReplacePod of a replacement still due: %v", err)
	}
	if mode := g.sandbox(name).mode(); mode != modeSuspended {
		t.Fatalf("the Sandbox is %s after a replacement that was still due, want it suspended", mode)
	}
}

// ReplacePod has nothing to end for a claim that runs nowhere, for a Sandbox that is absent or
// already suspended, or for a locator whose pod is no longer the Sandbox's own: each answers nil
// having written nothing, so a round whose pod went on its own finishes rather than failing.
func TestReplacePodIsANoOpWithoutThePodTheLocatorRecords(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	name := loc.Sandbox.Name
	if err := g.r.ReplacePod(g.ctx, runtime.Known{Claim: workerToken}, stillDue); err != nil {
		t.Fatalf("ReplacePod of a claim running nowhere: %v", err)
	}

	suspendSandbox(t, g, g.r, name)
	g.clearActions()
	if err := g.r.ReplacePod(g.ctx, runtime.Known{Claim: workerToken, Locator: &loc}, stillDue); err != nil {
		t.Fatalf("ReplacePod of a suspended sandbox: %v", err)
	}
	if wrote := g.writes(); len(wrote) > 0 {
		t.Fatalf("ReplacePod of a suspended sandbox wrote %v", wrote)
	}
	if named := g.sandbox(name).Annotations[displacedAnnotation]; named != "" {
		t.Fatalf("the suspended Sandbox names %q as a pod the daemon replaced, want none", named)
	}

	gone := newRig(t, nil)
	goneLoc := gone.spawn(workerSpec(t))
	if err := gone.dyn.Tracker().Delete(sandboxGVR, testNamespace, goneLoc.Sandbox.Name); err != nil {
		t.Fatal(err)
	}
	gone.eventually("the sandbox store to see it gone", func() bool {
		s, err := gone.r.storedSandbox(goneLoc.Sandbox.Name)
		return err == nil && s == nil
	})
	if err := gone.r.ReplacePod(gone.ctx, runtime.Known{Claim: workerToken, Locator: &goneLoc}, stillDue); err != nil {
		t.Fatalf("ReplacePod of an absent sandbox: %v", err)
	}
}

// The runtime answers neither question about a locator it did not mint: a tmux pane's, or one
// naming another namespace or another claim's Sandbox, is refused rather than acted on.
func TestPodDriftAndReplacePodRefuseALocatorTheRuntimeDidNotMint(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	elsewhere := loc
	sandboxOf := *loc.Sandbox
	sandboxOf.Namespace = "another-namespace"
	elsewhere.Sandbox = &sandboxOf
	known := runtime.Known{Claim: workerToken, Locator: &elsewhere}
	if _, err := g.r.PodDrift(g.ctx, known); err == nil {
		t.Error("PodDrift of a locator in another namespace was answered")
	}
	if err := g.r.ReplacePod(g.ctx, known, stillDue); err == nil {
		t.Error("ReplacePod of a locator in another namespace acted")
	}
}

// stillDue is the last word a test gives ReplacePod when its own subject is not that word: the
// replacement is due, so the runtime goes on. The parameter is required, since a replacement is
// the one call that deletes an issue's disk and every caller states what makes it still due.
func stillDue(context.Context) error { return nil }
