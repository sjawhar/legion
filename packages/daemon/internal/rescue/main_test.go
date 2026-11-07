package rescue_test

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// TestMain takes out of the process every variable that could lead the push check's real git and
// jj to a workspace, remote or credential not the test's own (fixtureenv.MustGuard).
func TestMain(m *testing.M) {
	fixtureenv.MustGuard()
	os.Exit(m.Run())
}
