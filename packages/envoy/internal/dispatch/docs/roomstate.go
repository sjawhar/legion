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
// What a release drops - the unrecorded marks' first sightings and their authors' departures, and
// the closed flag - is what a restart drops too.
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
	// updateClasses is the queue of classes the room's update observer recorded, in the order it
	// recorded them, which is seq order for each room instance (queuedClass). pendingUpdates
	// counts the durable classes whose update no call of the room's persistence has taken yet,
	// and queuedBytes is the bytes of the queue's updates, which a batch of the room's browser
	// appends is bounded by (roomAppendBatchBytes).
	updateClasses  []*queuedClass
	pendingUpdates int
	queuedBytes    int
	// appendLinger is what a lingering browser append of the room waits on, woken to end every
	// linger at once: when a wait for the room's appends begins (waitForRoomIdle, which also
	// records appendsFlushed, so an update recorded before that ask no longer lingers, and counts
	// itself in appendWaiters, so no append lingers while it waits), when the queue reaches a
	// batch's cap (queueUpdateClass), and when the record is handed over
	// (handOverFailedRoomLocked).
	appendLinger   wakeSignal
	appendsFlushed time.Time
	appendWaiters  int
	// idle is the room's idle signal: what a wait for the room's appends waits on
	// (waitForRoomIdle) once it finds pendingUpdates or durableAppends above zero, woken once both
	// are zero again (countsFellLocked), or when the record is handed over with its counts
	// (handOverFailedRoomLocked). Every place either count falls tells countsFellLocked.
	idle wakeSignal
	// parked are the parked bytes of commits that parked something and changed nothing, which no
	// OnUpdate hands to the room's persistence (classifyDrained). They wait here, with the cursor
	// of the room instance that parked them, until a drainer that holds no replica lock and no
	// document lock stores them (storeParked): a drain can run under a settlement's document lock,
	// and the store takes that lock.
	parked []parkedBytes
	// shadow is what the latest committed live write left the room to measure again the classes
	// of the browser updates stored after its own (roomShadow), nil while none is left.
	shadow     *roomShadow
	settle     *time.Timer
	unrecorded map[pmdoc.MarkRef]time.Time
	// departed is when each user whose browsers have all left this room lost the last of them,
	// under both spellings a mark's `by` can carry (recordMarkAuthorDeparture). An unrecorded
	// mark ages from its author's entry rather than from the sweep that last saw that author, so
	// a browser that drops between two sweeps keeps its mark for the whole TTL after the drop.
	// Coming back clears the entry, and the sweep drops one older than the TTL, which can no
	// longer change what it decides (sweepUnrecordedMarks).
	departed       map[string]time.Time
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
	admitting  int
	failed     error
	failedDone chan struct{}
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
// observer of the commit (sameUpdate), whether it changed the content, whether it is durable
// work the room's state waits on, the browser edit's in-flight credit, nil for an update no
// browser edit credited, and cursor, the durable version the room instance the update was made in
// stands at, which gates and then takes in the update's own append (StoreUpdateContext). seq is the
// update's commit position in that room instance (renderedReplica.seq): updates of one instance are
// stored in seq order (flushThrough). roomCredit is, for a browser update measured as no change
// while a live write was open, the peers connected as the room took it, outside F and with no
// credit sequence of its own: the credit its append takes if the room's shadow measures it a
// change after all (storedClasses). recorded is when a durable class was recorded, which the batch
// its own call takes lingers from (roomAppendLinger). resident is the token of the load whose
// room instance committed the update, which the update's bytes count against (queueUpdateClass),
// nil for a class no room instance made.
type documentUpdateClass struct {
	update         []byte
	seq            uint64
	contentChanged bool
	durable        bool
	credit         *UpdateCredit
	roomCredit     *UpdateCredit
	cursor         *roomCursor
	resident       *residentLoad
	recorded       time.Time
}

// queuedClass is a class in the room's queue (roomState.updateClasses), with where it stands:
// handed, once the room's persistence has taken it (consumeUpdateClass), and stored, once a
// committed transaction holds its update - the worker's own append, or any durable writer's
// flushThrough. Published live-write updates and repairs are queued already stored, since their
// writers store them. A class leaves the queue when its worker is done with it
// (finishUpdateClass), so a worker whose update another writer stored finds that out and skips
// it. handed is guarded by the mu of the state whose queue holds the class. stored is atomic: a
// failed room's hand-over moves an unhanded class to the record registered in its place
// (handOverFailedRoomLocked) while a transaction that took it from the failed record may still
// mark it.
type queuedClass struct {
	documentUpdateClass
	handed bool
	stored atomic.Bool
	// shadowed marks a class the room's shadow measured, and shadowChanged what it measured,
	// both guarded by that shadow's mu (roomShadow.measure).
	shadowed      bool
	shadowChanged bool
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
		departed:          make(map[string]time.Time),
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
// reservation on it, the bytes of one load of the name no longer count as resident (roomUnloaded,
// which ends the name's oldest load: the one ending, save in the out-of-order case below), and its
// state goes too unless something
// still holds it, whose end releases it instead. ygo calls it exactly once for each room whose
// load succeeded, and only for a room that has left its map, whichever way the room goes: the
// idle sweep, the last peer leaving, CloseRoom.
//
// The unload ends the oldest load counted under the name (roomUnloaded). That load is always one
// that has left ygo's map: ygo holds one room of a name at a time, takes a room out of its map
// only once that room's load has finished (CloseRoom, the idle sweep and a departing browser all
// wait for it to be ready), and calls this only after that; a load that fails ends its own count
// before it returns (onLoadDocument). Every load older than the newest has therefore left the map,
// and the newest has too unless ygo holds it, which it does only once every older one has gone.
// When two rooms of a name have both left the map and report their unloads out of order, the
// first to run ends the older load's count and the second the younger's: neither need be its
// own, and the total is exact once both have run.
func (s *Service) releaseUnloadedRoom(_ context.Context, room string) {
	if s.beforeUnloadEndsLoad != nil {
		s.beforeUnloadEndsLoad(room)
	}
	s.roomUnloaded(room, nil)
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
	// A wait for the room's appends on this record looks the room up again, and waits on the
	// record that counts the room's updates from now on, if any does.
	state.idle.wake()
	// The failed room's shadow goes with it (failRoomLocked frees it already): a carried class is
	// stored with the class its update observer recorded.
	state.shadow = nil
	// Only the updates still waiting for the room's persistence move, with the appends they
	// count and the bytes a batch of them is bounded by. An append already in flight finishes on
	// the record that counted it, with its class (finishUpdateClass takes the record
	// consumeUpdateClass answered), so both stay where they are.
	var carried []*queuedClass
	var durable int64
	var carriedBytes int
	kept := state.updateClasses[:0]
	for _, class := range state.updateClasses {
		if class.handed {
			kept = append(kept, class)
			continue
		}
		carried = append(carried, class)
		carriedBytes += len(class.update)
		if class.durable {
			durable++
		}
	}
	clear(state.updateClasses[len(kept):])
	state.updateClasses = kept
	if len(state.connected) == 0 && len(carried) == 0 {
		s.rooms.CompareAndDelete(room, state)
		return
	}
	successor := newRoomState()
	maps.Copy(successor.connected, state.connected)
	successor.updateClasses = carried
	successor.pendingUpdates = state.pendingUpdates
	successor.queuedBytes = carriedBytes
	successor.durableAppends.Store(durable)
	state.pendingUpdates = 0
	state.queuedBytes -= carriedBytes
	state.durableAppends.Add(-durable)
	// A browser append lingering on this record looks the queue up again on the successor.
	state.appendLinger.wake()
	s.rooms.CompareAndSwap(room, state, successor)
}

// unusedLocked is whether state holds nothing that outlasts its room. These are every holder: an
// open writer, a running or armed settlement, a latest edit source no settlement has read, a
// failure being recovered from (whose eviction forgets the state itself, evictRoom), a connected
// browser, a document socket's admission (admitting), an update the room's persistence has not
// taken or appended, a commit's parked bytes not yet stored, and an in-flight credit whose append
// has not landed. Its caller holds state.mu.
func (s *Service) unusedLocked(state *roomState) bool {
	return state.liveWriter == nil && state.settling == 0 && !s.isSettleTimerArmed(state.settle) &&
		!state.unsettled && state.failed == nil && len(state.connected) == 0 && state.admitting == 0 &&
		state.pendingUpdates == 0 && state.durableAppends.Load() == 0 && len(state.inflight) == 0 &&
		len(state.parked) == 0
}

// parkedBytes is one commit's parked bytes and the cursor of the room instance that took them.
type parkedBytes struct {
	update []byte
	cursor *roomCursor
}

// queueUpdateClass records what the room's update observer read off one update, for the
// persistence that takes it next (consumeUpdateClass). Only a durable class is work the room's
// state waits on: a repair's update is recorded for the room instance it names alone, and its
// writer, not the room's persistence, is what makes it durable. Stored is set for a class whose
// update a writer other than the room's persistence has already stored, or is storing under the
// document's advisory lock: a published live write's, or a repair's (classifyDrained).
//
// A queue that reaches a batch's cap ends the linger of the room's browser append, whose batch
// has nothing more to wait for. Every update the room commits is recorded here once, so this is
// also where the bytes it took count as resident, against the token of the load whose instance
// committed it (residentLoad, countResidentBytes), which an unload of the name ends. A class
// carrying no token - one a test built by hand - counts no bytes.
func (s *Service) queueUpdateClass(room string, update []byte, class documentUpdateClass, stored bool) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	class.update = update
	if class.durable {
		class.recorded = time.Now()
	}
	queued := &queuedClass{documentUpdateClass: class}
	queued.stored.Store(stored)
	state.updateClasses = append(state.updateClasses, queued)
	if class.durable {
		state.pendingUpdates++
		state.durableAppends.Add(1)
	} else {
		state.recordRepairLocked(class)
	}
	state.queuedBytes += len(class.update)
	s.countResidentBytes(class.resident, int64(len(class.update)))
	if len(state.updateClasses) >= roomAppendBatchUpdates || state.queuedBytes >= roomAppendBatchBytes {
		state.appendLinger.wake()
	}
}

// consumeUpdateClass takes the class the room's update observer recorded for update, wherever it
// stands among the room's recorded updates, and the record it took it from: it marks the class
// handed and leaves it queued until its worker is done with it (finishUpdateClass). ygo fires a
// transaction's observers after the transaction has released the document's lock, so two peers'
// updates reach persistence in the order their read loops hand them on, which need not be the
// order the room recorded them in; the order they are stored in is the room's all the same
// (flushThrough). Which class is update's is findUpdateClassLocked's rule, and a class already
// handed is never taken a second time.
func (s *Service) consumeUpdateClass(room string, update []byte) (*queuedClass, *roomState, bool) {
	state := s.lockExistingState(room)
	if state == nil {
		return unrecordedClass(), nil, false
	}
	defer s.unlockState(room, state)
	class := findUpdateClassLocked(state.updateClasses, update)
	if class == nil {
		return unrecordedClass(), nil, false
	}
	class.handed = true
	if class.durable {
		state.pendingUpdates--
		s.countsFellLocked(room, state)
	}
	return class, state, true
}

// findUpdateClassLocked is the class recorded for update that no call has taken yet, nil for
// none. The class is the one recorded with the very slice update is (sameUpdate): ygo hands the
// room's observer and its persistence the slice it encoded at the commit, so that is the same
// commit, where equal bytes need not be - two commits can encode alike, a browser's deletion and
// a repair's of the same items, and each takes its own class. Only an update no queued class
// holds the slice of - a copy, as a test can hand on - falls back to the first class with the
// same bytes. Its caller holds the queue's state.mu.
func findUpdateClassLocked(queue []*queuedClass, update []byte) *queuedClass {
	for _, class := range queue {
		if !class.handed && sameUpdate(class.update, update) {
			return class
		}
	}
	for _, class := range queue {
		if !class.handed && bytes.Equal(class.update, update) {
			return class
		}
	}
	return nil
}

// sameUpdate reports whether queued, the update a class holds, is update, the one its caller was
// handed: the very slice, not equal bytes. Two commits can encode alike - a browser's deletion
// and a repair's of the same items - so bytes cannot tell one from the other; the slice can,
// since ygo hands every observer of one commit the slice it encoded for it.
func sameUpdate(queued, update []byte) bool {
	return len(queued) == len(update) && len(queued) > 0 && &queued[0] == &update[0]
}

// unrecordedClass is the class of an update no observer recorded a class for: a content change,
// with no credit and no room instance.
func unrecordedClass() *queuedClass {
	return &queuedClass{documentUpdateClass: documentUpdateClass{contentChanged: true}}
}

// finishUpdateClass ends what the room's worker took with class from state, the record
// consumeUpdateClass answered: the class leaves the queue, stored or dropped, and a durable one
// ends the append queueUpdateClass counted. A failed room's eviction registers a new record in
// place of the one that counted the append (handOverFailedRoomLocked), so a late append of the
// failed room's never takes that new record's count below zero.
func (s *Service) finishUpdateClass(room string, state *roomState, class *queuedClass) {
	state.mu.Lock()
	state.updateClasses = slices.DeleteFunc(state.updateClasses, func(queued *queuedClass) bool { return queued == class })
	state.queuedBytes -= len(class.update)
	if class.durable {
		state.durableAppends.Add(-1)
	}
	s.countsFellLocked(room, state)
	s.unlockState(room, state)
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
// record, wherever it falls: an update's class handed to its call (consumeUpdateClass) or its
// append finished (finishUpdateClass).
// Once both are zero the room is idle: it closes the idle signal, so every wait for the room's
// appends wakes (waitForRoomIdle). Its caller holds state.mu.
func (s *Service) countsFellLocked(room string, state *roomState) {
	if state.pendingLocked() || state.appendingLocked() {
		return
	}
	state.idle.wake()
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

// roomSlot is what holds one room's place: loads, a token for each load of it whose unload has not
// run (one, save while a closed instance's unload and its successor's load overlap), oldest first,
// which is the order the unloads of the name end them in (roomUnloaded), and sockets, the document
// sockets holding a reservation on it. The room keeps its place while either is not empty.
type roomSlot struct {
	sockets int
	loads   []*residentLoad
}

// residentLoad is one room load's share of the service's resident bytes (Service.residentBytes):
// the history that load decoded (onLoadDocument) and every update the room instance it left
// behind committed (queueUpdateClass). The bytes are kept per load rather than on the room's
// record because ygo's OnUnloadDocument names the room alone and comes only after the room has
// left its map, so a room of the same name can load again first, onto that very record or onto
// one created after it was forgotten: either way a record's total is the wrong thing to take
// back, and the figure would read short for the whole of the newer room's life. Each unload of the
// name ends the oldest token (roomUnloaded): its own load's, save when two rooms of the name have
// both left ygo's map and report their unloads out of order, when each ends the other's, and the
// total is exact once both have run.
//
// bytes and ended are guarded by roomSlots.mu. A token leaves the name's list in the same hold of
// that lock that takes its bytes back (roomUnloaded), so each load's bytes leave the count once;
// ended, set there too, is what refuses a count that arrives for a load already over
// (countResidentBytes), such as an update the room's drain classifies after its unload.
type residentLoad struct {
	bytes int64
	ended bool
}

// countResidentBytes counts n, the bytes a load took, against load, that load's token, and among
// the service's resident bytes, which the unload that ends the token takes them back out of
// (roomUnloaded). A load that has ended counts nothing more, so no bytes are left in the figure
// with no unload to come for them, and neither does a class carrying no token at all
// (queueUpdateClass).
func (s *Service) countResidentBytes(load *residentLoad, n int64) {
	if load == nil {
		return
	}
	s.roomSlots.mu.Lock()
	defer s.roomSlots.mu.Unlock()
	if load.ended {
		return
	}
	load.bytes += n
	s.residentBytes.Add(n)
}

// setLocked records slot as room's, and forgets the room once nothing holds its place. Its caller
// holds slots.mu.
func (slots *roomSlots) setLocked(room string, slot roomSlot) {
	if len(slot.loads) == 0 && slot.sockets <= 0 {
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
// decisions (staleRoomEvictor.roomHolds). It answers with the load's own token: what the load
// takes counts against that token (countResidentBytes), and an unload of the name ends it
// (roomUnloaded).
func (s *Service) roomLoaded(room string) *residentLoad {
	s.heads.hold(room)
	s.roomSlots.mu.Lock()
	defer s.roomSlots.mu.Unlock()
	load := &residentLoad{}
	slot := s.roomSlots.rooms[room]
	slot.loads = append(slot.loads, load)
	s.roomSlots.setLocked(room, slot)
	s.residentRooms.Add(1)
	return load
}

// roomLoadedHere is whether a room of this name is loaded on this process, or has left ygo's map
// with its OnUnloadDocument still to come: the loads roomLoaded counted for the name that no unload
// has ended (roomSlot.loads). It is kept by the room's name rather than on its record, because
// ygo's OnUnloadDocument names the room alone and runs only after the room has left ygo's map and
// its persistence has drained, so a room of the same name can load again before an earlier one's
// unload arrives, onto a record created after the earlier room's record was forgotten
// (releaseIfUnusedLocked). A flag on the record would be cleared by that late unload while the
// newer room is loaded; each unload here ends one load's count, so the newer room's stays. A
// socket's admission asks it (offerPreload) under the room's state record, and a load counts itself
// here before it sweeps the preloads stored under the room's name (onLoadDocument), so an offer
// that sweep did not reach was stored by an admission whose own ask comes after this count and
// finds the room loaded.
func (s *Service) roomLoadedHere(room string) bool {
	s.roomSlots.mu.Lock()
	defer s.roomSlots.mu.Unlock()
	return len(s.roomSlots.rooms[room].loads) > 0
}

// roomUnloaded takes back what roomLoaded counted for one load of room - its place among the
// resident rooms and under the room cap, its hold on room's head-tracker entry, and the bytes
// counted against its token - and is the only place a load's bytes leave the service's count.
// load is the token to end: a failed load names its own (onLoadDocument), and an unload names
// none, which ends the oldest load of the name (releaseUnloadedRoom says why that one has ended).
// The token is chosen and taken out under the one hold of roomSlots.mu that ends it, so two
// unloads of a name never end the same load. The room keeps its place while a socket still holds
// a reservation on it.
func (s *Service) roomUnloaded(room string, load *residentLoad) {
	s.heads.release(room)
	s.roomSlots.mu.Lock()
	defer s.roomSlots.mu.Unlock()
	slot := s.roomSlots.rooms[room]
	at := -1
	if load != nil {
		at = slices.Index(slot.loads, load)
	} else if len(slot.loads) > 0 {
		at = 0
	}
	if at >= 0 {
		load = slot.loads[at]
		slot.loads = slices.Delete(slot.loads, at, at+1)
		load.ended = true
		s.residentBytes.Add(-load.bytes)
	}
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
