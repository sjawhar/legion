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
// (workflow.Engine.StartUnblockedChildren). A candidate the rule exempts is never judged, as the
// decider that would admit it does not judge it either (Exempt): what promotion starts whatever
// its links say is never reported as waiting on them.
func Blocked(rows map[string]Row, issues, waiting []record.Issue) []record.BlockedIssue {
	blocked := make([]record.BlockedIssue, 0, len(waiting))
	for _, issue := range waiting {
		if Exempt(issue) {
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

// Exempt says whether an issue starts whatever its links say, and so is neither held by the
// promotion skip (admit.waitsOn) nor reported here. It is the one definition both readings share,
// so an issue one of them exempts can never be reported blocked by the other.
//
// Two kinds of issue are exempt. One that carries a run into a tree of its own — a handed-over
// child, a re-admitted tree — is work already under way, which is never stopped and never
// re-checked (record.Issue.CarriesRun). And a tree Legion takes back after it has finished and
// lingered starts again at its person's word (record.Issue.TakenBack), though re-admission leaves
// it carrying no run. Everything else is checked like any other issue, a child that has been
// released but never started among them.
func Exempt(issue record.Issue) bool { return issue.CarriesRun() || issue.TakenBack }
