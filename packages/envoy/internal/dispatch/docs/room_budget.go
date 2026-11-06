package docs

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/reearth/ygo/provider/websocket"
)

// peerlessRoomHeapBudget is how much heap Dispatch keeps in rooms for documents no browser peer is
// connected to. roomIdleTimeout bounds such a room's residency in time alone, so what they hold
// together is whatever the load puts there, and a room holds two decoded copies of its document:
// the room's own and the replica its update observer renders (renderedReplica). At the end of the
// load test's 10x minute (packages/envoy/loadtest, 0.5x:60s,10x:60s,0.5x:120s) those were 391 to
// 447 MB of room documents and 327 to 398 MB of replicas over about 200 resident rooms, and the
// live heap was still 1.2 to 1.4 GiB a minute after the burst, where each garbage collection's
// marking cost the whole process seconds of latency (a four-second patch of slow requests at the
// base tree, requests answered at 30 to 46 a second throughout, while the heap grew from 1,159 to
// 1,491 MB between two cycles) and left requests at a 1.0 to 2.1 s p99 a minute after the load
// had dropped.
//
// 256 MiB is the cold reads' cache's budget (documentReadBudget), the other bound of this size in
// the service, inside a production task's 4,096 MiB (cmd/dispatch/memory_test.go). It is a budget
// of bytes rather than of rooms because a room's cost is its document's size, which spans two
// orders of magnitude across a project's documents: a count that fits the big ones wastes the
// heap on the small, and one that fits the small refuses to hold a working set of either. A
// budget of bytes also tightens itself exactly when it must - the burst grows the documents it
// writes, so the same budget holds fewer of them.
const peerlessRoomHeapBudget = 256 << 20

// roomHeapPerStoredByte is how much heap a resident room costs for each byte of the stored state
// its load decoded, and roomHeapFloor what one costs whatever that state's size. Neither ygo nor
// the replica measures a document's heap, so the budget estimates it from the one number every
// load has: the bytes it decoded (bootstrapLoadedState). The factor is measured, at the load
// test's recovery step: about 3.6 to 4.2 MB of heap a room - its document and its replica - over
// documents whose stored histories average 77 KiB and whose median is 45 KiB (2,310 documents of
// that seed). It is an estimate in one direction only: a room grows past it as writes land in it
// and shrinks back when it is closed and loaded again from the compacted store, so a room that a
// burst has written is heavier than its weight says, and the budget holds fewer rooms than its
// bytes suggest rather than more.
const (
	roomHeapPerStoredByte = 44
	roomHeapFloor         = 128 << 10
)

// roomTrimInterval is how often the rooms are weighed. The trim walks every resident room under
// its state's lock, so it is not run on every Apply of a burst; at the room-load rate the load
// test measured (4 to 13 a second) a tenth of a second puts at most one or two rooms past the
// budget before the next pass takes them.
const roomTrimInterval = 100 * time.Millisecond

// roomBudgetCloseWait is how long one close of the trim waits for a room a caller is inside before
// it leaves that room to the next pass (roomServer.closeWhenFree retries while ErrRoomInUse comes
// back). A caller inside a room leaves within milliseconds, so this is reached only by a room the
// load keeps busy, which the next pass finds among the most recently used and inside the budget.
const roomBudgetCloseWait = 2 * time.Second

// roomBudgetClosers is how many of the rooms over the budget one trim closes at a time. A close
// spends its time waiting - for the callers inside the room to leave, and for the room's own
// persistence worker to flush what it holds and exit, which ygo's CloseRoom waits for - rather
// than doing the work itself, so closing them one after another is as slow as the slowest room:
// measured on the load test's recovery step, one closer held about 4 closes a second against a
// room-load rate of 4 to 13 a second and never brought the resident rooms back to the budget at
// all, where four reach it. Four is also what settles at once (settlementWorkers), the other
// background work of the service that writes through the store.
const roomBudgetClosers = 4

// noteRoomUsed stamps room as used now, for the budget's order (peerlessRoomsOverBudget), and
// trims the peerless rooms the budget no longer holds. Its callers are the three moments a room
// becomes, or stops being, one of the rooms the budget counts: its load (onLoadDocument), the
// return of a Server.Apply on it (roomServer.Apply), and a browser leaving it (removeConnection).
// A document the service holds no state for is resident in no room of ours, so there is nothing to
// stamp.
func (s *Service) noteRoomUsed(room string) {
	if state := s.lockExistingState(room); state != nil {
		state.lastUsed = s.now()
		s.unlockState(room, state)
	}
	s.trimPeerlessRooms()
}

// trimPeerlessRooms asks for a close of the peerless rooms over the budget. One trim runs at a
// time, on a goroutine of its own, because a close flushes the room through the store and waits
// for the callers inside it: the write or the load that noted the room used must not pay for that,
// and a trim run inside a room's load hook would wait for another room's load through ygo's
// CloseRoom while holding its own.
//
// No ask is dropped, however many arrive while one trim runs: the ask is recorded first
// (trimPending) and the trim loops until it finds none recorded, so the rooms a burst loaded while
// the last pass was closing are weighed by the next. Each pass waits out roomTrimInterval from the
// last pass's weighing before it weighs the rooms, which is what keeps a burst's Applies from
// walking every room's state between two writes, and which puts every ask that arrives in that
// window into the pass that follows it.
//
// The trim counts as an eviction for Shutdown (evictWG), which waits for the closes already
// running and takes the rooms from there, and starts none once the service is stopping or
// quiescing: those two empty the rooms themselves, and a close racing them buys nothing.
func (s *Service) trimPeerlessRooms() {
	if s.peerlessRoomBytes.Load() <= 0 || s.stopping.Load() || s.quiescing.Load() {
		return
	}
	s.trimPending.Store(true)
	if !s.trimmingRooms.CompareAndSwap(false, true) {
		return
	}
	if !s.addUnlessStopping(&s.evictWG) {
		s.trimmingRooms.Store(false)
		return
	}
	go func() {
		defer s.evictWG.Done()
		defer s.trimmingRooms.Store(false)
		for s.trimPending.Swap(false) {
			if wait := roomTrimInterval - s.now().Sub(time.Unix(0, s.lastRoomTrim.Load())); wait > 0 {
				time.Sleep(wait)
			}
			if s.stopping.Load() || s.quiescing.Load() {
				return
			}
			s.lastRoomTrim.Store(s.now().UnixNano())
			s.closeRoomsOverBudget()
		}
	}()
}

// closeRoomsOverBudget closes the peerless rooms the budget does not hold, least recently used
// first, through the close every eviction of the service goes through (roomServer.closeRoom, as
// Evict), which flushes the room's updates to the store before ygo retires it, waits for its
// persistence worker to exit and fires OnUnloadDocument, so the room's state goes with it
// (releaseUnloadedRoom) unless something still holds it - the settlement it owes among them, which
// runs on its own timer and warms the document again, as it does for a room the idle sweeper
// evicted. The close is unforced, so a room a browser joined between the list and the close keeps
// its peer and stays.
func (s *Service) closeRoomsOverBudget() {
	over := s.peerlessRoomsOverBudget()
	if len(over) == 0 {
		return
	}
	rooms := make(chan string)
	var closers sync.WaitGroup
	for range min(roomBudgetClosers, len(over)) {
		closers.Add(1)
		go func() {
			defer closers.Done()
			for room := range rooms {
				s.closeRoomOverBudget(room)
			}
		}()
	}
	for _, room := range over {
		if s.stopping.Load() || s.quiescing.Load() {
			break
		}
		rooms <- room
	}
	close(rooms)
	closers.Wait()
}

// closeRoomOverBudget closes one room the budget does not hold and counts it, unless the close
// found no room of that name to close or something else is inside it.
func (s *Service) closeRoomOverBudget(room string) {
	ctx, cancel := context.WithTimeout(context.Background(), roomBudgetCloseWait)
	defer cancel()
	switch err := s.srv.closeRoom(ctx, room, false); {
	case err == nil:
		s.roomsClosedToBudget.Add(1)
	// The room has gone, a browser is in it, or a caller was inside it for the whole wait: each
	// leaves this pass with one room fewer to close, and the next pass reads the rooms as they
	// stand then.
	case errors.Is(err, websocket.ErrRoomNotFound),
		errors.Is(err, websocket.ErrRoomHasPeers),
		errors.Is(err, websocket.ErrServerShutdown),
		errors.Is(err, context.DeadlineExceeded):
	default:
		slog.Warn("dispatch: close a room over the resident-room budget", "room", room, "error", err)
	}
}

// peerlessRoom is one resident room with no browser connected: what the budget counts, when it was
// last used, and the heap its load says it holds (roomState.weight).
type peerlessRoom struct {
	room   string
	used   time.Time
	weight int64
}

// peerlessRooms is the resident rooms no browser is connected to, each with its weight.
//
// A room still loading is not one of them (ygo answers GetDoc nil until its load hook has returned
// and its document is nobody's to read), and nor is a room with a browser connected, one whose
// state carries a failure its own eviction has to close, one a close of the service's own has
// reserved (reserveRoomCloseLocked), or one a live write holds its writer slot on: the write
// publishes its committed update into the room it forked from, and a close between the two would
// make it reload and take the cold path for nothing. Those last three are bounded by what can run
// at once - the admitted writes and the evictions in flight - so they put a handful of rooms past
// the budget, never a burst's worth.
func (s *Service) peerlessRooms() []peerlessRoom {
	var peerless []peerlessRoom
	for _, room := range s.srv.Rooms() {
		if s.srv.GetDoc(room) == nil {
			continue
		}
		state := s.lockExistingState(room)
		if state == nil {
			continue
		}
		if len(state.connected) == 0 && state.failed == nil && !state.reloading && state.liveWriter == nil {
			peerless = append(peerless, peerlessRoom{room: room, used: state.lastUsed, weight: state.weight})
		}
		s.unlockState(room, state)
	}
	return peerless
}

// peerlessRoomsOverBudget is the peerless rooms whose weight the budget does not hold, least
// recently used first: the most recently used are kept until the budget is full, and the rest -
// the oldest - are the rooms a close brings the heap back inside it.
func (s *Service) peerlessRoomsOverBudget() []string {
	peerless := s.peerlessRooms()
	slices.SortFunc(peerless, func(a, b peerlessRoom) int { return b.used.Compare(a.used) })
	held := int64(0)
	for i, candidate := range peerless {
		held += candidate.weight
		if held > s.peerlessRoomBytes.Load() {
			over := make([]string, 0, len(peerless)-i)
			// Oldest first, so a pass cut short by a shutdown has closed the rooms least
			// likely to be wanted again.
			for _, dropped := range slices.Backward(peerless[i:]) {
				over = append(over, dropped.room)
			}
			return over
		}
	}
	return nil
}

// roomWeight is the heap a room whose load decoded stored is taken to hold, its document and its
// replica together (roomHeapPerStoredByte).
func roomWeight(stored []byte) int64 {
	return max(int64(len(stored))*roomHeapPerStoredByte, roomHeapFloor)
}

// ResidentRoomCount is how many documents this process holds a live room for, loaded or loading
// (dispatch_doc_rooms_resident). The test hooks' ResidentRooms names them.
func (s *Service) ResidentRoomCount() int64 {
	return int64(len(s.srv.Rooms()))
}

// PeerlessRooms is how many of the resident rooms have no browser connected - the rooms the budget
// counts (dispatch_doc_rooms_peerless) - and PeerlessRoomBytes the heap they are taken to hold,
// which the budget bounds (dispatch_doc_rooms_peerless_bytes). RoomsClosedToBudget is how many
// rooms the budget has closed since the process started
// (dispatch_doc_rooms_budget_closed_total). Together they say whether the budget is holding and
// what it costs.
func (s *Service) PeerlessRooms() int64 {
	return int64(len(s.peerlessRooms()))
}

func (s *Service) PeerlessRoomBytes() int64 {
	held := int64(0)
	for _, room := range s.peerlessRooms() {
		held += room.weight
	}
	return held
}

func (s *Service) RoomsClosedToBudget() int64 {
	return s.roomsClosedToBudget.Load()
}
