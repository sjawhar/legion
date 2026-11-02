package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A status row's kind used to be read from three marks in its payload, and a row carrying none of
// them read as the ordinary kind: the defect where a kind nobody gave a rule was judged by the
// ordinary rule in silence. The kind is a stored field now and the payload no longer names the
// marks, so a dispatch_status row the release before this one queued would fail both the new
// validation, which refuses a row with no kind, and its decode, which refuses fields the type no
// longer has — and a status row that cannot run holds every later write of its issue behind it.
// The migration gives each pending row the kind the payload names and takes the three marks away
// in the same statement, so none is left with the old shape or an empty kind.
//
// Two of the shapes that release queued carry no mark at all, and the rows seeded here are those
// it really wrote: its re-assert of its own status over one a session wrote, told by the done,
// backlog or icebox it observes, and its own close, told by the done it writes. Read as ordinary,
// each would be dropped by the ordinary rule at its first retry — the re-assert for being at the
// status it exists to undo, the close for having left the workflow it is closing.
//
// Each migrated row is decoded here the way the outbox decodes it (record.DecodeOutboxPayload,
// which both validates the kind and refuses an unknown field), so the row this test reads is one
// the daemon can run. That it then runs under the rule of its own kind is held by the daemon's
// own TestEveryStatusRowKindHasAStillWantedRule and
// TestAMigratedStatusRowRunsUnderItsOwnRule, which this package cannot call: the daemon imports
// the store, so a test of this package importing the daemon would be an import cycle.
func TestEveryQueuedStatusRowCarriesItsKindAfter20261102003600(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 20261029121200)
	rows := []struct {
		name, issue, payload string
		want                 record.StatusRowKind
	}{
		{name: "a person's handover", issue: "LEGION-208", want: record.PersonHandoverStatusRow,
			payload: `{"status":"todo","observedStatus":"triage","personHandover":true}`},
		{name: "a re-assert", issue: "LEGION-209", want: record.ReassertStatusRow,
			payload: `{"status":"in_progress","observedStatus":"done","reassert":true}`},
		{name: "a hand-over", issue: "LEGION-210", want: record.HandOverStatusRow,
			payload: `{"status":"todo","observedStatus":"in_progress","handOver":true,"handOverTree":"LEGION-200","handOverLinger":3}`},
		{name: "an unmarked write", issue: "LEGION-211", want: record.OrdinaryStatusRow,
			payload: `{"status":"testing","observedStatus":"in_progress"}`},
		{name: "an unmarked re-assert, as the release before this one queued one", issue: "LEGION-212", want: record.ReassertStatusRow,
			payload: `{"status":"retro","observedStatus":"done"}`},
		{name: "an unmarked park a session wrote over, observed at backlog", issue: "LEGION-213", want: record.ReassertStatusRow,
			payload: `{"status":"in_progress","observedStatus":"backlog"}`},
		{name: "a person's handover of a parked root, which the re-assert arm would also match", issue: "LEGION-215",
			want:    record.PersonHandoverStatusRow,
			payload: `{"status":"todo","observedStatus":"backlog","personHandover":true}`},
		{name: "an unmarked close, as a merge queued one", issue: "LEGION-214", want: record.CloseStatusRow,
			payload: `{"status":"done","observedStatus":"in_progress","reason":"merged"}`},
		// A write of done observed at done matches the re-assert arm and the close arm alike. The
		// re-assert arm is first and wins, which is right for the row main queued in that shape:
		// its sessionStatusWrite re-asserting the daemon's own done over a session's.
		{name: "a write that matches the re-assert arm and the close arm", issue: "LEGION-216",
			want:    record.ReassertStatusRow,
			payload: `{"status":"done","observedStatus":"done"}`},
	}
	for _, row := range rows {
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('dispatch_status', $1, $2, 0, now(), '')`, row.issue, row.payload); err != nil {
			t.Fatalf("queue %s: %v", row.name, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, row := range rows {
		var payload []byte
		if err := store.Pool().QueryRow(ctx, "select payload from outbox where issue = $1", row.issue).Scan(&payload); err != nil {
			t.Fatalf("read %s after the migration: %v", row.name, err)
		}
		decoded, err := record.DecodeOutboxPayload(record.OutboxRow{Kind: record.OutboxKindDispatchStatus, Issue: row.issue, Payload: payload})
		if err != nil {
			t.Fatalf("%s does not decode after the migration (%s): %v", row.name, payload, err)
		}
		write, ok := decoded.(record.StatusWrite)
		if !ok {
			t.Fatalf("%s decoded as %T, want a status write", row.name, decoded)
		}
		if write.Kind != row.want {
			t.Fatalf("%s carries kind %q after the migration, want %q", row.name, write.Kind, row.want)
		}
	}
	// The hand-over's fence is the one thing beside the kind that decides whether its row may
	// write, so the migration must leave it where it was.
	var tree string
	var linger int64
	if err := store.Pool().QueryRow(ctx,
		"select payload->>'handOverTree', (payload->>'handOverLinger')::bigint from outbox where issue = 'LEGION-210'").Scan(&tree, &linger); err != nil {
		t.Fatalf("read the migrated hand-over's fence: %v", err)
	}
	if tree != "LEGION-200" || linger != 3 {
		t.Fatalf("the migrated hand-over is fenced on %s at linger %d, want LEGION-200 at 3", tree, linger)
	}
	var old int
	if err := store.Pool().QueryRow(ctx, `select count(*) from outbox where kind = 'dispatch_status'
		and (payload->>'personHandover' is not null or payload->>'reassert' is not null or payload->>'handOver' is not null)`).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old != 0 {
		t.Fatalf("%d status rows still carry one of the three old marks, want none: a payload with one fails its decode", old)
	}
}
