package docs

import (
	"log/slog"
)

// This file holds the service's side of the room budget: the charge a room's load takes on
// (chargeLoadedRoom), the re-base a compaction gives it (rebaseRoomState), and the sweep that
// closes the rooms the budget cannot hold (sweepRooms, closeIfOpenedOverBudget, roomApplied).

// chargeLoadedRoom charges room's newly loaded instance, whose replica has just published its
// load's rendering (keepReplica), at stateBytes, the bytes of the stored state that load
// decoded, and hands the replica the charge its drains raise from then on
// (renderedReplica.chargeAdded). The state is the bytes already in hand: the load read them,
// decoded them into the room and decoded them again into the replica, so the budget charges the
// room what its two copies were built from without encoding either (roomWeight).
func (s *Service) chargeLoadedRoom(room string, replica *renderedReplica, stateBytes int64) *roomCharge {
	charge := s.budget.load(room, stateBytes)
	replica.state.Store(stateBytes)
	replica.charge.Store(charge)
	return charge
}

// rebaseRoomState re-bases room's tracked state on stateBytes, what a compaction that folded the
// room's whole stored history into one row wrote (compactResult), and charges the room again
// from it. It is the one thing that lowers a room's tracked state while the room stands: each
// drain adds its update's bytes, which carry what that transaction deleted beside what it added,
// so a room typed into for hours is tracked above the state it holds until a compaction reads
// the state itself. A room this server does not hold, or one no load listed, is left alone.
//
// A drain landing between the compaction's fold and this one is lost to the re-base, which
// under-states the room by that one update until its next drain adds more. That is accounting
// and not a refusal, the window is one update long, and the room's next load re-bases it exactly.
func (s *Service) rebaseRoomState(room string, stateBytes int64) {
	live := s.srv.GetDoc(room)
	if live == nil {
		return
	}
	weighing := s.roomWeighingOf(live)
	if weighing == nil {
		return
	}
	weighing.state.Store(stateBytes)
	weighing.roomCharge().weigh(stateBytes)
}

// sweepRooms is the budget's sweep (roomBudget.sweep): while the budget is over its limit
// (roomBudget.short) it
// closes the charged rooms that hold nothing outlasting them, the heaviest first, so as few as it
// can, through the eviction a failed room takes (evictRoom), whose close flushes the room before
// it goes. Unused is the service's own test (unusedLocked), not the weaker "no browser
// connected": a room with no browser can still hold an open writer, an armed settlement or an
// update its persistence has not appended. A room still loading is not closed either: its load
// has not returned, and the opener waiting on it would find it gone. The close's unload returns
// the room's charge (releaseUnloadedRoom), and ygo's CloseRoom fires that unload before it returns
// wherever it is the one that removed the room (sjawhar/ygo v1.55.1-sami.1,
// provider/websocket/inject.go:695-711), so the next look at the budget sees the charge gone. Each
// close is logged with the budget's own figures before and after it, which is how an operator, or
// a test of the real process, tells a room the budget closed from one that retired on its own.
//
// A room it leaves because something other than a connected browser holds it - a settlement, a
// writer, an append, a load - is pending: it will be closable once that ends, which starts a sweep
// of its own (Service.unlockState), so an admission waiting on the budget waits for it (await).
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
			slog.Info("dispatch: the room budget's sweep found no state for a charged room",
				"room", room)
			continue
		}
		if s.srv.GetDoc(room) == nil || !s.unusedLocked(state) {
			// Left as it is: unlockState's look at the budget would only ask for this sweep again.
			// The line names what holds it, which is the one reading that says why a sweep over a
			// budget past its limit closed nothing: a browser still connected, a browser gone
			// whose handler has not forgotten it, the room's own work (a settlement, a writer, an
			// append), or no room at all (INFRA-179).
			pending = pending || state.onlyDepartedLocked()
			slog.Info("dispatch: the room budget's sweep left a charged room open",
				"room", room, "held_by", s.holdingLocked(state), "pending", pending)
			state.mu.Unlock()
			continue
		}
		// As Evict does: a settlement read under the generation before the close is one the close
		// has invalidated. The state is unlocked without unlockState's look at the budget, which
		// would take this room, unused, for one to sweep or close again.
		state.roomGeneration++
		state.mu.Unlock()
		before, _ := s.budget.figures()
		closed, _ := s.evictUnusedRoom(room, state)
		if !closed {
			// An Apply is inside its callback on the room (closeUnused): the room will be
			// closable once that apply ends, which hands it to closeIfOpenedOverBudget and kicks
			// the budget, so the admissions waiting on it wait for that rather than being refused.
			pending = true
			continue
		}
		after, limit := s.budget.figures()
		slog.Info("dispatch: the room budget closed an unused document",
			"room", room, "charged_before", before, "charged", after, "limit", limit)
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
		before, _ := s.budget.figures()
		closed, _ := s.evictUnusedRoom(room, state)
		if !closed {
			s.budget.markAgain(room)
			return
		}
		after, limit := s.budget.figures()
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
