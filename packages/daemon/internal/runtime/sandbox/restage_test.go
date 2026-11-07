package sandbox

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// movedStreamURL and movedDaemonURL stand in for the daemon's own addresses after it moved:
// distinct from testOptions().StreamURL and testOptions().DaemonURL, the addresses every role
// process is already handed when a rig spawns it.
const (
	movedStreamURL = "tcp://192.0.2.9:13371"
	movedDaemonURL = "http://192.0.2.9:13370"
)

// secondRuntime builds another Runtime over g's same fake cluster state, with testOptions edited by
// move (the addresses a restarted daemon hands its role processes), on g's own context and store:
// the fake controller started in newRig, which goes on reconciling whichever Runtime's patches
// land, is this package's stand-in for the one real Agent Sandbox controller that ever serves a
// cluster, whatever daemon address last wrote its Sandboxes. This is how a daemon's own move to a
// new address is staged: the claims the restarted daemon re-adopts are the very Sandboxes and pods
// the one at the old address left running. restarted makes it g's runtime, so the fake launchers of
// every pod created from then on connect to it, as real ones dial the daemon at the new address.
func secondRuntime(t *testing.T, g *rig, move func(*Options)) (restarted func() *Runtime) {
	t.Helper()
	opts := testOptions()
	opts.Conns = g.conns
	opts.Now = func() time.Time { return *g.now.Load() }
	if g.store != nil {
		opts.Store = g.store
	}
	move(&opts)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.start(g.ctx, g.clients); err != nil {
		t.Fatal(err)
	}
	return func() *Runtime {
		g.r = r
		return r
	}
}

// moveStream moves only the worker stream listener, the address every role launcher's --connect
// names.
func moveStream(o *Options) { o.StreamURL = movedStreamURL }

// readopt re-adopts the claims of locs at r's address and returns their first observations, in the
// order the runtime emits them.
func readopt(t *testing.T, ctx context.Context, r *Runtime, locs ...runtime.Locator) []runtime.Observation {
	t.Helper()
	var known []runtime.Known
	for _, loc := range locs {
		known = append(known, runtime.Known{Claim: loc.Claim, Locator: &loc})
	}
	if err := r.ReconcileOrphans(ctx, known, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := r.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen []runtime.Observation
	for range locs {
		seen = append(seen, next(t, observations))
	}
	return seen
}

// observationOf is the one observation of seen about loc's claim.
func observationOf(t *testing.T, seen []runtime.Observation, loc runtime.Locator) runtime.Observation {
	t.Helper()
	i := slices.IndexFunc(seen, func(obs runtime.Observation) bool { return obs.Locator.Claim == loc.Claim })
	if i < 0 {
		t.Fatalf("no observation of %s among %+v", loc.Claim, seen)
	}
	return seen[i]
}

// connectOf is the --connect value the launcher in the pod's container named container dials.
func connectOf(t *testing.T, pod *corev1.Pod, container string) string {
	t.Helper()
	command := containerNamed(t, pod.Spec, container).Command
	i := slices.Index(command, connectFlag)
	if i < 0 || i+1 >= len(command) {
		t.Fatalf("pod %s's %s container runs %q, with no %s value", pod.Name, container, command, connectFlag)
	}
	return command[i+1]
}

// lastStart is the last start command token's fake launcher received.
func lastStart(t *testing.T, g *rig, token claim.Token) shimwire.LauncherStart {
	t.Helper()
	frames := g.sent(token)
	for i := len(frames) - 1; i >= 0; i-- {
		if start, ok := frames[i].(shimwire.LauncherStart); ok {
			return start
		}
	}
	t.Fatalf("%s's launcher was sent no start command", token)
	return shimwire.LauncherStart{}
}

// startEnv is a start command's environment as a map.
func startEnv(start shimwire.LauncherStart) map[string]string {
	env := map[string]string{}
	for _, pair := range start.Env {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	return env
}

// An issue pod whose launcher dials the stream the daemon moved from is reported StaleAddress for
// each of its roles the moment the runtime at the new address is told of their claims
// (re-adoption, ReconcileOrphans): a launcher never redials elsewhere, so it would otherwise read as
// disconnected for ever. The first role the supervisor relaunches — a Resume over its recorded
// locator — replaces the issue pod, whose launcher dials the corrected stream, and resumes the
// role's saved session there; a sibling relaunched after it resumes into that same new pod
// (LEGION-592).
func TestAnIssuePodDialingAStaleStreamIsReplacedAndEveryRoleResumesIntoTheNewPod(t *testing.T) {
	g := newRig(t, nil)
	tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
	testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)
	if testerLoc.Sandbox.PodUID != reviewerLoc.Sandbox.PodUID {
		t.Fatalf("the tester and reviewer run in pods %s and %s, want their issue's one pod", testerLoc.Sandbox.PodUID, reviewerLoc.Sandbox.PodUID)
	}

	restarted := secondRuntime(t, g, moveStream)
	r2 := restarted()
	seen := readopt(t, g.ctx, r2, testerLoc, reviewerLoc)
	for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
		obs := observationOf(t, seen, loc)
		if obs.Kind != runtime.StaleAddress || obs.Locator != loc {
			t.Fatalf("%s: %s at %+v, want StaleAddress at the recorded locator: %s", loc.Claim, obs.Kind, obs.Locator, obs.Detail)
		}
		if want := (movedAddress{connectFlag, testOptions().StreamURL, movedStreamURL}).String(); !strings.Contains(obs.Detail, want) {
			t.Errorf("%s's detail %q lacks %q", loc.Claim, obs.Detail, want)
		}
		if strings.Contains(obs.Detail, "LEGION_DAEMON_URL") {
			t.Errorf("%s's detail %q names LEGION_DAEMON_URL, which did not move", loc.Claim, obs.Detail)
		}
	}

	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	newTester, err := r2.Resume(g.ctx, &testerLoc, tester)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if newTester.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("Resume kept the pod %s; the pod dialing the stale stream was not replaced", testerLoc.Sandbox.PodUID)
	}
	pod := g.pod(testerLoc.Sandbox.Name)
	if dialed := connectOf(t, pod, workerContainer); dialed != movedStreamURL {
		t.Errorf("the replaced pod's launcher dials %q, want the new stream %q", dialed, movedStreamURL)
	}
	if start := lastStart(t, g, workerToken); start.ResumeFile != resumeSession || start.Generation != 2 {
		t.Errorf("the tester started generation %d resuming %q, want generation 2 resuming its saved session", start.Generation, start.ResumeFile)
	}
	if obs, err := r2.Probe(g.ctx, newTester); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe the replaced pod's tester: %+v, %v; want Alive", obs, err)
	}

	reviewer.Generation, reviewer.BootToken, reviewer.ResumeSessionFile = 2, "boot-reviewer-g2", resumeSession
	newReviewer, err := r2.Resume(g.ctx, &reviewerLoc, reviewer)
	if err != nil {
		t.Fatalf("Resume the reviewer: %v", err)
	}
	if newReviewer.Sandbox.PodUID != newTester.Sandbox.PodUID {
		t.Fatalf("the reviewer resumed into pod %s, want the tester's new pod %s", newReviewer.Sandbox.PodUID, newTester.Sandbox.PodUID)
	}
	if obs, err := r2.Probe(g.ctx, newReviewer); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("Probe the reviewer in the new pod: %+v, %v; want Alive", obs, err)
	}
}

// A daemon whose API moved while its worker stream stayed put leaves every role it re-adopts
// dialling the stream it still listens on, and running a generation told a LEGION_DAEMON_URL that
// no longer reaches it, so the agent's every call to the daemon's API fails. The Sandbox's record of
// that generation says so: the role is reported StaleAddress, its detail naming the variable with
// the value the generation was started with and the one a new generation is handed, and the
// relaunch starts a new generation in the same pod, told the new URL (LEGION-592).
func TestARoleToldAStaleDaemonURLGetsANewGenerationInTheSamePod(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)

	restarted := secondRuntime(t, g, func(o *Options) { o.DaemonURL = movedDaemonURL })
	r2 := restarted()
	g.connectRunning(workerToken, loc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	obs := readopt(t, g.ctx, r2, loc)[0]
	if obs.Kind != runtime.StaleAddress || obs.Locator != loc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
	if want := (movedAddress{"LEGION_DAEMON_URL", testOptions().DaemonURL, movedDaemonURL}).String(); !strings.Contains(obs.Detail, want) {
		t.Errorf("detail %q lacks %q", obs.Detail, want)
	}
	if strings.Contains(obs.Detail, connectFlag) {
		t.Errorf("detail %q names %s, which did not move", obs.Detail, connectFlag)
	}

	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	newLoc, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if newLoc.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod %s with %s; only the generation's environment moved", loc.Sandbox.PodUID, newLoc.Sandbox.PodUID)
	}
	start := lastStart(t, g, workerToken)
	if got := startEnv(start)["LEGION_DAEMON_URL"]; start.Generation != 2 || got != movedDaemonURL {
		t.Fatalf("the new generation %d is told LEGION_DAEMON_URL %q, want generation 2 told %q", start.Generation, got, movedDaemonURL)
	}
	g.eventually("the record of generation 2 to reach the store", func() bool {
		obs, err := r2.Probe(g.ctx, newLoc)
		return err == nil && obs.Kind == runtime.Alive
	})
}

// Enrolling a running deployment with the secrets broker (runtime.kubernetes.agent_secrets) leaves
// every issue pod without the broker's token projection and its roles' key directories, which a
// generation of a role that enrolls is started against: the shim's agent-secrets flags name both,
// and the shim refuses to start without its token file. The pod has drifted, so PodDrift names the
// projection the pod lacks and the daemon opens an update round over it; no observation of the
// role names that projection any more, and its relaunch keeps the pod, since ending it under the
// roles working in it is the thing the round exists to avoid. The running generation's own
// AGENT_SECRETS_URL moves with nothing: it runs in a pod that carries no enrollment, so it was
// handed no broker URL and is handed none now (Runtime.enrollsIn), and its role is Alive. Once
// the round has ended the pod (ReplacePod), the next launch makes one mounting both for every
// role, and a sibling resumes into that same new pod.
func TestEnrollingWithTheSecretsBrokerDriftsAnIssuePodMadeWithoutIt(t *testing.T) {
	g := newRig(t, nil)
	tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
	testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)

	broker := &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r2 := secondRuntime(t, g, func(o *Options) { o.AgentSecrets = broker })()
	g.connectRunning(workerToken, testerLoc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, testerLoc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	obs := readopt(t, g.ctx, r2, testerLoc)[0]
	if strings.Contains(obs.Detail, agentSecretsTokenVolume) {
		t.Fatalf("%s at %+v names the pod's token projection (%s), which is its round's to act on, not a repoint's",
			obs.Kind, obs.Locator, obs.Detail)
	}
	if obs.Kind != runtime.Alive {
		t.Fatalf("%s (%s), want the tester alive: its generation runs in a pod that carries no enrollment, so it holds no broker URL to move",
			obs.Kind, obs.Detail)
	}
	want := (movedAddress{agentSecretsTokenVolume, "(unset)", enrollmentName(brokerTokenProjection(broker))}).String()
	if drifted := driftOf(t, g.ctx, r2, testerLoc); !slices.Contains(drifted, want) {
		t.Errorf("PodDrift = %q, want %q among it", drifted, want)
	}

	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	kept, err := r2.Resume(g.ctx, &testerLoc, tester)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if kept.Sandbox.PodUID != testerLoc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod %s, which the tester's own update round replaces", testerLoc.Sandbox.PodUID)
	}

	if err := r2.ReplacePod(g.ctx, runtime.Known{Claim: workerToken, Locator: &kept}, stillDue); err != nil {
		t.Fatalf("ReplacePod: %v", err)
	}
	tester.Generation, tester.BootToken = 3, "boot-tester-g3"
	newTester, err := r2.Resume(g.ctx, &kept, tester)
	if err != nil {
		t.Fatalf("Resume the tester after its round: %v", err)
	}
	if newTester.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("the round kept the pod %s, which has no agent-secrets volume for the enrolled tester's shim", testerLoc.Sandbox.PodUID)
	}
	pod := g.pod(testerLoc.Sandbox.Name)
	if held := heldInPod(r2, pod, claim.Roles); len(held) > 0 {
		t.Fatalf("the replaced pod holds %+v, which a pod created now is not handed", held)
	}
	if drifted := driftOf(t, g.ctx, r2, newTester); len(drifted) > 0 {
		t.Fatalf("the replaced pod has drifted: %q", drifted)
	}
	mounted := slices.ContainsFunc(containerNamed(t, pod.Spec, workerContainer).VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == agentSecretsTokenVolume && m.MountPath == AgentSecretsTokenDir
	})
	if !mounted {
		t.Errorf("the replaced pod's launcher does not mount the broker's token at %s", AgentSecretsTokenDir)
	}
	if start := lastStart(t, g, workerToken); start.Generation != 3 || !slices.Contains(start.Argv, AgentSecretsTokenDir+"/"+AgentSecretsTokenFile) {
		t.Errorf("the tester started generation %d with %q, want generation 3 pointed at the pod's token file", start.Generation, start.Argv)
	}

	reviewer.Generation, reviewer.BootToken, reviewer.ResumeSessionFile = 2, "boot-reviewer-g2", resumeSession
	newReviewer, err := r2.Resume(g.ctx, &reviewerLoc, reviewer)
	if err != nil {
		t.Fatalf("Resume the reviewer: %v", err)
	}
	if newReviewer.Sandbox.PodUID != newTester.Sandbox.PodUID {
		t.Fatalf("the reviewer resumed into pod %s, want the tester's new pod %s", newReviewer.Sandbox.PodUID, newTester.Sandbox.PodUID)
	}
}

// ghItems are the items role's gh volume of the pod projects: the Secret keys of the owners the
// pod was created under, which are fixed for its life (manifest's volumes). A pod of this
// runtime's making holding no such projection fails the test, there being nothing to read.
func ghItems(t *testing.T, pod *corev1.Pod, role claim.Role) []corev1.KeyToPath {
	t.Helper()
	name := roleVolume(ghVolume, role)
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != name {
			continue
		}
		if volume.Secret == nil {
			t.Fatalf("the pod's %s volume is %+v, want its role Secret's gh keys", name, volume.VolumeSource)
		}
		return volume.Secret.Items
	}
	t.Fatalf("the pod has no %s volume", name)
	return nil
}

// ghItemsFor is what a gh volume projects for owners, in the order a launch of them builds it:
// each owner's hosts.yml and config.yml under a directory of its own name.
func ghItemsFor(owners ...string) []corev1.KeyToPath {
	items := make([]corev1.KeyToPath, 0, 2*len(owners))
	for _, owner := range owners {
		items = append(items,
			corev1.KeyToPath{Key: GitHubHostsKey(owner), Path: owner + "/" + ghconfig.HostsFile},
			corev1.KeyToPath{Key: GitHubConfigKey(owner), Path: owner + "/" + ghconfig.ConfigFile})
	}
	return items
}

// A project that gains a GitHub owner — a repository of a new owner in its mapping, which the
// restarted daemon serves (appauth.GitHubDirs) and renders a directory for in every role's
// bundle — leaves every issue pod created before it projecting the owners of its own creation:
// the refresher adds the new owner's two keys to the live role Secret, but a pod's projection
// names the keys it was built with, so /var/run/legion/gh holds no directory of that owner and
// every gh call of its roles against that owner's repositories runs as nobody. The pod has
// drifted, not moved: its workers are reachable, so PodDrift names the gh volume with the owners
// it holds and the owners served now, the daemon opens an update round, and the relaunch keeps
// the pod — replacing it under the roles working in it would delete the issue's disk with
// everything unpushed on it. Once the round has ended the pod (ReplacePod), the next launch makes
// one projecting the new owner's files too, and that pod has drifted from nothing.
func TestAddingAGitHubOwnerDriftsAnIssuePodWhoseGhVolumeLacksIt(t *testing.T) {
	g := newRig(t, nil)
	tester := workerSpec(t)
	testerLoc := g.spawn(tester)
	ghVolumeName := roleVolume(ghVolume, claim.RoleTester)
	if held := ghItems(t, g.pod(testerLoc.Sandbox.Name), claim.RoleTester); !slices.Equal(held, ghItemsFor(staticDirs...)) {
		t.Fatalf("the tester's gh volume projects %+v, want the two files of each of %q and nothing else", held, staticDirs)
	}

	r2 := secondRuntime(t, g, servingAddedOwner)()
	g.connectRunning(workerToken, testerLoc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, testerLoc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	obs := readopt(t, g.ctx, r2, testerLoc)[0]
	if obs.Kind != runtime.Alive || obs.Locator != testerLoc {
		t.Fatalf("%s at %+v, want the tester alive at its recorded locator until its round ends: %s", obs.Kind, obs.Locator, obs.Detail)
	}
	want := (movedAddress{ghVolumeName, ghDirsName(staticDirs), ghDirsName(servedDirs)}).String()
	drifted := driftOf(t, g.ctx, r2, testerLoc)
	if !slices.Contains(drifted, want) {
		t.Errorf("PodDrift = %q, want %q among it", drifted, want)
	}
	named := 0
	for _, reason := range drifted {
		if strings.Contains(reason, ghVolumeName) {
			named++
		}
	}
	if named != 1 {
		t.Errorf("PodDrift = %q, want exactly one reason naming %s: the gh directory is one volume, whatever it lacks", drifted, ghVolumeName)
	}

	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	kept, err := r2.Resume(g.ctx, &testerLoc, tester)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if kept.Sandbox.PodUID != testerLoc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod %s, which the tester's own update round replaces: the launch would delete the issue's disk", testerLoc.Sandbox.PodUID)
	}

	if err := r2.ReplacePod(g.ctx, runtime.Known{Claim: workerToken, Locator: &kept}, stillDue); err != nil {
		t.Fatalf("ReplacePod: %v", err)
	}
	tester.Generation, tester.BootToken = 3, "boot-tester-g3"
	newTester, err := r2.Resume(g.ctx, &kept, tester)
	if err != nil {
		t.Fatalf("Resume the tester after its round: %v", err)
	}
	if newTester.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("the round kept the pod %s, whose gh volume names no directory for %s", testerLoc.Sandbox.PodUID, addedOwner)
	}
	pod := g.pod(testerLoc.Sandbox.Name)
	held := ghItems(t, pod, claim.RoleTester)
	if !slices.Equal(held, ghItemsFor(servedDirs...)) {
		t.Errorf("the replaced pod's gh volume projects %+v, want the two files of each of %q", held, servedDirs)
	}
	for _, want := range ghItemsFor(addedOwner) {
		if !slices.Contains(held, want) {
			t.Errorf("the replaced pod's gh volume does not project %s at %s", want.Key, want.Path)
		}
	}
	if held := heldInPod(r2, pod, claim.Roles); len(held) > 0 {
		t.Fatalf("the replaced pod holds %+v, which a pod created now is not handed", held)
	}
	if drifted := driftOf(t, g.ctx, r2, newTester); len(drifted) > 0 {
		t.Fatalf("the replaced pod has drifted: %q", drifted)
	}
}

// ownerRuleOwners is the answer of the owner predicate this branch replaces, for the project
// serving repos with pair configured: the owner of every repository served, and the owner the
// pair moved a repository away from beside them whenever the project owned the pair's `to` owner
// and owned nothing of its `from` owner. It is written here to be compared against
// appauth.GitHubDirs, the rule that replaces it, since a pod created before this change
// projects a directory for each owner this answered.
func ownerRuleOwners(repos []ghrepo.Repository, pair ghrepo.Rename) []string {
	owns := func(owner string) bool {
		return slices.ContainsFunc(repos, func(r ghrepo.Repository) bool { return strings.EqualFold(r.Owner(), owner) })
	}
	owners := make([]string, 0, len(repos)+1)
	for _, repo := range repos {
		if owner := strings.ToLower(repo.Owner()); !slices.Contains(owners, owner) {
			owners = append(owners, owner)
		}
	}
	from, to := strings.ToLower(pair.From.Owner()), strings.ToLower(pair.To.Owner())
	if from != to && owns(to) && !owns(from) {
		owners = append(owners, from)
	}
	return slices.Sorted(slices.Values(owners))
}

// A pod created before this change, whose gh volume projects one directory for each owner the
// daemon served then, is unmoved against a daemon resolving the smoke repository's pair per
// repository, in both owner-set states: state A, where the project serves no repository of the
// recorded owner beyond what the pair gives it, and state B, where it serves one more
// (otherOfRecordedOwner). That is what the directory set can do and no more: the predicate this
// branch replaces added the pair's `from` owner whenever the project owned the pair's `to`
// owner, while the rule now adds it only when the project serves the pair's `to` repository, so
// the new set can lose a name and never gain one — and in state B, the one state where the two
// differ, the `from` owner is in the set anyway, as the owner of a repository the project serves
// under its own name (ownerRuleOwners above answers renamedDirs for both states). movedGhDirs
// compares one way, naming only what the daemon serves and the pod does not project, so a set
// that cannot gain a name moves no pod: no update round is opened over a gh directory, no issue
// pod is replaced, and the role's own addresses stay where they are.
//
// Those addresses are the rest of what this holds, in each state: the role's generation is started
// with GH_CONFIG_DIR naming the directory of the owner the issue's record names — the directory
// the pod projects, whose name authorises nothing — its pod is addressed at the cache of the
// repository that record names, not the one GitHub holds it under now, and that cache's token is
// minted for the owner serving the repository now, which is the one half a rename does move.
func TestARenamedIssuesPodIsUnmovedInEitherOwnerSetState(t *testing.T) {
	stateA := []ghrepo.Repository{servingSmoke}
	stateB := []ghrepo.Repository{servingSmoke, otherOfRecordedOwner}
	for state, repos := range map[string][]ghrepo.Repository{"A": stateA, "B": stateB} {
		serves := appauth.GitHubDirs(func() []ghrepo.Repository { return repos }, ghrepo.MustRenames(smokeRename),
			nil, appauth.Roles, recordedSmoke)
		owned := ownerRuleOwners(repos, smokeRename)
		if !slices.Equal(serves, renamedDirs) || !slices.Equal(owned, renamedDirs) {
			t.Fatalf("state %s serves %q by the repository rule and %q by the owner predicate it replaces, want %q from both: the new set gains no name, so a pod built under the old one lacks no directory",
				state, serves, owned, renamedDirs)
		}
	}

	g := newRig(t, nil, withOptions(servingRenamed(stateA...)))
	spec := workerSpec(t)
	loc := g.spawn(spec)
	pod := g.pod(loc.Sandbox.Name)
	ghVolumeName := roleVolume(ghVolume, claim.RoleTester)
	if held := ghItems(t, pod, claim.RoleTester); !slices.Equal(held, ghItemsFor(renamedDirs...)) {
		t.Fatalf("the tester's gh volume projects %+v, want the two files of each of %q: the directories a daemon of either state serves, and the owner predicate's own answer", held, renamedDirs)
	}
	cache := CacheSandboxName(testProject, spec.Repository)
	// reading is what k5 records of one owner-set state: whether the runtime reports the pod's gh
	// address moved, the GH_CONFIG_DIR the role's generation is started with, the repository cache
	// its pod's fetch is addressed at, and the owner that cache's token is minted for.
	type reading struct {
		moved                           bool
		configDir, cacheURL, cacheToken string
	}
	read := func(r *Runtime) reading {
		t.Helper()
		_, moved := r.movedGhDirs(g.pod(loc.Sandbox.Name), claim.RoleTester)
		return reading{
			moved:      moved,
			configDir:  startEnv(lastStart(t, g, workerToken))["GH_CONFIG_DIR"],
			cacheURL:   g.fetchEnv(loc.Sandbox.Name)["LEGION_REPOSITORY_CACHE_URL"],
			cacheToken: string(g.secret(cacheSecretName(cache)).Data[provisionTokenKey]),
		}
	}
	want := reading{
		moved:      false,
		configDir:  ghconfig.Dir(GHConfigDir, recordedSmoke.Owner()),
		cacheURL:   g.r.cacheURL(cache),
		cacheToken: "ghs_provision_" + servingSmoke.Owner(),
	}

	if got := read(g.r); got != want {
		t.Fatalf("state A reads %+v, want %+v", got, want)
	}
	if moved := CacheSandboxName(testProject, servingSmoke); g.sandbox(moved) != nil {
		t.Errorf("the launch made a cache named %s, the repository GitHub holds the record's under: only the token follows a rename, never an address", moved)
	}
	if held := heldInPod(g.r, pod, claim.Roles); len(held) > 0 {
		t.Fatalf("state A holds %+v in the pod, which a pod created now is not handed", held)
	}

	r2 := secondRuntime(t, g, servingRenamed(stateB...))()
	g.connectRunning(workerToken, loc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	if obs := readopt(t, g.ctx, r2, loc)[0]; obs.Kind != runtime.Alive || obs.Locator != loc {
		t.Fatalf("%s at %+v, want the tester alive at its recorded locator in state B: %s", obs.Kind, obs.Locator, obs.Detail)
	}
	for _, reason := range driftOf(t, g.ctx, r2, loc) {
		if strings.Contains(reason, ghVolumeName) {
			t.Errorf("PodDrift names %q in state B, so the daemon opens an update round over the pod's gh directory", reason)
		}
	}
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	kept, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume the tester in state B: %v", err)
	}
	if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume in state B replaced the pod %s, which holds the issue's disk", loc.Sandbox.PodUID)
	}
	r2.refreshCacheTokens(g.ctx)
	if got := read(r2); got != want {
		t.Fatalf("state B reads %+v, want %+v", got, want)
	}
	if held := heldInPod(r2, pod, claim.Roles); len(held) > 0 {
		t.Fatalf("state B holds %+v in the pod, which a pod created now is not handed", held)
	}
	later := launchOf(t, r2, childSpec(t))
	if dir := ghconfig.Dir(GHConfigDir, later.gitHubDefault); later.cacheURL != want.cacheURL || dir != want.configDir {
		t.Errorf("a launch of another issue recorded on %s names the cache %q and GH_CONFIG_DIR %q, want %q and %q: a pod created in state B lands where this one stands",
			recordedSmoke, later.cacheURL, dir, want.cacheURL, want.configDir)
	}

	// The control k5 names: the same pod against a daemon serving an owner it projects no
	// directory for (servingAddedOwner, which serves addedOwner beside this package's static
	// owners). The runtime must report that pod moved, and the launch a role gets from such a
	// daemon must name a gh directory the pod does not project. The control shows that this
	// reading can fail — the two states above are no vacuous pass — and it is no failure of the
	// head either, since the change moves neither value it reads: what the control changes is the
	// owner set itself, which gains a name here and can only lose one across the states above.
	r3 := secondRuntime(t, g, servingAddedOwner)()
	control, ok := r3.movedGhDirs(pod, claim.RoleTester)
	if want := (movedAddress{ghVolumeName, ghDirsName(renamedDirs), ghDirsName(servedDirs)}); !ok || control != want {
		t.Fatalf("the control reads %+v (moved %t), want %+v: a daemon serving an owner the pod projects no directory for moves it", control, ok, want)
	}
	launched := ghItems(t, &corev1.Pod{Spec: r3.podTemplate(launchOf(t, r3, spec)).Spec}, claim.RoleTester)
	for _, item := range ghItemsFor(addedOwner) {
		if !slices.Contains(launched, item) || slices.Contains(ghItems(t, pod, claim.RoleTester), item) {
			t.Errorf("the control's launch projects %+v, and the pod %+v; want %s at %s in the launch alone", launched, ghItems(t, pod, claim.RoleTester), item.Key, item.Path)
		}
	}
}

// A broker enrollment changed under running roles — another audience or token expiry, which only a
// daemon restart brings — is read on every role of the pod at once: PodDrift names, for each, the
// projection the pod holds and the one a pod created now carries, so one update round is opened
// over the whole pod and each role keeps running until it has answered. None of them is
// StaleAddress, which would have the supervisor repoint it mid-turn, and no relaunch replaces the
// pod under the rest. The round's one ReplacePod ends the pod: every role in it is reported ended
// by the daemon's own act (Displaced), never dead, and relaunched together, as the supervisor's
// machines relaunch them, they move into one new pod that has drifted from nothing.
func TestChangingTheBrokersAudienceDriftsEveryRoleOfThePodTogether(t *testing.T) {
	before := AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	now := before
	now.Audience = "agent-secrets-next"
	g := newRig(t, nil, withOptions(func(o *Options) { o.AgentSecrets = &before }))
	tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
	testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)
	name := testerLoc.Sandbox.Name

	r2 := secondRuntime(t, g, func(o *Options) { o.AgentSecrets = &now })()
	for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
		g.connectRunning(loc.Claim, loc.Sandbox.Generation)
		g.eventually("the "+string(loc.Claim)+" launcher to report its child to the restarted daemon", func() bool {
			state, connected := r2.launchers.state(loc.Claim, loc.Sandbox.PodUID)
			return connected && state.Child != nil
		})
	}
	if err := r2.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &testerLoc}, {Claim: otherToken, Locator: &reviewerLoc}}, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := r2.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	drift := (movedAddress{agentSecretsTokenVolume, enrollmentName(brokerTokenProjection(&before)), enrollmentName(brokerTokenProjection(&now))}).String()
	seen := []runtime.Observation{next(t, observations), next(t, observations)}
	for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
		obs := observationOf(t, seen, loc)
		if obs.Kind != runtime.Alive || obs.Locator != loc {
			t.Fatalf("%s: %s at %+v (%s), want it alive at its recorded locator until its round ends", loc.Claim, obs.Kind, obs.Locator, obs.Detail)
		}
		if drifted := driftOf(t, g.ctx, r2, loc); !slices.Contains(drifted, drift) {
			t.Fatalf("%s: PodDrift = %q, want %q among it", loc.Claim, drifted, drift)
		}
	}

	// The round is over: the pod is ended once, for every role in it at once.
	if err := r2.ReplacePod(g.ctx, runtime.Known{Claim: workerToken, Locator: &testerLoc}, stillDue); err != nil {
		t.Fatalf("ReplacePod: %v", err)
	}
	ends := map[claim.Token]runtime.Observation{}
	for range 40 {
		if len(ends) == 2 {
			break
		}
		obs := next(t, observations)
		if obs.Kind == runtime.Gone || obs.Kind == runtime.NotRecordedProcess {
			ends[obs.Locator.Claim] = obs
		}
	}
	for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
		ended, ok := ends[loc.Claim]
		if !ok {
			t.Fatalf("%s: no observation ended its process in the replaced pod", loc.Claim)
		}
		if !ended.Displaced || ended.Locator != loc {
			t.Errorf("%s: %s at %+v, Displaced %t (%s); want its process reported displaced at its recorded locator",
				loc.Claim, ended.Kind, ended.Locator, ended.Displaced, ended.Detail)
		}
	}

	// Both relaunches begin before either provisions the new pod, as the supervisor's machines begin
	// theirs on the ends above: the pod's launch turn is held until both are waiting on it.
	held, err := r2.lockPod(g.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	type relaunched struct {
		loc runtime.Locator
		err error
	}
	results := make(chan relaunched, 2)
	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	reviewer.Generation, reviewer.BootToken, reviewer.ResumeSessionFile = 2, "boot-reviewer-g2", resumeSession
	for _, relaunch := range []struct {
		prev runtime.Locator
		spec runtime.SpawnSpec
	}{{testerLoc, tester}, {reviewerLoc, reviewer}} {
		go func() {
			loc, err := r2.Resume(g.ctx, &relaunch.prev, relaunch.spec)
			results <- relaunched{loc, err}
		}()
	}
	g.eventually("both relaunches to wait on the pod's launch turn", func() bool {
		_, testerWatched := r2.recorded(workerToken)
		_, reviewerWatched := r2.recorded(otherToken)
		return !testerWatched && !reviewerWatched
	})
	held()
	var moved []runtime.Locator
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("a relaunch failed, which the supervisor would charge: %v", result.err)
		}
		moved = append(moved, result.loc)
	}
	if moved[0].Sandbox.PodUID != moved[1].Sandbox.PodUID || moved[0].Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("the roles relaunched into pods %s and %s, want one new pod in place of %s",
			moved[0].Sandbox.PodUID, moved[1].Sandbox.PodUID, testerLoc.Sandbox.PodUID)
	}
	if held := heldInPod(r2, g.pod(name), claim.Roles); len(held) > 0 {
		t.Fatalf("the new pod holds %+v, which a pod created now is not handed", held)
	}
	for _, loc := range moved {
		if drifted := driftOf(t, g.ctx, r2, loc); len(drifted) > 0 {
			t.Fatalf("the new pod has drifted for %s: %q", loc.Claim, drifted)
		}
	}
}

// A pod's agent-secrets volumes are fixed when it is created, so a pod made under another audience
// or token expiry has drifted, as one made before the runtime enrolled has; a pod made under the
// enrollment the runtime has now has not, whatever else its stored volumes carry beside the
// token's audience, expiry and path and the key directories — a mode, another projection source, a
// size the API server or an admission webhook set — since opening a round on those would rebuild
// the pod and re-provision its workspace at every restart. Either way the relaunch keeps the live
// pod: a drifted one is its round's to replace, never a launch's.
func TestAPodEnrolledOtherwiseThanTheRuntimeIsNowHasDrifted(t *testing.T) {
	broker := AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	carryingMore := func(pod *corev1.Pod) {
		for i := range pod.Spec.Volumes {
			volume := &pod.Spec.Volumes[i]
			switch {
			case volume.Projected != nil && volume.Name == agentSecretsTokenVolume:
				volume.Projected.DefaultMode = new(int32(0o444))
				volume.Projected.Sources = append(volume.Projected.Sources,
					corev1.VolumeProjection{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}},
					corev1.VolumeProjection{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
						Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
					}}}})
			case volume.EmptyDir != nil && strings.HasPrefix(volume.Name, agentSecretsKeyVolume):
				volume.EmptyDir.SizeLimit = nil
			}
		}
	}
	for name, tc := range map[string]struct {
		now     func(*AgentSecrets)
		stored  func(*corev1.Pod)
		drifted bool
	}{
		"the same enrollment":                       {func(*AgentSecrets) {}, nil, false},
		"the same enrollment, stored carrying more": {func(*AgentSecrets) {}, carryingMore, false},
		"another audience":                          {func(a *AgentSecrets) { a.Audience = "agent-secrets-next" }, nil, true},
		"another token expiry":                      {func(a *AgentSecrets) { a.TokenExpiry = 30 * time.Minute }, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			before, now := broker, broker
			tc.now(&now)
			g := newRig(t, nil, withOptions(func(o *Options) { o.AgentSecrets = &before }))
			spec := workerSpec(t)
			loc := g.spawn(spec)
			if tc.stored != nil {
				g.update(g.pod(loc.Sandbox.Name), tc.stored)
			}
			r2 := secondRuntime(t, g, func(o *Options) { o.AgentSecrets = &now })()
			g.connectRunning(workerToken, loc.Sandbox.Generation)
			g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			want := (movedAddress{agentSecretsTokenVolume, enrollmentName(brokerTokenProjection(&before)), enrollmentName(brokerTokenProjection(&now))}).String()
			drifted := driftOf(t, g.ctx, r2, loc)
			if slices.Contains(drifted, want) != tc.drifted {
				t.Fatalf("PodDrift = %q, want the enrollment drifted %t (%q)", drifted, tc.drifted, want)
			}
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
			newLoc, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if newLoc.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("Resume replaced the pod %s with %s; a drifted pod is its round's to replace", loc.Sandbox.PodUID, newLoc.Sandbox.PodUID)
			}
			if held := heldInPod(r2, g.pod(loc.Sandbox.Name), claim.Roles); len(held) > 0 {
				t.Fatalf("the pod the tester runs in holds %+v, which a pod created now is not handed", held)
			}
		})
	}
}

// heldInPod is everything pod holds, for any role of roles, that a pod created now is not handed
// and that no round can ask its workers about first (movedInPod, the one rule evaluate and
// relaunch read it by).
func heldInPod(r *Runtime, pod *corev1.Pod, roles []claim.Role) []movedAddress {
	var held []movedAddress
	for _, role := range roles {
		held = append(held, r.movedInPod(pod, role)...)
	}
	return held
}

// One rule decides both whether evaluate reads a role's pod as stale and whether relaunch replaces
// that pod (movedInPod): a role whose pod holds a move its workers cannot be asked about — the
// stream its launcher dials — is reported StaleAddress naming it, and its relaunch replaces the
// pod. An address carried in the generation's environment alone (the daemon's API, the broker's
// URL with its pod's enrollment unchanged) is stale too, but its relaunch starts a new generation
// in the same pod. What has drifted instead is neither: the broker's enrollment, configured only
// now or changed, leaves the role Alive and its pod kept, and is read by PodDrift alone, for its
// update round to act on once its workers have pushed — and enrolling only now moves no address
// of the running generation, which runs in a pod carrying no enrollment and so holds no broker
// URL to move (Runtime.enrollsIn). A role holding nothing moved and whose pod has drifted from
// nothing is alive and its pod kept.
func TestEvaluateReadsAPodStaleExactlyWhenRelaunchReplacesIt(t *testing.T) {
	broker := AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	enrolled := func(edit func(*AgentSecrets)) func(*Options) {
		return func(o *Options) {
			a := broker
			edit(&a)
			o.AgentSecrets = &a
		}
	}
	unchanged := func(*AgentSecrets) {}
	for name, tc := range map[string]struct {
		before, now func(*Options)
		// where is where the moved address is carried, as the detail names it; "" for nothing moved.
		where string
		// drifts is what PodDrift names of the pod, "" for nothing drifted.
		drifts   string
		replaced bool
	}{
		"nothing moved":                 {before: func(*Options) {}, now: func(*Options) {}},
		"the worker stream moved":       {before: func(*Options) {}, now: moveStream, where: connectFlag, replaced: true},
		"the daemon's API moved":        {before: func(*Options) {}, now: func(o *Options) { o.DaemonURL = movedDaemonURL }, where: "LEGION_DAEMON_URL"},
		"the broker enrolled only now":  {before: func(*Options) {}, now: enrolled(unchanged), drifts: agentSecretsTokenVolume},
		"the broker's audience changed": {before: enrolled(unchanged), now: enrolled(func(a *AgentSecrets) { a.Audience = "agent-secrets-next" }), drifts: agentSecretsTokenVolume},
		"the broker's expiry changed":   {before: enrolled(unchanged), now: enrolled(func(a *AgentSecrets) { a.TokenExpiry = 30 * time.Minute }), drifts: agentSecretsTokenVolume},
		"the broker's URL moved alone":  {before: enrolled(unchanged), now: enrolled(func(a *AgentSecrets) { a.URL = "https://secrets-next.internal.example" }), where: "AGENT_SECRETS_URL"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(tc.before))
			tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
			testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)
			r2 := secondRuntime(t, g, tc.now)()
			for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
				g.connectRunning(loc.Claim, loc.Sandbox.Generation)
				g.eventually("the "+string(loc.Claim)+" launcher to report its child to the restarted daemon", func() bool {
					state, connected := r2.launchers.state(loc.Claim, loc.Sandbox.PodUID)
					return connected && state.Child != nil
				})
			}
			seen := readopt(t, g.ctx, r2, testerLoc, reviewerLoc)
			for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
				obs := observationOf(t, seen, loc)
				switch {
				case tc.where == "" && obs.Kind != runtime.Alive:
					t.Fatalf("%s: %s (%s), want alive: nothing its launcher cannot be asked about moved", loc.Claim, obs.Kind, obs.Detail)
				case tc.where != "" && (obs.Kind != runtime.StaleAddress || !strings.Contains(obs.Detail, tc.where+" ")):
					t.Fatalf("%s: %s (%s), want StaleAddress naming %s", loc.Claim, obs.Kind, obs.Detail, tc.where)
				}
				drifted := strings.Join(driftOf(t, g.ctx, r2, loc), "; ")
				if (tc.drifts == "") != (drifted == "") || (tc.drifts != "" && !strings.Contains(drifted, tc.drifts+" ")) {
					t.Fatalf("%s: PodDrift = %q, want %q", loc.Claim, drifted, tc.drifts)
				}
			}
			fromPod := len(heldInPod(r2, g.pod(testerLoc.Sandbox.Name), claim.Roles)) > 0

			tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
			newLoc, err := r2.Resume(g.ctx, &testerLoc, tester)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			replaced := newLoc.Sandbox.PodUID != testerLoc.Sandbox.PodUID
			if replaced != fromPod || replaced != tc.replaced {
				t.Fatalf("Resume replaced the pod: %t; the pod held a move no worker can be asked about: %t; want both %t", replaced, fromPod, tc.replaced)
			}
		})
	}
}

// A role already running with every address the runtime hands now is left alone: Observe reports it
// Alive, never StaleAddress, so nothing about it is ever replaced.
func TestARoleAtTheCurrentAddressesIsNeverRepointed(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	if obs := readopt(t, g.ctx, g.r, loc)[0]; obs.Kind != runtime.Alive || obs.Locator != loc {
		t.Fatalf("%s at %+v, want Alive at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
}

// A claim mid-launch — its issue pod created and Pending, exactly what the runtime itself sees of a
// StateLaunching claim — is reported StaleAddress the same way a ready claim's is: the runtime has
// no notion of registration at all, only of the pod it watches, so re-adoption catches a stale
// stream whatever phase the supervisor's claim is in.
func TestAClaimMidLaunchWithAStaleStreamIsReported(t *testing.T) {
	g := newRig(t, nil)
	g.autoStart.Store(false) // the pod the controller creates stays Pending
	loc := g.spawn(workerSpec(t))
	if pod := g.pod(loc.Sandbox.Name); pod == nil || pod.Status.Phase != corev1.PodPending {
		t.Fatalf("the issue pod is %+v, want it Pending", pod)
	}

	restarted := secondRuntime(t, g, moveStream)
	if obs := readopt(t, g.ctx, restarted(), loc)[0]; obs.Kind != runtime.StaleAddress || obs.Locator != loc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
}

// Recording a generation's addresses costs one API write per role start: a role started in a
// running issue pod sends the Sandbox one patch, of that role's annotation alone, and nothing else.
func TestARoleStartedInARunningPodWritesOnlyItsAddressRecord(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(rootSpec(t))
	g.clearActions()
	g.spawn(workerSpec(t))
	writes := g.writes()
	if len(writes) != 1 || writes[0].verb != "patch" || writes[0].resource != "sandboxes" || writes[0].name != SandboxName(workerToken) {
		t.Fatalf("a start in a running pod wrote %v, want one patch of its Sandbox", writes)
	}
	if body := writes[0].patch; !strings.Contains(body, addressesAnnotation(claim.RoleTester)) || strings.Contains(body, addressesAnnotation(claim.RoleArchitect)) {
		t.Errorf("the patch %s is not the tester's record alone", body)
	}
	t.Logf("one role start: %d write, %d bytes of patch body", len(writes), len(writes[0].patch))
}
