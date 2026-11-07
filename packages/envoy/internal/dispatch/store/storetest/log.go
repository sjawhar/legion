package storetest

import (
	"github.com/sjawhar/envoy/internal/logtest"
)

// LockedLog is a log sink that the goroutines of the code under test write while the test reads
// it, such as the handler a test installs with slog.SetDefault. Its body is logtest.Buffer's.
type LockedLog struct {
	logtest.Buffer
}
