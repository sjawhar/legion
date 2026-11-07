package admit

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// lineIssue is one record of the waiting line's fixture, with the fields the line reads and
// nothing else: the rest of a record says nothing about where an issue stands in it.
func lineIssue(key, tree, status string, stands phase.Phase, rank string) record.Issue {
	return record.Issue{Key: key, Tree: tree, Project: testProject, Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: rank, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// A4. The waiting line's order, read from record.Waiting over one pass's facts.
//
// Its first part is the issues whose work came back, oldest first: an approved root whose own
// first phase can start, placed at the moment its gate opened, and an issue that owes its worker
// a start, placed at that row's creation. Its second part is the issues whose first phase has
// never started, by the Dispatch rank of their tree's root: a root that never ran and ranks
// above a live tree's root goes before that tree's child, and that child goes before a root
// ranked below its tree's root. A person's todo on a running root that holds no slot does not
// put it in the line as a root that never ran.
func TestTheWaitingLineRunsReturnsFirstThenTheBoardsRank(t *testing.T) {
	opened := fixedNow.Add(-2 * time.Hour)
	approved := 1
	issues := []record.Issue{
		// A live tree ranked in the middle of the board, its gate open, its root's own first
		// phase still to come and one released child waiting for its first.
		lineIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
		lineIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"),
		// A child of that tree whose work came back: its merger's start waits for a slot.
		lineIssue("LEGION-12", "LEGION-10", "retro", phase.Merging, "O"),
		// A root that never ran, ranked above the live tree's root.
		lineIssue("LEGION-20", "LEGION-20", "todo", phase.Admitted, "A"),
		// A root that never ran, ranked below it.
		lineIssue("LEGION-30", "LEGION-30", "todo", phase.Admitted, "Z"),
		// A root a person moved back to todo while its own tree runs and it waits in accepting:
		// it holds no slot, and it is no root that never ran.
		lineIssue("LEGION-50", "LEGION-50", "todo", phase.Accepting, "B"),
	}
	facts := record.LineFacts{
		Lifecycles: map[string]treelifecycle.Lifecycle{
			"LEGION-10": {Tree: "LEGION-10", Authority: treelifecycle.AuthorityWorkflow},
			"LEGION-50": {Tree: "LEGION-50", Authority: treelifecycle.AuthorityWorkflow},
		},
		Gates: map[string]record.DesignGate{
			"LEGION-10": {Issue: "LEGION-10", LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: opened}, EverOpened: true},
			"LEGION-50": {Issue: "LEGION-50", LatestVersion: 1, ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 8, At: opened}, EverOpened: true},
		},
		Owed: map[string]record.OwedStart{
			"LEGION-12": {Issue: "LEGION-12", ID: 42, CreatedAt: fixedNow.Add(-time.Hour)},
		},
	}

	var got []string
	var kinds []record.LineKind
	for _, entry := range record.Waiting(issues, nil, facts) {
		got = append(got, entry.Issue.Key)
		kinds = append(kinds, entry.Kind)
	}
	want := []string{"LEGION-10", "LEGION-12", "LEGION-20", "LEGION-11", "LEGION-30"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the line = %v, want %v: the approved root's own first phase and the returning child first, then the board's rank", got, want)
	}
	wantKinds := []record.LineKind{record.LineFirstPhase, record.LineReturning, record.LineQueuedRoot, record.LineFirstPhase, record.LineQueuedRoot}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("the line's kinds = %v, want %v", kinds, wantKinds)
	}

	// The gate opened before the child's start came back, so the root goes first; an approval
	// that lands after it puts the root behind the child, on the one time axis the two share.
	gate := facts.Gates["LEGION-10"]
	gate.ApprovedMark.At = fixedNow
	facts.Gates["LEGION-10"] = gate
	got = nil
	for _, entry := range record.Waiting(issues, nil, facts) {
		got = append(got, entry.Issue.Key)
	}
	if want := []string{"LEGION-12", "LEGION-10", "LEGION-20", "LEGION-11", "LEGION-30"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the line with the gate opened last = %v, want %v", got, want)
	}
}

// A4's blocker clause: an issue whose first phase an unfinished Dispatch issue holds where it
// stands is in no line, however long it has waited, and the line the caller reads says so only
// because the caller judged the mirror for it (record.LineFacts.Held, blockers.HeldIssues).
func TestTheWaitingLineLeavesOutAnIssueABlockerHolds(t *testing.T) {
	issues := []record.Issue{
		lineIssue("LEGION-10", "LEGION-10", "in_progress", phase.Admitted, "M"),
		lineIssue("LEGION-11", "LEGION-10", "todo", phase.Admitted, "N"),
	}
	approved := 1
	facts := record.LineFacts{
		Lifecycles: map[string]treelifecycle.Lifecycle{"LEGION-10": {Tree: "LEGION-10", Authority: treelifecycle.AuthorityWorkflow}},
		Gates: map[string]record.DesignGate{
			"LEGION-10": {Issue: "LEGION-10", LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true},
		},
		Held: map[string]struct{}{"LEGION-11": {}},
	}
	var got []string
	for _, entry := range record.Waiting(issues, nil, facts) {
		got = append(got, entry.Issue.Key)
	}
	if want := []string{"LEGION-10"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the line = %v, want %v: the held child waits on its blocker, not on a slot", got, want)
	}
}
