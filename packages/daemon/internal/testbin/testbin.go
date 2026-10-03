// Package testbin names the pinned binaries the real-binary tests run, and readies the fresh HOME
// a test runs Oh My Pi under (OMPHome).
package testbin

import (
	"os"
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

// binary is the path variable names. A run without one skips, except on GitHub Actions, where the
// daemon job installs every pin and a skip would hide the only check of the code a real binary
// exercises.
func binary(t *testing.T, variable, name string) string {
	t.Helper()
	path := os.Getenv(variable)
	switch {
	case path != "":
		return path
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatalf("%s is unset on GitHub Actions: name the pinned %s binary (the daemon job installs it)", variable, name)
	}
	t.Skipf("%s names no %s binary", variable, name)
	return ""
}
