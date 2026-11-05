package redeliver_test

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain gives the package both harnesses: storetest.Main runs the tests and drops the databases
// it clones, and testnats.MainWith shuts the one NATS server the package's tests share down after
// it (openState). A server per test was what this package paid before, 47 of them in a run, for
// state each test can have in an account of its own instead.
func TestMain(m *testing.M) { os.Exit(testnats.MainWith(m, storetest.Main)) }
