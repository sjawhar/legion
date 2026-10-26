// Package phase owns the issue workflow phase vocabulary.
package phase

// Phase is the issue's position in the daemon's transition table — the state it sits in, not the
// role working it (`workers` is keyed by role). The daemon advances it from facts it observes; no
// agent chooses it. The order is the transition table's own.
type Phase string

const (
	Admitted      Phase = "admitted"
	Planning      Phase = "planning"
	Implementing  Phase = "implementing"
	Testing       Phase = "testing"
	Reviewing     Phase = "reviewing"
	Accepting     Phase = "accepting"
	Retro         Phase = "retro"
	Merging       Phase = "merging"
	AwaitingMerge Phase = "awaiting_merge"
	Done          Phase = "done"
	Held          Phase = "held"
)

// All is every phase, in the transition table's order.
var All = []Phase{
	Admitted, Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging, AwaitingMerge,
	Done, Held,
}

// HandoffFile is the word of the handoff a phase ends with, and whether it ends with one its role
// writes: the planner's, the implementer's implementing rounds, the tester's, and the reviewer's,
// each under the word its role's prompt spells (internal/prompts/roles/*.md), the phase of the
// Dispatch handoff record whose newest version the `legion` tool's `handoff_complete` reads and
// names (packages/pi-shared/src/handoff-record.ts). Retro and the merger's READY write none, and
// report the commit they stand on; accepting is the architect's sign-off, no handoff at all.
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
