package sandbox

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	ompsessions "github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Three facets moved out of movedInPod into driftedInPod for this issue — the secrets broker's
// enrollment, a named role's overlays and the session store — so a launch no longer replaces a
// live pod that holds one, and an update round replaces it instead. PodDrift answers nothing for
// the project controller's pod, by design: that pod has no issue workspace and nothing to push,
// and drift.go says it "is replaced by its next launch as it always was".
//
// It is not. The launch replaces a live pod only for movedInPod, which these three left, so the
// controller's pod is replaced by neither: not by its launch, which keeps it, and not by a round,
// which is never opened over it. The controller then runs for ever in a pod built before the
// deployment changed — with no broker, so the token its `slack_post` is granted through
// agent-secrets never reaches it; with none of the overlays `role_config_files.controller` names;
// and writing its session to the store the old pod projects. Its observation is Alive and PodDrift
// is empty, so nothing anywhere says so.
//
// On main each of these is movedInPod, read StaleAddress, and the controller's next launch
// replaces its pod. A controller left behind by a configuration change must still be brought onto
// it: either its launch replaces the pod, as before, or PodDrift names the drift so something
// replaces it.
func TestTheControllersPodIsNotLeftBehindByAConfigurationItCannotCarry(t *testing.T) {
	const overlay = "/etc/legion-operator-roles/controller.yml"
	roles := corev1.Volume{Name: "operator-roles", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-roles"},
	}}}
	for name, now := range map[string]func(*Options){
		"the secrets broker's enrollment": func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
		},
		"the controller's own overlay": func(o *Options) {
			o.Pod.Volumes = append(o.Pod.Volumes, roles)
			o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: roles.Name, MountPath: "/etc/legion-operator-roles", ReadOnly: true})
			o.RoleConfigFiles = map[claim.Role][]string{claim.RoleController: {overlay}}
		},
		"the session store": func(o *Options) {
			o.SessionStore = ompsessions.Store{Kind: ompsessions.StorePostgres, DSNSecret: "legion/sessions/other"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil)
			spec := controllerSpec(t)
			loc := g.spawn(spec)
			r2 := secondRuntime(t, g, now)()
			g.connectRunning(controllerToken, loc.Sandbox.Generation)
			g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			held := r2.driftedInPod(g.pod(loc.Sandbox.Name), claim.RoleController)
			if len(held) == 0 {
				t.Fatalf("the controller's pod holds nothing of this change, so this case proves nothing")
			}
			obs := readopt(t, g.ctx, r2, loc)[0]
			drifted := driftOf(t, g.ctx, r2, loc)
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
			next, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume the controller: %v", err)
			}
			replaced := next.Sandbox.PodUID != loc.Sandbox.PodUID
			if replaced || len(drifted) > 0 {
				return
			}
			t.Errorf("the controller's pod holds %+v, which a pod created now is not handed, and nothing will replace it: its launch kept pod %s, PodDrift is empty and its observation is %s (%s)",
				held, loc.Sandbox.PodUID, obs.Kind, obs.Detail)
			if obs.Kind != runtime.StaleAddress {
				t.Logf("on main this same change reads StaleAddress and the launch replaces the pod")
			}
		})
	}
}

// The project controller's pod is the one pod no update round covers (roundCovers): PodDrift
// answers nothing for it, so every facet of driftedInPod reaches it through movedInPod's rule
// for an uncovered pod (addresses.go), and its next launch replaces it, as it always has. A
// served owner its gh volume projects no directory for is such a facet: the controller acts as
// the review App for every owner of the project, and one its pod holds no directory for is one
// its `gh` reaches as nobody. It has no issue workspace and nothing to push, so nothing is lost
// by replacing the pod where it stands — and it is replaced at its next launch, with no second
// registration anywhere.
func TestAControllerPodWhoseGhVolumeLacksAServedOwnerIsReplacedByItsNextLaunch(t *testing.T) {
	g := newRig(t, nil)
	spec := controllerSpec(t)
	loc := g.spawn(spec)
	ghVolumeName := roleVolume(ghVolume, claim.RoleController)
	r2 := secondRuntime(t, g, servingAddedOwner)()
	g.connectRunning(controllerToken, loc.Sandbox.Generation)
	g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	want := (movedAddress{ghVolumeName, ghDirsName(staticDirs), ghDirsName(servedDirs)}).String()
	held := r2.driftedInPod(g.pod(loc.Sandbox.Name), claim.RoleController)
	if !slices.ContainsFunc(held, func(m movedAddress) bool { return m.String() == want }) {
		t.Fatalf("the controller's pod holds %+v, want %q among it", held, want)
	}
	obs := readopt(t, g.ctx, r2, loc)[0]
	if obs.Kind != runtime.StaleAddress || !strings.Contains(obs.Detail, want) {
		t.Fatalf("%s (%s), want StaleAddress naming %q: no round is opened over the controller's pod, so its launch is what replaces it",
			obs.Kind, obs.Detail, want)
	}

	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
	next, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume the controller: %v", err)
	}
	if next.Sandbox.PodUID == loc.Sandbox.PodUID {
		t.Fatalf("the launch kept pod %s, whose gh volume names no directory for %s, and no round will ever replace it",
			loc.Sandbox.PodUID, addedOwner)
	}
	pod := g.pod(loc.Sandbox.Name)
	if held := ghItems(t, pod, claim.RoleController); !slices.Equal(held, ghItemsFor(servedDirs...)) {
		t.Errorf("the replaced controller pod's gh volume projects %+v, want the two files of each of %q", held, servedDirs)
	}
	if held := r2.driftedInPod(pod, claim.RoleController); len(held) > 0 {
		t.Fatalf("the replaced controller pod holds %+v, which a pod created now is not handed", held)
	}
}

// The controller of a project with no GitHub owner to mint for runs with gh acting as nobody and
// its gh volume an empty directory rather than a projection of its Secret
// (TestAControllerWithNoOwnerRunsWithAnEmptyGhDirectory, volumes: a projection with no items
// would project every key of the Secret, the launcher token among them). That state is ordinary
// and transient: a project whose mapping names no repository both Apps are installed on yet, and
// a daemon whose read of an installation GitHub did not answer, which leaves that repository out
// for now and reads it again.
//
// When the project then gains its first owner, that pod's /var/run/legion/gh holds no directory
// of that owner's name: the refresher's Update reaches the Secret and never the projection, so
// every `gh` call of the controller against that owner's repositories runs unauthenticated, and
// only a new pod projects the owner's files.
//
// So movedGhDirs reads a gh volume that is an empty directory as holding no directory: a served
// owner makes it a moved address, and because the controller's pod is the one pod no round
// covers and holds no issue disk, its next launch replaces it. Were the empty directory read as
// lacking nothing, driftedInPod would be empty, the observation Alive, and every launch would
// keep the pod for the life of the deployment. A pod with no gh volume at all is no pod of this
// runtime's making and stays unmoved, and the comparison stays one way: an owner the pod holds
// that the daemon no longer serves is a stale directory and no reason to remake a pod.
func TestAnOwnerlessControllerPodIsReplacedOnceItsProjectGainsAnOwner(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.ControllerCredential = noOwnerControllerCredential
		o.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string { return nil }
	}))
	spec := controllerSpec(t)
	loc := g.spawn(spec)
	ghVolumeName := roleVolume(ghVolume, claim.RoleController)
	at := slices.IndexFunc(g.pod(loc.Sandbox.Name).Spec.Volumes, func(v corev1.Volume) bool { return v.Name == ghVolumeName })
	if at < 0 || g.pod(loc.Sandbox.Name).Spec.Volumes[at].EmptyDir == nil {
		t.Fatalf("the ownerless controller's %s volume is %+v, want the empty directory its launch makes", ghVolumeName, g.pod(loc.Sandbox.Name).Spec.Volumes)
	}

	r2 := secondRuntime(t, g, func(o *Options) {
		o.ControllerCredential = staticControllerCredential
		o.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string { return staticDirs }
	})()
	g.connectRunning(controllerToken, loc.Sandbox.Generation)
	g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	want := (movedAddress{ghVolumeName, ghDirsName(nil), ghDirsName(staticDirs)}).String()
	held := r2.driftedInPod(g.pod(loc.Sandbox.Name), claim.RoleController)
	if !slices.ContainsFunc(held, func(m movedAddress) bool { return m.String() == want }) {
		t.Errorf("the controller's pod holds %+v, want %q among it", held, want)
	}
	obs := readopt(t, g.ctx, r2, loc)[0]
	if obs.Kind != runtime.StaleAddress || !strings.Contains(obs.Detail, want) {
		t.Errorf("%s (%s), want StaleAddress naming %q: no round is opened over the controller's pod, so its launch is what replaces it",
			obs.Kind, obs.Detail, want)
	}

	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
	next, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume the controller: %v", err)
	}
	if next.Sandbox.PodUID == loc.Sandbox.PodUID {
		t.Fatalf("the launch kept pod %s, whose gh directory is empty while the daemon serves %q, and no round will ever replace it",
			loc.Sandbox.PodUID, staticDirs)
	}
	pod := g.pod(loc.Sandbox.Name)
	if items := ghItems(t, pod, claim.RoleController); !slices.Equal(items, ghItemsFor(staticDirs...)) {
		t.Errorf("the replaced controller pod's gh volume projects %+v, want the two files of each of %q", items, staticDirs)
	}
	if drifted := r2.driftedInPod(pod, claim.RoleController); len(drifted) > 0 {
		t.Errorf("the replaced controller pod holds %+v, which a pod created now is not handed", drifted)
	}
}

// Reading an empty gh directory as the empty owner set costs nothing while the project it serves
// has no owner either: the comparison asks whether a served owner is missing, and no owner is
// served. Were it the other way round, every launch of such a deployment's controller would
// replace the pod it just made, for ever, since the replacement's gh directory is empty too.
func TestAnOwnerlessControllerIsKeptWhileItsProjectStaysOwnerless(t *testing.T) {
	ownerless := func(o *Options) {
		o.ControllerCredential = noOwnerControllerCredential
		o.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string { return nil }
	}
	g := newRig(t, nil, withOptions(ownerless))
	spec := controllerSpec(t)
	loc := g.spawn(spec)
	r2 := secondRuntime(t, g, ownerless)()
	g.connectRunning(controllerToken, loc.Sandbox.Generation)
	g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	if held := r2.driftedInPod(g.pod(loc.Sandbox.Name), claim.RoleController); len(held) > 0 {
		t.Fatalf("the controller's pod reads drift %+v while its project still serves no owner", held)
	}
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
	next, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume the controller: %v", err)
	}
	if next.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Errorf("the launch replaced pod %s with %s though nothing of its gh changed", loc.Sandbox.PodUID, next.Sandbox.PodUID)
	}
}

// The two pods movedGhDirs must leave alone whatever the daemon serves. A pod with no gh volume
// of the role at all is no pod of this runtime's making, and the runtime cannot say what it holds.
// A pod projecting an owner the daemon no longer serves is ahead of the daemon, not behind it: a
// stale directory costs nothing, and remaking the pod would delete the issue's disk for it.
func TestMovedGhDirsLeavesAPodItCannotJudgeAndOneAheadOfTheDaemon(t *testing.T) {
	g := newRig(t, nil)
	name := roleVolume(ghVolume, claim.RoleTester)
	for what, spec := range map[string]corev1.PodSpec{
		"no gh volume of this role at all": {Volumes: []corev1.Volume{{Name: "another-volume"}}},
		"an owner the daemon no longer serves": {Volumes: []corev1.Volume{{Name: name, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{Items: ghItemsFor(append([]string{"zeta"}, staticDirs...)...)},
		}}}},
	} {
		if moved, ok := g.r.movedGhDirs(&corev1.Pod{Spec: spec}, claim.RoleTester); ok {
			t.Errorf("%s: moved %q, want the pod left where it is", what, moved.String())
		}
	}
}

// A repository the mapping gains is no pod to replace; an owner it gains is. The directories a pod
// created now would hold depend on the pod's own repository — the owner it writes to holds one
// directory per other repository that owner serves — so movedGhDirs reads that repository off the
// pod's workspace fetch (fetchRepository) rather than guessing it, and asks for the owner-only set
// of a pod that fetches none, the controller's. It then compares owner directories alone: a pod
// whose own owner has since gained a sibling repository keeps running, reading that repository
// through its owner's directory until the issue's next pod projects a directory of its own for it,
// where a pod lacking an owner's directory holds no token of that owner at all.
func TestMovedGhDirsComparesTheOwnerDirectoriesOfThePodsOwnRepository(t *testing.T) {
	t.Run("a repository of an owner the pod already holds", func(t *testing.T) {
		var asked []string
		withSibling := func(repo ghrepo.Repository, _ []appauth.AppRole) []string {
			asked = append(asked, repo.String())
			if repo.IsZero() {
				return staticDirs
			}
			return append(slices.Clone(staticDirs), strings.ToLower(repo.Owner())+"/other")
		}
		g := newRig(t, nil, withOptions(func(o *Options) { o.GitHubDirs = withSibling }))
		spec := rootSpec(t)
		issue := g.spawn(spec)
		controller := g.spawn(controllerSpec(t))
		asked = nil

		if moved, ok := g.r.movedGhDirs(g.pod(issue.Sandbox.Name), claim.RoleArchitect); ok {
			t.Errorf("the issue pod moved %q, want it left where it is: the mapping gained a repository of an owner it holds, not an owner", moved.String())
		}
		if _, ok := g.r.movedGhDirs(g.pod(controller.Sandbox.Name), claim.RoleController); ok {
			t.Error("the controller's pod, which fetches no repository, moved though it holds every owner the daemon serves")
		}
		if want := []string{spec.Repository.String(), "/"}; !slices.Equal(asked, want) {
			t.Errorf("the drift check asked for %q, want the issue pod's own repository and the controller's zero one %q", asked, want)
		}
	})
	t.Run("an owner the pod holds no directory of", func(t *testing.T) {
		g := newRig(t, nil, withOptions(func(o *Options) {
			o.GitHubDirs = func(ghrepo.Repository, []appauth.AppRole) []string { return servedDirs }
		}))
		issue := g.spawn(rootSpec(t))
		moved, ok := g.r.movedGhDirs(g.pod(issue.Sandbox.Name), claim.RoleArchitect)
		if !ok || !strings.Contains(moved.String(), addedOwner) {
			t.Errorf("the issue pod moved = %t, %q; want the added owner's directory named as lacking", ok, moved.String())
		}
	})
}
