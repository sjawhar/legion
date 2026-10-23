package oauthstore

import "testing"

// SetSweepBatch sets sweepBatch to n for the rest of t.
func SetSweepBatch(t *testing.T, n int) {
	old := sweepBatch
	sweepBatch = n
	t.Cleanup(func() { sweepBatch = old })
}

// SetSweepMaxBatches sets sweepMaxBatches to n for the rest of t.
func SetSweepMaxBatches(t *testing.T, n int) {
	old := sweepMaxBatches
	sweepMaxBatches = n
	t.Cleanup(func() { sweepMaxBatches = old })
}
