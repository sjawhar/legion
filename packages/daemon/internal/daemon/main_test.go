package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain takes out of the process every variable that could lead the tests' real git and jj to a
// workspace, remote or credential not their own (fixtureenv.Guard; LEGION_KEPT_PROPERTY_SEED, which
// replays a property run, is kept), makes the one Postgres database this run's tests write in
// (testpg.Main) and removes the NATS container the package's tests share (testnats.Main).
//
// The run's database is legion_daemon_test_<random> on the server LEGION_TEST_PG_DSN names,
// dropped when the run ends; testDSN hands it to every test, and a daemon migrates Legion's
// schema into it, so two runs of this package on one server never write each other's rows. Each
// test still randomises its project name, as the tests did when they all shared one database.
//
// The guard runs for the test process alone. This binary is also the rig's role process, which its
// launcher re-enters as TestIdleRigRoleProcess (idle_rig_pod_test.go) with the environment a role
// of an issue pod is given: LEGION_IDLE_RIG_URL names the rig to serve, and LEGION_SANDBOX,
// LEGION_ISSUE, LEGION_ROLE and LEGION_DAEMON_URL say which role of which issue it is. Guard takes
// every LEGION_* variable out, so guarding that run would leave the child naming no rig at all: it
// would return from its test, print PASS and exit 0, and the daemon would read every one of its
// roles as a process that died at once. So the child's run is told apart by that variable and
// skips the guard, keeping what its launcher handed it, which is what Guard's own doc asks for a
// helper child ("that child keeps the variables its test hands it"); it runs no git or jj of its
// own, and goes on into m.Run as any other run of this binary does, where the one test it names
// serves the rig. That child takes no database either, and is told of none: the rig's launchers
// run without TestPostgres, so testPGEnv takes LEGION_TEST_PG_DSN out of the child. It also
// leaves through an os.Exit inside its test, so nothing after m.Run runs in it, and a database
// made there would be one per role process and dropped by none.
func TestMain(m *testing.M) {
	if os.Getenv(idleRigURLVariable) != "" {
		os.Exit(runRemovingTheBuiltListener(m))
	}
	if err := fixtureenv.Guard("LEGION_KEPT_PROPERTY_SEED"); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	os.Exit(testpg.Main("legion_daemon_test", func() int {
		return runRemovingTheBuiltListener(m)
	}))
}

// runRemovingTheBuiltListener runs the package's tests and removes the directory the built Envoy
// listener of the idle rig sits in afterwards (removeIdleRigListenerBuild). The build happens
// during a test, not here, so the removal reads the directory when it runs rather than taking its
// name now, when there is none. Being deferred, it also runs when setup here panics. A run
// killed by a signal still keeps the directory, which nothing here can help.
func runRemovingTheBuiltListener(m *testing.M) int {
	defer removeIdleRigListenerBuild()
	return testnats.Main(m)
}

func TestTheTestProcessIsGuarded(t *testing.T) {
	fixtureenv.RequireGuarded(t)
}
