package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// TestMain takes out of the process every variable that could lead the tests' real git and jj to a
// workspace, remote or credential not their own (fixtureenv.Guard; LEGION_KEPT_PROPERTY_SEED, which
// replays a property run, is kept), and removes the NATS container the package's tests share
// (testnats.Main).
//
// The guard runs for the test process alone. This binary is also the rig's role process, which its
// launcher re-enters as TestIdleRigRoleProcess (idle_rig_pod_test.go) with the environment a role
// of an issue pod is given: LEGION_IDLE_RIG_URL names the rig to serve, and LEGION_SANDBOX,
// LEGION_ISSUE, LEGION_ROLE and LEGION_DAEMON_URL say which role of which issue it is. Guard takes
// every LEGION_* variable out, so guarding that child would leave it naming no rig at all: it
// would return from its test, print PASS and exit 0, and the daemon would read every one of its
// roles as a process that died at once. So the branch where this binary runs as a helper child
// comes first, which is the contract Guard's own doc states — "that child keeps the variables its
// test hands it" — and the child runs no git or jj of its own.
func TestMain(m *testing.M) {
	if os.Getenv(idleRigURLVariable) == "" {
		if err := fixtureenv.Guard("LEGION_KEPT_PROPERTY_SEED"); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain:", err)
			os.Exit(1)
		}
	}
	os.Exit(testnats.Main(m))
}

func TestTheTestProcessIsGuarded(t *testing.T) {
	fixtureenv.RequireGuarded(t)
}
