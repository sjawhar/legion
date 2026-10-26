package record

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The mirror is written from events that can arrive out of order — a boot listing beside a live
// event, a redelivery after a newer one — so each write is fenced on Dispatch's own sequence: a
// fresher observation lands, an older one leaves the row exactly as it stands.
func TestPutDispatchIssueTakesOnlyAFresherObservation(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-901"}, Seq: 5}))

		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Blockers: nil, Seq: 7}))
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
		want := []DispatchIssueRow{{Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Blockers: []string{}, Seq: 7}}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("after the newer observation the mirror = %#v, want %#v", rows, want)
		}

		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "backlog", Blockers: []string{"LEGION-902"}, Seq: 4}))
		rows, err = records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("after the older observation the mirror = %#v, want the fresher row %#v", rows, want)
		}
	})
}

// A fact the daemon mints from its own record carries no Dispatch sequence. It is no observation
// of Dispatch's, and the fence cannot catch it on a key with no row yet — an insert has no
// conflict — so it writes nothing at all: neither a new row nor an update of one.
func TestPutDispatchIssueWritesNothingWithoutASequence(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-901"}}))
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("a zero-sequence write on an unmirrored key left %#v, want nothing", rows)
		}

		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Blockers: []string{"LEGION-901"}, Seq: 6}))
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "done", Blockers: nil}))
		rows, err = records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
		want := []DispatchIssueRow{{Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Blockers: []string{"LEGION-901"}, Seq: 6}}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("a zero-sequence write over a mirrored key left %#v, want it untouched as %#v", rows, want)
		}
	})
}

// The database is shared by every project's daemon, and each judges what its own issues wait on:
// the listing is one project's rows, by key.
func TestDispatchIssuesListsOneProject(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		for _, row := range []DispatchIssueRow{
			{Key: "LEGION-902", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-901"}, Seq: 2},
			{Key: "LEGION-901", Project: "LEGION", Status: "done", Seq: 1},
			{Key: "INFRA-7", Project: "INFRA", Status: "todo", Seq: 3},
		} {
			must(t, records.PutDispatchIssue(t.Context(), tx, row))
		}
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
		want := []DispatchIssueRow{
			{Key: "LEGION-901", Project: "LEGION", Status: "done", Blockers: []string{}, Seq: 1},
			{Key: "LEGION-902", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-901"}, Seq: 2},
		}
		if !reflect.DeepEqual(rows, want) {
			t.Fatalf("the mirror of LEGION = %#v, want %#v", rows, want)
		}
	})
}
