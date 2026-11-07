package api

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// guardKeep is what this package's guard keeps beyond fixtureenv's own test inputs: the operator's
// inputs to the two gated real-GitHub proofs, each naming a repository, a revision or a binary and
// none of them a credential. The five LEGION_READY_SCOPE_* are the READY scope proof's subject —
// TestRealGitHubReadyHoldsTheHeadToRetrosCommits (real_github_ready_test.go) reads the repository,
// the pull request, the issue, the approved head and the head through a get that fails the test
// when one is empty, so a guard that took them out would turn that proof into a failure.
// LEGION_REAL_GITHUB gates both real-GitHub proofs, LEGION_GH_PATH names the real gh where a
// pane's or a pod's own `gh` is the `legion gh` wrapper in front of it, and
// LEGION_READY_SCOPE_REFUSE is the optional path that proof runs its refusal case on. Kept, the
// operator commands of scripts/e2e/README.md still run under the guard. LEGION_TEST_PG_DSN and the
// four LEGION_E2E_*_APP_* inputs the App lib reads need no entry: fixtureenv's own testInput keeps
// every LEGION_TEST_ and LEGION_E2E name by prefix.
var guardKeep = []string{
	"LEGION_READY_SCOPE_REPO", "LEGION_READY_SCOPE_PR", "LEGION_READY_SCOPE_ISSUE",
	"LEGION_READY_SCOPE_APPROVED", "LEGION_READY_SCOPE_HEAD",
	realGitHubProofEnabled, "LEGION_GH_PATH", "LEGION_READY_SCOPE_REFUSE",
}

// TestMain takes out of the process every variable that could lead this package's real git and gh
// — the credential proof's `git clone` of a real repository through gh's own credential helper,
// and the gh it runs over the rendered files — to a repository, remote or credential not the
// test's own (fixtureenv.MustGuard), keeping guardKeep, the gated proofs' own inputs. Nothing in
// this package re-executes the test binary, so there is no helper-child branch before the guard.
// It then drops the migrated template testStore clones its databases from (testpg.Main), which
// wraps the run: the guard decides the environment the tests and the template's migration both
// run in.
func TestMain(m *testing.M) {
	fixtureenv.MustGuard(guardKeep...)
	os.Exit(testpg.Main("legion_api_test", m.Run))
}

// TestTheGuardKeepsTheGatedProofsInputs holds guardKeep to the inputs the gated proofs read: with
// each of the eight set to a value of this test's, and a pod's own GIT_DIR, GIT_CONFIG_COUNT and
// GH_TOKEN planted beside them, a Guard of guardKeep leaves all eight as the test set them and
// takes the pod's three out. The eight are written out here rather than read from guardKeep, so
// that dropping a name from guardKeep fails this test naming it. Guard writes the package state
// the rest of the run reads (RequireGuarded reads what the last Guard kept), so the cleanup runs
// the package's own Guard again, as internal/fixtureenv's own guard test does.
func TestTheGuardKeepsTheGatedProofsInputs(t *testing.T) {
	t.Cleanup(func() {
		if err := fixtureenv.Guard(guardKeep...); err != nil {
			t.Error(err)
		}
	})
	inputs := map[string]string{
		"LEGION_READY_SCOPE_REPO":     "acme/widgets",
		"LEGION_READY_SCOPE_PR":       "42",
		"LEGION_READY_SCOPE_ISSUE":    "WIDGETS-7",
		"LEGION_READY_SCOPE_APPROVED": "1111111111111111111111111111111111111111",
		"LEGION_READY_SCOPE_HEAD":     "2222222222222222222222222222222222222222",
		"LEGION_REAL_GITHUB":          "1",
		"LEGION_GH_PATH":              "/usr/local/bin/gh",
		"LEGION_READY_SCOPE_REFUSE":   "packages/daemon/internal/api/ready.go",
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
			t.Errorf("%s=%q after Guard, want the gated proofs' input %q kept", name, got, value)
		}
	}
	for name := range pod {
		if got, set := os.LookupEnv(name); set {
			t.Errorf("the test process still holds %s=%q, which Guard takes out", name, got)
		}
	}
}
