package docs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A browser whose client id is lower than the server's writes 150,000 blocks into a paragraph the
// server created. A room's whole state lists its clients in ascending order, so the decoder meets
// every one of the browser's blocks before the paragraph it lands in. The room holds and serves
// that document, so every copy the service takes of it - the snapshot a read renders, and the fork
// a write starts from - holds it too, item for item.
func TestACopyHoldsEveryItemOneClientWroteAheadOfItsParent(t *testing.T) {
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
// TestAStoredHistoryLoadsEveryItemOneClientWroteAheadOfItsParent writes, in two updates.
const storedHistoryBlocks = 150_000

// The same shape of document, in a document's stored history rather than a copy of its room. A
// server client writes a paragraph; a browser numbered lower writes storedHistoryBlocks paragraphs
// ahead of it in two updates, each of which the store takes on its own, as a room persists them.
// The merged history meets the browser's paragraphs before the one they lean on, and the room that
// wrote it served it. That document is whole: a read with no room resident serves it, a room opens
// on it, and a rebuild, which would replace its history with its latest saved version, refuses it
// as a document that loads and leaves its history as it was.
func TestAStoredHistoryLoadsEveryItemOneClientWroteAheadOfItsParent(t *testing.T) {
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

// browserUpdateBlocks is how many paragraphs, each with its block id, the browser of
// TestTheStoreTakesOneBrowserUpdateItsRoomTook sends in one update: two items each, 150,000 in
// all, past ygo's default pending queue of 100,000.
const browserUpdateBlocks = 75_000

// A browser sends, in one update, paragraphs written ahead of one the document already holds, each
// with its block id. The room holds that paragraph and applies the update. The store's check of an
// update it appends decodes the update alone, where the first paragraph's neighbour is missing, so
// ygo defers it and parks every later item of the update behind it; at ygo's default queue that
// check refused the update once 100,000 were parked, which failed the room and dropped the edit.
// The store takes it, the room keeps it, and so does the room loaded again from the store; a
// transaction's append takes the same update.
func TestTheStoreTakesOneBrowserUpdateItsRoomTook(t *testing.T) {
	service, artifactID := newTestService(t)
	// Settlement is not under test, and its stamp would race the reads below.
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "before")
	persist := NewPgVersioned(service.store)
	seeded, err := persist.Load(ctx, artifactID)
	if err != nil {
		t.Fatalf("load the seeded document: %v", err)
	}
	browser := crdt.New()
	if err := crdt.ApplyUpdateV1(browser, seeded.Update, nil); err != nil {
		t.Fatalf("open the document in the browser: %v", err)
	}
	fragment := browser.GetXmlFragment(fragmentName)
	before := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) {
		// Prepended, since ygo walks a fragment's children from its start to find an insert
		// position; the first lands ahead of the seeded paragraph and leans on it.
		for block := range browserUpdateBlocks {
			paragraph := crdt.NewYXmlElement("paragraph")
			fragment.InsertElement(txn, 0, paragraph)
			paragraph.SetAttribute(txn, pmdoc.BlockIDAttr, "browser-"+strconv.Itoa(block))
		}
	})
	update := crdt.EncodeStateAsUpdateV1(browser, before)
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err == nil {
		t.Fatal("ygo's default pending queue took the browser's update; this test no longer reaches the queue")
	}
	want, err := renderDocument(browser)
	if err != nil {
		t.Fatalf("render the browser's document: %v", err)
	}

	t.Run("a room stores it and keeps it", func(t *testing.T) {
		httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
		t.Cleanup(httpServer.Close)
		peer := newDeepPeer(t, httpServer.URL, artifactID)
		waitFor(t, 10*time.Second, "the peer's room to open", func() bool {
			return service.srv.GetDoc(artifactID) != nil
		})
		state := service.room(artifactID)
		head, err := persist.Head(ctx, artifactID)
		if err != nil {
			t.Fatalf("read the document's head: %v", err)
		}
		peer.write(t, ygsync.EncodeUpdate(update))
		// The store holds the update once a version past head declares its items: the peer's
		// answer to the room's sync step 1, an empty update, can move the head first.
		stored := func() bool {
			versions, err := persist.ListVersions(ctx, artifactID)
			if err != nil {
				return false
			}
			for _, version := range versions {
				if version.Version <= head {
					return false
				}
				got, _, ok, err := persist.GetUpdate(ctx, artifactID, version.Version)
				if items, counted := updateItems(got); err == nil && ok && counted && items == 2*browserUpdateBlocks {
					return true
				}
			}
			return false
		}
		waitFor(t, time.Minute, "the room to store the browser's update or fail", func() bool {
			return state.failure() != nil || stored()
		})
		if err := state.failure(); err != nil {
			t.Fatalf("the browser's update failed the room: %v", err)
		}
		if text, err := service.Text(ctx, artifactID); err != nil || text != want {
			t.Fatalf("the room renders %d bytes (%v), want the browser's %d", len(text), err, len(want))
		}
		if err := service.Evict(ctx, artifactID); err != nil {
			t.Fatalf("evict the room: %v", err)
		}
		if text, err := service.Text(ctx, artifactID); err != nil || text != want {
			t.Fatalf("the stored history renders %d bytes (%v), want the browser's %d", len(text), err, len(want))
		}
	})
	t.Run("a transaction appends it", func(t *testing.T) {
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the append: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := persist.AppendUpdateTx(ctx, tx, artifactID, update, true); err != nil {
			t.Fatalf("append the browser's update in a transaction: %v", err)
		}
	})
}

// A settlement stamps a block id on every block that lacks one, in one update the room broadcasts
// to its peers. ygo checks each update the service broadcasts by decoding it alone under the
// server's pending queue (Server.MaxPendingItems), and the stamp of a block the document already
// holds leans on that block, so a settlement stamping more blocks than ygo's default queue of
// 100,000 broadcasts only under the server's maxUpdateItems. Refused, the broadcast fails the room
// on every reload, and the document is never stamped.
func TestASettlementStampsMoreBlocksThanYgosDefaultQueue(t *testing.T) {
	service, artifactID := newTestService(t)
	// The settlement runs below, once, on the test's own goroutine.
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	const blocks = 150_000
	if err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(txn *crdt.Transaction) {
			for range blocks {
				fragment.InsertElement(txn, 0, crdt.NewYXmlElement("paragraph"))
			}
		})
	}); err != nil {
		t.Fatalf("write %d paragraphs without block ids: %v", blocks, err)
	}
	versions := func() (count int) {
		t.Helper()
		if err := service.store.Pool.QueryRow(ctx, `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&count); err != nil {
			t.Fatalf("count document versions: %v", err)
		}
		return count
	}
	before := versions()

	settleCurrentGeneration(t, service, artifactID)
	if err := service.roomFailure(artifactID); err != nil {
		t.Fatalf("the settlement failed the room: %v", err)
	}
	if after := versions(); after != before+1 {
		t.Fatalf("the settlement wrote %d versions, want 1", after-before)
	}
	if stamped, err := service.Blocks(ctx, artifactID); err != nil || len(stamped) != blocks+1 {
		t.Fatalf("the settled document has %d blocks with ids (%v), want %d", len(stamped), err, blocks+1)
	}
	stored, err := NewPgVersioned(service.store).Load(ctx, artifactID)
	if err != nil {
		t.Fatalf("load the stored history: %v", err)
	}
	durable := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(durable, stored.Update, nil); err != nil {
		t.Fatalf("decode the stored history: %v", err)
	}
	tree, err := treeOf(durable)
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	if repairs := pmdoc.BlockIDRepairCount(tree); repairs != 0 {
		t.Fatalf("the stored document still needs %d block-id repairs", repairs)
	}
}
