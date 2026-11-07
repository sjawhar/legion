package docs

import (
	"bytes"
	"context"
	"fmt"
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
// the peer update hook weighs (roomWeighing.size, which the hook reads through roomWeighingOf).
// What the budget charges it is its tracked state - the bytes of the stored state its load
// decoded - at both copies (chargeLoadedRoom, roomWeight). The replica then moves only through
// the room's commits as the update log hands them over (keepReplica, doc.OpenUpdateLog): once
// the browser's edit is in, the replica holds the room as of that commit with nothing left in
// the log, its rendering is the measurement, and every logged update's own bytes have been added
// to the room's tracked state once (renderedReplica.chargeAdded).
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
	replica := service.replicaOf(live)
	if replica == nil {
		t.Fatal("the loaded room listed no replica for the peer update hook to read")
	}
	loaded := renderedLive(t, service, room)
	bytes, measured := replica.size()
	if !measured || bytes != int64(len(loaded)) {
		t.Fatalf("the loaded room measures %d bytes (measured %t), want its rendering's %d", bytes, measured, len(loaded))
	}
	charge := replica.roomCharge()
	if charge == nil {
		t.Fatal("the loaded room's replica holds no charge in the room budget")
	}
	tracked := replica.roomStateBytes()
	if tracked == 0 {
		t.Fatal("the loaded room is tracked at no state, want the bytes of the stored state its load decoded")
	}
	if want, used := roomWeight(tracked), budgetUsed(service); used != want {
		t.Fatalf("the budget holds %d bytes for the loaded room, want its %d tracked bytes of state at both copies, %d", used, tracked, want)
	}

	// Every update the room commits from here on, as the room's own log records it.
	log := live.OpenUpdateLog()
	t.Cleanup(log.Close)
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
	if bytes, measured := replica.size(); !measured || bytes != int64(len(want)) {
		t.Fatalf("after the browser's edit the room measures %d bytes (measured %t), want its rendering's %d", bytes, measured, len(want))
	}
	_, logged := log.Drain()
	var added int64
	for _, update := range logged {
		added += int64(len(update.Update))
	}
	if added == 0 {
		t.Fatal("the browser's edit committed no update, so nothing here is charged")
	}
	if after := replica.roomStateBytes(); after-tracked != added {
		t.Fatalf("the room's tracked state took %d bytes for the updates it committed, want the %d those updates hold", after-tracked, added)
	}
	if want, used := roomWeight(tracked+added), budgetUsed(service); used != want {
		t.Fatalf("the budget holds %d bytes once the edit is in, want the %d its tracked state comes to", used, want)
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
	replica := service.replicaOf(live)
	if replica == nil || replica.roomCharge() == nil {
		t.Fatal("the room's replica holds no charge in the room budget")
	}
	log := live.OpenUpdateLog()
	t.Cleanup(log.Close)
	before := replica.roomStateBytes()

	release := holdPeerEdit(t, service, artifactID, &service.beforeObserveUpdate, appendBlocks(t, "Bob's paragraph.\n"))
	editAsPeer(t, service, artifactID, appendBlocks(t, "Carol's paragraph.\n"))
	_, logged := log.Drain()
	if len(logged) != 2 {
		t.Fatalf("the room committed %d updates, want Bob's and Carol's", len(logged))
	}
	var added int64
	for _, update := range logged {
		if len(update.Update) == 0 {
			t.Fatal("an edit committed no update, so nothing here is charged")
		}
		added += int64(len(update.Update))
	}
	if after := replica.roomStateBytes(); after-before != added {
		t.Fatalf("with Bob's observer held, the room's tracked state took %d bytes, want the %d both edits hold, charged by Carol's observer's drain", after-before, added)
	}

	release()
	if pending := log.Pending(); pending != 0 {
		t.Fatalf("releasing Bob's observer committed %d more updates, want none", pending)
	}
	if after := replica.roomStateBytes(); after-before != added {
		t.Fatalf("once Bob's observer ran, the room's tracked state holds %d bytes more, want the %d both edits hold, each charged once", after-before, added)
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
			replica := service.replicaOf(live)
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
	decoded := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(decoded, crdt.EncodeStateAsUpdateV1(live, nil), nil); err != nil {
		t.Fatal(err)
	}
	log := live.OpenUpdateLog()
	t.Cleanup(log.Close)
	replica := service.keepReplica(live, log, liveRendering(t, live), decoded)

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
		held := markdownAt(replica.rendering)
		if held == nil || *held != room {
			t.Fatalf("%s: the replica renders %s, want the room's %q", when, describeRendering(held), room)
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
	if want := "# second first Title\n"; !strings.Contains(replica.rendering.markdown, want) {
		t.Fatalf("the replica renders %q, want it to hold %q", replica.rendering.markdown, want)
	}
	runtime.KeepAlive(replica)
}

// loadRendering is markdown as a room's load hands it to its replica (keepReplica,
// replicaRendering), for a test whose replica renders nothing of its own: it carries no layout,
// which only a replica splicing an update into the rendering reads (spliceTouched). So it is for
// a replica that never drains the room's log - one keepReplica is given no log - and a test whose
// replica follows the room's commits takes liveRendering instead: production makes no rendering
// without a layout (documentRenderingWithLayout, renderDocumentForUpdate, pmdoc.Layout.Splice),
// and a splice of one would read a layout that is not there.
func loadRendering(markdown string) *replicaRendering {
	return &replicaRendering{markdown: markdown}
}

// liveRendering is live's rendering as a room's load makes it, the markdown and the layout of one
// render (documentRenderingWithLayout), for a test whose replica follows the room's commits and
// splices an update into that rendering.
func liveRendering(tb testing.TB, live *crdt.Doc) *replicaRendering {
	tb.Helper()
	tree, err := treeOf(live)
	if err != nil {
		tb.Fatal(err)
	}
	markdown, _, layout, err := documentRenderingWithLayout(tree)
	if err != nil {
		tb.Fatal(err)
	}
	return &replicaRendering{markdown: markdown, layout: layout}
}

// headings is a rendering of a few thousand headings, a different one for each n.
func headings(n int) string {
	return strings.Repeat("# a\n", 4_096+n)
}

// forgedGCRunDocument is the document the forged GC run's rooms load with: one paragraph long
// enough that a delete over every other character of it is a few hundred ranges, and short enough
// that each room loads in a few milliseconds.
const forgedGCRunDocument = "word word word word word word word word word word\n"

// A GC run born deleted over every clock the room holds, which the peer update hook admits
// (gcRunOverHeldClocks: it integrates one struct, of a client the room holds nothing of, and
// nothing else), leaves the room taking edits: the delete a browser sends next is admitted, the
// replica's rendering after it is what a fresh render of the room answers, and the room's text
// carries the delete. The forged run touches no block of the replica's document - it integrates
// one struct born deleted, under no block - so the rendering the room loaded with is what the
// delete's own blocks are spliced into (renderedReplica.spliceTouched), which is why the room is
// loaded here as production loads one (drainedPeerUpdateRoom, keepReplica): with the markdown and
// the layout of one render of the state its replica starts from.
func TestARoomTakesAnHonestDeleteAfterAForgedGCRun(t *testing.T) {
	service, room, replica, _ := drainedPeerUpdateRoom(t, forgedGCRunDocument)
	before := firstText(room.GetXmlFragment(fragmentName)).ToString()
	forged := gcRunOverHeldClocks(room)
	if err, called := weighInto(service, room, forged); err != nil || !called {
		t.Fatalf("the %d-byte GC run was answered %v (applied %t), want it admitted", len(forged), err, called)
	}
	requireRoomRendered(t, room, replica, "the forged GC run")
	honest := peerDeletingEveryOtherCharacter(t, room)
	if err, called := weighInto(service, room, honest); err != nil || !called {
		t.Fatalf("the %d-byte honest delete was answered %v (applied %t), want it admitted", len(honest), err, called)
	}
	requireRoomRendered(t, room, replica, "the honest delete")
	if got, want := firstText(room.GetXmlFragment(fragmentName)).ToString(), everyOtherCharacterDeleted(before); got != want {
		t.Fatalf("the room's text after the delete reads %q, want %q", got, want)
	}
	runtime.KeepAlive(replica)
}

// The same forged GC run, and then the smallest edit there is: one character typed on a browser's
// own copy of the room (snapshotDocument), three of them, each from a copy taken after the one
// before it. Each is admitted, each leaves the replica's rendering equal to a fresh render of the
// room, and the room's text carries all three.
func TestARoomTakesTypedCharactersAfterAForgedGCRun(t *testing.T) {
	service, room, replica, _ := drainedPeerUpdateRoom(t, forgedGCRunDocument)
	before := firstText(room.GetXmlFragment(fragmentName)).ToString()
	forged := gcRunOverHeldClocks(room)
	if err, called := weighInto(service, room, forged); err != nil || !called {
		t.Fatalf("the %d-byte GC run was answered %v (applied %t), want it admitted", len(forged), err, called)
	}
	requireRoomRendered(t, room, replica, "the forged GC run")
	const typed = "y"
	for keystroke := 1; keystroke <= 3; keystroke++ {
		update := browserTypingACharacter(t, room, typed)
		if err, called := weighInto(service, room, update); err != nil || !called {
			t.Fatalf("keystroke %d, %d bytes, was answered %v (applied %t), want it admitted", keystroke, len(update), err, called)
		}
		requireRoomRendered(t, room, replica, fmt.Sprintf("keystroke %d", keystroke))
	}
	if got, want := firstText(room.GetXmlFragment(fragmentName)).ToString(), strings.Repeat(typed, 3)+before; got != want {
		t.Fatalf("the room's text after the three keystrokes reads %q, want %q", got, want)
	}
	runtime.KeepAlive(replica)
}

// A replica that could not place an update holds no document, no fragment and no rendering, and
// waits for the base the room's next commit records (renderedReplica.recover): the edit after that
// commit starts the replica from that base, rendering it whole, and splices its own block into that
// rendering. So a restart leaves the room taking edits too, and nothing on the path reads the
// document, the fragment or the layout the replica dropped.
func TestARestartedReplicaRendersItsBaseAndSplicesTheEditAfterIt(t *testing.T) {
	service, room, replica, _ := drainedPeerUpdateRoom(t, "# Title\n\nParagraph one of the room.\n\nParagraph two of the room.\n")
	// The room's first commit, which hands the replica the log's first base and leaves it
	// following the room (renderedReplica.start): what a restart after it starts again from is
	// the fresh base the log records, which no load rendered.
	if err, called := weighInto(service, room, browserTypingACharacter(t, room, "a")); err != nil || !called {
		t.Fatalf("the room's first edit was answered %v (applied %t), want it admitted", err, called)
	}
	requireRoomRendered(t, room, replica, "the room's first edit")
	// The replica cannot place the next update, as one whose apply failed cannot (takeLocked).
	forceRecover(replica)
	before := readRenderPaths(service)
	unplaced := browserTypingACharacter(t, room, "b")
	if err, called := weighInto(service, room, unplaced); err != nil || !called {
		t.Fatalf("the update the replica could not place was answered %v (applied %t), want it admitted", err, called)
	}
	doc, fragment, rendering, layout := replicaHolds(replica)
	if doc || fragment || rendering || layout {
		t.Fatalf("between the drop and the room's next commit the replica holds a document %t, a fragment %t, a rendering %t and a layout %t, want none of them",
			doc, fragment, rendering, layout)
	}
	if took := readRenderPaths(service).minus(before); took != (renderPaths{}) {
		t.Fatalf("the update it could not place took the paths %+v, want none", took)
	}
	before = readRenderPaths(service)
	edit := browserTypingACharacter(t, room, "c")
	if err, called := weighInto(service, room, edit); err != nil || !called {
		t.Fatalf("the edit after the restart was answered %v (applied %t), want it admitted", err, called)
	}
	if took, want := readRenderPaths(service).minus(before), (renderPaths{catchUpRendered: 1, observerIncremental: 1}); took != want {
		t.Fatalf("the edit after the restart took the paths %+v, want %+v", took, want)
	}
	requireRoomRendered(t, room, replica, "the edit after the restart")
	if got := paragraphTexts(room)[0].ToString(); !strings.HasPrefix(got, "cba") {
		t.Fatalf("the room's first paragraph reads %q, want it to carry the three characters typed", got)
	}
	runtime.KeepAlive(replica)
}

// requireRoomRendered holds the rendering the replica keeps after an update to a fresh render of
// the room: whichever path the update took - spliced, rendered whole, or taken from a published
// write - the replica's markdown is the room's own.
func requireRoomRendered(t *testing.T, room *crdt.Doc, replica *renderedReplica, after string) {
	t.Helper()
	want, err := renderDocument(room)
	if err != nil {
		t.Fatalf("after %s, render the room: %v", after, err)
	}
	held := markdownAt(heldRendering(replica))
	if held == nil {
		t.Fatalf("after %s the replica holds no rendering, want the room's", after)
	}
	if *held != want {
		t.Fatalf("after %s the replica's rendering is %q and a fresh render of the room %q", after, *held, want)
	}
}

// replicaHolds is whether the replica holds a document, a fragment, a rendering and a layout with
// that rendering, read under its lock.
func replicaHolds(replica *renderedReplica) (doc, fragment, rendering, layout bool) {
	replica.mu.Lock()
	defer replica.mu.Unlock()
	return replica.doc != nil, replica.fragment != nil, replica.rendering != nil,
		replica.rendering != nil && replica.rendering.layout != nil
}

// browserTypingACharacter is the update a browser sends that types character at the start of the
// first paragraph of its own copy of the room (snapshotDocument): the smallest edit the editor
// makes, from a copy of the room as it stands now.
func browserTypingACharacter(t *testing.T, room *crdt.Doc, character string) []byte {
	t.Helper()
	browser, err := snapshotDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	texts := paragraphTexts(browser)
	if len(texts) == 0 {
		t.Fatal("the browser's copy of the room holds no paragraph")
	}
	var sent []byte
	browser.OnUpdate(func(update []byte, _ any) { sent = update })
	browser.Transact(func(txn *crdt.Transaction) { texts[0].Insert(txn, 0, character, nil) })
	if sent == nil {
		t.Fatal("the browser's keystroke made no update")
	}
	return sent
}

// everyOtherCharacterDeleted is text with the characters peerDeletingEveryOtherCharacter deletes
// taken out of it: every other one from its last, none of them the first.
func everyOtherCharacterDeleted(text string) string {
	runes := []rune(text)
	for at := len(runes) - 1; at > 0; at -= 2 {
		runes = append(runes[:at], runes[at+1:]...)
	}
	return string(runes)
}
