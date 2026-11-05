package docs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Writes through the API to more documents than the budget of resident peerless rooms leave at
// most that many rooms resident, and the ones left are the ones written last: the budget closes
// the least recently used first (peerlessRoomsOverBudget). Nothing here waits a minute - the rooms
// idle out no sooner than roomIdleTimeout, so without the budget every room a write opened is
// still resident when this ends and the count is the documents written, not the budget.
//
// The document a browser has open is never one of them, however long ago it was written: the
// budget's close is ygo's unforced CloseRoom, which refuses a room with a peer, and the service
// does not even list such a room (its state holds the connection from the moment the browser is
// authorized). Here that document is the one written first, so an eviction by age alone would take
// it before any other.
func TestTheRoomBudgetClosesTheLeastRecentlyUsedPeerlessRooms(t *testing.T) {
	const budget = 8
	// Wide enough that nothing is closed while the writes run, so the rooms the trim below
	// weighs are every document written and the order it closes them in is the write order. A
	// trim that ran between the writes would be closing rooms from a list the writes were still
	// adding to, and the rooms left would be the newest only to within a pass.
	service, database := newRoomBudgetService(t, 100*budget)
	ids := createDocuments(t, database, 3*budget)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	// The browser's document is written first, so it is the oldest room of all.
	browsers := ids[0]
	connection, err := dialDocument(httpServer.URL, browsers)
	if err != nil {
		t.Fatalf("open the browser's document: %v", err)
	}
	defer leaveDocument(connection)
	waitFor(t, 30*time.Second, "the browser's room to load", func() bool {
		return service.srv.GetDoc(browsers) != nil
	})

	for _, id := range ids {
		appendThroughAPI(t, service, id)
	}
	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	service.lastRoomTrim.Store(0)
	service.trimPeerlessRooms()
	waitFor(t, 30*time.Second, "the rooms over the budget to close", func() bool {
		return service.PeerlessRoomBytes() <= service.peerlessRoomBytes.Load()
	})

	peerless := peerlessRoomNames(service)
	if len(peerless) > budget {
		t.Fatalf("%d peerless rooms resident after writes to %d documents, want at most the budget of %d",
			len(peerless), len(ids), budget)
	}
	want := ids[len(ids)-len(peerless):]
	slices.Sort(want)
	slices.Sort(peerless)
	if !slices.Equal(peerless, want) {
		t.Fatalf("the peerless rooms left are %v, want the %d documents written last, %v", peerless, len(want), want)
	}
	if service.srv.GetDoc(browsers) == nil {
		t.Fatalf("the budget closed the room the browser has open (%s), which it must never close", browsers)
	}
}

// A write to a document whose room the budget closed lands and settles: the room reloads from the
// store, which holds every update the closed room took (ygo flushes a room before it retires it),
// the write appends to what was there, and the settlement the write arms commits a version of the
// whole document. So the budget costs a reload and loses nothing.
func TestAWriteToARoomTheBudgetClosedLandsAndSettles(t *testing.T) {
	const budget = 1
	service, database := newRoomBudgetService(t, budget)
	service.settle = 20 * time.Millisecond
	ids := createDocuments(t, database, 4)
	first, rest := ids[0], ids[1:]

	appendMarkdownThroughAPI(t, service, first, "The write before the close.")
	for _, id := range rest {
		appendMarkdownThroughAPI(t, service, id, "A write to another document.")
	}
	waitFor(t, 30*time.Second, "the first document's room to close", func() bool {
		return service.srv.GetDoc(first) == nil
	})

	appendMarkdownThroughAPI(t, service, first, "The write after the close.")
	const want = "Room cap probe.\n\nThe write before the close.\n\nThe write after the close.\n"
	text, err := service.Text(context.Background(), first)
	if err != nil {
		t.Fatalf("read the document the budget closed: %v", err)
	}
	if text != want {
		t.Fatalf("the document reads back %q, want %q", text, want)
	}
	var markdown string
	waitFor(t, 30*time.Second, "the settlement of the reloaded room to commit its version", func() bool {
		return database.Pool.QueryRow(context.Background(), `
			select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
		`, first).Scan(&markdown) == nil && markdown == want
	})
}

// newRoomBudgetService is a document service whose budget of resident peerless rooms holds rooms
// of this package's test documents, and whose rooms idle out no sooner than production's
// roomIdleTimeout, so a test sees the budget's closes and nothing else. The budget is bytes
// (peerlessRoomHeapBudget): each of these documents' stored state is far below the weight floor,
// so each of their rooms weighs exactly roomHeapFloor and a budget of rooms of it holds that many.
// Its settlements are an hour out, so no settlement warms a room the budget closed; a test that
// wants one shortens it.
func newRoomBudgetService(t *testing.T, rooms int) (*Service, *store.Store) {
	t.Helper()
	database := storetest.Open(t)
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(database.Pool, testSigningKey, nil)},
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	})
	service.peerlessRoomBytes.Store(int64(rooms) * roomHeapFloor)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, database
}

// peerlessRoomNames is the documents the service holds a resident room for with no browser
// connected - what the budget counts, as Service.PeerlessRooms counts them - by name, so a test can
// say which rooms are left and not only how many.
func peerlessRoomNames(service *Service) []string {
	var peerless []string
	for _, room := range service.srv.Rooms() {
		if service.srv.GetDoc(room) == nil {
			continue
		}
		state := service.lockExistingState(room)
		if state == nil {
			continue
		}
		if len(state.connected) == 0 {
			peerless = append(peerless, room)
		}
		service.unlockState(room, state)
	}
	return peerless
}
