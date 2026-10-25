package blockers

import (
	"reflect"
	"testing"

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
