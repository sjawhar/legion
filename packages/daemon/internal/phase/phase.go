// Package phase owns the issue workflow phase vocabulary.
package phase

import "github.com/sjawhar/legion/daemon/internal/claim"

// Phase is the issue's position in the daemon's transition table — the state it sits in, not the
// role working it (`workers` is keyed by role). The daemon advances it from facts it observes; no
// agent chooses it. The order is the transition table's own.
type Phase string

const (
	Admitted Phase = "admitted"
	// AwaitingPerson is a root admitted tree that waits only on a person: its architect asked a
	// question and handed it to the person. It holds no admission slot, and its architect stays
	// live in its own session, reading and answering the person's comments; the person's answer
	// puts the root at the front of the waiting line, and its slot delivers the architect its task.
	AwaitingPerson Phase = "awaiting_person"
	Planning       Phase = "planning"
	Implementing   Phase = "implementing"
	Testing        Phase = "testing"
	Reviewing      Phase = "reviewing"
	Accepting      Phase = "accepting"
	Retro          Phase = "retro"
	Merging        Phase = "merging"
	AwaitingMerge  Phase = "awaiting_merge"
	Done           Phase = "done"
	Held           Phase = "held"
)

// All is every phase, in the transition table's order.
var All = []Phase{
	Admitted, AwaitingPerson, Planning, Implementing, Testing, Reviewing, Accepting, Retro, Merging,
	AwaitingMerge, Done, Held,
}

// Handoff is the word of the Dispatch handoff record a phase ends with, the role that writes it,
// and whether the phase ends with one: the planner's, the implementer's implementing rounds, the
// tester's, and the reviewer's, each under the word its role's prompt spells
// (internal/prompts/roles/*.md), the phase of the record whose newest version the `legion` tool's
// `handoff_complete` reads and names (packages/pi-shared/src/handoff-record.ts). The writer is the
// role the workflow starts on the phase (workflow.RoleFor, whose tests tie the two). Retro and the
// merger's READY write none, and report the commit they stand on; accepting is the architect's
// sign-off, no handoff at all.
func Handoff(p Phase) (word string, writer claim.Role, ok bool) {
	switch p {
	case Planning:
		return "plan", claim.RolePlanner, true
	case Implementing:
		return "implement", claim.RoleImplementer, true
	case Testing:
		return "test", claim.RoleTester, true
	case Reviewing:
		return "review", claim.RoleReviewer, true
	default:
		return "", "", false
	}
}
