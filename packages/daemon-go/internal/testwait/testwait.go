// Package testwait holds the bounded wait the daemon's tests share. A test that drives real
// processes, a real database or a real socket waits for something it cannot make happen on its
// own — a delivery, the transaction that answers it — and the wait is bounded by the test
// binary's own deadline rather than a fixed span: on a loaded machine a step that is merely slow
// is not a failure, and a condition that never holds still fails, naming what it waited for.
package testwait

import (
	"testing"
	"time"
)

// poll is how often a condition is asked. It is short enough that a test which passes in
// milliseconds still does, and long enough that a waiting test costs a loaded machine little.
const poll = 10 * time.Millisecond

// Eventually waits for condition, failing the test with what it waited for when the deadline
// passes. The deadline is a minute, or the test binary's own less a second when that is sooner,
// so a `-timeout` smaller than a minute still reports the wait rather than the binary's panic.
func Eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Add(-time.Second).Before(deadline) {
		deadline = testDeadline.Add(-time.Second)
	}
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(poll)
	}
	t.Fatalf("timed out waiting for %s", what)
}
