// Package testomp names the pinned Oh My Pi binary the real-binary tests run.
package testomp

import (
	"os"
	"testing"
)

// Binary is the pinned Oh My Pi binary LEGION_TEST_OMP names (.github/actions/install-omp). A run
// without one skips, except on GitHub Actions, where the daemon-go job installs the pin and a skip
// would hide the only check of the code a real binary exercises.
func Binary(t *testing.T) string {
	t.Helper()
	omp := os.Getenv("LEGION_TEST_OMP")
	switch {
	case omp != "":
		return omp
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatal("LEGION_TEST_OMP is unset on GitHub Actions: name the pinned Oh My Pi binary (the daemon-go job installs it)")
	}
	t.Skip("LEGION_TEST_OMP names no Oh My Pi binary")
	return ""
}
