package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

func TestBackfillReferenceExcerptsStoresTheCitingBlock(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Opening.\n\nThe source cites dispatch://CORE-1.\n")
	if _, err := namedVersion(t, service, artifactID, "index source", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write source version: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		update refs
		set excerpt_block_id = '', excerpt_text = '', excerpt_ready = false
		where from_kind = 'artifact' and from_id = $1
	`, artifactID); err != nil {
		t.Fatalf("clear stored excerpt: %v", err)
	}

	needed, err := refs.NeedsDocumentExcerptBackfill(context.Background(), service.store.Pool)
	if err != nil {
		t.Fatalf("check missing stored excerpt: %v", err)
	}
	if !needed {
		t.Fatal("missing stored excerpt did not require backfill")
	}

	reports, err := service.BackfillReferenceExcerpts(context.Background())
	if err != nil {
		t.Fatalf("backfill reference excerpts: %v", err)
	}
	if len(reports) != 1 || reports[0].ArtifactID != artifactID || reports[0].References != 1 || reports[0].Err != nil {
		t.Fatalf("backfill reports = %#v", reports)
	}

	var blockID, excerpt string
	var ready bool
	if err := service.store.Pool.QueryRow(context.Background(), `
		select excerpt_block_id, excerpt_text, excerpt_ready
		from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'issue' and to_id = 'CORE-1'
	`, artifactID).Scan(&blockID, &excerpt, &ready); err != nil {
		t.Fatalf("read stored reference excerpt: %v", err)
	}
	if blockID == "" || excerpt != "The source cites dispatch://CORE-1." || !ready {
		t.Fatalf("stored reference excerpt = block %q text %q ready %t", blockID, excerpt, ready)
	}

	needed, err = refs.NeedsDocumentExcerptBackfill(context.Background(), service.store.Pool)
	if err != nil {
		t.Fatalf("check completed stored excerpt: %v", err)
	}
	if needed {
		t.Fatal("stored excerpt backfill still required")
	}
}
