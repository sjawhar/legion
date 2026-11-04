package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The children-first wait is read from the mirror's parent column (Store.DispatchChildren), and
// it ends when the child is released and leaves the workflow, is closed, is moved to icebox, or
// is moved out from under the parent. A person can do the last one in two ways: move the child
// to another parent, or take the parent off it altogether. Dispatch reports both the same way,
// as the issue's parent now, so the mirror has to take "no parent" as the fact it is.
func TestTheMirrorTakesAChildOutFromUnderItsParent(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutDispatchIssue(ctx, tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "todo", Parent: "LEGION-800", Seq: 5}))
		children, err := records.DispatchChildren(ctx, tx, "LEGION", []string{"LEGION-800"})
		if err != nil {
			t.Fatalf("read the children of LEGION-800: %v", err)
		}
		if len(children["LEGION-800"]) != 1 {
			t.Fatalf("the children of LEGION-800 = %#v, want the one Dispatch filed under it", children)
		}

		// The person takes the parent off the child, and Dispatch reports it with no parent.
		must(t, records.PutDispatchIssue(ctx, tx, DispatchIssueRow{
			Key: "LEGION-900", Project: "LEGION", Status: "todo", Seq: 6}))
		children, err = records.DispatchChildren(ctx, tx, "LEGION", []string{"LEGION-800"})
		if err != nil {
			t.Fatalf("read the children of LEGION-800: %v", err)
		}
		if len(children["LEGION-800"]) != 0 {
			t.Fatalf("the children of LEGION-800 = %#v, want none: the child is under no parent now, so nothing of it holds LEGION-800's own first phase", children)
		}
	})
}
