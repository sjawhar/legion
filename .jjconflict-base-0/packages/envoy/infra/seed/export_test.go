package seed

import (
	"testing"
	"time"
)

// SetReadBackWaits sets the pauses generate takes while the store does not show its write, for the
// rest of the test.
func SetReadBackWaits(t testing.TB, waits ...time.Duration) {
	saved := readBackWaits
	readBackWaits = waits
	t.Cleanup(func() { readBackWaits = saved })
}
