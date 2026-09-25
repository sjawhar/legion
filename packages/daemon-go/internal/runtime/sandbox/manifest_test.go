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
// placed beside a scheduled pod of its tree, and one placed with none; a resume; and a relaunch
// whose workspace is recovered after its volume was lost. colocate is whether another pod of the
// tree is scheduled when the launch runs.
func manifestCases(t *testing.T) map[string]struct {
	spec     runtime.SpawnSpec
	colocate bool
} {
	resume := workerSpec(t)
	resume.Generation, resume.BootToken, resume.ResumeSessionFile = 2, "boot-g2", resumeSession
	recovered := workerSpec(t)
	recovered.WorkspaceRecoveredFrom = "legion/LEGION-208"
	return map[string]struct {
		spec     runtime.SpawnSpec
		colocate bool
	}{
		"root":               {rootSpec(t), false},
		"worker-affinity":    {workerSpec(t), true},
		"worker-no-affinity": {workerSpec(t), false},
		"resume":             {resume, true},
		"recovered":          {recovered, true},
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
	probe, err := encodeProbe(r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)))
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

// PI_SHELL_PREFIX is a shell command Oh My Pi's bash tool runs before each command, in tmux's form
// over the pod's own directories — worker-bin on the tree volume, then the Go legion's — never a
// path list (P3).
func TestPIShellPrefixIsTmuxsFormOverThePodsDirectories(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(podOf(t, r, workerSpec(t), false).Containers[0])
	got := kubeExpand(env["PI_SHELL_PREFIX"], env)
	want := `PATH='/legion/worker-bin:/opt/legion/go/bin:'${PATH#'/legion/worker-bin:/opt/legion/go/bin:'} &&`
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
	init, main := containerNamed(t, pod, initContainer), containerNamed(t, pod, mainContainer)
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
	main := pod.Containers[0]
	want := StateDir + "/secrets/" + string(spec.Claim) + "-grant"
	if got := envOf(main)["LEGION_GRANT_FILE"]; got != want {
		t.Fatalf("the worker container's LEGION_GRANT_FILE = %q, want %q", got, want)
	}
	for _, init := range pod.InitContainers {
		if got, ok := envOf(init)["LEGION_GRANT_FILE"]; ok {
			t.Errorf("init container %s is told LEGION_GRANT_FILE=%q; no init container redeems a grant", init.Name, got)
		}
	}
	// The deepest mount holding the path is the volume the file lands on.
	var volume, at string
	for _, mount := range main.VolumeMounts {
		if strings.HasPrefix(want, mount.MountPath+"/") && len(mount.MountPath) > len(at) {
			volume, at = mount.Name, mount.MountPath
		}
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
		"no repository":       {func(s *runtime.SpawnSpec) { s.Repository = "" }, "no repository"},
		"not owner/repo":      {func(s *runtime.SpawnSpec) { s.Repository = "sjawhar/legion-smoke/extra" }, "must be owner/repository"},
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
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	spec.Env, spec.Secrets = nil, nil
	told := map[string]bool{}
	for name := range envOf(podOf(t, r, spec, false).Containers[0]) {
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
			pointer, pointed := envOf(r.podTemplate(l, false).Spec.Containers[0])["DISPATCH_TOKEN_FILE"]
			if got := l.secrets[dispatchTokenKey]; got != tc.bearer || pointed != (tc.bearer != "") {
				t.Fatalf("the claim's Secret carries %q as %s and DISPATCH_TOKEN_FILE is %q (set: %t), want %q",
					got, dispatchTokenKey, pointer, pointed, tc.bearer)
			}
			if pointed && pointer != BootDir+"/"+dispatchTokenKey {
				t.Fatalf("DISPATCH_TOKEN_FILE = %q, want the boot projection's %s", pointer, BootDir+"/"+dispatchTokenKey)
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
			for _, name := range []string{fetchContainer, mainContainer} {
				if _, set := envOf(containerNamed(t, pod, name))["LEGION_WORKSPACE_RECOVERED_FROM"]; set {
					t.Errorf("%s carries LEGION_WORKSPACE_RECOVERED_FROM", name)
				}
			}
		})
	}
}

// A pod holds no token Legion projects: it runs as the operator's ServiceAccount, the namespace's
// default when the operator names none, with the API server's own token never mounted and no
// projected volume of Legion's (the one token a pod carries, if any, is the operator's own).
func TestAPodRunsAsTheOperatorsAccountWithNoTokenOfLegions(t *testing.T) {
	for name, account := range map[string]string{"the operator's account": "operator-worker", "none named": ""} {
		t.Run(name, func(t *testing.T) {
			opts := goldenOptions()
			opts.Pod.ServiceAccount = account
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			probe := r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec
			for pod, spec := range map[string]corev1.PodSpec{"root": podOf(t, r, rootSpec(t), false), "worker": podOf(t, r, workerSpec(t), true), "probe": probe} {
				if spec.ServiceAccountName != account {
					t.Errorf("%s: serviceAccountName = %q, want %q", pod, spec.ServiceAccountName, account)
				}
				if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
					t.Errorf("%s: the API server's service account token is mounted", pod)
				}
				for _, volume := range spec.Volumes {
					if volume.Projected != nil {
						t.Errorf("%s: Legion projects volume %+v", pod, volume)
					}
				}
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
	for _, tc := range []struct {
		name  string
		pod   corev1.PodSpec
		agent string
	}{
		{"root", podOf(t, r, rootSpec(t), false), mainContainer},
		{"worker", podOf(t, r, workerSpec(t), true), mainContainer},
		{"probe", r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec, probeContainer},
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
			agent := containerNamed(t, tc.pod, tc.agent)
			for _, want := range opts.Pod.VolumeMounts {
				if !slices.ContainsFunc(agent.VolumeMounts, func(m corev1.VolumeMount) bool { return reflect.DeepEqual(m, want) }) {
					t.Errorf("%s mounts %+v, want %+v among them", tc.agent, agent.VolumeMounts, want)
				}
			}
			if got := envOf(agent)["PI_CONFIG_FILES"]; got != opts.Pod.Env["PI_CONFIG_FILES"] {
				t.Errorf("%s's PI_CONFIG_FILES = %q, want the operator's %q", tc.agent, got, opts.Pod.Env["PI_CONFIG_FILES"])
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

// With provider keys, every pod Legion runs mounts exactly the configured keys of the providers
// Secret — the TypeScript runtime's legion-<project token>-providers — each as a file named for the
// variable Oh My Pi reads, read-only at ProvidersDir in the agent's container alone, and the
// worker's shim exports that directory into Oh My Pi's environment (--provider-env-dir); with none,
// no pod mounts the Secret and no shim is told a directory.
func TestProviderKeysReachEveryPodAsTheProvidersSecretsFiles(t *testing.T) {
	const providersSecret = "legion-" + testProject + "-providers"
	for name, keys := range map[string]map[string]string{
		"configured": {"GEMINI_API_KEY": "gemini", "ANTHROPIC_API_KEY": "anthropic"},
		"none":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			opts.ProviderKeys = keys
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			worker := podOf(t, r, workerSpec(t), false)
			for _, tc := range []struct {
				pod   corev1.PodSpec
				agent string
			}{
				{worker, mainContainer},
				{r.probeManifest("legion-probe", ImageProbe{Contract: 5}, time.Time{}).Spec.PodTemplate.Spec, probeContainer},
			} {
				var volumes []corev1.Volume
				for _, volume := range tc.pod.Volumes {
					if volume.Secret != nil && volume.Secret.SecretName == providersSecret {
						volumes = append(volumes, volume)
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
				agent := containerNamed(t, tc.pod, tc.agent)
				if keys == nil {
					if len(volumes) != 0 || len(mounted(agent)) != 0 {
						t.Errorf("%s: with no provider keys the pod has %+v and mounts %+v", tc.agent, volumes, mounted(agent))
					}
					continue
				}
				want := &corev1.SecretVolumeSource{
					SecretName:  providersSecret,
					Items:       []corev1.KeyToPath{{Key: "anthropic", Path: "ANTHROPIC_API_KEY"}, {Key: "gemini", Path: "GEMINI_API_KEY"}},
					DefaultMode: new(int32(0o440)),
				}
				if len(volumes) != 1 || !reflect.DeepEqual(volumes[0].Secret, want) {
					t.Fatalf("%s's pod holds the providers Secret as %+v, want once as %+v", tc.agent, volumes, *want)
				}
				if got := mounted(agent); len(got) != 1 || got[0].MountPath != ProvidersDir || !got[0].ReadOnly || got[0].SubPath != "" {
					t.Errorf("%s mounts the providers Secret as %+v, want once, read-only, at %s", tc.agent, got, ProvidersDir)
				}
				for _, init := range tc.pod.InitContainers {
					if got := mounted(init); len(got) != 0 {
						t.Errorf("%s mounts the providers Secret: %+v", init.Name, got)
					}
				}
			}
			main := containerNamed(t, worker, mainContainer)
			shim := main.Command[:slices.Index(main.Command, "--")]
			at := slices.Index(shim, "--provider-env-dir")
			switch {
			case keys == nil && at >= 0:
				t.Errorf("with no provider keys the shim is told %v", shim)
			case keys != nil && (at < 0 || at+1 == len(shim) || shim[at+1] != ProvidersDir):
				t.Errorf("the shim runs as %v, want --provider-env-dir %s", shim, ProvidersDir)
			}
		})
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
		carry(pod, containerNamed(t, pod, mainContainer))
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

// Whatever text a launch carries reaches the process as written, although the kubelet expands
// `$(NAME)` and `$$` in a container's command and env values: the inlined system prompt, an
// operator's instructions, and a spec's env values mean what they say in a pod as in a pane.
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
	main := r.podTemplate(l, false).Spec.Containers[0]
	env := envOf(main)
	argv := l.agentArgv(r.agent)
	for i, arg := range main.Command[len(main.Command)-len(argv):] {
		if got := kubeExpand(arg, env); got != argv[i] {
			t.Errorf("the agent's argument #%d reaches it as %q, want %q", i, got, argv[i])
		}
	}
	for name, want := range map[string]string{
		"LEGION_E2E_NOTE": literal,
		"PI_SHELL_PREFIX": shellprefix.For(workerBin, filepath.Dir(r.tools.Legion)),
	} {
		if got := kubeExpand(env[name], env); got != want {
			t.Errorf("%s reaches the agent as %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(l.prompt, literal) {
		t.Fatalf("the system prompt does not carry the instructions as written: %q", l.prompt)
	}
}
