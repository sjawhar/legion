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
// whose workspace is recovered after its volume was lost.
func manifestCases(t *testing.T) map[string]struct {
	spec     runtime.SpawnSpec
	affinity bool
} {
	resume := workerSpec(t)
	resume.Generation, resume.BootToken, resume.ResumeSessionFile = 2, "boot-g2", resumeSession
	recovered := workerSpec(t)
	recovered.WorkspaceRecoveredFrom = "legion/LEGION-208"
	return map[string]struct {
		spec     runtime.SpawnSpec
		affinity bool
	}{
		"root":               {rootSpec(t), false},
		"worker-affinity":    {workerSpec(t), true},
		"worker-no-affinity": {workerSpec(t), false},
		"resume":             {resume, true},
		"recovered":          {recovered, true},
	}
}

// manifestOf is the Sandbox the runtime creates for spec, as the API server receives it.
func manifestOf(t *testing.T, r *Runtime, spec runtime.SpawnSpec, affinity bool) any {
	t.Helper()
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	u, err := encodeSandbox(r.sandboxManifest(l, affinity))
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
			if err := encoder.Encode(manifestOf(t, r, tc.spec, tc.affinity)); err != nil {
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
			if found := schemaViolations(manifestOf(t, r, tc.spec, tc.affinity), schema, ""); len(found) > 0 {
				t.Fatalf("the manifest has fields the CRD does not declare as sent:\n%s", strings.Join(found, "\n"))
			}
		})
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

// PI_SHELL_PREFIX is a shell command Oh My Pi's bash tool runs before each command, in tmux's form
// over the pod's own directories — worker-bin on the tree volume, then the Go legion's — never a
// path list (P3).
func TestPIShellPrefixIsTmuxsFormOverThePodsDirectories(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	got := envOf(podOf(t, r, workerSpec(t), false).Containers[0])["PI_SHELL_PREFIX"]
	want := `PATH='/legion/worker-bin:/opt/legion/go/bin:'${PATH#'/legion/worker-bin:/opt/legion/go/bin:'} &&`
	if got != want {
		t.Fatalf("PI_SHELL_PREFIX\n got: %s\nwant: %s", got, want)
	}
}

// The init container runs with the provisioning credential, so nothing it executes may come from
// the tree volume, which every agent of the tree can write: its PATH names the image's directories
// only, and it is told no tool path (#1258 deep review, finding 3).
func TestTheInitContainersPathNamesNoTreeVolumeDirectory(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	init := podOf(t, r, workerSpec(t), false).InitContainers[0]
	env := envOf(init)
	path, ok := env["PATH"]
	if !ok {
		t.Fatal("the init container's PATH is left to the image; it must be stated")
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == TreeRoot || strings.HasPrefix(dir, TreeRoot+"/") {
			t.Errorf("the init container's PATH names %s, on the tree volume", dir)
		}
	}
	for name := range env {
		if strings.HasPrefix(name, "LEGION_") && strings.HasSuffix(name, "_PATH") {
			t.Errorf("the init container is told %s; it resolves its tools from the image's PATH", name)
		}
	}
}

// jj keeps a repository's `--repo` configuration under $XDG_CONFIG_HOME, so what workspace-init
// sets there reaches the agent's jj only when both containers have one config home, on a volume
// both mount (#1258 deep review, finding 4). It is in memory, so every pod's starts empty and
// nothing an agent wrote reaches the init container's jj.
func TestBothContainersShareOneInMemoryXDGConfigHome(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	pod := podOf(t, r, workerSpec(t), false)
	init, main := pod.InitContainers[0], pod.Containers[0]
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

// The provisioning token reaches the init container alone, through its own projection of the
// claim's Secret, and never lands on the tree volume: workspace-init keeps its credential under
// TMPDIR, an in-memory volume of its own (#1258 review).
func TestTheProvisionTokenReachesOnlyTheInitContainer(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	pod := podOf(t, r, workerSpec(t), false)
	init, main := pod.InitContainers[0], pod.Containers[0]
	volumes := map[string]corev1.Volume{}
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume
	}
	projects := func(c corev1.Container, key string) bool {
		for _, mount := range c.VolumeMounts {
			if secret := volumes[mount.Name].Secret; secret != nil {
				for _, item := range secret.Items {
					if item.Key == key {
						return true
					}
				}
			}
		}
		return false
	}
	if !projects(init, provisionTokenKey) || projects(main, provisionTokenKey) {
		t.Fatalf("the provision token is projected into the init container: %t, the main container: %t",
			projects(init, provisionTokenKey), projects(main, provisionTokenKey))
	}
	if _, ok := envOf(main)["LEGION_PROVISION_TOKEN_FILE"]; ok {
		t.Fatal("the main container is pointed at the provision token")
	}
	temp := envOf(init)["TMPDIR"]
	if temp == "" || temp == TreeRoot || strings.HasPrefix(temp, TreeRoot+"/") {
		t.Fatalf("the init container's TMPDIR %q is on the tree volume or unset", temp)
	}
	for _, mount := range init.VolumeMounts {
		if mount.MountPath == temp {
			if dir := volumes[mount.Name].EmptyDir; dir == nil || dir.Medium != corev1.StorageMediumMemory {
				t.Fatalf("TMPDIR %s is mounted from %+v, not an in-memory emptyDir", temp, volumes[mount.Name].VolumeSource)
			}
			return
		}
	}
	t.Fatalf("TMPDIR %s is not a mounted volume", temp)
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
		"tag image":           {func(o *Options) { o.Image = "ghcr.io/sjawhar/legion-worker:latest" }, "not pinned by digest"},
		"no class":            {func(o *Options) { o.StorageClass = "" }, "no storage class"},
		"no tree volume":      {func(o *Options) { o.TreeVolume = resource.Quantity{} }, "no tree volume size"},
		"unix stream":         {func(o *Options) { o.StreamURL = "unix:///run/legion.sock" }, "is not tcp://host:port"},
		"relative tool":       {func(o *Options) { o.Tools.Git = "git" }, "git path \"git\" is not absolute"},
		"bad project":         {func(o *Options) { o.Project = "s4a run" }, "is not a label value"},
		"url no bearer":       {func(o *Options) { o.DispatchURL = "https://dispatch.internal" }, "configured together"},
		"bearer no url":       {func(o *Options) { o.DispatchToken = "dispatch-bearer" }, "configured together"},
		"no gateway URL":      {func(o *Options) { o.Gateway.URL = "" }, "no model gateway URL"},
		"no gateway audience": {func(o *Options) { o.Gateway.Audience = "" }, "no model gateway audience"},
		"no service account":  {func(o *Options) { o.Gateway.ServiceAccount = "" }, "no ServiceAccount"},
		"token below minimum": {func(o *Options) { o.Gateway.TokenExpiry = 599 * time.Second }, "at least 10m0s"},
		"another pool":        {func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: "gpu"} }, "legion.dev/pool is the runtime's"},
		"the pool restated":   {func(o *Options) { o.Scheduling.NodeSelector = map[string]string{poolKey: poolValue} }, "legion.dev/pool is the runtime's"},
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
// (LEGION_WORKSPACE_RECOVERED_FROM, cmd/legion/workspace_init.go): the ref reaches the init
// container alone, never the agent, and a launch recovering nothing names none.
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
			got, set := envOf(pod.InitContainers[0])["LEGION_WORKSPACE_RECOVERED_FROM"]
			if got != tc.want || set != (tc.want != "") {
				t.Errorf("the init container's LEGION_WORKSPACE_RECOVERED_FROM = %q (set: %t), want %q", got, set, tc.want)
			}
			if _, set := envOf(pod.Containers[0])["LEGION_WORKSPACE_RECOVERED_FROM"]; set {
				t.Error("the agent's container carries LEGION_WORKSPACE_RECOVERED_FROM")
			}
		})
	}
}

// A pod reaches the models through the gateway as the Gateway's ServiceAccount (decision 1, C6):
// the one credential it holds there is a projected token for the gateway's audience, which the
// kubelet rotates within TokenExpiry, mounted read-only at GatewayDir in the worker container
// alone, beside the gateway's URL; the API server's own token is never mounted.
func TestAPodReachesTheModelGatewayAsItsServiceAccount(t *testing.T) {
	opts := goldenOptions()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	want := corev1.VolumeProjection{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
		Audience: opts.Gateway.Audience, ExpirationSeconds: new(int64(600)), Path: "token",
	}}
	for name, spec := range map[string]runtime.SpawnSpec{"root": rootSpec(t), "worker": workerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			pod := podOf(t, r, spec, false)
			if pod.ServiceAccountName != opts.Gateway.ServiceAccount {
				t.Errorf("serviceAccountName = %q, want %q", pod.ServiceAccountName, opts.Gateway.ServiceAccount)
			}
			if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
				t.Error("the API server's service account token is mounted")
			}
			var projected []corev1.Volume
			for _, volume := range pod.Volumes {
				if volume.Projected != nil {
					projected = append(projected, volume)
				}
			}
			if len(projected) != 1 || len(projected[0].Projected.Sources) != 1 ||
				!reflect.DeepEqual(projected[0].Projected.Sources[0], want) {
				t.Fatalf("projected volumes %+v, want exactly one holding only %+v", projected, *want.ServiceAccountToken)
			}
			mounted := func(c corev1.Container) []corev1.VolumeMount {
				var mounts []corev1.VolumeMount
				for _, mount := range c.VolumeMounts {
					if mount.Name == projected[0].Name {
						mounts = append(mounts, mount)
					}
				}
				return mounts
			}
			main, init := pod.Containers[0], pod.InitContainers[0]
			if got := mounted(main); len(got) != 1 || got[0].MountPath != GatewayDir || !got[0].ReadOnly {
				t.Errorf("the worker container mounts the token volume as %+v, want once, read-only, at %s", got, GatewayDir)
			}
			if got := mounted(init); len(got) != 0 {
				t.Errorf("the init container mounts the token volume: %+v", got)
			}
			if got := envOf(main)["LEGION_MODEL_GATEWAY_URL"]; got != opts.Gateway.URL {
				t.Errorf("the worker container's LEGION_MODEL_GATEWAY_URL = %q, want %q", got, opts.Gateway.URL)
			}
			if _, set := envOf(init)["LEGION_MODEL_GATEWAY_URL"]; set {
				t.Error("the init container is told the gateway's URL")
			}
		})
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
