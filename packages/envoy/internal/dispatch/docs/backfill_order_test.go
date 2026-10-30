package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The block-id backfill is a durable writer of the room like every other, so the rule this change
// makes holds for it too: a browser update the room committed before the backfill reads it is
// stored BEFORE the backfill's own stamp, and the stamp's class is measured against that stored
// prefix. Until this round the backfill appended its stamp alone, with a class taken from the
// room's state, so a browser update the store had not taken yet was stored after the stamp and
// the versions folded from the stored history stopped giving the markdown they were written at.
//
// The browser's append here is held in the store, which is the state the stamp must not jump: the
// room holds the paragraph, the store does not, and the paragraph repeats the first block's id, so
// the document needs the repair that makes the backfill write at all.
func TestStoredOrderBackfillStampOverAnUnstoredEdit(t *testing.T) {
	service, artifactID, entered, release := heldAppendService(t)
	first, _ := liveTree(t, service, artifactID).Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	if first == "" {
		t.Fatal("the document's first block has no id to repeat")
	}
	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob,
		appendBlockWithCraftedID(t, "A paragraph repeating the first block's id.\n", first))
	awaitHeldAppend(t, entered)

	report := service.backfillBlockIDs(context.Background(), artifactID)
	if report.Err != nil {
		t.Fatalf("the backfill failed: %v", report.Err)
	}
	if report.Stamped == 0 {
		t.Fatal("the backfill stamped nothing, so it stored nothing and this test proves nothing")
	}
	release()
	report2 := assertStoredOrder(t, service, artifactID)
	if report2.UpdatesChecked == 0 {
		t.Fatal("the check compared no update, so a clean result means nothing")
	}
}
