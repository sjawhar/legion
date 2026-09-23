// Package phase owns the issue workflow phase vocabulary.
package phase

// Phase is the issue's position in the daemon's transition table — the state it sits in, not the
// role working it (`workers` is keyed by role). The daemon advances it from facts it observes; no
// agent chooses it. The order is the transition table's own.
type Phase string

const (
	Admitted        Phase = "admitted"
	Planning        Phase = "planning"
	Implementing    Phase = "implementing"
	Testing         Phase = "testing"
	Reviewing       Phase = "reviewing"
	Retro           Phase = "retro"
	Merging         Phase = "merging"
	AwaitingMerge   Phase = "awaiting_merge"
	ProductionCheck Phase = "production_check"
	Done            Phase = "done"
	Held            Phase = "held"
)
