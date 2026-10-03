package docs

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

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

// storedHistoryBlocks is how many paragraphs the browser of
// TestAStoredHistoryLoadsWhatItsRoomParksForOneClient writes, past ygo's default pending queue of
// 100,000, in two updates each under it.
const storedHistoryBlocks = 150_000

// The same parking, in a document's stored history rather than a copy of its room. A server client
// writes a paragraph; a browser numbered lower writes storedHistoryBlocks paragraphs ahead of it in
// two updates, each of which the store takes on its own, as a room persists them. The merged history
// meets the browser's paragraphs before the one they lean on, so ygo's default queue refuses it,
// while the room that wrote it served it. That document is whole: a read with no room resident
// serves it, a room opens on it, and a rebuild, which would replace its history with its latest
// saved version, refuses it as a document that loads and leaves its history as it was.
func TestAStoredHistoryLoadsWhatItsRoomParksForOneClient(t *testing.T) {
	service, artifactID := newTestService(t)
	// Settlement is not under test, and its stamp of every paragraph's block id would race the
	// subtests' reads of the history.
	service.settle = time.Hour
	ctx := context.Background()
	persist := NewPgVersioned(service.store)
	live := crdt.New(crdt.WithClientID(1_000_000))
	liveFragment := live.GetXmlFragment(fragmentName)
	live.Transact(func(txn *crdt.Transaction) {
		liveFragment.InsertElement(txn, 0, crdt.NewYXmlElement("paragraph"))
	})
	if _, err := persist.AppendUpdate(ctx, artifactID, crdt.EncodeStateAsUpdateV1(live, nil)); err != nil {
		t.Fatalf("store the server's paragraph: %v", err)
	}
	browser := crdt.New(crdt.WithClientID(5))
	if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(live, nil), nil); err != nil {
		t.Fatalf("open the document in the browser: %v", err)
	}
	browserFragment := browser.GetXmlFragment(fragmentName)
	for range 2 {
		before := browser.StateVector()
		// Prepended, since ygo walks a fragment's children from its start to find an insert
		// position; the first lands ahead of the server's paragraph and leans on it.
		browser.Transact(func(txn *crdt.Transaction) {
			for range storedHistoryBlocks / 2 {
				browserFragment.InsertElement(txn, 0, crdt.NewYXmlElement("paragraph"))
			}
		})
		if _, err := persist.AppendUpdate(ctx, artifactID, crdt.EncodeStateAsUpdateV1(browser, before)); err != nil {
			t.Fatalf("store the browser's paragraphs: %v", err)
		}
	}
	stored, err := persist.Load(ctx, artifactID)
	if err != nil {
		t.Fatalf("load the stored history: %v", err)
	}
	if err := crdt.ApplyUpdateV1(crdt.New(), stored.Update, nil); err == nil {
		t.Fatal("ygo's default pending queue took the stored history; this test no longer reaches the queue")
	}
	want, err := renderDocument(browser)
	if err != nil {
		t.Fatalf("render the browser's document: %v", err)
	}

	t.Run("reads with no room resident", func(t *testing.T) {
		if service.srv.GetDoc(artifactID) != nil {
			t.Fatal("the document's room is resident before the read")
		}
		if text, err := service.Text(ctx, artifactID); err != nil || text != want {
			t.Fatalf("read the stored history: %d bytes (%v), want the browser's %d", len(text), err, len(want))
		}
	})
	t.Run("opens in a room", func(t *testing.T) {
		if err := service.warmLiveDocument(ctx, artifactID); err != nil {
			t.Fatalf("open a room on the stored history: %v", err)
		}
		room := service.srv.GetDoc(artifactID)
		if room == nil {
			t.Fatal("no room is resident after it opened")
		}
		snapshot, err := snapshotDocument(room)
		if err != nil {
			t.Fatalf("snapshot the room: %v", err)
		}
		if text, err := renderDocument(snapshot); err != nil || text != want {
			t.Fatalf("the room renders %d bytes (%v), want the browser's %d", len(text), err, len(want))
		}
		if err := service.Evict(ctx, artifactID); err != nil {
			t.Fatalf("evict the room: %v", err)
		}
	})
	t.Run("refuses a rebuild with its history intact", func(t *testing.T) {
		before, err := persist.Load(ctx, artifactID)
		if err != nil {
			t.Fatalf("load the history before the rebuild: %v", err)
		}
		if _, _, err := rebuildDocument(t, service, artifactID, nil); !errors.Is(err, ErrDocumentLoads) {
			t.Fatalf("rebuild a document whose history loads: %v, want ErrDocumentLoads", err)
		}
		after, err := persist.Load(ctx, artifactID)
		if err != nil {
			t.Fatalf("load the history after the refused rebuild: %v", err)
		}
		if after.Version != before.Version || !bytes.Equal(after.Update, before.Update) {
			t.Fatalf("the refused rebuild moved the history from version %d (%d bytes) to version %d (%d bytes)", before.Version, len(before.Update), after.Version, len(after.Update))
		}
	})
}
