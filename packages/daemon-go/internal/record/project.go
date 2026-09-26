package record

import (
	"context"
	"slices"
	"sort"
	"strings"

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

// LegionLabel is the Dispatch label that hands an issue to Legion. A human sets it from the issue
// header in the Dispatch dashboard, an agent with Dispatch's issue tools. A label is the mark, and
// not the issue's route, because a route has Dispatch publish every event of the issue to the
// route's topic, while a label only marks it.
const LegionLabel = "legion"

// CarriesLegionLabel says whether labels include LegionLabel. Dispatch keeps a label's case as it
// was typed and holds labels differing only in case as one label, so the match ignores case.
func CarriesLegionLabel(labels []string) bool {
	return slices.ContainsFunc(labels, func(label string) bool { return strings.EqualFold(label, LegionLabel) })
}

// Waiting returns the slotless todo roots and orphans handed to Legion, in Dispatch rank order. A
// root without the label waits for nothing; a child's tree holds its place, so a child needs none.
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
	sort.Slice(waiting, func(i, j int) bool { return RankLess(waiting[i], waiting[j]) })
	return waiting
}

// TreeLive says whether root's tree is live: the root holds a slot or waits for one, and does not
// linger after its close. A todo child under a live tree runs in it; under any other it is an orphan.
func TreeLive(ctx context.Context, store Store, tx pgx.Tx, root Issue) (bool, error) {
	if root.LingerUntil != nil {
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
	return slices.ContainsFunc(Waiting(issues, slots), func(waiting Issue) bool { return waiting.Key == root.Key }), nil
}
