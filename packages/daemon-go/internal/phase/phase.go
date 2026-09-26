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

// FileBacked says whether a phase ends with a handoff file its role writes and commits: the
// planner's, the implementer's implementing rounds, the tester's, and the reviewer's. Retro, the
// production check, and the merger's READY write none, and report the commit they stand on.
func FileBacked(p Phase) bool {
	switch p {
	case Planning, Implementing, Testing, Reviewing:
		return true
	default:
		return false
	}
}
