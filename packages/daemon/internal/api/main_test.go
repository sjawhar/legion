package api

import (
	"fmt"
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// guardKeep is what this package's guard keeps beyond fixtureenv's own test inputs: the operator's
// inputs to the gated real-GitHub proof, each naming a repository or a binary and none of them a
// credential. LEGION_REAL_GITHUB gates the proof and LEGION_GH_PATH names the real gh where a
// pane's or a pod's own `gh` is the `legion gh` wrapper in front of it. Kept, the operator
// commands of scripts/e2e/README.md still run under the guard. LEGION_TEST_PG_DSN and the four
// LEGION_E2E_*_APP_* inputs the App lib reads need no entry: fixtureenv's own testInput keeps
// every LEGION_TEST_ and LEGION_E2E name by prefix. The five LEGION_READY_SCOPE_* names went with
// the proof that read them, TestRealGitHubReadyHoldsTheHeadToRetrosCommits, when READY stopped
// comparing the head with the approved head (dispatch://INFRA-764).
var guardKeep = []string{realGitHubProofEnabled, "LEGION_GH_PATH"}

// TestMain takes out of the process every variable that could lead this package's real git and gh
// — the credential proof's `git clone` of a real repository through gh's own credential helper,
// and the gh it runs over the rendered files — to a repository, remote or credential not the
// test's own (fixtureenv.Guard), keeping guardKeep, the gated proofs' own inputs. Nothing in this
// package re-executes the test binary, so there is no helper-child branch before the guard.
func TestMain(m *testing.M) {
	if err := fixtureenv.Guard(guardKeep...); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestTheTestProcessIsGuarded(t *testing.T) {
	fixtureenv.RequireGuarded(t)
}

// TestTheGuardKeepsTheGatedProofsInputs holds guardKeep to the inputs the gated proof reads: with
// each of the two set to a value of this test's, and a pod's own GIT_DIR, GIT_CONFIG_COUNT and
// GH_TOKEN planted beside them, a Guard of guardKeep leaves both as the test set them and takes
// the pod's three out. The two are written out here rather than read from guardKeep, so that
// dropping a name from guardKeep fails this test naming it. Guard writes the package state
// the rest of the run reads (RequireGuarded reads what the last Guard kept), so the cleanup runs
// the package's own Guard again, as internal/fixtureenv's own guard test does.
func TestTheGuardKeepsTheGatedProofsInputs(t *testing.T) {
	t.Cleanup(func() {
		if err := fixtureenv.Guard(guardKeep...); err != nil {
			t.Error(err)
		}
	})
	inputs := map[string]string{
		"LEGION_REAL_GITHUB": "1",
		"LEGION_GH_PATH":     "/usr/local/bin/gh",
	}
	pod := map[string]string{
		"GIT_DIR":          "/legion/repos/acme/widgets/.git",
		"GIT_CONFIG_COUNT": "1",
		"GH_TOKEN":         "a pod's token",
	}
	for name, value := range inputs {
		t.Setenv(name, value)
	}
	for name, value := range pod {
		t.Setenv(name, value)
	}
	if err := fixtureenv.Guard(guardKeep...); err != nil {
		t.Fatal(err)
	}
	for name, value := range inputs {
		if got := os.Getenv(name); got != value {
			t.Errorf("%s=%q after Guard, want the gated proof's input %q kept", name, got, value)
		}
	}
	for name := range pod {
		if got, set := os.LookupEnv(name); set {
			t.Errorf("the test process still holds %s=%q, which Guard takes out", name, got)
		}
	}
}
