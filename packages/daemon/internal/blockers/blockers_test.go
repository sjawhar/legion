package blockers

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Unfinished is the whole judgement the mirror exists for, and it is read inside a transaction
// that can ask Dispatch nothing: what it does not know, it treats as still blocking.
func TestUnfinishedHoldsOnEveryBlockerThatIsNotDone(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows map[string]Row
		key  string
		want []string
	}{
		{
			name: "done is the Dispatch status done, exactly",
			rows: map[string]Row{
				"LEGION-900": {Status: "todo", Blockers: []string{"LEGION-901", "LEGION-902", "LEGION-903", "LEGION-904"}},
				"LEGION-901": {Status: "done"},
				"LEGION-902": {Status: "backlog"},
				"LEGION-903": {Status: "icebox"},
				"LEGION-904": {Status: "in_progress"},
			},
			key:  "LEGION-900",
			want: []string{"LEGION-902", "LEGION-903", "LEGION-904"},
		},
		{
			name: "a blocker with no row of its own is not done",
			rows: map[string]Row{
				"LEGION-900": {Status: "todo", Blockers: []string{"LEGION-901", "LEGION-902"}},
				"LEGION-902": {Status: "done"},
			},
			key:  "LEGION-900",
			want: []string{"LEGION-901"},
		},
		{
			name: "a key with no row of its own waits on nothing",
			rows: map[string]Row{"LEGION-901": {Status: "todo"}},
			key:  "LEGION-900",
			want: []string{},
		},
		{
			name: "nothing unfinished is an empty answer",
			rows: map[string]Row{
				"LEGION-900": {Status: "todo", Blockers: []string{"LEGION-901", "LEGION-902"}},
				"LEGION-901": {Status: "done"},
				"LEGION-902": {Status: "done"},
			},
			key:  "LEGION-900",
			want: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Unfinished(tc.rows, tc.key)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Unfinished(%s) = %#v, want %#v in the row's own order", tc.key, got, tc.want)
			}
		})
	}
}

// UnfinishedOf judges a list its caller holds, so an issue the mirror has no row of yet is judged
// all the same, and by the same rule as a mirrored one: a blocker with no row is not done.
func TestUnfinishedOfJudgesAListWhoseIssueHasNoRow(t *testing.T) {
	rows := map[string]Row{"LEGION-901": {Status: "done"}, "LEGION-902": {Status: "in_progress"}}
	if got, want := UnfinishedOf(rows, []string{"LEGION-901", "LEGION-902", "LEGION-903"}), []string{"LEGION-902", "LEGION-903"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UnfinishedOf = %#v, want %#v in the list's own order", got, want)
	}
	if got := UnfinishedOf(rows, nil); got == nil || len(got) != 0 {
		t.Fatalf("UnfinishedOf of no blockers = %#v, want an empty list", got)
	}
}

// Every caller reads its project's mirror in one query and judges in memory, so the rows arrive
// as a slice and are indexed by key once.
func TestRowsIndexesTheProjectByKey(t *testing.T) {
	rows := Rows([]record.DispatchIssueRow{
		{Key: "LEGION-900", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-901"}, Seq: 2},
		{Key: "LEGION-901", Project: "LEGION", Status: "done", Blockers: []string{}, Seq: 1},
	})
	want := map[string]Row{
		"LEGION-900": {Status: "todo", Blockers: []string{"LEGION-901"}},
		"LEGION-901": {Status: "done", Blockers: []string{}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("Rows = %#v, want %#v", rows, want)
	}
	if got := Unfinished(rows, "LEGION-900"); len(got) != 0 {
		t.Fatalf("Unfinished of an issue whose only blocker is done = %#v, want none", got)
	}
}

// Blocked is the one list the state shows and the tick notice carries: a waiting root promotion
// would skip and a child the workflow holds at `admitted`, together and sorted by issue key, each
// with what it still waits on. A root carrying a run is never judged, a child of a tree that is
// past `admitted` has already started, and an issue whose blockers are all done is in neither
// half.
func TestBlockedListsTheWaitingRootsAndTheHeldChildrenByKey(t *testing.T) {
	rows := map[string]Row{
		"LEGION-211": {Status: "todo", Blockers: []string{"LEGION-300"}},
		"LEGION-212": {Status: "todo", Blockers: []string{"LEGION-301"}},
		"LEGION-213": {Status: "todo", Blockers: []string{"LEGION-302"}},
		"LEGION-209": {Status: "todo", Blockers: []string{"LEGION-300", "LEGION-303"}},
		"LEGION-214": {Status: "todo", Blockers: []string{"LEGION-303"}},
		"LEGION-300": {Status: "in_progress"},
		"LEGION-301": {Status: "in_progress"},
		"LEGION-303": {Status: "done"},
	}
	issues := []record.Issue{
		// The root of a live tree, admitted and slotted, with a child held behind an unfinished
		// issue and one whose blockers are all done.
		{Key: "LEGION-208", Tree: "LEGION-208", Phase: phase.Implementing},
		{Key: "LEGION-209", Tree: "LEGION-208", Phase: phase.Admitted},
		{Key: "LEGION-214", Tree: "LEGION-208", Phase: phase.Admitted},
		// A child already past `admitted`: its own start read the links once and is done with them.
		{Key: "LEGION-215", Tree: "LEGION-208", Phase: phase.Planning},
		// Three roots in the waiting line: one blocked, one carrying a run into a tree of its own,
		// and one whose blocker Dispatch has not recorded at all.
		{Key: "LEGION-211", Tree: "LEGION-211", Phase: phase.Admitted},
		{Key: "LEGION-212", Tree: "LEGION-212", Phase: phase.Testing},
		{Key: "LEGION-213", Tree: "LEGION-213", Phase: phase.Admitted},
	}
	waiting := []record.Issue{issues[4], issues[5], issues[6]}

	want := []record.BlockedIssue{
		{Issue: "LEGION-209", BlockedBy: []string{"LEGION-300"}},
		{Issue: "LEGION-211", BlockedBy: []string{"LEGION-300"}},
		{Issue: "LEGION-213", BlockedBy: []string{"LEGION-302"}},
	}
	if got := Blocked(rows, issues, waiting); !reflect.DeepEqual(got, want) {
		t.Fatalf("Blocked = %#v, want %#v by issue key", got, want)
	}
}

// Nothing blocked is an empty list, never nil: the state serves it as an array and the notice
// leaves it out.
func TestBlockedIsEmptyWhenNothingWaitsOnALink(t *testing.T) {
	got := Blocked(map[string]Row{}, []record.Issue{{Key: "LEGION-209", Tree: "LEGION-208", Phase: phase.Admitted}}, nil)
	if len(got) != 0 || got == nil {
		t.Fatalf("Blocked = %#v, want an empty list", got)
	}
}
