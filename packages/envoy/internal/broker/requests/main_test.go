package requests

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// TestMain hands the package's tests to storetest, which clones each of them a database off the
// one migrated template, drops each with its test, and drops the template when the last test
// has ended.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
