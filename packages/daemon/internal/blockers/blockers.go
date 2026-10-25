// Package blockers mirrors what Dispatch says every issue of the daemon's project waits on, and
// judges from that mirror which of an issue's blockers are still unfinished.
//
// Admission decides whether to admit a root, and the workflow whether to start a child's first
// phase, inside a transaction that performs no I/O: neither can ask Dispatch what an issue is
// blocked by at the moment it decides. So the daemon keeps its own table of Dispatch's answer
// (record.DispatchIssueRow), written by Mirror as each issue event arrives and by Reconcile from
// the boot listing, and read back whole, one project at a time, by whoever judges.
package blockers

import (
	"cmp"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// doneStatus is the one Dispatch status that finishes an issue. Nothing else does: an issue moved
// to backlog or icebox has stopped, not finished, and an issue waiting on it still waits.
const doneStatus = "done"

// Row is the mirror as the judgement reads it: one issue's Dispatch status and the keys it waits
// on, in the order Dispatch lists them.
type Row struct {
	Status   string
	Blockers []string
}

// Rows indexes a project's mirror by issue key, as Unfinished takes it: every caller reads the
// whole project in one query (record.Store.DispatchIssues) and then judges in memory.
func Rows(mirrored []record.DispatchIssueRow) map[string]Row {
	rows := make(map[string]Row, len(mirrored))
	for _, row := range mirrored {
		rows[row.Key] = Row{Status: row.Status, Blockers: row.Blockers}
	}
	return rows
}

// Unfinished is the keys key waits on that are not done, in the order its row lists them, and is
// empty when key may proceed.
//
// A blocker with no row of its own counts as unfinished: a child can name a blocker whose
// issue.created the daemon has not consumed yet, and holding is the safe reading — the row
// arrives with the next event, the periodic turn or the next boot, and the wait ends then. A key
// with no row of its own, on the other hand, names no blockers at all and so waits on nothing.
func Unfinished(rows map[string]Row, key string) []string {
	unfinished := []string{}
	for _, blocker := range rows[key].Blockers {
		if row, ok := rows[blocker]; !ok || row.Status != doneStatus {
			unfinished = append(unfinished, blocker)
		}
	}
	return unfinished
}

// Blocked is every issue Legion is waiting to start that waits on an issue Dispatch has not
// finished, by issue key: what the daemon's state shows under `blocked` and what a tick notice
// tells the controller, from the one judgement both readings and both deciders share.
//
// It is the two decisions themselves, read rather than made: a root of waiting — the line
// admission promotes from, which the caller passes as it computes it — that promotion skips
// (admit.waitsOn), and a recorded child the workflow holds at `admitted`
// (workflow.Engine.StartUnblockedChildren). A candidate that carries a run is never judged, as
// neither decider judges one: work already under way is never stopped and never re-checked,
// whatever its links say.
func Blocked(rows map[string]Row, issues, waiting []record.Issue) []record.BlockedIssue {
	blocked := make([]record.BlockedIssue, 0, len(waiting))
	for _, issue := range waiting {
		if carriesRun(issue) {
			continue
		}
		if unfinished := Unfinished(rows, issue.Key); len(unfinished) > 0 {
			blocked = append(blocked, record.BlockedIssue{Issue: issue.Key, BlockedBy: unfinished})
		}
	}
	for _, issue := range issues {
		if claim.IsTreeRoot(issue.Key, issue.Tree) || issue.Phase != phase.Admitted {
			continue
		}
		if unfinished := Unfinished(rows, issue.Key); len(unfinished) > 0 {
			blocked = append(blocked, record.BlockedIssue{Issue: issue.Key, BlockedBy: unfinished})
		}
	}
	slices.SortFunc(blocked, func(a, b record.BlockedIssue) int { return cmp.Compare(a.Issue, b.Issue) })
	return blocked
}

// carriesRun says whether an issue carries a run: it started a phase, and its tree neither left
// it admitted nor finished it. It is admit.startedRun's rule, the one that keeps a handed-over
// child or a re-admitted tree out of every blocker judgement.
func carriesRun(issue record.Issue) bool {
	return issue.Phase != phase.Admitted && issue.Phase != phase.Done
}
