package launcher

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The helpers every test of a role's namespaces shares: roleInitEnv carries role-init's flags to
// the test binary re-entered as role-init (TestRoleInitHelper), and roleProbeEnv names the
// directory requireRoleNamespaces' probe mounts over (TestRoleNamespaceProbeHelper).
const (
	roleInitEnv  = "LEGION_ROLE_INIT_TEST_INIT"
	roleProbeEnv = "LEGION_ROLE_INIT_TEST_PROBE"
)

// requireRoleNamespaces skips a test that cannot create the namespaces a role generation runs
// in, or cannot mount inside them. Unprivileged user namespaces are refused on a host whose
// AppArmor sysctl restricts them (Ubuntu 24.04's default), and the job that must run these
// tests turns the restriction off and proves it (.github/workflows/pr-and-main.yaml), so a
// refusal under GITHUB_ACTIONS is a failure and never a skip, as namespace_test's rig has it.
func requireRoleNamespaces(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("a role's namespaces need Linux")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	attr, err := roleNamespaceAttr()
	var output []byte
	if err == nil {
		probe := exec.Command(self, "-test.run=^TestRoleNamespaceProbeHelper$")
		probe.Env = append(os.Environ(), roleProbeEnv+"="+t.TempDir())
		probe.SysProcAttr = attr
		output, err = probe.CombinedOutput()
	}
	if err == nil {
		return
	}
	skipOutsideCI(t, "a role's namespaces: %v: %s", err, output)
}

// skipOutsideCI skips a role-namespace test this machine cannot run, and fails it under
// GITHUB_ACTIONS: the job that must run these tests is set up to run them, so anything that would
// make it skip is a broken runner, never a green job with the proof gone from it.
func skipOutsideCI(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// namespaceScratch is a directory for a test whose role mounts its own /tmp over the container's:
// a layout kept under /tmp would be hidden from role-init the moment it binds the role's own
// temporary directory there, so the scratch sits outside it.
func namespaceScratch(t *testing.T) string {
	t.Helper()
	if dir := t.TempDir(); !strings.HasPrefix(dir, "/tmp/") {
		return dir
	}
	home := os.Getenv("HOME")
	if home == "" {
		skipOutsideCI(t, "no directory outside /tmp for the role's layout: TMPDIR is under /tmp and HOME is unset")
	}
	dir, err := os.MkdirTemp(home, "legion-launcher-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// useRoleLayout runs the package's role generations the way a test can: role-init is this test
// binary re-entered as TestRoleInitHelper, since the binary is not `legion`, and the home, the home
// mounts and the fixed root are the test's own directories, since a machine running the tests has
// neither the image's /home/legion nor its /var/run/legion mount points, and could not create them.
func useRoleLayout(t *testing.T, home, homeMounts, fixed string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previousHome, previousMounts, previousFixed, previousLaunch := roleHome, roleHomeMounts, roleFixedRoot, roleInitLaunch
	roleHome, roleHomeMounts, roleFixedRoot, roleInitLaunch = home, homeMounts, fixed, roleInitHelperLaunch(self)
	t.Cleanup(func() {
		roleHome, roleHomeMounts, roleFixedRoot, roleInitLaunch = previousHome, previousMounts, previousFixed, previousLaunch
	})
}

// roleInitHelperLaunch is roleInitLaunch for a test binary: it re-enters self as role-init, with
// the flags the launcher would pass on the command line carried in roleInitEnv.
func roleInitHelperLaunch(self string) func(RoleInitConfig) ([]string, []string, error) {
	return func(cfg RoleInitConfig) ([]string, []string, error) {
		body, err := json.Marshal(cfg)
		if err != nil {
			return nil, nil, err
		}
		return []string{self, "-test.run=^TestRoleInitHelper$"}, []string{roleInitEnv + "=" + string(body)}, nil
	}
}

// TestRoleInitHelper is role-init itself, as `legion launcher role-init` runs it: the launcher or
// the stage clones it into a role's new namespaces, and it reads the flags it would have been
// given on the command line from its environment.
func TestRoleInitHelper(t *testing.T) {
	encoded := os.Getenv(roleInitEnv)
	if encoded == "" {
		return
	}
	var cfg RoleInitConfig
	if err := json.Unmarshal([]byte(encoded), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "role-init:", err)
		os.Exit(2)
	}
	cfg.Stdout, cfg.Stderr = os.Stdout, os.Stderr
	if err := RoleInit(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "role-init:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
