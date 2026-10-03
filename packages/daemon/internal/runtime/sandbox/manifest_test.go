package sandbox

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

var updateGolden = flag.Bool("update", false, "rewrite the manifest goldens this package pins")

// goldenOptions are production's settings: the budgets a golden's lock wait and grace are read
// from, a role's resources, and the Legion priority class.
func goldenOptions() Options {
	opts := testOptions()
	opts.BootTimeout = 120 * time.Second
	opts.BootIntervals = 3
	opts.TerminationGrace = 30 * time.Second
	opts.Scheduling = Scheduling{PriorityClass: "legion"}
	opts.Resources = map[claim.Role]corev1.ResourceRequirements{
		claim.RoleTester: {
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
		},
	}
	return opts
}

// resumeSession is a session file Oh My Pi recorded in a pod.
const resumeSession = ompSessionsDir + "/--legion-workspaces-sjawhar-legion-smoke-legion-208--/2026-09-23T12-00-00-000Z_0198.jsonl"

// manifestCases are the Sandboxes the goldens pin: the root, which owns the tree volume; a worker
// placed beside a scheduled pod of its tree, and one placed with none; a resume; a relaunch
// whose workspace is recovered after its volume was lost; and the root enrolled with the secrets
// broker. colocate is whether another pod of the tree is scheduled when the launch runs, and
// agentSecrets, set for root-enrolled alone, is the runtime's enrollment for that one case
// (TestManifestGoldens, TestManifestMatchesTheSandboxCRD apply it to the shared runtime before
// building that case's manifest, and restore nil after — every other case runs unenrolled).
func manifestCases(t *testing.T) map[string]struct {
	spec         runtime.SpawnSpec
	colocate     bool
	agentSecrets *AgentSecrets
} {
	resume := workerSpec(t)
	resume.Generation, resume.BootToken, resume.ResumeSessionFile = 2, "boot-g2", resumeSession
	recovered := workerSpec(t)
	recovered.WorkspaceRecoveredFrom = "legion/LEGION-208"
	return map[string]struct {
		spec         runtime.SpawnSpec
		colocate     bool
		agentSecrets *AgentSecrets
	}{
		"root":               {rootSpec(t), false, nil},
		"worker-affinity":    {workerSpec(t), true, nil},
		"worker-no-affinity": {workerSpec(t), false, nil},
		"resume":             {resume, true, nil},
		"recovered":          {recovered, true, nil},
		"root-enrolled": {rootSpec(t), false, &AgentSecrets{
			URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour,
		}},
	}
}

// manifestOf is the Sandbox a launch of spec leaves running, as the API server holds it: the one
// the runtime creates, with the relaunch's Running patch applied — the pod template for colocate,
// and the Running mode.
func manifestOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec, colocate bool) any {
	t.Helper()
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	s := r.sandboxManifest(l)
	s.Spec.PodTemplate, s.Spec.OperatingMode = r.podTemplate(l, colocate), modeRunning
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
			if err := encoder.Encode(manifestOf(t, r, tc.spec, tc.colocate)); err != nil {
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
			if found := schemaViolations(manifestOf(t, r, tc.spec, tc.colocate), schema, ""); len(found) > 0 {
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
	for name, manifest := range map[string]any{"worker": manifestOf(t, r, workerSpec(t), true), "probe": wire(t, probe.Object)} {
		if found := schemaViolations(manifest, schema, ""); len(found) > 0 {
			t.Errorf("the %s manifest has fields the CRD does not declare as sent:\n%s", name, strings.Join(found, "\n"))
		}
	}
}

// podOf is the pod template a launch of spec runs.
func podOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec, affinity bool) corev1.PodSpec {
	t.Helper()
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	return r.podTemplate(l, affinity).Spec
}

// workerContainer is the role container of workerSpec, the tester.
const workerContainer = string(claim.RoleTester)

// workerOf is the worker-shim process a launch of spec starts: its role container as the pod runs
// it (mounts, kubelet-resolved environment), with the launcher start command's argv as Command and
// its plain environment appended. That is the process's whole environment: the launcher passes its
// own on, then the start command's.
func workerOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec, affinity bool) corev1.Container {
	t.Helper()
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	c := containerNamed(t, r.podTemplate(l, affinity).Spec, string(spec.Role))
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
// over the pod's own directories — worker-bin on the tree volume, then legion's — never a
// path list (P3).
func TestPIShellPrefixIsTmuxsFormOverThePodsDirectories(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(workerOf(t, r, workerSpec(t), false))
	got := kubeExpand(env["PI_SHELL_PREFIX"], env)
	want := shellprefix.For("/legion/worker-bin", "/opt/legion/bin")
	if got != want {
		t.Fatalf("PI_SHELL_PREFIX\n got: %s\nwant: %s", got, want)
	}
}

// The init containers run where the provisioning Secret is mounted, or on the tree volume every
// agent of the tree can write, so nothing either executes may come from the tree volume: each one's
// PATH names the image's directories only, and neither is told a tool path (#1258 deep review,
// finding 3).
func TestTheInitContainersPathNamesNoTreeVolumeDirectory(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, init := range podOf(t, r, workerSpec(t), false).InitContainers {
		env := envOf(init)
		path, ok := env["PATH"]
		if !ok {
			t.Fatalf("%s's PATH is left to the image; it must be stated", init.Name)
		}
		for _, dir := range filepath.SplitList(path) {
			if dir == TreeRoot || strings.HasPrefix(dir, TreeRoot+"/") {
				t.Errorf("%s's PATH names %s, on the tree volume", init.Name, dir)
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
// nothing an agent wrote reaches workspace-init's jj.
func TestBothContainersShareOneInMemoryXDGConfigHome(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	pod := podOf(t, r, workerSpec(t), false)
	init, main := containerNamed(t, pod, initContainer), workerOf(t, r, workerSpec(t), false)
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		if envOf(init)[name] == "" || envOf(init)[name] != envOf(main)[name] {
			t.Errorf("%s: init %q, main %q; the two containers must agree", name, envOf(init)[name], envOf(main)[name])
		}
	}
	configHome := envOf(main)["XDG_CONFIG_HOME"]
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

// bun (measured: bun 1.3.14, LEGION-198) resolves its install cache at
// $XDG_CACHE_HOME/.bun/install/cache when neither BUN_INSTALL nor BUN_INSTALL_CACHE_DIR is set —
// neither is ever set here — and hardlinks that cache's files into every worktree's node_modules,
// which is how one shared cache corrupted every checkout on a box at once (LEGION-198's spec). A
// pod's XDG_CACHE_HOME is mounted from no volume of this pod's — not the tree volume, the boot
// volume, the state volume, nor the in-memory config volume, at, above, or beneath it — so it
// lives on the pod's own ephemeral container filesystem alone: private to this one pod, never
// shared with the operator or another pod's, and gone with the pod. `overlaps` (operatorpod.go),
// the package's own at/under/above predicate for exactly this question, catches a volume mounted
// beneath the cache home (e.g. a tree-shared warm-cache volume at
// $XDG_CACHE_HOME/.bun/install/cache) that a bare prefix-or-equal check would miss. No code
// change is needed to keep the cache private; this test locks the absence of any overlapping
// mount so a future volume addition cannot reintroduce sharing.
func TestBunCacheHomeIsMountedFromNoVolume(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	main := workerOf(t, r, workerSpec(t), false)
	cacheHome := envOf(main)["XDG_CACHE_HOME"]
	if cacheHome == "" {
		t.Fatal("the main container carries no XDG_CACHE_HOME")
	}
	for _, mount := range main.VolumeMounts {
		if overlaps(mount.MountPath, cacheHome) {
			t.Fatalf("XDG_CACHE_HOME %s overlaps volume %q's mount %q; bun's cache would then persist or share across pods",
				cacheHome, mount.Name, mount.MountPath)
		}
	}
}

// uv links a project's .venv, in an issue's workspace on the tree volume, to an interpreter under
// UV_PYTHON_INSTALL_DIR, and installs into it from UV_CACHE_DIR. Both are on the tree volume's own
// mount (no other mount covering them) and outside the workspace, whose tree is the project's, so a
// later pod runs the .venv as it is and reuses what an earlier pod downloaded. The cache is the
// tree's, the same in every pod of it. The interpreter directory is the issue's: the same for two
// pods of one issue, and another for a second issue of the tree, whose pods may install at the same
// moment and share no lock with the first issue's across pods.
func TestUvKeepsItsPythonsAndCacheOnTheTreeVolume(t *testing.T) {
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
		main := workerOf(t, r, spec, false)
		env := envOf(main)
		for _, name := range []string{"UV_PYTHON_INSTALL_DIR", "UV_CACHE_DIR"} {
			dir := env[name]
			if mount := mountHolding(main, dir); mount == nil || mount.Name != treeVolume || mount.SubPath != "" {
				t.Errorf("%s %s: %s %q is on mount %+v, want the tree volume's own mount at %s", spec.Issue, spec.Role, name, dir, mount, TreeRoot)
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

// Oh My Pi copies its own environment once for every `gh` it runs to serve a pr:// or issue://
// read, so the worker container is told LEGION_GRANT_FILE from its start; the extension writes a
// grant there before each such call (LEGION-262). The file is the claim's, on the state volume in
// memory — never on the tree volume every agent of the tree can read — and no init container,
// none of which redeems a grant, is told one.
func TestTheWorkerContainerNamesItsGrantFileInMemory(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	pod := podOf(t, r, spec, false)
	main := workerOf(t, r, spec, false)
	want := StateDir + "/secrets/" + string(spec.Claim) + "-grant"
	if got := envOf(main)["LEGION_GRANT_FILE"]; got != want {
		t.Fatalf("the worker container's LEGION_GRANT_FILE = %q, want %q", got, want)
	}
	for _, init := range pod.InitContainers {
		if got, ok := envOf(init)["LEGION_GRANT_FILE"]; ok {
			t.Errorf("init container %s is told LEGION_GRANT_FILE=%q; no init container redeems a grant", init.Name, got)
		}
	}
	var volume string
	if mount := mountHolding(main, want); mount != nil {
		volume = mount.Name
	}
	for _, v := range pod.Volumes {
		if v.Name == volume && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory {
			return
		}
	}
	t.Fatalf("the grant file %s is on volume %q, which is not an in-memory emptyDir", want, volume)
}

// The provisioning token never shares a process with anything a tree agent can write (Stage 4b
// Task 4b.6b): the claim's Secret projects it into workspace-fetch alone, the only container told
// where it is, and no other container can write a volume workspace-fetch mounts — the feed it
// fills is read-only in workspace-init, and its TMPDIR, where its one-shot credential goes, is an
// in-memory volume no other container mounts. So workspace-fetch mounts neither the tree volume
// nor the config home. Every pod's manifest holds to it, by every route Kubernetes offers into a
// Secret, the worker's container included.
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
			pod := r.podTemplate(l, tc.colocate).Spec
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
			fetch := containerNamed(t, pod, fetchContainer)
			for _, mount := range fetch.VolumeMounts {
				for _, other := range containers {
					if other.Name == fetch.Name {
						continue
					}
					for _, theirs := range other.VolumeMounts {
						if theirs.Name == mount.Name && !theirs.ReadOnly {
							t.Errorf("%s can write volume %s, which %s mounts at %s", other.Name, mount.Name, fetch.Name, mount.MountPath)
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
		"session off volume":  {func(s *runtime.SpawnSpec) { s.ResumeSessionFile = "/home/legion/elsewhere.jsonl" }, "cannot be resumed on this runtime"},
		"runtime-owned env":   {func(s *runtime.SpawnSpec) { s.Env["PATH"] = "/bin" }, "Env sets PATH"},
		"credential in env":   {func(s *runtime.SpawnSpec) { s.Env["GH_TOKEN"] = "x" }, "credential-shaped"},
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

// New refuses options no cluster could run: an image not pinned by digest, a tree volume with no
// storage class on a cluster that has no default, a stream pods cannot dial, a pool the runtime
// does not choose.
func TestNewRefusesOptionsNoPodCouldRun(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Options)
		want string
	}{
		"tag image":         {func(o *Options) { o.Image = "ghcr.io/sjawhar/legion-worker:latest" }, "not pinned by digest"},
		"no class":          {func(o *Options) { o.StorageClass = "" }, "no storage class"},
		"no tree volume":    {func(o *Options) { o.TreeVolume = resource.Quantity{} }, "no tree volume size"},
		"unix stream":       {func(o *Options) { o.StreamURL = "unix:///run/legion.sock" }, "is not tcp://host:port"},
		"relative tool":     {func(o *Options) { o.Tools.Git = "git" }, "git path \"git\" is not absolute"},
		"bad project":       {func(o *Options) { o.Project = "s4a run" }, "is not a label value"},
		"url no bearer":     {func(o *Options) { o.DispatchURL = "https://dispatch.internal" }, "configured together"},
		"bearer no url":     {func(o *Options) { o.DispatchToken = "dispatch-bearer" }, "configured together"},
		"another pool":      {func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: "gpu"} }, "legion.dev/pool is the runtime's"},
		"the pool restated": {func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: poolValue} }, "legion.dev/pool is the runtime's"},
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

// runtimeOwned is exactly what the worker container is told by the runtime itself: every name
// mainEnvironment sets with Env empty and no secret of the spec's, every optional value configured.
// A name added to the environment and not to runtimeOwned is one a spec could override; a name left
// in runtimeOwned that the environment no longer sets is one a spec is refused for nothing.
func TestRuntimeOwnedIsWhatTheWorkerContainerIsToldByTheRuntime(t *testing.T) {
	opts := testOptions()
	opts.DispatchURL, opts.DispatchToken = "https://dispatch.internal", "dispatch-bearer"
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	spec.Env, spec.Secrets = nil, nil
	told := map[string]bool{}
	for name := range envOf(workerOf(t, r, spec, false)) {
		told[name] = true
	}
	if !maps.Equal(told, runtimeOwned) {
		t.Errorf("the worker container is told %v by the runtime, and runtimeOwned is %v",
			slices.Sorted(maps.Keys(told)), slices.Sorted(maps.Keys(runtimeOwned)))
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
			pointer, pointed := envOf(workerOf(t, r, workerSpec(t), false))["DISPATCH_TOKEN_FILE"]
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
			pod := podOf(t, r, tc.spec, false)
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
			spec := r.podTemplate(l, false).Spec
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
			worker := workerOf(t, r, manifestCases(t)["root"].spec, false)
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
// alone in its volume, and the account the pods run as.
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
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "operator-creds", MountPath: "/etc/legion-operator/creds", ReadOnly: true},
			{Name: "operator-config", MountPath: ompAgentDir + "/models.yml", SubPath: "models.yml", ReadOnly: true},
			{Name: "operator-token", MountPath: "/var/run/operator", ReadOnly: true},
		},
		ServiceAccount: "operator-worker",
	}
}

// The operator's pod (runtime.kubernetes.pod) reaches every pod Legion runs — the root's, a
// worker's, and the image probe's — exactly as configured: its volumes beside Legion's, its
// variables and mounts in the agent's container alone, never in an init container, and its account
// as the pod's.
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
		{"root", podOf(t, r, rootSpec(t), false), workerOf(t, r, rootSpec(t), false)},
		{"worker", podOf(t, r, workerSpec(t), true), workerOf(t, r, workerSpec(t), true)},
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
			worker := r.podTemplate(l, false).Spec
			probe := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
			workerProcess := workerOf(t, r, spec, false)
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
// of a spec's or the operator's. A piece added to a pod and not to its list is one an operator
// could collide with unrefused; one left in a list that no pod carries is refused for nothing.
func TestLegionsOwnNamesAreWhatItsPodsCarry(t *testing.T) {
	opts := goldenOptions()
	opts.DispatchURL, opts.DispatchToken = "https://dispatch.internal", "dispatch-bearer"
	opts.ProviderKeys = map[string]string{"ANTHROPIC_API_KEY": "anthropic"}
	opts.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
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
	for _, tc := range manifestCases(t) {
		spec := tc.spec
		spec.Env, spec.Secrets = nil, nil
		pod := podOf(t, r, spec, tc.colocate)
		carry(pod, workerOf(t, r, spec, tc.colocate))
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
// mount there would hide: Oh My Pi, the Legion plugin the agent loads, the Go legion every
// container runs, the profile's installed plugins, and the databases Oh My Pi keeps in the
// profile's agent directory.
func TestImageOwnedPathsCoverWhatAPodRunsFromTheImage(t *testing.T) {
	for _, used := range []string{
		defaultAgent, legionPlugin, testOptions().Tools.Legion,
		ompProfileDir + "/plugins/node_modules", ompAgentDir + "/agent.db", ompAgentDir + "/models.db",
	} {
		if !slices.ContainsFunc(imageOwnedPaths(), func(owned string) bool {
			return used == owned || strings.HasPrefix(used, owned+"/")
		}) {
			t.Errorf("%s is not under imageOwnedPaths %v", used, imageOwnedPaths())
		}
	}
}

// Every tree pod refuses a node that holds a pod of another tree (decision 2: the pool's floor
// sizes the node for one tree), whether or not it also requires its own tree's node, and a pod
// with no tree label — the image probe — never counts against it.
func TestEveryTreePodKeepsOffAnotherTreesNode(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := []corev1.PodAffinityTerm{{
		LabelSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: labelTree, Operator: metav1.LabelSelectorOpExists},
			{Key: labelTree, Operator: metav1.LabelSelectorOpNotIn, Values: []string{testTree}},
		}},
		TopologyKey: corev1.LabelHostname,
	}}
	for name, tc := range map[string]struct {
		spec     runtime.SpawnSpec
		affinity bool
	}{
		"root, alone":             {rootSpec(t), false},
		"worker, beside its tree": {workerSpec(t), true},
		"worker, alone":           {workerSpec(t), false},
	} {
		t.Run(name, func(t *testing.T) {
			pod := podOf(t, r, tc.spec, tc.affinity)
			if pod.Affinity == nil || pod.Affinity.PodAntiAffinity == nil ||
				!reflect.DeepEqual(pod.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, want) ||
				len(pod.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0 {
				t.Fatalf("pod anti-affinity %+v, want exactly the required term %+v", pod.Affinity, want)
			}
			if got := pod.Affinity.PodAffinity != nil; got != tc.affinity {
				t.Errorf("the pod requires its own tree's node: %t, want %t", got, tc.affinity)
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
		"PI_SHELL_PREFIX": shellprefix.For(workerBin, filepath.Dir(r.tools.Legion)),
	} {
		if got := env[name]; got != want {
			t.Errorf("%s reaches the agent as %q, want %q", name, got, want)
		}
	}
	role := containerNamed(t, r.podTemplate(l, false).Spec, string(spec.Role))
	for _, arg := range role.Command {
		if expanded := kubeExpand(arg, envOf(role)); strings.ReplaceAll(arg, "$$", "$") != expanded {
			t.Errorf("the launcher's argument %q reaches it as %q", arg, expanded)
		}
	}
	if !strings.Contains(l.prompt, literal) {
		t.Fatalf("the system prompt does not carry the instructions as written: %q", l.prompt)
	}
}
