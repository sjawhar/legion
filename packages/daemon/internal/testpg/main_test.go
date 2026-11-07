package testpg

import (
	"os"
	"testing"
)

// TestMain drops the template database this package's own test of Clone builds, as every package
// whose tests clone does (Main).
func TestMain(m *testing.M) { os.Exit(Main("legion_testpg_test", m.Run)) }
