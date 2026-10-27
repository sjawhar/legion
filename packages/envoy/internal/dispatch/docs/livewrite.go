package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
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
// document's advisory lock (lockDocumentRoom; AppendUpdateTx takes it, and so does a fork whose
// first read finds its room's cursor away from the head, roomBehindHead). A joined read,
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
	// transaction's updates applied, brought up to date before each operation (forkLive).
	clientID crdt.ClientID
	fork     *crdt.Doc
	// forkedFrom is the room document fork was last brought up to date from, held weakly: a
	// write open across a room's eviction must not be what keeps its document resident past it.
	// forkSeq is the room's credit sequence just before it was read for that: each in-flight
	// credit observed by then is for a change the room held, which the fork took in. forkHold is
	// which durable updates that room instance held (roomHold). An upload's version takes out both
	// (uploadCapture). forkCursor is the durable version the fork's basis stands at: that room
	// instance's live cursor, so an append of a peer of the same room that lands while this write
	// runs counts (heldThrough). It is nil for a cold fork, which has no room behind it and whose
	// appends no cursor gates (coldFork, appendingCursor).
	// cold says fork was built from the stored document while no room held it (coldFork), so it
	// has no room to have been brought up to date from. forkInstance is the room instance a warm
	// fork was last brought up to date from, whose updates only the room holds the write stores
	// ahead of its own rows (storeRoomUpdatesAhead); nil for a cold fork.
	forkedFrom   weak.Pointer[crdt.Doc]
	forkSeq      uint64
	forkHold     *roomHold
	forkCursor   *roomCursor
	forkInstance *roomInstance
	cold         bool
	updates      [][]byte
	// versions are the doc_updates versions updates were appended at, one each.
	versions []persistence.Version
	// copies are the versions this transaction stored its room's unstored updates at, ahead of its
	// own rows (storeRoomUpdatesAhead), and heldCopies those of them whose in-flight credit the
	// fork's last read of the room took in (forkSeq), whose pending-author rows an upload's version
	// takes out (uploadCapture). appended is every version the transaction appended, the copies
	// and its own, in the order it appended them (withOwnAppends).
	copies     []persistence.Version
	heldCopies []int64
	appended   []persistence.Version
	// storeAhead stores the room's unstored updates ahead of the row an append of this write is
	// about to store (storeRoomUpdatesAhead); every append of the write's calls it first, under the
	// document's advisory lock (appendUpdateTxClass). storingAhead is set while it runs, so the
	// appends it makes store nothing ahead of themselves.
	storeAhead   func(ctx context.Context, tx pgx.Tx) error
	storingAhead bool
	// tree and markdown are the document as this transaction's latest operation left it,
	// rendered once by that operation (applyLive) for the version its transaction may write, and
	// offsets each identified block's byte range in markdown from that same render.
	// forkLive drops them whenever the fork they describe moves.
	tree     *pmdoc.Node
	markdown string
	offsets  []pmdoc.BlockOffset
	// anchorsTree is the tree the transaction's anchors were last refreshed against, so the
	// version write does not refresh the same tree's anchors a second time.
	anchorsTree *pmdoc.Node
	// credits are the authors of the transaction's content changes no version the transaction
	// wrote lists (Ledger.recordVersion takes out each author a version lists), and creditedBy the
	// append that recorded each one's latest change, which its pending row names; actor made the
	// latest content change, and is nil while the transaction has made none.
	// addedAskBlockIDs is every ask block id a readable operation of this transaction introduced,
	// over its own before/after trees (applyLive), credited to actor at commit (Ledger.creditRooms,
	// registerAskAuthors) rather than guessed from whichever update's observer happens to render
	// this write's publish first (LEGION-503). renamedAskBlockIDs is every id one of those
	// operations' own block-id repair re-minted (registerStampedAskBlocks), excluded from
	// addedAskBlockIDs: a rename is never a genuinely new block, whether or not an author could be
	// carried forward for it, and must not be claimed for this write's actor just because
	// applyLive's own before/after diff cannot otherwise tell a rename from an insertion.
	// carriedAskAuthors is the author carried forward (registerStampedAskBlocks,
	// roomState.carriedAskAuthors) for each id in renamedAskBlockIDs that has one, credited at
	// commit the same way (Ledger.creditRooms, roomState.registerCarriedAskAuthors) rather than
	// written into the room's own bookkeeping as soon as the stamp runs: a write refused after the
	// stamp but before commit (growth, a later validation failure) must not leave the room
	// holding a trace of it. applyLive's own defer clears all three maps whenever its one
	// operation changed the fork but was never recorded - the same guard that drops the fork's
	// rendering - so a growth-refused edit's own stamp never reaches Ledger.creditRooms. That defer
	// runs per applyLive call, so it does not distinguish this call's own stamp from an earlier,
	// already-recorded call's on the same liveWrite; no production caller invokes ApplyOps or
	// ReplaceText more than once per Join (every api/ call site joins once per request), so two
	// calls never share one liveWrite today, and a future one that did would need its own guard.
	credits            map[string]model.Actor
	creditedBy         map[string]persistence.Version
	actor              *model.Actor
	addedAskBlockIDs   map[string]struct{}
	renamedAskBlockIDs map[string]struct{}
	carriedAskAuthors  map[string]model.Actor
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
// committed, and registered the ask ids it introduced before this write's publish could reach any
// observer (Ledger.creditRooms, registerAskAuthors). It carries the write's
// persistence-suppression slot, which that observer finishes (onLoadDocument). It must remain
// non-zero sized because ygo compares origins by interface equality.
type liveWriteOrigin struct {
	slot *suppressSlot
}

// joinedLiveWrite is the calling transaction's open write to artifactID, if it has one.
func joinedLiveWrite(ctx context.Context, artifactID string) *liveWrite {
	return ledgerFrom(ctx).liveWriteFor(artifactID)
}

// joinLiveWrite returns the transaction's write to artifactID. When the transaction has no write
// to it yet, it first has every other process holding a room of the document make that room's
// updates durable (flushOtherRooms), then takes the document's owner row, refuses a failed room,
// and then takes the writer slot (see liveWrite).
func (s *Service) joinLiveWrite(ctx context.Context, ledger *Ledger, artifactID string) (*liveWrite, error) {
	if write := ledger.liveWriteFor(artifactID); write != nil {
		return write, nil
	}
	// The flush runs before any lock of the document's: a per-document gate that orders this
	// process's writers goes ahead of it, never after the owner row or the advisory lock.
	if err := s.flushOtherRooms(ctx, ledger.tx, artifactID); err != nil {
		return nil, err
	}
	if _, _, err := lockArtifactOwner(ctx, ledger.tx, artifactID); err != nil {
		return nil, err
	}
	if err := s.awaitRoomRecovery(ctx, artifactID); err != nil {
		return nil, err
	}
	return s.openLiveWrite(ctx, ledger, artifactID)
}

// flushOtherRooms has every other envoy-dispatch process that holds a room of artifactID store the
// updates that room has applied (a flush query, room_query.go), and waits for their answers, for
// at most roomQueryWait. A write forked after it - from the store, or from this process's room once
// it is at the head (forkLive) - then holds every update any process applied before it asked: an
// edit whose target a person on another process has just deleted fails as it would on one process
// (ErrQuoteNotFound), rather than writing into a paragraph that is gone.
//
// What the wait holds, and why it cannot deadlock: the calling transaction's own connection
// (tx, through which it reads the members, so it takes no second connection of the pool), and no
// lock of this document - not its owner row, not its writer slot, not its advisory lock. The query
// goes through the process's NOTIFY sender, its own connection, and is delivered at once, not at
// tx's commit. What a member's flush waits on is its room's appends, which take this document's
// advisory lock and a key-share lock on its artifact row, neither of which the caller holds. A
// caller holding the advisory lock is a mistake in the lock order the flush depends on; the query
// reads that from the database and skips the flush rather than wait out every member
// (roomQueries.ask).
func (s *Service) flushOtherRooms(ctx context.Context, tx Queryer, artifactID string) error {
	_, _, err := s.roomQueries.ask(ctx, tx, artifactID, roomQueryFlush)
	return err
}

// openLiveWrite takes artifactID's writer slot for the transaction, first waiting for the
// transaction holding it, if one does. A room a close has reserved because its stored history was
// replaced under it (reserveRoomCloseLocked) gives its slot to no one: a write admitted then would be
// forgotten with the room's state, and the room that replaces it would load knowing nothing of the
// write. The transaction does not wait for that close, whose persistence flush takes the
// document's advisory lock (awaitRoomRecovery says why no transaction waits for such work): it
// fails with ErrServiceUnavailable, as on a failed room, and its caller retries once the room has
// reloaded.
//
// Taking the slot moves the room's generation, which ends a settlement already running or about to
// run: one whose timer fired whose settleRoomWithin has not yet read the generation, or one past
// that read whose supersession check or reload (reloadReplacedRoom) then meets the moved
// generation and writes nothing. The write takes that settlement over (settleDeferred), as it takes
// over an armed timer it stops, and finishing it arms the settlement again, whether or not the write
// changed anything: a write that changed nothing publishes no update whose observer would arm one.
// A settlement that got past its supersession check before the write opened commits its own work,
// and the one the write arms then finds nothing new.
func (s *Service) openLiveWrite(ctx context.Context, ledger *Ledger, artifactID string) (*liveWrite, error) {
	write := &liveWrite{artifactID: artifactID, clientID: crdt.NewClientID(), done: make(chan struct{})}
	write.storeAhead = func(ctx context.Context, tx pgx.Tx) error { return s.storeRoomUpdatesAhead(ctx, write, tx) }
	for {
		state := s.lockState(artifactID)
		if state.reloading {
			s.unlockState(artifactID, state)
			slog.Warn("dispatch: refuse a write on a document room closing on a replaced stored history", "room", artifactID)
			return nil, fmt.Errorf("%w: room %s", errRoomReloading, artifactID)
		}
		if state.liveWriter == nil {
			state.liveWriter = write
			state.roomGeneration = s.nextRoomGeneration()
			// The take-over runs whatever the record already says, so it always stops an
			// armed timer: `settleDeferred || takeOver…` would skip the call on a record
			// that already defers one, and leave a timer to fire under this write.
			took := s.takeOverSettlementLocked(state)
			state.settleDeferred = state.settleDeferred || took
			s.unlockState(artifactID, state)
			write.state = state
			break
		}
		done := state.liveWriter.done
		s.unlockState(artifactID, state)
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
		state := s.lockExistingState(artifactID)
		if state == nil {
			return nil
		}
		writer := state.liveWriter
		s.unlockState(artifactID, state)
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
//
// A room behind the durable head is forked from the store too, by the same cold path. The other
// envoy-dispatch process appended what this room's catch-up has not taken in yet, so a write made
// on this room's document would be refused at its append (the gate's derived rule in
// appendUpdateTxClass) and the agent's edit would answer 503; the stored document holds every
// update, so the write is made on that instead and succeeds against the current text. Which room
// that is has to be read exactly, under the document's advisory lock (roomBehindHead): a healthy
// room judged behind is forked from a store that holds less of the document than the room does.
// Uploads and conditional edits hold that lock from before this read until they commit
// (ReplaceText, ApplyOps), so for them the head this compares against is the head their append
// meets from the first read on.
func (s *Service) forkLive(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	live := s.srv.GetDoc(write.artifactID)
	if live == nil {
		return s.coldFork(ctx, write)
	}
	behind, err := s.roomBehindHead(ctx, write, live)
	if err != nil {
		return nil, err
	}
	if behind {
		return s.coldFork(ctx, write)
	}
	return s.warmFork(ctx, write)
}

// roomBehindHead reports whether live, the room resident for write's document, stands behind the
// durable head - an update the other process appended that this room has not caught up yet. The
// head is read in the caller's own transaction, which a write always has (liveWrite): a read of
// its own would be a second pooled connection taken under that transaction
// (store.ErrNestedAcquire). That head includes the versions this transaction has appended itself,
// which are in no room until it publishes, so the room's cursor is read with them folded in
// (liveWrite.withOwnAppends); without that every operation after a transaction's first append
// would judge its own room behind. A document whose room kept no cursor is not judged behind:
// nothing says it is.
//
// A difference the first read sees is not the answer: every durable writer of this process
// advances the room's cursor inside the document's advisory lock, after the commit that raised the
// head (PgVersioned.appendUpdate, Ledger.takeRoomCursors), so a reader holding no lock lands
// between a browser append's commit and its cursor advance and reads a healthy room as one another
// process left behind. The confirmation takes that lock in the caller's own transaction, which
// holds it until the transaction ends, as an upload and a conditional edit already hold it from
// before their fork read (ReplaceText, ApplyOps): under it no write of this process sits between
// its commit and its cursor, and no append lands between the cursor read and the head read beside
// it. It costs that lock only where the cheap read saw a difference, which a healthy room under
// its own browsers' writes sees rarely and a room another process left behind sees until its
// catch-up lands.
//
// Why the false verdict is worth a lock. It sent the write down the cold path (forkLive), which
// reads the stored document - and the store of a room still writing is behind the room by
// whatever its persistence worker has not appended yet, in the order its observers fired rather
// than the order the room applied them (consumeUpdateClass). A version prefix of that is not a
// state the document ever held: an update whose deletes landed without the inserts they replaced
// leaves a tree with no block in it at all, which every write starting from it refuses
// (ErrDocOutsideSchema). A room confirmed behind the head forks from that same store, which its
// own browsers' appends still reach (the gate admits them, appendBrowser), so the cold path costs
// it the same exposure for the moment until its catch-up lands; a warm fork from a room missing
// the other process's rows would instead be refused at its append.
func (s *Service) roomBehindHead(ctx context.Context, write *liveWrite, live *crdt.Doc) (bool, error) {
	cursor := s.cursorOf(live)
	tx, joined := txFromContext(ctx)
	if cursor == nil || !joined {
		return false, nil
	}
	head, err := currentUpdateCursor(ctx, tx, write.artifactID)
	if err != nil {
		return false, err
	}
	if write.withOwnAppends(cursor.heldThrough()) == head {
		return false, nil
	}
	if err := lockDocumentRoom(ctx, tx, write.artifactID); err != nil {
		return false, err
	}
	if head, err = currentUpdateCursor(ctx, tx, write.artifactID); err != nil {
		return false, err
	}
	return write.withOwnAppends(cursor.heldThrough()) != head, nil
}

// warmFork is forkLive's fork of a document a room holds, brought up to date from that room.
func (s *Service) warmFork(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	seq := write.state.creditSeq.Load()
	var gained []byte
	var room *crdt.Doc
	var incremental bool
	err := s.srv.Apply(ctx, write.artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		room = doc
		incremental = write.fork != nil && doc == write.forkedFrom.Value()
		var since crdt.StateVector
		if incremental {
			since = write.fork.StateVector()
		}
		gained = crdt.EncodeStateAsUpdateV1(doc, since)
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	if !incremental {
		// The fork being replaced goes before its replacement is built, so the two are never
		// held at once.
		write.releaseFork()
	}
	fork, err := buildFork(write, incremental, gained)
	if err != nil {
		// A fork an update failed to reach is in an unknown state, and so is the rendering taken
		// from it: the next operation rebuilds both.
		write.releaseFork()
		return nil, err
	}
	// The instance once, rather than its hold and its cursor looked up one after the other: the
	// two are halves of one *roomInstance, and a room evicted between the lookups would leave
	// the write a hold from the gone instance beside a nil cursor, a split the gate is not
	// written for. A write whose room is gone keeps neither.
	instance := s.instanceOf(room)
	var hold *roomHold
	var cursor *roomCursor
	if instance != nil {
		hold, cursor = &instance.hold, &instance.cursor
	}
	write.fork, write.forkedFrom, write.forkSeq, write.cold = fork, weak.Make(room), seq, false
	write.forkHold, write.forkCursor, write.forkInstance = hold, cursor, instance
	if s.afterForkRead != nil {
		s.afterForkRead(write.artifactID)
	}
	return fork, nil
}

// coldFork returns the fork of a write to a document no room holds: the stored document, which is
// all of it then, with the transaction's writes applied. It loads no room, which would hold a
// second copy of the document for as long as the write runs (LEGION-504), and neither does the
// write's publish (publishLiveWrite): the next access loads the room from the store, which by then
// holds the write. A fork it built is kept for the transaction's next operation while no room has
// loaded since: on this process the transaction's writer slot holds off every other writer and
// settlement, and a browser's edits reach a room. Another process can append between this read and
// the write's first append, which takes the document's advisory lock, so a kept fork is brought up
// to the stored head first (followStoredHead). A browser's room load takes no lock the write
// holds (onLoadDocument), so a room can load while this reads the store: a room found once the
// read is done is forked from instead (warmFork), and the copy read from the store is dropped
// undecoded, so the document is never held both in a room and in a fork read from the store. A
// room that loads after that is forked from at the write's next operation (forkLive).
//
// What an upload's version takes out (uploadCapture) is what this read held: the stored updates
// through the head it loaded (forkHold), and no in-flight credit (forkSeq 0), since a credit no
// room holds is a browser edit whose append may not have reached the store this read loaded.
func (s *Service) coldFork(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	if write.fork != nil && write.cold {
		if err := s.followStoredHead(ctx, write); err != nil {
			return nil, err
		}
		return write.fork, nil
	}
	// What the room's Apply checks before it would load the room: the document is not shutting
	// down, its room has not failed, and its issue is open.
	if err := s.allowInject(ctx, websocket.InjectInfo{Room: write.artifactID, Op: websocket.OpApply}); err != nil {
		return nil, fmt.Errorf("%w: %w", websocket.ErrInjectRefused, err)
	}
	// A fork kept from a room that has since been evicted goes before the new one is built.
	write.releaseFork()
	loaded, err := s.persistence.Load(ctx, write.artifactID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		s.failRoom(write.artifactID, err)
		return nil, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	// A room that loaded during the read holds the document; forking it rather than the store
	// keeps one copy beside it, and its browser's edits in the fork. One behind the durable head
	// does not: it is why this write took the cold path, and it holds less of the document than
	// the read just made (forkLive).
	if live := s.srv.GetDoc(write.artifactID); live != nil {
		behind, err := s.roomBehindHead(ctx, write, live)
		if err != nil {
			return nil, err
		}
		if !behind {
			loaded.Update = nil
			return s.warmFork(ctx, write)
		}
	}
	fork := newDocumentCopy(crdt.WithClientID(write.clientID))
	if len(loaded.Update) > 0 {
		if err := crdt.ApplyUpdateV1(fork, loaded.Update, nil); err != nil {
			// A room's load of the same history would fail it, as loadTree does.
			s.failRoom(write.artifactID, fmt.Errorf("decode live document: %w", err))
			return nil, fmt.Errorf("%w: decode live document: %w", ErrDocumentUnloadable, err)
		}
	}
	loaded.Update = nil
	if err := applyTransactionWrites(fork, write.updates); err != nil {
		return nil, err
	}
	write.fork, write.cold, write.forkSeq = fork, true, 0
	write.forkHold = &roomHold{loaded: int64(loaded.Version)}
	// No cursor, so no gate: the gate is about a room (gateAppend), and this write
	// holds none. Its update is a Yjs update, which commutes with whatever landed between this
	// read and its append, and the room that loads after its commit reads the store, which by
	// then holds both. A private cursor at the head this read loaded would refuse the write for
	// an append that is no concern of its own - a browser opening the document behind this read
	// and typing into it (TestABrowserOpeningADocumentMidColdWriteKeepsBothEdits) - and the
	// agent's edit would answer 503 for a conflict Yjs resolves.
	write.forkCursor, write.forkInstance = nil, nil
	if s.afterForkRead != nil {
		s.afterForkRead(write.artifactID)
	}
	return fork, nil
}

// followStoredHead brings write's kept cold fork up to the stored head when its transaction holds
// the document's advisory lock, which it does from its first append to its end (applyLive): every
// stored row past the fork's basis (forkHold) that the transaction did not append itself is merged
// into the fork, and enters forkHold, since the fork now holds it. Another envoy-dispatch process
// can have appended those rows between the fork's read of the store and the transaction's first
// append - a person's delete whose append was waiting for the lock, say - and under the lock no
// more can land. So the version the write renders next, and the loss judged on its fork
// (refuseLostWrite), stand on every stored row the version's update cursor claims. The rows are
// read through the transaction, which holds the lock; they are durable, with their pending
// authors, so the fork may version them. A write that has not appended yet holds no lock, and its
// fork is left as its read made it.
func (s *Service) followStoredHead(ctx context.Context, write *liveWrite) error {
	tx, joined := txFromContext(ctx)
	if !joined || len(write.appended) == 0 {
		return nil
	}
	held := write.forkHold.snapshot()
	_, _, rows, err := readCatchUp(ctx, tx, write.artifactID, held.through)
	if err != nil {
		return err
	}
	versions := make([]int64, 0, len(rows))
	updates := make([][]byte, 0, len(rows))
	for _, row := range rows {
		if slices.Contains(held.since, row.version) || slices.Contains(write.appended, persistence.Version(row.version)) {
			continue
		}
		versions = append(versions, row.version)
		updates = append(updates, row.update)
	}
	if len(updates) == 0 {
		return nil
	}
	batch, err := mergeUpdates(updates)
	if err == nil {
		err = crdt.ApplyUpdateV1(write.fork, batch, nil)
	}
	if err != nil {
		// A fork an update failed to reach is in an unknown state: the next operation rebuilds it.
		write.releaseFork()
		return fmt.Errorf("bring a cold write's fork up to the stored head: %w", err)
	}
	for _, version := range versions {
		write.forkHold.add(version)
	}
	// The rows moved the fork, so the rendering taken from it no longer describes the document.
	write.dropRendering()
	return nil
}

// buildFork brings write's kept fork up to date with gained, what its room gained since the fork
// was last brought up to date, or, when the fork cannot be kept, builds a new one from gained,
// the room's whole state, and the transaction's writes. Either way it drops a rendering the
// fork has moved past.
func buildFork(write *liveWrite, incremental bool, gained []byte) (*crdt.Doc, error) {
	if incremental {
		// Content the room gained moves the fork, so the rendering taken from it no longer
		// describes the document. What says the fork moved is the fork itself, read on either
		// side of this one apply: nothing else can be trusted. Two reads of the room are two
		// snapshots - Server.Apply holds no lock across its callback - so an update landing
		// between them is in one and not the other, and the fork would keep a rendering it
		// has already moved past. The fork's own state vector and delete set together are the
		// whole answer, because a deletion creates no struct and so advances no clock, while
		// the update and every observer fire even for an update that integrates nothing.
		var wasClocks crdt.StateVector
		var wasDeletes *crdt.IDSet
		if write.tree != nil {
			wasClocks, wasDeletes = write.fork.StateVector(), crdt.DeleteSetFromDoc(write.fork)
		}
		if err := crdt.ApplyUpdateV1(write.fork, gained, nil); err != nil {
			return nil, fmt.Errorf("bring live document fork up to date: %w", err)
		}
		if write.tree != nil && !forkHeld(wasClocks, wasDeletes, write.fork) {
			write.dropRendering()
		}
		return write.fork, nil
	}
	fork := newDocumentCopy(crdt.WithClientID(write.clientID))
	if err := crdt.ApplyUpdateV1(fork, gained, nil); err != nil {
		return nil, fmt.Errorf("fork live document: %w", err)
	}
	if err := applyTransactionWrites(fork, write.updates); err != nil {
		return nil, err
	}
	// A rebuilt fork is a different document from the one the rendering was taken from.
	write.dropRendering()
	return fork, nil
}

// applyTransactionWrites applies updates, the writes a transaction has made so far, to fork, a copy
// of the document built without them.
func applyTransactionWrites(fork *crdt.Doc, updates [][]byte) error {
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(fork, update, nil); err != nil {
			return fmt.Errorf("fork live document with transaction writes: %w", err)
		}
	}
	return nil
}

// heldThrough is the durable version the document this write's operations run on holds every
// update through, which gates each of its appends (gateAppend, appendDerived): its fork's basis, read
// now so that a peer's append into the same room, which the fork takes in at its next read,
// counts, with the versions this transaction has appended itself folded in (withOwnAppends). It
// is the cursor of a write whose fork came from a room, and only appendingCursor reads it, which
// hands the gate no cursor at all for a cold write (coldFork).
func (w *liveWrite) heldThrough() int64 {
	return w.withOwnAppends(w.forkCursor.heldThrough())
}

// withOwnAppends folds into base, the version the document a write is made on stands at, the
// versions this transaction has appended itself - its own rows and the room updates it stored
// ahead of them (storeRoomUpdatesAhead): they are in its fork, and in no room's cursor until the
// write commits, so every head read inside its transaction counts them and no cursor does. An
// accept writes its text change and its margin projection as two appends of one transaction, so
// without this the second would find the head one past the fork and be refused.
func (w *liveWrite) withOwnAppends(base int64) int64 {
	for _, version := range w.appended {
		if int64(version) == base+1 {
			base++
		}
	}
	return base
}

// storedAhead is where an update only its room held is stored: 0 while it is nowhere, else the
// version that holds it, which a live write of this process records when it stores the update in
// its own transaction (storeRoomUpdatesAhead) and the room's own append records when it commits
// (PgVersioned.appendUpdate). A version a write recorded is the update's only once that write has
// committed, and its transaction can roll back, so every reader confirms the stored row is the
// update before relying on it (storedAt), under the document's advisory lock, which no
// transaction holds across its own end.
type storedAhead struct{ version atomic.Int64 }

// storedAt is the version holding the update ahead recorded for, when the row stored there is that
// update, read through q under the document's advisory lock. A nil ahead is an update nothing
// stores ahead of its room's append.
func (ahead *storedAhead) storedAt(ctx context.Context, q Queryer, room string, update []byte) (persistence.Version, bool, error) {
	if ahead == nil {
		return 0, false, nil
	}
	at := ahead.version.Load()
	if at == 0 {
		return 0, false, nil
	}
	var stored bool
	if err := q.QueryRow(ctx, `
		select exists(select 1 from doc_updates where artifact_id = $1 and version = $2 and update = $3)
	`, room, at, update).Scan(&stored); err != nil {
		return 0, false, fmt.Errorf("read whether the room's update is stored already: %w", err)
	}
	return persistence.Version(at), stored, nil
}

type storedAheadContextKey struct{}

// withStoredAhead marks a room append's context with where its update may be stored already
// (StoreUpdateContext).
func withStoredAhead(ctx context.Context, ahead *storedAhead) context.Context {
	if ahead == nil {
		return ctx
	}
	return context.WithValue(ctx, storedAheadContextKey{}, ahead)
}

// storedAheadFrom is the storedAhead withStoredAhead marked ctx with, nil for none.
func storedAheadFrom(ctx context.Context) *storedAhead {
	ahead, _ := ctx.Value(storedAheadContextKey{}).(*storedAhead)
	return ahead
}

// listUnstored lists class, an update only this instance holds, as unstored (roomInstance.unstored).
func (instance *roomInstance) listUnstored(class *documentUpdateClass) {
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	instance.unstored = append(instance.unstored, class)
}

// unlistUnstored takes class off the instance's unstored updates once its append has returned. A
// nil instance is a class nothing listed.
func (instance *roomInstance) unlistUnstored(class *documentUpdateClass) {
	if instance == nil {
		return
	}
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	if index := slices.Index(instance.unstored, class); index >= 0 {
		instance.unstored = slices.Delete(instance.unstored, index, index+1)
	}
}

// unstoredUpdates is the instance's unstored updates as of now, in the order the room committed
// them.
func (instance *roomInstance) unstoredUpdates() []*documentUpdateClass {
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	return slices.Clone(instance.unstored)
}

// storeRoomUpdatesAhead appends, in write's own transaction and ahead of the row it is about to
// append, every update the room instance its warm fork came from holds and the store does not
// (plan D3.8). A room's updates reach the store in the order its observers fire, and ygo
// broadcasts a peer's update only after they have, so the store holds no update before the ones it
// depends on - except for this writer: its fork can hold a peer's update whose append is still
// queued behind the room's persistence or waiting for the document's advisory lock, which the
// write holds from its first append until it ends, and its row, made on that update, would commit
// ahead of it. The store would then hold, until that append landed, a state the document never
// held, which every reader of it - a cold fork, a read, a load on another process - would take for
// the document. Storing those updates first keeps every stored prefix a state the room held.
//
// Every append of the write's runs it first (appendUpdateTxClass calls write.storeAhead), under
// the document's advisory lock that append has just taken. The updates are the instance's
// unstored ones (roomInstance.unstored), taken after draining the room's update log
// (observeRoomUpdates): every update committed to the room before the fork was read is classified,
// and listed, by then. They are all of them, in the order the room committed them, so each comes
// after the ones it depends on whether or not the fork holds it. One already stored - by its room's
// append, which committed before this write took the lock, or by an earlier append of this write's
// or a write that committed - is skipped, which storedAt tells from the stored row itself. Each is
// appended as its room's append would have been, with its classification and its browser's
// in-flight credit (AppendUpdateTxWithCredit): the pending authors and the latest edit source it
// writes are its browser's, not the write's actor's, and land, as the write's own, only if the
// transaction commits. Its room's append then finds it stored and writes nothing
// (PgVersioned.appendUpdate); a transaction that rolls back leaves that append to store it.
//
// A cold fork holds no room's update the store lacks, and stores nothing here; neither do the
// appends this makes, which pass through appendUpdateTxClass themselves (storingAhead).
func (s *Service) storeRoomUpdatesAhead(ctx context.Context, write *liveWrite, tx pgx.Tx) error {
	instance := write.forkInstance
	if write.cold || instance == nil || write.storingAhead {
		return nil
	}
	write.storingAhead = true
	defer func() { write.storingAhead = false }()
	if live := write.forkedFrom.Value(); live != nil {
		s.observeRoomUpdates(write.artifactID, live)
	}
	for _, class := range instance.unstoredUpdates() {
		// The transaction reads its own rows, so an update an earlier append of this write stored
		// counts as stored too.
		if _, stored, err := class.ahead.storedAt(ctx, tx, write.artifactID, class.update); err != nil {
			return err
		} else if stored {
			continue
		}
		version, err := s.persistence.AppendUpdateTxWithCredit(ctx, tx, write.artifactID, class.update, class.contentChanged, class.credit)
		if err != nil {
			return fmt.Errorf("store the room's update ahead of the write: %w", err)
		}
		class.ahead.version.Store(int64(version))
		write.copies = append(write.copies, version)
		write.appended = append(write.appended, version)
		if class.credit != nil && class.credit.record.seq <= write.forkSeq {
			write.heldCopies = append(write.heldCopies, int64(version))
		}
	}
	return nil
}

// dropRendering forgets the rendering and the anchor refresh taken from a fork that has moved.
func (w *liveWrite) dropRendering() {
	w.tree, w.markdown, w.offsets, w.anchorsTree = nil, "", nil, nil
}

// releaseFork drops write's fork and the rendering taken from it, which serve the transaction's
// operations alone. A write can outlive its transaction by a long way: whatever holds the
// transaction's context holds its ledger and so the write, and a pooled pgx connection keeps the
// context of the queries it last served. Kept, a heavy document's fork and tree stayed resident
// after the edit that built them had answered, beside the document's room (LEGION-504).
func (w *liveWrite) releaseFork() {
	w.fork, w.forkedFrom, w.cold = nil, weak.Pointer[crdt.Doc]{}, false
	w.dropRendering()
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
// to artifactID is open on this process, then returns nil: what the caller reads next includes
// every committed write, the head it reads next (readStoredHead) included. Outside a transaction
// it takes nothing and returns nil. It never waits for a room's catch-up: a read of a room behind
// the stored head reads the store instead (heldTree, docTree).
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

// readStoredHead is the stored head a read of artifactID compares a resident room's cursor
// against (roomHoldsHead): the greatest stored version, as the gate and a version capture read it
// (currentUpdateCursor). It reads through the caller's transaction when there is one, since a
// second connection of the shared pool taken under it is what deadlocks the pool
// (store.ErrNestedAcquire), and otherwise on the pool that owns loads, as a cold read reads the
// store's stamp (PgVersioned.DocumentStamp), so a caller holding a pooled connection it did not
// join, settlement's transaction, takes no second one either. It takes no lock and waits for
// nothing. It is the one place a read learns the head.
func (s *Service) readStoredHead(ctx context.Context, artifactID string) (int64, error) {
	var q Queryer
	if tx, joined := txFromContext(ctx); joined {
		q = tx
	} else {
		rooms, err := s.store.Pool.Rooms()
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
		}
		q = rooms
	}
	head, err := currentUpdateCursor(ctx, q, artifactID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, fmt.Errorf("%w: %w", ErrServiceUnavailable, err)
	}
	return head, nil
}

// roomHoldsHead reports whether live, a resident room, holds every stored update through head:
// its cursor's contiguous run (roomCursor.heldThrough) stands at or past it. Each cursor advance
// follows the update into the document, so a room it reports holds that much, and a room only
// gains updates, so it holds them for every read after this. A room behind head is missing an
// update another process stored that its catch-up has not applied yet (roomCatchUps), and a read
// takes the stored document instead (storedTree) rather than wait for that catch-up. A room that
// keeps no cursor is not judged behind: nothing says it is (roomBehindHead).
func (s *Service) roomHoldsHead(live *crdt.Doc, head int64) bool {
	cursor := s.cursorOf(live)
	return cursor == nil || cursor.heldThrough() >= head
}

// docTree is the tree of the document the caller sees: the transaction's fork's when there is one
// (joinRead), otherwise the live room's, which it loads, read as of one moment (liveTree): the
// room's peers and the service write the room while a walk reads it. It is never older than the
// stored head read before the room (readStoredHead): a room behind that head, one another process
// wrote past and whose catch-up has not landed, is read from the store (storedTree), and the read
// waits for no catch-up.
func (s *Service) docTree(ctx context.Context, artifactID string) (*pmdoc.Node, error) {
	fork, err := s.joinRead(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if fork != nil {
		return treeOf(fork)
	}
	head, err := s.readStoredHead(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	var tree *pmdoc.Node
	var treeErr error
	behind := false
	err = s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		if !s.roomHoldsHead(doc, head) {
			behind = true
			return
		}
		tree, treeErr = s.liveTree(artifactID, doc)
	})
	if treeErr != nil {
		return nil, treeErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	if behind {
		return s.storedTree(ctx, artifactID)
	}
	return tree, nil
}

// creditLiveWrite records whom a joined content change is credited to once its transaction
// commits: its actor alone, as a service mutation that reaches the room directly is credited
// (creditContentChange). A browser connected to the room made none of it, and no version the
// transaction wrote before holds it.
func (s *Service) creditLiveWrite(write *liveWrite, actor model.Actor) {
	if write.credits == nil {
		write.credits = make(map[string]model.Actor)
		write.creditedBy = make(map[string]persistence.Version)
	}
	key := actorKey(actor)
	write.credits[key] = actor
	write.creditedBy[key] = write.versions[len(write.versions)-1]
	write.actor = new(actor)
}

// publishLiveWrite applies write's updates to the room one operation at a time, in the order
// they were made, as a room transaction and a broadcast each. A browser editor then receives
// them as it would have received the operations themselves: an accepted suggestion's text
// change before the margin projection that closes it, never both in one update.
//
// It publishes only into a room this process holds, resident or loading (roomHeld): a write to a
// document no room holds here - a cold write (coldFork) - loads none, which would hold the
// document in memory with no reader in it (audit finding 11). Its committed rows reach whatever
// room loads next, from the store, and every other process's room through its catch-up
// (roomCatchUps). The settlement the room's update observer would have armed for the published
// update is armed here instead (scheduleSettle; the write's slot defers it to finishLiveWrite), so
// the write is versioned a settle delay on, as a published one is; that settlement loads the room
// it settles. A load already under way when the write committed was told of the write's
// versions before the commit (Ledger.takeRoomCursors) and is listed by ygo from before its read
// of the store, so it is found here and published into: Server.Apply waits for its load. A room
// that retires between this check and the publish's Apply is loaded again by that Apply, from a
// store that holds the write, and idles out like any room a write loads.
//
// A room that cannot take an update is failed - but not one that refused the publish because it
// had already failed (ErrServiceUnavailable). By the time such a refusal is handled the recovery
// may have finished and registered a replacement room, which holds this write: its append
// committed. Failing that room by name would fail a healthy replacement for the predecessor's
// error, and every write to the document until it recovered again would be refused.
func (s *Service) publishLiveWrite(write *liveWrite) {
	defer s.finishLiveWrite(write)
	s.hooks.parkLiveWrite(write.artifactID, PauseAfterCommit)
	// Committed, the write has no more use for its fork, so the fork goes before the publish.
	write.releaseFork()
	if !s.roomHeld(write.artifactID) {
		if len(write.updates) > 0 {
			s.scheduleSettle(write.artifactID)
		}
	} else {
		for index, update := range write.updates {
			err := s.publishLiveUpdate(write.artifactID, update, write.versions[index])
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
	}
	s.recordPublishedLoss(write)
}

// roomHeld reports whether this process holds a room of room, resident or still loading.
func (s *Service) roomHeld(room string) bool {
	return s.srv.GetDoc(room) != nil || slices.Contains(s.srv.Rooms(), room)
}

// publishedLossTimeout bounds what recordPublishedLoss reads: the other processes' flush, a
// catch-up of this process's room, or a load of the stored document. The write's writer slot is
// held until it returns.
const publishedLossTimeout = 10 * time.Second

// recordPublishedLoss judges, once the write is committed and published, which of the write's
// operations the document at its stored head no longer holds, and records it: LEGION-269's second
// window, a concurrent change that landed between the version render and this publish. Nothing
// can be rolled back here - the update is durable and the version written - so the verdict rides
// the caller's response instead (Ledger.LostOps).
//
// A write that inserted no text reaches the verdict too, an empty one: a delete, a replace that
// only shortens, a retype, an operation that changed nothing - none of them wrote anything a
// concurrent change could remove, so "nothing was lost" is a statement this can make without
// reading the document, and it is the one those edits deserve. Only a check that could not read
// the document, or a document holding a tree too deep to read, leaves no verdict, which the caller
// reports as undetermined.
//
// A person on another process can have deleted what the write inserted, in a room this process
// never holds. So first every other process holding a room of the document makes its updates
// durable (a second flush, flushOtherRooms's query, asked now that the write holds no lock), and
// the verdict is judged on the document at the stored head: this process's room, caught up to the
// head first when another process is a member (catchUpRoom), or, with no room here or one that
// could not be caught up, the stored document itself, read without loading a room (on the pool
// document loads use: other writers waiting for this write's slot hold the shared pool's
// connections).
//
// The read is a point-in-time statement, as the spec says it must be: a deletion landing after it
// is not reported, and a deletion landing before it is. A room is read as of one moment under its
// document lock (readLive), since the room's peers write it while the check walks it, and the walk
// (pmdoc.AuthoredTextRuns) takes no lock. The room's update observer has already brought its
// replica up to date with this write, so the check reads the write's own blocks rather than a copy
// of the whole document. It is deliberately taken without a Yjs transaction of its own: an empty
// transaction still fires the room's update observers, so it would put an empty update through
// ygo's persistence worker - a durable doc_updates row - on every edit.
func (s *Service) recordPublishedLoss(write *liveWrite) {
	if write.loss == nil {
		write.lost, write.lostVerdict = nil, true
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishedLossTimeout)
	defer cancel()
	rooms, err := s.store.Pool.Rooms()
	if err != nil {
		slog.Warn("dispatch: reach the document store to judge a published write's text", "room", write.artifactID, "error", err)
		return
	}
	_, others, err := s.roomQueries.ask(ctx, rooms, write.artifactID, roomQueryFlush)
	if err != nil {
		// Whether another process holds the document is unknown, so the verdict reads the head.
		slog.Warn("dispatch: ask the other processes to store their rooms' updates before judging a published write's text",
			"room", write.artifactID, "error", err)
		others = true
	}
	lost, err := s.lostAtStoredHead(ctx, rooms, write, others)
	if err != nil {
		slog.Warn("dispatch: read a published write's text back from the document", "room", write.artifactID, "error", err)
		return
	}
	write.lost, write.lostVerdict = lost, true
}

// lostAtStoredHead is write's lost operations on the document at its stored head: this process's
// room - caught up through rooms first when others says another process may have stored what it
// lacks - or the stored document when no room is resident here or its catch-up did not bring it
// to the head.
func (s *Service) lostAtStoredHead(ctx context.Context, rooms Queryer, write *liveWrite, others bool) ([]int, error) {
	room := write.artifactID
	if live := s.srv.GetDoc(room); live != nil {
		current := true
		if others {
			outcome, err := s.catchUpRoom(ctx, rooms, room, live)
			current = err == nil && (outcome == catchUpCurrent || outcome == catchUpApplied)
		}
		if current {
			var lost []int
			var lossErr error
			if err := s.readLive(room, live, func(doc *crdt.Doc) {
				lost, lossErr = write.loss.lost(doc)
			}); err != nil {
				return nil, fmt.Errorf("copy the room: %w", err)
			}
			return lost, lossErr
		}
	}
	loaded, err := s.persistence.Load(ctx, room)
	if err != nil {
		return nil, fmt.Errorf("load the stored document: %w", err)
	}
	doc := newDocumentCopy()
	if len(loaded.Update) > 0 {
		if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
			return nil, fmt.Errorf("decode the stored document: %w", err)
		}
	}
	return write.loss.lost(doc)
}

// publishLiveUpdate applies one committed update to the room and broadcasts it. It reads nothing
// from the shared pool: the transaction that wrote the update checked the document's owner and
// held its row until it committed, so the injections are owner-verified. Other transactions'
// writes to the document hold pooled connections while they wait for this write's slot, so a
// publish that asked the shared pool for the issue state could wait on them for good.
//
// The update is already durable, so the room's persistence must not append it again: the
// suppression slot the publish queues is finished by the room's own update observer, with the
// bytes ygo's persistence observer is handed next (onLoadDocument), never after Apply returns. A
// room whose persistence worker CloseRoom retired under this Apply hands the commit to ygo's
// stranded persistence on this goroutine, which would otherwise wait on this slot for good.
func (s *Service) publishLiveUpdate(room string, update []byte, version persistence.Version) error {
	ctx := withOwnerVerified(context.Background())
	slot := s.prepareSuppressedPersistence(room)
	origin := &liveWriteOrigin{slot: slot}
	recorded, err := s.applyCaptured(ctx, room, origin, func(doc *crdt.Doc) error {
		instance := s.instanceOf(doc)
		if instance != nil {
			instance.hold.add(int64(version))
			// The room takes the update this version is stored at, here, where it reaches the
			// room's document: the write appended it outside the room, so nothing else advances
			// this instance's cursor for it, and a room left behind by its own committed write
			// would be refused everything until it reloaded.
			advanceRoomCursor(&instance.cursor, version)
		}
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
	if err := s.srv.BroadcastUpdate(ctx, room, applied); err != nil {
		// A room CloseRoom retired under this Apply has no peer left to tell.
		if errors.Is(err, websocket.ErrRoomNotFound) {
			return nil
		}
		// Connected browsers did not receive the write the room now holds; failing the room
		// closes them, and they sync the durable document when they reconnect.
		return fmt.Errorf("broadcast committed live document write: %w", err)
	}
	return nil
}

// finishLiveWrite releases write's room: its writer slot, and the settlement it suppressed, which
// is armed again when opening the write took one over (takeOverSettlementLocked) or a settlement
// was asked for while it was open. It drops what the write held for its transaction's operations,
// its fork and their updates, keeping the verdict LostOps reads.
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
	s.unlockState(write.artifactID, state)
	close(write.done)
}
