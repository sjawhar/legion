package record

import "sort"

// RankLess compares Dispatch's fractional key, compared as bytes — the order rank.Between
// generates. Equal keys are ordered by issue key.
func RankLess(a, b Issue) bool {
	if a.Rank == b.Rank {
		return a.Key < b.Key
	}
	return a.Rank < b.Rank
}

// Waiting returns slotless todo roots and orphans, in Dispatch rank order.
func Waiting(issues []Issue, slots []Slot) []Issue {
	slotted := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		slotted[slot.Issue] = struct{}{}
	}
	waiting := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Status == "todo" && issue.Tree == issue.Key {
			if _, ok := slotted[issue.Key]; !ok {
				waiting = append(waiting, issue)
			}
		}
	}
	sort.Slice(waiting, func(i, j int) bool { return RankLess(waiting[i], waiting[j]) })
	return waiting
}
