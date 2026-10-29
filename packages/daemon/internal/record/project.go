package record

import (
	"context"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// RankLess compares Dispatch's fractional key, compared as bytes — the order rank.Between
// generates. Equal keys are ordered by issue key.
func RankLess(a, b Issue) bool {
	if a.Rank == b.Rank {
		return a.Key < b.Key
	}
	return a.Rank < b.Rank
}

// OutOfWorkflow says whether a Dispatch status takes an issue out of the workflow: every status
// but todo that no phase writes. Admission holds no slot for such an issue, and the engine leaves
// it; todo admits, and in_progress, testing, needs_review and retro are the workflow's own.
func OutOfWorkflow(status string) bool {
	switch status {
	case "triage", "icebox", "backlog", "done":
		return true
	default:
		return false
	}
}

// Waiting returns the slotless roots handed to Legion that may take a slot. A root a person
// answered while it waited in awaiting_person comes before every root that never ran, ordered by
// its answer time and then rank; the others follow, the slotless todo roots and orphans, in
// Dispatch rank order. An unanswered awaiting root waits on the person rather than a slot,
// whatever Dispatch status it currently shows, so it is not in the line until it is Answered. A
// root without the label waits for nothing; a child's tree holds its place, so a child needs none.
// A root that never ran, whose recorded parent is a live tree's member, is adopted into that tree
// before admission reads the line (admit's adoptQueuedRoots), so it is not read as waiting there.
// A root under a parent unrecorded or recorded as a root, whose Dispatch chain (Ancestry) holds
// any live root but its own key, is held under an unreleased child of that tree (the ancestry
// hold) and waits for nothing: it is that tree's unreleased work, which its architect's release,
// or its parent's, brings into the tree, exactly as release_children's own walk of the chain
// reaches it. A recorded root on the chain that is not live is passed over. A tree is live here as
// everywhere (LiveRoots), so the tree of a root held under an unreleased child, or of a root
// waiting on a person, is live too; once no live root is left on its chain (each lingers or leaves
// the workflow, or a person moves an ancestor out from under it), the root waits as its own again.
// A root that ran, an awaiting one among them, records no chain (workflow's Ancestry), so the hold
// never takes an answered root out of the line. The other hold, on Dispatch blocked-by links, does
// not take a root out of here: promotion passes such a root over in the line (blockers.WaitsOn),
// and the state names it under admission.blocked.
func Waiting(issues []Issue, slots []Slot) []Issue {
	waiting := line(issues, slots)
	if slices.ContainsFunc(waiting, func(issue Issue) bool { return len(issue.Ancestry) != 0 }) {
		live := liveRoots(issues, slots, waiting)
		waiting = slices.DeleteFunc(waiting, func(issue Issue) bool {
			return slices.ContainsFunc(issue.Ancestry, func(key string) bool {
				_, under := live[key]
				return under && key != issue.Key
			})
		})
	}
	sort.Slice(waiting, func(i, j int) bool {
		left, right := waiting[i], waiting[j]
		leftAnswered, rightAnswered := left.Answered(), right.Answered()
		switch {
		case leftAnswered && rightAnswered:
			if !left.Awaiting.AnsweredAt.Equal(*right.Awaiting.AnsweredAt) {
				return left.Awaiting.AnsweredAt.Before(*right.Awaiting.AnsweredAt)
			}
		case leftAnswered:
			return true
		case rightAnswered:
			return false
		}
		return RankLess(left, right)
	})
	return waiting
}

// line is every slotless root handed to Legion that may take a slot, in no order, before the
// ancestry hold is read: the answered awaiting roots and the todo roots that wait on no person.
// Waiting leaves the ones held under an unreleased child out of it, and with the slotted roots and
// the roots waiting on a person it gives the live trees (liveRoots).
func line(issues []Issue, slots []Slot) []Issue {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	line := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if !issue.HandedOver || !claim.IsTreeRoot(issue.Key, issue.Tree) {
			continue
		}
		if _, ok := slotted[issue.Key]; ok {
			continue
		}
		if issue.Answered() || (!issue.AwaitingPerson() && issue.Status == "todo") {
			line = append(line, issue)
		}
	}
	return line
}

// TreeLive says whether root's tree is live: the root holds a slot, waits for one (held under an
// unreleased child or not), or waits on a person with its session suspended, and does not linger
// after its close. A todo child under a live tree runs in it, or waits with its root held under an
// unreleased child and moves with it into the tree that adopts it; under any other it is an
// orphan.
func TreeLive(ctx context.Context, store Store, tx pgx.Tx, root Issue) (bool, error) {
	if root.Lingers() {
		return false, nil
	}
	if root.AwaitingPerson() {
		return true, nil
	}
	slots, err := store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(slots, func(slot Slot) bool { return slot.Issue == root.Key }) {
		return true, nil
	}
	issues, err := store.Issues(ctx, tx)
	if err != nil {
		return false, err
	}
	_, live := LiveRoots(issues, slots)[root.Key]
	return live, nil
}

// LiveRoots is the set of keys whose trees are live (TreeLive), read from issues and slots already
// in hand.
func LiveRoots(issues []Issue, slots []Slot) map[string]struct{} {
	return liveRoots(issues, slots, line(issues, slots))
}

// liveRoots is LiveRoots over line, the line already read from issues and slots: each root does
// not linger and holds a slot, is in the line, or waits on a person (TreeLive), its returned slot
// leaving its tree as live as before. A root Waiting holds under an unreleased child is still in the
// line, so the ancestry hold is read against these same trees and a todo child of such a root
// records into its tree.
func liveRoots(issues []Issue, slots []Slot, line []Issue) map[string]struct{} {
	live := make(map[string]struct{}, len(slots)+len(line))
	for _, slot := range slots {
		live[slot.Issue] = struct{}{}
	}
	for _, issue := range line {
		live[issue.Key] = struct{}{}
	}
	for _, issue := range issues {
		switch {
		case issue.Lingers():
			delete(live, issue.Key)
		case issue.AwaitingPerson():
			live[issue.Key] = struct{}{}
		}
	}
	return live
}
