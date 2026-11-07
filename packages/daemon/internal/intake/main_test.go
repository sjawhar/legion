package intake

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template this package's tests — the internal ones and the external
// intake_test ones, which share this binary — clone their databases from (testpg.Main), removes
// the directory their Envoy binaries were built into (testbuild.Main), and removes the NATS
// container they share (testnats.Main).
func TestMain(m *testing.M) {
	os.Exit(testpg.Main("legion_intake_test", func() int {
		return testbuild.Main(func() int { return testnats.Main(m) })
	}))
}
