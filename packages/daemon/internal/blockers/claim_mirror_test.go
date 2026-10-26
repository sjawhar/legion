package blockers

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// claimOf is the claim the mirror holds for key: its holder and the sequence it was seen at.
func claimOf(t *testing.T, rows []record.DispatchIssueRow, key string) (string, int64) {
	t.Helper()
	for _, row := range rows {
		if row.Key == key {
			return row.ClaimedBy, row.ClaimSeq
		}
	}
	t.Fatalf("the mirror holds no row of %s: %#v", key, rows)
	return "", 0
}

// An issue event that shows the claim writes it at the event's sequence; one from a Dispatch that
// did not send it leaves the claim the row holds.
func TestMirrorWritesTheClaimAnIssueEventShows(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	apply := func(fact intake.Fact) {
		t.Helper()
		inTx(t, pool, func(tx pgx.Tx) {
			if _, err := mirror.Apply(context.Background(), tx, fact); err != nil {
				t.Fatalf("apply %#v: %v", fact, err)
			}
		})
	}
	apply(intake.DispatchIssue{Key: "LEGION-900", Seq: 4, Status: "todo", ClaimedBy: "user:alice", ClaimKnown: true})
	if holder, seq := claimOf(t, mirrored(t, pool, records), "LEGION-900"); holder != "user:alice" || seq != 4 {
		t.Fatalf("claim after an issue event showing it = %q at %d, want user:alice at 4", holder, seq)
	}
	apply(intake.DispatchIssue{Key: "LEGION-900", Seq: 5, Status: "in_progress"})
	if holder, seq := claimOf(t, mirrored(t, pool, records), "LEGION-900"); holder != "user:alice" || seq != 4 {
		t.Fatalf("claim after an issue event that does not show it = %q at %d, want user:alice at 4 kept", holder, seq)
	}
	apply(intake.DispatchIssue{Key: "LEGION-900", Seq: 6, Status: "in_progress", ClaimKnown: true})
	if holder, seq := claimOf(t, mirrored(t, pool, records), "LEGION-900"); holder != "" || seq != 6 {
		t.Fatalf("claim after an issue event showing none = %q at %d, want nobody at 6", holder, seq)
	}
}

// Dispatch's issue.claimed and issue.released move the claim of a mirrored issue and nothing else
// of its row, and a claim event older than the claim held changes nothing. One for an issue the
// mirror has no row of makes no row, but is kept: the issue's own older event, delivered after it,
// shows that claim rather than the older one it carries.
func TestMirrorWritesTheClaimOfAClaimEvent(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	apply := func(fact intake.Fact) {
		t.Helper()
		inTx(t, pool, func(tx pgx.Tx) {
			result, err := mirror.Apply(context.Background(), tx, fact)
			if err != nil || result.Refusal != nil {
				t.Fatalf("apply %#v: %v %#v; the mirror refuses nothing", fact, err, result.Refusal)
			}
		})
	}
	apply(intake.DispatchIssue{Key: "LEGION-900", Seq: 4, Status: "todo", Blockers: []string{"LEGION-901"}})
	apply(intake.DispatchClaim{Key: "LEGION-900", Seq: 7, ClaimedBy: "user:alice"})
	apply(intake.DispatchClaim{Key: "LEGION-900", Seq: 6})
	apply(intake.DispatchClaim{Key: "LEGION-902", Seq: 3, ClaimedBy: "user:alice"})
	rows := mirrored(t, pool, records)
	if len(rows) != 1 || rows[0].Status != "todo" || rows[0].Seq != 4 || len(rows[0].Blockers) != 1 {
		t.Fatalf("the mirror = %#v, want LEGION-900 alone with its status and blockers as the issue event left them", rows)
	}
	if holder, seq := claimOf(t, rows, "LEGION-900"); holder != "user:alice" || seq != 7 {
		t.Fatalf("claim = %q at %d, want user:alice at 7", holder, seq)
	}
	apply(intake.DispatchClaim{Key: "LEGION-900", Seq: 8})
	if holder, seq := claimOf(t, mirrored(t, pool, records), "LEGION-900"); holder != "" || seq != 8 {
		t.Fatalf("claim after the release = %q at %d, want nobody at 8", holder, seq)
	}
	apply(intake.DispatchIssue{Key: "LEGION-902", Seq: 2, Status: "todo", ClaimKnown: true})
	if holder, seq := claimOf(t, mirrored(t, pool, records), "LEGION-902"); holder != "user:alice" || seq != 3 {
		t.Fatalf("claim of LEGION-902 after its older first event = %q at %d, want user:alice at 3", holder, seq)
	}
}

// The boot listing and the periodic turn's re-read each show the claim, and write it behind its
// fence: a view older than a claim event already applied leaves that claim.
func TestTheBootListingAndTheTickReReadWriteTheClaim(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := Reconcile(context.Background(), tx, records, testProject, []dispatch.IssueSummary{
			{Key: "LEGION-900", Status: "todo", LastSeq: 4, ClaimedBy: "user:alice"},
			{Key: "LEGION-901", Status: "todo", LastSeq: 5},
		}); err != nil {
			t.Fatalf("reconcile the listing: %v", err)
		}
	})
	rows := mirrored(t, pool, records)
	if holder, seq := claimOf(t, rows, "LEGION-900"); holder != "user:alice" || seq != 4 {
		t.Fatalf("listed claim = %q at %d, want user:alice at 4", holder, seq)
	}
	if holder, seq := claimOf(t, rows, "LEGION-901"); holder != "" || seq != 5 {
		t.Fatalf("listed issue with no claim = %q at %d, want nobody at 5", holder, seq)
	}
	mirror := NewMirror(records, testProject)
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := mirror.Apply(context.Background(), tx, intake.DispatchClaim{Key: "LEGION-901", Seq: 9, ClaimedBy: "session:other"}); err != nil {
			t.Fatal(err)
		}
		if _, err := mirror.Apply(context.Background(), tx, intake.ControllerTick{Observed: []intake.IssueStatus{
			{Key: "LEGION-900", Status: "todo", Seq: 8, ClaimKnown: true},
			{Key: "LEGION-901", Status: "todo", Seq: 7, ClaimKnown: true},
		}}); err != nil {
			t.Fatalf("apply the tick: %v", err)
		}
	})
	rows = mirrored(t, pool, records)
	if holder, seq := claimOf(t, rows, "LEGION-900"); holder != "" || seq != 8 {
		t.Fatalf("claim after the re-read showed none = %q at %d, want nobody at 8", holder, seq)
	}
	if holder, seq := claimOf(t, rows, "LEGION-901"); holder != "session:other" || seq != 9 {
		t.Fatalf("claim after an older re-read = %q at %d, want session:other at 9 kept", holder, seq)
	}
}
