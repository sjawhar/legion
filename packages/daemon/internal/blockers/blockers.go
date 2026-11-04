// Package blockers mirrors what Dispatch says every issue of the daemon's project waits on, and
// judges from that mirror which of an issue's blockers are still unfinished.
//
// Admission decides whether to admit a root, and the workflow whether to start a child's first
// phase, inside a transaction that performs no I/O: neither can ask Dispatch what an issue is
// blocked by at the moment it decides. So the daemon keeps its own table of Dispatch's answer
// (record.DispatchIssueRow), written by Mirror as each issue event arrives and by Reconcile from
// the boot listing, and read back for the issues a judgement names, with what they wait on.
package blockers

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

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

// Rows indexes mirrored rows by issue key, as Unfinished takes them: every caller reads the rows
// of the issues it judges in one query (Load) and then judges in memory.
func Rows(mirrored []record.DispatchIssueRow) map[string]Row {
	rows := make(map[string]Row, len(mirrored))
	for _, row := range mirrored {
		rows[row.Key] = Row{Status: row.Status, Blockers: row.Blockers}
	}
	return rows
}

// Load is the mirror of keys, the issues a judgement reads, and of every issue their rows wait
// on, read in one query (record.Store.DispatchIssuesOf) and indexed as every judgement here takes
// it. No keys read nothing.
//
// Admission decides whether to admit a root, and the workflow whether to start a child's first
// phase, inside a transaction that performs no I/O: neither can ask Dispatch what an issue is
// blocked by at the moment it decides. Both read the rows Mirror wrote instead — the deciding
// fact's own row included, written ahead of them in the same transaction — and judge them in
// memory. Each call that judges loads once, whatever the number of issues it judges, and reads the
// rows of those issues alone, never the project's whole mirror, which keeps a row of every issue
// the project ever had.
func Load(ctx context.Context, tx pgx.Tx, store record.Store, project string, keys []string) (map[string]Row, error) {
	if len(keys) == 0 {
		return map[string]Row{}, nil
	}
	mirrored, err := store.DispatchIssuesOf(ctx, tx, project, keys)
	if err != nil {
		return nil, fmt.Errorf("read what the issues of %s wait on: %w", project, err)
	}
	return Rows(mirrored), nil
}

// Judged is the keys whose rows a judgement of issues and waiting reads (Blocked, and the
// deciders it stands for): each root of waiting the rule does not exempt (WaitsOn), and each issue
// a blocker can hold where it stands (Holdable: Holds, and the waiting line's own hold through
// HeldIssues). Load reads these and what they wait on.
func Judged(issues, waiting []record.Issue) []string {
	keys := make([]string, 0, len(waiting))
	for _, issue := range waiting {
		if !Exempt(issue) {
			keys = append(keys, issue.Key)
		}
	}
	for _, issue := range issues {
		if Holdable(issue) {
			keys = append(keys, issue.Key)
		}
	}
	return keys
}

// Unfinished is the keys key waits on that are not done, in the order its row lists them, and is
// empty when key may proceed.
//
// A blocker with no row of its own counts as unfinished: a child can name a blocker whose
// issue.created the daemon has not consumed yet, and holding is the safe reading — the row
// arrives with the next event, the periodic turn or the next boot, and the wait ends then. A key
// with no row of its own, on the other hand, names no blockers at all and so waits on nothing.
func Unfinished(rows map[string]Row, key string) []string {
	return UnfinishedOf(rows, rows[key].Blockers)
}

// UnfinishedOf is the keys of blockedBy that are not done, in its order, judged as Unfinished
// judges a mirrored issue's own list. It takes the list from the caller for an issue whose own
// row may not exist yet: release_children answers for a child it has only just written todo,
// whose row arrives with that write's event, from the blockers Dispatch just showed it.
func UnfinishedOf(rows map[string]Row, blockedBy []string) []string {
	unfinished := []string{}
	for _, blocker := range blockedBy {
		if row, ok := rows[blocker]; !ok || row.Status != doneStatus {
			unfinished = append(unfinished, blocker)
		}
	}
	return unfinished
}

// Blocked is every issue Legion is waiting to start that waits on another issue, by issue key:
// what the daemon's state shows under `blocked` and what a tick notice tells the controller,
// from the one judgement both readings and both deciders share.
//
// It is the decisions themselves, read rather than made: a root of waiting — the line admission
// promotes from, which the caller passes as it computes it — that promotion skips (WaitsOn), and
// a recorded child the workflow holds at `admitted` (Holds; admission starts it once Holdable
// meets nothing Unfinished). A candidate the rule exempts is never judged, as the decider that
// would admit it does not judge it either (Exempt): what promotion starts whatever its links say
// is never reported as waiting on them.
//
// more is what a decider outside this package has already judged and rendered, which today is
// the children-first wait (record.ChildWaitRows): that rule reads the mirror's parent column
// against the daemon's records rather than an issue's blocked-by links, so it cannot be made
// here, and it must still reach the one list a reader reads. A key both deciders name keeps one
// row, with each one's issues in it — the deciders are independent, and an issue can wait on a
// link and on a child at once.
func Blocked(rows map[string]Row, issues, waiting []record.Issue, more []record.BlockedIssue) []record.BlockedIssue {
	blocked := make([]record.BlockedIssue, 0, len(waiting)+len(more))
	at := make(map[string]int, len(waiting)+len(more))
	add := func(key string, waits []string) {
		if index, named := at[key]; named {
			for _, wait := range waits {
				if !slices.Contains(blocked[index].BlockedBy, wait) {
					blocked[index].BlockedBy = append(blocked[index].BlockedBy, wait)
				}
			}
			return
		}
		at[key] = len(blocked)
		blocked = append(blocked, record.BlockedIssue{Issue: key, BlockedBy: waits})
	}
	for _, issue := range waiting {
		if waits := WaitsOn(rows, issue); len(waits) > 0 {
			add(issue.Key, waits)
		}
	}
	for _, issue := range issues {
		if waits := Holds(rows, issue); len(waits) > 0 {
			add(issue.Key, waits)
		}
	}
	for _, row := range more {
		add(row.Issue, row.BlockedBy)
	}
	slices.SortFunc(blocked, func(a, b record.BlockedIssue) int { return cmp.Compare(a.Issue, b.Issue) })
	return blocked
}

// Exempt says whether an issue starts whatever its links say, and so is neither held by the
// promotion skip (WaitsOn) nor reported here. It is the one definition the promotion skip and the
// state's reading of waiting roots share, so a root one of them exempts can never be reported
// blocked by the other.
//
// Two kinds of issue are exempt. One that carries a run into a tree of its own — a handed-over
// child, a re-admitted tree — is work already under way, which is never stopped and never
// re-checked (record.Issue.CarriesRun), by whichever door it arrives. And a tree Legion takes back
// after it has finished and lingered starts again at its person's word, though re-admission leaves
// it carrying no run: that is both halves at once, that it ran and that the tree coming back is its
// own, which record.Issue.TakenBack holds. The mark is a fact about a run: the workflow writes it
// as a phase of that run begins, re-admission keeps it when the root's own tree comes back, and
// every other path that opens a run starts it clean — a child's re-entry into its tree, and a
// child's hand-over to a root of its own without its run. So no issue is exempt unless the run it
// is exempt for is the run it arrived with. A root waiting on a person (awaiting_person, answered or
// not) carries its run (record.Issue.CarriesRun): its links were read when it was first admitted,
// and its resume takes the next free slot whatever they say now. Everything else is checked like
// any other issue: a child that has been released but never started, a waiting root that only left
// the line and came back, and a child handed over from a finished tree as a root of its own without
// its run, whether or not it ran there, a done child a person labels as an orphan root among them. A
// child held at phase.Admitted in its own tree is no candidate here at all: the workflow holds it,
// and Blocked reports it, by Holds, whatever its mark — and a re-entered child carries none.
func Exempt(issue record.Issue) bool { return issue.CarriesRun() || issue.TakenBack }

// WaitsOn is what issue still waits on, from rows: the issues Dispatch says it is blocked by
// that are not done (Unfinished, which counts a blocker the daemon has no row of as not done),
// and nothing at all for an issue the rule exempts, which starts whatever its links say — the
// one definition the promotion skip and the root half of the state's `blocked` both read, the
// exemption included (Exempt). The workflow's hold of a child reads Holds instead, which exempts
// nothing.
func WaitsOn(rows map[string]Row, issue record.Issue) []string {
	if Exempt(issue) {
		return nil
	}
	return Unfinished(rows, issue.Key)
}

// Holdable says whether a blocker can hold issue where it stands: it is a child of a tree rather
// than a root of its own, and it waits at phase.Admitted, where workflow.Engine.enterChild or
// the opening of its tree's gate left it. A root's wait is the waiting line's instead, judged
// over record.Waiting by the promotion that skips it.
func Holdable(issue record.Issue) bool {
	return !claim.IsTreeRoot(issue.Key, issue.Tree) && issue.Phase == phase.Admitted
}

// Holds is what holds issue where it stands: the issues it waits on that are not done
// (Unfinished) when a blocker can hold it there (Holdable), and nothing otherwise. It exempts
// nothing: a child held at phase.Admitted is held whatever its mark (Exempt). It is the one
// statement of the child hold rule — what a held child waits on — which the waiting line's own
// hold (record.LineFacts.Held, through HeldIssues) and the child half of Blocked both read.
func Holds(rows map[string]Row, issue record.Issue) []string {
	if !Holdable(issue) {
		return nil
	}
	return Unfinished(rows, issue.Key)
}

// HeldIssues is the set of issues among issues that a blocker holds where they stand (Holds),
// which is what the waiting line takes as record.LineFacts.Held: the line must not offer a slot
// to an issue whose first phase an unfinished Dispatch issue holds, and it cannot judge that
// itself, since internal/record may not read this package. Every caller that reads the line
// therefore loads the mirror rows first (Load over Judged with no line) and fills the field from
// here, which JudgeLine does for all of them, so the line, the promotion's own skip and the
// state's `blocked` all answer from one judgement of one read.
func HeldIssues(rows map[string]Row, issues []record.Issue) map[string]struct{} {
	held := make(map[string]struct{})
	for _, issue := range issues {
		if len(Holds(rows, issue)) > 0 {
			held[issue.Key] = struct{}{}
		}
	}
	return held
}

// JudgeLine reads what the issues of project wait on and builds the waiting line from it, in the
// one order the two allow. First the mirror rows of every issue a blocker can hold (Judged with
// no line), because the line must leave out every issue whose first phase one of them holds and
// cannot judge that itself: internal/record may not read this package, so the hold is handed to
// the line as a fact (record.LineFacts.Held, HeldIssues). The children-first wait is handed to
// it the same way and for a like reason — the line is given facts rather than a transaction, and
// that judgement reads the Dispatch mirror against the daemon's own records
// (record.LineFacts.ChildWaits, record.ChildWaitsOf) — and is read again on each judgement
// rather than stored anywhere, so a mirror row that has gone stale holds a parent for one pass
// and no longer. Then the line. Then the rows of the issues the line itself names, which only the
// line can say.
//
// rows is whatever the caller has already read, nil on its first judgement; judgeAgain reads only
// the keys it does not hold, so a pass that reads the line twice makes one mirror read in all.
// The facts come back with the held set and the child waits filled, so the release, the
// promotion, the architect's notice and the state's `blocked` all answer from the one judgement.
// slots is the caller's own read of the slots table, which record.Waiting reads as the set of
// issues already holding one.
//
// It is the one place the line is judged: admission's own pass (admit's promoteHolds), the
// projected state and the controller tick's read of what the project waits on all come through
// here, so no two of them can judge the line from different reads or leave a later hold out of
// one of the three.
func JudgeLine(ctx context.Context, tx pgx.Tx, store record.Store, project string, issues []record.Issue,
	slots []record.Slot, facts record.LineFacts, rows map[string]Row,
) (map[string]Row, record.LineFacts, []record.LineEntry, error) {
	if rows == nil {
		rows = map[string]Row{}
	}
	rows, err := judgeAgain(ctx, tx, store, project, issues, nil, rows)
	if err != nil {
		return rows, facts, nil, err
	}
	facts.Held = HeldIssues(rows, issues)
	if facts.ChildWaits, err = record.ChildWaitsOf(ctx, tx, store, project, issues, facts.Gates); err != nil {
		return rows, facts, nil, fmt.Errorf("read what the unstarted issues of %s wait on under them: %w", project, err)
	}
	line := record.Waiting(issues, slots, facts)
	rows, err = judgeAgain(ctx, tx, store, project, issues, line, rows)
	return rows, facts, line, err
}

// judgeAgain adds to rows the mirror rows of every key the rebuilt line judges that the first
// judgement did not name. The rows already read stand — no Dispatch issue finishes inside one
// transaction — so only the new keys cost a read, and a round that names none costs nothing.
func judgeAgain(ctx context.Context, tx pgx.Tx, store record.Store, project string, issues []record.Issue,
	line []record.LineEntry, rows map[string]Row,
) (map[string]Row, error) {
	var fresh []string
	for _, key := range Judged(issues, record.WaitingIssues(line)) {
		if _, known := rows[key]; !known && !slices.Contains(fresh, key) {
			fresh = append(fresh, key)
		}
	}
	if len(fresh) == 0 {
		return rows, nil
	}
	more, err := Load(ctx, tx, store, project, fresh)
	if err != nil {
		return rows, err
	}
	maps.Copy(rows, more)
	return rows, nil
}
