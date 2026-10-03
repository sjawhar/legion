package docs

import (
	"bytes"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A browser whose client id is lower than the server's writes many blocks into a paragraph the
// server created. A room's whole state lists its clients in ascending order, so ygo's decoder
// meets the browser's first block before the paragraph it lands in, defers it, and parks every
// later one of the browser's items behind it; at its default it refuses the update once 100,000
// are parked. The room holds and serves that document, so every copy the service takes of it -
// the snapshot a read renders, and the fork a write starts from - holds it too, item for item.
func TestACopyHoldsEveryItemItsRoomParksForOneClient(t *testing.T) {
	const blocks = 150_000
	live := crdt.New(crdt.WithClientID(1_000_000))
	fragment := live.GetXmlFragment(fragmentName)
	live.Transact(func(txn *crdt.Transaction) {
		fragment.InsertElement(txn, 0, crdt.NewYXmlElement("paragraph"))
	})
	browser := crdt.New(crdt.WithClientID(5))
	if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(live, nil), nil); err != nil {
		t.Fatalf("open the document in the browser: %v", err)
	}
	paragraph := browser.GetXmlFragment(fragmentName).Children()[0].(*crdt.YXmlElement)
	browser.Transact(func(txn *crdt.Transaction) {
		for range blocks {
			paragraph.InsertElement(txn, 0, crdt.NewYXmlElement("hard_break"))
		}
	})
	if err := crdt.ApplyUpdateV1(live, crdt.EncodeStateAsUpdateV1(browser, live.StateVector()), nil); err != nil {
		t.Fatalf("apply the browser's blocks to the room: %v", err)
	}
	state := crdt.EncodeStateAsUpdateV1(live, nil)
	if err := crdt.ApplyUpdateV1(crdt.New(), state, nil); err == nil {
		t.Fatal("ygo's default pending queue took the room's whole state; this test no longer reaches the queue")
	}

	snapshot, err := snapshotDocument(live)
	if err != nil {
		t.Fatalf("snapshot the room: %v", err)
	}
	if !bytes.Equal(crdt.EncodeStateAsUpdateV1(snapshot, nil), state) {
		t.Fatal("the snapshot holds a different document from its room")
	}
	fork, err := buildFork(&liveWrite{clientID: 42}, false, state)
	if err != nil {
		t.Fatalf("fork the room for a write: %v", err)
	}
	if !bytes.Equal(crdt.EncodeStateAsUpdateV1(fork, nil), state) {
		t.Fatal("the fork holds a different document from its room")
	}
}
