package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
)

// helperChildVariables are the markers this package's helper children are re-executed with: the
// test binary runs as a launcher's child (TestLauncherChild), as a launcher of its own
// (TestLauncherProcess, TestLauncherPIDOneHelper), as a generation that waits for a SIGTERM
// (TestLauncherTermDialer) and as role-init in a role's namespaces (TestRoleInitHelper and the
// helpers of roleinit_test.go, which register their own markers here). Each reads its
// instructions from a LEGION_ variable its test hands it, and Guard takes every LEGION_ variable
// out, so a process that is one of these children runs no Guard of its own: it was started by a
// guarded test process, whose environment it inherits, so it holds nothing of the pod's either.
var helperChildVariables = []string{
	launcherChildEnv, launcherProcessEnv, launcherTermDialerEnv, namespaceHelperEnv,
	roleInitEnv, roleProbeEnv,
}

// TestMain takes out of the process every variable that could lead git or jj to a workspace,
// remote or credential not the test's own (fixtureenv.Guard). The tests here run both: a released
// workspace's bundle is restored by this package's own code, in process, with the real git and jj
// (restoreWorkspace), and the launcher starts every generation under the process's own
// environment merged with the start's (mergeEnv), which is where a restore reads the workspace it
// is to work (envValue(env, "LEGION_WORKSPACE") in bundleRestore.apply). In a Legion issue pod
// that variable names the pod's live workspace, so without this a test whose start named no
// workspace of its own would unbundle into it, move its bookmarks --allow-backwards and jj edit
// the working copy back to a saved head, over the work the pod's own roles have done
// (docs/testing.md, "A test that starts jj or git inherits no pod's environment").
//
// It takes out the two variables that name a session store as well, which Guard leaves: Guard
// takes out the LEGION_, GIT_, JJ_, GH_ and GITHUB_ variables and not these. A start's
// environment is mergeEnv(os.Environ(), command.Env) and a resume looks its session up through
// newSessionDial on that environment, so in a Legion issue pod, where OMP_SESSION_SQL_DSN_FILE
// and OMP_SESSION_STORAGE name the deployment's shared sessions database, a test of this package
// would connect to and query the store every agent of the deployment reads and writes. Each test
// here that needs a session database names its own (testpg.DSNFile).
func TestMain(m *testing.M) {
	for _, name := range helperChildVariables {
		if os.Getenv(name) != "" {
			os.Exit(m.Run())
		}
	}
	fixtureenv.MustGuard()
	for _, name := range []string{ompsessions.DSNFileVariable, ompsessions.StorageVariable} {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain:", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// No test of this package can reach the workspace of the pod it runs in. The first two cases fail
// without a Guard in a Legion issue pod, where LEGION_WORKSPACE names the pod's live workspace,
// and name the workspace a restore of this package's tests would have worked. The third holds
// whether or not a Guard ran: it checks only that fixtureenv.Isolated, which the bundle fixture
// builds its environment from, drops a workspace the process holds.
func TestNoTestOfThisPackageCanReachThePodsWorkspace(t *testing.T) {
	// The test process: Guard ran, and nothing it takes out is left — no LEGION_, no GIT_ but
	// GIT_ALLOW_PROTOCOL=file, no JJ_ (fixtureenv.RequireGuarded), so neither this package's code
	// under test nor a child it starts with no environment of its own can read the pod's.
	t.Run("the test process holds no workspace of the pod's", func(t *testing.T) {
		fixtureenv.RequireGuarded(t)
		if dir := os.Getenv("LEGION_WORKSPACE"); dir != "" {
			t.Errorf("the test process holds LEGION_WORKSPACE=%s, the workspace a restore would work", dir)
		}
	})
	// The environment a generation is started under, which is where a restore reads the workspace
	// it works (bundleRestore.apply): a start that names no workspace of its own must reach a
	// restore that has none at all. RequireGuarded holds the process's variables; this holds the
	// launcher's own merge of them with the start's.
	t.Run("a generation a start names no workspace in reaches none", func(t *testing.T) {
		started := mergeEnv(os.Environ(), []string{"TOLD=" + t.TempDir()})
		if dir := envValue(started, "LEGION_WORKSPACE"); dir != "" {
			t.Errorf("a generation started by a test here would restore into %s, the workspace of the process that runs the tests", dir)
		}
	})
	// fixtureenv.Isolated, which bundleFixture.env is built from, drops a workspace the process
	// holds whether or not a Guard ran. This reads Isolated and not bundleFixture.env itself. The
	// process is handed a workspace of this case's own to read back, so what passes is Isolated's
	// doing and not an environment that held none anyway.
	t.Run("a fixture's environment carries no workspace the process holds", func(t *testing.T) {
		decoy := filepath.Join(t.TempDir(), "not-the-pods-workspace")
		t.Setenv("LEGION_WORKSPACE", decoy)
		if got := envValue(os.Environ(), "LEGION_WORKSPACE"); got != decoy {
			t.Fatalf("this case holds LEGION_WORKSPACE=%q, want the decoy %s it set: it proves nothing of Isolated", got, decoy)
		}
		if dir := envValue(fixtureenv.Isolated(t.TempDir()), "LEGION_WORKSPACE"); dir != "" {
			t.Errorf("a fixture of this package would run its git and jj under LEGION_WORKSPACE=%s", dir)
		}
	})
}

// No test of this package can reach the session store of the pod it runs in. Both cases fail
// without TestMain's unset in a Legion issue pod, where OMP_SESSION_SQL_DSN_FILE and
// OMP_SESSION_STORAGE name the deployment's shared sessions database, and name the store a
// resume of this package's tests would have queried. A test that wants a session database names
// its own with testpg.DSNFile, in the start's own environment.
func TestNoTestOfThisPackageCanReachThePodsSessionStore(t *testing.T) {
	// The test process: TestMain unset both, so neither this package's code under test nor a
	// child it starts with no environment of its own can read the pod's store.
	t.Run("the test process holds no session store of the pod's", func(t *testing.T) {
		for _, name := range []string{ompsessions.DSNFileVariable, ompsessions.StorageVariable} {
			if value := os.Getenv(name); value != "" {
				t.Errorf("the test process holds %s=%s, the session store a resume would query", name, value)
			}
		}
	})
	// The environment a generation is started under, which is where a resume reads the store it
	// looks its session up in (newSessionDial): a start that names no store of its own must reach
	// a lookup that has none at all.
	t.Run("a generation a start names no session store in reaches none", func(t *testing.T) {
		started := mergeEnv(os.Environ(), []string{"TOLD=" + t.TempDir()})
		for _, name := range []string{ompsessions.DSNFileVariable, ompsessions.StorageVariable} {
			if value := envValue(started, name); value != "" {
				t.Errorf("a generation started by a test here would resume against %s=%s, the session store of the deployment the test process runs in", name, value)
			}
		}
	})
}
