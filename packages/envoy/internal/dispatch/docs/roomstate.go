package docs

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// roomState is what the service keeps for one document while ygo holds a room for it or something
// on the document still holds the state (unusedLocked names every holder), and no longer.
//
// Every lookup takes a state through lockState, or lockExistingState when it has nothing to record
// for a document without one, and ends with unlockState. The lookups never hand out a released
// state, so nothing is written to a state the service has forgotten, and unlockState releases a
// state that holds nothing once its room has gone (releaseIfUnusedLocked). So whatever ends a
// holder releases the state through its own unlock, and ygo's OnUnloadDocument releases it when
// the room is the last thing to go (releaseUnloadedRoom).
//
// What a release drops - the unrecorded marks' first sightings and the closed flag - is what a
// restart drops too.
type roomState struct {
	mu        sync.Mutex
	connected map[uint64]model.Actor
	// inflight is F (pending_authors.go): each browser edit's credit from the moment the room's
	// update observer records it (creditContentChange) until the edit's append lands, keyed by its
	// creditSeq. creditSeq is atomic so forkLive, which every document write calls, reads it for
	// forkSeq without taking the state lock; captures, an upload's included, read it to bound the
	// credits they take, and closing an issue compares it as a change token. It is process-local
	// and orders only this state's own in-flight credits.
	inflight  map[uint64]*inflightCredit
	creditSeq atomic.Uint64
	// askBlocks are the ask blocks the room's document held when its update observer last
	// rendered it, or when it loaded; nil when neither could read it. askAuthors names the author
	// of the update that introduced each one no settlement has indexed yet (observeAskBlocks).
	// pendingAskAuthors credits a committed transaction's write or a service mutation with the ask
	// ids it introduces, registered at the write site before the update can reach any observer
	// (registerAskAuthors, registerCarriedAskAuthors); observeAskBlocks consumes each entry into
	// askAuthors the first time it sees the id and never prunes one it has not consumed, so
	// attribution survives however many updates land before an observer finally renders the id
	// (LEGION-503).
	askBlocks         map[string]struct{}
	askAuthors        map[string]model.Actor
	pendingAskAuthors map[string]model.Actor
	// lastActor is the most recent edit's source: the actor of a service mutation, or the sole
	// connected peer of a browser edit. A version lists its authors, so a settlement that runs
	// after an edit's own version was committed would otherwise attribute the block asks it
	// indexes to nobody.
	lastActor *model.Actor
	// unsettled marks a lastActor no settlement has read since: the settlement it was recorded
	// for names it, so the state holds it until one commits (settleRoomWithin). Closing persists
	// it on the pending-settlement row before clearing it (SetIssueClosed), so that row carries it
	// through a room release or restart. The authors themselves are in F and R, never only here.
	unsettled      bool
	updateClasses  []documentUpdateClass
	pendingUpdates int
	settle         *time.Timer
	unrecorded     map[pmdoc.MarkRef]time.Time
	durableAppends atomic.Int64
	// roomGeneration moves at each of this room's own in-process invalidations (a reload, a
	// failure, a settlement's own commit): a settlement compares the value it started at against
	// the room's current one to tell a read still good from one a change since has invalidated.
	// Every value is drawn from the service's one counter (nextRoomGeneration), so it also tells
	// this state from any state that replaced it.
	roomGeneration uint64
	// liveWriter is the open transaction writing this document (see liveWrite), or nil. While
	// it is set no settlement is armed; settleDeferred records one that opening the write took
	// over (armed, fired or running) or that was asked for meanwhile, which finishing the write
	// arms.
	liveWriter     *liveWrite
	settleDeferred bool
	// settleWarming is set while a settlement loads the room it is about to settle, so that load
	// arms no second settlement for the document's pending-settlement row (onLoadDocument).
	settleWarming  bool
	settleFailures int
	// settling counts the settlements running on this state (settleRoomWithin), which hold it
	// across their database work.
	settling   int
	closed     bool
	failed     error
	failedDone chan struct{}
	// reloading marks a state whose room a close is about to retire because the room stands behind
	// the durable head (reserveRoomCloseLocked): no transaction takes its writer slot from then on
	// (openLiveWrite), until the close has returned, whether or not it closed the room it checked
	// (closeReservedRoom).
	reloading bool
	// released marks a state the service has forgotten (forgetLocked), so a lookup that found it
	// before it went takes the document's current state instead.
	released bool
}

// documentUpdateClass is what the room's update observer recorded for one update, which its
// persistence takes (consumeUpdateClass): whether it changed the content, whether it is durable
// work the room's state waits on, the browser edit's in-flight credit, nil for an update no
// browser edit credited, and cursor, the durable version the room instance the update was made in
// stands at, which gates and then takes in the update's own append (StoreUpdateContext).
type documentUpdateClass struct {
	update         []byte
	contentChanged bool
	durable        bool
	credit         *UpdateCredit
	cursor         *roomCursor
}

// lockState returns room's state locked, creating it when the service holds none.
func (s *Service) lockState(room string) *roomState {
	return s.lookUpState(room, true)
}

// lockExistingState returns room's state locked, or nil when the service holds none.
func (s *Service) lockExistingState(room string) *roomState {
	return s.lookUpState(room, false)
}

// newRoomState is a document's record with nothing of a room on it yet.
func newRoomState() *roomState {
	return &roomState{
		connected:         make(map[uint64]model.Actor),
		inflight:          make(map[uint64]*inflightCredit),
		askBlocks:         make(map[string]struct{}),
		askAuthors:        make(map[string]model.Actor),
		pendingAskAuthors: make(map[string]model.Actor),
		unrecorded:        make(map[pmdoc.MarkRef]time.Time),
	}
}

// lookUpState is lockState and lockExistingState. A state released after the map handed it out
// and before its lock was taken is skipped for the document's current one.
func (s *Service) lookUpState(room string, create bool) *roomState {
	for {
		value, ok := s.rooms.Load(room)
		if !ok {
			if !create {
				return nil
			}
			value, _ = s.rooms.LoadOrStore(room, newRoomState())
		}
		state := value.(*roomState)
		if s.afterStateLookup != nil {
			s.afterStateLookup(room)
		}
		state.mu.Lock()
		if !state.released {
			return state
		}
		state.mu.Unlock()
	}
}

// unlockState unlocks state, room's, forgetting it first when it holds nothing
// (releaseIfUnusedLocked).
func (s *Service) unlockState(room string, state *roomState) {
	s.releaseIfUnusedLocked(room, state)
	state.mu.Unlock()
}

// releaseIfUnused forgets room's state when it holds nothing (releaseIfUnusedLocked).
func (s *Service) releaseIfUnused(room string) {
	if state := s.lockExistingState(room); state != nil {
		s.unlockState(room, state)
	}
}

// releaseUnloadedRoom is ygo's OnUnloadDocument: the room has gone, so its state goes too unless
// something still holds it, whose end releases it instead.
func (s *Service) releaseUnloadedRoom(_ context.Context, room string) {
	s.releaseIfUnused(room)
}

// releaseIfUnusedLocked forgets state, room's, once ygo holds no room for it - none loaded or
// loading - and it holds nothing that outlasts the room (unusedLocked). Its caller holds
// state.mu.
//
// A room's load attaches to the state it finds (onLoadDocument takes it through lockState), and
// ygo publishes the loading room before it calls that hook, so a state a load attached to is never
// released while the room is live, and one released first is skipped by the load's lookup.
func (s *Service) releaseIfUnusedLocked(room string, state *roomState) {
	if state.released || s.srv.GetDoc(room) != nil || !s.unusedLocked(state) || slices.Contains(s.srv.Rooms(), room) {
		return
	}
	s.forgetLocked(room, state)
}

// forgetLocked takes state, room's, out of the service: a lookup that already found it skips it
// (lookUpState). Its one caller, releaseIfUnusedLocked, forgets only a state nothing holds
// (unusedLocked), so no in-flight credit is on it. Its caller holds state.mu.
func (s *Service) forgetLocked(room string, state *roomState) {
	state.released = true
	s.rooms.CompareAndDelete(room, state)
}

// handOverFailedRoomLocked takes state, room's, out of the service once the eviction of the room
// it failed has closed that room, and hands the browsers that joined it meanwhile to the record
// registered in its place. The record has to go: it carries the failure, which every holder of it
// still reads (failure) and no later access may (roomFailure, which refuses a load and a connect).
//
// ygo takes the closed room out of its map before CloseRoom returns (sjawhar/ygo v1.51.3-sami.1,
// provider/websocket/inject.go), and a browser registers as it is authorized, before its room is
// loaded or joined (authorize), so a browser that opened the document while the eviction ran is on
// this record. Dropping it outright loses that browser, and with it the credit for the person's
// next keystroke. A load refuses a failed room, so no room of its own attached to this record,
// and what the failed room left is the failed room's, except the
// updates its persistence has not taken yet. Those are carried, with the counts that hold the
// record for them: a failed room's update is dropped while the room is failed
// (StoreUpdateContext), but ygo's stranded persistence can hand one on after the eviction has
// cleared that failure, and a class the successor does not hold is an append stored with no
// credit at all - the author of that keystroke is then on no version and in no pending row. An
// in-flight credit the record still holds is reached through its update's UpdateCredit, which
// holds the record, so an append that lands after the record went loses nothing it writes. A
// browser the close disconnects is forgotten by its own removeConnection, which finds it under
// the connection id the service gave it. Its caller holds state.mu.
func (s *Service) handOverFailedRoomLocked(room string, state *roomState) {
	state.released = true
	if len(state.connected) == 0 && len(state.updateClasses) == 0 {
		s.rooms.CompareAndDelete(room, state)
		return
	}
	successor := newRoomState()
	maps.Copy(successor.connected, state.connected)
	// Only the updates still waiting for the room's persistence move, with the appends they
	// count. An append already in flight finishes on the record that counted it
	// (finishDurableAppend takes the record consumeUpdateClass answered), so its count stays
	// where it is.
	carried := int64(0)
	for _, class := range state.updateClasses {
		if class.durable {
			carried++
		}
	}
	successor.updateClasses = state.updateClasses
	successor.pendingUpdates = state.pendingUpdates
	successor.durableAppends.Store(carried)
	state.updateClasses = nil
	state.pendingUpdates = 0
	state.durableAppends.Add(-carried)
	s.rooms.CompareAndSwap(room, state, successor)
}

// unusedLocked is whether state holds nothing that outlasts its room. These are every holder: an
// open writer, a running or armed settlement, a latest edit source no settlement has read, a
// failure being recovered from (whose eviction forgets the state itself, evictRoom), a connected
// browser, an update the room's persistence has not taken or appended, and an in-flight credit
// whose append has not landed. Its caller holds state.mu.
func (s *Service) unusedLocked(state *roomState) bool {
	return state.liveWriter == nil && state.settling == 0 && !s.isSettleTimerArmed(state.settle) &&
		!state.unsettled && state.failed == nil && len(state.connected) == 0 &&
		state.pendingUpdates == 0 && state.durableAppends.Load() == 0 && len(state.inflight) == 0
}

// recordUpdateClass records what the room's update observer read off one update, for the
// persistence that takes it next (consumeUpdateClass). Only a durable class is work the room's
// state waits on: a repair's update is recorded for the room instance it names alone, and its
// writer, not the room's persistence, is what makes it durable.
func (s *Service) recordUpdateClass(room string, update []byte, class documentUpdateClass) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	class.update = append([]byte(nil), update...)
	state.updateClasses = append(state.updateClasses, class)
	if class.durable {
		state.pendingUpdates++
		state.durableAppends.Add(1)
	}
}

// consumeUpdateClass takes the class the room's update observer recorded for update, wherever it
// stands among the room's recorded updates, and the record it took it from. ygo fires a
// transaction's observers after the transaction has released the document's lock, so two peers'
// updates reach persistence in the order their read loops hand them on, which need not be the
// order the room recorded them in.
func (s *Service) consumeUpdateClass(room string, update []byte) (documentUpdateClass, *roomState, bool) {
	state := s.lockExistingState(room)
	if state == nil {
		return documentUpdateClass{contentChanged: true}, nil, false
	}
	defer s.unlockState(room, state)
	for index, class := range state.updateClasses {
		if !bytes.Equal(class.update, update) {
			continue
		}
		state.updateClasses = append(state.updateClasses[:index], state.updateClasses[index+1:]...)
		if class.durable {
			state.pendingUpdates--
		}
		return class, state, true
	}
	return documentUpdateClass{contentChanged: true}, nil, false
}

func (s *Service) hasPendingUpdates(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.pendingUpdates > 0
}

// finishDurableAppend ends a durable append recordUpdateClass counted on state, the record that
// counted it (consumeUpdateClass answers it). A failed room's eviction registers a new record in
// place of the one that counted the append (handOverFailedRoomLocked), so a late append of the
// failed room's never takes that new record's count below zero.
func (s *Service) finishDurableAppend(room string, state *roomState) {
	state.mu.Lock()
	state.durableAppends.Add(-1)
	s.unlockState(room, state)
}

func (s *Service) hasDurableAppend(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.durableAppends.Load() > 0
}

// hasOpenLiveWrite is whether an open live-write transaction of this process is writing room's
// document (liveWrite). The stale-room evictor asks it before it closes a room another process's
// write left behind, because that writer alone is out of reach of the confirmation the evictor
// makes under the document's advisory lock (roomBehindDurableHead): an open write holds no lock
// between its operations, and between its commit and its publish it has moved the cursor of the
// room it forked past what that room holds (Ledger.takeRoomCursors, publish), so the room it
// publishes into is the room the evictor would close.
//
// The other writers of this process take the document's lock and are ordered by that
// confirmation instead, so they are not asked about here. A browser's append (appendUpdate,
// under withRoomLock) and a settlement (settleRoomWithin, under lockSettlementCursor) each either
// advance the instance's cursor before they drop the lock or are refused by the gate on a room
// behind the head (ErrRoomBehindHead), and a cursor moves only by the version after the one it
// stands at (advanceRoomCursor), so a room the confirmation finds behind is one none of them can
// bring back to the head.
func (s *Service) hasOpenLiveWrite(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.openLiveWriteLocked()
}

// openLiveWriteLocked is hasOpenLiveWrite for a state its caller holds state.mu on: the close of
// a room behind the durable head reads it again under the hold that reserves the close, so no
// write is admitted between the evictor's own unlocked read and the reservation.
func (state *roomState) openLiveWriteLocked() bool {
	return state.liveWriter != nil
}

// editSourceMark is where room's record stood, as to its latest edit source, when a room's load
// was about to read the document's pending-settlement row: the record then registered, nil for
// none; how many edit sources it had recorded (creditSeq, which every recorder of one moves:
// creditContentChange, Ledger.creditRooms, a seed); and whether it held a browser edit whose
// append has yet to write that row (quiet when it held none).
type editSourceMark struct {
	state *roomState
	seq   uint64
	quiet bool
}

// markEditSource marks where room's record stands before its load reads the pending-settlement
// row (onLoadDocument).
func (s *Service) markEditSource(room string) editSourceMark {
	state := s.lockExistingState(room)
	if state == nil {
		return editSourceMark{quiet: true}
	}
	defer s.unlockState(room, state)
	quiet := len(state.inflight) == 0 && state.pendingUpdates == 0 && state.durableAppends.Load() == 0
	return editSourceMark{state: state, seq: state.creditSeq.Load(), quiet: quiet}
}

// rowIsCurrent is whether the pending-settlement row a load read after mark is at least as new as
// the latest edit source state holds, so the load may take the row's in its place. A committed
// write writes the row in its transaction and records the source once it commits
// (Ledger.creditRooms), so a source it recorded by the mark is in the row the load read, and one
// it recorded after moved creditSeq. A browser edit records its source first (creditContentChange)
// and writes it to the row when its append lands, so the row is not current while the record held
// such an edit at the mark: its credit, its update or its append. Two appends write no source:
// one whose credit a version written without a live write already consumed, and one made with no
// browser connected. Each keeps the row's earlier source, which the load then takes, as a record
// made fresh from the row always did. A settlement records no source, so neither a running one
// nor an open write keeps the record's: the settlement whose warm-up runs this very load reads the
// row's. A record registered after the mark counts from none. Its caller holds state.mu.
func (mark editSourceMark) rowIsCurrent(state *roomState) bool {
	if mark.state != state {
		mark = editSourceMark{quiet: true}
	}
	return mark.quiet && state.creditSeq.Load() == mark.seq
}

func (s *Service) roomFailure(room string) error {
	state := s.lockExistingState(room)
	if state == nil {
		return nil
	}
	defer s.unlockState(room, state)
	return state.failureLocked()
}

func (state *roomState) failureLocked() error {
	if state.failed == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrServiceUnavailable, state.failed)
}

// failure is state's own failure as an ErrServiceUnavailable, or nil while it has not failed.
// Which state a caller asks is the question: the one registered for the room now, or the one a
// write holds its slot on, which an eviction has replaced.
func (state *roomState) failure() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.failureLocked()
}

func (s *Service) roomFailed(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.failed != nil
}

// roomClosed is whether room's state remembers its issue closed. A document with no state remembers
// nothing, and its caller reads the issue (allowInject).
func (s *Service) roomClosed(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.closed
}

// canOpenRoom is whether a document socket may open room: one already live, or a new one while
// fewer than maxLiveRooms rooms are live. A document whose room has gone counts for nothing,
// whatever the service once kept for it.
func (s *Service) canOpenRoom(room string) bool {
	if s.srv.GetDoc(room) != nil {
		return true
	}
	live := s.srv.Rooms()
	return len(live) < maxLiveRooms || slices.Contains(live, room)
}

func (s *Service) canAddConnection() bool {
	count := 0
	s.rooms.Range(func(_, value any) bool {
		state := value.(*roomState)
		state.mu.Lock()
		count += len(state.connected)
		state.mu.Unlock()
		return count < maxRoomConnections
	})
	return count < maxRoomConnections
}
