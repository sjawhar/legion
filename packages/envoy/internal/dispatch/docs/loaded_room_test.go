package docs

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room a browser opens over a real socket is measured and charged at its load, and its replica
// follows the room's update log. The load's rendering is the room's first measurement: the bytes
// the peer update hook weighs (renderedReplica.size, which the hook reads through liveReplica) and
// the elements the room budget charges the load by (chargeLoadedRoom) beside the struct total the
// load counted, at both copies, in place of the socket's reservation. The replica then moves only
// through the room's commits as the update log hands them over (keepReplica, doc.OpenUpdateLog):
// once the browser's edit is in, the replica holds the room as of that commit with nothing left in
// the log, its rendering is the measurement, and the content every logged update added is charged
// once.
func TestAFreshlyLoadedRoomIsMeasuredAndChargedWithItsUpdateLog(t *testing.T) {
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	service.settle = time.Hour // no settlement writes into the room while it is weighed
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	room := createDocuments(t, database, 1)[0]
	browser := connectBrowser(t, httpServer.URL, room, "alice")

	live := service.srv.GetDoc(room)
	if live == nil {
		t.Fatal("the room is not resident while its browser is connected")
	}
	replica := service.liveReplica(live)
	if replica == nil {
		t.Fatal("the loaded room listed no replica for the peer update hook to read")
	}
	loaded := renderedLive(t, service, room)
	bytes, elements, measured := replica.size()
	if !measured || bytes != int64(len(loaded)) || elements != countElements(loaded) {
		t.Fatalf("the loaded room measures %d bytes and %d elements (measured %t), want its rendering's %d bytes and %d elements",
			bytes, elements, measured, len(loaded), countElements(loaded))
	}
	charge := replica.roomCharge()
	if charge == nil {
		t.Fatal("the loaded room's replica holds no charge in the room budget")
	}
	if want, used := roomWeight(copyHeap(bytes, elements, replica.structs.countedStructs()), 0, 0), budgetUsed(service); used != want {
		t.Fatalf("the budget holds %d bytes for the loaded room, want its load's count at both copies, %d", used, want)
	}

	// Every update the room commits from here on, as the room's own log records it.
	log := live.OpenUpdateLog()
	t.Cleanup(log.Close)
	_, before, _, _ := chargeParts(charge)
	const edited = "Room cap probe.\n\nAlice's paragraph.\n"
	editAsBrowser(t, service, room, browser, edited)
	want := renderedLive(t, service, room)
	waitFor(t, roomWait(t), "the replica to hold the browser's edit", func() bool {
		doc := replica.hold()
		if doc == nil {
			return false
		}
		defer replica.mu.Unlock()
		rendered, err := renderDocument(doc)
		return err == nil && rendered == want
	})
	if bytes, _, measured := replica.size(); !measured || bytes != int64(len(want)) {
		t.Fatalf("after the browser's edit the room measures %d bytes (measured %t), want its rendering's %d", bytes, measured, len(want))
	}
	_, logged := log.Drain()
	var added int64
	for _, update := range logged {
		added += contentAdded(update.Update)
	}
	if added == 0 {
		t.Fatal("the browser's edit added no content, so nothing here is charged")
	}
	if _, content, _, _ := chargeParts(charge); content-before != added {
		t.Fatalf("the room's charge took %d bytes of content for the updates it committed, want the %d they added", content-before, added)
	}
}

// A transaction's update is charged to its room once, by whichever update observer drains it from
// the room's update log, before that observer renders it. Bob's edit is held at its own observer
// (beforeObserveUpdate) while Carol's edit commits and its observer drains both: Bob's update is
// charged then, already, and his own observer, once released, finds nothing left to charge.
// Charging each observer's own update in that observer, as the room did before the log, would
// charge Bob's update after the rendering that holds it had been published, or twice beside the
// drain's charge.
func TestATransactionsUpdateIsChargedOnceWhenAnotherObserverRendersItFirst(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	settleCurrentGeneration(t, service, artifactID)
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("document room is not resident")
	}
	charge := service.liveReplica(live).roomCharge()
	if charge == nil {
		t.Fatal("the room's replica holds no charge in the room budget")
	}
	log := live.OpenUpdateLog()
	t.Cleanup(log.Close)
	_, before, _, _ := chargeParts(charge)

	release := holdPeerEdit(t, service, artifactID, &service.beforeObserveUpdate, appendBlocks(t, "Bob's paragraph.\n"))
	editAsPeer(t, service, artifactID, appendBlocks(t, "Carol's paragraph.\n"))
	_, logged := log.Drain()
	if len(logged) != 2 {
		t.Fatalf("the room committed %d updates, want Bob's and Carol's", len(logged))
	}
	var added int64
	for _, update := range logged {
		if contentAdded(update.Update) == 0 {
			t.Fatal("an edit added no content, so nothing here is charged")
		}
		added += contentAdded(update.Update)
	}
	if _, content, _, _ := chargeParts(charge); content-before != added {
		t.Fatalf("with Bob's observer held, the room's charge took %d bytes of content, want the %d both edits added, charged by Carol's observer's drain", content-before, added)
	}

	release()
	if pending := log.Pending(); pending != 0 {
		t.Fatalf("releasing Bob's observer committed %d more updates, want none", pending)
	}
	if _, content, _, _ := chargeParts(charge); content-before != added {
		t.Fatalf("once Bob's observer ran, the room's charge holds %d bytes of content, want the %d both edits added, each charged once", content-before, added)
	}
}

// A room's load decodes its stored state once of its own, and that copy is the room's replica: the
// copy a socket's admission decoded for its schema check (loadTree), or, with no admission before
// it, the one the load decoded for its scan count (LoadDoc). The replica holds it from the load on,
// before the room's first transaction, apart from the room's own document; neither the admission's
// preload nor the load's record keeps it; and the room's first transaction moves that same copy
// rather than decoding the room again.
func TestARoomsReplicaIsTheCopyItsLoadDecoded(t *testing.T) {
	for _, admitted := range []bool{true, false} {
		name := "with no admission before the load"
		if admitted {
			name = "after a socket's admission"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour // no settlement writes into the room while it is read
			seedServiceText(t, service, artifactID, "Alpha paragraph.\n\nBravo paragraph.\n")
			closeDocumentRoom(t, service, artifactID)
			ctx := context.Background()
			var preload *preloadedDocument
			if admitted {
				var err error
				if _, preload, err = service.loadTree(ctx, artifactID); err != nil || preload == nil || preload.decoded.doc == nil {
					t.Fatalf("the admission decoded no copy to preload (%v)", err)
				}
				defer service.leavePreload(artifactID, preload)()
			}
			var copied *crdt.Doc
			if preload != nil {
				copied = preload.decoded.doc
			}
			if err := service.warmLiveDocument(ctx, artifactID); err != nil {
				t.Fatal(err)
			}
			live := roomDocument(t, service, artifactID)
			replica := service.liveReplica(live)
			if replica == nil {
				t.Fatal("the loaded room lists no replica")
			}
			doc := replica.hold()
			if doc == nil {
				t.Fatal("the replica holds no document before the room's first transaction")
			}
			held, err := renderDocument(doc)
			replica.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case doc == live:
				t.Fatal("the replica is the room's own document")
			case copied != nil && doc != copied:
				t.Fatal("the replica is not the copy the socket's admission decoded")
			case preload != nil && preload.decoded.doc != nil:
				t.Fatal("the socket's preload still holds the copy its room's replica took")
			}
			if want := renderedLive(t, service, artifactID); held != want {
				t.Fatalf("the replica renders %q before any transaction, want the room's %q", held, want)
			}
			value, _ := service.roomLoads.Load(artifactID)
			load := value.(*roomLoad)
			load.mu.Lock()
			kept := load.loaded.decoded.doc
			load.mu.Unlock()
			if kept != nil {
				t.Fatal("the load's record still holds the copy its room's replica took")
			}

			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				added, err := pmdoc.Parse("Charlie paragraph.\n")
				if err != nil {
					t.Fatal(err)
				}
				tree.Children = append(tree.Children, added.Children...)
				return tree
			})
			want := renderedLive(t, service, artifactID)
			moved := replica.hold()
			if moved == nil {
				t.Fatal("the replica holds no document once the room's first transaction is observed")
			}
			after, err := renderDocument(moved)
			replica.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if moved != doc || after != want {
				t.Fatalf("after the room's first transaction the replica is the load's copy (%t) rendering %q, want that copy rendering the room's %q", moved == doc, after, want)
			}
		})
	}
}

// A socket's preload that no load will take gives up its copy at once: one left while the room is
// resident, which the socket joins as it is, and one left while a load that read the store before
// it runs, which that load drops as it ends. Neither stays listed, nor holds its copy for the
// socket's handler, which keeps the preload until the socket ends (leavePreload).
func TestAPreloadNoLoadWillTakeGivesUpItsCopy(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	store := &roomDuringColdReadStore{VersionedStore: NewPgVersioned(database), room: artifactID}
	service := New(Deps{
		Store:       database,
		Persistence: store,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "Alpha paragraph.\n\nBravo paragraph.\n")
	closeDocumentRoom(t, service, artifactID)
	ctx := context.Background()
	admit := func() *preloadedDocument {
		t.Helper()
		_, preload, err := service.loadTree(ctx, artifactID)
		if err != nil || preload == nil || preload.decoded.doc == nil {
			t.Fatalf("the admission decoded no copy to preload (%v)", err)
		}
		return preload
	}
	requireGivenUp := func(preload *preloadedDocument, when string) {
		t.Helper()
		if _, listed := service.preloads.Load(artifactID); listed {
			t.Fatalf("a preload is still listed %s", when)
		}
		if preload.decoded.doc != nil {
			t.Fatalf("the preload still holds its copy %s", when)
		}
	}

	// Both admissions decode while no room holds the document.
	during, resident := admit(), admit()
	var forget func()
	store.opened = func() { forget = service.leavePreload(artifactID, during) }
	store.armed.Store(true)
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatal(err)
	}
	if forget == nil {
		t.Fatal("the load did not read the store")
	}
	defer forget()
	requireGivenUp(during, "once the load it was left during has ended")

	defer service.leavePreload(artifactID, resident)()
	requireGivenUp(resident, "when it was left for a resident room")
}

// A browser's update that parks behind a gap in its own clocks leaves the replica holding what the
// room holds: the log records each committed transaction's update and what it parked beside it,
// and the replica applies both in turn (renderedReplica.observe, crdt.LoggedUpdate). The parked
// struct renders nothing while it waits, so the room's rendering and the replica's are the same
// through the gap and after the update that fills it, and the replica parks exactly what the room
// parks: a replica that took the update alone would hold neither the parked struct nor, until the
// gap was filled, the room's state.
func TestAPeerUpdateThatParksBehindAGapLeavesTheReplicaHoldingWhatTheRoomHolds(t *testing.T) {
	service, live, _ := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	rendering, err := renderDocument(live)
	if err != nil {
		t.Fatal(err)
	}
	decoded := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(decoded, crdt.EncodeStateAsUpdateV1(live, nil), nil); err != nil {
		t.Fatal(err)
	}
	log := live.OpenUpdateLog()
	t.Cleanup(log.Close)
	replica := service.keepReplica(live, log, &rendering, decoded)

	// Two of one browser's transactions, the second built on the first, sent out of order: the
	// room is given the second alone, whose struct names the first's as its origin.
	browser, text := peerDocument(t, live)
	browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "first ", nil) })
	filling := crdt.EncodeStateAsUpdateV1(browser, live.StateVector())
	held := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "second ", nil) })
	parking := crdt.EncodeStateAsUpdateV1(browser, held)

	requireReplicaHoldsTheRoom := func(when string) {
		t.Helper()
		replica.observe("room")
		room, err := renderDocument(live)
		if err != nil {
			t.Fatalf("%s: render the room: %v", when, err)
		}
		if replica.markdown == nil || *replica.markdown != room {
			t.Fatalf("%s: the replica renders %s, want the room's %q", when, describeRendering(replica.markdown), room)
		}
		roomPending, replicaPending := live.PendingStats(), replica.doc.PendingStats()
		if roomPending.Items != replicaPending.Items || roomPending.DeleteRanges != replicaPending.DeleteRanges ||
			!slices.Equal(roomPending.MissingFor, replicaPending.MissingFor) {
			t.Fatalf("%s: the replica parks %+v, want the room's %+v", when, replicaPending, roomPending)
		}
		if !bytes.Equal(crdt.EncodeStateAsUpdateV1(replica.doc, nil), crdt.EncodeStateAsUpdateV1(live, nil)) {
			t.Fatalf("%s: the replica's state is not the room's", when)
		}
	}

	if err := crdt.ApplyUpdateV1(live, parking, nil); err != nil {
		t.Fatalf("the room refused the later update: %v", err)
	}
	if parked := live.PendingStats(); parked.Items == 0 {
		t.Fatalf("the room parked %+v, want the update behind the gap in the browser's clocks parked", parked)
	}
	requireReplicaHoldsTheRoom("with the browser's second update parked behind the gap")

	if err := crdt.ApplyUpdateV1(live, filling, nil); err != nil {
		t.Fatalf("the room refused the update filling the gap: %v", err)
	}
	if parked := live.PendingStats(); parked.Items != 0 {
		t.Fatalf("the room still parks %+v once the gap is filled", parked)
	}
	requireReplicaHoldsTheRoom("once the update filling the gap is in")
	if want := "# second first Title\n"; !strings.Contains(*replica.markdown, want) {
		t.Fatalf("the replica renders %q, want it to hold %q", *replica.markdown, want)
	}
	runtime.KeepAlive(replica)
}
