package redeliver

import (
	"testing"
	"time"
)

// FixJitter makes every wait Run leaves after a sweep the interval plus draw(interval/10), where
// duty.Jittered draws at random in [0, interval/10), until the test ends.
func FixJitter(t *testing.T, draw func(n time.Duration) time.Duration) {
	t.Helper()
	saved := jittered
	jittered = func(d time.Duration) time.Duration { return d + draw(d/10) }
	t.Cleanup(func() { jittered = saved })
}

// PendingPrefix is the prefix every index entry key carries, and PendingKey is the entry key for
// guid due at at (index.go). A test outside the package builds a key or recognises one through
// these, so no test passes while the key the sweep writes drifts from the one it asserts.
const PendingPrefix = pendingPrefix

func PendingKey(at time.Time, guid string) string { return pendingKey(at, guid) }
