package helper

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// TestMain hands the package's tests to storetest, which clones each of them a database off the
// one migrated template and drops each with its test. Without it every brokertest.Rig migrated a
// database of its own, which is the whole migration set per test and was most of this package's
// time (dispatch://INFRA-1075).
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
