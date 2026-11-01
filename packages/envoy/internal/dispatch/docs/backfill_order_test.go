package docs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence"

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

// The backfill takes the document's advisory lock before it reads the room's queue, so it never
// stores an update the room's own worker has already committed. The window is the worker's, in
// AppendRoomFlush: it has committed bob's edit and holds the lock, and has not marked that
// update stored yet. A writer that reads the queue without the lock sees the edit unstored and
// queues it to store, and only then blocks on the lock at its first append; by the time it takes
// the lock the worker has marked the edit stored and advanced the instance's cursor to its
// version, so the gate passes and the edit is appended a second time, under the stamp. The
// backfill's service serves this browser, as any service may: nothing keeps a browser off the
// rooms it stamps.
func TestStoredOrderBackfillDoesNotStoreTheRoomsCommittedEditTwice(t *testing.T) {
	service, artifactID := storedOrderService(t)
	appends, ok := service.persistence.(*PgVersioned)
	if !ok {
		t.Fatalf("the service appends through %T, which holds no window between a commit and its mark", service.persistence)
	}
	// The worker is held between its commit and the mark that makes the edit stored, with the
	// document's advisory lock in its hand. Released before the service's own cleanup, which
	// cannot shut down while an append is held here.
	committed, release := make(chan persistence.Version, 1), make(chan struct{})
	var opened, closed sync.Once
	releaseWorker := func() { closed.Do(func() { close(release) }) }
	t.Cleanup(releaseWorker)
	appends.afterAppendCommit = func(room string, version persistence.Version) {
		held := false
		if room == artifactID {
			opened.Do(func() { held = true })
		}
		if !held {
			return
		}
		committed <- version
		<-release
	}

	// Bob's paragraph repeats the first block's id, which is what gives the backfill something
	// to stamp at all.
	first, _ := liveTree(t, service, artifactID).Children[0].Attrs[pmdoc.BlockIDAttr].(string)
	if first == "" {
		t.Fatal("the document's first block has no id to repeat")
	}
	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob,
		appendBlockWithCraftedID(t, "A paragraph repeating the first block's id.\n", first))
	var edit persistence.Version
	select {
	case edit = <-committed:
	case <-time.After(roomWait(t)):
		t.Fatal("bob's edit never committed, so the window it is held in never opened")
	}

	var report BlockIDBackfill
	backfilled := make(chan error, 1)
	go func() {
		report = service.backfillBlockIDs(context.Background(), artifactID)
		backfilled <- report.Err
	}()
	// The backfill waits on the document's advisory lock the worker holds, which it takes as
	// soon as its transaction begins - before it reads the room's queue, which is the whole
	// point. Taking it at the first append instead is what stored the edit twice.
	waitForLockWait(t, context.Background(), service.store, "%pg_advisory_xact_lock%", backfilled)
	releaseWorker()
	if err := <-backfilled; err != nil {
		t.Fatalf("the backfill failed: %v", err)
	}
	if report.Stamped == 0 {
		t.Fatal("the backfill stamped nothing, so it stored nothing and this test proves nothing")
	}

	var update []byte
	if err := service.store.Pool.QueryRow(context.Background(),
		`select update from doc_updates where artifact_id = $1 and version = $2`, artifactID, int64(edit)).Scan(&update); err != nil {
		t.Fatalf("read the update the room's worker stored: %v", err)
	}
	var rows int
	if err := service.store.Pool.QueryRow(context.Background(),
		`select count(*) from doc_updates where artifact_id = $1 and update = $2`, artifactID, update).Scan(&rows); err != nil {
		t.Fatalf("count the rows holding bob's edit: %v", err)
	}
	if rows != 1 {
		t.Errorf("bob's edit is stored in %d rows, want 1: the backfill read the room's queue before it held the document's lock", rows)
	}
	checked := assertStoredOrder(t, service, artifactID)
	if checked.UpdatesChecked == 0 {
		t.Fatal("the check compared no update, so a clean result means nothing")
	}
}
