package sandbox

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// rolesLocator is a recorded role process locator in its issue pod's one container, `roles`.
func rolesLocator(token claim.Token, uid string) runtime.Locator {
	loc := sandboxLocator(token, uid)
	loc.Sandbox.Container = "roles"
	return loc
}

// An issue pod runs every role of the issue in one container, `roles`, whose PID 1 is one
// `legion launcher` serving all six (multi-role mode: --roles-dir and one --role per role, each
// role's token and private directory under the roles directory). Each role's per-role volumes are
// mounted under /var/run/legion/roles/<role>/ — its launcher token and gh files read-only, its
// private, state and agent-secrets key directories its own — and none at the fixed paths a role
// reads them at, which role-init binds inside the role's namespaces; the role homes volume is a
// disk emptyDir at /var/run/legion/roles/.homes; the shared mounts are mounted once. The container
// and both init containers carry the one issue-pod reservation, by default the six roles' former
// sum: 3 CPU and 19Gi, each requested in full, and an 80Gi disk limit over a 6Gi request.
func TestAnIssuePodRunsEveryRoleInOneContainer(t *testing.T) {
	opts := goldenOptions()
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	l := launchOf(t, r, rootSpec(t))
	pod := r.podTemplate(l).Spec
	if len(pod.Containers) != 1 || pod.Containers[0].Name != "roles" {
		names := make([]string, 0, len(pod.Containers))
		for _, c := range pod.Containers {
			names = append(names, c.Name)
		}
		t.Fatalf("the issue pod runs containers %v, want one, roles", names)
	}
	c := pod.Containers[0]
	want := []string{"/opt/legion/bin/legion", "launcher", "--connect", opts.StreamURL, "--sandbox", l.name, "--roles-dir", "/var/run/legion/roles"}
	for _, role := range claim.Roles {
		want = append(want, "--role", string(role))
	}
	want = append(want, "--stop-grace", "30s")
	if !slices.Equal(c.Command, want) {
		t.Errorf("the roles container runs %q, want %q", c.Command, want)
	}
	byPath := map[string]corev1.VolumeMount{}
	byName := map[string]int{}
	for _, m := range c.VolumeMounts {
		if _, twice := byPath[m.MountPath]; twice {
			t.Errorf("the roles container mounts %s twice", m.MountPath)
		}
		byPath[m.MountPath] = m
		byName[m.Name]++
	}
	for _, role := range claim.Roles {
		dir := "/var/run/legion/roles/" + string(role)
		for _, mount := range []struct {
			path, volume string
			readOnly     bool
		}{
			{dir + "/launcher", "launcher-" + string(role), true},
			{dir + "/gh", "gh-" + string(role), true},
			{dir + "/private", "private-" + string(role), false},
			{dir + "/state", "state-" + string(role), false},
			{dir + "/agent-secrets", "agent-secrets-key-" + string(role), false},
		} {
			if got, ok := byPath[mount.path]; !ok || got.Name != mount.volume || got.ReadOnly != mount.readOnly || got.SubPath != "" {
				t.Errorf("the roles container mounts %+v at %s, want volume %s read-only %t", got, mount.path, mount.volume, mount.readOnly)
			}
		}
	}
	// The volumes that belong under the home are mounted under HomeMountsDir at the same relative
	// path, so the home carries no mount: each role's namespace overlays it, which Linux refuses
	// over a lower layer with mounts under it, and binds each back at its own path (role-init).
	for path, volume := range map[string]string{
		"/var/run/legion/roles/.homes": "role-homes", TreeRoot: issueVolume,
		"/var/run/legion/home/.omp/profiles/legion/agent/sessions": issueVolume,
		"/var/run/legion/home/.config":                             configVolume,
		FeedDir:                                                    issueVolume, AgentSecretsTokenDir: agentSecretsTokenVolume,
	} {
		if got, ok := byPath[path]; !ok || got.Name != volume {
			t.Errorf("the roles container mounts %+v at %s, want volume %s", got, path, volume)
		}
	}
	for _, m := range c.VolumeMounts {
		if m.MountPath == podHome || strings.HasPrefix(m.MountPath, podHome+"/") {
			t.Errorf("the roles container mounts %s at %s, under the home every role's namespace overlays: it belongs under %s", m.Name, m.MountPath, HomeMountsDir)
		}
	}
	for _, inNamespace := range []string{LauncherDir, LauncherPrivateDir, StateDir, GHConfigDir, AgentSecretsKeyDir} {
		if m := mountHolding(c, inNamespace); m != nil {
			t.Errorf("the roles container mounts %s at %s, which only a role's namespace binds (role-init)", m.Name, m.MountPath)
		}
	}
	for name, count := range byName {
		if want := map[bool]int{true: 3, false: 1}[name == issueVolume]; count != want {
			t.Errorf("the roles container mounts volume %s %d times, want %d", name, count, want)
		}
	}
	var homes *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "role-homes" {
			homes = &pod.Volumes[i]
		}
	}
	if homes == nil || homes.EmptyDir == nil || homes.EmptyDir.Medium != corev1.StorageMediumDefault {
		t.Errorf("the role homes volume is %+v, want an emptyDir on the node's disk", homes)
	}
	reservation := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("19Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("6Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("19Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("80Gi")},
	}
	for _, container := range slices.Concat(pod.InitContainers, pod.Containers) {
		if !sameRequirements(container.Resources, reservation) {
			t.Errorf("container %s carries %+v, want the issue pod's one reservation %+v", container.Name, container.Resources, reservation)
		}
	}
}

// sameRequirements is whether two requirements hold the same quantities, however each is spelled.
func sameRequirements(a, b corev1.ResourceRequirements) bool {
	same := func(x, y corev1.ResourceList) bool {
		if len(x) != len(y) {
			return false
		}
		for name, quantity := range x {
			other, ok := y[name]
			if !ok || quantity.Cmp(other) != 0 {
				return false
			}
		}
		return true
	}
	return same(a.Requests, b.Requests) && same(a.Limits, b.Limits)
}

// The roles container's own environment is the pod's alone, whichever role's launch created it:
// only what the kubelet resolves (the downward API's POD_UID), never a plain value, so no role's
// value (its XDG_STATE_HOME, LEGION_ROLE, generation), no operator value and nothing secret lands in
// a container every role's launcher shares. Each such value travels in that role's generation's
// start command alone.
func TestTheRolesContainerEnvironmentNamesNoRole(t *testing.T) {
	opts := goldenOptions()
	opts.Pod.Env = map[string]string{"OPERATOR_SETTING": "operator-value"}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	var first []corev1.EnvVar
	for _, spec := range []runtime.SpawnSpec{rootSpec(t), workerSpec(t), childSpec(t)} {
		l := launchOf(t, r, spec)
		c := containerNamed(t, r.podTemplate(l).Spec, runtime.IssuePodContainer)
		if len(c.Env) == 0 {
			t.Fatalf("%s's launch gives the roles container no environment, want POD_UID from the downward API", spec.Role)
		}
		for _, v := range c.Env {
			if v.ValueFrom == nil {
				t.Errorf("%s's launch puts %s=%q in the roles container's own environment, want only kubelet-resolved values", spec.Role, v.Name, v.Value)
			}
			for _, role := range claim.Roles {
				if strings.Contains(v.String(), string(role)) {
					t.Errorf("%s's launch puts %s, naming role %s, in the roles container's own environment", spec.Role, v.String(), role)
				}
			}
		}
		if first == nil {
			first = c.Env
		} else if !slices.EqualFunc(first, c.Env, func(a, b corev1.EnvVar) bool { return a.String() == b.String() }) {
			t.Errorf("%s's launch gives the roles container %v, another role's launch %v; the environment is the pod's, not a role's", spec.Role, c.Env, first)
		}
	}
}

// toEarlierLayout edits a stored issue pod into the layout before one container: one container per
// workflow role, each named for its role and running a single-role launcher on the stream a pod
// created now is handed, all running.
func toEarlierLayout(p *corev1.Pod) {
	var containers []corev1.Container
	for _, role := range claim.Roles {
		containers = append(containers, corev1.Container{
			Name: string(role), Image: testImage,
			Command: []string{"/opt/legion/bin/legion", "launcher", connectFlag, testOptions().StreamURL, "--token-file", LauncherDir + "/" + LauncherTokenFile,
				"--sandbox", p.Name, "--role", string(role), "--private-dir", LauncherPrivateDir, "--stop-grace", "200ms"},
		})
	}
	p.Spec.Containers = containers
	p.Status = runningStatusOf(p.Spec)
}

// A running issue pod of the layout before one container — exactly one container per workflow
// role — is a pod a pod created now is not: every role recorded there is StaleAddress, before its
// launcher's state is read, so the supervisor repoints each uncharged. The first relaunch replaces
// it even when it is an installing launch, which alone keeps a drifted pod, and the replacement is
// a fresh pod on the configured image, so that launch installs nothing; the pod it makes runs one
// container, the siblings' old processes read as the daemon's replacement (Displaced), and a
// sibling resumes into the same new pod.
func TestAnIssuePodOfTheEarlierLayoutIsStaleForEveryRoleAndReplacedByTheNextLaunch(t *testing.T) {
	g := newRig(t, nil)
	tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
	testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)
	name := testerLoc.Sandbox.Name
	g.update(g.pod(name), toEarlierLayout)
	g.eventually("the store to see the earlier layout", func() bool {
		p := g.r.storedPod(name)
		return p != nil && len(p.Spec.Containers) == len(claim.Roles)
	})
	for _, loc := range []runtime.Locator{testerLoc, reviewerLoc} {
		obs, err := g.r.Probe(g.ctx, loc)
		if err != nil {
			t.Fatalf("Probe %s: %v", loc.Claim, err)
		}
		if obs.Kind != runtime.StaleAddress || !strings.Contains(obs.Detail, "the pod's containers architect, planner, implementer, tester, reviewer, merger, now roles") {
			t.Fatalf("%s in the earlier layout: %s %q, want StaleAddress naming the layout", loc.Claim, obs.Kind, obs.Detail)
		}
	}

	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	tester.Plugins = &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	moved, err := g.r.Resume(g.ctx, &testerLoc, tester)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if moved.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("the installing Resume kept the earlier layout's pod %s", testerLoc.Sandbox.PodUID)
	}
	pod := g.pod(name)
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "roles" {
		t.Fatalf("the replacement runs %d containers, want one, roles", len(pod.Spec.Containers))
	}
	start := lastStart(t, g, workerToken)
	if i := slices.Index(start.Argv, "--"); i < 0 || i+1 >= len(start.Argv) || start.Argv[i+1] == "/bin/sh" {
		t.Fatalf("the replaced pod's installing Resume started %q, want the plain agent: a fresh pod loads the image's plugins", start.Argv)
	}
	if obs, err := g.r.Probe(g.ctx, reviewerLoc); err != nil || !obs.Displaced {
		t.Fatalf("the reviewer's process in the replaced pod: %+v, %v; want the daemon's replacement (Displaced)", obs, err)
	}
	reviewer.Generation, reviewer.BootToken, reviewer.ResumeSessionFile = 2, "boot-reviewer-g2", resumeSession
	resumed, err := g.r.Resume(g.ctx, &reviewerLoc, reviewer)
	if err != nil {
		t.Fatalf("Resume the reviewer: %v", err)
	}
	if resumed.Sandbox.PodUID != moved.Sandbox.PodUID {
		t.Fatalf("the reviewer resumed into pod %s, want the tester's new pod %s", resumed.Sandbox.PodUID, moved.Sandbox.PodUID)
	}
	if obs, err := g.r.Probe(g.ctx, resumed); err != nil || obs.Kind != runtime.Alive {
		t.Fatalf("the reviewer in the new pod: %+v, %v; want Alive", obs, err)
	}
}

// A role's locator in an issue pod names the pod's one container, `roles`, so the role whose
// enrollment, address record and launcher are read is the claim's: an enrolled pod holding the
// role's own key directory is alive, and a generation the role's record says was told a moved
// daemon URL is stale.
func TestAnIssuePodsRoleIsReadFromItsClaimNotItsContainer(t *testing.T) {
	broker := func(o *Options) {
		o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	}
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	podUnder := func(t *testing.T, edit func(*Options)) *corev1.Pod {
		opts := testOptions()
		edit(&opts)
		r, err := configure(opts)
		if err != nil {
			t.Fatal(err)
		}
		spec := podOf(t, r, workerSpec(t))
		pod := podObject(name, recorded, sandboxUID, labels, runningStatusOf(spec))
		pod.Spec = spec
		return pod
	}
	recordOf := func(t *testing.T, edit func(*Options)) string {
		opts := testOptions()
		edit(&opts)
		r, err := configure(opts)
		if err != nil {
			t.Fatal(err)
		}
		record, err := r.recordFor(claim.RoleTester, 1)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	alive := shimwire.LauncherState{Child: &shimwire.LauncherChild{Generation: 1, PID: 42}}
	for _, tc := range []struct {
		name    string
		runtime func(*Options)
		record  func(*Options)
		want    runtime.ObservationKind
		detail  string
	}{
		{"an enrolled pod holding the role's own key directory", broker, nil, runtime.Alive, "role tester runs generation 1"},
		{"a generation told a moved daemon URL", func(*Options) {}, func(o *Options) { o.DaemonURL = movedDaemonURL }, runtime.StaleAddress, "LEGION_DAEMON_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sandboxObject(t, name, sandboxUID, modeRunning, labels)
			if tc.record != nil {
				s.SetAnnotations(map[string]string{addressesAnnotation(claim.RoleTester): recordOf(t, tc.record)})
			}
			g := newRig(t, []k8sruntime.Object{s, podUnder(t, tc.runtime)}, withoutController(), withOptions(tc.runtime))
			g.reportLauncher(workerToken, alive)
			obs, err := g.r.Probe(g.ctx, rolesLocator(workerToken, recorded))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if obs.Kind != tc.want || !strings.Contains(obs.Detail, tc.detail) {
				t.Fatalf("%s %q, want %s naming %q", obs.Kind, obs.Detail, tc.want, tc.detail)
			}
		})
	}
}

// The issue pod's one container ending ends every role of it: each role recorded there is Gone,
// its detail naming the container's reason and exit code, OOMKilled included, and quoting its own
// lines of the container's log alone (roleLines), never a sibling's or the launcher's unprefixed
// ones: the rig's log reads "fake logs", which no role's prefix begins.
func TestTheRolesContainerEndingEndsEveryRole(t *testing.T) {
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	status := corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "roles", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
	}}}
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeRunning, labels), podObject(name, recorded, sandboxUID, labels, status)}, withoutController())
	for _, token := range []claim.Token{workerToken, otherToken} {
		obs, err := g.r.Probe(g.ctx, rolesLocator(token, recorded))
		if err != nil {
			t.Fatalf("Probe %s: %v", token, err)
		}
		role, _ := token.Role()
		if obs.Kind != runtime.Gone || !strings.Contains(obs.Detail, "container roles terminated (OOMKilled, exit code 137)") ||
			!strings.Contains(obs.Detail, "last lines of "+string(role)) || strings.Contains(obs.Detail, "fake logs") {
			t.Fatalf("%s: %s %q, want Gone naming the container's end and quoting only %s's lines", token, obs.Kind, obs.Detail, role)
		}
	}
}

// A Gone's quote of the shared log is the role's newest lines, read through the pod's log as the
// kubelet serves it: a log of interleaved tester, reviewer and launcher lines larger than the byte
// bound quotes the tester's last lines, the ones nearest the container's end, and none of the
// reviewer's or the tester's oldest; the launcher's own unprefixed lines follow under their own
// header, its newest, and none of the reviewer's.
func TestARolesQuoteIsItsNewestLinesOfALogPastTheBound(t *testing.T) {
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	status := corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "roles", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
	}}}
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeRunning, labels), podObject(name, recorded, sandboxUID, labels, status)}, withoutController())
	padding := strings.Repeat("x", 1000)
	var log strings.Builder
	for i := range sharedLogLines {
		fmt.Fprintf(&log, "tester: line %d %s\nreviewer: secret %d\nlauncher tick %d\n", i, padding, i, i)
	}
	if log.Len() <= sharedLogBytes {
		t.Fatalf("the log is %d bytes, not past the %d-byte bound", log.Len(), sharedLogBytes)
	}
	g.kube.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &k8sruntime.Unknown{Raw: []byte(log.String())}, nil
	})
	obs, err := g.r.Probe(g.ctx, rolesLocator(workerToken, recorded))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	last := sharedLogLines - 1
	quote := obs.Detail[strings.Index(obs.Detail, "last lines of tester:\n")+len("last lines of tester:\n"):]
	own, container, found := strings.Cut(quote, "\n"+containerLinesHeader+"\n")
	if !found {
		t.Fatalf("the quote has no section of the container's own lines: %.200q", quote)
	}
	lines := strings.Split(own, "\n")
	if len(lines) != logTailLines || lines[len(lines)-1] != fmt.Sprintf("line %d %s", last, padding) ||
		lines[0] != fmt.Sprintf("line %d %s", last-logTailLines+1, padding) {
		t.Fatalf("the tester's quote is %d lines from %.20q to %.20q, want its last %d, line %d to line %d", len(lines), lines[0], lines[len(lines)-1], logTailLines, last-logTailLines+1, last)
	}
	if strings.Contains(own, "secret") || strings.Contains(own, "tick") {
		t.Fatalf("the tester's quote carries a sibling's or the launcher's line: %.200q", own)
	}
	ticks := strings.Split(container, "\n")
	if len(ticks) != logTailLines || ticks[0] != fmt.Sprintf("launcher tick %d", last-logTailLines+1) || ticks[len(ticks)-1] != fmt.Sprintf("launcher tick %d", last) {
		t.Fatalf("the container's own lines are %q, want the launcher's last %d ticks", ticks, logTailLines)
	}
	if strings.Contains(container, "secret") || strings.Contains(container, "tester:") {
		t.Fatalf("the container's own lines carry a role's line: %.200q", container)
	}
}

// The one container's log interleaves every role's lines, each prefixed `<role>: ` by the
// launcher: a role's quote is its own last lines alone, the prefix dropped, and a sibling's, the
// launcher's own unprefixed ones and a role whose name another's begins with are none of it.
func TestARolesQuoteIsItsOwnLinesOfTheSharedLog(t *testing.T) {
	log := strings.Join([]string{
		"launcher: connected",
		"tester: one",
		"reviewer: not mine",
		"tester: two",
		"testers: not mine either",
		"tester: three",
		"tester: four",
	}, "\n")
	if got, want := roleLines(log, claim.RoleTester, 3), "two\nthree\nfour"; got != want {
		t.Errorf("roleLines = %q, want %q", got, want)
	}
	if got := roleLines(log, claim.RoleMerger, 3); got != "" {
		t.Errorf("roleLines of a role with no lines = %q, want none", got)
	}
	if got, want := containerLines(log, 3), "launcher: connected\ntesters: not mine either"; got != want {
		t.Errorf("containerLines = %q, want %q", got, want)
	}
}

// When the launcher, PID 1 of the one container, panics, every role of the pod ends, and the
// cause is in its own unprefixed lines: each role's Gone quotes them after its own lines, and
// never a sibling's. The read's first line is attributed to no one, since the kubelet's tail may
// start it inside a line: a fragment there that reads as the tester's is not quoted as the
// tester's.
func TestEachRolesQuoteCarriesTheLaunchersCrash(t *testing.T) {
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	status := corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "roles", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}},
	}}}
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeRunning, labels), podObject(name, recorded, sandboxUID, labels, status)}, withoutController())
	log := strings.Join([]string{
		"tester: forged by a fragment",
		"tester: running the suite",
		"reviewer: reading the diff",
		"panic: runtime error: index out of range [3] with length 3",
		"",
		"goroutine 7 [running]:",
	}, "\n") + "\n"
	g.kube.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &k8sruntime.Unknown{Raw: []byte(log)}, nil
	})
	for token, want := range map[claim.Token]string{
		workerToken: "last lines of tester:\nrunning the suite\n" + containerLinesHeader + "\npanic: runtime error: index out of range [3] with length 3\n\ngoroutine 7 [running]:",
		otherToken:  "last lines of reviewer:\nreading the diff\n" + containerLinesHeader + "\npanic: runtime error: index out of range [3] with length 3\n\ngoroutine 7 [running]:",
	} {
		obs, err := g.r.Probe(g.ctx, rolesLocator(token, recorded))
		if err != nil {
			t.Fatalf("Probe %s: %v", token, err)
		}
		if obs.Kind != runtime.Gone || !strings.HasSuffix(obs.Detail, want) {
			t.Errorf("%s: %s %q, want Gone ending %q", token, obs.Kind, obs.Detail, want)
		}
	}
}

// The stream an issue pod's launcher dials is read from its one container: a pod whose roles
// container dials a stream the daemon moved from is StaleAddress for its role.
func TestTheStreamIsReadFromTheRolesContainer(t *testing.T) {
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	pod := podObject(name, recorded, sandboxUID, labels, corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "roles", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}})
	pod.Spec.Containers = []corev1.Container{{Name: "roles", Image: testImage, Command: []string{"/opt/legion/bin/legion", "launcher", connectFlag, movedStreamURL}}}
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeRunning, labels), pod}, withoutController())
	obs, err := g.r.Probe(g.ctx, rolesLocator(workerToken, recorded))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if want := (movedAddress{connectFlag, movedStreamURL, testOptions().StreamURL}).String(); obs.Kind != runtime.StaleAddress || !strings.Contains(obs.Detail, want) {
		t.Fatalf("%s %q, want StaleAddress naming %q", obs.Kind, obs.Detail, want)
	}
}

// The boot census passes an issue Sandbox whose pod runs the one container `roles`, and one of the
// layout before it, exactly one container per workflow role, which the next launch replaces; every
// other shape it refused it still refuses.
func TestTheBootCensusPassesBothIssuePodLayouts(t *testing.T) {
	issue := claimLabels(claim.RoleArchitect)
	for name, containers := range map[string][]string{
		"one container": {"roles"},
		"the earlier layout, one container per workflow role": workflowContainers(),
	} {
		t.Run(name, func(t *testing.T) {
			sandboxes := newDynamic(t, sandboxRunning(t, "legion-legion-legion-208", issue, containers...)).Resource(sandboxGVR).Namespace(testNamespace)
			if err := rejectLegacyIssueSandboxes(t.Context(), sandboxes, testProject); err != nil {
				t.Fatalf("census = %v, want none", err)
			}
		})
	}
	sandboxes := newDynamic(t, sandboxRunning(t, "legion-legion-legion-208", issue, "roles", "tester")).Resource(sandboxGVR).Namespace(testNamespace)
	if err := rejectLegacyIssueSandboxes(t.Context(), sandboxes, testProject); err == nil || !strings.Contains(err.Error(), "legion-legion-legion-208") {
		t.Fatalf("census of an issue pod running roles and a role container = %v, want it refused by name", err)
	}
}

// A role's namespace puts its own filesystems at /tmp, /var/tmp, /run/lock (also /var/lock) and
// /dev/shm, and covers /var/run/legion/roles whole, carrying back only mounts under the first five:
// an operator's mount at or above one of the five, or at, under or above the roles directory,
// would vanish from every role, so it is refused; one under the five reaches them. role-init
// resolves each path before it covers it, so a mount spelled through one of the worker image's
// links to them (/var/run to /run, /var/lock and /var/run/lock to /run/lock, /run/shm and
// /var/run/shm to /dev/shm, read from the image) lands on what role-init covers and is refused as
// well.
func TestCheckPodRefusesAMountARolesNamespaceHides(t *testing.T) {
	mountAt := func(path string) Pod {
		return Pod{
			Volumes:      []corev1.Volume{{Name: "share", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			VolumeMounts: []corev1.VolumeMount{{Name: "share", MountPath: path}},
		}
	}
	for _, path := range []string{
		"/tmp", "/var/tmp", "/run/lock", "/var/lock", "/dev/shm", "/run", "/dev", "/var/run/legion/roles", "/var/run/legion/roles/tester", "/var/run/legion/roles/.homes",
		"/var/run/lock", "/run/shm", "/var/run/shm", "/var/run", "/run/legion/roles", "/run/legion/roles/tester/gh", "/run/legion/home",
	} {
		if err := CheckPod(mountAt(path), nil, testOptions().Tools, nil, nil, ""); err == nil {
			t.Errorf("a mount at %s passed, want it refused", path)
		}
	}
	for _, path := range []string{
		"/tmp/cache", "/var/tmp/cache", "/run/lock/operator", "/var/lock/operator", "/dev/shm/operator", "/run/shm/operator", "/home/legion/.cache/operator",
		"/run/operator", "/var/run/operator",
	} {
		if err := CheckPod(mountAt(path), nil, testOptions().Tools, nil, nil, ""); err != nil {
			t.Errorf("a mount at %s was refused: %v; role-init carries it back", path, err)
		}
	}
}
