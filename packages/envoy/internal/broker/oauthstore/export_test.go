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

// SweepStatement is one of Sweep's statements: what it sweeps and its SQL, whose $1 is the batch.
type SweepStatement struct{ What, SQL string }

// SweepStatements answers Sweep's statements, in the order it runs them.
func SweepStatements() []SweepStatement {
	out := make([]SweepStatement, 0, len(sweepStatements))
	for _, q := range sweepStatements {
		out = append(out, SweepStatement{What: q.what, SQL: q.sql})
	}
	return out
}
