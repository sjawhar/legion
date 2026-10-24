package oauthstore

import (
	"testing"
	"time"
)

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

// SweepStatement is one of Sweep's statements: what it sweeps, its SQL and arguments for a batch
// of 1000, and whether it picks the batch (so its plan must cut with a Limit).
type SweepStatement struct {
	What, SQL  string
	Args       []any
	PicksBatch bool
}

// SweepStatements answers Sweep's statements, in the order it runs them.
func SweepStatements() []SweepStatement {
	out := make([]SweepStatement, 0, len(sweepStatements)+2)
	for _, q := range sweepStatements {
		out = append(out, SweepStatement{What: q.what, SQL: q.sql, Args: []any{1000}, PicksBatch: true})
	}
	return append(out,
		SweepStatement{What: "refresh token candidates", SQL: sweepRefreshCandidates, Args: []any{1000, time.Time{}, ""}, PicksBatch: true},
		SweepStatement{What: "refresh token families", SQL: sweepRefreshDelete, Args: []any{[]string{"a", "b"}}})
}
