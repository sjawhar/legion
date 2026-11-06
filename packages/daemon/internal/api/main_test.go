package api

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// TestMain drops the migrated template testStore clones its databases from (testpg.Main).
func TestMain(m *testing.M) { os.Exit(testpg.Main("legion_api_test", m.Run)) }
