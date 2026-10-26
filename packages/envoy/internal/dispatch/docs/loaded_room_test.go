package docs

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room a browser opens over a real socket is measured and charged at its load, and its replica
// follows the room's update log. The load's rendering is the room's first measurement: the bytes
// the peer update hook weighs (renderedReplica.size, which the hook reads through liveReplica) and
// the elements the room budget charges the load by (chargeLoadedRoom), at both copies, in place of
// the socket's reservation. The replica then moves only through the room's commits as the update
// log hands them over (keepReplica, doc.OpenUpdateLog): once the browser's edit is in, the replica
// holds the room as of that commit with nothing left in the log, its rendering is the measurement,
// and the content every logged update added is charged once.
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
	if want, used := roomWeight(copyHeap(bytes, elements), 0, 0), budgetUsed(service); used != want {
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
