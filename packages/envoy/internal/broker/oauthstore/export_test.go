package oauthstore

import "testing"

// SetSweepBatch sets sweepBatch to n for the rest of t.
func SetSweepBatch(t *testing.T, n int) {
	old := sweepBatch
	sweepBatch = n
	t.Cleanup(func() { sweepBatch = old })
}
