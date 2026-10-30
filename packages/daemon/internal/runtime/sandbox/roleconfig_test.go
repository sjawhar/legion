package sandbox

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// roleFiles are role_config_files for the tester and the daemon-launched controller, each a file
// the operator's creds Secret volume holds (operatorPod).
var roleFiles = map[claim.Role][]string{
	claim.RoleTester:     {"/etc/legion-operator/creds/fallback.yml", "/etc/legion-operator/creds/tester.yml"},
	claim.RoleController: {"/etc/legion-operator/creds/fallback.yml"},
}

// runtimeWithRoleFiles is the test runtime over the operator's pod, given files as role_config_files.
func runtimeWithRoleFiles(t *testing.T, files map[claim.Role][]string) *Runtime {
	t.Helper()
	opts := testOptions()
	opts.Pod = operatorPod()
	opts.RoleConfigFiles = files
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A role role_config_files names is told its own overlays after the operator's in PI_CONFIG_FILES,
// in order — the tester in its issue pod and the daemon-launched controller in its own — and the
// shim's pod baseline hands that value to Oh My Pi untouched, as it does the operator's alone. A
// role it does not name runs exactly the process, in exactly the pod, a daemon without the key
// gives it, and so does a named role's pod: its overlays ride the start command.
func TestARoleIsToldItsOwnOverlaysAfterTheOperators(t *testing.T) {
	with, without := runtimeWithRoleFiles(t, roleFiles), runtimeWithRoleFiles(t, nil)
	operator := operatorPod().Env["PI_CONFIG_FILES"]
	for _, tc := range []struct {
		role string
		spec func(*testing.T) runtime.SpawnSpec
		want string
	}{
		{"tester", workerSpec, operator + ":/etc/legion-operator/creds/fallback.yml:/etc/legion-operator/creds/tester.yml"},
		{"controller", controllerSpec, operator + ":/etc/legion-operator/creds/fallback.yml"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			got := envOf(workerOf(t, with, tc.spec(t)))["PI_CONFIG_FILES"]
			if got != tc.want {
				t.Fatalf("the %s's PI_CONFIG_FILES = %q, want %q", tc.role, got, tc.want)
			}
			applied := podsafety.Apply([]string{"PI_CONFIG_FILES=" + got})
			if want := "PI_CONFIG_FILES=" + tc.want; !slices.Contains(applied, want) {
				t.Errorf("the %s's Oh My Pi starts on %q, want %q among them", tc.role, applied, want)
			}
			if !reflect.DeepEqual(podOf(t, with, tc.spec(t)), podOf(t, without, tc.spec(t))) {
				t.Errorf("the %s's pod differs from the one a daemon without role_config_files makes", tc.role)
			}
		})
	}
	for _, tc := range []struct {
		role string
		spec func(*testing.T) runtime.SpawnSpec
	}{{"architect", rootSpec}} {
		if got, want := workerOf(t, with, tc.spec(t)), workerOf(t, without, tc.spec(t)); !reflect.DeepEqual(got, want) {
			t.Errorf("the %s's process with another role's overlays =\n%+v\nwant the one without the key:\n%+v", tc.role, got, want)
		}
		if got := envOf(workerOf(t, with, tc.spec(t)))["PI_CONFIG_FILES"]; got != operator {
			t.Errorf("the %s's PI_CONFIG_FILES = %q, want the operator's %q alone", tc.role, got, operator)
		}
	}
}

// With no overlay in the operator's pod env, a named role's PI_CONFIG_FILES is its own overlays
// alone.
func TestARolesOwnOverlaysStandAloneWithoutTheOperators(t *testing.T) {
	opts := testOptions()
	opts.Pod = operatorPod()
	opts.Pod.Env = nil
	opts.RoleConfigFiles = roleFiles
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := envOf(workerOf(t, r, workerSpec(t)))["PI_CONFIG_FILES"], strings.Join(roleFiles[claim.RoleTester], ":"); got != want {
		t.Errorf("the tester's PI_CONFIG_FILES = %q, want %q", got, want)
	}
}

// The image probe is told each named role's overlays, one --role-config-files per role in the
// workflow's order, so it reads them in the mounts every pod carries; a deployment that names none
// probes with the command it always had.
func TestTheImageProbeIsToldEachRolesOwnOverlays(t *testing.T) {
	command := func(r *Runtime) []string {
		return containerNamed(t, r.probeManifest("legion-probe", ImageProbe{Contract: 5, RoleReferences: testRoleReferences}, time.Time{}).Spec.PodTemplate.Spec, probeContainer).Command
	}
	plain := command(runtimeWithRoleFiles(t, nil))
	if slices.Contains(plain, bootprobe.RoleConfigFilesFlag) {
		t.Fatalf("the probe without role_config_files is told %q", plain)
	}
	want := append(slices.Clone(plain),
		bootprobe.RoleConfigFilesFlag, "tester=/etc/legion-operator/creds/fallback.yml:/etc/legion-operator/creds/tester.yml",
		bootprobe.RoleConfigFilesFlag, "controller=/etc/legion-operator/creds/fallback.yml")
	if got := command(runtimeWithRoleFiles(t, roleFiles)); !slices.Equal(got, want) {
		t.Errorf("the probe's command = %q, want %q", got, want)
	}
}

// The probe pod's refusal line is the daemon's refusal, naming the role, the file and what to fix,
// not the image's.
func TestTheProbesRoleOverlayRefusalIsTheBootsRefusal(t *testing.T) {
	r := runtimeWithRoleFiles(t, roleFiles)
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: probeContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
	}}}}
	line := bootprobe.RoleOverlaysRefusedLine("tester", "/etc/legion-operator/creds/tester.yml cannot be read: stat /etc/legion-operator/creds/tester.yml: no such file or directory")
	outcome := r.judge("legion-probe", "sha256:00", pod, line+"\n", nil, 5)
	want := "the probe pod legion-probe refused role tester's Oh My Pi overlays: /etc/legion-operator/creds/tester.yml cannot be read: stat /etc/legion-operator/creds/tester.yml: no such file or directory; " +
		"the probe pod mounts runtime.kubernetes.pod's volumes as every pod does, so the tester's Oh My Pi would not start: add the file to the ConfigMap its mount serves, or correct role_config_files.tester — log tail: " + line + "\n"
	if outcome.Refusal == nil || outcome.Refusal.Error() != want {
		t.Errorf("judge = %+v, want the refusal %q", outcome, want)
	}
}

// New refuses role overlays no Legion role or PI_CONFIG_FILES could carry.
func TestNewRefusesRoleOverlaysNoRoleCouldCarry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[claim.Role][]string
		want  string
	}{
		{"an unknown role", map[claim.Role][]string{"coder": {"/etc/a.yml"}}, `sandbox runtime: Oh My Pi overlays for "coder", which is not a Legion role`},
		{"a relative file", map[claim.Role][]string{claim.RoleMerger: {"a.yml"}}, `sandbox runtime: merger's Oh My Pi overlay "a.yml" is not an absolute path PI_CONFIG_FILES can name`},
		{"the separator", map[claim.Role][]string{claim.RoleMerger: {"/etc/a:b.yml"}}, `sandbox runtime: merger's Oh My Pi overlay "/etc/a:b.yml" is not an absolute path PI_CONFIG_FILES can name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.RoleConfigFiles = tc.files
			if _, err := configure(opts); err == nil || err.Error() != tc.want {
				t.Fatalf("configure = %v, want %q", err, tc.want)
			}
		})
	}
}

// Adding a role's overlay to a running deployment — the operator's volume and mount holding it, and
// role_config_files naming it — leaves every issue pod created before without that mount, while the
// role's next generation is started with the file in its PI_CONFIG_FILES, on which its Oh My Pi
// would refuse to start. So the role is reported StaleAddress, its detail naming the file and the
// volume the pod lacks, and its relaunch replaces the issue pod; the new pod mounts the volume,
// holds nothing a pod created now is not handed, and a sibling the key does not name resumes into
// that same new pod.
func TestAddingARoleOverlaysMountReplacesAnIssuePodMadeWithoutIt(t *testing.T) {
	g := newRig(t, nil)
	tester, reviewer := workerSpec(t), testSpec(t, otherToken, claim.RoleReviewer, testTree)
	testerLoc, reviewerLoc := g.spawn(tester), g.spawn(reviewer)

	const file = "/etc/legion-operator-roles/fallback.yml"
	roles := corev1.Volume{Name: "operator-roles", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-roles"},
	}}}
	r2 := secondRuntime(t, g, func(o *Options) {
		o.Pod.Volumes = append(o.Pod.Volumes, roles)
		o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: roles.Name, MountPath: "/etc/legion-operator-roles", ReadOnly: true})
		o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {file}}
	})()
	g.connectRunning(workerToken, testerLoc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, testerLoc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	obs := readopt(t, g.ctx, r2, testerLoc)[0]
	if obs.Kind != runtime.StaleAddress || obs.Locator != testerLoc {
		t.Fatalf("%s at %+v, want StaleAddress at the recorded locator: %s", obs.Kind, obs.Locator, obs.Detail)
	}
	if want := (movedAddress{"role_config_files.tester " + file, "(unset)", "volume operator-roles from ConfigMap legion-operator-roles items []"}).String(); !strings.Contains(obs.Detail, want) {
		t.Errorf("detail %q lacks %q", obs.Detail, want)
	}

	tester.Generation, tester.BootToken, tester.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	newTester, err := r2.Resume(g.ctx, &testerLoc, tester)
	if err != nil {
		t.Fatalf("Resume the tester: %v", err)
	}
	if newTester.Sandbox.PodUID == testerLoc.Sandbox.PodUID {
		t.Fatalf("Resume kept the pod %s, which does not mount the tester's overlay %s", testerLoc.Sandbox.PodUID, file)
	}
	pod := g.pod(testerLoc.Sandbox.Name)
	if held := heldInPod(r2, pod, claim.Roles); len(held) > 0 {
		t.Fatalf("the replaced pod holds %+v, which a pod created now is not handed", held)
	}
	if !slices.ContainsFunc(containerNamed(t, pod.Spec, workerContainer).VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == roles.Name }) {
		t.Errorf("the replaced pod does not mount %s", roles.Name)
	}
	if got := startEnv(lastStart(t, g, workerToken))["PI_CONFIG_FILES"]; !strings.HasSuffix(got, file) {
		t.Errorf("the tester's generation 2 PI_CONFIG_FILES = %q, want it to end with %s", got, file)
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

// Moving the overlay volume to another mount path (same volume, role_config_files pointed at the
// new path) is a move too: the old pod mounts the volume where the role's file is not, so the role
// is StaleAddress naming the new file. Restarting on the same configuration, a mount of the file's
// parent directory, and a mount under the image's home (which an issue pod's roles container holds
// at its HomeMountsDir view) are no move, for an issue pod and the controller's pod alike.
func TestMovingARoleOverlaysMountPathIsAMove(t *testing.T) {
	roles := corev1.Volume{Name: "operator-roles", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-roles"},
	}}}
	at := func(path string, files ...string) func(*Options) {
		return func(o *Options) {
			o.Pod.Volumes = append(o.Pod.Volumes, roles)
			o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: roles.Name, MountPath: path, ReadOnly: true})
			o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: files, claim.RoleController: files}
		}
	}
	const home = "/home/legion/legion-roles"
	for _, tc := range []struct {
		name          string
		before, after func(*Options)
		moved         string
	}{
		{"the mount path moved", at("/etc/a", "/etc/a/fallback.yml"), at("/etc/b", "/etc/b/fallback.yml"), "role_config_files.tester /etc/b/fallback.yml"},
		{"the same configuration", at("/etc/a", "/etc/a/fallback.yml"), at("/etc/a", "/etc/a/fallback.yml"), ""},
		{"a file under a parent-directory mount", at("/etc/a", "/etc/a/roles/fallback.yml"), at("/etc/a", "/etc/a/roles/fallback.yml"), ""},
		{"a mount under the image's home", at(home, home+"/fallback.yml"), at(home, home+"/fallback.yml"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r1, r2 := runtimeWithOptions(t, tc.before), runtimeWithOptions(t, tc.after)
			for _, spec := range []runtime.SpawnSpec{workerSpec(t), controllerSpec(t)} {
				pod := &corev1.Pod{Spec: podOf(t, r1, spec)}
				got, moved := r2.movedRoleOverlays(pod, spec.Role)
				if tc.moved == "" && moved {
					t.Errorf("%s: moved %s, want no move", spec.Role, got)
				}
				if want := strings.Replace(tc.moved, "tester", string(spec.Role), 1); tc.moved != "" && (!moved || got.where != want) {
					t.Errorf("%s: moved %v %+v, want a move of %s", spec.Role, moved, got, want)
				}
			}
		})
	}
}

// runtimeWithOptions is the test runtime over the operator's pod, changed by change.
func runtimeWithOptions(t *testing.T, change func(*Options)) *Runtime {
	t.Helper()
	opts := testOptions()
	opts.Pod = operatorPod()
	change(&opts)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A pod that lacks the mount of any named role's file is a move for every role of it, as a broker
// enrollment is: the tester whose own file the pod holds, and the architect the key does not name,
// read the implementer's missing mount too, so the pod is replaced at the boot's re-adoption of
// whichever role runs, not at the implementer's later launch. The controller's pod is its own: an
// issue role's missing mount is no move for it.
func TestAMissingRoleOverlayMountMovesEveryRoleOfThePod(t *testing.T) {
	vol := func(name string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}
	}
	before := runtimeWithOptions(t, func(o *Options) {
		o.Pod.Volumes = append(o.Pod.Volumes, vol("roles-x"))
		o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: "roles-x", MountPath: "/etc/x", ReadOnly: true})
		o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {"/etc/x/tester.yml"}}
	})
	after := runtimeWithOptions(t, func(o *Options) {
		o.Pod.Volumes = append(o.Pod.Volumes, vol("roles-x"), vol("roles-y"))
		o.Pod.VolumeMounts = append(o.Pod.VolumeMounts,
			corev1.VolumeMount{Name: "roles-x", MountPath: "/etc/x", ReadOnly: true}, corev1.VolumeMount{Name: "roles-y", MountPath: "/etc/y", ReadOnly: true})
		o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {"/etc/x/tester.yml"}, claim.RoleImplementer: {"/etc/y/implementer.yml"}}
	})
	pod := &corev1.Pod{Spec: podOf(t, before, workerSpec(t))}
	want := (movedAddress{"role_config_files.implementer /etc/y/implementer.yml", "(unset)", "volume roles-y from ConfigMap roles-y items []"}).String()
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleArchitect, claim.RoleImplementer} {
		moved := after.movedInPod(pod, role)
		if !slices.ContainsFunc(moved, func(m movedAddress) bool { return m.String() == want }) {
			t.Errorf("the %s's pod holds %+v, want %s among it", role, moved, want)
		}
	}
	controller := &corev1.Pod{Spec: podOf(t, before, controllerSpec(t))}
	if got, moved := after.movedRoleOverlays(controller, claim.RoleController); moved {
		t.Errorf("the controller's pod moved for an issue role's mount: %+v", got)
	}
}

// A mount that hid the file in the old pod is a move once the configuration drops it: the old pod
// mounts volume Y at /etc/a/roles over volume X at /etc/a, and the configuration now serves
// /etc/a/roles/fallback.yml from X alone, so the old pod's deepest mount at the file is Y, not the
// configured holder X — for an issue pod and the controller's pod alike.
func TestADroppedMountThatHidTheFileIsAMove(t *testing.T) {
	vol := func(name string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}}}
	}
	const file = "/etc/a/roles/fallback.yml"
	files := map[claim.Role][]string{claim.RoleTester: {file}, claim.RoleController: {file}}
	before := runtimeWithOptions(t, func(o *Options) {
		o.Pod.Volumes = append(o.Pod.Volumes, vol("x"), vol("y"))
		o.Pod.VolumeMounts = append(o.Pod.VolumeMounts,
			corev1.VolumeMount{Name: "x", MountPath: "/etc/a", ReadOnly: true}, corev1.VolumeMount{Name: "y", MountPath: "/etc/a/roles", ReadOnly: true})
		o.RoleConfigFiles = files
	})
	after := runtimeWithOptions(t, func(o *Options) {
		o.Pod.Volumes = append(o.Pod.Volumes, vol("x"))
		o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: "x", MountPath: "/etc/a", ReadOnly: true})
		o.RoleConfigFiles = files
	})
	for _, spec := range []runtime.SpawnSpec{workerSpec(t), controllerSpec(t)} {
		pod := &corev1.Pod{Spec: podOf(t, before, spec)}
		if got, moved := after.movedRoleOverlays(pod, spec.Role); !moved || got.where != "role_config_files."+string(spec.Role)+" "+file {
			t.Errorf("%s: moved %v %+v, want a move of its file under the dropped mount", spec.Role, moved, got)
		}
		if got, moved := before.movedRoleOverlays(pod, spec.Role); moved {
			t.Errorf("%s: the same configuration moved %+v", spec.Role, got)
		}
	}
}
