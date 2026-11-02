package redeliver_test

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestMain drops the databases the harness clones (storetest.Main). Each harness starts a NATS
// server of its own (testnats.Start), since testnats.Main, which the shared server needs, and
// storetest.Main each run the package's tests themselves.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
