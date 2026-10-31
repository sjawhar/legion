// Package testbin names the pinned binaries the real-binary tests run, readies the fresh HOME a
// test runs Oh My Pi under (OMPHome), and probes the pinned jj for the working-copy filters
// provisioning is proven against (RequireJJFilters).
package testbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// OMP is the pinned Oh My Pi binary LEGION_TEST_OMP names (.github/actions/install-omp).
func OMP(t *testing.T) string {
	t.Helper()
	return binary(t, "LEGION_TEST_OMP", "Oh My Pi")
}

// UV is the uv the worker image ships, which LEGION_TEST_UV names (.github/actions/install-uv,
// from worker.Dockerfile's ARG UV_VERSION and ARG UV_SHA256).
func UV(t *testing.T) string {
	t.Helper()
	return binary(t, "LEGION_TEST_UV", "uv")
}

// RequireJJFilters fails unless the jj under test, reading no configuration of the user's, has
// working-copy filters: provisioning is proven against the jj the worker image ships.
func RequireJJFilters(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	probe := exec.Command("jj", "config", "list", "--include-defaults", "git.filter.enabled")
	// The probe runs in the empty directory it reads its configuration from: jj searches upward
	// from the directory it runs in for the closest .jj, so a probe with no Dir would open the
	// workspace the test process stands in, which in a Legion issue pod is a real one.
	probe.Dir = home
	probe.Env = append(os.Environ(), "JJ_CONFIG="+filepath.Join(home, "none.toml"), "XDG_CONFIG_HOME="+home, "HOME="+home)
	if out, _ := probe.Output(); len(strings.TrimSpace(string(out))) == 0 {
		t.Fatal("this jj has no working-copy filters: provisioning is proven against the jj the worker image ships (packages/daemon/docker/worker.Dockerfile, ARG JJ_TOOL), which CI installs")
	}
}

// binary is the path variable names. A run without one skips, except on GitHub Actions, where
// ci.yaml's test job installs every pin and a skip would hide the only check of the code a real
// binary exercises.
func binary(t *testing.T, variable, name string) string {
	t.Helper()
	path := os.Getenv(variable)
	switch {
	case path != "":
		return path
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatalf("%s is unset on GitHub Actions: name the pinned %s binary (ci.yaml's test job installs it)", variable, name)
	}
	t.Skipf("%s names no %s binary", variable, name)
	return ""
}
