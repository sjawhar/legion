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

	"github.com/reearth/ygo/crdt"

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
	unsettled bool
	// updateClasses is what the room's update observer recorded for each update the room's
	// persistence has yet to take, in the order the room recorded them, and pendingUpdates counts
	// its durable classes whose update no call of the room's persistence has taken yet;
	// queuedBytes is the bytes of their updates, which a batch of the room's browser appends is
	// bounded by (roomAppendBatchBytes). A class stays queued while the call that took it runs its
	// append, alone or in a batch, and a class another call's batch stored stays queued, marked
	// stored, until ygo's own call for its update consumes it (room_appends.go).
	updateClasses  []documentUpdateClass
	pendingUpdates int
	queuedBytes    int
	// appendLinger is what a lingering browser append of the room waits on, woken to end every
	// linger at once: when a wait for the room's appends begins (waitForRoomIdle, which also
	// records appendsFlushed, so an update recorded before that ask no longer lingers, and counts
	// itself in appendWaiters, so no append lingers while it waits), when the queue reaches a
	// batch's cap (recordUpdateClass), and when the record is handed over
	// (handOverFailedRoomLocked).
	appendLinger   wakeSignal
	appendsFlushed time.Time
	appendWaiters  int
	// idle is the room's idle signal: what a wait for the room's appends waits on
	// (waitForRoomIdle) once it finds pendingUpdates or durableAppends above zero, woken once both
	// are zero again (countsFellLocked), or when the record is handed over with its counts
	// (handOverFailedRoomLocked). Every place either count falls tells countsFellLocked.
	// settleWhenIdle marks a settlement whose wait for the appends ran out (retrySettleSoon): the
	// same fall that wakes the signal arms it again, at once, as any settlement timer is armed.
	idle           wakeSignal
	settleWhenIdle bool
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
	settling int
	// closed is whether the document's issue is closed as of closedRead: when the newest read of
	// the issue recorded here began, or when a close or reopen of the issue was recorded
	// (SetIssueClosed). A read that began earlier than the one recorded never replaces it
	// (recordIssueLocked), so the record is at least as fresh as any read a holder made before it.
	// A zero closedRead is no record, and closed then says nothing.
	closed     bool
	closedRead time.Time
	// admitting counts the document sockets whose admission holds this record (holdForAdmission):
	// from the moment it records its read of the issue here until the socket ends. A cold room's
	// load takes that read instead of reading the issue again (admittedIssueRead), and a close of
	// the issue while the socket loads its document reaches the socket through it.
	admitting int
	// residentBytes is the encoded bytes the room resident on this record took: the history its
	// load decoded and each update it committed since (recordUpdateClass). The service's
	// residentBytes counts them too, until the room unloads or the record goes
	// (dropResidentBytesLocked).
	residentBytes int64
	failed        error
	failedDone    chan struct{}
	// reloading marks a state whose room a close is about to retire because the room stands behind
	// the durable head (reserveRoomCloseLocked): no transaction takes its writer slot from then on
	// (openLiveWrite), until the close has returned, whether or not it closed the room it checked
	// (closeReservedRoom). reservedFor is the instance that close checked: a reservation outlasts
	// that instance when it is retired and a load replaces it on this state before the close
	// returns, and the replacement is not the reserving close's to answer for (closeRoomBehindHead).
	reloading   bool
	reservedFor *crdt.Doc
	// released marks a state the service has forgotten (forgetLocked), so a lookup that found it
	// before it went takes the document's current state instead.
	released bool
}

// documentUpdateClass is what the room's update observer recorded for one update, which its
// persistence takes (consumeUpdateClass): the update itself, the very slice ygo hands every
// observer of the commit (sameUpdate), whether it changed the content, whether it is durable work
// the room's state waits on, the browser edit's in-flight credit, nil for an update no browser
// edit credited, and cursor, the durable version the room instance the update was made in stands
// at, which gates and then takes in the update's own append (StoreUpdateContext). recorded is when
// a durable class was recorded, which its batch lingers from (roomAppendLinger). handed marks a
// class whose update's own call has taken it and runs its append, alone or in a batch: the class
// stays queued for that append to find and mark, and no other call's batch takes it, but it is no
// longer pending (pendingUpdates), as a class consumed at the start of its call is not. stored
// marks a class an append of the room's browser updates committed: its own call's, which consumes
// it next, or another call's batch, which leaves ygo's call for it only to consume it
// (room_appends.go).
type documentUpdateClass struct {
	update         []byte
	contentChanged bool
	durable        bool
	handed         bool
	stored         bool
	credit         *UpdateCredit
	cursor         *roomCursor
	recorded       time.Time
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

// releaseUnloadedRoom is ygo's OnUnloadDocument: the room has gone, so it no longer counts among
// the resident rooms, nor holds a place under the room cap unless a socket still holds a
// reservation on it (roomUnloaded, which takes back what its load counted in onLoadDocument), the
// bytes it took no longer count as resident, and its state goes too unless something still holds
// it, whose end releases it instead. ygo calls it exactly once for each room whose load succeeded,
// whichever way the room goes: the idle sweep, the last peer leaving, CloseRoom.
func (s *Service) releaseUnloadedRoom(_ context.Context, room string) {
	s.roomUnloaded(room)
	if state := s.lockExistingState(room); state != nil {
		s.dropResidentBytesLocked(state)
		s.unlockState(room, state)
	}
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
	s.dropResidentBytesLocked(state)
	state.released = true
	s.rooms.CompareAndDelete(room, state)
}

// dropResidentBytesLocked takes the bytes state's room took out of the service's resident bytes,
// once that room has gone or the record goes. A record outlives its room only while something
// holds it, and a room loaded on it again counts its own load. Its caller holds state.mu.
func (s *Service) dropResidentBytesLocked(state *roomState) {
	s.residentBytes.Add(-state.residentBytes)
	state.residentBytes = 0
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
	// The failed room is closed, so the bytes it took are no longer resident.
	s.dropResidentBytesLocked(state)
	// A wait for the room's appends on this record looks the room up again, and waits on the
	// record that counts the room's updates from now on, if any does.
	state.idle.wake()
	if len(state.connected) == 0 && len(state.updateClasses) == 0 {
		s.rooms.CompareAndDelete(room, state)
		return
	}
	successor := newRoomState()
	maps.Copy(successor.connected, state.connected)
	// Only the classes still queued move, with the appends they count: one waiting for its call,
	// and one whose call has taken it and runs its append (handed), whose append then finishes
	// on the record that holds the class when it ends (roomAppend.committed, or
	// finishDurableAppend on the record consumeUpdateClass answers). A class a batch already
	// stored moves with its stored mark, which ygo's late call for it reads, but counts no
	// append: the batch finished that count at its commit.
	carried := int64(0)
	for _, class := range state.updateClasses {
		if class.durable && !class.stored {
			carried++
		}
	}
	successor.updateClasses = state.updateClasses
	successor.pendingUpdates = state.pendingUpdates
	successor.queuedBytes = state.queuedBytes
	successor.durableAppends.Store(carried)
	state.updateClasses = nil
	state.pendingUpdates = 0
	state.queuedBytes = 0
	state.durableAppends.Add(-carried)
	// A browser append lingering on this record looks the queue up again on the successor.
	state.appendLinger.wake()
	s.rooms.CompareAndSwap(room, state, successor)
}

// unusedLocked is whether state holds nothing that outlasts its room. These are every holder: an
// open writer, a running or armed settlement, a latest edit source no settlement has read, a
// failure being recovered from (whose eviction forgets the state itself, evictRoom), a connected
// browser, a document socket's admission (admitting), an update the room's persistence has not
// taken or appended, and an in-flight credit whose append has not landed. Its caller holds
// state.mu.
func (s *Service) unusedLocked(state *roomState) bool {
	return state.liveWriter == nil && state.settling == 0 && !s.isSettleTimerArmed(state.settle) &&
		!state.unsettled && state.failed == nil && len(state.connected) == 0 && state.admitting == 0 &&
		state.pendingUpdates == 0 && state.durableAppends.Load() == 0 && len(state.inflight) == 0
}

// recordUpdateClass records what the room's update observer read off one update, for the
// persistence that takes it next (consumeUpdateClass). Only a durable class is work the room's
// state waits on: a repair's update is recorded for the room instance it names alone, and its
// writer, not the room's persistence, is what makes it durable.
//
// The class keeps update itself, the slice ygo hands this observer and then the room's
// persistence, with no copy: a lagging room's queue holds no second copy of every update in it,
// and the persistence finds the class by that slice (consumeUpdateClass). A queue that reaches a
// batch's cap ends the linger of the room's browser append, whose batch has nothing more to wait
// for. Every update the room commits is recorded here once, so this is also where the bytes it
// took count as resident (residentBytes).
func (s *Service) recordUpdateClass(room string, update []byte, class documentUpdateClass) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	class.update = update
	if class.durable {
		class.recorded = time.Now()
		state.pendingUpdates++
		state.durableAppends.Add(1)
	}
	state.updateClasses = append(state.updateClasses, class)
	state.queuedBytes += len(update)
	state.residentBytes += int64(len(update))
	s.residentBytes.Add(int64(len(update)))
	if len(state.updateClasses) >= roomAppendBatchUpdates || state.queuedBytes >= roomAppendBatchBytes {
		state.appendLinger.wake()
	}
}

// consumeUpdateClass takes the class the room's update observer recorded for update, wherever it
// stands among the room's recorded updates, and the record it took it from. ygo fires a
// transaction's observers after the transaction has released the document's lock, so two peers'
// updates reach persistence in the order their read loops hand them on, which need not be the
// order the room recorded them in. Which class is update's is findUpdateClassLocked's rule.
func (s *Service) consumeUpdateClass(room string, update []byte) (documentUpdateClass, *roomState, bool) {
	state := s.lockExistingState(room)
	if state == nil {
		return documentUpdateClass{contentChanged: true}, nil, false
	}
	defer s.unlockState(room, state)
	index := findUpdateClassLocked(state.updateClasses, update)
	if index < 0 {
		return documentUpdateClass{contentChanged: true}, nil, false
	}
	return s.consumeUpdateClassLocked(room, state, index), state, true
}

// consumeUpdateClassLocked takes the class at index out of state's queue, room's record, and
// answers it: its bytes leave queuedBytes, and a durable class no call had taken yet (handed) ends
// its pending count. Its caller holds state.mu.
func (s *Service) consumeUpdateClassLocked(room string, state *roomState, index int) documentUpdateClass {
	class := state.updateClasses[index]
	if index == 0 {
		// The usual case, the queue's oldest class: its slot is cleared, so the array the queue
		// keeps holds no update the room has already stored.
		state.updateClasses[0] = documentUpdateClass{}
		state.updateClasses = state.updateClasses[1:]
	} else {
		state.updateClasses = slices.Delete(state.updateClasses, index, index+1)
	}
	state.queuedBytes -= len(class.update)
	if class.durable && !class.handed {
		state.pendingUpdates--
		s.countsFellLocked(room, state)
	}
	return class
}

// findUpdateClassLocked is the index in queue of the class recorded for update, -1 for none. The
// class is the one recorded with the very slice update is (sameUpdate): ygo hands the room's
// observer and its persistence the slice it encoded at the commit, so that is the same commit,
// where equal bytes need not be - two commits that change nothing encode alike, and each takes its
// own class. The search runs from the queue's oldest class, the one ygo's in-order persistence
// usually hands on, so the usual match is the first comparison. Only an update no class holds the
// slice of - a copy, as a test can hand on - falls back to the first class with the same bytes.
// Its caller holds the queue's state.mu.
func findUpdateClassLocked(queue []documentUpdateClass, update []byte) int {
	if at := queuedUpdateLocked(queue, update, 0); at >= 0 {
		return at
	}
	for index := range queue {
		if bytes.Equal(queue[index].update, update) {
			return index
		}
	}
	return -1
}

func (s *Service) hasPendingUpdates(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.pendingLocked()
}

// pendingLocked is whether state counts an update the room's persistence has not taken yet. Its
// caller holds state.mu.
func (state *roomState) pendingLocked() bool {
	return state.pendingUpdates > 0
}

// finishDurableAppend ends a durable append recordUpdateClass counted on state, the record that
// counted it (consumeUpdateClass answers it). A failed room's eviction registers a new record in
// place of the one that counted the append (handOverFailedRoomLocked), so a late append of the
// failed room's never takes that new record's count below zero.
func (s *Service) finishDurableAppend(room string, state *roomState) {
	state.mu.Lock()
	state.durableAppends.Add(-1)
	s.countsFellLocked(room, state)
	s.unlockState(room, state)
}

func (s *Service) hasDurableAppend(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.appendingLocked()
}

// appendingLocked is whether state counts a durable append that has not landed. Its caller holds
// state.mu.
func (state *roomState) appendingLocked() bool {
	return state.durableAppends.Load() > 0
}

// countsFellLocked is told each time pendingUpdates or durableAppends falls on state, room's
// record, wherever it falls: an update's class consumed (consumeUpdateClass) or taken by its call
// (planRoomAppend), an append finished (finishDurableAppend) or committed (roomAppend.committed).
// Once both are zero the room is idle: it closes the idle signal, so every wait for the room's
// appends wakes (waitForRoomIdle), and arms the settlement a wait gave up on (retrySettleSoon) at
// once, through scheduleSettleAfterLocked like any settlement timer, so settleWG counts it and
// stopAllSettleTimers stops it. A record released meanwhile - handed over, its counts carried to
// its successor - arms nothing: the failure that dropped it dropped its settlement
// (failRoomLocked). Its caller holds state.mu.
func (s *Service) countsFellLocked(room string, state *roomState) {
	if state.pendingLocked() || state.appendingLocked() {
		return
	}
	state.idle.wake()
	if state.settleWhenIdle && !state.released {
		s.rearmSettleLocked(room, state)
	}
}

// wakeSignal is a channel any number of waiters wait on, and one wake ends every wait at once. The
// first waiter makes it (wait), and wake closes it, then drops it, so the next waiter makes its
// own; nothing is made while nobody waits. The zero wakeSignal is ready, and its owner's mutex
// guards it.
type wakeSignal struct{ ch chan struct{} }

// wait is the channel the next wake closes, made now when no waiter has made it yet.
func (w *wakeSignal) wait() <-chan struct{} {
	if w.ch == nil {
		w.ch = make(chan struct{})
	}
	return w.ch
}

// wake closes the channel, if a waiter made one, and drops it.
func (w *wakeSignal) wake() {
	if w.ch != nil {
		close(w.ch)
		w.ch = nil
	}
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
// confirmation instead, so they are not asked about here. A batch of a room's browser updates
// (PgVersioned.AppendUpdatesWithCredit, under the document's session lock on its own connection)
// and a settlement (settleRoomWithin, under lockSettlementCursor) each either advance the
// instance's cursor, once for each version they stored, before they drop the lock, or are refused
// by the gate on a room behind the head (ErrRoomBehindHead), and a cursor moves only by the
// version after the one it stands at (advanceRoomCursor), so a room the confirmation finds behind
// is one none of them can bring back to the head.
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

// issueRead is one read of a document's issue: whether the issue was open, and when the read
// began. A read sees the issue as of some moment after it began, so a read that began later is at
// least as fresh as one that began earlier. The zero issueRead is no read at all.
type issueRead struct {
	open bool
	at   time.Time
}

// readIssue reads whether room's issue is open (issueOpen) on q, stamped with when the read began:
// the stamp is taken before the query, so the read is never taken for fresher than it is.
func (s *Service) readIssue(ctx context.Context, q Queryer, room string) (issueRead, error) {
	read := issueRead{at: time.Now()}
	var err error
	read.open, err = s.issueOpen(ctx, q, room)
	return read, err
}

// recordIssueLocked records read on state unless state holds a record made later (closedRead):
// an older read never replaces a newer one, nor a close or reopen recorded after it began. Its
// caller holds state.mu.
func (state *roomState) recordIssueLocked(read issueRead) {
	if read.at.IsZero() || read.at.Before(state.closedRead) {
		return
	}
	state.closed, state.closedRead = !read.open, read.at
}

// recordedIssueClosed is whether room's state records its issue closed, and whether it records
// the issue at all: a document with no state, or a state no read or close of the issue reached,
// records nothing.
func (s *Service) recordedIssueClosed(room string) (closed, recorded bool) {
	state := s.lockExistingState(room)
	if state == nil {
		return false, false
	}
	defer s.unlockState(room, state)
	return state.closed, !state.closedRead.IsZero()
}

// recordIssueRead records read on room's state, when the service holds one (recordIssueLocked).
func (s *Service) recordIssueRead(room string, read issueRead) {
	if state := s.lockExistingState(room); state != nil {
		state.recordIssueLocked(read)
		s.unlockState(room, state)
	}
}

// admittedIssueRead is what room's state records of its issue while a document socket's
// admission holds the state (holdForAdmission), and whether it does: the socket's own read, or a
// close or reopen recorded since. A cold socket's admission triggers the room's load, which takes
// this rather than reading the issue a second time (onLoadDocument); a load no admission holds -
// a settlement's warm-up, a live write's publish - reads the issue itself.
func (s *Service) admittedIssueRead(room string) (issueRead, bool) {
	state := s.lockExistingState(room)
	if state == nil {
		return issueRead{}, false
	}
	defer s.unlockState(room, state)
	if state.admitting == 0 || state.closedRead.IsZero() {
		return issueRead{}, false
	}
	return issueRead{open: !state.closed, at: state.closedRead}, true
}

// holdForAdmission holds room's state for a document socket's admission, from its read of the
// issue, which it records there, until releaseAdmission, which the socket's end runs. While it
// holds the state, a close of the issue (SetIssueClosed) finds the state and records the close on
// it, so a socket whose issue closes while it loads its document is still admitted read-only
// (authorize), and the room the socket's load opens takes the record rather than reading the
// issue again (admittedIssueRead).
func (s *Service) holdForAdmission(room string, read issueRead) *roomState {
	state := s.lockState(room)
	state.admitting++
	state.recordIssueLocked(read)
	s.unlockState(room, state)
	return state
}

// releaseAdmission ends the hold holdForAdmission took on state, room's, and forgets the state if
// nothing else holds it (releaseIfUnusedLocked).
func (s *Service) releaseAdmission(room string, state *roomState) {
	state.mu.Lock()
	state.admitting--
	s.unlockState(room, state)
}

// roomSlots is the places the room cap (roomCap) counts: every room ygo holds, from its load
// (onLoadDocument) to its unload (releaseUnloadedRoom), whoever opened it, and every room a
// document socket reserved a place for (reserveRoom) and has not given back, each room once however
// many loads and sockets hold it. A socket reserves before it loads anything, and the check and the
// taking are one step under mu, so sockets that open at once each see the places the others took,
// and a reserved room ygo then loads keeps the one place it had. The count is the map's length, so
// the answer walks no list of rooms: an eviction or a deploy reconnects every browser at once, and
// a walk on each open would cost every room for each of them. A document whose room has gone and
// that no socket holds counts for nothing, whatever the service once kept for it.
type roomSlots struct {
	mu    sync.Mutex
	rooms map[string]roomSlot
}

// roomSlot is what holds one room's place: resident, the loads of it ygo holds (one, save while a
// closed instance's unload and its successor's load overlap), and sockets, the document sockets
// holding a reservation on it. The room keeps its place while either is above zero.
type roomSlot struct {
	resident int
	sockets  int
}

// setLocked records slot as room's, and forgets the room once nothing holds its place. Its caller
// holds slots.mu.
func (slots *roomSlots) setLocked(room string, slot roomSlot) {
	if slot.resident <= 0 && slot.sockets <= 0 {
		delete(slots.rooms, room)
		return
	}
	if slots.rooms == nil {
		slots.rooms = make(map[string]roomSlot)
	}
	slots.rooms[room] = slot
}

// reserveRoom takes a place under the room cap for a document socket on room, and is whether it
// did: the room's own place when it holds one already - resident, loading, or reserved by another
// socket - or a new one while fewer than roomCap rooms hold places. A room an API path loads
// (Server.Apply) holds its place from its load (roomLoaded) though no socket asked for it, and is
// refused nothing, so it counts against the sockets that come after it. The socket gives the
// reservation back when it ends, whatever ended it (releaseRoom); a room ygo loaded meanwhile keeps
// the place until it unloads.
func (s *Service) reserveRoom(room string) bool {
	s.roomSlots.mu.Lock()
	defer s.roomSlots.mu.Unlock()
	slot, held := s.roomSlots.rooms[room]
	if !held && len(s.roomSlots.rooms) >= s.roomCap {
		return false
	}
	slot.sockets++
	s.roomSlots.setLocked(room, slot)
	return true
}

// releaseRoom gives back a socket's reservation on room (reserveRoom).
func (s *Service) releaseRoom(room string) {
	s.roomSlots.mu.Lock()
	defer s.roomSlots.mu.Unlock()
	slot := s.roomSlots.rooms[room]
	slot.sockets--
	s.roomSlots.setLocked(room, slot)
}

// roomLoaded counts a load of room among the resident rooms (residentRooms) and among the room
// cap's places, where it takes the place a socket reserved for the room, if one did, rather than
// a second one, and holds room's head-tracker entry (headTracker.hold) for the room's evictor
// decisions (staleRoomEvictor.roomHolds).
func (s *Service) roomLoaded(room string) {
	s.heads.hold(room)
	s.roomSlots.mu.Lock()
	defer s.roomSlots.mu.Unlock()
	slot := s.roomSlots.rooms[room]
	slot.resident++
	s.roomSlots.setLocked(room, slot)
	s.residentRooms.Add(1)
}

// roomUnloaded takes back what roomLoaded counted for a load of room, and its hold on room's
// head-tracker entry. The room keeps its place while a socket still holds a reservation on it.
func (s *Service) roomUnloaded(room string) {
	s.heads.release(room)
	s.roomSlots.mu.Lock()
	defer s.roomSlots.mu.Unlock()
	slot := s.roomSlots.rooms[room]
	slot.resident--
	s.roomSlots.setLocked(room, slot)
	s.residentRooms.Add(-1)
}

// reserveConnection takes a place under the connection cap for a document socket, and is whether
// it did. connections counts the browsers registered on the room records and the sockets holding a
// place for the connection they are about to register. The check and the taking are one
// compare-and-swap, so sockets that open at once never hold more places than the cap. A socket's
// place is its connection's count from here: authorize registers the connection without counting
// it again, and removeConnection, which drops the count as it forgets the browser, ends it; a
// socket that ends unregistered gives it back (releaseConnection). The count lives outside the
// room records: a failed room's eviction hands its browsers to the record that replaces it
// (handOverFailedRoomLocked), which moves no connection.
func (s *Service) reserveConnection() bool {
	for {
		held := s.connections.Load()
		if held >= int64(s.connectionCap) {
			return false
		}
		if s.connections.CompareAndSwap(held, held+1) {
			return true
		}
	}
}

// releaseConnection gives back the place reserveConnection took for a socket that registered no
// connection.
func (s *Service) releaseConnection() {
	s.connections.Add(-1)
}
