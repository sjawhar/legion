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
	// Unrecorded is not a workflow phase and no issue record ever holds it: it is what the state
	// route says for an issue the workflow does not record, where an operator's claim exists and
	// an issue does not. The transition table never reaches it, and no role works it.
	Unrecorded Phase = "unrecorded"
)

// HandoffFile is the handoff a phase ends with, .legion/<word>.json, and whether it ends with one
// its role writes and commits: the planner's, the implementer's implementing rounds, the tester's,
// and the reviewer's, each under the word its role's prompt passes to the legion tool's
// handoff_write (packages/pi-envoy/roles/*.md). Retro, the production check, and the merger's
// READY write none, and report the commit they stand on.
func HandoffFile(p Phase) (string, bool) {
	switch p {
	case Planning:
		return "plan", true
	case Implementing:
		return "implement", true
	case Testing:
		return "test", true
	case Reviewing:
		return "review", true
	default:
		return "", false
	}
}

// FileBacked says whether a phase ends with a handoff file its role writes and commits
// (HandoffFile).
func FileBacked(p Phase) bool {
	_, ok := HandoffFile(p)
	return ok
}
