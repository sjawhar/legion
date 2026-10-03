package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
// key update`, does not block.
//
// A failed room. Its eviction flushes and compacts the document under the advisory lock, so a
// transaction never waits for it (awaitRoomRecovery): an operation that meets a failed room
// fails with ErrServiceUnavailable, and the transaction rolls back. A write whose room fails
// after it opened fails at its next append, the point from which its advisory lock holds off
// any eviction until it ends (applyLive): its slot is on the failed room, which neither the
// reloaded room's settlement nor its next writer sees.
//
// The write's actor, who made its content changes, is credited to the room once the transaction
// commits (Ledger.Commit), never while it may still roll back.
type liveWrite struct {
	artifactID string
	state      *roomState
	// clientID authors every item the transaction writes, on fork: the room's state with the
	// transaction's updates applied, brought up to date before each operation (forkLive). It is
	// chosen when the transaction first forks the document (writerAfter), and zero until then.
	clientID crdt.ClientID
	fork     *crdt.Doc
	// forkedFrom is the room document fork was last brought up to date from, or nil for a fork
	// built from the stored document while no room held it (coldFork).
	forkedFrom *crdt.Doc
	// loads reports that fork, but for this transaction's own writes, is a state ygo loads parking
	// at most loadableItems: it was built from one encoding of the document under that cap
	// (decodeFork), and nothing has reached it since but this transaction's writes
	// (refuseUnloadable).
	loads   bool
	updates [][]byte
	// tree and markdown are the document as this transaction's latest operation left it,
	// rendered once by that operation (applyLive) for the version its transaction may write.
	// forkLive drops them whenever the fork they describe moves.
	tree     *pmdoc.Node
	markdown string
	// anchorsTree is the tree the transaction's anchors were last refreshed against, so the
	// version write does not refresh the same tree's anchors a second time.
	anchorsTree *pmdoc.Node
	// credits are the authors of the transaction's content changes; actor made the latest.
	credits map[string]model.Actor
	actor   *model.Actor
	// loss records what this write's latest batch of operations inserted, so a merge with the
	// room's concurrent changes can be told from a clean one (see lossCheck). A later operation
	// of the same transaction that inserts nothing an operation claims - an accept's margin
	// projection, an anchor refresh - leaves the earlier batch's record in place.
	loss *lossCheck
	// lost names the operations whose text the room did not hold once this write was published,
	// and lostVerdict says the check ran at all: a publish that failed, or a room holding a tree
	// too deep to read, reaches no verdict, which is reported as undetermined rather than as
	// survival.
	lost        []int
	lostVerdict bool
	done        chan struct{}
	finished    bool
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
// to it yet, it first takes the document's owner row, refuses a failed room, and then takes the
// writer slot (see liveWrite).
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
	write := &liveWrite{artifactID: artifactID, done: make(chan struct{})}
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
// then rebuilt from the reloaded room and the transaction's writes. A document no room holds is
// forked from the store instead (coldFork).
func (s *Service) forkLive(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	if s.srv.GetDoc(write.artifactID) == nil {
		return s.coldFork(ctx, write)
	}
	var gained []byte
	var room *crdt.Doc
	var incremental bool
	err := s.srv.Apply(ctx, write.artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		room = doc
		incremental = write.fork != nil && doc == write.forkedFrom
		var since crdt.StateVector
		if incremental {
			since = write.fork.StateVector()
		}
		if write.clientID == 0 {
			write.clientID = writerAfter(doc.StateVector())
		}
		gained = crdt.EncodeStateAsUpdateV1(doc, since)
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	var fork *crdt.Doc
	if incremental {
		fork, err = catchUpFork(write, gained)
	} else {
		// The fork being replaced goes before its replacement is built, so the two are never
		// held at once.
		write.fork, write.forkedFrom = nil, nil
		fork, err = decodeFork(write, gained)
	}
	if err != nil {
		// A fork an update failed to reach is in an unknown state, and so is the rendering taken
		// from it: the next operation rebuilds both.
		write.fork, write.forkedFrom = nil, nil
		write.dropRendering()
		return nil, err
	}
	write.fork, write.forkedFrom = fork, room
	return fork, nil
}

// coldFork returns the fork of a write to a document no room holds: the stored document, which is
// all of it then, with the transaction's writes applied. It loads no room, which would hold a
// second copy of the document for as long as the write runs (LEGION-504); the room loads when the
// write is published, from the store, which by then holds the write. A fork it built is kept for
// the transaction's next operation while no room has loaded since, as nothing else can have
// written the document meanwhile: the transaction's writer slot holds off every other writer and
// settlement, and a browser's edits reach a room. A room that has loaded since is forked from
// again (forkLive).
func (s *Service) coldFork(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	if write.fork != nil && write.forkedFrom == nil {
		return write.fork, nil
	}
	// What the room's Apply checks before it would load the room: the document is not shutting
	// down, its room has not failed, and its issue is open.
	if err := s.allowInject(ctx, websocket.InjectInfo{Room: write.artifactID, Op: websocket.OpApply}); err != nil {
		return nil, fmt.Errorf("%w: %w", websocket.ErrInjectRefused, err)
	}
	// A fork kept from a room that has since been evicted goes before the new one is built.
	write.fork, write.forkedFrom = nil, nil
	write.dropRendering()
	loaded, err := s.persistence.Load(ctx, write.artifactID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.failRoom(write.artifactID, err)
		return nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	fork, err := decodeFork(write, loaded.Update)
	if err != nil {
		s.failRoom(write.artifactID, err)
		return nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	write.fork = fork
	return fork, nil
}

// writerAfter is the client id a transaction's writes to a document take, where writers are the
// document's: one past the highest of them. ygo loads a document's state writer by writer in order
// of their ids and parks every item it reads before the writer that item builds on, so a write by
// a writer read after every writer it builds on parks none of its own items, and the same write
// to the same document leaves the same state (refuseUnloadable). A random id, ygo's default, put a
// new version before the first version it replaced about half the time, and every item it wrote
// was parked. Only a peer can hold the highest id there is, and a write then takes a random one.
func writerAfter(writers crdt.StateVector) crdt.ClientID {
	var highest crdt.ClientID
	for writer := range writers {
		highest = max(highest, writer)
	}
	if highest == math.MaxUint64 {
		return crdt.NewClientID()
	}
	return highest + 1
}

// catchUpFork brings write's kept fork up to date with gained, what its room gained since the
// fork was last brought up to date, and drops a rendering the fork has moved past.
func catchUpFork(write *liveWrite, gained []byte) (*crdt.Doc, error) {
	// Content the room gained moves the fork, so the rendering taken from it no longer describes
	// the document, and the fork no longer holds only the encoding it was loaded from and this
	// transaction's writes (liveWrite.loads). What says the fork moved is the fork itself, read on
	// either side of this one apply: nothing else can be trusted. Two reads of the room are two
	// snapshots - Server.Apply holds no lock across its callback - so an update landing between
	// them is in one and not the other, and the fork would keep a rendering it has already moved
	// past. The fork's own state vector and delete set together are the whole answer, because a
	// deletion creates no struct and so advances no clock, while the update and every observer
	// fire even for an update that integrates nothing.
	watched := write.tree != nil || write.loads
	var wasClocks crdt.StateVector
	var wasDeletes *crdt.IDSet
	if watched {
		wasClocks, wasDeletes = write.fork.StateVector(), crdt.DeleteSetFromDoc(write.fork)
	}
	if err := crdt.ApplyUpdateV1(write.fork, gained, nil); err != nil {
		return nil, fmt.Errorf("bring live document fork up to date: %w", err)
	}
	if watched && !forkHeld(wasClocks, wasDeletes, write.fork) {
		write.dropRendering()
		write.loads = false
	}
	return write.fork, nil
}

// decodeFork builds a new fork for write from state, one encoding of the document's whole state,
// and the transaction's writes. It loads state under the cap refuseUnloadable holds a growing
// write to, and records whether it loaded there (liveWrite.loads); a state that parks more is
// loaded again under ygo's own cap, as a room loads it. A transaction with no writer id yet takes
// one past the writers state holds (writerAfter), which only a load of state names, so its first
// fork reads state twice: once for the writers, once under its id.
func decodeFork(write *liveWrite, state []byte) (*crdt.Doc, error) {
	fork, loads, err := loadForkState(write.clientID, state)
	if err != nil {
		return nil, fmt.Errorf("fork live document: %w", err)
	}
	if write.clientID == 0 {
		write.clientID = writerAfter(fork.StateVector())
		fork = nil
		if fork, loads, err = loadForkState(write.clientID, state); err != nil {
			return nil, fmt.Errorf("fork live document: %w", err)
		}
	}
	for _, update := range write.updates {
		if err := crdt.ApplyUpdateV1(fork, update, nil); err != nil {
			return nil, fmt.Errorf("fork live document with transaction writes: %w", err)
		}
	}
	// A rebuilt fork is a different document from the one the rendering was taken from.
	write.dropRendering()
	write.loads = loads
	return fork, nil
}

// loadForkState loads state into a new document whose writes client authors (ygo's random id
// for zero), under loadableItems and, failing that, under maxPendingItems, and reports whether it
// loaded under the first. Empty state, a document nothing has stored yet, is an empty document.
func loadForkState(client crdt.ClientID, state []byte) (*crdt.Doc, bool, error) {
	load := func(pending int) (*crdt.Doc, error) {
		options := []crdt.DocOption{crdt.WithMaxPendingItems(pending)}
		if client != 0 {
			options = append(options, crdt.WithClientID(client))
		}
		doc := crdt.New(options...)
		if len(state) == 0 {
			return doc, nil
		}
		return doc, crdt.ApplyUpdateV1(doc, state, nil)
	}
	if doc, err := load(loadableItems); err == nil {
		return doc, true, nil
	}
	doc, err := load(maxPendingItems)
	if err != nil {
		return nil, false, err
	}
	return doc, false, nil
}

// dropRendering forgets the rendering and the anchor refresh taken from a fork that has moved.
func (w *liveWrite) dropRendering() {
	w.tree, w.markdown, w.anchorsTree = nil, "", nil
}

// forkHeld reports whether fork still carries exactly the content clocks and deletes describe.
// A deletion advances no clock, so the delete set is half the answer.
func forkHeld(clocks crdt.StateVector, deletes *crdt.IDSet, fork *crdt.Doc) bool {
	now := fork.StateVector()
	if len(clocks) != len(now) {
		return false
	}
	for client, clock := range clocks {
		if now[client] != clock {
			return false
		}
	}
	return sameDeletes(deletes, crdt.DeleteSetFromDoc(fork))
}

// sameDeletes reports whether two delete sets cover the same runs. Ranges are normalized, so
// equal sets compare equal run for run.
func sameDeletes(was, now *crdt.IDSet) bool {
	wasClients, nowClients := was.Clients(), now.Clients()
	if len(wasClients) != len(nowClients) {
		return false
	}
	for _, client := range wasClients {
		wasRanges, nowRanges := was.Ranges(client), now.Ranges(client)
		if len(wasRanges) != len(nowRanges) {
			return false
		}
		for index := range wasRanges {
			if wasRanges[index] != nowRanges[index] {
				return false
			}
		}
	}
	return true
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
// commits: its actor alone, as a service mutation that reaches the room directly is credited
// (creditContentChange). A browser connected to the room made none of it.
func (s *Service) creditLiveWrite(write *liveWrite, actor model.Actor) {
	if write.credits == nil {
		write.credits = make(map[string]model.Actor)
	}
	write.credits[actorKey(actor)] = actor
	write.actor = new(actor)
}

// publishLiveWrite applies write's updates to the room one operation at a time, in the order
// they were made, as a room transaction and a broadcast each. A browser editor then receives
// them as it would have received the operations themselves: an accepted suggestion's text
// change before the margin projection that closes it, never both in one update.
//
// A room that cannot take an update is failed - but not one that refused the publish because it
// had already failed (ErrServiceUnavailable). By the time such a refusal is handled the recovery
// may have finished and registered a replacement room, which holds this write: its append
// committed. Failing that room by name would fail a healthy replacement for the predecessor's
// error, and every write to the document until it recovered again would be refused.
func (s *Service) publishLiveWrite(write *liveWrite) {
	defer s.finishLiveWrite(write)
	// Committed, the write has no more use for its fork, so the fork goes before the publish,
	// which loads the room when no room holds the document (coldFork).
	write.releaseFork()
	for _, update := range write.updates {
		err := s.publishLiveUpdate(write.artifactID, update)
		if err == nil {
			continue
		}
		slog.Error("dispatch: publish committed live document write", "room", write.artifactID, "error", err)
		if s.afterPublishRefused != nil {
			s.afterPublishRefused(write.artifactID)
		}
		if !errors.Is(err, ErrServiceUnavailable) {
			s.failRoom(write.artifactID, err)
		}
		return
	}
	s.recordPublishedLoss(write)
}

// recordPublishedLoss reads the room the write has just reached and records which of the write's
// operations it does not hold: LEGION-269's second window, a concurrent change that landed
// between the version render and this publish. Nothing can be rolled back here - the update is
// durable and the version written - so the verdict rides the caller's response instead
// (Ledger.LostOps).
//
// A write that inserted no text reaches the verdict too, an empty one: a delete, a replace that
// only shortens, a retype, an operation that changed nothing - none of them wrote anything a
// concurrent change could remove, so "nothing was lost" is a statement this can make without
// reading the room, and it is the one those edits deserve. Only a publish that failed, or a room
// holding a tree too deep to read, leaves no verdict, which the caller reports as undetermined.
//
// The read is a point-in-time statement, as the spec says it must be: srv.Apply holds no room
// lock across its callback, so a deletion landing after it is not reported, and a deletion
// landing before it is. It is deliberately taken without a Yjs transaction of its own: an empty
// transaction still fires the room's update observers, so it would put an empty update through
// ygo's persistence worker - a durable doc_updates row - on every edit.
func (s *Service) recordPublishedLoss(write *liveWrite) {
	if write.loss == nil {
		write.lost, write.lostVerdict = nil, true
		return
	}
	doc := s.srv.GetDoc(write.artifactID)
	if doc == nil {
		return
	}
	lost, err := write.loss.lost(doc)
	if err != nil {
		slog.Warn("dispatch: read a published write's text back from its room", "room", write.artifactID, "error", err)
		return
	}
	write.lost, write.lostVerdict = lost, true
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
// it was open. It drops what the write held for its transaction's operations, its fork and their
// updates, keeping the verdict LostOps reads.
func (s *Service) finishLiveWrite(write *liveWrite) {
	if write.finished {
		return
	}
	write.finished = true
	write.releaseFork()
	write.updates = nil
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

// releaseFork drops write's fork and the rendering taken from it, which serve the transaction's
// operations alone. A write can outlive its transaction by a long way: whatever holds the
// transaction's context holds its ledger and so the write, and a pooled pgx connection keeps the
// context of the queries it last served. Kept, a heavy document's fork and tree stayed resident
// after the edit that built them had answered (LEGION-504).
func (w *liveWrite) releaseFork() {
	w.fork, w.forkedFrom = nil, nil
	w.loads = false
	w.dropRendering()
}
