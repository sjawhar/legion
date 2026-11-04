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
func TestMain(m *testing.M) {
	if err := fixtureenv.Guard("LEGION_KEPT_PROPERTY_SEED"); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	os.Exit(testnats.Main(m))
}

func TestTheTestProcessIsGuarded(t *testing.T) {
	fixtureenv.RequireGuarded(t)
}
