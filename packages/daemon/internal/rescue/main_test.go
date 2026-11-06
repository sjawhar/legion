package rescue_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// TestMain takes out of the process every variable that could lead the push check's real git and
// jj to a workspace, remote or credential not the test's own (fixtureenv.Guard).
func TestMain(m *testing.M) {
	if err := fixtureenv.Guard(); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestTheTestProcessIsGuarded(t *testing.T) {
	fixtureenv.RequireGuarded(t)
}
