package sandbox

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
	if err := r.start(g.ctx, g.dyn, g.kube); err != nil {
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

// connectOf is the --connect value the named role container's launcher dials.
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

// An issue pod whose launchers dial the stream the daemon moved from is reported StaleAddress for
// each of its roles the moment the runtime at the new address is told of their claims
// (re-adoption, ReconcileOrphans): a launcher never redials elsewhere, so it would otherwise read as
// disconnected for ever. The first role the supervisor relaunches — a Resume over its recorded
// locator — replaces the issue pod, whose launchers dial the corrected stream, and resumes the
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
	for _, role := range claim.Roles {
		if dialed := connectOf(t, pod, string(role)); dialed != movedStreamURL {
			t.Errorf("the replaced pod's %s launcher dials %q, want the new stream %q", role, dialed, movedStreamURL)
		}
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
// and the shim refuses to start without its token file. So the role is reported StaleAddress, its
// detail naming the projection the pod lacks, and the relaunch replaces the issue pod rather than
// start the role in one that cannot run it; the new pod mounts both for every role, and a sibling
// resumes into that same new pod.
func TestEnrollingWithTheSecretsBrokerReplacesAnIssuePodMadeWithoutIt(t *testing.T) {
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
	if obs.Kind != runtime.StaleAddress || obs.Locator != testerLoc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
	if want := (movedAddress{agentSecretsTokenVolume, "(unset)", enrollmentName(brokerTokenProjection(broker))}).String(); !strings.Contains(obs.Detail, want) {
		t.Errorf("detail %q lacks %q", obs.Detail, want)
	}

	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	newTester, err := r2.Resume(g.ctx, &testerLoc, tester)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if newTester.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("Resume kept the pod %s, which has no agent-secrets volume for the enrolled tester's shim", testerLoc.Sandbox.PodUID)
	}
	pod := g.pod(testerLoc.Sandbox.Name)
	if r2.lacksEnrollment(pod, claim.Roles) {
		t.Fatalf("the replaced pod's volumes %+v lack the enrollment the runtime has now", pod.Spec.Volumes)
	}
	for _, role := range claim.Roles {
		mounted := slices.ContainsFunc(containerNamed(t, pod.Spec, string(role)).VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == agentSecretsTokenVolume && m.MountPath == AgentSecretsTokenDir
		})
		if !mounted {
			t.Errorf("the replaced pod's %s launcher does not mount the broker's token at %s", role, AgentSecretsTokenDir)
		}
	}
	if start := lastStart(t, g, workerToken); start.Generation != 2 || !slices.Contains(start.Argv, AgentSecretsTokenDir+"/"+AgentSecretsTokenFile) {
		t.Errorf("the tester started generation %d with %q, want generation 2 pointed at the pod's token file", start.Generation, start.Argv)
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

// A broker enrollment changed under running roles — another audience or token expiry, which only a
// daemon restart brings — is read on every role of the pod at once: each is StaleAddress at its
// re-adoption, naming the projection the pod holds and the one a pod created now carries, so the
// supervisor relaunches every one of them uncharged (supervise's repoint), rather than leaving them
// running until some unrelated relaunch replaces the pod under the rest. Relaunched together, as the
// supervisor's machines relaunch them, they move into one new pod, and no role is ever reported
// gone or not the recorded process on the way.
func TestChangingTheBrokersAudienceMovesEveryRoleOfThePodTogether(t *testing.T) {
	before := AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	now := before
	now.Audience = "agent-secrets-next"
	g := newRig(t, nil, withOptions(func(o *Options) { o.AgentSecrets = &before }))
	tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
	testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)
	name := testerLoc.Sandbox.Name

	r2 := secondRuntime(t, g, func(o *Options) { o.AgentSecrets = &now })()
	if err := r2.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &testerLoc}, {Claim: otherToken, Locator: &reviewerLoc}}, 0); err != nil {
		t.Fatal(err)
	}
	observations, err := r2.Observe(g.ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := (movedAddress{agentSecretsTokenVolume, enrollmentName(brokerTokenProjection(&before)), enrollmentName(brokerTokenProjection(&now))}).String()
	seen := []runtime.Observation{next(t, observations), next(t, observations)}
	for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
		obs := observationOf(t, seen, loc)
		if obs.Kind != runtime.StaleAddress || obs.Locator != loc || !strings.Contains(obs.Detail, stale) {
			t.Fatalf("%s: %s at %+v (%s), want StaleAddress at the recorded locator naming %q", loc.Claim, obs.Kind, obs.Locator, obs.Detail, stale)
		}
	}

	// Every observation from here on: none may say a role is gone or not the recorded process.
	var mu sync.Mutex
	var later []runtime.Observation
	collecting, stop := context.WithCancel(g.ctx)
	defer stop()
	go func() {
		for {
			select {
			case obs := <-observations:
				mu.Lock()
				later = append(later, obs)
				mu.Unlock()
			case <-collecting.Done():
				return
			}
		}
	}()
	// Both relaunches begin before either replaces the pod, as the supervisor's machines begin theirs
	// on the observations above: the pod's launch turn is held until both are waiting on it.
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
	var pods []string
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("a relaunch failed, which the supervisor would charge: %v", result.err)
		}
		pods = append(pods, result.loc.Sandbox.PodUID)
	}
	if pods[0] != pods[1] || pods[0] == testerLoc.Sandbox.PodUID {
		t.Fatalf("the roles relaunched into pods %v, want one new pod in place of %s", pods, testerLoc.Sandbox.PodUID)
	}
	if pod := g.pod(name); r2.lacksEnrollment(pod, claim.Roles) {
		t.Fatalf("the new pod lacks the enrollment the runtime has now: %+v", pod.Spec.Volumes)
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	for _, obs := range later {
		if obs.Kind == runtime.Gone || obs.Kind == runtime.NotRecordedProcess {
			t.Errorf("%s was reported %s on the way (%s)", obs.Locator.Claim, obs.Kind, obs.Detail)
		}
	}
}

// A pod's agent-secrets volumes are fixed when it is created, so a relaunch of a role that enrolls
// replaces a pod made under another audience or token expiry, as it does one made before the
// runtime enrolled; a pod made under the enrollment the runtime has now is kept, whatever else its
// stored volumes carry beside the token's audience, expiry and path and the key directories — a
// mode, another projection source, a size the API server or an admission webhook set — since
// replacing it for those would rebuild the pod and re-provision its workspace at every relaunch.
func TestARelaunchReplacesAPodEnrolledOtherwiseThanTheRuntimeIsNow(t *testing.T) {
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
		now      func(*AgentSecrets)
		stored   func(*corev1.Pod)
		replaced bool
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
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
			newLoc, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if replaced := newLoc.Sandbox.PodUID != loc.Sandbox.PodUID; replaced != tc.replaced {
				t.Fatalf("Resume replaced the pod: %t, want %t (pod %s, then %s)", replaced, tc.replaced, loc.Sandbox.PodUID, newLoc.Sandbox.PodUID)
			}
			if pod := g.pod(loc.Sandbox.Name); r2.lacksEnrollment(pod, claim.Roles) {
				t.Fatalf("the pod the tester runs in lacks the enrollment the runtime has now: %+v", pod.Spec.Volumes)
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
