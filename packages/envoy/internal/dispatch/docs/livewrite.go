package docs

import (
	"context"
	"errors"
	"fmt"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
)

// liveWrite is one transaction's writes to one live document. They run on a fork of the room's
// document and are appended inside the transaction; the room applies and broadcasts them only
// once the transaction has committed (PublishLiveWrites). A transaction that does not commit
// leaves the room, and every browser connected to it, as they were (DiscardLiveWrites).
//
// While a liveWrite is open it holds its room's writer slot, so another transaction's joined
// operation on the same document waits for this one to be published or discarded before it
// forks: it then sees exactly the committed writes. It also holds off the room's settlement,
// which would otherwise version the room between this transaction's commit and its publish:
// none is armed while the write is open, and one already running writes no version
// (settleRoom).
type liveWrite struct {
	artifactID string
	state      *roomState
	// clientID authors every item the transaction writes. Each fork is rebuilt from the room's
	// current state plus updates, and reuses it so the transaction's clocks continue.
	clientID crdt.ClientID
	updates  [][]byte
	// rearm records that opening the write stopped an armed settlement timer.
	rearm    bool
	done     chan struct{}
	finished bool
}

// liveWriteOrigin tags the room transaction that applies a committed live write, so the room's
// update observer credits it to its actor rather than to connected browsers. It must remain
// non-zero sized because ygo compares origins by interface equality.
type liveWriteOrigin struct{ _ byte }

var errLiveWriteNeedsCollector = errors.New("dispatch: a live document write joined to a transaction needs an event collector")

func (c *EventCollector) liveWriteFor(artifactID string) *liveWrite {
	if c == nil {
		return nil
	}
	return c.live[artifactID]
}

// joinedLiveWrite is the calling transaction's open write to artifactID, if it has one.
func joinedLiveWrite(ctx context.Context, artifactID string) *liveWrite {
	if _, joined := txFromContext(ctx); !joined {
		return nil
	}
	return eventCollector(ctx).liveWriteFor(artifactID)
}

// openLiveWrite returns the transaction's write to artifactID, first waiting for the room's
// writer slot if another transaction holds it.
func (s *Service) openLiveWrite(ctx context.Context, collector *EventCollector, artifactID string) (*liveWrite, error) {
	if write := collector.liveWriteFor(artifactID); write != nil {
		return write, nil
	}
	write := &liveWrite{artifactID: artifactID, clientID: crdt.NewClientID(), done: make(chan struct{})}
	for {
		state := s.room(artifactID)
		state.mu.Lock()
		if state.liveWriter == nil {
			state.liveWriter = write
			state.gen++
			write.rearm = s.stopSettleTimer(state.settle)
			state.suppressSettle++
			state.mu.Unlock()
			write.state = state
			break
		}
		done := state.liveWriter.done
		state.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if collector.live == nil {
		collector.live = make(map[string]*liveWrite)
	}
	collector.live[artifactID] = write
	collector.order = append(collector.order, artifactID)
	return write, nil
}

// awaitLiveWriter waits until no other transaction holds artifactID's writer slot, so a read
// joined to a transaction sees every write committed before it.
func (s *Service) awaitLiveWriter(ctx context.Context, artifactID string) error {
	for {
		state := s.room(artifactID)
		state.mu.Lock()
		writer := state.liveWriter
		state.mu.Unlock()
		if writer == nil {
			return nil
		}
		select {
		case <-writer.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// forkLive builds the document a transaction's operation on write's room sees: the room's
// current state with the transaction's own writes applied.
func (s *Service) forkLive(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	var state []byte
	err := s.srv.Apply(ctx, write.artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		state = crdt.EncodeStateAsUpdateV1(doc, nil)
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	fork := crdt.New(crdt.WithClientID(write.clientID))
	if err := crdt.ApplyUpdateV1(fork, state, nil); err != nil {
		return nil, fmt.Errorf("fork live document: %w", err)
	}
	for _, update := range write.updates {
		if err := crdt.ApplyUpdateV1(fork, update, nil); err != nil {
			return nil, fmt.Errorf("fork live document with transaction writes: %w", err)
		}
	}
	return fork, nil
}

// joinedFork is the fork holding the calling transaction's writes to artifactID, or nil when it
// has none. A caller joined to a transaction without one first waits until no other
// transaction's write to artifactID is open, so what it reads next includes every committed write.
func (s *Service) joinedFork(ctx context.Context, artifactID string) (*crdt.Doc, error) {
	if write := joinedLiveWrite(ctx, artifactID); write != nil {
		return s.forkLive(ctx, write)
	}
	if _, joined := txFromContext(ctx); joined {
		return nil, s.awaitLiveWriter(ctx, artifactID)
	}
	return nil, nil
}

// docView runs read against the document the caller sees: the joinedFork when there is one,
// otherwise the live room. It returns the room's Apply error, websocket.ErrNoChanges included,
// as Apply does.
func (s *Service) docView(ctx context.Context, artifactID string, read func(*crdt.Doc)) error {
	fork, err := s.joinedFork(ctx, artifactID)
	if err != nil {
		return err
	}
	if fork != nil {
		read(fork)
		return nil
	}
	return s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		read(doc)
	})
}

// PublishLiveWrites applies the live writes of a committed transaction to their rooms and
// broadcasts them. Each update is already durable, so the room's own persistence of it is
// suppressed. A room that cannot take its update is failed, and reloads the durable document on
// its next access.
func (s *Service) PublishLiveWrites(collector *EventCollector) {
	if collector == nil {
		return
	}
	for _, artifactID := range collector.order {
		write := collector.live[artifactID]
		if write.finished {
			continue
		}
		s.publishLiveWrite(write)
	}
}

func (s *Service) publishLiveWrite(write *liveWrite) {
	defer s.finishLiveWrite(write)
	if len(write.updates) == 0 {
		return
	}
	room := write.artifactID
	update, err := mergeUpdates(write.updates)
	if err != nil {
		s.failRoom(room, fmt.Errorf("merge committed live document writes: %w", err))
		return
	}
	origin := &liveWriteOrigin{}
	s.serviceOrigins.Store(origin, struct{}{})
	defer s.serviceOrigins.Delete(origin)
	slot := s.prepareSuppressedPersistence(room)
	var applied []byte
	var applyErr error
	err = s.srv.Apply(context.Background(), room, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		unsubscribe := doc.OnUpdate(func(encoded []byte, updateOrigin any) {
			if updateOrigin == origin {
				applied = append([]byte(nil), encoded...)
			}
		})
		defer unsubscribe()
		applyErr = crdt.ApplyUpdateV1(doc, update, origin)
	})
	if applyErr == nil && err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		applyErr = err
	}
	if applyErr != nil {
		s.cancelSuppressedPersistence(room, slot)
		s.failRoom(room, fmt.Errorf("apply committed live document write: %w", applyErr))
		return
	}
	if applied == nil {
		s.cancelSuppressedPersistence(room, slot)
		return
	}
	s.finishSuppressedPersistence(slot, applied)
	if err := s.srv.BroadcastUpdate(context.Background(), room, applied); err != nil {
		// Connected browsers did not receive the write the room now holds; failing the room
		// closes them, and they sync the durable document when they reconnect.
		s.failRoom(room, fmt.Errorf("broadcast committed live document write: %w", err))
	}
}

// DiscardLiveWrites drops the live writes of a transaction that did not commit. Their rooms
// never saw them. It is a no-op for writes PublishLiveWrites already applied, so handlers defer
// it right after they create the collector.
func (s *Service) DiscardLiveWrites(collector *EventCollector) {
	if collector == nil {
		return
	}
	for _, artifactID := range collector.order {
		s.finishLiveWrite(collector.live[artifactID])
	}
}

// FailLiveWrites fails the rooms of a transaction whose commit returned an error. The commit may
// have gone through, so the rooms cannot tell whether they should hold the writes; failed, they
// reload the durable document, whichever way the commit went.
func (s *Service) FailLiveWrites(collector *EventCollector, cause error) {
	if collector == nil {
		return
	}
	for _, artifactID := range collector.order {
		write := collector.live[artifactID]
		if write.finished {
			continue
		}
		if len(write.updates) > 0 {
			s.failRoom(artifactID, fmt.Errorf("commit live document write: %w", cause))
		}
		s.finishLiveWrite(write)
	}
}

// finishLiveWrite releases write's room: its writer slot, and the settlement it suppressed,
// which is armed again when opening the write stopped one or a settlement was asked for while
// it was open.
func (s *Service) finishLiveWrite(write *liveWrite) {
	if write.finished {
		return
	}
	write.finished = true
	state := write.state
	state.mu.Lock()
	if state.liveWriter == write {
		state.liveWriter = nil
	}
	state.suppressSettle--
	if state.suppressSettle == 0 && (write.rearm || state.settleDeferred) {
		state.settleDeferred = false
		s.scheduleSettleLocked(write.artifactID, state)
	}
	state.mu.Unlock()
	close(write.done)
}
