package workflow

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template migratedPool clones its databases from (testpg.Main) and
// removes the NATS container the package's tests share (testnats.Main).
func TestMain(m *testing.M) {
	os.Exit(testpg.Main("legion_workflow_test", func() int { return testnats.Main(m) }))
}
