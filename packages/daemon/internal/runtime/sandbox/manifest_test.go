package sandbox

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

var updateGolden = flag.Bool("update", false, "rewrite the manifest goldens this package pins")

// goldenOptions are production's settings: the budgets a golden's grace is read from and the
// Legion priority class, over testOptions' reservations at the daemon's defaults.
func goldenOptions() Options {
	opts := testOptions()
	opts.BootTimeout = 120 * time.Second
	opts.TerminationGrace = 30 * time.Second
	opts.Scheduling = Scheduling{PriorityClass: "legion"}
	return opts
}

// reservations are the daemon's default reservations (config.DefaultResources) as the daemon
// translates them for the runtime (internal/daemon/kubernetes.go, roleRequirements): every role,
// the controller included, its cpu and memory requested as the defaults set them — each equal to
// its limit — and its ephemeral storage the limit over the smaller request.
func reservations() map[claim.Role]corev1.ResourceRequirements {
	translated := map[claim.Role]corev1.ResourceRequirements{}
	for role, reservation := range config.DefaultResources() {
		translated[role] = requirementsOf(reservation)
	}
	return translated
}

// probeReservation is the image probe's own reservation as the daemon hands it
// (internal/daemon/kubernetes.go, probeReservation), translated the same way: 250m and 1Gi, with a
// 5Gi disk bound over a 1Gi request — not a role's share, written here as data as the defaults are.
func probeReservation() corev1.ResourceRequirements {
	return requirementsOf(config.RoleResources{CPU: "250m", CPURequest: "250m", Memory: "1Gi", MemoryRequest: "1Gi", EphemeralStorage: "5Gi", EphemeralStorageRequest: "1Gi"})
}

// requirementsOf is reservation as a container's requirements, translated as roleRequirements
// translates it.
func requirementsOf(reservation config.RoleResources) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(reservation.CPURequest), corev1.ResourceMemory: resource.MustParse(reservation.MemoryRequest),
			corev1.ResourceEphemeralStorage: resource.MustParse(reservation.EphemeralStorageRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(reservation.CPU), corev1.ResourceMemory: resource.MustParse(reservation.Memory),
			corev1.ResourceEphemeralStorage: resource.MustParse(reservation.EphemeralStorage),
		},
	}
}

// podQOS is the QoS class the API server records for a pod of spec (status.qosClass), by the
// kubelet's rule over cpu and memory alone: no request or limit anywhere is BestEffort; every
// container, init containers included, limited in both and the pod's summed requests equal to its
// summed limits is Guaranteed; anything else is Burstable.
func podQOS(spec corev1.PodSpec) corev1.PodQOSClass {
	requests, limits := corev1.ResourceList{}, corev1.ResourceList{}
	guaranteed := true
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		limited := 0
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			if request, ok := c.Resources.Requests[name]; ok && !request.IsZero() {
				sum := requests[name]
				sum.Add(request)
				requests[name] = sum
			}
			if limit, ok := c.Resources.Limits[name]; ok && !limit.IsZero() {
				limited++
				sum := limits[name]
				sum.Add(limit)
				limits[name] = sum
			}
		}
		if limited != 2 {
			guaranteed = false
		}
	}
	if len(requests) == 0 && len(limits) == 0 {
		return corev1.PodQOSBestEffort
	}
	if guaranteed && len(requests) == len(limits) {
		for name, request := range requests {
			if limit, ok := limits[name]; !ok || limit.Cmp(request) != 0 {
				guaranteed = false
			}
		}
		if guaranteed {
			return corev1.PodQOSGuaranteed
		}
	}
	return corev1.PodQOSBurstable
}

// Every container of every pod the runtime builds at the daemon's default reservations reserves cpu
// and memory and bursts past neither: each has a non-zero request for both, equal to its limit,
// which is the kubelet's rule for the Guaranteed class — judged over every container of the pod,
// the init containers included — and each is bounded on the node's disk, an ephemeral-storage
// request under its limit. An issue pod's six launchers each carry their role's reservation and its
// two init containers the launching role's; the controller's pod carries the controller's on both
// its containers; and the image probe's one container carries the probe's own, which the daemon
// hands it. A deployment with no `resources` block therefore runs only Guaranteed pods.
func TestEveryContainerOfEveryPodIsGuaranteed(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := reservations()
	probe := r.probeManifest(probeSandboxName, ImageProbe{Contract: 3, Resources: probeReservation(), RoleReferences: testRoleReferences}, time.Now())
	for name, tc := range map[string]struct {
		pod corev1.PodSpec
		// launch is the reservation every container that is not a role's own carries: the init
		// containers', the role whose launch creates the pod; the probe's one, the probe's own.
		launch     corev1.ResourceRequirements
		containers int
	}{
		"root":       {podOf(t, r, rootSpec(t)), want[claim.RoleArchitect], 8},
		"worker":     {podOf(t, r, workerSpec(t)), want[claim.RoleTester], 8},
		"controller": {podOf(t, r, controllerSpec(t)), want[claim.RoleController], 2},
		"probe":      {probe.Spec.PodTemplate.Spec, probeReservation(), 1},
	} {
		t.Run(name, func(t *testing.T) {
			if qos := podQOS(tc.pod); qos != corev1.PodQOSGuaranteed {
				t.Errorf("the pod is %s, want %s", qos, corev1.PodQOSGuaranteed)
			}
			containers := slices.Concat(tc.pod.InitContainers, tc.pod.Containers)
			if len(containers) != tc.containers {
				t.Fatalf("the pod runs %d containers, want %d", len(containers), tc.containers)
			}
			for _, c := range containers {
				expected, whose := tc.launch, "the launch's"
				if named := claim.Role(c.Name); claim.IsRole(named) || named == claim.RoleController {
					expected, whose = want[named], string(named)+"'s"
				}
				if !reflect.DeepEqual(c.Resources, expected) {
					t.Errorf("container %s carries %+v, want %s reservation %+v", c.Name, c.Resources, whose, expected)
				}
				for _, kind := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
					request, limit := c.Resources.Requests[kind], c.Resources.Limits[kind]
					if request.Sign() <= 0 {
						t.Errorf("container %s requests %s %s, want a positive reservation", c.Name, kind, request.String())
					}
					if request.Cmp(limit) != 0 {
						t.Errorf("container %s requests %s %s but is limited to %s; the pod is Guaranteed only when they are equal", c.Name, kind, request.String(), limit.String())
					}
				}
				if request, limit := c.Resources.Requests[corev1.ResourceEphemeralStorage], c.Resources.Limits[corev1.ResourceEphemeralStorage]; request.Sign() <= 0 || request.Cmp(limit) > 0 {
					t.Errorf("container %s requests ephemeral-storage %s under a limit of %s, want a positive request no greater than the limit", c.Name, request.String(), limit.String())
				}
			}
		})
	}
}

// A role whose reservation requests less memory than its limit — the implementer at 8Gi under
// 32Gi — runs its launcher container with exactly that request and limit, and every issue pod it
// is a container of is Burstable, whichever role's launch created it; the init containers carry the
// launching role's reservation as before: the root's, created by the architect's launch, the
// architect's default Guaranteed 250m and 1Gi, and a pod the implementer's launch creates the
// implementer's 8Gi request and 32Gi limit. The controller's pod, which holds no implementer, and
// the image probe's, sized by the probe's own reservation, stay Guaranteed.
func TestARequestBelowItsLimitRendersABurstablePod(t *testing.T) {
	opts := goldenOptions()
	bursting := config.RoleResources{CPU: "750m", CPURequest: "750m", Memory: "32Gi", MemoryRequest: "8Gi", EphemeralStorage: "20Gi", EphemeralStorageRequest: "1Gi"}
	opts.Resources[claim.RoleImplementer] = requirementsOf(bursting)
	r, err := configure(opts)
	if err != nil {
		t.Fatalf("configure with the implementer requesting 8Gi under a 32Gi limit: %v", err)
	}
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m"), corev1.ResourceMemory: resource.MustParse("8Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m"), corev1.ResourceMemory: resource.MustParse("32Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("20Gi")},
	}
	architect := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("10Gi")},
	}
	for name, tc := range map[string]struct {
		pod  corev1.PodSpec
		init corev1.ResourceRequirements
	}{
		"root":        {podOf(t, r, rootSpec(t)), architect},
		"implementer": {podOf(t, r, childSpec(t)), want},
	} {
		t.Run(name, func(t *testing.T) {
			if got := containerNamed(t, tc.pod, string(claim.RoleImplementer)).Resources; !reflect.DeepEqual(got, want) {
				t.Errorf("the implementer's container carries %+v, want %+v", got, want)
			}
			if qos := podQOS(tc.pod); qos != corev1.PodQOSBurstable {
				t.Errorf("the issue pod is %s, want %s", qos, corev1.PodQOSBurstable)
			}
			if len(tc.pod.InitContainers) == 0 {
				t.Fatal("the issue pod has no init containers")
			}
			for _, c := range tc.pod.InitContainers {
				if !reflect.DeepEqual(c.Resources, tc.init) {
					t.Errorf("init container %s carries %+v, want the launching role's %+v", c.Name, c.Resources, tc.init)
				}
			}
		})
	}
	if qos := podQOS(podOf(t, r, controllerSpec(t))); qos != corev1.PodQOSGuaranteed {
		t.Errorf("the controller's pod is %s, want %s", qos, corev1.PodQOSGuaranteed)
	}
	probe := r.probeManifest(probeSandboxName, ImageProbe{Contract: 3, Resources: probeReservation(), RoleReferences: testRoleReferences}, time.Now())
	if qos := podQOS(probe.Spec.PodTemplate.Spec); qos != corev1.PodQOSGuaranteed {
		t.Errorf("the probe pod is %s, want %s", qos, corev1.PodQOSGuaranteed)
	}
}

// resumeSession is a session file Oh My Pi recorded in a pod.
const resumeSession = ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/2026-09-23T12-00-00-000Z_0198.jsonl"

// manifestCases are the Sandboxes the goldens pin: the root; a worker; a resume; a relaunch whose
// workspace is recovered after its volume was lost; the root enrolled with the secrets broker; and
// the project controller's pod, launched fresh and resuming. Every one owns its volume.
// agentSecrets, set for root-enrolled alone, is the runtime's enrollment for that one case
// (TestManifestGoldens, TestManifestMatchesTheSandboxCRD apply it to the shared runtime before
// building that case's manifest, and restore nil after — every other case runs unenrolled).
func manifestCases(t *testing.T) map[string]struct {
	spec         runtime.SpawnSpec
	agentSecrets *AgentSecrets
} {
	resume := workerSpec(t)
	resume.Generation, resume.BootToken, resume.ResumeSessionFile = 2, "boot-g2", resumeSession
	recovered := workerSpec(t)
	recovered.WorkspaceRecoveredFrom = "legion/LEGION-208"
	controllerResume := controllerSpec(t)
	controllerResume.Generation, controllerResume.BootToken = 2, "boot-g2"
	controllerResume.ResumeSessionFile = controllerSession
	return map[string]struct {
		spec         runtime.SpawnSpec
		agentSecrets *AgentSecrets
	}{
		"root":              {rootSpec(t), nil},
		"worker":            {workerSpec(t), nil},
		"resume":            {resume, nil},
		"recovered":         {recovered, nil},
		"controller":        {controllerSpec(t), nil},
		"controller-resume": {controllerResume, nil},
		"root-enrolled": {rootSpec(t), &AgentSecrets{
			URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour,
		}},
	}
}

// manifestOf is the Sandbox a launch of spec leaves running, as the API server holds it: the one
// the runtime creates, with the relaunch's Running patch applied — the launch's pod template, and
// the Running mode.
func manifestOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec) any {
	t.Helper()
	l := launchOf(t, r, spec)
	s := r.sandboxManifest(l)
	s.Spec.PodTemplate, s.Spec.OperatingMode = r.podTemplate(l), modeRunning
	u, err := encodeSandbox(s)
	if err != nil {
		t.Fatal(err)
	}
	return wire(t, u.Object)
}

// The goldens pin every byte of the Sandboxes a launch creates. A pod contract change shows
// here as a diff to read, never as a surprise in a cluster.
func TestManifestGoldens(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			r.agentSecrets = tc.agentSecrets
			var buffer bytes.Buffer
			encoder := json.NewEncoder(&buffer)
			encoder.SetEscapeHTML(false)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(manifestOf(t, r, tc.spec)); err != nil {
				t.Fatal(err)
			}
			encoded := buffer.Bytes()
			path := filepath.Join("testdata", "golden", name+".json")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, encoded, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run `go test ./internal/runtime/sandbox/ -update`): %v", err)
			}
			if !bytes.Equal(encoded, want) {
				t.Fatalf("%s is stale\n got: %s\nwant: %s", path, encoded, want)
			}
		})
	}
}

// Every issue pod and the project controller's pod carry the two stop-eviction annotations
// unconditionally (the image probe carries only Karpenter's,
// TestTheProbePodCarriesNoGKEExtendedDurationAnnotation): Karpenter's own, which protects a pod on
// an AWS cluster, and the Cluster Autoscaler's, which GKE Autopilot honours; GKE ignores
// Karpenter's, so a pod on Autopilot without the second can be evicted mid-turn by consolidation or
// a node upgrade.
func TestEveryIssueAndControllerPodIsProtectedFromAutoscalerEviction(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		doNotDisruptAnnotation: "true",
		safeToEvictAnnotation:  "false",
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			r.agentSecrets = tc.agentSecrets
			l, err := r.prepare(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.podTemplate(l).Metadata.Annotations; !maps.Equal(got, want) {
				t.Errorf("pod annotations = %v, want exactly %v", got, want)
			}
		})
	}
}

// Every field of every manifest, podTemplate.spec included, is one the v1.0.3 CRD declares with
// that type: the API server prunes an unknown field without a word, so this walk is the only place
// a misspelled or misplaced one shows (decision 9, N18).
func TestManifestMatchesTheSandboxCRD(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	schema := sandboxSchema(t)
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			r.agentSecrets = tc.agentSecrets
			if found := schemaViolations(manifestOf(t, r, tc.spec), schema, ""); len(found) > 0 {
				t.Fatalf("the manifest has fields the CRD does not declare as sent:\n%s", strings.Join(found, "\n"))
			}
		})
	}
}

// What the operator adds to a pod — a Secret, a ConfigMap by sub_path, a projected token, a
// variable, an account, and the providers Secret's keys — is sent in fields the CRD declares, in
// the worker's Sandbox and the probe's alike, so the API server keeps every one of them.
func TestTheOperatorsPodIsSentInFieldsTheSandboxCRDDeclares(t *testing.T) {
	opts := goldenOptions()
	opts.Pod = operatorPod()
	opts.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "anthropic"}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := encodeProbe(r.probeManifest("legion-probe", ImageProbe{Contract: 5, RoleReferences: testRoleReferences}, time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	schema := sandboxSchema(t)
	for name, manifest := range map[string]any{"worker": manifestOf(t, r, workerSpec(t)), "probe": wire(t, probe.Object)} {
		if found := schemaViolations(manifest, schema, ""); len(found) > 0 {
			t.Errorf("the %s manifest has fields the CRD does not declare as sent:\n%s", name, strings.Join(found, "\n"))
		}
	}
}

// podOf is the pod template a launch of spec runs.
func podOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec) corev1.PodSpec {
	t.Helper()
	l := launchOf(t, r, spec)
	return r.podTemplate(l).Spec
}

// workerContainer is the role container of workerSpec, the tester.
const workerContainer = string(claim.RoleTester)

// workerOf is the worker-shim process a launch of spec starts: its role container as the pod runs
// it (mounts, kubelet-resolved environment), with the launcher start command's argv as Command and
// its plain environment appended. That is the process's whole environment: the launcher passes its
// own on, then the start command's.
func workerOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec) corev1.Container {
	t.Helper()
	l := launchOf(t, r, spec)
	c := containerNamed(t, r.podTemplate(l).Spec, string(spec.Role))
	start := launcherCommand(l, r)
	c.Command = start.Argv
	for _, entry := range start.Env {
		name, value, _ := strings.Cut(entry, "=")
		c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: value})
	}
	return c
}

func envOf(container corev1.Container) map[string]string {
	env := map[string]string{}
	for _, v := range container.Env {
		env[v.Name] = v.Value
	}
	return env
}

// containerNamed is the pod's init or main container of that name.
func containerNamed(t *testing.T, pod corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the pod has no container %s", name)
	return corev1.Container{}
}

// mountHolding is the deepest of c's mounts at or above path, the one path lands on, or nil when
// none holds it.
func mountHolding(c corev1.Container, path string) *corev1.VolumeMount {
	var holding *corev1.VolumeMount
	for i, m := range c.VolumeMounts {
		if (path == m.MountPath || strings.HasPrefix(path, m.MountPath+"/")) && (holding == nil || len(m.MountPath) > len(holding.MountPath)) {
			holding = &c.VolumeMounts[i]
		}
	}
	return holding
}

// PI_SHELL_PREFIX is a shell command Oh My Pi's bash tool runs before each command, in tmux's form
// over the pod's one directory of its own — legion's in the image; no directory of the tree volume,
// since the agent's gh is the image's — never a path list (P3).
func TestPIShellPrefixIsTmuxsFormOverThePodsDirectories(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(workerOf(t, r, workerSpec(t)))
	got := kubeExpand(env["PI_SHELL_PREFIX"], env)
	want := shellprefix.For("/opt/legion/bin")
	if got != want {
		t.Fatalf("PI_SHELL_PREFIX\n got: %s\nwant: %s", got, want)
	}
}

// Every git a role process runs answers https://github.com from the role's own helper, outside
// any clone too — a clone the setup command makes, Oh My Pi's marketplace clone among them: the
// GIT_CONFIG pairs the role is told reset every helper a configuration file names for github.com
// (a planted global one here) and set `legion git-credential` with the repository's path, whose
// owner picks the token. Another scheme, host or port is left to the configuration files. A real
// git runs `git credential fill` in a directory that is no repository, with a stand-in `legion`
// first on PATH, for a workflow role and the controller alike.
func TestEveryRolesGitAsksItsOwnHelperForGitHubOutsideAnyClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git in this environment")
	}
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The stand-in answers `git-credential get` with a token naming the owner of git's path, and
	// nothing without a path, as `legion git-credential` does (cmd/legion/gitcredential.go).
	legion := "#!/bin/sh\n[ \"$1 $2\" = 'git-credential get' ] || exit 0\n" +
		"while read -r line; do case $line in path=*) p=${line#path=} ;; esac; done\n" +
		"[ -n \"$p\" ] || exit 1\necho username=x-access-token\necho \"password=role-token-for-${p%%/*}\"\n"
	if err := os.WriteFile(filepath.Join(bin, "legion"), []byte(legion), 0o700); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = \"!f() { echo username=planted; echo password=planted; }; f\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "no-repository")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]runtime.SpawnSpec{"a workflow role": workerSpec(t), "the controller": controllerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			var gitEnv []string
			for name, value := range envOf(workerOf(t, r, spec)) {
				if strings.HasPrefix(name, "GIT_CONFIG_") {
					gitEnv = append(gitEnv, name+"="+value)
				}
			}
			if len(gitEnv) == 0 {
				t.Fatal("the role is told no GIT_CONFIG pairs")
			}
			for _, tc := range []struct{ url, want string }{
				{"https://github.com/acme/agent-skills.git", "role-token-for-acme"},
				{"https://github.com/sjawhar/legion-smoke", "role-token-for-sjawhar"},
				{"http://github.com/acme/agent-skills.git", "planted"},
				{"https://github.com:8443/acme/agent-skills.git", "planted"},
				{"https://github.com.evil.example/acme/agent-skills.git", "planted"},
				{"https://evil.example/acme/agent-skills.git", "planted"},
			} {
				fill := exec.Command("git", "credential", "fill")
				fill.Dir = outside
				fill.Stdin = strings.NewReader("url=" + tc.url + "\n\n")
				for _, entry := range os.Environ() {
					if !strings.HasPrefix(entry, "GIT_") && !strings.HasPrefix(entry, "PATH=") {
						fill.Env = append(fill.Env, entry)
					}
				}
				fill.Env = append(fill.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
					"GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_NOSYSTEM=1", "GIT_CEILING_DIRECTORIES="+dir, "GIT_TERMINAL_PROMPT=0")
				fill.Env = append(fill.Env, gitEnv...)
				out, err := fill.Output()
				got := ""
				for _, line := range strings.Split(string(out), "\n") {
					if value, ok := strings.CutPrefix(line, "password="); ok {
						got = value
					}
				}
				if err != nil || got != tc.want {
					t.Errorf("%s: password %q (%v), want %q", tc.url, got, err, tc.want)
				}
			}
		})
	}
}

// The init containers run where the provisioning Secret is mounted, or on the issue volume every
// agent of the issue can write, so nothing either executes may come from the issue volume: each
// one's PATH names the image's directories only, and neither is told a tool path (#1258 deep
// review, finding 3).
func TestTheInitContainersPathNamesNoIssueVolumeDirectory(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, init := range podOf(t, r, workerSpec(t)).InitContainers {
		env := envOf(init)
		path, ok := env["PATH"]
		if !ok {
			t.Fatalf("%s's PATH is left to the image; it must be stated", init.Name)
		}
		for _, dir := range filepath.SplitList(path) {
			if dir == TreeRoot || strings.HasPrefix(dir, TreeRoot+"/") {
				t.Errorf("%s's PATH names %s, on the issue volume", init.Name, dir)
			}
		}
		for name := range env {
			if strings.HasPrefix(name, "LEGION_") && strings.HasSuffix(name, "_PATH") {
				t.Errorf("%s is told %s; it resolves its tools from the image's PATH", init.Name, name)
			}
		}
	}
}

// jj keeps a repository's `--repo` configuration under $XDG_CONFIG_HOME, so what workspace-init
// sets there reaches the agent's jj only when both containers have one config home, on a volume
// both mount (#1258 deep review, finding 4). It is in memory, so every pod's starts empty and
// nothing an agent wrote reaches workspace-init's jj. The config home is the one home the two
// agree on: a role container's cache and data homes are the issue's scratch's on the issue's
// volume and its state home its own directory there
// (TestTheIssuesRolesShareOneScratchAndOneGoCachePair,
// TestEachRolesAgentIsToldAStateHomeOfItsOwn), while the setup containers keep the image's home
// directory ones, so nothing of the issue's scratch reaches a container that holds the
// provisioning token or provisions the issue's clone.
func TestBothContainersShareOneInMemoryXDGConfigHome(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	pod := podOf(t, r, workerSpec(t))
	init, main := containerNamed(t, pod, initContainer), workerOf(t, r, workerSpec(t))
	configHome := envOf(main)["XDG_CONFIG_HOME"]
	if configHome == "" || configHome != envOf(init)["XDG_CONFIG_HOME"] {
		t.Errorf("XDG_CONFIG_HOME: init %q, main %q; the two containers must agree",
			envOf(init)["XDG_CONFIG_HOME"], configHome)
	}
	for name, want := range map[string]string{
		"XDG_CACHE_HOME": podHome + "/.cache",
		"XDG_DATA_HOME":  podHome + "/.local/share",
		"XDG_STATE_HOME": podHome + "/.local/state",
	} {
		if got := envOf(init)[name]; got != want {
			t.Errorf("%s is told %s=%q, want the image's home directory's %q", initContainer, name, got, want)
		}
	}
	for _, setup := range pod.InitContainers {
		for _, v := range setup.Env {
			if v.Value == scratchRoot || strings.HasPrefix(v.Value, scratchRoot+"/") {
				t.Errorf("%s is told %s=%q, under the roles' scratch; a setup container keeps nothing there",
					setup.Name, v.Name, v.Value)
			}
		}
	}
	volumeAt := func(c corev1.Container) string {
		for _, mount := range c.VolumeMounts {
			if mount.MountPath == configHome {
				return mount.Name
			}
		}
		return ""
	}
	shared := volumeAt(init)
	if shared == "" || shared != volumeAt(main) {
		t.Fatalf("XDG_CONFIG_HOME %s is mounted from %q in the init container and %q in the main one", configHome, shared, volumeAt(main))
	}
	for _, volume := range pod.Volumes {
		if volume.Name == shared && (volume.EmptyDir == nil || volume.EmptyDir.Medium != corev1.StorageMediumMemory) {
			t.Fatalf("the config home's volume %q is not an in-memory emptyDir: %+v", shared, volume.VolumeSource)
		}
	}
}

// The role containers of one issue pod share the pod's network namespace, the workspace path, the
// Oh My Pi profile path and the rest of the issue's scratch, and Oh My Pi's browser broker lock is
// an abstract unix socket named from the lock path under its state root: one state root would
// make it one name in every container, the first role's lock blocking every other role's broker.
// So each role's agent is told a XDG_STATE_HOME of its own, its own directory under the scratch's
// `state` (`/legion/scratch/state/<role>`), differing from every other role's, and the
// controller's agent one by the same rule; workspace-init, which runs alone, keeps the plain
// home-directory state home. The value is on the issue's volume, deliberately: Oh My Pi's logs,
// Chromium's profiles and the installed plugin generation are charged to neither the pod's memory
// nor the gigabyte of ephemeral storage the container is admitted with, and they outlive the pod.
// Which mount holds it is the argument above, not housekeeping: a state home reached through a
// sub-path mount, or one overlapping the workspace or uv's directories, is a directory whose
// contents are no longer the role's alone, and the broker-lock reasoning quietly stops holding.
// So each role's value is checked to sit on the issue volume's own mount and to overlap none of
// those three, exactly as the shared six are
// (TestTheIssuesRolesShareOneScratchAndOneGoCachePair).
func TestEachRolesAgentIsToldAStateHomeOfItsOwn(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	told := map[string]claim.Role{}
	for _, role := range launcherRoles {
		worker := workerOf(t, r, roleSpec(t, role))
		env := envOf(worker)
		got := env["XDG_STATE_HOME"]
		if want := TreeRoot + "/scratch/state/" + string(role); got != want {
			t.Errorf("%s's agent is told XDG_STATE_HOME=%q, want %q", role, got, want)
		}
		if mount := mountHolding(worker, got); mount == nil || mount.Name != issueVolume || mount.SubPath != "" {
			t.Errorf("%s: XDG_STATE_HOME %q is on mount %+v, want the issue volume's own mount at %s", role, got, mount, TreeRoot)
		}
		for _, theirs := range []string{"LEGION_WORKSPACE", "UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR"} {
			if path := env[theirs]; path != "" && overlaps(got, path) {
				t.Errorf("%s: XDG_STATE_HOME %q overlaps %s %q", role, got, theirs, path)
			}
		}
		if other, taken := told[got]; taken {
			t.Errorf("%s's agent is told XDG_STATE_HOME=%q, %s's too", role, got, other)
		}
		told[got] = role
	}
	init := containerNamed(t, podOf(t, r, workerSpec(t)), initContainer)
	if got := envOf(init)["XDG_STATE_HOME"]; got != xdgStateHome {
		t.Errorf("workspace-init is told XDG_STATE_HOME=%q, want %q", got, xdgStateHome)
	}
}

// roleSpec is the spawn spec of role in the test tree's root issue: the controller's own, or a
// workflow role's claim of that issue.
func roleSpec(t *testing.T, role claim.Role) runtime.SpawnSpec {
	t.Helper()
	if role == claim.RoleController {
		return controllerSpec(t)
	}
	token, err := claim.NewToken("legion", testTree, role)
	if err != nil {
		t.Fatal(err)
	}
	return testSpec(t, token, role, testTree)
}

// The six directories a role's tools write to without being told where are the issue's, not the
// role's: every role container of a workflow pod and the controller's is told the same temporary
// directory, cache home, data home and bun install cache — the four directly under the issue's
// scratch on its volume — and the same Go build and module cache pair beside the workspaces. A
// directory per role is what this replaces, by the human's decision of 2026-10-20: disk is cheap,
// an issue gets the space it needs, and the roles within a pod are not kept from sharing a cache.
// Splitting them bought no isolation the pod has anyway — the roles of an issue share the pod,
// the volume whole, the one checkout they all work in (workspace.Location names the repository
// and the issue, never a role), the sessions directory and uv's Pythons and cache, and each can
// read and write what the others write — while costing a copy of every cache per role. Bun
// hardlinks out of its install cache into whatever it installs for (LEGION-198, where that
// hardlinking was first met), so a file edited in place in node_modules is already in the
// packages every role runs from, whatever we do about caches; the one install Legion renders
// itself copies rather than links (plugins.go). The two Go caches hold content addressed by its
// hash and the roles of one issue may fill them at once: of the build cache `go help cache` says
// "The cache is safe for concurrent invocations of the go command", and what reaches the module
// cache is a module zip or go.mod the go command has verified against a known hash (`go help
// module-auth`) in a directory it then makes read-only, which only `go build -modcacherw` leaves
// writable; a pair is 751 MiB of build cache and 375 MiB of module cache for this repository, so
// six roles with a pair each would hold five redundant copies, about 5.5 GiB spent on nothing.
// Each directory is held by the issue volume's own mount, exactly as uv's are
// (TestUvKeepsItsPythonsAndCacheOnTheIssueVolume), and overlaps neither the workspace nor uv's
// directories, and no two of the six are one directory. The state home is the one thing of the
// scratch that stays the role's own, for the browser broker's lock
// (TestEachRolesAgentIsToldAStateHomeOfItsOwn).
func TestTheIssuesRolesShareOneScratchAndOneGoCachePair(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	shared := [][2]string{
		{"TMPDIR", TreeRoot + "/scratch/tmp"},
		{"XDG_CACHE_HOME", TreeRoot + "/scratch/cache"},
		{"XDG_DATA_HOME", TreeRoot + "/scratch/data"},
		{"BUN_INSTALL_CACHE_DIR", TreeRoot + "/scratch/bun/install/cache"},
		{"GOCACHE", TreeRoot + "/go/build"},
		{"GOMODCACHE", TreeRoot + "/go/mod"},
	}
	for _, role := range launcherRoles {
		main := workerOf(t, r, roleSpec(t, role))
		env := envOf(main)
		told := map[string]string{}
		for _, pair := range shared {
			name, want := pair[0], pair[1]
			dir := env[name]
			if dir != want {
				t.Errorf("%s is told %s=%q, want %q, the one directory every role of the issue is told", role, name, dir, want)
				continue
			}
			if mount := mountHolding(main, dir); mount == nil || mount.Name != issueVolume || mount.SubPath != "" {
				t.Errorf("%s: %s %q is on mount %+v, want the issue volume's own mount at %s", role, name, dir, mount, TreeRoot)
			}
			for _, theirs := range []string{"LEGION_WORKSPACE", "UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR"} {
				if path := env[theirs]; path != "" && overlaps(dir, path) {
					t.Errorf("%s: %s %q overlaps %s %q", role, name, dir, theirs, path)
				}
			}
			if other, taken := told[dir]; taken {
				t.Errorf("%s is told %s=%q and %s=%q; these are six directories, and the go command keeps its two caches apart", role, other, dir, name, dir)
			}
			told[dir] = name
		}
	}
}

// Every directory the runtime tells a role to write in is one the shim makes, and every directory
// the shim makes is one the runtime names. The shim reads its list from podsafety.ScratchVariables
// and the runtime renders roleEnvironment, so without this the two could drift silently: a role
// would be told a temporary directory nothing ever created, and mktemp and every library
// equivalent would fail in its first turn, which is the failure EnsureScratch exists to prevent.
// So each name on the shim's list, plus XDG_STATE_HOME, which the shim makes elsewhere
// (podsafety.EnsureStateHome, which makes Oh My Pi's profile directory under it), is set to an
// absolute path on the issue's volume — the issue's shared scratch for four of them, the issue's
// Go caches for two, and the role's own state home for the last — and, the other way, every
// variable the runtime sets to a path on that volume is on that list, bar the three it names for
// a reason of their own, so neither a new directory of the issue's scratch nor a new per-role one
// can appear that nothing makes.
func TestTheRuntimeTellsTheShimEveryScratchDirectoryItMakes(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(workerOf(t, r, workerSpec(t)))
	made := append(slices.Clone(podsafety.ScratchVariables), "XDG_STATE_HOME")
	for _, name := range made {
		if dir := env[name]; !strings.HasPrefix(dir, TreeRoot+"/") {
			t.Errorf("the shim makes %s, which the runtime sets to %q; want an absolute path under %s", name, dir, TreeRoot)
		}
	}
	// The runtime names these three on the issue's volume for a reason of their own and does not
	// ask the shim to make them: LEGION_WORKSPACE is the issue's checkout, which workspace-init
	// provisions before any role container starts, and UV_PYTHON_INSTALL_DIR and UV_CACHE_DIR are
	// uv's own, which uv makes and the tree's pods share by design
	// (TestUvKeepsItsPythonsAndCacheOnTheIssueVolume). Every other path the runtime names there is
	// a directory it tells the role to write in, so a fourth exemption has to be argued for rather
	// than slipped in.
	theirOwn := []string{"LEGION_WORKSPACE", "UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR"}
	for name, value := range env {
		if !strings.HasPrefix(value, TreeRoot+"/") || slices.Contains(made, name) || slices.Contains(theirOwn, name) {
			continue
		}
		t.Errorf("the runtime tells the role %s=%q on the issue's volume; the shim makes %v and EnsureStateHome the state home, so nothing would make it",
			name, value, podsafety.ScratchVariables)
	}
}

// uv links a project's .venv, in an issue's workspace on the issue's volume, to an interpreter
// under UV_PYTHON_INSTALL_DIR, and installs into it from UV_CACHE_DIR. Both are on the volume's own
// mount (no other mount covering them) and outside the workspace, whose tree is the project's, so a
// later pod runs the .venv as it is and reuses what an earlier pod downloaded. The cache is at the
// same path in every pod. The interpreter directory is the issue's: the same for two pods of one
// issue, and another for a second issue of the tree.
func TestUvKeepsItsPythonsAndCacheOnTheIssueVolume(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	child, err := claim.NewToken("legion", "LEGION-209", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	var envs []map[string]string
	for _, spec := range []runtime.SpawnSpec{rootSpec(t), workerSpec(t), testSpec(t, child, claim.RoleImplementer, "LEGION-209")} {
		main := workerOf(t, r, spec)
		env := envOf(main)
		for _, name := range []string{"UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR"} {
			dir := env[name]
			if mount := mountHolding(main, dir); mount == nil || mount.Name != issueVolume || mount.SubPath != "" {
				t.Errorf("%s %s: %s %q is on mount %+v, want the issue volume's own mount at %s", spec.Issue, spec.Role, name, dir, mount, TreeRoot)
			}
			if overlaps(dir, env["LEGION_WORKSPACE"]) {
				t.Errorf("%s %s: %s %q overlaps the workspace %s", spec.Issue, spec.Role, name, dir, env["LEGION_WORKSPACE"])
			}
		}
		envs = append(envs, env)
	}
	architect, tester, other := envs[0], envs[1], envs[2]
	if architect["UV_PYTHON_INSTALL_DIR"] != tester["UV_PYTHON_INSTALL_DIR"] {
		t.Errorf("two pods of one issue install Pythons into %q and %q; the later would not find the earlier's interpreter",
			architect["UV_PYTHON_INSTALL_DIR"], tester["UV_PYTHON_INSTALL_DIR"])
	}
	if other["UV_PYTHON_INSTALL_DIR"] == architect["UV_PYTHON_INSTALL_DIR"] {
		t.Errorf("two issues of one tree share the Python directory %q", other["UV_PYTHON_INSTALL_DIR"])
	}
	if architect["UV_CACHE_DIR"] != tester["UV_CACHE_DIR"] || other["UV_CACHE_DIR"] != architect["UV_CACHE_DIR"] {
		t.Errorf("the tree's pods use the caches %q, %q and %q; want one", architect["UV_CACHE_DIR"], tester["UV_CACHE_DIR"], other["UV_CACHE_DIR"])
	}
}

// A pod's own IP changes on every restart, so a daemon that runs as a pod hands pods a stable
// address instead, a Kubernetes Service's DNS name: StreamURL is advertise_host at the worker
// stream's port (shimAddress, internal/daemon/daemon.go) and DaemonURL is daemon_url. Neither
// Options field needs an IP: the manifest carries each exactly as configured, on every role
// launcher's --connect and in the worker process the start command runs.
func TestTheManifestCarriesADNSNamedStreamAndDaemonURL(t *testing.T) {
	opts := goldenOptions()
	opts.StreamURL = "tcp://legion-daemon-widgets.legion.svc:13371"
	opts.DaemonURL = "http://legion-daemon-widgets.legion.svc:13370"
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range podOf(t, r, workerSpec(t)).Containers {
		if !slices.Contains(c.Command, "tcp://legion-daemon-widgets.legion.svc:13371") {
			t.Errorf("the %s launcher's argv = %v, want it to dial tcp://legion-daemon-widgets.legion.svc:13371", c.Name, c.Command)
		}
	}
	worker := workerOf(t, r, workerSpec(t))
	if !slices.Contains(worker.Command, "tcp://legion-daemon-widgets.legion.svc:13371") {
		t.Errorf("the shim's argv = %v, want it to dial tcp://legion-daemon-widgets.legion.svc:13371", worker.Command)
	}
	if got := envOf(worker)["LEGION_DAEMON_URL"]; got != "http://legion-daemon-widgets.legion.svc:13370" {
		t.Errorf("LEGION_DAEMON_URL = %q, want http://legion-daemon-widgets.legion.svc:13370", got)
	}
}

// The provisioning token never shares a process with anything a tree agent can write (Stage 4b
// Task 4b.6b): the claim's Secret projects it into workspace-fetch alone, the only container told
// where it is, and no other container can write a volume workspace-fetch mounts — the feed it
// fills is read-only in workspace-init, and its TMPDIR, where its one-shot credential goes, is an
// in-memory volume no other container mounts. So workspace-fetch mounts neither the tree volume
// nor the config home. Every pod's manifest holds to it, by every route Kubernetes offers into a
// Secret, the worker's container included; the controller's pod has no workspace-fetch, so none of
// its containers reaches the token at all.
//
// What the boundary protects is identity, not a repository-write token kept off the tree volume:
// an implement-role container now holds the implement App's token — the same lease as the
// provisioning token — read-only in its own gh volume (GHConfigDir), and a review-role container
// the review App's. What the fetch container's boundary still keeps is that no review-role
// container ever holds the implement App's token, which is what lets the reviewer's approval
// count and the daemon's fix-attempt counting read a push's App from the App that made it.
func TestTheProvisionTokenSharesNoContainerWithAnythingTheTreeCanWrite(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			l, err := r.prepare(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			template := r.podTemplate(l)
			pod := template.Spec
			claimSecret := secretName(l.name)
			volumes := map[string]corev1.Volume{}
			for _, volume := range pod.Volumes {
				volumes[volume.Name] = volume
			}
			// A Secret source that maps no keys maps every key.
			maps := func(items []corev1.KeyToPath) bool {
				return len(items) == 0 || slices.ContainsFunc(items, func(item corev1.KeyToPath) bool { return item.Key == provisionTokenKey })
			}
			reaches := func(c corev1.Container) []string {
				var routes []string
				for _, mount := range c.VolumeMounts {
					volume := volumes[mount.Name]
					if secret := volume.Secret; secret != nil && secret.SecretName == claimSecret && maps(secret.Items) {
						routes = append(routes, "secret volume "+mount.Name)
					}
					if projected := volume.Projected; projected != nil {
						for _, source := range projected.Sources {
							if secret := source.Secret; secret != nil && secret.Name == claimSecret && maps(secret.Items) {
								routes = append(routes, "projected volume "+mount.Name)
							}
						}
					}
				}
				for _, v := range c.Env {
					if from := v.ValueFrom; from != nil && from.SecretKeyRef != nil && from.SecretKeyRef.Name == claimSecret && from.SecretKeyRef.Key == provisionTokenKey {
						routes = append(routes, "env "+v.Name)
					}
				}
				for _, from := range c.EnvFrom {
					if from.SecretRef != nil && from.SecretRef.Name == claimSecret {
						routes = append(routes, "envFrom")
					}
				}
				return routes
			}
			containers := slices.Concat(pod.InitContainers, pod.Containers)
			for _, c := range containers {
				_, pointed := envOf(c)["LEGION_PROVISION_TOKEN_FILE"]
				routes := reaches(c)
				if holds := c.Name == fetchContainer; (len(routes) > 0) != holds || pointed != holds {
					t.Errorf("%s reaches the provisioning token by %v, pointed at %t; want %t for both", c.Name, routes, pointed, holds)
				}
			}
			kind, err := podKindOf(template.Metadata.Labels)
			if err != nil {
				t.Fatal(err)
			}
			if kind == (controllerPod{}) {
				if slices.ContainsFunc(containers, func(c corev1.Container) bool { return c.Name == fetchContainer }) {
					t.Errorf("the controller's pod runs %s", fetchContainer)
				}
				return
			}
			// No other container may write what workspace-fetch mounts: the feed, a subPath of the issue
			// volume, is reachable through another container's read-write mount of the volume (or of a
			// subPath above the feed) unless that container mounts the feed read-only over exactly the
			// path the read-write mount reaches it at.
			fetch := containerNamed(t, pod, fetchContainer)
			for _, mount := range fetch.VolumeMounts {
				for _, other := range containers {
					if other.Name == fetch.Name {
						continue
					}
					for _, theirs := range other.VolumeMounts {
						if theirs.Name != mount.Name || theirs.ReadOnly {
							continue
						}
						below, ok := strings.CutPrefix(mount.SubPath, theirs.SubPath)
						if !ok || (theirs.SubPath != "" && below != "" && !strings.HasPrefix(below, "/")) {
							continue
						}
						reached := path.Join(theirs.MountPath, below)
						shadowed := slices.ContainsFunc(other.VolumeMounts, func(m corev1.VolumeMount) bool {
							return m.Name == mount.Name && m.ReadOnly && m.SubPath == mount.SubPath && m.MountPath == reached
						})
						if !shadowed {
							t.Errorf("%s can write volume %s at %s, which %s mounts at %s", other.Name, mount.Name, reached, fetch.Name, mount.MountPath)
						}
					}
				}
			}
			temp := envOf(fetch)["TMPDIR"]
			in := func(c corev1.Container, path string) string {
				for _, mount := range c.VolumeMounts {
					if mount.MountPath == path {
						return mount.Name
					}
				}
				return ""
			}
			if dir := volumes[in(fetch, temp)].EmptyDir; temp == "" || dir == nil || dir.Medium != corev1.StorageMediumMemory {
				t.Errorf("%s's TMPDIR %q is not an in-memory volume of its own", fetch.Name, temp)
			}
			if feed := in(containerNamed(t, pod, initContainer), FeedDir); feed == "" || feed != in(fetch, FeedDir) {
				t.Errorf("workspace-init reads the feed from %q, and %s fills %q", feed, fetch.Name, in(fetch, FeedDir))
			}
		})
	}
}

// A launch the runtime cannot honour exactly is refused before any API call.
func TestALaunchItCannotHonourIsRefused(t *testing.T) {
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		edit func(*runtime.SpawnSpec)
		want string
	}{
		"no repository":       {func(s *runtime.SpawnSpec) { s.Repository = ghrepo.Repository{} }, "no repository"},
		"no default branch":   {func(s *runtime.SpawnSpec) { s.DefaultBranch = "" }, "no default branch for repository sjawhar/legion-smoke"},
		"session off volume":  {func(s *runtime.SpawnSpec) { s.ResumeSessionFile = "/home/legion/elsewhere.jsonl" }, "cannot be resumed on this runtime"},
		"runtime-owned env":   {func(s *runtime.SpawnSpec) { s.Env["PATH"] = "/bin" }, "Env sets PATH"},
		"credential in env":   {func(s *runtime.SpawnSpec) { s.Env["ANTHROPIC_API_KEY"] = "x" }, "credential-shaped"},
		"gh token in env":     {func(s *runtime.SpawnSpec) { s.Env["GH_TOKEN"] = "x" }, "Env sets GH_TOKEN"},
		"boot token secret":   {func(s *runtime.SpawnSpec) { s.Secrets[bootTokenKey] = "x" }, "is a variable the runtime sets itself"},
		"provisioning secret": {func(s *runtime.SpawnSpec) { s.Secrets[provisionTokenKey] = "x" }, "a key the runtime writes itself"},
		"dispatch secret":     {func(s *runtime.SpawnSpec) { s.Secrets[dispatchTokenKey] = "x" }, "DISPATCH_TOKEN_FILE is a variable the runtime sets itself"},
		"prompt too large":    {func(s *runtime.SpawnSpec) { s.Prompt.Addressing = strings.Repeat("x", maxArgBytes) }, "the value of --append-system-prompt"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := workerSpec(t)
			tc.edit(&spec)
			if _, err := r.prepare(spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("prepare: %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// New refuses options no cluster could run: an image not pinned by digest, an issue volume with no
// storage class on a cluster that has no default, a stream pods cannot dial, a pool the runtime
// does not choose, no source for the provisioning token or the roles' gh files, and a Resources
// map that would leave a container unreserved — a role or the controller without an entry, or an
// entry that is no reservation: a cpu or memory request past its limit, no memory, an
// ephemeral-storage request past its limit or none at all, or a resource the daemon never sizes —
// each refusal naming the role. A request below its limit is a reservation
// (TestARequestBelowItsLimitRendersABurstablePod).
func TestNewRefusesOptionsNoPodCouldRun(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Options)
		want string
	}{
		"tag image":                 {func(o *Options) { o.Image = "ghcr.io/sjawhar/legion-worker:latest" }, "not pinned by digest"},
		"no class":                  {func(o *Options) { o.StorageClass = "" }, "no storage class"},
		"no volume size":            {func(o *Options) { o.IssueVolume = resource.Quantity{} }, "no issue volume size"},
		"unix stream":               {func(o *Options) { o.StreamURL = "unix:///run/legion.sock" }, "is not tcp://host:port"},
		"relative tool":             {func(o *Options) { o.Tools.Git = "git" }, "git path \"git\" is not absolute"},
		"bad project":               {func(o *Options) { o.Project = "s4a run" }, "is not a label value"},
		"url no bearer":             {func(o *Options) { o.DispatchURL = "https://dispatch.internal" }, "configured together"},
		"bearer no url":             {func(o *Options) { o.DispatchToken = "dispatch-bearer" }, "configured together"},
		"another pool":              {func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: "gpu"} }, "legion.dev/pool is the runtime's"},
		"the pool restated":         {func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: poolValue} }, "legion.dev/pool is the runtime's"},
		"no tokens":                 {func(o *Options) { o.Tokens = nil }, "no provisioning token source"},
		"no gh credential":          {func(o *Options) { o.GitHubCredential = nil }, "no github credential function"},
		"no repository mapping":     {func(o *Options) { o.Mapped = nil }, "no repository mapping for the repository caches"},
		"no cache volume":           {func(o *Options) { o.RepositoryCache.Volume = resource.Quantity{} }, "repository cache volume 0 is not a positive quantity"},
		"no cache retention":        {func(o *Options) { o.RepositoryCache.Retain = 0 }, "repository cache retention 0s is not positive"},
		"an unreserved cache":       {func(o *Options) { o.RepositoryCache.Resources = corev1.ResourceRequirements{} }, "the repository cache reserves no cpu"},
		"a role unreserved":         {func(o *Options) { delete(o.Resources, claim.RoleTester) }, "no reservation for role tester"},
		"the controller unreserved": {func(o *Options) { delete(o.Resources, claim.RoleController) }, "no reservation for role controller"},
		"cpu request past limit": {
			func(o *Options) {
				o.Resources[claim.RoleReviewer].Requests[corev1.ResourceCPU] = resource.MustParse("1500m")
			},
			"role reviewer requests cpu 1500m past its limit 750m",
		},
		"memory request past limit": {
			func(o *Options) {
				o.Resources[claim.RoleImplementer].Requests[corev1.ResourceMemory] = resource.MustParse("7Gi")
			},
			"role implementer requests memory 7Gi past its limit 6Gi",
		},
		"no memory": {
			func(o *Options) {
				delete(o.Resources[claim.RoleMerger].Requests, corev1.ResourceMemory)
				delete(o.Resources[claim.RoleMerger].Limits, corev1.ResourceMemory)
			},
			"role merger reserves no memory",
		},
		"ephemeral-storage request past its limit": {
			func(o *Options) {
				o.Resources[claim.RoleImplementer].Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("40Gi")
			},
			"role implementer requests ephemeral-storage 40Gi past its limit 20Gi",
		},
		"ephemeral-storage unset": {
			func(o *Options) {
				delete(o.Resources[claim.RoleMerger].Requests, corev1.ResourceEphemeralStorage)
				delete(o.Resources[claim.RoleMerger].Limits, corev1.ResourceEphemeralStorage)
			},
			"role merger bounds no ephemeral-storage",
		},
		"an extended resource": {
			func(o *Options) {
				o.Resources[claim.RoleTester].Limits["nvidia.com/gpu"] = resource.MustParse("1")
			},
			"role tester names nvidia.com/gpu in its limits",
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			tc.edit(&opts)
			if _, err := configure(opts); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("configure: %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// runtimeOwned is exactly what the worker container is told by the runtime itself, a tree agent's,
// a Claude Code worker's and the controller's together: every name mainEnvironment sets with Env
// empty and no secret of the spec's, every optional value configured. A name added to the
// environment and not to runtimeOwned is one a spec could override; a name left in runtimeOwned
// that the environment no longer sets is one a spec is refused for nothing.
func TestRuntimeOwnedIsWhatTheWorkerContainerIsToldByTheRuntime(t *testing.T) {
	opts := testOptions()
	opts.DispatchURL, opts.DispatchToken = "https://dispatch.internal", "dispatch-bearer"
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	opts.SessionDSNKey = "SESSION_DSN"
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	told := map[string]bool{}
	for _, spec := range []runtime.SpawnSpec{workerSpec(t), claudeWorkerSpec(t), controllerSpec(t)} {
		spec.Env, spec.Secrets = nil, nil
		for name := range envOf(workerOf(t, r, spec)) {
			told[name] = true
		}
	}
	if !maps.Equal(told, runtimeOwned) {
		t.Errorf("the worker container is told %v by the runtime, and runtimeOwned is %v",
			slices.Sorted(maps.Keys(told)), slices.Sorted(maps.Keys(runtimeOwned)))
	}
}

// The four stall limits a phase worker's reminder runs on reach a pod as ordinary entries of the
// worker process's environment: the daemon merges the ones it holds into the spec's Env (the
// daemon package's workerSpawnSpec), and Env travels verbatim as plain pairs of the generation's
// start command (launchEnvironment), so nothing of them is this runtime's own — they are no part
// of runtimeOwned, which a spec may not set.
func TestTheWorkerContainersEnvCarriesTheStallLimits(t *testing.T) {
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	limits := map[string]string{
		"LEGION_STALL_REMINDER_MS":     "45000",
		"LEGION_STALL_WAITING_MS":      "900000",
		"LEGION_STALL_RESUME_MS":       "15000",
		"LEGION_STALL_ARCHITECT_AFTER": "3",
	}
	spec := workerSpec(t)
	maps.Copy(spec.Env, limits)
	env := envOf(workerOf(t, r, spec))
	for name, want := range limits {
		if env[name] != want {
			t.Errorf("the worker container's %s = %q, want %q", name, env[name], want)
		}
	}
}

// handedAddresses is exactly the addresses a role process launched now carries, every optional
// address configured: each address-valued argument of every launcher's command and of the
// generation's worker-shim argv (the words before `--`), by its flag, and each address-valued
// variable the runtime itself sets in the generation's environment, an address being a value with a
// scheme. The git credential entries (gitCredentialEnv) are not addresses but git configuration
// keys the binary fixes, `credential.https://github.com.helper` among them, so no configuration
// moves them. An address added to a launch and not to handedAddresses is one the daemon could move
// without any role it re-adopts being relaunched (evaluate). A workflow role's launch and the
// controller's, each enrolled with the secrets broker here, are each held to that one list.
func TestHandedAddressesAreEveryAddressAPodCarries(t *testing.T) {
	opts := testOptions()
	opts.DispatchURL, opts.DispatchToken = "https://dispatch.internal.example", "dispatch-bearer"
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]runtime.SpawnSpec{"a workflow role": workerSpec(t), "the controller": controllerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			l, err := r.prepare(spec)
			if err != nil {
				t.Fatal(err)
			}
			start := launcherCommand(l, r)
			carried := map[string]string{}
			words := [][]string{start.Argv[:slices.Index(start.Argv, "--")]}
			for _, c := range podOf(t, r, spec).Containers {
				words = append(words, c.Command)
			}
			for _, command := range words {
				for i := 1; i < len(command); i++ {
					if strings.Contains(command[i], "://") {
						carried[command[i-1]] = command[i]
					}
				}
			}
			gitConfig := map[string]bool{}
			for _, entry := range gitCredentialEnv() {
				gitConfig[entry.Name] = true
			}
			for _, pair := range start.Env {
				if name, value, _ := strings.Cut(pair, "="); runtimeOwned[name] && !gitConfig[name] && strings.Contains(value, "://") {
					carried[name] = value
				}
			}
			handed := map[string]string{}
			for _, a := range r.handedAddresses() {
				if a.value != "" {
					handed[a.name] = a.value
				}
			}
			if !maps.Equal(carried, handed) {
				t.Errorf("a role launched now carries the addresses %v, and handedAddresses is %v", carried, handed)
			}
		})
	}
}

// The Dispatch bearer is the runtime's to carry, not the spec's: with Dispatch configured, every
// claim's Secret holds it as DISPATCH_TOKEN and the worker container reads it through
// DISPATCH_TOKEN_FILE; without it, neither exists.
func TestTheDispatchBearerIsARuntimeOption(t *testing.T) {
	for name, tc := range map[string]struct {
		url, bearer string
	}{
		"configured":     {"https://dispatch.internal", "dispatch-bearer"},
		"not configured": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			opts.DispatchURL, opts.DispatchToken = tc.url, tc.bearer
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			l, err := r.prepare(workerSpec(t))
			if err != nil {
				t.Fatal(err)
			}
			pointer, pointed := envOf(workerOf(t, r, workerSpec(t)))["DISPATCH_TOKEN_FILE"]
			if got := l.secrets[dispatchTokenKey]; got != tc.bearer || pointed != (tc.bearer != "") {
				t.Fatalf("the claim's Secret carries %q as %s and DISPATCH_TOKEN_FILE is %q (set: %t), want %q",
					got, dispatchTokenKey, pointer, pointed, tc.bearer)
			}
			if want := generationDir(workerSpec(t).Generation) + "/" + dispatchTokenKey; pointed && pointer != want {
				t.Fatalf("DISPATCH_TOKEN_FILE = %q, want the generation's private %s", pointer, want)
			}
		})
	}
}

// A workspace recovered after its volume was lost is workspace-init's to recreate and mark
// (LEGION_WORKSPACE_RECOVERED_FROM, cmd/legion/workspace_init.go): the ref reaches the
// workspace-init container alone, never the agent, and a launch recovering nothing names none.
func TestTheRecoveredRefReachesTheInitContainerAlone(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	recovered := workerSpec(t)
	recovered.WorkspaceRecoveredFrom = "legion/LEGION-208"
	for name, tc := range map[string]struct {
		spec runtime.SpawnSpec
		want string
	}{
		"recovered": {recovered, "legion/LEGION-208"},
		"fresh":     {workerSpec(t), ""},
	} {
		t.Run(name, func(t *testing.T) {
			pod := podOf(t, r, tc.spec)
			got, set := envOf(containerNamed(t, pod, initContainer))["LEGION_WORKSPACE_RECOVERED_FROM"]
			if got != tc.want || set != (tc.want != "") {
				t.Errorf("the init container's LEGION_WORKSPACE_RECOVERED_FROM = %q (set: %t), want %q", got, set, tc.want)
			}
			for _, name := range []string{fetchContainer, workerContainer} {
				if _, set := envOf(containerNamed(t, pod, name))["LEGION_WORKSPACE_RECOVERED_FROM"]; set {
					t.Errorf("%s carries LEGION_WORKSPACE_RECOVERED_FROM", name)
				}
			}
		})
	}
}

// workspace-init provisions the issue's workspace at the repository's default branch, which the
// spec names as GitHub does, slash and all: the init container's command passes it verbatim as
// --default-branch's value, and no container of the pod is told main.
func TestTheInitContainerProvisionsAtTheSpecsDefaultBranch(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	spec.DefaultBranch = "release/v7.1"
	command := containerNamed(t, podOf(t, r, spec), initContainer).Command
	at := slices.Index(command, "--default-branch")
	if at < 0 || at+1 >= len(command) || command[at+1] != "release/v7.1" {
		t.Fatalf("the init container's command %q names no --default-branch release/v7.1", command)
	}
	if slices.Contains(command, "main") {
		t.Errorf("the init container's command %q names main", command)
	}
}

// Without agent_secrets a pod carries no projected token of Legion's (the one token a pod carries,
// if any, is the operator's own); with it, exactly one — alone in its volume, for the configured
// audience and lifetime, mounted read-only in the worker container alone — beside a memory-backed
// key directory, the two AGENT_SECRETS_* variables, and the shim's three flags. The probe pod
// carries none of it: it runs no shim and enrolls nothing.
func TestAPodCarriesLegionsTokenExactlyWhenItIsEnrolled(t *testing.T) {
	for name, tc := range map[string]struct {
		secrets *AgentSecrets
		account string
	}{
		"not enrolled, the operator's account": {nil, "operator-worker"},
		"not enrolled, no account":             {nil, ""},
		"enrolled":                             {&AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}, "legion-worker"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := goldenOptions()
			opts.Pod.ServiceAccount = tc.account
			opts.AgentSecrets = tc.secrets
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			l, err := r.prepare(manifestCases(t)["root"].spec)
			if err != nil {
				t.Fatal(err)
			}
			spec := r.podTemplate(l).Spec
			if spec.ServiceAccountName != tc.account || spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
				t.Fatalf("serviceAccountName %q automount %v", spec.ServiceAccountName, spec.AutomountServiceAccountToken)
			}
			var tokens []corev1.Volume
			for _, v := range spec.Volumes {
				if v.Projected != nil && slices.ContainsFunc(v.Projected.Sources, func(s corev1.VolumeProjection) bool { return s.ServiceAccountToken != nil }) {
					tokens = append(tokens, v)
				}
			}
			role := manifestCases(t)["root"].spec.Role
			worker := workerOf(t, r, manifestCases(t)["root"].spec)
			keyVolume := roleVolume(agentSecretsKeyVolume, role)
			env := map[string]string{}
			for _, e := range worker.Env {
				env[e.Name] = e.Value
			}
			if tc.secrets == nil {
				if len(tokens) != 0 || env["AGENT_SECRETS_URL"] != "" || env["AGENT_SECRETS_KEY_DIR"] != "" || slices.Contains(worker.Command, "--agent-secrets-key-dir") {
					t.Fatalf("an unenrolled pod carries agent-secrets pieces: tokens %d, env %v, command %v", len(tokens), env, worker.Command)
				}
				return
			}
			if len(tokens) != 1 || tokens[0].Name != agentSecretsTokenVolume || len(tokens[0].Projected.Sources) != 1 {
				t.Fatalf("token volumes %+v, want exactly %s with one source", tokens, agentSecretsTokenVolume)
			}
			token := tokens[0].Projected.Sources[0].ServiceAccountToken
			if token.Audience != "agent-secrets" || token.ExpirationSeconds == nil || *token.ExpirationSeconds != 3600 || token.Path != AgentSecretsTokenFile {
				t.Fatalf("token source %+v", token)
			}
			mounts := map[string]corev1.VolumeMount{}
			for _, m := range worker.VolumeMounts {
				mounts[m.Name] = m
			}
			if m := mounts[agentSecretsTokenVolume]; m.MountPath != AgentSecretsTokenDir || !m.ReadOnly {
				t.Fatalf("token mount %+v, want read-only at %s", m, AgentSecretsTokenDir)
			}
			if m := mounts[keyVolume]; m.MountPath != AgentSecretsKeyDir || m.ReadOnly {
				t.Fatalf("key mount %+v, want writable at %s", m, AgentSecretsKeyDir)
			}
			key := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == keyVolume })
			if key < 0 || spec.Volumes[key].EmptyDir == nil || spec.Volumes[key].EmptyDir.Medium != corev1.StorageMediumMemory {
				t.Fatalf("key volume %+v, want a memory-backed emptyDir", spec.Volumes[key])
			}
			for _, init := range spec.InitContainers {
				for _, m := range init.VolumeMounts {
					if m.Name == agentSecretsTokenVolume || strings.HasPrefix(m.Name, agentSecretsKeyVolume) {
						t.Fatalf("init container %s mounts %s", init.Name, m.Name)
					}
				}
			}
			if env["AGENT_SECRETS_URL"] != tc.secrets.URL || env["AGENT_SECRETS_KEY_DIR"] != AgentSecretsKeyDir {
				t.Fatalf("env %v", env)
			}
			want := []string{"--agent-secrets-key-dir", AgentSecretsKeyDir, "--pod-token-file", AgentSecretsTokenDir + "/" + AgentSecretsTokenFile, "--agent-secrets-bin", opts.Tools.AgentSecrets}
			joined := strings.Join(worker.Command, "\x00")
			if !strings.Contains(joined, strings.Join(want, "\x00")) || strings.Index(joined, "--agent-secrets-key-dir") > strings.Index(joined, "\x00--\x00") {
				t.Fatalf("shim command %v, want the three flags before --", worker.Command)
			}
			probe := r.probeManifest("legion-probe-test", ImageProbe{Contract: 7}, time.Now().Add(time.Hour)).Spec.PodTemplate.Spec
			for _, v := range probe.Volumes {
				if v.Name == agentSecretsTokenVolume || strings.HasPrefix(v.Name, agentSecretsKeyVolume) {
					t.Fatalf("the probe pod carries %s", v.Name)
				}
			}
		})
	}
}

// A tree role's shim is told --warm-codegraph, before `--`, and the LEGION_WORKSPACE the flag
// needs: once its Oh My Pi has started, the shim builds that workspace's CodeGraph index behind
// the launch (cmd/legion/worker_shim.go). The controller's shim, whose pod has no workspace, is
// not (TestTheControllersAgentIsToldItIsTheControllerAndNothingOfATree).
func TestATreeRolesShimIsToldToWarmTheWorkspacesCodegraphIndex(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	worker := workerOf(t, r, workerSpec(t))
	shim := worker.Command[:slices.Index(worker.Command, "--")]
	if !slices.Contains(shim, "--warm-codegraph") {
		t.Fatalf("the shim runs as %v, want --warm-codegraph before --", worker.Command)
	}
	if envOf(worker)["LEGION_WORKSPACE"] == "" {
		t.Fatalf("the shim is told --warm-codegraph with no LEGION_WORKSPACE to warm; env %v", worker.Env)
	}
	// The shim's wait for that warm-up on its way out is bounded by the stop grace its launcher
	// kills it at, so the shim is told the same TerminationGrace the launcher runs with.
	at := slices.Index(shim, "--stop-grace")
	if at < 0 || at+1 >= len(shim) || shim[at+1] != r.terminationGrace.String() {
		t.Fatalf("the shim runs as %v, want --stop-grace %s, the launcher's own", shim, r.terminationGrace)
	}
}

// New refuses an agent-secrets configuration no pod could run: no broker URL, no token audience,
// an expiry outside the API server's floor (10m) and the cluster's admission cap (1h), or the image's
// agent-secrets binary missing.
func TestNewRefusesAnAgentSecretsOptionNoPodCouldRun(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Options)
		want string
	}{
		"no url":      {func(o *Options) { o.AgentSecrets = &AgentSecrets{Audience: "a", TokenExpiry: time.Hour} }, "agent secrets: no broker URL"},
		"no audience": {func(o *Options) { o.AgentSecrets = &AgentSecrets{URL: "https://s", TokenExpiry: time.Hour} }, "agent secrets: no token audience"},
		"expiry too long": {func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://s", Audience: "a", TokenExpiry: 2 * time.Hour}
		}, "agent secrets: token expiry 2h0m0s is not between 10m0s and 1h0m0s"},
		"no binary": {func(o *Options) {
			o.AgentSecrets = &AgentSecrets{URL: "https://s", Audience: "a", TokenExpiry: time.Hour}
			o.Tools.AgentSecrets = ""
		}, "the image's agent-secrets path \"\" is not absolute"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := goldenOptions()
			tc.edit(&opts)
			_, err := configure(opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("configure() = %v, want %q", err, tc.want)
			}
		})
	}
}

// operatorPod is an operator's runtime.kubernetes.pod: a variable, a Secret volume, a ConfigMap
// volume mounted by sub_path into the profile's agent directory, a projected ServiceAccount token
// alone in its volume, an existing share by its PersistentVolumeClaim mounted read-only, an NFS
// export mounted writable, and the account the pods run as.
func operatorPod() Pod {
	expiry := int64(3600)
	return Pod{
		Env: map[string]string{"PI_CONFIG_FILES": "/etc/legion-operator/overlay.yml"},
		Volumes: []corev1.Volume{
			{Name: "operator-creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "legion-operator-creds"}}},
			{Name: "operator-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator"},
				Items:                []corev1.KeyToPath{{Key: "models.yml", Path: "models.yml"}},
			}}},
			{Name: "operator-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
				ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "operator-audience", ExpirationSeconds: &expiry, Path: "token"},
			}}}}},
			{Name: "operator-share", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-share"}}},
			{Name: "operator-nas", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nas.internal.example", Path: "/exports/builds"}}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "operator-creds", MountPath: "/etc/legion-operator/creds", ReadOnly: true},
			{Name: "operator-config", MountPath: ompAgentDir + "/models.yml", SubPath: "models.yml", ReadOnly: true},
			{Name: "operator-token", MountPath: "/var/run/operator", ReadOnly: true},
			{Name: "operator-share", MountPath: "/mnt/team", ReadOnly: true},
			{Name: "operator-nas", MountPath: "/mnt/nas"},
		},
		ServiceAccount: "operator-worker",
	}
}

// The operator's pod (runtime.kubernetes.pod) reaches every pod Legion runs — the root's, a
// worker's, the daemon-launched controller's and the image probe's — exactly as configured: its
// volumes beside Legion's, its variables and mounts in the agent's container alone, never in an
// init container, and its account as the pod's. The controller's account being the issue pods'
// is what makes it the Legion daemon service's own pod to the secrets broker, which binds one
// account per service, so its slack_post is granted the service's LEGION_SLACK_BOT_TOKEN.
func TestTheOperatorsPodReachesEveryPodLegionRuns(t *testing.T) {
	opts := testOptions()
	opts.Pod = operatorPod()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	probeSpec := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
	for _, tc := range []struct {
		name  string
		pod   corev1.PodSpec
		agent corev1.Container
	}{
		{"root", podOf(t, r, rootSpec(t)), workerOf(t, r, rootSpec(t))},
		{"worker", podOf(t, r, workerSpec(t)), workerOf(t, r, workerSpec(t))},
		{"controller", podOf(t, r, controllerSpec(t)), workerOf(t, r, controllerSpec(t))},
		{"probe", probeSpec, containerNamed(t, probeSpec, probeContainer)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pod.ServiceAccountName != opts.Pod.ServiceAccount {
				t.Errorf("serviceAccountName = %q, want the operator's %q", tc.pod.ServiceAccountName, opts.Pod.ServiceAccount)
			}
			for _, want := range opts.Pod.Volumes {
				var got []corev1.Volume
				for _, volume := range tc.pod.Volumes {
					if volume.Name == want.Name {
						got = append(got, volume)
					}
				}
				if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
					t.Errorf("the pod's volumes named %s are %+v, want exactly %+v", want.Name, got, want)
				}
			}
			agent := tc.agent
			for _, want := range opts.Pod.VolumeMounts {
				if !slices.ContainsFunc(agent.VolumeMounts, func(m corev1.VolumeMount) bool { return reflect.DeepEqual(m, want) }) {
					t.Errorf("%s mounts %+v, want %+v among them", agent.Name, agent.VolumeMounts, want)
				}
			}
			if got := envOf(agent)["PI_CONFIG_FILES"]; got != opts.Pod.Env["PI_CONFIG_FILES"] {
				t.Errorf("%s's PI_CONFIG_FILES = %q, want the operator's %q", agent.Name, got, opts.Pod.Env["PI_CONFIG_FILES"])
			}
			for _, init := range tc.pod.InitContainers {
				for _, mount := range init.VolumeMounts {
					if strings.HasPrefix(mount.Name, "operator-") {
						t.Errorf("%s mounts the operator's %s", init.Name, mount.Name)
					}
				}
				if _, set := envOf(init)["PI_CONFIG_FILES"]; set {
					t.Errorf("%s is told the operator's PI_CONFIG_FILES", init.Name)
				}
			}
		})
	}
}

// The providers Secret — the TypeScript runtime's legion-<project token>-providers — reaches every
// pod Legion runs, a worker's and the image probe's alike, as exactly its configured keys, read-only
// at ProvidersDir in the agent's container alone: each provider key as a file named for the variable
// Oh My Pi reads, which the worker's shim exports into Oh My Pi's environment (--provider-env-dir),
// and each providers secret (the NATS nkey seed) as a file of its own name that the container's
// `<NAME>_FILE` names, so the shim exports it to no one. The pointer is the runtime's, not the
// spec's: a spec that carries no seed still gets it, and the claim's Secret never holds a copy. With
// neither, no pod mounts the Secret, no shim is told a directory, and there is no pointer.
func TestTheProvidersSecretReachesEveryPodAsItsOwnFiles(t *testing.T) {
	const providersSecret = "legion-" + testProject + "-providers"
	const seed = "SUAIBDPBAUTWCWBKIO6XHQNINK5FWJW4OHLXC3HQ2KFE4PEJUA44CNHTC4"
	nats := []string{"NATS_NKEY_SEED"}
	natsItem := corev1.KeyToPath{Key: "NATS_NKEY_SEED", Path: "NATS_NKEY_SEED"}
	anthropic := corev1.KeyToPath{Key: "anthropic", Path: "ANTHROPIC_API_KEY"}
	for _, tc := range []struct {
		name      string
		keys      map[string]string
		providers []string
		specSeed  bool
		items     []corev1.KeyToPath
	}{
		{"provider keys", map[string]string{"GEMINI_API_KEY": "gemini", "ANTHROPIC_API_KEY": "anthropic"}, nil, false,
			[]corev1.KeyToPath{anthropic, {Key: "gemini", Path: "GEMINI_API_KEY"}}},
		{"neither", nil, nil, false, nil},
		{"the seed with provider keys", map[string]string{"ANTHROPIC_API_KEY": "anthropic"}, nats, true, []corev1.KeyToPath{anthropic, natsItem}},
		{"the seed alone", nil, nats, true, []corev1.KeyToPath{natsItem}},
		{"the seed, on a spec that carries none", nil, nats, false, []corev1.KeyToPath{natsItem}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.ProviderKeys, opts.ProvidersSecrets = tc.keys, tc.providers
			opts.LaunchSecrets = []string{"ENVOY_TOKEN", "NATS_NKEY_SEED"}
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			spec := workerSpec(t)
			spec.Secrets = map[string]string{"ENVOY_TOKEN": "envoy-bearer"}
			if tc.specSeed {
				spec.Secrets["NATS_NKEY_SEED"] = seed
			}
			l, err := r.prepare(spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, copied := l.secrets["NATS_NKEY_SEED"]; copied {
				t.Errorf("the claim's Secret carries NATS_NKEY_SEED: %v", slices.Sorted(maps.Keys(l.secrets)))
			}
			worker := r.podTemplate(l).Spec
			probe := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
			workerProcess := workerOf(t, r, spec)
			for _, pod := range []struct {
				spec      corev1.PodSpec
				container string
				agent     corev1.Container
			}{{worker, workerContainer, workerProcess}, {probe, probeContainer, containerNamed(t, probe, probeContainer)}} {
				var volumes []corev1.Volume
				for _, volume := range pod.spec.Volumes {
					if volume.Secret != nil && volume.Secret.SecretName == providersSecret {
						volumes = append(volumes, volume)
					}
					if volume.Secret != nil && strings.HasPrefix(volume.Secret.SecretName, SandboxName(workerToken)) {
						for _, item := range volume.Secret.Items {
							if item.Key == "NATS_NKEY_SEED" {
								t.Errorf("%s's boot projection carries NATS_NKEY_SEED", pod.container)
							}
						}
					}
				}
				mounted := func(c corev1.Container) []corev1.VolumeMount {
					var mounts []corev1.VolumeMount
					for _, mount := range c.VolumeMounts {
						if slices.ContainsFunc(volumes, func(v corev1.Volume) bool { return v.Name == mount.Name }) || mount.MountPath == ProvidersDir {
							mounts = append(mounts, mount)
						}
					}
					return mounts
				}
				agent := pod.agent
				for _, init := range pod.spec.InitContainers {
					if got := mounted(init); len(got) != 0 {
						t.Errorf("%s mounts the providers Secret: %+v", init.Name, got)
					}
				}
				if tc.items == nil {
					if len(volumes) != 0 || len(mounted(agent)) != 0 {
						t.Errorf("%s: with neither the pod has %+v and mounts %+v", pod.container, volumes, mounted(agent))
					}
				} else {
					want := &corev1.SecretVolumeSource{SecretName: providersSecret, Items: tc.items, DefaultMode: new(int32(0o440))}
					if len(volumes) != 1 || !reflect.DeepEqual(volumes[0].Secret, want) {
						t.Errorf("%s's pod holds the providers Secret as %+v, want once as %+v", pod.container, volumes, *want)
					}
					if got := mounted(agent); len(got) != 1 || got[0].MountPath != ProvidersDir || !got[0].ReadOnly || got[0].SubPath != "" {
						t.Errorf("%s mounts the providers Secret as %+v, want once, read-only, at %s", pod.container, got, ProvidersDir)
					}
				}
				env := envOf(agent)
				pointer, pointed := env["NATS_NKEY_SEED_FILE"]
				if want := tc.providers != nil; pointed != want || (want && pointer != ProvidersDir+"/NATS_NKEY_SEED") {
					t.Errorf("%s's NATS_NKEY_SEED_FILE = %q (set: %t), want %s/NATS_NKEY_SEED set %t", pod.container, pointer, pointed, ProvidersDir, want)
				}
				for name, value := range env {
					if strings.Contains(value, seed) {
						t.Errorf("%s's %s carries the seed", pod.container, name)
					}
				}
				if _, set := env["NATS_NKEY_SEED"]; set {
					t.Errorf("%s sets NATS_NKEY_SEED", pod.container)
				}
			}
			main := workerProcess
			shim := main.Command[:slices.Index(main.Command, "--")]
			at := slices.Index(shim, "--provider-env-dir")
			switch {
			case tc.items == nil && at >= 0:
				t.Errorf("with neither the shim is told %v", shim)
			case tc.items != nil && (at < 0 || at+1 == len(shim) || shim[at+1] != ProvidersDir):
				t.Errorf("the shim runs as %v, want --provider-env-dir %s", shim, ProvidersDir)
			}
		})
	}
}

// A providers secret must be a launch secret: CheckPod refuses a provider key or operator variable
// colliding with a launch secret's pointer, which is what keeps a providers secret's file and
// pointer the runtime's alone.
func TestNewRefusesAProvidersSecretThatIsNoLaunchSecret(t *testing.T) {
	opts := testOptions()
	opts.LaunchSecrets, opts.ProvidersSecrets = []string{"ENVOY_TOKEN"}, []string{"NATS_NKEY_SEED"}
	if _, err := configure(opts); err == nil || err.Error() != "sandbox runtime: providers secret NATS_NKEY_SEED is not a launch secret (ENVOY_TOKEN)" {
		t.Fatalf("configure = %v, want the refusal naming NATS_NKEY_SEED", err)
	}
}

// legionVolumeNames, legionMountPaths, and runtimeOwned — what CheckPod refuses in the operator's
// pod — are exactly what Legion's own pods carry: every volume of every pod a launch or
// the image probe runs, and every mount and variable of the containers the operator's pieces join
// (the agent's and the probe's), with every optional piece the runtime adds configured and nothing
// of a spec's or the operator's. A Claude Code worker joins the goldens' cases here, since three
// of the variables are its alone and no golden is a Claude pod's. A piece added to a pod and not
// to its list is one an operator could collide with unrefused; one left in a list that no pod
// carries is refused for nothing.
func TestLegionsOwnNamesAreWhatItsPodsCarry(t *testing.T) {
	opts := goldenOptions()
	opts.DispatchURL, opts.DispatchToken = "https://dispatch.internal", "dispatch-bearer"
	opts.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "anthropic"}
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	opts.SessionDSNKey = "SESSION_DSN"
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	volumes, mounts, env := map[string]bool{}, map[string]bool{}, map[string]bool{}
	carry := func(pod corev1.PodSpec, agent corev1.Container) {
		for _, volume := range pod.Volumes {
			volumes[volume.Name] = true
		}
		for _, mount := range agent.VolumeMounts {
			mounts[mount.MountPath] = true
		}
		for _, variable := range agent.Env {
			env[variable.Name] = true
		}
	}
	specs := []runtime.SpawnSpec{claudeWorkerSpec(t)}
	for _, tc := range manifestCases(t) {
		specs = append(specs, tc.spec)
	}
	for _, spec := range specs {
		spec.Env, spec.Secrets = nil, nil
		pod := podOf(t, r, spec)
		carry(pod, workerOf(t, r, spec))
	}
	probe := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
	carry(probe, containerNamed(t, probe, probeContainer))
	for _, list := range []struct {
		name    string
		got     []string
		carried map[string]bool
	}{
		{"legionVolumeNames", legionVolumeNames(), volumes},
		{"legionMountPaths", legionMountPaths(), mounts},
		{"runtimeOwned", slices.Collect(maps.Keys(runtimeOwned)), env},
	} {
		listed := map[string]bool{}
		for _, entry := range list.got {
			listed[entry] = true
		}
		if !maps.Equal(listed, list.carried) {
			t.Errorf("%s is %v, and Legion's pods carry %v", list.name, slices.Sorted(maps.Keys(listed)), slices.Sorted(maps.Keys(list.carried)))
		}
	}
}

// imageOwnedPaths covers every path in the image a pod runs or loads from, which an operator's
// mount there would hide: Oh My Pi, the Envoy and Legion plugins the agent loads, the Go legion
// every container runs, the profile's installed plugins, and the databases Oh My Pi keeps in the
// profile's agent directory.
func TestImageOwnedPathsCoverWhatAPodRunsFromTheImage(t *testing.T) {
	for _, used := range []string{
		defaultAgent, envoyPlugin, legionPlugin, testOptions().Tools.Legion,
		ompProfileDir + "/plugins/node_modules", ompAgentDir + "/agent.db", ompAgentDir + "/models.db",
	} {
		if !slices.ContainsFunc(imageOwnedPaths(), func(owned string) bool {
			return used == owned || strings.HasPrefix(used, owned+"/")
		}) {
			t.Errorf("%s is not under imageOwnedPaths %v", used, imageOwnedPaths())
		}
	}
}

// Every Legion pod, an issue's and the controller's alike, asks nothing of its placement beyond the
// Legion pool: of its template's fields, the ones that bear on scheduling are the pool's node
// selector and tolerations and the Legion priority class, and no other — nothing ties it to, or
// keeps it off, another pod's node. Each issue pod owns a ReadWriteOnce volume of its own and
// shares nothing with the other pods of its tree, so the scheduler places it wherever the pool has
// room (LEGION-632). Pinned as the exact key set of the pod spec the manifest sends, so a field
// added to the template shows here by name.
func TestEveryLegionPodAsksNothingOfItsPlacementBeyondThePool(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"automountServiceAccountToken", "containers", "enableServiceLinks", "initContainers", "nodeSelector",
		"priorityClassName", "restartPolicy", "runtimeClassName", "securityContext", "terminationGracePeriodSeconds",
		"tolerations", "volumes",
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			r.agentSecrets = tc.agentSecrets
			manifest := manifestOf(t, r, tc.spec).(map[string]any)
			pod := manifest["spec"].(map[string]any)["podTemplate"].(map[string]any)["spec"].(map[string]any)
			if got := slices.Sorted(maps.Keys(pod)); !slices.Equal(got, want) {
				t.Errorf("the pod spec sent carries %v, want exactly %v", got, want)
			}
		})
	}
}

// The volume claim template of every Sandbox, an issue's and the controller's alike, carries no
// tree label: an issue's claim is labelled by project and issue, the controller's by project and
// role, while the Sandbox itself keeps the tree, which its tree's cleanup selects by. The volume is
// the issue's, not the tree's: a child re-admitted as a root of its own keeps its Sandbox and
// volume, and the relabel (ensureSandbox) patches the Sandbox's labels alone — the daemon's
// restricted identity has no PVC verb — so a tree label on the claim would name the volume's first
// tree for life (LEGION-632).
func TestTheClaimTemplateCarriesTheIssueAndNeverTheTree(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		t.Run(name, func(t *testing.T) {
			r.agentSecrets = tc.agentSecrets
			l, err := r.prepare(tc.spec)
			if err != nil {
				t.Fatal(err)
			}
			s := r.sandboxManifest(l)
			want := map[string]string{labelProject: testProject, labelIssue: labelValue(tc.spec.Issue)}
			if l.kind == (controllerPod{}) {
				want = map[string]string{labelProject: testProject, labelRole: string(claim.RoleController)}
			} else if s.Labels[labelTree] != labelValue(tc.spec.Tree) {
				t.Errorf("the Sandbox carries %s=%q, want its tree %q: the tree's cleanup selects it by that label", labelTree, s.Labels[labelTree], tc.spec.Tree)
			}
			if got := s.Spec.VolumeClaimTemplates[0].Metadata.Labels; !maps.Equal(got, want) {
				t.Errorf("the claim template carries %v, want exactly %v", got, want)
			}
		})
	}
}

// kubeExpand is the kubelet's expansion of a container's command, args, and env values
// (k8s.io/kubernetes third_party/forked/golang/expansion): `$$` is a literal `$`, and `$(NAME)` is
// the value of a variable the container defines, left as written when it defines none.
func kubeExpand(input string, env map[string]string) string {
	var out strings.Builder
	for i := 0; i < len(input); i++ {
		if input[i] != '$' || i+1 == len(input) {
			out.WriteByte(input[i])
			continue
		}
		switch next := input[i+1]; {
		case next == '$':
			out.WriteByte('$')
			i++
		case next == '(' && strings.Contains(input[i+2:], ")"):
			name, _, _ := strings.Cut(input[i+2:], ")")
			if value, ok := env[name]; ok {
				out.WriteString(value)
			} else {
				out.WriteString("$(" + name + ")")
			}
			i += len(name) + 2
		default:
			out.WriteByte('$')
		}
	}
	return out.String()
}

// Whatever text a launch carries reaches the process as written: the worker's argv and plain
// environment travel in the launcher's start command, which no kubelet expands, so `$(NAME)` and
// `$$` in the inlined system prompt, an operator's instructions, and a spec's env values mean
// what they say in a pod as in a pane. The role container's own command and env, which the kubelet
// does expand, still survive it.
func TestTextSurvivesTheKubeletsExpansion(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	literal := "Run `echo $(HOME)` and `kill $$`; your issue is not $(LEGION_ISSUE), and $LEGION_WORKSPACE is yours. $"
	spec := workerSpec(t)
	if err := os.WriteFile(spec.Prompt.DeploymentInstructionsPath, []byte(literal+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec.Env["LEGION_E2E_NOTE"] = literal
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	start := launcherCommand(l, r)
	argv := l.agentArgv(r.agent)
	if got := start.Argv[len(start.Argv)-len(argv):]; !slices.Equal(got, argv) {
		t.Errorf("the agent's argv reaches the launcher as %q, want %q", got, argv)
	}
	env := map[string]string{}
	for _, entry := range start.Env {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	for name, want := range map[string]string{
		"LEGION_E2E_NOTE": literal,
		"PI_SHELL_PREFIX": shellprefix.For(filepath.Dir(r.tools.Legion)),
	} {
		if got := env[name]; got != want {
			t.Errorf("%s reaches the agent as %q, want %q", name, got, want)
		}
	}
	role := containerNamed(t, r.podTemplate(l).Spec, string(spec.Role))
	for _, arg := range role.Command {
		if expanded := kubeExpand(arg, envOf(role)); strings.ReplaceAll(arg, "$$", "$") != expanded {
			t.Errorf("the launcher's argument %q reaches it as %q", arg, expanded)
		}
	}
	if !strings.Contains(l.prompt, literal) {
		t.Fatalf("the system prompt does not carry the instructions as written: %q", l.prompt)
	}
}

// uuid is the form a Claude Code session id takes (claudelaunch.NewSessionID), the only word of a
// Claude launch's agent command that is not fixed.
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// A Claude Code role's pod runs the image's Claude Code under `legion claude-bridge`, word for
// word: the image's own paths, every permission granted, only the MCP document this generation's
// launcher writes into its private directory, the claude-envoy plugin and the generated Legion
// plugin, a fresh session, and the one system prompt word — the same text the Oh My Pi branch of
// the same launch inlines, since a pod cannot read the daemon's prompt files whichever harness it
// runs. A launch with no recorded session starts a session of its own, which is what the fresh
// uuid here says; one that resumes names the transcript's id instead
// (TestAResumingClaudePodContinuesItsTranscriptByItsID).
func TestAClaudePodsAgentCommandIsClaudeCodeUnderTheBridge(t *testing.T) {
	opts := goldenOptions()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	omp := launchOf(t, r, spec)
	spec.Backend = runtime.BackendClaude
	l := launchOf(t, r, spec)
	argv := l.agentArgv(r.agent)
	if l.prompt != omp.prompt {
		t.Fatalf("the Claude launch's system prompt is %q, want the Oh My Pi launch's %q", l.prompt, omp.prompt)
	}
	session := ""
	if at := slices.Index(argv, "--session-id"); at < 0 || at+1 == len(argv) {
		t.Fatalf("the agent command %q names no --session-id", argv)
	} else if session = argv[at+1]; !uuid.MatchString(session) {
		t.Errorf("the session is %q, want a version 4 uuid", session)
	}
	want := []string{
		"/opt/legion/bin/legion", "claude-bridge", "--", "/usr/local/bin/claude",
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
		"--strict-mcp-config", "--mcp-config", generationDir(spec.Generation) + "/claude-mcp.json",
		"--plugin-dir", "/opt/legion/claude-envoy",
		"--plugin-dir", "/opt/legion/claude-envoy/legion",
		"--session-id", session,
		"--append-system-prompt", omp.prompt,
	}
	if !slices.Equal(argv, want) {
		t.Errorf("the Claude pod's agent command is %q, want %q", argv, want)
	}
	// Options.Agent is the Oh My Pi override and nothing else: a Claude role's command is the
	// image's, as the tmux lane's is the host's boot-resolved one.
	opts.Agent = []string{"/opt/omp/bin/omp", "--operator"}
	overriding, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	overridden := launchOf(t, overriding, spec).agentArgv(overriding.agent)
	if slices.Contains(overridden, "--operator") {
		t.Errorf("with Options.Agent set the Claude pod's command is %q, want the image's Claude Code alone", overridden)
	}
}

// A Claude Code role's worker container is told where Claude Code keeps its state and that
// nothing of Claude Code's own may run after a turn's last result — the harness's analogue of the
// turn scoping `--pod-safety` writes for Oh My Pi — and an Oh My Pi role's container is told none
// of the three. The directory is the role's own on the issue's volume, so it outlives the pod and
// is no sibling role's.
func TestOnlyAClaudeWorkerIsToldClaudeCodesOwnVariables(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := claudeWorkerSpec(t)
	claude := envOf(workerOf(t, r, spec))
	for name, want := range map[string]string{
		"CLAUDE_CONFIG_DIR":                    scratchRoot + "/state/" + string(spec.Role) + "/claude",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1",
		"CLAUDE_CODE_DISABLE_CRON":             "1",
	} {
		if claude[name] != want {
			t.Errorf("a Claude worker is told %s=%q, want %q", name, claude[name], want)
		}
		if omp := envOf(workerOf(t, r, workerSpec(t))); omp[name] != "" {
			t.Errorf("an Oh My Pi worker is told %s=%q, want nothing", name, omp[name])
		}
		if !runtimeOwned[name] {
			t.Errorf("%s is not runtimeOwned: a spec or an operator could set it", name)
		}
	}
}

// A Claude Code role's relaunch continues the transcript its replaced pod left: the agent command
// ends `--resume <uuid>`, the id parsed from the recorded path, and names neither a --session-id
// nor the path itself — Claude Code resumes by id alone. The recorded path still reaches the role
// launcher, which stats it before the child starts (internal/launcher). prepare is where the rule
// is applied, so a Claude claim still recording an Oh My Pi session is refused there, naming the
// backend change that left it recorded.
func TestAResumingClaudePodContinuesItsTranscriptByItsID(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := claudeResumeSpec(t)
	l := launchOf(t, r, spec)
	argv := l.agentArgv(r.agent)
	want := []string{"--resume", claudeTranscriptID, "--append-system-prompt", l.prompt}
	if got := argv[max(len(argv)-4, 0):]; !slices.Equal(got, want) {
		t.Errorf("the resuming Claude command ends %q, want %q", got, want)
	}
	if slices.Contains(argv, "--session-id") {
		t.Errorf("the resuming Claude command %q names --session-id, want the recorded session continued", argv)
	}
	for _, arg := range argv {
		if strings.Contains(arg, spec.ResumeSessionFile) {
			t.Errorf("the resuming Claude command carries the recorded path in %q, want its session id alone", arg)
		}
	}
	if start := launcherCommand(l, r); start.ResumeFile != spec.ResumeSessionFile {
		t.Errorf("the start command resumes %q, want the recorded transcript %s", start.ResumeFile, spec.ResumeSessionFile)
	}
	stale := claudeWorkerSpec(t)
	stale.Generation, stale.BootToken, stale.ResumeSessionFile = 2, "boot-g2", resumeSession
	if _, err := r.prepare(stale); err == nil || !strings.Contains(err.Error(), "the role's backend changed since it was recorded") {
		t.Errorf("prepare of a Claude claim recording the Oh My Pi session %s = %v, want a refusal naming the backend change", resumeSession, err)
	}
}

// validateSessionPath holds each backend's recorded session to its own harness's rule. A Claude
// claim resumes a transcript of the pod's own Claude Code directory and nothing else: one of
// another role's directory, an Oh My Pi session file, or a name that is no session id is refused,
// and a session of the other harness is refused naming the backend change that left it recorded,
// as the tmux lane's resume refuses one (claudelaunch.ResumeSession). An Oh My Pi claim's rule is
// the pod's sessions directory, unchanged, and a Claude transcript is no session of it.
func TestValidateSessionPathIsEachBackendsOwnRule(t *testing.T) {
	transcript := claudeTranscript(claim.RoleTester)
	session, err := validateSessionPath(transcript, runtime.BackendClaude, claim.RoleTester)
	if err != nil || session != claudeTranscriptID {
		t.Fatalf("validateSessionPath(%s, claude) = %q, %v; want the transcript's session id %s", transcript, session, err, claudeTranscriptID)
	}
	for name, file := range map[string]string{
		"a transcript of another role's directory": claudeTranscript(claim.RolePlanner),
		"an Oh My Pi session file":                 resumeSession,
	} {
		_, err := validateSessionPath(file, runtime.BackendClaude, claim.RoleTester)
		if err == nil || !strings.Contains(err.Error(), file) ||
			!strings.Contains(err.Error(), "the role's backend changed since it was recorded") {
			t.Errorf("validateSessionPath(%s, %s, claude) = %v, want a refusal naming it and the backend change", name, file, err)
		}
	}
	named := claudeConfigDir(claim.RoleTester) + "/projects/--cwd--/2026-09-23T12-00-00-000Z_0198.jsonl"
	if _, err := validateSessionPath(named, runtime.BackendClaude, claim.RoleTester); err == nil ||
		!strings.Contains(err.Error(), "not named for a Claude Code session id") {
		t.Errorf("validateSessionPath(%s, claude) = %v, want it refused as no session id", named, err)
	}
	if _, err := validateSessionPath(resumeSession, runtime.BackendOmp, claim.RoleTester); err != nil {
		t.Errorf("validateSessionPath(%s, omp) = %v, want the pod's sessions directory accepted", resumeSession, err)
	}
	if _, err := validateSessionPath(transcript, runtime.BackendOmp, claim.RoleTester); err == nil ||
		!strings.Contains(err.Error(), "cannot be resumed on this runtime") {
		t.Errorf("validateSessionPath(%s, omp) = %v, want it refused as no file of the pod's sessions directory", transcript, err)
	}
}

// SessionOnVolume moves an Oh My Pi session file's recorded path onto a mounted volume and refuses
// a Claude Code transcript with a line naming it: the one-off copy that path is computed for
// (`legion sessions import`) fills Oh My Pi's session table, which holds no session of a
// transcript, and a Claude claim's session stays on the volume its role's own Claude Code reads it
// from. A pane's transcript, under the daemon's state directory rather than a pod's volume, is
// refused alike: the check is told no configuration directory and reads the shape itself.
func TestSessionOnVolumeRefusesAClaudeTranscript(t *testing.T) {
	const root = "/mnt/tree"
	onVolume, err := SessionOnVolume(resumeSession, root)
	want := root + "/" + SessionsSubPath + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/2026-09-23T12-00-00-000Z_0198.jsonl"
	if err != nil || onVolume != want {
		t.Fatalf("SessionOnVolume(%s) = %q, %v; want %q", resumeSession, onVolume, err, want)
	}
	for _, file := range []string{
		claudeTranscript(claim.RoleTester),
		"/var/lib/legion/claude/legion-legion-legion-208-tester/projects/--cwd--/" + claudeTranscriptID + ".jsonl",
	} {
		if _, err := SessionOnVolume(file, root); err == nil ||
			!strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "Claude Code transcript") {
			t.Errorf("SessionOnVolume(%s) = %v, want a refusal naming it a Claude Code transcript", file, err)
		}
	}
}
