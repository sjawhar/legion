package record

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// ChildrenFirstCandidates is every issue of issues whose own first phase has not started yet and
// could: it stands at phase.Admitted, Dispatch still shows it in the workflow, its tree is not
// lingering, and its tree's design gate stands open. Those are the only issues the
// children-first rule can hold, so they are the only parents whose Dispatch children are read at
// all (ChildWaitsOf). The keys come out sorted, so one pass reads the same set in the same order
// whatever order the records arrived in.
//
// The open gate is what puts the start of the wait where the spec puts it: at the gate's
// opening, before the architect's design-approved turn releases the wave. Before that opening a
// root's architect is still writing the spec and its own first phase is nothing the line would
// offer a slot to, so a root there waits for nothing and is told nothing; from the opening the
// root and every released member of its tree are issues whose first phase the line does offer a
// slot to, and each of them waits for whatever Dispatch still shows hanging under it.
func ChildrenFirstCandidates(issues []Issue, gates map[string]DesignGate) []string {
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Phase != phase.Admitted || !InWorkflow(issue) || byKey[issue.Tree].Lingers() {
			continue
		}
		if gate, registered := gates[issue.Tree]; !registered || !gateOpen(gate) {
			continue
		}
		keys = append(keys, issue.Key)
	}
	slices.Sort(keys)
	return keys
}

// ChildOpen says whether one Dispatch child of a parent still holds that parent's own first
// phase. It is read over two sources and no others: the status Dispatch shows the child in, from
// the daemon's mirror of the project's issues, and the daemon's own record of the child, nil when
// it has never recorded one.
//
//	| Dispatch status of the child       | daemon record of the child                                   | open? |
//	| ---------------------------------- | ------------------------------------------------------------ | ----- |
//	| done                               | any                                                            | no    |
//	| icebox                             | any                                                            | no    |
//	| backlog                            | none                                                           | yes   |
//	| backlog                            | one whose phase is done or whose status left the workflow      | no    |
//	| triage                             | none                                                           | yes   |
//	| triage                             | one that left the workflow                                     | no    |
//	| todo, in_progress, testing, …      | none, or one that is no tree of its own                        | yes   |
//	| todo, in_progress, testing, …      | a tree of its own that has left the workflow                   | no    |
//
// A child the architect has filed and not released yet — backlog or triage, with no record of its
// own — is open, because it is work of the parent that has still to happen: the parent's wait
// therefore begins at its gate's opening, before the design-approved turn releases the wave. The
// same two statuses on a child some tree of the daemon records are the opposite case, a child a
// tree ran and parked or finished, which holds nothing. Which tree recorded it does not come into
// it: a parked child holds nothing wherever it was parked, and reading it as open because another
// tree parked it would hold its parent on work no tree will do.
//
// A descendant that has become a tree of its own — handed over, or recorded under its own key as
// its tree's root — is read by its own record whatever Dispatch shows: its run belongs to that
// tree now, and it is open exactly while it is in the workflow there.
//
// An unknown status is open. The safe reading of a status this daemon does not know is that the
// child still has work in it; a wait costs a pass, and starting a parent too early costs its
// children's merges.
func ChildOpen(child DispatchIssueRow, recorded *Issue) bool {
	switch child.Status {
	case "done", "icebox":
		return false
	}
	if recorded != nil && (recorded.HandedOverFrom != "" || claim.IsTreeRoot(recorded.Key, recorded.Tree)) {
		return InWorkflow(*recorded)
	}
	switch child.Status {
	case "backlog", "triage":
		return recorded == nil || InWorkflow(*recorded)
	default:
		return true
	}
}

// ChildWaits is the children-first judgement of one admission pass: for each parent of children,
// the open Dispatch children under it (ChildOpen), by parent key. A parent with none has no
// entry, so the map is exactly the set of issues whose own first phase waits. children is what
// the mirror says hangs under each parent (Store.DispatchChildren) and issues the records
// the pass already holds.
//
// Nothing here is stored. The wait ends when every open child is released and leaves the
// workflow, is closed, is moved to icebox, is moved to another parent, or is moved out from
// under its parent to no parent at all — Dispatch reports the last two the same way, as the
// issue's parent now, and the mirror takes both (Store.PutDispatchIssue). Each of those is a
// change to a mirror row or a record that the next pass reads. A flag would have to be cleared by
// whichever of them happened, and a missed clear would hold a parent for ever; a judgement made
// again each pass costs one pass when a mirror row is stale, and the boot listing and the
// controller tick both rewrite the mirror.
//
// The wait is only as complete as the mirror is. A child in backlog or triage that the daemon has
// never recorded reaches the mirror only from a Dispatch listing of the whole project — the boot
// read and the controller tick's re-read — because the daemon consumes the events of its own
// project's issues and such a child has none it has applied. So a daemon that has not listed
// since the child was filed does not see it, and the children-first wait is complete only from
// the first boot listing after the child exists.
func ChildWaits(issues []Issue, children map[string][]DispatchIssueRow, records map[string]Issue) map[string]ChildWait {
	byKey := make(map[string]Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	waits := make(map[string]ChildWait, len(children))
	for key, rows := range children {
		parent, recorded := byKey[key]
		if !recorded {
			continue
		}
		var open []OpenChild
		for _, row := range rows {
			var child *Issue
			if found, ok := records[row.Key]; ok {
				child = &found
			} else if found, ok := byKey[row.Key]; ok {
				child = &found
			}
			if !ChildOpen(row, child) {
				continue
			}
			open = append(open, OpenChild{Key: row.Key, Status: row.Status, Cycle: cycleAncestor(row, parent, byKey)})
		}
		if len(open) > 0 {
			waits[key] = ChildWait{Parent: key, Open: open}
		}
	}
	return waits
}

// cycleAncestor is the issue a Dispatch blocked_by link of child names that stands above child on
// its own Dispatch parent chain — its parent itself, or one above that — and "" when no link
// does. Such a link is a cycle: the child waits for an issue that is waiting for the child, and
// neither can ever start. The chain is walked over the recorded parents the pass already holds
// (AcceptedAncestor walks the same one), starting at the child's parent, which is every
// ancestor inside the tree.
//
// The daemon names it and does nothing else. Taking the link off is a decision about what the
// work depends on, which is a person's to make, and the alternative — the daemon dropping a
// Dispatch link of its own accord — would lose a fact nobody asked it to lose.
func cycleAncestor(child DispatchIssueRow, parent Issue, byKey map[string]Issue) string {
	above := map[string]bool{parent.Key: true}
	seen := map[string]bool{child.Key: true, parent.Key: true}
	for key := parent.Parent; key != nil && !seen[*key]; {
		seen[*key] = true
		above[*key] = true
		next, recorded := byKey[*key]
		if !recorded {
			break
		}
		key = next.Parent
	}
	for _, blocker := range child.Blockers {
		if above[blocker] {
			return blocker
		}
	}
	return ""
}

// ChildWaitsOf reads the Dispatch children of every issue whose first phase has not started
// (ChildrenFirstCandidates) and judges them (ChildWaits). It is one query whatever the number of
// parents, because every judgement the daemon makes about children-first runs inside a
// transaction that performs no I/O of its own and under the one fact lock every fact takes
// (intake.ApplyFact): a read per parent would cost a round trip per unstarted issue on every fact
// of every consumer.
func ChildWaitsOf(ctx context.Context, tx pgx.Tx, store Store, project string, issues []Issue, gates map[string]DesignGate) (map[string]ChildWait, error) {
	children, err := store.DispatchChildren(ctx, tx, project, ChildrenFirstCandidates(issues, gates))
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(children))
	for _, rows := range children {
		for _, row := range rows {
			keys = append(keys, row.Key)
		}
	}
	if len(keys) == 0 {
		return map[string]ChildWait{}, nil
	}
	slices.Sort(keys)
	// The daemon's own record of each child is read by key rather than taken from issues, the
	// project's live records: a child that has left the workflow is not among those, in this
	// parent's tree or in any other, and a child the daemon has never recorded is not either,
	// and the two are opposite answers — the first holds nothing, the second is work nobody has
	// released yet (ChildOpen). One query, whatever the number of children.
	rows, err := store.IssuesOf(ctx, tx, project, slices.Compact(keys))
	if err != nil {
		return nil, err
	}
	records := make(map[string]Issue, len(rows))
	for _, row := range rows {
		records[row.Key] = row
	}
	return ChildWaits(issues, children, records), nil
}

// ChildWaitRows is the children-first waits as the rows the state's `blocked` and a tick
// controller notice carry. Each waiting parent gives one row naming the open children under it,
// and each cycle among them gives a second row, naming the child and the ancestor its own
// blocked_by link holds it under: the link is a fact about the child, not about the parent, and
// a reader that saw only the parent's row would have no way to tell a child nobody has released
// from one that can never be released at all. The architect's notice says the same two things in
// words (ChildCycleReason). They are built from the judgement the line is held by, so what a
// reader sees is what the decider decided.
func ChildWaitRows(waits map[string]ChildWait) []BlockedIssue {
	rows := make([]BlockedIssue, 0, len(waits))
	for parent, wait := range waits {
		by := make([]string, 0, len(wait.Open))
		for _, child := range wait.Open {
			by = append(by, child.Key)
			if child.Cycle != "" {
				rows = append(rows, BlockedIssue{Issue: child.Key, BlockedBy: []string{child.Cycle}})
			}
		}
		rows = append(rows, BlockedIssue{Issue: parent, BlockedBy: by})
	}
	slices.SortFunc(rows, func(a, b BlockedIssue) int { return strings.Compare(a.Issue, b.Issue) })
	return rows
}
