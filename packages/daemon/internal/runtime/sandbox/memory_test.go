package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// memoryDir is where the operator mounts the ConfigMap holding the project's memory overlays, and
// memoryOverlays are the `memory.config_files` of every test here, in the order the operator wrote
// them.
const memoryDir = "/etc/legion-memory"

var memoryOverlays = []string{memoryDir + "/memory.yml", memoryDir + "/recall.yml"}

// memoryMount is that ConfigMap's volume and its mount at dir, which the operator's pod block adds
// to every pod, as the volume holding a role's own overlays does (roleconfig_test).
func memoryMount(dir string) (corev1.Volume, corev1.VolumeMount) {
	volume := corev1.Volume{Name: "operator-memory", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-memory"},
	}}}
	return volume, corev1.VolumeMount{Name: volume.Name, MountPath: dir, ReadOnly: true}
}

// memoryRuntime is the test runtime over the operator's pod, which mounts the memory ConfigMap at
// memoryDir and names roleFiles as role_config_files, given files as the project's memory
// overlays. A nil list is a daemon whose project names no `memory` block at all, or one at mode
// off, which the daemon hands none.
func memoryRuntime(t *testing.T, files []string) *Runtime {
	t.Helper()
	return runtimeWithOptions(t, func(o *Options) {
		volume, mount := memoryMount(memoryDir)
		o.Pod.Volumes = append(o.Pod.Volumes, volume)
		o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, mount)
		o.RoleConfigFiles = roleFiles
		o.MemoryConfigFiles = files
	})
}

// memorySpec is a launch for an issue whose tree was drawn into the memory group.
func memorySpec(t *testing.T) runtime.SpawnSpec {
	t.Helper()
	spec := workerSpec(t)
	spec.Memory = true
	return spec
}

// A role whose tree was drawn into the memory group is told the project's memory overlays last in
// PI_CONFIG_FILES: the operator's own first, then the role's own, then the memory block's, in the
// order the operator wrote them, and the shim hands that value to Oh My Pi untouched. A launch for
// a tree in no memory group is told the operator's and the role's own alone.
func TestAMemoryGroupRoleIsToldTheMemoryOverlaysLast(t *testing.T) {
	r := memoryRuntime(t, memoryOverlays)
	own := operatorPod().Env["PI_CONFIG_FILES"] + ":" + strings.Join(roleFiles[claim.RoleTester], ":")
	if got, want := envOf(workerOf(t, r, memorySpec(t)))["PI_CONFIG_FILES"], own+":"+strings.Join(memoryOverlays, ":"); got != want {
		t.Errorf("the memory-group tester's PI_CONFIG_FILES = %q, want %q", got, want)
	}
	if got := envOf(workerOf(t, r, workerSpec(t)))["PI_CONFIG_FILES"]; got != own {
		t.Errorf("a no-memory tester's PI_CONFIG_FILES = %q, want %q", got, own)
	}
}

// The system prompt of a memory-group role ends with the project's memory instructions: the role
// prompts, the addressing sentence, the deployment's own instructions, then the memory's, each
// separated by a blank line and its trailing newlines dropped. A launch given none ends at the
// deployment's own.
func TestTheMemoryInstructionsAreTheLastFragmentOfTheSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	parts := runtime.PromptParts{
		RolePromptPaths:            []string{write("role.md", "You are the tester.\n")},
		Addressing:                 "Legion addressing: your role topic is `notifications.role.worker`.",
		DeploymentInstructionsPath: write("instructions.md", "Deployment instructions.\n"),
		MemoryInstructionsPath:     write("memory.md", "Read the tree's memory.\n\n"),
	}
	got, err := systemPrompt(parts)
	if err != nil {
		t.Fatal(err)
	}
	want := "You are the tester.\n\n" + parts.Addressing + "\n\nDeployment instructions.\n\nRead the tree's memory."
	if got != want {
		t.Errorf("the memory-group role's system prompt =\n%q\nwant\n%q", got, want)
	}
	parts.MemoryInstructionsPath = ""
	if got, err := systemPrompt(parts); err != nil || got != strings.TrimSuffix(want, "\n\nRead the tree's memory.") {
		t.Errorf("a launch told no memory instructions = %q, %v, want the prompt to end at the deployment's own", got, err)
	}
}

// A pod created before the memory ConfigMap was mounted holds none of the memory overlays, so the
// launch that keeps it tells its memory-group role the operator's and the role's own alone: Oh My
// Pi refuses to start on an entry that is not there, and a role that never starts never answers
// the update round that is to replace its pod. A pod built with the mount holds them all, and its
// role is told the whole list.
func TestAKeptPodBuiltBeforeTheMemoryMountIsToldNoMemoryOverlay(t *testing.T) {
	before := runtimeWithOptions(t, func(o *Options) { o.RoleConfigFiles = roleFiles })
	after := memoryRuntime(t, memoryOverlays)
	own := operatorPod().Env["PI_CONFIG_FILES"] + ":" + strings.Join(roleFiles[claim.RoleTester], ":")
	for _, tc := range []struct {
		name   string
		built  *Runtime
		memory []string
		want   string
	}{
		{"a pod built before the memory mount", before, nil, own},
		{"a pod built with it", after, memoryOverlays, own + ":" + strings.Join(memoryOverlays, ":")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := memorySpec(t)
			s := &sandbox{Spec: sandboxSpec{PodTemplate: podTemplate{Spec: podOf(t, tc.built, spec)}}}
			l := launchOf(t, after, spec)
			kept, readable := after.keptBy(s, l)
			if !readable {
				t.Fatal("keptBy could not read the pod's template")
			}
			if !slices.Equal(kept.overlays, roleFiles[claim.RoleTester]) {
				t.Errorf("the kept pod holds the tester's %q, want %q", kept.overlays, roleFiles[claim.RoleTester])
			}
			if !slices.Equal(kept.memory, tc.memory) {
				t.Errorf("the kept pod holds the memory overlays %q, want %q", kept.memory, tc.memory)
			}
			l.kept = &kept
			if got := envOf(corev1.Container{Env: after.roleOperatorEnv(l)})["PI_CONFIG_FILES"]; got != tc.want {
				t.Errorf("the kept launch's PI_CONFIG_FILES = %q, want %q", got, tc.want)
			}
		})
	}
}

// Moving a memory overlay's mount is drift for every issue pod of the deployment, the pod of a
// tree in no memory group among them: any tree may be drawn into the group, so one update round
// over the whole pod replaces it rather than a later launch replacing it under the roles working
// in it. The drift names the key and the file. The controller's own pod is not drifted by it: the
// controller runs in no tree, so no memory group ever names it those files. Restarting on the
// same configuration is no move.
func TestAMovedMemoryOverlaysMountDriftsEveryIssuePodOfTheDeployment(t *testing.T) {
	at := func(dir string) func(*Options) {
		return func(o *Options) {
			volume, mount := memoryMount(dir)
			o.Pod.Volumes = append(o.Pod.Volumes, volume)
			o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, mount)
			o.MemoryConfigFiles = []string{dir + "/memory.yml"}
		}
	}
	before, after := runtimeWithOptions(t, at("/etc/a")), runtimeWithOptions(t, at("/etc/b"))
	worker := workerSpec(t)
	pod := podOf(t, before, worker)
	got, moved := after.movedRoleOverlays(pod, worker.Role)
	if want := "memory.config_files /etc/b/memory.yml"; !moved || got.where != want {
		t.Errorf("%s: moved %v %+v, want a move of %s", worker.Role, moved, got, want)
	}
	if got, moved := before.movedRoleOverlays(pod, worker.Role); moved {
		t.Errorf("%s: the same configuration moved %+v", worker.Role, got)
	}

	controller := controllerSpec(t)
	if got, moved := after.movedRoleOverlays(podOf(t, before, controller), controller.Role); moved {
		t.Errorf("the controller's pod moved %+v, want no move: it is in no tree and names no memory overlay", got)
	}
}

// The image probe is told every workflow role's whole PI_CONFIG_FILES, the role's own overlays and
// then the project's memory overlays, since any tree may be drawn into the memory group, so a file
// the mounts lack refuses the boot rather than that role's first launch. The controller runs in no
// tree, so its value is its own overlays alone, and a project at mode off is probed with the
// command a deployment with no memory block is.
func TestTheImageProbeReadsEveryWorkflowRolesMemoryOverlays(t *testing.T) {
	command := func(r *Runtime) []string {
		return containerNamed(t, r.probeManifest("legion-probe", ImageProbe{Contract: 5, RoleReferences: testRoleReferences}, time.Time{}).Spec.PodTemplate.Spec, probeContainer).Command
	}
	base := command(runtimeWithRoleFiles(t, nil))
	if slices.Contains(base, bootprobe.RoleConfigFilesFlag) {
		t.Fatalf("the probe of a deployment that names no overlay at all is told %q", base)
	}
	roleOnly := slices.Clone(base)
	for _, role := range roleConfigRoles {
		if files := roleFiles[role]; len(files) > 0 {
			roleOnly = append(roleOnly, bootprobe.RoleConfigFilesFlag, bootprobe.RoleConfigFilesValue(string(role), files))
		}
	}
	if got := command(memoryRuntime(t, nil)); !slices.Equal(got, roleOnly) {
		t.Errorf("a project at mode off is probed with %q, want the roles' own overlays alone: %q", got, roleOnly)
	}
	want := slices.Clone(base)
	for _, role := range claim.Roles {
		files := slices.Concat(roleFiles[role], memoryOverlays)
		want = append(want, bootprobe.RoleConfigFilesFlag, bootprobe.RoleConfigFilesValue(string(role), files))
	}
	want = append(want, bootprobe.RoleConfigFilesFlag, bootprobe.RoleConfigFilesValue(string(claim.RoleController), roleFiles[claim.RoleController]))
	if got := command(memoryRuntime(t, memoryOverlays)); !slices.Equal(got, want) {
		t.Errorf("the probe's command = %q, want %q", got, want)
	}
}

// A launch for an issue whose tree carries no memory group builds, under a deployment whose
// project names the block, the pod a deployment that names none builds, byte for byte, and starts
// exactly the process in it: the overlays reach only the roles of the trees the draw gave them, so
// moving a project onto memory changes no other pod. Memory before the block and after it are the
// same pod.
func TestNoMemoryBeforeOrAfterTheBlockBuildsTheSamePod(t *testing.T) {
	with, without := memoryRuntime(t, memoryOverlays), memoryRuntime(t, nil)
	spec := workerSpec(t)
	if got, want := podOf(t, with, spec), podOf(t, without, spec); !reflect.DeepEqual(got, want) {
		t.Errorf("a no-memory tree's pod under the block =\n%+v\nwant the one a daemon with no block builds:\n%+v", got, want)
	}
	if got, want := workerOf(t, with, spec), workerOf(t, without, spec); !reflect.DeepEqual(got, want) {
		t.Errorf("a no-memory tree's process under the block =\n%+v\nwant the one a daemon with no block starts:\n%+v", got, want)
	}
}

// configure refuses a memory overlay PI_CONFIG_FILES cannot name, as it refuses a role's own.
func TestConfigureRefusesAMemoryOverlayPIConfigFilesCannotName(t *testing.T) {
	for _, file := range []string{"memory.yml", "/etc/a:b.yml"} {
		opts := testOptions()
		opts.MemoryConfigFiles = []string{file}
		want := `sandbox runtime: the memory overlay "` + file + `" is not an absolute path PI_CONFIG_FILES can name`
		if _, err := configure(opts); err == nil || err.Error() != want {
			t.Errorf("configure with the memory overlay %q = %v, want %q", file, err, want)
		}
	}
}
