package docs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestBackfillAnchorBlocksPinsResolvableLegacyRowsOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n")
	askID := insertAnchoredAsk(t, service, artifactID, "Target passage")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")

	first, err := service.BackfillAnchorBlocks(context.Background())
	if err != nil {
		t.Fatalf("backfill anchor blocks: %v", err)
	}
	if first.Asks != 1 || first.Comments != 1 || first.Skipped != 0 {
		t.Fatalf("first backfill = %#v, want both rows updated", first)
	}

	wantBlockID, err := service.BlockForQuote(context.Background(), artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve target block: %v", err)
	}
	for _, row := range []struct {
		id    string
		table string
	}{{askID, "asks"}, {commentID, "comments"}} {
		var encoded []byte
		if err := service.store.Pool.QueryRow(context.Background(), "select anchor from "+row.table+" where id = $1", row.id).Scan(&encoded); err != nil {
			t.Fatalf("load %s anchor: %v", row.table, err)
		}
		var anchor model.Anchor
		if err := json.Unmarshal(encoded, &anchor); err != nil {
			t.Fatalf("decode %s anchor: %v", row.table, err)
		}
		if anchor.BlockID == nil || *anchor.BlockID != wantBlockID {
			t.Fatalf("%s block_id = %v, want %q", row.table, anchor.BlockID, wantBlockID)
		}
	}

	second, err := service.BackfillAnchorBlocks(context.Background())
	if err != nil {
		t.Fatalf("repeat backfill anchor blocks: %v", err)
	}
	if second.Asks != 0 || second.Comments != 0 || second.Skipped != 0 {
		t.Fatalf("second backfill = %#v, want idempotent no-op", second)
	}
}

func TestBlockForQuoteLeavesTopLevelCrossBlockAnchorsUnpinned(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "One.\n\nTwo.\n")

	blockID, err := service.BlockForQuote(context.Background(), artifactID, "One. Two.")
	if err != nil {
		t.Fatalf("resolve cross-block quote: %v", err)
	}
	if blockID != "" {
		t.Fatalf("cross-block quote block_id = %q, want empty", blockID)
	}
}
