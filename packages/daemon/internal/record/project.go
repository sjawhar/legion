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

// Waiting returns the slotless todo roots and orphans handed to Legion, in Dispatch rank order. A
// root without the label waits for nothing; a child's tree holds its place, so a child needs none.
// A root that never ran, whose recorded parent is a live tree's member, is adopted into that tree
// before admission reads the line (admit's adoptQueuedRoots), so it is not read as waiting there.
// A root whose parent is not recorded, under an ancestor recorded in another live tree
// (AncestorTree), waits for nothing either: it is that tree's unreleased work, which its
// architect's release, or its parent's, brings into the tree. A tree is live here when its root
// does not linger and holds a slot or is itself in the line this reads before leaving such roots
// out; once that tree lingers or leaves the workflow, the root waits as its own again.
func Waiting(issues []Issue, slots []Slot) []Issue {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	waiting := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Status == "todo" && issue.HandedOver && claim.IsTreeRoot(issue.Key, issue.Tree) {
			if _, ok := slotted[issue.Key]; !ok {
				waiting = append(waiting, issue)
			}
		}
	}
	if slices.ContainsFunc(waiting, func(issue Issue) bool { return issue.AncestorTree != "" }) {
		queued := make(map[string]struct{}, len(waiting))
		for _, issue := range waiting {
			queued[issue.Key] = struct{}{}
		}
		live := make(map[string]struct{}, len(slots)+len(waiting))
		for _, issue := range issues {
			_, holds := slotted[issue.Key]
			_, waits := queued[issue.Key]
			if (holds || waits) && !issue.Lingers() {
				live[issue.Key] = struct{}{}
			}
		}
		waiting = slices.DeleteFunc(waiting, func(issue Issue) bool {
			_, under := live[issue.AncestorTree]
			return under && issue.AncestorTree != issue.Key
		})
	}
	sort.Slice(waiting, func(i, j int) bool { return RankLess(waiting[i], waiting[j]) })
	return waiting
}

// TreeLive says whether root's tree is live: the root holds a slot or waits for one, and does not
// linger after its close. A todo child under a live tree runs in it; under any other it is an orphan.
func TreeLive(ctx context.Context, store Store, tx pgx.Tx, root Issue) (bool, error) {
	if root.Lingers() {
		return false, nil
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
// in hand: each root does not linger and holds a slot or waits for one.
func LiveRoots(issues []Issue, slots []Slot) map[string]struct{} {
	live := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		live[slot.Issue] = struct{}{}
	}
	for _, issue := range Waiting(issues, slots) {
		live[issue.Key] = struct{}{}
	}
	for _, issue := range issues {
		if issue.Lingers() {
			delete(live, issue.Key)
		}
	}
	return live
}
