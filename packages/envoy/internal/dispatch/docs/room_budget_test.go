package docs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Writes through the API to more documents than the budget of resident peerless rooms leave at
// most that many rooms resident, once their settlements have run and they are past the budget's
// window (roomBudgetMinIdle), and the ones left are the ones written last: the budget closes the
// least recently used first (peerlessRoomsOverBudget). Nothing here waits a minute - the rooms
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
	advance := roomBudgetClock(service)
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

	// The budget closes no room owing a settlement, and a settlement touches the room it
	// settles, so each write's settlement is run before the next write: at the end no room owes
	// one and the last thing each room saw is its own write. A second of the test's clock
	// separates the writes, so the order the budget reads off the rooms is the write order
	// whatever the writes themselves took, and the clock then moves past the window the budget
	// holds a freshly used room for. Both exclusions hold every room here, so without them the
	// trim closes nothing.
	for _, id := range ids {
		appendThroughAPI(t, service, id)
		settleRoomNow(t, service, id)
		advance(time.Second)
	}
	advance(roomBudgetMinIdle)
	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	trimNow(service)
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
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 4)
	first, rest := ids[0], ids[1:]

	// Each write's settlement is run before the next write, as in the test above, so the first
	// document's room is the least recently used of the four and owes nothing; the clock then
	// moves past the window the budget holds a freshly used room for.
	appendMarkdownThroughAPI(t, service, first, "The write before the close.")
	settleRoomNow(t, service, first)
	advance(time.Second)
	for _, id := range rest {
		appendMarkdownThroughAPI(t, service, id, "A write to another document.")
		settleRoomNow(t, service, id)
		advance(time.Second)
	}
	advance(roomBudgetMinIdle)
	trimNow(service)
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

// The budget leaves a room used inside roomBudgetMinIdle alone and takes an older one instead,
// even where the budget's bytes reach into the young rooms: a room the service used seconds ago
// is one it is about to use again, and closing it costs four reads of the store to load it back
// where keeping it costs one.
func TestTheRoomBudgetKeepsARoomUsedInsideItsWindow(t *testing.T) {
	const budget = 1
	// Wide while the rooms load, so the order below is the order this test set and not what a
	// trim between two loads left.
	service, database := newRoomBudgetService(t, 100*budget)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 3)
	older, young, youngest := ids[0], ids[1], ids[2]

	warmRoom(t, service, older)
	advance(roomBudgetMinIdle)
	warmRoom(t, service, young)
	warmRoom(t, service, youngest)

	// One room's weight: the budget reaches past the youngest room into young, which it may not
	// close, and on to older, which it may.
	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	trimNow(service)
	waitFor(t, 30*time.Second, "the room past the window to close", func() bool {
		return service.srv.GetDoc(older) == nil
	})
	if service.srv.GetDoc(young) == nil {
		t.Fatalf("the budget closed %s, used inside its %v window", young, roomBudgetMinIdle)
	}
	if service.srv.GetDoc(youngest) == nil {
		t.Fatalf("the budget closed %s, used inside its %v window", youngest, roomBudgetMinIdle)
	}
}

// The budget leaves a room owing a settlement alone and takes the next room over, however long
// ago that room was used: the settlement loads the document again the moment the close retires
// its room, so closing it buys the heap nothing and costs the settlement a reload.
func TestTheRoomBudgetKeepsARoomThatOwesASettlement(t *testing.T) {
	const budget = 2
	service, database := newRoomBudgetService(t, 100*budget)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 4)
	owing, next := ids[0], ids[1]

	for _, id := range ids {
		warmRoom(t, service, id)
		advance(time.Second)
	}
	// The service's settlements are an hour out, so this one stays owed for the whole test.
	service.scheduleSettle(owing)
	advance(roomBudgetMinIdle)

	// Two rooms' weight, so the budget reaches the two least recently used: the room owing the
	// settlement, which it must leave, and the room after it, which it closes instead.
	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	trimNow(service)
	waitFor(t, 30*time.Second, "the budget to close the room behind the one owing a settlement", func() bool {
		return service.srv.GetDoc(next) == nil
	})
	if service.srv.GetDoc(owing) == nil {
		t.Fatalf("the budget closed %s, which owes a settlement", owing)
	}
}

// Past the window the budget's order is what it always was: the least recently used room is the
// first closed, and the rooms held back are the newest. The window only holds a room back until
// it is old enough to close.
func TestTheRoomBudgetClosesTheLeastRecentlyUsedRoomPastItsWindow(t *testing.T) {
	const budget = 2
	service, database := newRoomBudgetService(t, 100*budget)
	advance := roomBudgetClock(service)
	ids := createDocuments(t, database, 4)

	for _, id := range ids {
		warmRoom(t, service, id)
		advance(time.Second)
	}
	advance(roomBudgetMinIdle)

	service.peerlessRoomBytes.Store(budget * roomHeapFloor)
	trimNow(service)
	waitFor(t, 30*time.Second, "the rooms over the budget to close", func() bool {
		return service.PeerlessRoomBytes() <= service.peerlessRoomBytes.Load()
	})
	peerless := peerlessRoomNames(service)
	want := ids[len(ids)-budget:]
	slices.Sort(want)
	slices.Sort(peerless)
	if !slices.Equal(peerless, want) {
		t.Fatalf("the peerless rooms left are %v, want the %d loaded last, %v", peerless, budget, want)
	}
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

// roomBudgetClock puts service on a clock this test moves, so a room's age against the budget's
// window (roomBudgetMinIdle) is the one the test set and not however long its writes took. It
// answers the function that moves the clock on; it is installed before any room loads, so no
// trim reads the clock while it is replaced.
func roomBudgetClock(service *Service) func(time.Duration) {
	started := time.Now()
	var aged atomic.Int64
	service.now = func() time.Time { return started.Add(time.Duration(aged.Load())) }
	return func(d time.Duration) { aged.Add(int64(d)) }
}

// trimNow weighs the peerless rooms at once, as any use of a room asks the service to
// (noteRoomUsed), without waiting out the roomTrimInterval since the last pass.
func trimNow(service *Service) {
	service.lastRoomTrim.Store(0)
	service.trimPeerlessRooms()
}

// warmRoom loads the document's room with an Apply that changes nothing, as a settlement's
// warm-up does (warmLiveDocument), and stamps it used now. The document's first version already
// matches its stored state (createDocuments), so the load leaves no settlement owed and the
// budget may close the room as soon as it is past its window.
func warmRoom(t *testing.T, service *Service, id string) {
	t.Helper()
	if err := service.warmLiveDocument(context.Background(), id); err != nil {
		t.Fatalf("load the room of document %s: %v", id, err)
	}
	if service.srv.GetDoc(id) == nil {
		t.Fatalf("document %s's room is not resident after its load", id)
	}
}

// settleRoomNow settles the document at once rather than at the end of its settle window and
// waits for the settlement to end, so its room is one the budget may close: it closes no room
// owing a settlement (peerlessRooms).
func settleRoomNow(t *testing.T, service *Service, id string) {
	t.Helper()
	service.SettleNow(id)
	waitFor(t, 30*time.Second, "the settlement of document "+id+" to end", func() bool {
		return !roomOwesSettlement(service, id)
	})
}

// roomOwesSettlement is whether the service holds a settlement of the document running or still
// to run (owesSettlementLocked), which keeps its room out of the budget's closes.
func roomOwesSettlement(service *Service, room string) bool {
	state := service.lockExistingState(room)
	if state == nil {
		return false
	}
	defer service.unlockState(room, state)
	return owesSettlementLocked(state, service.armedSettleTimers())
}
