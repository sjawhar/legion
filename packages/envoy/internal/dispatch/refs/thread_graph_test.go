package refs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestParticipationUsesSharedGraphAndLeavesDefaultReadsUnchanged(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "CORE")
	createReferenceIssue(t, tx, "CORE-1", "CORE")
	comment := createReferenceComment(t, tx, "CORE-1")
	edges, err := Edges(ctx, tx, Query{Direction: "out", Kind: "person", ID: "alice", Kinds: []string{"participates_in"}})
	if err != nil || len(edges) != 1 || edges[0].Kind != "participates_in" || edges[0].Node.Kind != "comment" || edges[0].Node.ID != comment {
		t.Fatalf("participation=%+v err=%v", edges, err)
	}
	edges, err = Edges(ctx, tx, Query{Direction: "in", Kind: "comment", ID: comment})
	if err != nil || len(edges) != 0 {
		t.Fatalf("default old graph read changed: %+v %v", edges, err)
	}
	edges, err = Edges(ctx, tx, Query{Direction: "in", Kind: "comment", ID: comment, Kinds: []string{"participates_in"}})
	if err != nil || len(edges) != 1 || edges[0].Node.Kind != "person" || edges[0].Node.ID != "alice" {
		t.Fatalf("incoming participation=%+v err=%v", edges, err)
	}
}
