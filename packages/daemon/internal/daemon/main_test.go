package daemon

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// TestMain takes out of the process every variable that could lead the tests' real git and jj to
// a workspace, remote or credential not their own (fixtureenv.MustGuard), keeping the three the
// kept-decision property test reads: LEGION_KEPT_PROPERTY_SEED, which replays one run, and
// LEGION_KEPT_PROPERTY_RUNS and LEGION_KEPT_PROPERTY_STEPS, its two counts. CI's pull-request
// lane reduces the runs (ci.yaml) and leaves the steps at the test's own default; the guard keeps
// all three, so a developer may set any of them by hand. It drops the migrated template
// isolatedOutboxPool clones its databases from (testpg.Main), makes the one database this run's
// tests write in (testdb.RunDatabaseMain), removes the directory the tests' Envoy binaries were
// built into (testbuild.Main), and removes the NATS container the package's tests share
// (testnats.Main).
//
// The run's database is legion_daemon_run_<random> on the server LEGION_TEST_PG_DSN names,
// dropped when the run ends; testDSN hands it to every test, and a daemon migrates Legion's
// schema into it, so two runs of this package on one server never write each other's rows, and
// neither writes the database that server serves. It is named apart from the template's
// legion_daemon_test_template_<random>, which carries the migrations isolatedOutboxPool clones:
// a census of either name then says which of the two it counted. Each test still randomises its
// project name, as the tests did when they all shared one database.
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
// serves the rig. That child serves one rig and clones no database and builds nothing, so it
// takes neither template nor build directory, and it is told of no database either: the rig's
// launchers run without TestPostgres, so testPGEnv takes LEGION_TEST_PG_DSN out of the child.
func TestMain(m *testing.M) {
	if os.Getenv(idleRigURLVariable) != "" {
		os.Exit(testnats.Main(m))
	}
	fixtureenv.MustGuard("LEGION_KEPT_PROPERTY_SEED", "LEGION_KEPT_PROPERTY_RUNS", "LEGION_KEPT_PROPERTY_STEPS")
	os.Exit(testpg.Main("legion_daemon_test", func() int {
		return testdb.RunDatabaseMain("legion_daemon_run", func() int {
			return testbuild.Main(func() int { return testnats.Main(m) })
		})
	}))
}
