package record

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A project keeps the record of every issue it ever finished, so what a fact reads must be the
// records it can act on: every record of the project not in phase done, a done root a person set
// back to todo (re-admission reads it from the line), and a done record still holding a slot
// (the next release of slots frees it, and the cap counts it until then). A finished record, a
// parked child and another project's record are read only by key.
func TestLiveIssuesReadsTheRecordsOfTheProjectThatAreNotFinished(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	issue := func(key, project string, at phase.Phase, status string) Issue {
		recorded := issueFixture(key)
		recorded.Project, recorded.Phase, recorded.Status, recorded.Parent, recorded.ReadyPendingVersion = project, at, status, nil, nil
		return recorded
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, recorded := range []Issue{
			issue("LEGION-1", "LEGION", phase.Implementing, "in_progress"),
			issue("LEGION-2", "LEGION", phase.Done, "done"),
			issue("LEGION-3", "LEGION", phase.Done, "todo"),
			issue("LEGION-4", "LEGION", phase.Done, "done"),
			issue("LEGION-5", "LEGION", phase.Done, "backlog"),
			issue("LEGION-6", "LEGION", phase.Admitted, "todo"),
			issue("OTHER-1", "OTHER", phase.Implementing, "in_progress"),
		} {
			must(t, records.PutIssue(t.Context(), tx, recorded))
		}
		must(t, records.PutSlot(t.Context(), tx, Slot{Issue: "LEGION-4", Index: 0, AdmittedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)}))

		live, err := records.LiveIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("read the live issues: %v", err)
		}
		if got, want := keysOf(live), []string{"LEGION-1", "LEGION-3", "LEGION-4", "LEGION-6"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("live issues = %v, want %v: unfinished, set back to todo or holding a slot, by key", got, want)
		}

		named, err := records.IssuesOf(t.Context(), tx, "LEGION", []string{"LEGION-5", "OTHER-1", "LEGION-2", "LEGION-9"})
		if err != nil {
			t.Fatalf("read issues by key: %v", err)
		}
		if got, want := keysOf(named), []string{"LEGION-2", "LEGION-5"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("issues by key = %v, want %v: the project's recorded keys, by key", got, want)
		}
		if none, err := records.IssuesOf(t.Context(), tx, "LEGION", nil); err != nil || len(none) != 0 {
			t.Fatalf("issues of no key = %v, %v; want none", none, err)
		}
	})
}

// A judgement of an issue's blockers reads that issue's mirrored row and the rows of what it waits
// on, never the whole project's mirror, which keeps a row of every issue the project ever had. A
// blocker's own blockers are not read, nor is another project's row.
func TestDispatchIssuesOfReadsTheKeysAndWhatTheyWaitOn(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		for _, row := range []DispatchIssueRow{
			{Key: "LEGION-10", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-12", "LEGION-11", "OTHER-11"}, Seq: 3},
			{Key: "LEGION-11", Project: "LEGION", Status: "done", Seq: 4},
			{Key: "LEGION-12", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-13"}, Seq: 5},
			{Key: "LEGION-13", Project: "LEGION", Status: "todo", Seq: 6},
			{Key: "LEGION-14", Project: "LEGION", Status: "todo", Seq: 7},
			{Key: "OTHER-11", Project: "OTHER", Status: "done", Seq: 8},
		} {
			must(t, records.PutDispatchIssue(t.Context(), tx, row))
		}
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-10", "session:s-1", 9))

		rows, err := records.DispatchIssuesOf(t.Context(), tx, "LEGION", []string{"LEGION-10"})
		if err != nil {
			t.Fatalf("read the mirror of LEGION-10: %v", err)
		}
		want := []DispatchIssueRow{
			{Key: "LEGION-10", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-12", "LEGION-11", "OTHER-11"}, Seq: 3, ClaimedBy: "session:s-1", ClaimSeq: 9},
			{Key: "LEGION-11", Project: "LEGION", Status: "done", Blockers: []string{}, Seq: 4},
			{Key: "LEGION-12", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-13"}, Seq: 5},
		}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("the mirror of LEGION-10 = %#v, want %#v: its row and its blockers' in the project, by key", rows, want)
		}
		if none, err := records.DispatchIssuesOf(t.Context(), tx, "LEGION", nil); err != nil || len(none) != 0 {
			t.Fatalf("the mirror of no key = %#v, %v; want none", none, err)
		}
	})
}

func keysOf(issues []Issue) []string {
	keys := make([]string, len(issues))
	for i, issue := range issues {
		keys[i] = issue.Key
	}
	return keys
}
