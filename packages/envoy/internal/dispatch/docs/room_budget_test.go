package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A room is charged one weight: its tracked state at bytesOfHeapAStateByte in each of the two
// copies a room keeps (roomWeight). Its load sets that state from the stored state it decoded
// (chargeLoadedRoom); each update the room's observer drains adds the update's own bytes to it
// (renderedReplica.chargeAdded, here driven through roomCharge.weigh, which is all that drain
// does); a compaction that folds the room's whole stored history into one row re-bases it
// (Service.rebaseRoomState); and the room's retirement drops the whole charge. The budget's
// arithmetic after that is one sum: what the charges come to, against the limit.
func TestTheRoomBudgetChargesARoomItsTrackedStateAtBothCopies(t *testing.T) {
	budget := &roomBudget{limit: roomBudgetBytes}
	const loaded, typed = 10_000, 100
	charge := budget.load("room", loaded)
	requireCharged(t, budget, charge, roomWeight(loaded), "a room that has taken no update is charged its stored state at both copies")

	charge.weigh(loaded + typed)
	requireCharged(t, budget, charge, roomWeight(loaded+typed), "an update's own bytes are added to the room's tracked state")
	charge.weigh(loaded + 2*typed)
	requireCharged(t, budget, charge, roomWeight(loaded+2*typed), "a later update adds its own bytes too")
	// A compaction's re-base is the one thing that lowers a room's tracked state while it stands:
	// the state a fold wrote, which merges a run of typed characters into one item and drops what
	// the updates carried of what they deleted.
	charge.weigh(loaded)
	requireCharged(t, budget, charge, roomWeight(loaded), "a compaction's re-base lowers the room to the state its fold wrote")

	budget.retire("room")
	charge.weigh(loaded + typed)
	if budget.used != 0 || len(budget.rooms) != 0 {
		t.Fatalf("the budget holds %d bytes over %d rooms once the room retired, want nothing", budget.used, len(budget.rooms))
	}

	// A budget no service built never sweeps, so an admission it has no room for is refused at
	// once, and leaves nothing waiting once it has been.
	other := budget.load("other", loaded)
	budget.limit = other.weight - 1
	ctx := context.Background()
	if _, err := budget.refusal(ctx, time.Now().Add(time.Hour), "room", 0); !errors.Is(err, errRoomBudgetFull) {
		t.Fatalf("an admission past the limit was answered %v, want it refused", err)
	}
	if count := waiting(budget); count != 0 {
		t.Fatalf("a refused admission left %d admissions waiting, want none", count)
	}
	// A sweep that leaves the budget within its limit raises nothing; one that leaves it over
	// raises full, which refuses every apply that integrates anything in every room, and the
	// room's retirement lowers it.
	budget.limit = roomBudgetBytes
	budget.swept(false)
	if budget.full.Load() {
		t.Fatal("a sweep that left the budget within its limit raised full")
	}
	budget.limit = other.weight - 1
	budget.swept(false)
	if !budget.full.Load() {
		t.Fatal("a sweep that left the budget over did not raise full")
	}
	budget.retire("other")
	if budget.full.Load() || budget.used != 0 {
		t.Fatalf("the budget holds %d bytes and full is %t once its room retired, want nothing and full lowered", budget.used, budget.full.Load())
	}
}

// An admission is weighed as the budget will stand once its room has loaded, not as it stands
// before: the weight it carries is the room it is about to load, at roomWeight of the stored
// state the admission read for it. So a budget with room to spare still refuses an admission
// whose own room would not fit, and the budget is never taken past its limit by admitting one.
// That is what keeps the pod out of the state a full budget puts it in, where full refuses every
// insert in every room until a sweep can close something (refusal, Service.weighPeerUpdate).
//
// A budget no service built never sweeps, so each refusal below is immediate and leaves nothing
// waiting.
func TestAnAdmissionIsWeighedWithTheRoomItIsAboutToLoad(t *testing.T) {
	const stored = 1_000_000
	budget := &roomBudget{limit: roomWeight(stored) + roomWeight(stored)/2}
	ctx := context.Background()
	first, err := budget.refusal(ctx, time.Now(), "first", roomWeight(stored))
	if err != nil {
		t.Fatalf("the first admission was answered %v on an empty budget with room for it", err)
	}
	// A second socket to the same document shares that reservation rather than holding a second:
	// one load serves both.
	shared, err := budget.refusal(ctx, time.Now(), "first", roomWeight(stored))
	if err != nil || shared.weight != 0 || budget.reserved != roomWeight(stored) {
		t.Fatalf("a second admission to the same document was answered %v holding %d, with %d reserved; want it to share the first's %d",
			err, shared.weight, budget.reserved, roomWeight(stored))
	}
	charge := budget.load("first", stored)
	if budget.reserved != 0 || !first.settled || !shared.settled {
		t.Fatalf("the room's load left %d bytes reserved, want every reservation of it settled", budget.reserved)
	}
	if charge == nil || budget.used != roomWeight(stored) {
		t.Fatalf("the budget holds %d bytes once the first room loaded, want %d", budget.used, roomWeight(stored))
	}
	// Half a room is left, so an admission for a room of this size is refused although the
	// charges themselves are still well inside the limit. Weighed the other way round - the
	// charges alone - it would be admitted and its load would take the budget over.
	if budget.used > budget.limit {
		t.Fatalf("the charges are %d of %d bytes, so an admission weighed on the charges alone would pass", budget.used, budget.limit)
	}
	refused, err := budget.refusal(ctx, time.Now(), "second", roomWeight(stored))
	if refused != nil || !errors.Is(err, errRoomBudgetFull) {
		t.Fatalf("an admission whose room would cross the limit was answered %v, want it refused", err)
	}
	if count := waiting(budget); count != 0 {
		t.Fatalf("a refused admission left %d admissions waiting, want none", count)
	}
	// A room small enough to fit what is left is admitted, and a room already charged is joined
	// rather than weighed at all.
	small, err := budget.refusal(ctx, time.Now(), "small", roomWeight(stored/4))
	if err != nil {
		t.Fatalf("an admission for a room that fits what is left was answered %v", err)
	}
	// Its reservation is weighed beside the charges: with it standing, a room of half the first
	// one no longer fits, though the charges alone would leave room for it.
	if _, err := budget.refusal(ctx, time.Now(), "another", roomWeight(stored/2)); !errors.Is(err, errRoomBudgetFull) {
		t.Fatalf("an admission weighed beside a standing reservation was answered %v, want it refused", err)
	}
	// A reservation released by a socket that goes before its room loads leaves nothing behind.
	budget.release(small)
	if budget.reserved != 0 {
		t.Fatalf("a released reservation left %d bytes reserved, want none", budget.reserved)
	}
	if !budget.join("first") {
		t.Fatal("a socket joining the room the budget already charges was not joined")
	}
}

// The budget's arithmetic at the shapes its design is stated in, each room loaded into an empty
// budget at the bytes of state its stored document holds. A room is charged
// bytesOfHeapAStateByte in each of roomCopies copies for each byte of its state, 28 bytes a
// state byte, whatever its shape renders: a mebibyte of prose, whose state is 3,035,240 bytes,
// is charged 84,986,720, so twenty-five of them fit the budget; a document at the state cap
// (maxRoomStateBytes) is charged 469,762,048, so four of those do; and the long-edited document,
// 5 KiB of prose carrying a million keystrokes in a 7,814,707-byte state, is charged
// 218,811,796, so nine of those do.
//
// Exactly what fits is admitted and the next is refused: an admission carries the weight of the
// room it is about to load (roomBudget.refusal), so the budget is never taken past its limit by
// admitting one. No room has a ceiling of its own, though: a document twice the whole budget is
// admitted into an empty one and charged what its state comes to, and what answers a room that
// heavy once it is open is the sweep and the hook's refusal of every apply that integrates
// anything while nothing can be closed (roomBudget.full).
func TestTheRoomBudgetsArithmetic(t *testing.T) {
	for _, shape := range []struct {
		name           string
		stateBytes     int64
		weight, admits int64
	}{
		{"a room of a mebibyte of prose", 3_035_240, 84_986_720, 25},
		{"a document at the state cap", maxRoomStateBytes, 469_762_048, 4},
		{"the long-edited document", 7_814_707, 218_811_796, 9},
		{"a document twice the whole budget", roomBudgetBytes, 60_129_542_144, 0},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if weight := roomWeight(shape.stateBytes); weight != shape.weight {
				t.Fatalf("a room tracked at %d bytes of state is charged %d bytes, want %d", shape.stateBytes, weight, shape.weight)
			}
			budget := &roomBudget{limit: roomBudgetBytes}
			admitted := int64(0)
			for {
				if _, err := budget.refusal(context.Background(), time.Now(), "room", shape.weight); errors.Is(err, errRoomBudgetFull) {
					break
				} else if err != nil {
					t.Fatalf("admit room %d: %v", admitted+1, err)
				}
				charge := budget.load(fmt.Sprintf("room-%d", admitted), shape.stateBytes)
				if charge.weight != shape.weight {
					t.Fatalf("room %d is charged %d bytes, want %d", admitted+1, charge.weight, shape.weight)
				}
				admitted++
			}
			if admitted != shape.admits {
				t.Fatalf("the budget admitted %d rooms charged %d bytes each, want %d", admitted, shape.weight, shape.admits)
			}
			if budget.used > budget.limit {
				t.Fatalf("the rooms admitted come to %d bytes against the %d-byte limit: an admission took the budget past it",
					budget.used, budget.limit)
			}
			// No ceiling on one room: the heaviest shape is refused at admission by the budget,
			// and loaded anyway where nothing weighed it, as the server's own work does.
			if shape.admits == 0 {
				charge := budget.load("server-opened", shape.stateBytes)
				if charge.weight != shape.weight || budget.used != shape.weight {
					t.Fatalf("a room the server opened over the budget is charged %d of %d bytes, want %d",
						charge.weight, budget.used, shape.weight)
				}
			}
		})
	}
}

func requireCharged(t *testing.T, budget *roomBudget, charge *roomCharge, want int64, what string) {
	t.Helper()
	if charge.weight != want || budget.used != want {
		t.Fatalf("%s: the room is charged %d bytes and the budget holds %d, want %d", what, charge.weight, budget.used, want)
	}
}

// fakeSwept is a budget whose sweeps are the test's: each start is reported on the channel it
// returns, and lasts until the test ends it (roomBudget.swept).
func fakeSwept(limit int64) (*roomBudget, <-chan struct{}) {
	sweeps := make(chan struct{}, 8)
	budget := &roomBudget{limit: limit}
	budget.sweep = func() { sweeps <- struct{}{} }
	return budget, sweeps
}

// waiting reads how many admissions wait on budget.
func waiting(budget *roomBudget) int {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.waiting
}

// An admission the budget has no room for waits for the sweeps it starts, and is refused only
// once the last of them has ended with nothing left that something other than a browser holds.
// There are two of them here: the charge that takes the budget past its limit starts one of its
// own (unlock), and the admission that arrives while that one runs asks for it to run again
// (sweepAgainLocked), since the first may have weighed the budget before the admission arrived.
// A second look at a budget past its limit waits the same way, and passes once a room's
// retirement brings the budget back within it.
func TestTheRoomBudgetRefusesAnAdmissionOnlyOnceItsSweepHasEnded(t *testing.T) {
	ctx := context.Background()
	// A limit one byte under what the one charged room comes to, so the budget is over it.
	budget, sweeps := fakeSwept(roomWeight(20) - 1)
	budget.load("held", 20)
	<-sweeps
	answered := make(chan error, 1)
	go func() { answered <- second(budget.refusal(ctx, time.Now().Add(time.Minute), "room", 0)) }()
	waitFor(t, 5*time.Second, "the admission to wait", func() bool { return waiting(budget) == 1 })
	select {
	case err := <-answered:
		t.Fatalf("the admission was answered %v while the charge's own sweep ran", err)
	case <-time.After(100 * time.Millisecond):
	}
	budget.swept(false)
	select {
	case <-sweeps:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep the admission asked for while the first ran never started")
	}
	select {
	case err := <-answered:
		t.Fatalf("the admission was answered %v while the sweep it asked for ran", err)
	case <-time.After(100 * time.Millisecond):
	}
	budget.swept(false)
	select {
	case err := <-answered:
		if !errors.Is(err, errRoomBudgetFull) {
			t.Fatalf("the admission was answered %v once its sweep ended with nothing pending, want it refused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the admission still waits after its sweep ended with nothing pending")
	}
	if count := waiting(budget); count != 0 {
		t.Fatalf("a refused admission left %d admissions waiting", count)
	}

	passed := make(chan error, 1)
	go func() { passed <- second(budget.refusal(ctx, time.Now().Add(time.Minute), "room", 0)) }()
	<-sweeps
	waitFor(t, 5*time.Second, "the look past the limit to wait", func() bool { return waiting(budget) == 1 })
	budget.swept(true)
	select {
	case err := <-passed:
		t.Fatalf("the look past the limit was answered %v while a room was pending", err)
	case <-time.After(100 * time.Millisecond):
	}
	budget.retire("held")
	select {
	case err := <-passed:
		if err != nil {
			t.Fatalf("the look past the limit was answered %v once a retirement brought the budget within it", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the look past the limit still waits with the budget within it")
	}
}

// Every admission waiting on the budget is kept waiting until the budget has room for it: a
// sweep that ends with two still waiting leaves both of them waiting, and the second, which
// began to wait while that sweep ran, has it run once more. Once a room retires both pass.
func TestTheRoomBudgetKeepsEveryWaitingAdmissionAcrossASweep(t *testing.T) {
	ctx := context.Background()
	deadline := time.Now().Add(time.Minute)
	budget, sweeps := fakeSwept(roomWeight(20) - 1)
	budget.load("held", 20)
	admitted := make(chan error, 2)
	admit := func() { admitted <- second(budget.refusal(ctx, deadline, "room", 0)) }
	go admit()
	<-sweeps
	waitFor(t, 5*time.Second, "the first admission to wait", func() bool { return waiting(budget) == 1 })
	go admit()
	waitFor(t, 5*time.Second, "the second admission to wait", func() bool { return waiting(budget) == 2 })
	budget.swept(true)
	select {
	case <-sweeps:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep an admission began to wait during did not run again")
	}
	if count := waiting(budget); count != 2 {
		t.Fatalf("after a sweep ended %d admissions wait, want both", count)
	}
	budget.retire("held")
	for range 2 {
		select {
		case err := <-admitted:
			if err != nil {
				t.Fatalf("a waiting admission was answered %v once a room retired, want it admitted", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a waiting admission was not admitted once a room retired")
		}
	}
	budget.swept(false)
	if count := waiting(budget); count != 0 || budget.used != 0 {
		t.Fatalf("after both were admitted %d wait and the budget holds %d bytes, want none and nothing", count, budget.used)
	}
}

// Every way out of the document socket's handler leaves the budget charging nothing for a
// document no room holds, and the one way that keeps the socket charges the room its load makes:
// a socket refused outside the schema, one whose admission load fails, one the budget refuses,
// and one that joins its room. Nothing is reserved before a load: the budget charges a room only
// once its load has decoded the state it is charged for (chargeLoadedRoom), and what keeps a
// burst of admissions from each reading the same headroom is the load slot each of their loads
// takes (MemoryBounds.LoadSlots).
func TestTheDocumentSocketChargesNothingUntilItsRoomLoads(t *testing.T) {
	database := storetest.Open(t)
	watch := &admissionWatchStore{VersionedStore: NewPgVersioned(database), charged: make(map[string][]int64)}
	service := newRoomBudgetService(t, database, watch)
	watch.service.Store(service)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 4)
	outside, failing, refused, joined := ids[0], ids[1], ids[2], ids[3]

	t.Run("refused outside the schema", func(t *testing.T) {
		if err := AppendRenderOnlySchemaViolationForTest(context.Background(), database, outside); err != nil {
			t.Fatal(err)
		}
		requireClosedAtUpgrade(t, httpServer.URL, outside, documentSchemaCloseCode, contracts.DocumentSchemaCloseReason)
		watch.requireChargedAtLoad(t, outside, 0)
		requireNothingCharged(t, service)
	})
	t.Run("its admission load fails", func(t *testing.T) {
		watch.failing.Store(failing)
		if connection, err := dialDocument(httpServer.URL, failing); err == nil {
			_ = connection.Close()
			t.Fatal("the socket opened, want the failed load answered 503")
		} else if !strings.Contains(err.Error(), "503") {
			t.Fatalf("the socket was refused with %v, want 503", err)
		}
		watch.requireChargedAtLoad(t, failing, 0)
		requireNothingCharged(t, service)
	})
	t.Run("the budget has no room for it", func(t *testing.T) {
		// One room a browser holds, which no sweep may close, and a limit under its charge.
		holder, err := dialDocument(httpServer.URL, joined)
		if err != nil {
			t.Fatalf("open the room that fills the budget: %v", err)
		}
		waitFor(t, 10*time.Second, "the room that fills the budget to go live", func() bool { return service.srv.GetDoc(joined) != nil })
		service.SetRoomBudgetForTest(1)
		t.Cleanup(func() { service.SetRoomBudgetForTest(0) })
		requireClosedAtUpgrade(t, httpServer.URL, refused, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
		if service.srv.GetDoc(refused) != nil || len(watch.loads(refused)) != 0 {
			t.Fatal("the refused socket's document was loaded, want it refused before any load")
		}
		leaveDocument(holder)
		if err := service.Evict(context.Background(), joined); err != nil {
			t.Fatal(err)
		}
		requireNothingCharged(t, service)
	})
	t.Run("it joins its room", func(t *testing.T) {
		connection, err := dialDocument(httpServer.URL, joined)
		if err != nil {
			t.Fatalf("open the document: %v", err)
		}
		waitFor(t, 10*time.Second, "the room to go live", func() bool { return service.srv.GetDoc(joined) != nil })
		watch.requireChargedAtLoad(t, joined, 0)
		replica := service.replicaOf(service.srv.GetDoc(joined))
		if replica == nil {
			t.Fatal("the loaded room lists no replica")
		}
		tracked := replica.roomStateBytes()
		if used, want := budgetUsed(service), roomWeight(tracked); tracked == 0 || used != want {
			t.Fatalf("the budget holds %d bytes for the open room, want its %d tracked bytes of state at both copies, %d", used, tracked, want)
		}
		leaveDocument(connection)
		if err := service.Evict(context.Background(), joined); err != nil {
			t.Fatal(err)
		}
		if used := budgetUsed(service); used != 0 {
			t.Fatalf("the budget holds %d bytes once the room retired, want none", used)
		}
	})
}

// Over the budget, the sweep closes the rooms nothing is still using and leaves the ones something
// is, then raises the signal the hook reads; the next browser is refused with the transient close
// before any room is made for it, while the server's own write still lands and its room is closed
// once it has; and once the last room in use is left, the budget closes it and lowers the signal.
func TestTheRoomBudgetClosesUnusedRoomsAndRefusesWhenNoneIsLeft(t *testing.T) {
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 3)
	used, written, refused := ids[0], ids[1], ids[2]

	connection, err := dialDocument(httpServer.URL, used)
	if err != nil {
		t.Fatalf("open the document in use: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	waitFor(t, 10*time.Second, "the room in use to go live", func() bool { return service.srv.GetDoc(used) != nil })
	appendThroughAPI(t, service, written)
	waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, written) })

	service.SetRoomBudgetForTest(1)
	waitFor(t, 10*time.Second, "the sweep to close the unused room", func() bool { return service.srv.GetDoc(written) == nil })
	waitFor(t, 10*time.Second, "the budget to report itself full", service.budget.full.Load)
	if service.srv.GetDoc(used) == nil {
		t.Fatal("the sweep closed the room a browser holds open, want it kept")
	}

	requireClosedAtUpgrade(t, httpServer.URL, refused, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
	if service.srv.GetDoc(refused) != nil {
		t.Fatal("the refused browser's room was made, want none")
	}

	// The server's own write is never refused: it opens its room over the budget, lands, and the
	// room goes once nothing holds it, the signal staying up for the room still in use.
	appendThroughAPI(t, service, refused)
	if text, err := service.Text(context.Background(), refused); err != nil || strings.Count(text, "Room cap probe.") != 2 {
		t.Fatalf("the document the server wrote over a full budget reads %q (%v), want the write", text, err)
	}
	waitFor(t, 10*time.Second, "the server's room to close once its write lands", func() bool { return service.srv.GetDoc(refused) == nil })
	if !service.budget.full.Load() {
		t.Fatal("the budget lowered its signal with the room in use still over it")
	}

	leaveDocument(connection)
	waitFor(t, 10*time.Second, "the left room to close", func() bool { return service.srv.GetDoc(used) == nil })
	waitFor(t, 10*time.Second, "the budget to lower its signal", func() bool { return !service.budget.full.Load() })
	if used := budgetUsed(service); used != 0 {
		t.Fatalf("the budget holds %d bytes with no room open, want none", used)
	}
}

// An admission the budget has no room for while it is within its limit starts a sweep that makes
// room for it, and waits for it: the sweep closes unused rooms, the heaviest first, until the
// budget is within its limit, and the admission is admitted into the room it made, its first
// attempt. Nothing in this raises full, which refuses growth in every room.
func TestTheRoomBudgetAdmissionWaitsForItsSweepAndIsAdmittedIntoTheRoomItMakes(t *testing.T) {
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 4)
	unused, joining := ids[:3], ids[3]
	for _, room := range unused {
		appendThroughAPI(t, service, room)
		waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, room) })
	}
	charged := budgetUsed(service)
	service.budget.mu.Lock()
	lightest := int64(0)
	for _, room := range unused {
		weight := service.budget.rooms[room].charges[0].weight
		if lightest == 0 || weight < lightest {
			lightest = weight
		}
	}
	service.budget.mu.Unlock()
	if lightest <= 0 {
		t.Fatalf("the lightest unused room is charged %d bytes, want a room whose close makes room", lightest)
	}

	// A limit one byte under what the unused rooms fill: the sweep must close one of them before
	// the admission passes.
	service.SetRoomBudgetForTest(charged - 1)
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })
	connection, err := dialDocument(httpServer.URL, joining)
	if err != nil {
		t.Fatalf("an admission to a budget its unused rooms fill was refused: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	waitFor(t, 10*time.Second, "the admitted room to go live", func() bool { return service.srv.GetDoc(joining) != nil })
	closed := 0
	for _, room := range unused {
		if service.srv.GetDoc(room) == nil {
			closed++
		}
	}
	if closed == 0 || closed == len(unused) {
		t.Fatalf("the sweep closed %d of the %d unused rooms, want the ones that make room for the admission and no more", closed, len(unused))
	}
	if count := waiting(&service.budget); count != 0 {
		t.Fatalf("the admitted admission left %d admissions waiting, want none", count)
	}
}

// Every transaction the peer hook did not apply is charged to its room as a browser's admitted
// update is - by the room's own update observer, from the bytes of the update that observer
// drains (renderedReplica.chargeAdded) - and none is refused for it: here the server's own
// writes, into a room one browser holds, with the budget far past its limit and full raised. A
// browser's update the hook admits is charged the same way, once, by that same drain.
func TestTheRoomBudgetChargesEveryServerWriteAndRefusesNone(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	service.settle = time.Hour // no settlement writes into the room while it is weighed
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	room := createDocumentsHolding(t, database, 1, "First paragraph of ordinary prose.")[0]
	browser := connectBrowser(t, httpServer.URL, room, "alice")
	t.Cleanup(browser.Close)
	live := service.srv.GetDoc(room)
	replica := service.replicaOf(live)
	if replica == nil || replica.roomCharge() == nil {
		t.Fatal("the browser's room holds no charge in the room budget")
	}
	tracked := func() int64 { return replica.roomStateBytes() }

	// A browser's keystroke, through the hook into the room the observer drains.
	origin := &struct{ browser string }{"alice"}
	keystroke := peerTyping(t, live, "y")
	before := tracked()
	if err := service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: room, Doc: live, Update: keystroke, Origin: origin}, func() error {
		return crdt.ApplyUpdateV1(live, keystroke, origin)
	}); err != nil {
		t.Fatalf("the hook refused a keystroke: %v", err)
	}
	waitFor(t, 10*time.Second, "the room's observer to drain the keystroke", func() bool { return tracked() > before })
	if grew := tracked() - before; grew > int64(len(keystroke)) {
		t.Fatalf("a browser's keystroke grew the room's tracked state by %d bytes, want no more than its update's %d", grew, len(keystroke))
	}

	service.SetRoomBudgetForTest(1)
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })
	waitFor(t, 10*time.Second, "the budget to report itself full", service.budget.full.Load)
	write := func(what string, change func(doc *crdt.Doc, transact func(func(*crdt.Transaction)))) int64 {
		t.Helper()
		before := tracked()
		if err := service.srv.Apply(ctx, room, change); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Fatalf("%s on a full budget: %v", what, err)
		}
		waitFor(t, 10*time.Second, "the room's observer to drain "+what, func() bool { return tracked() > before })
		return tracked() - before
	}
	inText := func(edit func(text *crdt.YXmlText, txn *crdt.Transaction)) func(*crdt.Doc, func(func(*crdt.Transaction))) {
		return func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			// Read before the transaction, which holds the document's lock the read takes.
			text := firstText(doc.GetXmlFragment(fragmentName))
			transact(func(txn *crdt.Transaction) { edit(text, txn) })
		}
	}
	if grew := write("type three characters", inText(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Insert(txn, 0, "abc", nil) })); grew == 0 {
		t.Fatal("three characters the server typed grew the room's tracked state by nothing")
	}
	if grew := write("delete one character", inText(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Delete(txn, 8, 1) })); grew == 0 {
		t.Fatal("a character the server deleted grew the room's tracked state by nothing: a delete's own update is still bytes the room holds")
	}
	before = tracked()
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := joinedApplyOps(service, room, []model.EditOp{{Op: "replace", Find: "ordinary", With: "plain"}}, agent, nil); err != nil {
		t.Fatalf("an API write on a full budget: %v", err)
	}
	waitFor(t, 10*time.Second, "the room's observer to drain the API write", func() bool { return tracked() > before })
	if text := renderedLive(t, service, room); !strings.Contains(text, "plain prose") {
		t.Fatalf("the room reads %q after the API write, want it to hold the write", text)
	}
	if used, want := budgetUsed(service), roomWeight(tracked()); used != want {
		t.Fatalf("the budget holds %d bytes for the room, want the %d its %d tracked bytes of state come to", used, want, tracked())
	}
}

// An admission whose budget is short waits for a room whose browser has just gone: such a room
// is not closable until what the browser left - here a latest edit source no settlement has read,
// which a browser's edit leaves until its settlement runs - is gone. Once it is, the room's state
// starts a sweep that closes it, and the waiting admission passes. With what the browser left
// never ending, the admission is refused only once admissionWait has passed.
func TestTheRoomBudgetAdmissionWaitsForARoomWhoseBrowserJustLeft(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	// The admissions below are the budget's own look, which names no document; the ids are
	// the rooms the budget must make room out of.
	left := createDocuments(t, database, 3)[0]
	appendThroughAPI(t, service, left)
	waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, left) })
	hold := func(unsettled bool) {
		state := service.lockExistingState(left)
		if state == nil {
			t.Fatal("the room whose browser left has no state")
		}
		state.unsettled = unsettled
		service.unlockState(left, state)
	}
	hold(true)
	// One byte under what the rooms are charged, so the budget stands over its limit: nothing is
	// reserved for an admission any more (roomBudget.refusal), so the room that crosses the limit
	// is the last one admitted and a limit at exactly what the rooms come to still fits one more.
	service.SetRoomBudgetForTest(budgetUsed(service) - 1)
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })

	answered := make(chan error, 1)
	go func() {
		answered <- second(service.budget.refusal(ctx, time.Now().Add(service.admissionWait), "room", 0))
	}()
	waitFor(t, 5*time.Second, "the admission to wait", func() bool { return waiting(&service.budget) == 1 })
	select {
	case err := <-answered:
		t.Fatalf("the admission was answered %v while the room whose browser left still settled", err)
	case <-time.After(300 * time.Millisecond):
	}
	if service.srv.GetDoc(left) == nil {
		t.Fatal("the budget closed the room whose browser left before it was done with")
	}
	hold(false)
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the admission was answered %v once the room whose browser left was closable, want it admitted", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admission still waits after the room whose browser left became closable")
	}
	if service.srv.GetDoc(left) != nil {
		t.Fatal("the room whose browser left is still open after the admission it made room for")
	}

	// The same wait with what the browser left never ending: the admission is refused once
	// admissionWait has passed, and not before. The room is written into again under the whole
	// budget, so it is not opened over it.
	service.SetRoomBudgetForTest(0)
	appendThroughAPI(t, service, left)
	waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, left) })
	hold(true)
	t.Cleanup(func() {
		if state := service.lockExistingState(left); state != nil {
			state.unsettled = false
			service.unlockState(left, state)
		}
	})
	service.SetRoomBudgetForTest(budgetUsed(service) - 1)
	service.admissionWait = 500 * time.Millisecond
	started := time.Now()
	_, err := service.budget.refusal(ctx, time.Now().Add(service.admissionWait), "room", 0)
	if !errors.Is(err, errRoomBudgetFull) {
		t.Fatalf("an admission nothing could make room for was answered %v, want it refused", err)
	}
	if waited := time.Since(started); waited < service.admissionWait {
		t.Fatalf("the admission was refused after %v, before the %v it waits for a pending room", waited, service.admissionWait)
	}
	if service.srv.GetDoc(left) == nil {
		t.Fatal("the budget closed a room something still held")
	}
}

// The same wait with a real browser: one edits its document over the socket and leaves, which
// settles the room at once (settleLastPeer), and the next admission, to a budget that room fills,
// arrives while the settlement still runs - here held at its document lock, as a mebibyte's
// settlement takes seconds on a loaded pod. The admission waits for the settlement to commit and
// the sweep to close the room, and is admitted into the room the close made: the room is gone by
// the time the admission is answered, never refused first and closed after.
func TestTheRoomBudgetAdmitsABrowserOnceTheRoomAnEditingBrowserLeftIsClosed(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	// The edit's own settle timer never fires: the browser's leaving takes it and settles at once.
	service.settle = time.Minute
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	left := createDocuments(t, database, 2)[0]
	settling, release := make(chan struct{}, 1), make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	service.afterSettleLock = func(room string) {
		if room == left {
			select {
			case settling <- struct{}{}:
			default:
			}
			<-release
		}
	}

	peer := connectPeer(t, httpServer.URL, left, "alice")
	select {
	case <-peer.Answers:
	case <-time.After(10 * time.Second):
		t.Fatal("the browser was never handed its document")
	}
	text := firstText(peer.Doc.GetXmlFragment(fragmentName))
	if text == nil {
		t.Fatal("the browser's document holds no text")
	}
	if _, err := peer.Send(func(txn *crdt.Transaction) { text.Insert(txn, 0, "Edited. ", nil) }); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the room to take the browser's edit", func() bool {
		return strings.Contains(renderedLive(t, service, left), "Edited. ")
	})
	peer.Close()
	select {
	case <-settling:
	case <-time.After(10 * time.Second):
		t.Fatal("the browser left and no settlement ran")
	}
	// The browser's handler forgets it only once the settlement its leaving runs has returned, so
	// the room's state still names it, as departed; a sweep that took it for a browser holding the
	// room would leave the admission nothing to wait for.
	state := service.lockExistingState(left)
	departed := state != nil && len(state.connected) == 1 && state.onlyDepartedLocked() && !service.unusedLocked(state)
	if state != nil {
		service.unlockState(left, state)
	}
	if !departed {
		t.Fatal("while its settlement runs, the room whose browser left does not name that browser as departed")
	}
	service.SetRoomBudgetForTest(budgetUsed(service) - 1)
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })

	answered := make(chan error, 1)
	go func() {
		answered <- second(service.budget.refusal(ctx, time.Now().Add(service.admissionWait), "room", 0))
	}()
	waitFor(t, 5*time.Second, "the admission to wait", func() bool { return waiting(&service.budget) == 1 })
	select {
	case err := <-answered:
		t.Fatalf("the admission was answered %v while the room whose browser left still settled", err)
	case <-time.After(300 * time.Millisecond):
	}
	released.Do(func() { close(release) })
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the admission was answered %v once the room whose browser left had settled, want it admitted", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admission still waits after the room whose browser left settled")
	}
	if service.srv.GetDoc(left) != nil {
		t.Fatal("the admission was answered while the room whose browser left was still open")
	}
}

// An admission whose budget is short waits for the one room the sweep could close while a
// Server.Apply is inside its callback on it: that close declines (closeUnused), since the apply
// holds the room and holds nothing in the room's state that the sweep could see, and the room is
// left pending rather than the admission refused. The apply's return is what reports its end
// (roomApplied), and the sweep that follows closes the room and admits the admission.
func TestTheRoomBudgetAdmissionWaitsForARoomAnApplyIsInside(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	closable := createDocuments(t, database, 2)[0]
	appendThroughAPI(t, service, closable)
	waitFor(t, 10*time.Second, "the written room to settle and hold nothing", func() bool { return roomUnused(service, closable) })

	inside, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	applied := make(chan error, 1)
	go func() {
		applied <- service.srv.Apply(ctx, closable, func(*crdt.Doc, func(func(*crdt.Transaction))) {
			close(inside)
			<-release
		})
	}()
	select {
	case <-inside:
	case <-time.After(10 * time.Second):
		t.Fatal("the apply never reached its callback")
	}
	if !roomUnused(service, closable) {
		t.Fatal("the room an apply is inside holds something in its state, so the sweep would leave it pending without the apply")
	}
	service.SetRoomBudgetForTest(budgetUsed(service) - 1)
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })

	answered := make(chan error, 1)
	go func() {
		answered <- second(service.budget.refusal(ctx, time.Now().Add(service.admissionWait), "room", 0))
	}()
	waitFor(t, 5*time.Second, "the admission to wait", func() bool { return waiting(&service.budget) == 1 })
	select {
	case err := <-answered:
		t.Fatalf("the admission was answered %v while the apply held the only room the budget could close", err)
	case <-time.After(300 * time.Millisecond):
	}
	if service.srv.GetDoc(closable) == nil {
		t.Fatal("the budget closed the room an apply was inside")
	}

	released.Do(func() { close(release) })
	if err := <-applied; err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		t.Fatalf("the apply holding the room answered %v", err)
	}
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the admission was answered %v once the apply holding the room returned, want it admitted", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admission still waits after the apply that held the only closable room returned")
	}
	if service.srv.GetDoc(closable) != nil {
		t.Fatal("the room the apply held is still open after the admission its close made room for")
	}
}

// roomHasBrowser is whether room's state has a browser connected.
func roomHasBrowser(service *Service, room string) bool {
	state := service.lockExistingState(room)
	if state == nil {
		return false
	}
	defer service.unlockState(room, state)
	return len(state.connected) > 0
}

// A room the server's own work loads through the budget's one opening while the charges are
// about to pass its limit is closed once that work no longer holds it, though the budget is back
// within its limit and not full by then, so nothing else would close it before it retires a
// minute later. One a browser joins before the work ends is the browser's: it is not closed
// while the browser holds it, nor once the browser has left.
//
// The opening is what tells the server's own room from a browser's, now that nothing is reserved
// for an admission: the budget marks the room its opening named, and only where that room's
// charge takes the charges past the limit (roomBudget.load). So each room here is opened as
// every opener of Server.Apply opens one - the opening booked first, under a limit the room's
// own charge then crosses, and given back once the Apply has returned (publishLiveWrite,
// settleRoomWithin, VerifyMark, docTree, captureLiveTextAndAuthors) - and one stands at a time.
func TestTheRoomBudgetClosesARoomTheServerOpenedOverItOnceUnusedAndNeverABrowsersRoom(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 2)
	opened, joined := ids[0], ids[1]
	// The server's work in each room outlasts the Apply that opens it, as a settlement or a write's
	// append does: a latest edit source no settlement has read yet, recorded inside the Apply, once
	// the room's load has taken the edit source its store row names (onLoadDocument, which clears
	// one a row does not name).
	hold := func(room string, unsettled bool) {
		state := service.lockState(room)
		state.unsettled = unsettled
		service.unlockState(room, state)
	}

	for _, room := range []string{opened, joined} {
		service.SetRoomBudgetForTest(budgetUsed(service) + 1)
		if !service.budget.openServerRoom(room) {
			t.Fatal("the budget refused the server's work an opening while the charges stood under its limit")
		}
		err := service.srv.Apply(ctx, room, func(*crdt.Doc, func(func(*crdt.Transaction))) { hold(room, true) })
		service.budget.closeServerRoom()
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Fatal(err)
		}
		if !service.budget.isMarked(room) {
			t.Fatalf("the room the server opened as its charge passed the budget's limit was not marked to close once unused")
		}
	}
	service.SetRoomBudgetForTest(0)
	waitFor(t, 10*time.Second, "the budget's sweeps to end", func() bool {
		service.budget.mu.Lock()
		defer service.budget.mu.Unlock()
		return !service.budget.sweeping
	})
	if service.budget.full.Load() || service.srv.GetDoc(opened) == nil || service.srv.GetDoc(joined) == nil {
		t.Fatalf("with the budget back within its limit, full is %t and the rooms are open: %t, %t; want full down and both open while the work holds them",
			service.budget.full.Load(), service.srv.GetDoc(opened) != nil, service.srv.GetDoc(joined) != nil)
	}

	connection, err := dialDocument(httpServer.URL, joined)
	if err != nil {
		t.Fatalf("open the room the server opened: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	waitFor(t, 10*time.Second, "the browser to join", func() bool { return roomHasBrowser(service, joined) })
	if service.budget.isMarked(joined) {
		t.Fatal("a room a browser joined is still marked to close once the server's work ends")
	}

	hold(opened, false)
	waitFor(t, 10*time.Second, "the room the server opened over the budget to close once unused", func() bool { return service.srv.GetDoc(opened) == nil })
	hold(joined, false)
	leaveDocument(connection)
	waitFor(t, 10*time.Second, "the browser to leave", func() bool { return !roomHasBrowser(service, joined) })
	time.Sleep(300 * time.Millisecond)
	if service.srv.GetDoc(joined) == nil {
		t.Fatal("the budget closed the room a browser had joined once the server's work in it ended")
	}
}

// settlementOwed is whether the document still owes a settlement, which the settlement that has
// read every one of its updates deletes as it commits (clearSettlementPending).
func settlementOwed(t *testing.T, service *Service, room string) bool {
	t.Helper()
	var owed bool
	if err := service.store.Pool.QueryRow(context.Background(),
		`select exists(select 1 from doc_settlements_pending where artifact_id = $1)`, room).Scan(&owed); err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	return owed
}

// A committed write to a document no room holds publishes into no room while the rooms already
// open leave the budget no opening for one (roomBudget.openServerRoom): the budget's charges
// stand where they stood, and the write is applied all the same, its update and its version
// having been durable before the publish began. The settlement it leaves owed is what cannot do
// without a room, so it waits for the budget's one opening whatever the charges read
// (awaitServerRoom), warms its room over the full budget and settles the document, and the
// budget closes that room as soon as the settlement has ended (closeIfOpenedOverBudget). With an
// opening to be had, a publish opens its room as such a publish always has.
//
// Before the opening every such publish loaded its room whatever the budget held, charged it to a
// budget already full, and left it charged until the settlement the write armed had run: a burst
// of three hundred of them over a full budget took the pod 713,885,218 bytes past its Go memory
// limit (cmd/dispatch, TestABurstOfConcurrentLiveWritesStaysUnderThePodsMemory).
func TestAWritePublishesIntoNoRoomWhileTheBudgetHasNoOpeningForOne(t *testing.T) {
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	// Long enough that the settlement the write owes does not run while the publish is weighed;
	// the test runs it itself below (SettleNow).
	service.settle = time.Minute
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, 3)
	held, cold, opened := ids[0], ids[1], ids[2]

	// One room a browser holds, which no sweep may close, and a budget its charge fills exactly.
	browser := connectBrowser(t, httpServer.URL, held, "alice")
	t.Cleanup(browser.Close)
	waitFor(t, 10*time.Second, "the browser's room to be charged", func() bool { return budgetUsed(service) > 0 })
	limit := service.SetRoomBudgetForTest(budgetUsed(service))
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })

	appendThroughAPI(t, service, cold)
	if service.srv.GetDoc(cold) != nil {
		t.Fatal("the publish of a write to a document no room holds opened its room while the budget had no opening for one")
	}
	if used := budgetUsed(service); used != limit {
		t.Fatalf("the open rooms are charged %d bytes of the %d-byte budget after the write, want the charges left where they stood", used, limit)
	}
	// The paragraph the write appended beside the one the document was created holding, in the
	// store, which held both before the publish began.
	if stored, want := storedMarkdown(t, database, cold), 2; strings.Count(stored, "Room cap probe.") != want {
		t.Fatalf("the written document reads %q, want the %d paragraphs of a write that was applied though it opened no room", stored, want)
	}
	if !settlementOwed(t, service, cold) {
		t.Fatal("the written document owes no settlement, want the write to have armed the one its publish could not")
	}

	// The settlement takes the budget's opening though the budget is full, settles the document,
	// and the room it warmed is closed once it has: it was opened over the limit, so the budget
	// owes it that close.
	service.SettleNow(cold)
	waitFor(t, 30*time.Second, "the document to settle in the room its settlement warms", func() bool {
		return !settlementOwed(t, service, cold)
	})
	waitFor(t, 30*time.Second, "the budget to close the room its settlement opened over it", func() bool {
		return service.srv.GetDoc(cold) == nil
	})
	waitFor(t, 30*time.Second, "the budget's charges to fall back to what the browser's room holds", func() bool {
		return budgetUsed(service) == limit
	})

	// A write with an opening to be had publishes into the room it opens, as every such write did
	// before the budget booked openings at all.
	service.SetRoomBudgetForTest(0)
	appendThroughAPI(t, service, opened)
	if service.srv.GetDoc(opened) == nil {
		t.Fatal("the publish of a write to a document no room holds opened no room, want the room such a publish opens while the budget has an opening for one")
	}
}

// A burst of API reads of documents no room holds opens no room while the budget has no opening
// for one: each read is answered from the stored document instead (docTree, storedTree), and the
// budget's charges stand where they stood. Without the opening each of those reads loaded a room
// of its own, all of them at once, charged to a budget that refuses the server's own work
// nothing.
func TestABurstOfReadsOfColdDocumentsOpensNoRoomWhileTheBudgetHasNoOpeningForOne(t *testing.T) {
	const reads = 12
	ctx := context.Background()
	database := storetest.Open(t)
	service := newRoomBudgetService(t, database, NewPgVersioned(database))
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	ids := createDocuments(t, database, reads+1)
	held, cold := ids[0], ids[1:]

	browser := connectBrowser(t, httpServer.URL, held, "alice")
	t.Cleanup(browser.Close)
	waitFor(t, 10*time.Second, "the browser's room to be charged", func() bool { return budgetUsed(service) > 0 })
	limit := service.SetRoomBudgetForTest(budgetUsed(service))
	t.Cleanup(func() { service.SetRoomBudgetForTest(0) })

	blocks := make([]string, reads)
	errs := make([]error, reads)
	var group sync.WaitGroup
	start := make(chan struct{})
	for index, room := range cold {
		group.Go(func() {
			<-start
			blocks[index], errs[index] = service.BlockForQuote(ctx, room, "Room cap probe.")
		})
	}
	close(start)
	group.Wait()
	for index, err := range errs {
		if err != nil || blocks[index] == "" {
			t.Fatalf("read %d of a document no room holds answered %q (%v), want the block holding its quote", index, blocks[index], err)
		}
	}
	for index, room := range cold {
		if service.srv.GetDoc(room) != nil {
			t.Fatalf("read %d opened a room for document %s while the budget had no opening for one", index, room)
		}
	}
	if used := budgetUsed(service); used != limit {
		t.Fatalf("the open rooms are charged %d bytes of the %d-byte budget after %d reads, want the charges left where they stood", used, limit, reads)
	}

	// With an opening to be had, a read loads the room as such a read always has.
	service.SetRoomBudgetForTest(0)
	if _, err := service.BlockForQuote(ctx, cold[0], "Room cap probe."); err != nil {
		t.Fatalf("a read with an opening to be had: %v", err)
	}
	if service.srv.GetDoc(cold[0]) == nil {
		t.Fatal("a read of a document no room holds opened no room, want the room such a read opens while the budget has an opening for one")
	}
}

// The opening the server's own work books is weighed against the charge of the room the opening
// before it made, not against the budget as it stood before that room loaded: the load charges
// the room inside the Apply that opens it (chargeLoadedRoom, roomBudget.load), which returns
// before its opener gives the opening back (closeServerRoom), so the next openServerRoom reads a
// used that already holds it. One room past the limit is therefore all the charges can stand
// past it.
func TestAnOpeningIsWeighedAgainstTheRoomTheOpeningBeforeItMade(t *testing.T) {
	budget := &roomBudget{limit: 1_000}
	if !budget.openServerRoom("opened") {
		t.Fatal("the first opening of an empty budget was refused")
	}
	if budget.openServerRoom("another") {
		t.Fatal("a second opening was booked while the first stood")
	}
	// What the load does inside the Apply the opening covers, before the opening is given back:
	// a room of 40 bytes of state, charged roomWeight(40) = 1,120, past this budget's limit.
	budget.load("opened", 40)
	budget.closeServerRoom()
	if used := budget.used; used <= budget.limit {
		t.Fatalf("the room the opening made is charged %d of %d bytes, want this test's room to fill the limit", used, budget.limit)
	}
	if budget.openServerRoom("another") {
		t.Fatalf("an opening was booked with %d of %d bytes charged, want it refused against the room the opening before it made", budget.used, budget.limit)
	}
	budget.retire("opened")
	if !budget.openServerRoom("another") {
		t.Fatal("an opening was refused once the room before it retired")
	}
	budget.closeServerRoom()
}

// newRoomBudgetService is a document service over persist whose rooms stay resident a minute after
// their last use, as production's do, so what closes one in these tests is the budget's sweep.
func newRoomBudgetService(t *testing.T, database *store.Store, persist VersionedStore) *Service {
	t.Helper()
	service := New(Deps{
		Store:       database,
		Persistence: persist,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service
}

// admissionWatchStore records, at each load of a document's stored state (the socket's admission
// check, loadTree, and the room's own), what the room budget charges that document, and fails the
// loads of the document failing names.
type admissionWatchStore struct {
	VersionedStore
	service atomic.Pointer[Service]
	failing atomic.Value
	mu      sync.Mutex
	charged map[string][]int64
}

func (s *admissionWatchStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	budget := &s.service.Load().budget
	budget.mu.Lock()
	held := int64(0)
	if entry := budget.rooms[room]; entry != nil {
		for _, charge := range entry.charges {
			held += charge.weight
		}
	}
	budget.mu.Unlock()
	s.mu.Lock()
	s.charged[room] = append(s.charged[room], held)
	s.mu.Unlock()
	if failing, _ := s.failing.Load().(string); failing == room {
		return persistence.LoadResult{}, errors.New("injected load failure")
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s *admissionWatchStore) loads(room string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.charged[room]...)
}

// requireChargedAtLoad requires the admission check's load of room, its first, to have found want
// bytes charged for that document: nothing, until its room's own load has decoded the state the
// budget charges it for.
func (s *admissionWatchStore) requireChargedAtLoad(t *testing.T, room string, want int64) {
	t.Helper()
	loads := s.loads(room)
	if len(loads) == 0 || loads[0] != want {
		t.Fatalf("the admission's load found %v bytes charged, want %d first", loads, want)
	}
}

// requireClosedAtUpgrade opens room's socket and requires the server to complete the upgrade and
// close it at once with code and reason, before any sync.
func requireClosedAtUpgrade(t *testing.T, serverURL, room string, code int, reason string) {
	t.Helper()
	connection, err := dialDocument(serverURL, room)
	if err != nil {
		t.Fatalf("open the document: %v, want an upgrade the server closes", err)
	}
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, message, err := connection.ReadMessage()
	var closed *gws.CloseError
	if !errors.As(err, &closed) || closed.Code != code || closed.Text != reason {
		t.Fatalf("first frame = %q (%v), want close %d %q before any sync", message, err, code, reason)
	}
}

// requireNothingCharged requires the budget to charge nothing once the handlers the test drove
// have returned: a refusing handler returns once the client answers its close, a moment after the
// client has read it.
func requireNothingCharged(t *testing.T, service *Service) {
	t.Helper()
	waitFor(t, 5*time.Second, "the budget to charge nothing", func() bool { return budgetUsed(service) == 0 })
}

func budgetUsed(service *Service) int64 {
	service.budget.mu.Lock()
	defer service.budget.mu.Unlock()
	return service.budget.used
}

// roomUnused is whether room's state holds nothing that outlasts the room (unusedLocked).
func roomUnused(service *Service, room string) bool {
	state := service.lockExistingState(room)
	if state == nil {
		return false
	}
	defer service.unlockState(room, state)
	return service.unusedLocked(state)
}

// renderedLive is the rendering of room's resident document, which its load counted.
func renderedLive(t *testing.T, service *Service, room string) string {
	t.Helper()
	markdown, err := renderDocument(service.srv.GetDoc(room))
	if err != nil {
		t.Fatal(err)
	}
	return markdown
}

// second is the error of a two-value answer, for a goroutine that sends only the error on a
// channel (roomBudget.refusal, whose reservation such a test does not hold).
func second(_ *roomReservation, err error) error { return err }
