package docs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// renderedState is the markdown an encoded document state renders.
func renderedState(t *testing.T, state []byte) string {
	t.Helper()
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatalf("decode the read state: %v", err)
	}
	markdown, err := renderDocument(doc)
	if err != nil {
		t.Fatalf("render the read state: %v", err)
	}
	return markdown
}

// replicaFor is the replica the room's update observer keeps for live.
func replicaFor(t *testing.T, service *Service, live *crdt.Doc) *renderedReplica {
	t.Helper()
	replica := service.replicaOf(live)
	if replica == nil {
		t.Fatal("the room lists no replica, or its replica is gone")
	}
	return replica
}

// Bob's update is in the room and its append is held before the store. A joined transaction reads
// the room at an exact point and flushes through it: Bob's update is stored in that transaction,
// with his credit, and his held append, once released, stores nothing. The update is stored once,
// and the stored history replays.
func TestAFlushStoresTheRoomsHeldUpdateOnceWithItsCredit(t *testing.T) {
	service, artifactID, entered, release := heldAppendService(t)
	before := storedUpdateCount(t, service.store, artifactID)
	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob, replaceRun("Second.", "Second, bob."))
	awaitHeldAppend(t, entered)
	live := service.srv.GetDoc(artifactID)
	var state []byte
	var point roomPoint
	if err := joinedWrite(service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		if err := lockDocumentRoom(ctx, tx, artifactID); err != nil {
			return err
		}
		var err error
		if state, point, _, err = service.readPoint(artifactID, live); err != nil {
			return err
		}
		ledger := ledgerFrom(ctx)
		if err := ledger.flushThrough(ctx, tx, artifactID, point); err != nil {
			return err
		}
		// A second flush through the same point stores nothing again.
		return ledger.flushThrough(ctx, tx, artifactID, point)
	}); err != nil {
		t.Fatalf("flush the room in a joined transaction: %v", err)
	}
	if point.seq == 0 || point.cursor != service.cursorOf(live) {
		t.Fatalf("read point = %+v, want a position in the resident room instance", point)
	}
	if markdown := renderedState(t, state); !strings.Contains(markdown, "Second, bob.") {
		t.Fatalf("the read state renders %q, want Bob's edit in it", markdown)
	}
	if stored := storedUpdateCount(t, service.store, artifactID); stored != before+1 {
		t.Fatalf("the flush left %d stored updates, want %d: Bob's update, once", stored, before+1)
	}
	// The commit took the flushed version into the instance Bob's update was made in.
	head, err := service.persistence.Head(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the stored head: %v", err)
	}
	if at := point.cursor.heldThrough(); at != int64(head) {
		t.Fatalf("the room's cursor stands at %d after the flush committed, want the head, %d", at, head)
	}
	if authors := pendingAuthorRows(t, service.store, artifactID); !slices.Contains(authors, storedOrderBob) {
		t.Fatalf("the document's pending authors after the flush = %v, want %v among them", authors, storedOrderBob)
	}
	release()
	waitForLandedAppends(t, service, artifactID)
	if stored := storedUpdateCount(t, service.store, artifactID); stored != before+1 {
		t.Fatalf("Bob's held append left %d stored updates, want %d: the flush stored it already", stored, before+1)
	}
	assertStoredOrder(t, service, artifactID)
}

// A replica that could not follow the room holds no state until the room's next commit records a
// fresh base (renderedReplica.recover). A read point taken then is still exact: with no commit
// since, it is the room as it stands, at the position the replica stopped at; with a commit whose
// observer has not run yet, it starts the replica from that commit's base and takes the commit in,
// classifying it, at the next position.
func TestAReadPointOfARecoveringReplicaIsExact(t *testing.T) {
	service, artifactID := storedOrderService(t)
	editAsPeer(t, service, artifactID, replaceRun("First.", "First, alice."))
	waitForLandedAppends(t, service, artifactID)
	live := service.srv.GetDoc(artifactID)
	replica := replicaFor(t, service, live)
	replica.lockForUpdate()
	replica.recover(artifactID, nil, func(crdt.LoggedUpdate, bool, func() map[string]struct{}) {})
	stopped := replica.seq
	replica.mu.Unlock()

	state, point, _, err := service.readPoint(artifactID, live)
	if err != nil {
		t.Fatalf("read the recovering room: %v", err)
	}
	if point.seq != stopped {
		t.Fatalf("the recovering room's read point is at %d, want %d, where its replica stopped", point.seq, stopped)
	}
	if markdown := renderedState(t, state); !strings.Contains(markdown, "First, alice.") {
		t.Fatalf("the recovering room's read state renders %q, want the room as it stands", markdown)
	}

	releaseBob := holdPeerEdit(t, service, artifactID, &service.beforeObserveUpdate, replaceRun("Second.", "Second, bob."))
	state, point, _, err = service.readPoint(artifactID, live)
	if err != nil {
		t.Fatalf("read the room past Bob's commit: %v", err)
	}
	if point.seq != stopped+1 {
		t.Fatalf("the read point past Bob's commit is at %d, want %d", point.seq, stopped+1)
	}
	if markdown := renderedState(t, state); !strings.Contains(markdown, "Second, bob.") {
		t.Fatalf("the read state past Bob's commit renders %q, want his edit in it", markdown)
	}
	state0 := service.room(artifactID)
	state0.mu.Lock()
	queued := slices.ContainsFunc(state0.updateClasses, func(class *queuedClass) bool {
		return class.seq == point.seq && class.durable && class.contentChanged
	})
	state0.mu.Unlock()
	if !queued {
		t.Fatal("the read point took Bob's commit in without queueing its class at its position")
	}
	releaseBob()
	waitForLandedAppends(t, service, artifactID)
	assertStoredOrder(t, service, artifactID)
}
