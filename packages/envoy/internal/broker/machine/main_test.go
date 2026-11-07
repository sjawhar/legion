package machine

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// TestMain hands the package's tests to storetest, which clones each of them a database off the
// one migrated template and drops each with its test. The template is the migration set's and
// stays for the next test process.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
