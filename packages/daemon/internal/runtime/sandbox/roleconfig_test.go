package sandbox

import (
	"path/filepath"
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
// shim's pod baseline then names the turn-scoping overlay ahead of them all, as it does the
// operator's alone. A role it does not name runs exactly the process, in exactly the pod, a daemon
// without the key gives it, and so does a named role's pod: its overlays ride the start command.
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
			state := t.TempDir()
			applied, err := podsafety.Apply([]string{"PI_CONFIG_FILES=" + got}, state)
			if err != nil {
				t.Fatal(err)
			}
			if want := "PI_CONFIG_FILES=" + filepath.Join(state, podsafety.TurnScopeFile) + ":" + tc.want; !slices.Contains(applied, want) {
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
