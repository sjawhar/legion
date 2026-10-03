package docs

import (
	"bytes"
	"context"
	"fmt"
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
// What a release drops - the unrecorded marks' first sightings, the closed flag and rendered
// markdown a load re-reads - is what a restart drops too.
type roomState struct {
	mu        sync.Mutex
	connected map[uint64]model.Actor
	pending   map[string]model.Actor
	// lastActor is the most recent edit's source: the actor of a service mutation, or the sole
	// connected peer of a browser edit. Version writes clear `pending`, so a settlement that
	// runs after an edit's own version was committed would otherwise attribute the block asks
	// it indexes to nobody.
	lastActor *model.Actor
	// unsettled marks authors recorded in pending or lastActor that no settlement has read since:
	// the settlement they were recorded for credits them, so the state holds them until one
	// commits (settleRoomWithin). Closing persists them on the pending-settlement row before
	// clearing them (SetIssueClosed), so that row carries them through a room release or restart.
	// creditVersion identifies the snapshot the row has durably received, so a late write cannot
	// be cleared by an earlier close's flush.
	unsettled       bool
	creditVersion   uint64
	pendingVersions map[int]versionPending
	// contentMarkdown is the live document's rendered markdown when the room's update observer
	// last saw it change, nil until the room loads.
	contentMarkdown *string
	updateClasses   []documentUpdateClass
	pendingUpdates  int
	settle          *time.Timer
	unrecorded      map[pmdoc.MarkRef]time.Time
	durableAppends  atomic.Int64
	gen             uint64
	// liveWriter is the open transaction writing this document (see liveWrite), or nil. While
	// it is set no settlement is armed; settleDeferred records one that was stopped or asked for
	// meanwhile, which finishing the write arms.
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
	// released marks a state the service has forgotten (forgetLocked), so a lookup that found it
	// before it went takes the document's current state instead.
	released bool
}

type documentUpdateClass struct {
	update         []byte
	contentChanged bool
	durable        bool
	credit         settlementCredit
	creditVersion  uint64
}

// lockState returns room's state locked, creating it when the service holds none.
func (s *Service) lockState(room string) *roomState {
	return s.lookUpState(room, true)
}

// lockExistingState returns room's state locked, or nil when the service holds none.
func (s *Service) lockExistingState(room string) *roomState {
	return s.lookUpState(room, false)
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
			value, _ = s.rooms.LoadOrStore(room, &roomState{
				connected:       make(map[uint64]model.Actor),
				pending:         make(map[string]model.Actor),
				pendingVersions: make(map[int]versionPending),
				unrecorded:      make(map[pmdoc.MarkRef]time.Time),
			})
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
// (lookUpState). Its caller holds state.mu.
func (s *Service) forgetLocked(room string, state *roomState) {
	state.released = true
	s.rooms.CompareAndDelete(room, state)
}

// unusedLocked is whether state holds nothing that outlasts its room. These are every holder: an
// open writer, a running or armed settlement, authors no settlement has read, a failure being
// recovered from (whose eviction forgets the state itself, evictRoom), a connected browser, an
// update the room's persistence has not taken or appended, and a version whose authors wait on its
// commit. Its caller holds state.mu.
func (s *Service) unusedLocked(state *roomState) bool {
	return state.liveWriter == nil && state.settling == 0 && !s.isSettleTimerArmed(state.settle) &&
		!state.unsettled && state.failed == nil && len(state.connected) == 0 &&
		state.pendingUpdates == 0 && state.durableAppends.Load() == 0 && len(state.pendingVersions) == 0
}

func (s *Service) recordUpdateClass(room string, update []byte, class documentUpdateClass) {
	state := s.lockState(room)
	defer s.unlockState(room, state)
	class.update = append([]byte(nil), update...)
	state.updateClasses = append(state.updateClasses, class)
	state.pendingUpdates++
	if class.durable {
		state.durableAppends.Add(1)
	}
}

func (s *Service) consumeUpdateClass(room string, update []byte) (documentUpdateClass, bool) {
	state := s.lockExistingState(room)
	if state == nil {
		return documentUpdateClass{contentChanged: true}, false
	}
	defer s.unlockState(room, state)
	for index, class := range state.updateClasses {
		if !bytes.Equal(class.update, update) {
			continue
		}
		state.updateClasses = append(state.updateClasses[:index], state.updateClasses[index+1:]...)
		state.pendingUpdates--
		return class, true
	}
	return documentUpdateClass{contentChanged: true}, false
}

func (s *Service) hasPendingUpdates(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.pendingUpdates > 0
}

// finishDurableAppend ends a durable append recordUpdateClass counted. The state that counted it
// holds it until then (unusedLocked), so it is the state found here.
func (s *Service) finishDurableAppend(room string) {
	if state := s.lockExistingState(room); state != nil {
		state.durableAppends.Add(-1)
		s.unlockState(room, state)
	}
}

func (s *Service) hasDurableAppend(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return state.durableAppends.Load() > 0
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
