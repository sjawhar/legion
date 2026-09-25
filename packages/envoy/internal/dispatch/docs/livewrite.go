package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// liveWrite is one transaction's writes to one live document. They run on a fork of the room's
// document and are appended inside the transaction; the room applies and broadcasts them only
// once the transaction has committed (Ledger.Commit). A transaction that does not commit leaves
// the room, and every browser connected to it, as they were (Ledger.Discard).
//
// While a liveWrite is open it holds its room's writer slot, so another transaction's joined
// operation on the same document waits for this one to be published or discarded before it
// forks: it then sees exactly the committed writes. It also holds off the room's settlement,
// which would otherwise version the room between this transaction's commit and its publish:
// none is armed while the write is open, and one already running writes no version
// (settleRoom).
//
// Locks. A transaction takes a live document's locks in one order and holds each until it
// commits, the slot until it publishes: the document's owner row (lockArtifactOwner: its
// issue's row, or a project document's own artifact row), then the writer slot, then the
// document's advisory lock (lockDocumentRoom; AppendUpdateTx takes it too). A joined read,
// which waits for the slot but writes nothing, takes the owner row alone. Postgres cannot see a
// wait for the slot, which is in memory. Every transaction that waits for or holds it holds the
// owner row first, so a transaction waiting for the slot waits only for a committed write's
// publish, which takes no database lock; and it holds no advisory lock while it waits, so a
// failed room's eviction, whose compaction takes that lock, is never held up by a waiter.
// Durable writers outside a transaction (ygo's persistence worker, compaction) take the advisory
// lock and then only the foreign-key share lock on the owner row, which the owner lock, `for no
// key update`, does not block. A write recovers a failed room before it takes the slot, so its
// slot is on the room state that recovery left: taken on a failed one, it would hold off
// neither the reloaded room's settlement nor the next write.
//
// The write's actor, and the browsers connected when it changed the content, are credited to
// the room once the transaction commits (Ledger.Commit), never while it may still roll back.
type liveWrite struct {
	artifactID string
	state      *roomState
	// clientID authors every item the transaction writes, on fork: the room's state with the
	// transaction's updates applied, brought up to date before each operation (forkLive).
	clientID crdt.ClientID
	fork     *crdt.Doc
	// forkedFrom is the room document fork was last brought up to date from.
	forkedFrom *crdt.Doc
	updates    [][]byte
	// credits are the authors of the transaction's content changes; actor made the latest.
	credits  map[string]model.Actor
	actor    *model.Actor
	done     chan struct{}
	finished bool
}

// liveWriteOrigin tags the room transaction that applies a committed live write, so the room's
// update observer credits it to no one: Ledger.Commit credited it when its transaction
// committed. It must remain non-zero sized because ygo compares origins by interface equality.
type liveWriteOrigin struct{ _ byte }

// joinedLiveWrite is the calling transaction's open write to artifactID, if it has one.
func joinedLiveWrite(ctx context.Context, artifactID string) *liveWrite {
	return ledgerFrom(ctx).liveWriteFor(artifactID)
}

// joinLiveWrite returns the transaction's write to artifactID. When the transaction has no write
// to it yet, it first takes the document's owner row and then, once a failed room has
// recovered, the writer slot (see liveWrite).
func (s *Service) joinLiveWrite(ctx context.Context, ledger *Ledger, artifactID string) (*liveWrite, error) {
	if write := ledger.liveWriteFor(artifactID); write != nil {
		return write, nil
	}
	if _, _, err := lockArtifactOwner(ctx, ledger.tx, artifactID); err != nil {
		return nil, err
	}
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return nil, err
	}
	return s.openLiveWrite(ctx, ledger, artifactID)
}

// openLiveWrite takes artifactID's writer slot for the transaction, first waiting for the
// transaction holding it, if one does.
func (s *Service) openLiveWrite(ctx context.Context, ledger *Ledger, artifactID string) (*liveWrite, error) {
	write := &liveWrite{artifactID: artifactID, clientID: crdt.NewClientID(), done: make(chan struct{})}
	for {
		state := s.room(artifactID)
		state.mu.Lock()
		if state.liveWriter == nil {
			state.liveWriter = write
			state.gen++
			state.settleDeferred = s.stopSettleTimer(state.settle)
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
	ledger.addLiveWrite(write)
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

// forkLive returns the document a transaction's operation on write's room sees: the room's
// current state with the transaction's own writes applied. The fork is kept on write, and each
// call brings it up to date with only what the room gained since, so an operation does not
// re-encode the whole room. A room that was reloaded in between is a different document, which
// may lack state the kept fork still holds (a browser update whose append failed), so the fork is
// then rebuilt from the reloaded room and the transaction's writes.
func (s *Service) forkLive(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	var gained []byte
	var room *crdt.Doc
	err := s.srv.Apply(ctx, write.artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		room = doc
		var since crdt.StateVector
		if write.fork != nil && doc == write.forkedFrom {
			since = write.fork.StateVector()
		}
		gained = crdt.EncodeStateAsUpdateV1(doc, since)
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	if write.fork != nil && room == write.forkedFrom {
		if err := crdt.ApplyUpdateV1(write.fork, gained, nil); err != nil {
			write.fork = nil
			return nil, fmt.Errorf("bring live document fork up to date: %w", err)
		}
		return write.fork, nil
	}
	fork := crdt.New(crdt.WithClientID(write.clientID))
	if err := crdt.ApplyUpdateV1(fork, gained, nil); err != nil {
		return nil, fmt.Errorf("fork live document: %w", err)
	}
	for _, update := range write.updates {
		if err := crdt.ApplyUpdateV1(fork, update, nil); err != nil {
			return nil, fmt.Errorf("fork live document with transaction writes: %w", err)
		}
	}
	write.fork = fork
	write.forkedFrom = room
	return fork, nil
}

// joinRead joins a read of artifactID to the calling transaction, as joinLiveWrite joins a write.
// With a write of its own open it returns the fork holding that write. Otherwise, for a caller in
// a transaction, it takes the document's owner row and waits until no other transaction's write
// to artifactID is open, then returns nil: what the caller reads next includes every committed
// write. Outside a transaction it takes nothing and returns nil.
func (s *Service) joinRead(ctx context.Context, artifactID string) (*crdt.Doc, error) {
	if write := joinedLiveWrite(ctx, artifactID); write != nil {
		return s.forkLive(ctx, write)
	}
	if tx, joined := txFromContext(ctx); joined {
		if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
			return nil, err
		}
		return nil, s.awaitLiveWriter(ctx, artifactID)
	}
	return nil, nil
}

// docView runs read against the document the caller sees: the transaction's fork when there is
// one (joinRead), otherwise the live room, which it loads.
func (s *Service) docView(ctx context.Context, artifactID string, read func(*crdt.Doc)) error {
	fork, err := s.joinRead(ctx, artifactID)
	if err != nil {
		return err
	}
	if fork != nil {
		read(fork)
		return nil
	}
	err = s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		read(doc)
	})
	if errors.Is(err, websocket.ErrNoChanges) {
		return nil
	}
	return err
}

// creditLiveWrite records whom a joined content change is credited to once its transaction
// commits: the actor, and every browser connected to the room, as a change that reaches the
// room directly is credited (recordConnectedActors).
func (s *Service) creditLiveWrite(write *liveWrite, actor model.Actor) {
	if write.credits == nil {
		write.credits = make(map[string]model.Actor)
	}
	state := s.room(write.artifactID)
	state.mu.Lock()
	for _, connected := range state.connected {
		write.credits[actorKey(connected)] = connected
	}
	state.mu.Unlock()
	write.credits[actorKey(actor)] = actor
	write.actor = new(actor)
}

// publishLiveWrite applies write's updates to the room one operation at a time, in the order
// they were made, as a room transaction and a broadcast each. A browser editor then receives
// them as it would have received the operations themselves: an accepted suggestion's text
// change before the margin projection that closes it, never both in one update.
func (s *Service) publishLiveWrite(write *liveWrite) {
	defer s.finishLiveWrite(write)
	for _, update := range write.updates {
		if err := s.publishLiveUpdate(write.artifactID, update); err != nil {
			slog.Error("dispatch: publish committed live document write", "room", write.artifactID, "error", err)
			s.failRoom(write.artifactID, err)
			return
		}
	}
}

// publishLiveUpdate applies one committed update to the room and broadcasts it. It reads nothing
// from the shared pool: the transaction that wrote the update checked the document's owner and
// held its row until it committed, so the injections are owner-verified. Other transactions'
// writes to the document hold pooled connections while they wait for this write's slot, so a
// publish that asked the shared pool for the issue state could wait on them for good.
func (s *Service) publishLiveUpdate(room string, update []byte) error {
	ctx := withOwnerVerified(context.Background())
	origin := &liveWriteOrigin{}
	slot := s.prepareSuppressedPersistence(room)
	recorded, err := s.applyCaptured(ctx, room, origin, func(doc *crdt.Doc) error {
		return crdt.ApplyUpdateV1(doc, update, origin)
	})
	if err != nil {
		s.cancelSuppressedPersistence(room, slot)
		return fmt.Errorf("apply committed live document write: %w", err)
	}
	if len(recorded) == 0 {
		s.cancelSuppressedPersistence(room, slot)
		return nil
	}
	applied, err := mergeUpdates(recorded)
	if err != nil {
		s.cancelSuppressedPersistence(room, slot)
		return fmt.Errorf("merge committed live document write: %w", err)
	}
	s.finishSuppressedPersistence(slot, applied)
	if err := s.srv.BroadcastUpdate(ctx, room, applied); err != nil {
		// Connected browsers did not receive the write the room now holds; failing the room
		// closes them, and they sync the durable document when they reconnect.
		return fmt.Errorf("broadcast committed live document write: %w", err)
	}
	return nil
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
	state.liveWriter = nil
	if state.settleDeferred {
		state.settleDeferred = false
		s.scheduleSettleLocked(write.artifactID, state)
	}
	state.mu.Unlock()
	close(write.done)
}
