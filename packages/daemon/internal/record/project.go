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
// A root whose parent is not recorded, whose nearest recorded root up its Dispatch chain is in
// another live tree (RecordedAncestor, whose current tree is read here, so a hold follows that
// root's adoption into a tree), waits for nothing either: it is that tree's unreleased work, which
// its architect's release, or its parent's, brings into the tree. A tree is live here as
// everywhere (LiveRoots), so a held root's tree is live too; once the tree it is held under lingers
// or leaves the workflow, or a person moves an ancestor out from under it, the root waits as its
// own again.
func Waiting(issues []Issue, slots []Slot) []Issue {
	waiting := line(issues, slots)
	if slices.ContainsFunc(waiting, func(issue Issue) bool { return issue.RecordedAncestor != "" }) {
		live := liveRoots(issues, slots, waiting)
		trees := make(map[string]string, len(issues))
		for _, issue := range issues {
			trees[issue.Key] = issue.Tree
		}
		waiting = slices.DeleteFunc(waiting, func(issue Issue) bool {
			tree, ok := trees[issue.RecordedAncestor]
			if !ok || issue.RecordedAncestor == issue.Key {
				return false
			}
			_, under := live[tree]
			return under && tree != issue.Key
		})
	}
	sort.Slice(waiting, func(i, j int) bool { return RankLess(waiting[i], waiting[j]) })
	return waiting
}

// line is every slotless todo root handed to Legion, in no order, before any hold is read: the
// roots Waiting leaves the held ones out of, and with the slotted roots the live trees (liveRoots).
func line(issues []Issue, slots []Slot) []Issue {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	line := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Status == "todo" && issue.HandedOver && claim.IsTreeRoot(issue.Key, issue.Tree) {
			if _, ok := slotted[issue.Key]; !ok {
				line = append(line, issue)
			}
		}
	}
	return line
}

// TreeLive says whether root's tree is live: the root holds a slot or waits for one, held or not,
// and does not linger after its close. A todo child under a live tree runs in it, or waits with its
// held root and moves with it into the tree that adopts it; under any other it is an orphan.
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
// in hand.
func LiveRoots(issues []Issue, slots []Slot) map[string]struct{} {
	return liveRoots(issues, slots, line(issues, slots))
}

// liveRoots is LiveRoots over line, the line already read from issues and slots: each root does
// not linger and holds a slot or is in the line. A root Waiting holds is still in the line, so the
// hold is read against these same trees and a todo child of a held root records into its tree.
func liveRoots(issues []Issue, slots []Slot, line []Issue) map[string]struct{} {
	live := make(map[string]struct{}, len(slots)+len(line))
	for _, slot := range slots {
		live[slot.Issue] = struct{}{}
	}
	for _, issue := range line {
		live[issue.Key] = struct{}{}
	}
	for _, issue := range issues {
		if issue.Lingers() {
			delete(live, issue.Key)
		}
	}
	return live
}
