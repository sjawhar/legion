package docs

import (
	"log/slog"

	"github.com/reearth/ygo/crdt"
)

// This file holds the service's side of the room budget: the charge a room's load takes on
// (chargeLoadedRoom), and the sweep that closes the rooms the budget cannot hold
// (sweepRooms, closeIfOpenedOverBudget, roomApplied).

// chargeLoadedRoom charges room's newly loaded instance, live, whose replica has just published
// its load's count (keepReplica), in place of the reservations of the sockets admitted to it, and
// hands the replica the charge its counts re-base from then on (renderedReplica.storeCounted). The
// load is priced from that count, the rendering's bytes and the struct total the replica counted
// at the load (newRoomTotals), whichever is the most (copyHeap). The charge lists live's delete
// set as the ranges the room holds deleted (deletedRuns) before the replica hands it to the peer
// hook: a walk of the document's whole store, beside the load's rendering of it, which is not a
// keystroke.
func (s *Service) chargeLoadedRoom(room string, live *crdt.Doc, replica *renderedReplica) *roomCharge {
	bytes, elements, _ := replica.size()
	charge := s.budget.load(room, copyHeap(bytes, elements, replica.structs.countedStructs()))
	charge.deleted.seed(crdt.DeleteSetFromDoc(live))
	replica.charge.Store(charge)
	return charge
}

// sweepRooms is the budget's sweep (roomBudget.sweep): while the budget is short (roomBudget.short)
// - over its limit, or without room for what the admissions waiting on it need (wanted) - it
// closes the charged rooms that hold nothing outlasting them, the heaviest first, so as few as it
// can, through the eviction a failed room takes (evictRoom), whose close flushes the room before
// it goes. Unused is the service's own test (unusedLocked), not the weaker "no browser
// connected": a room with no browser can still hold an open writer, an armed settlement or an
// update its persistence has not appended. A room still loading is not closed either: its load
// has not returned, and the opener waiting on it would find it gone. The close's unload returns
// the room's charge (releaseUnloadedRoom), and ygo's CloseRoom fires that unload before it returns
// wherever it is the one that removed the room (sjawhar/ygo v1.54.1-sami.3,
// provider/websocket/inject.go:625-641), so the next look at the budget sees the charge gone. Each
// close is logged with the budget's own figures before and after it, which is how an operator, or
// a test of the real process, tells a room the budget closed from one that retired on its own.
//
// A room it leaves because something other than a connected browser holds it - a settlement, a
// writer, an append, a load - is pending: it will be closable once that ends, which starts a sweep
// of its own (Service.unlockState), so the admissions waiting on the budget wait for it (await).
// So is a room whose browsers have all gone though their handlers have not yet forgotten them
// (roomState.departedPeers): its last browser's leaving runs the settlement it owes before they do.
// Once nothing more can be closed it raises full if the budget is still over its limit
// (roomBudget.swept).
//
// It runs on a goroutine of its own, as failRoomLocked's eviction does, which Shutdown joins
// (evictWG); a service stopping sweeps nothing, since its Shutdown closes every room itself.
func (s *Service) sweepRooms() {
	pending := false
	defer func() { s.budget.swept(pending) }()
	if !s.addUnlessStopping(&s.evictWG) {
		return
	}
	defer s.evictWG.Done()
	for _, room := range s.budget.heaviest() {
		if !s.budget.short() {
			return
		}
		state := s.lockExistingState(room)
		if state == nil {
			pending = true
			continue
		}
		if s.srv.GetDoc(room) == nil || !s.unusedLocked(state) {
			// Left as it is: unlockState's look at the budget would only ask for this sweep again.
			pending = pending || state.onlyDepartedLocked()
			state.mu.Unlock()
			continue
		}
		// As Evict does: a settlement read under the generation before the close is one the close
		// has invalidated. The state is unlocked without unlockState's look at the budget, which
		// would take this room, unused, for one to sweep or close again.
		state.roomGeneration++
		state.mu.Unlock()
		before, _, _ := s.budget.figures()
		closed, _ := s.evictUnusedRoom(room, state)
		if !closed {
			// An Apply is inside its callback on the room (closeUnused): the room will be
			// closable once that apply ends, which hands it to closeIfOpenedOverBudget and kicks
			// the budget, so the admissions waiting on it wait for that rather than being refused.
			pending = true
			continue
		}
		after, limit, wanted := s.budget.figures()
		slog.Info("dispatch: the room budget closed an unused document",
			"room", room, "charged_before", before, "charged", after, "limit", limit, "making_room_for", wanted)
	}
}

// closeIfOpenedOverBudget closes room once it is unused (unusedLocked) when its charge is marked
// closeOnceUnused: a room the server's own work loaded while the budget was over its limit
// (roomBudget.load), which no browser has joined since (roomBudget.joinedLocked). It is called
// whenever room's state may have stopped holding anything (Service.unlockState) and once each
// Server.Apply on it has returned (roomServer.Apply), which between them see the end of every
// opener's use: a writer's, a settlement's and an append's end in the state, a read's in its
// Apply. A room still loading, or one a browser holds, is left: a browser connected is a holder
// unusedLocked names, so a room a browser holds is never closed this way. The close is the
// eviction the sweep makes (evictUnusedRoom), on a goroutine of its own, which Shutdown joins
// (evictWG), and is logged with the budget's figures before and after it as the sweep's are. A
// room an Apply is inside its callback on is left marked, for the end of that Apply to look at
// again.
func (s *Service) closeIfOpenedOverBudget(room string) {
	if s.budget.marked.Load() == 0 || !s.budget.isMarked(room) {
		return
	}
	state := s.lockExistingState(room)
	if state == nil {
		return
	}
	if state.released || s.srv.GetDoc(room) == nil || !s.unusedLocked(state) || !s.budget.takeMark(room) {
		state.mu.Unlock()
		return
	}
	state.roomGeneration++
	state.mu.Unlock()
	if !s.addUnlessStopping(&s.evictWG) {
		return
	}
	go func() {
		defer s.evictWG.Done()
		before, _, _ := s.budget.figures()
		closed, _ := s.evictUnusedRoom(room, state)
		if !closed {
			s.budget.markAgain(room)
			return
		}
		after, limit, _ := s.budget.figures()
		slog.Info("dispatch: the room budget closed a document the server opened over it",
			"room", room, "charged_before", before, "charged", after, "limit", limit)
	}()
}

// roomApplied is what the service does with a room once a Server.Apply on it has returned
// (roomServer.applied). An apply holds the room against the budget's closes while it is inside
// its callback and holds nothing in the room's state, so those closes give up in front of it
// (closeUnused) and nothing else reports its end: this is where a room the sweep passed over, and
// one the server opened over the budget, are looked at again. A room the budget does not charge
// starts no sweep (roomBudget.kick) and carries no mark.
func (s *Service) roomApplied(room string) {
	s.budget.kick(room)
	s.closeIfOpenedOverBudget(room)
}
