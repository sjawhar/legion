package docs

import (
	"sync"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// treePoint walks the room's replica (treeOfUnshared), and only a holder of the replica's mu may
// read it. The walk sits inside atPoint's callback, which holds that lock, so a room committing
// while a settlement reads it is serialised.
//
// Nothing else in this package drives those two at once. Carrying the replica's document out of
// the callback and walking it outside the lock leaves every other test here passing and the race
// detector quiet, because no test commits to a room while its tree is read. This test is that
// drive: one goroutine commits to the room and drains the commits into the replica while another
// reads the tree, so -race reports the walk the moment it leaves the lock.
func TestTheSettlementTreeReadWalksTheReplicaUnderItsLock(t *testing.T) {
	service := &Service{}
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, newProofHistory(t, 1).write("## Heading\n\nThe paragraph the room types into.\n"), nil); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	log := live.OpenUpdateLog()
	defer log.Close()
	replica := service.keepReplica(live, log, nil)
	// A commit of its own gives the replica the room's state to follow: without it the tree read
	// decodes the copy taken from the room itself (stateTree) and walks no replica at all.
	typeIntoLastParagraph(t, live, "first ")
	replica.observe("room")
	if replica.doc == nil {
		t.Fatal("the replica holds no state, so the tree read below would decode the room's own copy")
	}

	// The keystrokes run without the test's helpers, which call t.Fatal, since this goroutine is
	// not the test's own.
	target := lastParagraphText(t, live)
	const rounds = 200
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			live.Transact(func(txn *crdt.Transaction) { target.Insert(txn, 0, "x", nil) })
			replica.observe("room")
		}
	}()
	for range rounds {
		if _, _, _, err := service.treePoint("room", live); err != nil {
			t.Errorf("the tree read of a room committing beside it answered %v", err)
			break
		}
	}
	wg.Wait()
}
