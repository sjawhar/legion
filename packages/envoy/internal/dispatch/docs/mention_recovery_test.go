package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

func TestRebuildAndOwedSweepRestoreDocumentIdentityMentionsAfterOldReindex(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	id := createOwedDocument(t, database, service, "", "identity-mentions", "Please read dispatch://people/alice", "")
	settle(t, database, id)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("first index=%+v", tick)
	}
	count := func() int {
		var n int
		if err := database.Pool.QueryRow(ctx, `select count(*) from graph_mention_edges g join document_blocks d on d.node_id=g.from_id where g.from_kind='block' and d.artifact_id=$1 and g.to_kind='person' and g.to_id='alice'`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(); got != 1 {
		t.Fatalf("indexed mentions=%d", got)
	}
	// The old parser finds no identity reference; its reconciliation removes those rows
	// while leaving an unchanged document's index version current.
	if _, err := database.Pool.Exec(ctx, `delete from block_refs where artifact_id=$1 and to_kind='person_mention'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `delete from refs where from_kind='artifact' and from_id=$1 and to_kind='person_mention'`, id); err != nil {
		t.Fatal(err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 0 || count() != 0 {
		t.Fatalf("unchanged document must remain absent until rebuild: %+v", tick)
	}
	if _, err := refs.RebuildAll(ctx, database.Pool, "https://dispatch.example"); err != nil {
		t.Fatal(err)
	}
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("rebuild owed sweep=%+v", tick)
	}
	if got := count(); got != 1 {
		t.Fatalf("restored mentions=%d", got)
	}
}
