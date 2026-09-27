package docs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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

// A settlement's anchor refresh owns two fields of an anchor, the quote and the orphan flag. It
// reads each open row's anchor and writes it back, so a write of the whole column erases whatever
// another writer put in that anchor after the refresh read it - here the block id the one-time
// BackfillAnchorBlocks repair pins. The advisory lock holds the settlement between the ask's
// refresh event and the comment's update, which is the window a loaded CI runner hit on its own.
func TestAnchorRefreshKeepsABlockIDPinnedAfterItReadTheRow(t *testing.T) {
	const holdKey = int64(4815162342)
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Introduction.\n\nTarget passage.\n\nOther passage.\n")
	askID := insertAnchoredAsk(t, service, artifactID, "Other passage")
	commentID := insertAnchoredComment(t, service, artifactID, "Target passage")

	// Both rows are stale: their marks are live but their anchors say orphaned, so the refresh
	// has a reason to write each one. The ask's block is already pinned, which is what keeps the
	// backfill off the row the settlement locks first.
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks
		set anchor = jsonb_set(jsonb_set(anchor, '{orphaned}', to_jsonb(true)), '{block_id}', to_jsonb('already-pinned'::text), true)
		where id = $1
	`, askID); err != nil {
		t.Fatalf("stale the ask anchor: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		update comments set anchor = jsonb_set(anchor, '{orphaned}', to_jsonb(true)) where id = $1
	`, commentID); err != nil {
		t.Fatalf("stale the comment anchor: %v", err)
	}
	wantBlockID, err := service.BlockForQuote(context.Background(), artifactID, "Target passage")
	if err != nil {
		t.Fatalf("resolve target block: %v", err)
	}

	if _, err := service.store.Pool.Exec(context.Background(), `
		create function dispatch_test_hold_anchor_refresh() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(4815162342);
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create anchor refresh hold function: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create trigger dispatch_test_hold_anchor_refresh
		before insert on events for each row
		when (new.type = 'ask.anchor_refreshed')
		execute function dispatch_test_hold_anchor_refresh()
	`); err != nil {
		t.Fatalf("create anchor refresh hold trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = service.store.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_hold_anchor_refresh on events`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop function if exists dispatch_test_hold_anchor_refresh()`)
	})

	// A content change is what makes the settlement write a version, and a version write is what
	// refreshes the anchors.
	editLiveTree(t, service, artifactID, replaceRun("Introduction.", "Intro."))
	settleCtx, cancelSettle := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSettle()
	if err := service.waitForPendingUpdates(settleCtx, artifactID); err != nil {
		t.Fatalf("wait for live updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(settleCtx, artifactID); err != nil {
		t.Fatalf("wait for live updates to become durable: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()

	hold, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin anchor refresh hold: %v", err)
	}
	defer hold.Rollback(context.Background())
	if _, err := hold.Exec(context.Background(), `select pg_advisory_xact_lock($1)`, holdKey); err != nil {
		t.Fatalf("hold the anchor refresh: %v", err)
	}

	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	waitForHeldAnchorRefresh(t, service, holdKey)

	backfill, err := service.BackfillAnchorBlocks(context.Background())
	if err != nil {
		t.Fatalf("backfill anchor blocks beside the settlement: %v", err)
	}
	if backfill.Asks != 0 || backfill.Comments != 1 {
		t.Fatalf("backfill beside the settlement = %#v, want the comment alone", backfill)
	}
	if err := hold.Rollback(context.Background()); err != nil {
		t.Fatalf("release the anchor refresh: %v", err)
	}
	<-settled

	var encoded []byte
	if err := service.store.Pool.QueryRow(context.Background(), `select anchor from comments where id = $1`, commentID).Scan(&encoded); err != nil {
		t.Fatalf("load comment anchor: %v", err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode comment anchor: %v", err)
	}
	if anchor.BlockID == nil || *anchor.BlockID != wantBlockID {
		t.Fatalf("comment block_id after the settlement = %v, want %q", anchor.BlockID, wantBlockID)
	}
	if anchor.Orphaned {
		t.Fatalf("comment anchor stayed orphaned, so the settlement never refreshed it")
	}
}

// waitForHeldAnchorRefresh returns once the settlement is inside its anchor refresh, waiting on
// the advisory lock the test holds. The lock's own wait queue is the settlement's observable
// position; nothing here assumes how long a step takes.
func waitForHeldAnchorRefresh(t *testing.T, service *Service, key int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from pg_locks
			where locktype = 'advisory' and not granted
				and ((classid::bigint << 32) | objid::bigint) = $1
		`, key).Scan(&waiting); err != nil {
			t.Fatalf("read the anchor refresh hold: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the settlement never reached its anchor refresh")
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
