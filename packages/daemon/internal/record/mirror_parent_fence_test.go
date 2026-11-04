package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The parent the mirror holds decides which issues hold a parent's own first phase, so the two
// observations that are not simply newer have to land the way the fence means them to: a listing
// at the sequence the row already holds is the only way the parent of an issue the daemon has
// never recorded reaches the mirror, and must still be taken, while an observation strictly
// older than the row is a state Dispatch has already moved past and must change nothing.
func TestTheMirrorsParentFollowsTheFence(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	parentOf := func(t *testing.T, tx pgx.Tx, key string) string {
		t.Helper()
		var parent *string
		if err := tx.QueryRow(ctx, `select parent from dispatch_issues where key = $1`, key).Scan(&parent); err != nil {
			t.Fatalf("read the mirror of %s: %v", key, err)
		}
		if parent == nil {
			return ""
		}
		return *parent
	}
	inTx(t, st, func(tx pgx.Tx) {
		// A listing at the sequence the row already holds: the row has no parent yet, and this
		// is how an issue the daemon never recorded gets one.
		must(t, records.PutDispatchIssue(ctx, tx, DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "todo", Seq: 5}))
		must(t, records.PutDispatchIssue(ctx, tx, DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "todo", Parent: "LEGION-800", Seq: 5}))
		if got := parentOf(t, tx, "LEGION-900"); got != "LEGION-800" {
			t.Fatalf("the mirror's parent after a listing at the row's own sequence = %q, want LEGION-800", got)
		}

		// An observation strictly older than the row changes nothing, parent included: the row's
		// own observation saw Dispatch later.
		must(t, records.PutDispatchIssue(ctx, tx, DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "backlog", Parent: "LEGION-700", Seq: 4}))
		if got := parentOf(t, tx, "LEGION-900"); got != "LEGION-800" {
			t.Fatalf("the mirror's parent after an older observation = %q, want the row's own LEGION-800", got)
		}
		var status string
		if err := tx.QueryRow(ctx, `select status from dispatch_issues where key = $1`, "LEGION-900").Scan(&status); err != nil {
			t.Fatalf("read the mirror's status: %v", err)
		}
		if status != "todo" {
			t.Fatalf("the mirror's status after an older observation = %q, want the row's own todo", status)
		}

		// A newer observation that carries no parent takes the child out from under it.
		must(t, records.PutDispatchIssue(ctx, tx, DispatchIssueRow{Key: "LEGION-900", Project: "LEGION", Status: "todo", Seq: 6}))
		if got := parentOf(t, tx, "LEGION-900"); got != "" {
			t.Fatalf("the mirror's parent after Dispatch reported none = %q, want none", got)
		}
		children, err := records.DispatchChildren(ctx, tx, "LEGION", []string{"LEGION-800"})
		if err != nil {
			t.Fatalf("read the children of LEGION-800: %v", err)
		}
		if len(children["LEGION-800"]) != 0 {
			t.Fatalf("the children of LEGION-800 = %#v, want none", children)
		}
	})
}
