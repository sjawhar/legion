package sandbox

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// TestMain takes out of the process every variable that could lead the init containers' and the
// roles' real git, which these tests run on this machine, to a repository, remote or credential
// not the test's own (fixtureenv.MustGuard), in the e2e build too, whose untagged tests are these
// same fixtures. The e2e build's live proof takes the process's own environment back for its run
// (fixtureenv.Live).
func TestMain(m *testing.M) {
	fixtureenv.MustGuard()
	os.Exit(m.Run())
}
