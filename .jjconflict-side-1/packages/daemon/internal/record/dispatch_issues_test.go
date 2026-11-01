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

// The claim on a mirror row carries a fence of its own. An observation that shows the claim
// writes it only over an older claim, one that does not show it leaves it, and the status fence
// and the claim fence each move only their own columns: a claim event newer than the row's status
// moves the claim alone, and an issue event older than that claim but newer than the status moves
// the status alone.
func TestTheMirrorsClaimHasAFenceOfItsOwn(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	read := func(tx pgx.Tx) DispatchIssueRow {
		t.Helper()
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil || len(rows) != 1 {
			t.Fatalf("the mirror = %#v, %v; want one row", rows, err)
		}
		return rows[0]
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "todo", Seq: 5, ClaimedBy: "user:alice", ClaimSeq: 5}))
		if got, want := read(tx), (DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "todo", Blockers: []string{}, Seq: 5, ClaimedBy: "user:alice", ClaimSeq: 5}); !reflect.DeepEqual(got, want) {
			t.Fatalf("first observation = %#v, want %#v", got, want)
		}

		// A claim event at 9: the claim moves, the status stays.
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-900", "session:architect", 9))
		// An issue event at 7 showing an older claim: the status moves, the claim stays.
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Seq: 7, ClaimedBy: "user:alice", ClaimSeq: 7}))
		if got, want := read(tx), (DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "in_progress", Blockers: []string{}, Seq: 7, ClaimedBy: "session:architect", ClaimSeq: 9}); !reflect.DeepEqual(got, want) {
			t.Fatalf("after a newer claim and an older issue event = %#v, want %#v", got, want)
		}

		// A release older than the claim held, and an issue event that does not show the claim.
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-900", "", 8))
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "testing", Seq: 10}))
		if got := read(tx); got.ClaimedBy != "session:architect" || got.ClaimSeq != 9 || got.Status != "testing" || got.Seq != 10 {
			t.Fatalf("after an older release and a view without the claim = %#v, want the claim kept and the status moved", got)
		}

		// A newer release clears it, and a newer view showing nobody keeps it cleared.
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-900", "", 11))
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "testing", Seq: 12, ClaimSeq: 12}))
		if got := read(tx); got.ClaimedBy != "" || got.ClaimSeq != 12 {
			t.Fatalf("after a release = %#v, want nobody at 12", got)
		}
	})
}

// A claim event for a key the mirror has no row of yet is kept, but makes no row: the row needs a
// status and the blockers, which only an issue's own observation carries, so the mirror lists
// nothing for the key until then. The issue's first observation, an older event delivered after the
// claim (one nak'd and redelivered), then shows the claim the newer event set, not its own. A claim
// with no sequence writes nothing.
func TestAClaimDeliveredBeforeItsIssuesFirstEventIsKept(t *testing.T) {
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-901", "session:s-1", 2))
		rows, err := records.DispatchIssues(t.Context(), tx, "LEGION")
		if err != nil || len(rows) != 0 {
			t.Fatalf("the mirror after a claim of an unmirrored key = %#v, %v; want no row", rows, err)
		}
		must(t, records.PutDispatchIssue(t.Context(), tx, DispatchIssueRow{Key: "LEGION-901", Project: "LEGION", Status: "todo", Seq: 1, ClaimSeq: 1}))
		rows, err = records.DispatchIssues(t.Context(), tx, "LEGION")
		if want := []DispatchIssueRow{{Key: "LEGION-901", Project: "LEGION", Status: "todo", Blockers: []string{}, Seq: 1, ClaimedBy: "session:s-1", ClaimSeq: 2}}; err != nil || !reflect.DeepEqual(rows, want) {
			t.Fatalf("the mirror after the issue's older first event = %#v, %v; want %#v", rows, err, want)
		}
		must(t, records.PutDispatchClaim(t.Context(), tx, "LEGION-901", "user:alice", 0))
		if rows, err = records.DispatchIssues(t.Context(), tx, "LEGION"); err != nil || rows[0].ClaimSeq != 2 || rows[0].ClaimedBy != "session:s-1" {
			t.Fatalf("the mirror after a claim with no sequence = %#v, %v; want the claim at 2 kept", rows, err)
		}
	})
}
