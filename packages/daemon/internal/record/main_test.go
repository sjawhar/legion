package record

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template migratedStore clones its databases from (testpg.Main).
func TestMain(m *testing.M) { os.Exit(testpg.Main("legion_record_test", m.Run)) }
