package refs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestPersonNodesResolveCanonicalAliasesWithoutChangingDefaultReferenceReads(t *testing.T) {
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
	if _, err := tx.Exec(ctx, `update comments set author = '{"kind":"user","id":" ALICE "}' where id = $1`, comment); err != nil {
		t.Fatal(err)
	}
	person, found, err := Node(ctx, tx, "person", "alice", false)
	if err != nil || !found || person.Kind != "person" || person.ID != "alice" || person.Ref != "" {
		t.Fatalf("person node = %+v found=%v err=%v", person, found, err)
	}
	if _, found, err := Node(ctx, tx, "person", "nobody", false); err != nil || found {
		t.Fatalf("unknown person found=%v err=%v", found, err)
	}
	links, err := Edges(ctx, tx, Query{Direction: "out", Kind: "comment", ID: comment})
	if err != nil || len(links) != 0 {
		t.Fatalf("default links = %+v err=%v", links, err)
	}
	links, err = Edges(ctx, tx, Query{Direction: "out", Kind: "comment", ID: comment, Kinds: []string{"authored_by"}})
	if err != nil || len(links) != 1 || links[0].Node.Kind != "person" || links[0].Node.ID != "alice" {
		t.Fatalf("explicit authorship = %+v err=%v", links, err)
	}
}
