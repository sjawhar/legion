package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"weak"

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
// any eviction until it ends (recordLiveOperation): its slot is on the failed room, which neither
// the reloaded room's settlement nor its next writer sees.
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
	// has no room to have been brought up to date from.
	forkedFrom weak.Pointer[crdt.Doc]
	forkSeq    uint64
	forkHold   *roomHold
	forkCursor *roomCursor
	cold       bool
	updates    [][]byte
	// renderings are, one for each of updates and index for index, what the room's update
	// observer may take in place of rendering the room after that update (publishedRendering):
	// the operation's own rendering of the fork it left, for an operation whose fork was read warm,
	// and the zero value, which vouches for nothing, for one read cold. They go with updates
	// (finishLiveWrite).
	renderings []publishedRendering
	// versions are the doc_updates versions updates were appended at, one each.
	versions []persistence.Version
	// marginRooms names, for each of updates, the room document a margin write's fork was
	// warm-built from (applyMargin), held weakly as forkedFrom is: its publish tags the update
	// margin-only when it reaches that same room instance (liveWriteOrigin.marginOnly). It is the
	// zero pointer for every other update, and for a margin write made on a cold fork, which no
	// room was behind. recordLiveOperation appends it beside the update, so the two never fall
	// out of step.
	marginRooms []weak.Pointer[crdt.Doc]
	// tree and markdown are the document as this transaction's latest operation left it,
	// rendered once for the version its transaction may write - by applyLive, or by a margin
	// write (applyMargin) that found none kept - and offsets each identified block's byte range
	// in markdown from that same render. A later margin write, which leaves the fragment they
	// describe as it was, reuses them rather than rendering again. forkLive drops them whenever
	// the fork they describe moves, so while tree is set markdown is also the rendering of the
	// fork as the transaction's next operation finds it: that operation takes it as its rendering
	// before it applies, known without rendering the fork again (applyLive).
	tree     *pmdoc.Node
	markdown string
	offsets  []pmdoc.BlockOffset
	// forkMarkdown is the fork's rendering as forkLive left it, known without rendering it: the
	// room replica's rendering of the very state the fork was read from (warmFork says when that
	// is exact), nil when nothing vouches for it. Every forkLive clears it before it reads, so
	// no fork read inherits an earlier read's; dropRendering clears it with the rest of the
	// rendering, and applyLive takes it, once, before its operation moves the fork. It shares
	// the replica's string, so it holds no copy of the document of its own.
	forkMarkdown *string
	// forkRead is the room replica's epoch and foreign count as the latest fork read found them
	// just before it encoded the room (warmFork), zero for a fork no replica stood behind: a cold
	// fork, which no room's state went into. forkLive clears it with forkMarkdown.
	forkRead forkReadMark
	// anchorsTree is the tree the transaction's anchors were last refreshed against, so the
	// version write does not refresh the same tree's anchors a second time.
	anchorsTree *pmdoc.Node
	// credits are the authors of the transaction's content changes no version the transaction
	// wrote lists (Ledger.recordVersion takes out each author a version lists), and creditedBy the
	// append that recorded each one's latest change, which its pending row names; actor made the
	// latest content change, and is nil while the transaction has made none. A server lifecycle
	// write (WriteIssueAskStates, withLifecycleWrite) is no edit, so it leaves credits, creditedBy
	// and actor as they were (applyLive).
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
	// holding a trace of it. liveOperation.abandon, which applyLive and applyMargin both defer,
	// clears all three maps whenever its one operation changed the fork but was never recorded, or
	// panicked - the same guard that drops the fork's rendering - so a growth-refused edit's own
	// stamp never reaches Ledger.creditRooms. It runs per operation, so it does not distinguish
	// this operation's own stamp from an earlier, already-recorded one's on the same liveWrite; no
	// production caller invokes ApplyOps or ReplaceText more than once per Join (every api/ call
	// site joins once per request), so two calls never share one liveWrite today, and a future
	// one that did would need its own guard.
	credits    map[string]model.Actor
	creditedBy map[string]persistence.Version
	actor      *model.Actor
	// creditSeq is the room's credit sequence this write's latest content operation took
	// (roomState.creditSeq), 0 while it has made none: the one ordering that compares its own
	// edits with the browser edits the room observed beside them (latestEditSource).
	creditSeq          uint64
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

// forkReadMark is what a warm fork read found of the room replica (renderedReplica) just before it
// encoded the room's state: the replica's epoch and foreign count, read without its lock
// (renderedReplica.mark). warm says a replica stood behind the read at all; a mark that is not
// warm vouches for nothing. A replica that later holds every logged commit at the same epoch and
// foreign count (renderedReplica.standsAt) consumed nothing since but published live writes'
// commits (renderedReplica.foreign says why).
type forkReadMark struct {
	epoch, foreign uint64
	warm           bool
}

// liveWriteOrigin tags the room transaction that applies a committed live write, so the room's
// update observer credits it to no one: Ledger.Commit credited it when its transaction
// committed, and registered the ask ids it introduced before this write's publish could reach any
// observer (Ledger.creditRooms, registerAskAuthors). It carries the write's
// persistence-suppression slot, which that observer finishes (onLoadDocument). It must remain
// non-zero sized because ygo compares origins by interface equality.
//
// It also carries what lets the observer classify the update without rendering the room
// (renderedReplica.changedBy): rendering, the operation's own rendering of the state the update
// leaves, which the observer takes out once it has consumed the update (renderedReplica.consumed),
// so no markdown outlives that; and inRoom, which says the room instance the update is published
// into loaded it already (roomInstance.loadedHead). publishLiveUpdate sets inRoom on the
// publishing goroutine before the update reaches the room, and the observer reads it only once the
// room's log has handed the update over.
type liveWriteOrigin struct {
	slot      *suppressSlot
	rendering publishedRendering
	inRoom    bool
	// marginOnly says the update is a margin write (applyMargin) whose fork was warm-built from
	// the very room document it is applied to, so it cannot change the document's XML fragment
	// and the room's update observer keeps the rendering it has rather than rendering the
	// replica again (renderedReplica.observe). The fork was room instance R's whole state, its
	// delete set included, plus the transaction's earlier updates, which publishLiveWrite
	// publishes into R before this one, in order, stopping at the first that fails. ygo encodes
	// every update with the whole delete set of the document it was made on (sjawhar/ygo
	// crdt/update.go, buildDeleteSet), so every delete the margin update carries is already in R
	// but the marks map value it replaced, and every struct it adds is an item of the marks map.
	// The replica is R as of the commit before this one (the update log's order), so neither the
	// fragment nor its rendering can move. A margin write made on a cold fork is never tagged: its
	// room stood behind the durable head, and the store it forked holds another process's deletes
	// R lacks, which the update carries in. Nor is one published into a room replaced or reloaded
	// since its fork, which can lack deletes the fork held (a browser update whose append failed).
	// It is set inside the publish's Apply, against the document the update reaches, beside
	// inRoom (publishLiveUpdate).
	marginOnly bool
}

// publishedRendering is one operation's rendering of the fork it left, kept for the room's update
// observer (renderedReplica.changedBy): markdown, and askBlocks, the ids of the ask blocks that
// tree holds, walked only when the operation changed the rendered markdown, since only then does
// the observer record them. The embedded forkReadMark is the room replica's epoch and foreign
// count as the operation's own fork read found them; the rendering is the replica's after the
// update only while the replica still stands at both (renderedReplica says why). Its warm says
// the operation forked warm: the zero value, a cold operation's, vouches for nothing, since a
// cold fork was read from the store and no replica's state went into it.
type publishedRendering struct {
	forkReadMark
	markdown  string
	askBlocks map[string]struct{}
}

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
// transaction holding it, if one does. A room a close has reserved because it stands behind the
// durable head (reserveRoomCloseLocked) gives its slot to no one: a write admitted then would be
// forgotten with the room's state, and the room that replaces it would load knowing nothing of the
// write. The transaction does not wait for that close, whose persistence flush takes the
// document's advisory lock (awaitRoomRecovery says why no transaction waits for such work): it
// fails with ErrServiceUnavailable, as on a failed room, and its caller retries once the room has
// reloaded.
//
// Taking the slot moves the room's generation, which ends a settlement already running or about to
// run: one whose timer fired whose settleRoomWithin has not yet read the generation, or one past
// that read whose supersession check or reload (reloadRoomBehindHead) then meets the moved
// generation and writes nothing. The write takes that settlement over (settleDeferred), as it takes
// over an armed timer it stops, and finishing it arms the settlement again, whether or not the write
// changed anything: a write that changed nothing publishes no update whose observer would arm one.
// A settlement that got past its supersession check before the write opened commits its own work,
// and the one the write arms then finds nothing new.
func (s *Service) openLiveWrite(ctx context.Context, ledger *Ledger, artifactID string) (*liveWrite, error) {
	write := &liveWrite{artifactID: artifactID, clientID: crdt.NewClientID(), done: make(chan struct{})}
	for {
		state := s.lockState(artifactID)
		if state.reloading {
			s.unlockState(artifactID, state)
			slog.Warn("dispatch: refuse a write on a document room closing behind the durable head", "room", artifactID)
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
// envoy-dispatch process appended what this room never took, so a write made on this room's
// document would be refused at its append (the gate in appendUpdateTxClass) and the agent's edit
// would answer 503; the stored document holds every update, so the write is made on that instead
// and succeeds against the current text. Which room that is has to be read exactly, under the
// document's advisory lock (roomBehindHead): a healthy room judged behind is forked from a store
// that holds less of the document than the room does. Uploads and conditional edits hold that
// lock from before this read until they commit (ReplaceText, ApplyOps), so for them the head this
// compares against is the head their append meets from the first read on.
func (s *Service) forkLive(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	write.forkMarkdown, write.forkRead = nil, forkReadMark{}
	// A document no room holds, or whose room stands behind the durable head, is forked cold.
	live := s.srv.GetDoc(write.artifactID)
	cold := live == nil
	var err error
	if !cold {
		if cold, err = s.roomBehindHead(ctx, write, live); err != nil {
			return nil, err
		}
	}
	var fork *crdt.Doc
	if cold {
		fork, err = s.coldFork(ctx, write)
	} else {
		fork, err = s.warmFork(ctx, write)
	}
	if err != nil {
		return nil, err
	}
	if s.checkRendering != nil {
		s.checkRendering("fork", write)
	}
	return fork, nil
}

// roomBehindHead reports whether live, the room resident for write's document, stands behind the
// durable head - an update the other process appended that this room never took. The head is read
// in the caller's own transaction, which a write always has (liveWrite): a read of its own would
// be a second pooled connection taken under that transaction (store.ErrNestedAcquire). That head
// includes the versions this transaction has appended itself, which are in no room until it
// publishes, so the room's cursor is read with them folded in (liveWrite.withOwnAppends);
// without that every operation after a transaction's first append would judge its own room
// behind. A document whose room kept no cursor is not judged behind: nothing says it is.
//
// A difference the first read sees is not the answer, for the reason the stale-room evictor reads
// twice as well (documentChanged): every durable writer of this process advances the room's
// cursor inside the document's advisory lock, after the commit that raised the head
// (PgVersioned.appendUpdate, Ledger.takeRoomCursors), so a reader holding no lock lands between a
// browser append's commit and its cursor advance and reads a healthy room as one another process
// left behind. The confirmation takes that lock in the caller's own transaction, which holds it
// until the transaction ends, as an upload and a conditional edit already hold it from before
// their fork read (ReplaceText, ApplyOps): under it no write of this process sits between its
// commit and its cursor, and no append lands between the cursor read and the head read beside it.
// It costs that lock only where the cheap read saw a difference, which a healthy room under its
// own browsers' writes sees rarely and a room another process left behind sees once and then
// always.
//
// Why the false verdict is worth a lock. It sent the write down the cold path (forkLive), which
// reads the stored document - and the store of a room still writing is behind the room by
// whatever its persistence worker has not appended yet, in the order its observers fired rather
// than the order the room applied them (consumeUpdateClass). A version prefix of that is not a
// state the document ever held: an update whose deletes landed without the inserts they replaced
// leaves a tree with no block in it at all, which every write starting from it refuses
// (ErrDocOutsideSchema). A room confirmed behind the head has no such store: nothing it still
// owes can become durable, because the gate refuses its every append (refuseRoomBehindHead) until
// it is reloaded, so the stored document is the whole of what this process can still write from.
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
//
// It records on write the room replica's epoch and foreign count as it finds them just before it
// encodes the room's state (forkRead), and, for the transaction's first write (no update of its
// own yet), takes the replica's rendering as the fork's (forkMarkdown) when the replica, once it
// holds every logged commit (renderedReplica.hold), still stands at both. That is exact: the
// counters were read before the encode, so a replica found at both under hold has consumed, since
// that read, only published live writes' commits; under the writer slot those can only be this
// write's own published updates, and it has none before its first operation's publish
// (publishLiveWrite runs after the commit, and finishLiveWrite releases the slot only after it).
// The replica therefore holds what the room held at the encode, which is the fork: the room's
// state, and no write of the transaction's. A foreign commit the replica consumes after the read
// moves the count, the safe way; recover draws a fresh epoch, and epochs are unique in the
// process, so a replica of another room instance never matches. A replica that is busy, behind
// the room, or outside the schema (a nil markdown) vouches for nothing, and applyLive renders the
// fork. No pointer to the replica outlives this call.
func (s *Service) warmFork(ctx context.Context, write *liveWrite) (*crdt.Doc, error) {
	seq := write.state.creditSeq.Load()
	var gained []byte
	var room *crdt.Doc
	var incremental bool
	var replica *renderedReplica
	var read forkReadMark
	err := s.srv.Apply(ctx, write.artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		room = doc
		incremental = write.fork != nil && doc == write.forkedFrom.Value()
		// The counters before the encode, never after it: read after, they would count a foreign
		// commit landing between the two, which the replica holds and the fork does not.
		if replica = s.replicaOf(doc); replica != nil {
			read = replica.mark()
		}
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
	var hold *roomHold
	var cursor *roomCursor
	if instance := s.instanceOf(room); instance != nil {
		hold, cursor = &instance.hold, &instance.cursor
	}
	write.fork, write.forkedFrom, write.forkSeq, write.cold = fork, weak.Make(room), seq, false
	write.forkHold, write.forkCursor, write.forkRead = hold, cursor, read
	if len(write.updates) == 0 && replica != nil {
		if doc := replica.hold(); doc != nil {
			if replica.standsAt(read) && replica.markdown != nil {
				write.forkMarkdown = replica.markdown
			}
			replica.mu.Unlock()
		}
	}
	if s.afterForkRead != nil {
		s.afterForkRead(write.artifactID)
	}
	return fork, nil
}

// coldFork returns the fork of a write to a document no room holds: the stored document, which is
// all of it then, with the transaction's writes applied. It loads no room, which would hold a
// second copy of the document for as long as the write runs (LEGION-504); the room loads when the
// write is published, from the store, which by then holds the write. A fork it built is kept for
// the transaction's next operation while no room has loaded since, as nothing else can have
// written the document meanwhile: the transaction's writer slot holds off every other writer and
// settlement, and a browser's edits reach a room. A browser's room load takes no lock the write
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
	// No cursor, so no gate: the gate is about a room (refuseRoomBehindHead), and this write
	// holds none. Its update is a Yjs update, which commutes with whatever landed between this
	// read and its append, and the room that loads after its publish reads the store, which by
	// then holds both. A private cursor at the head this read loaded would refuse the write for
	// an append that is no concern of its own - a browser opening the document behind this read
	// and typing into it (TestABrowserOpeningADocumentMidColdWriteKeepsBothEdits) - and the
	// agent's edit would answer 503 for a conflict Yjs resolves.
	write.forkCursor = nil
	if s.afterForkRead != nil {
		s.afterForkRead(write.artifactID)
	}
	return fork, nil
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
// update through, which gates each of its appends (refuseRoomBehindHead): its fork's basis, read
// now so that a peer's append into the same room, which the fork takes in at its next read,
// counts, with the versions this transaction has appended itself folded in (withOwnAppends). It
// is the cursor of a write whose fork came from a room, and only appendingCursor reads it, which
// hands the gate no cursor at all for a cold write (coldFork).
func (w *liveWrite) heldThrough() int64 {
	return w.withOwnAppends(w.forkCursor.heldThrough())
}

// withOwnAppends folds into base, the version the document a write is made on stands at, the
// versions this transaction has appended itself: they are in its fork, and in no room until the
// write publishes, so every head read inside its transaction counts them and no cursor does. An
// accept writes its text change and its margin projection as two appends of one transaction, so
// without this the second would find the head one past the fork and be refused.
func (w *liveWrite) withOwnAppends(base int64) int64 {
	for _, version := range w.versions {
		if int64(version) == base+1 {
			base++
		}
	}
	return base
}

// dropRendering forgets the rendering and the anchor refresh taken from a fork that has moved,
// and the rendering forkLive found for it (forkMarkdown). What makes each reuse of a rendering
// exact (applyLive) is that it describes the fork as it stands, so a fork that moves keeps none:
// the next operation renders, or takes a rendering vouched for afresh.
func (w *liveWrite) dropRendering() {
	w.tree, w.markdown, w.offsets, w.anchorsTree, w.forkMarkdown = nil, "", nil, nil, nil
}

// rendering is the write's rendering of fork - its tree, markdown and offsets - and whether it has
// one: exactly when it holds a rendering and fork is the write's own fork. forkLive drops the
// rendering whenever the fork moves (buildFork, releaseFork), and an operation that moves it
// records the rendering of what it left or abandons both (applyLive, applyMargin), so while the
// rendering is set it describes the fork as it stands, and a margin write (applyMargin) or a read
// joined to the write's transaction takes it in place of reading and rendering the fork again. The
// tree is the write's own, which the transaction's next operation and its version still use, so a
// caller only reads it.
func (w *liveWrite) rendering(fork *crdt.Doc) (renderedTree, bool) {
	if w.tree == nil || w.fork != fork {
		return renderedTree{}, false
	}
	return renderedTree{tree: w.tree, markdown: w.markdown, offsets: w.offsets}, true
}

// releaseFork drops write's fork and the rendering taken from it, which serve the transaction's
// operations alone. A write can outlive its transaction by a long way: whatever holds the
// transaction's context holds its ledger and so the write, and a pooled pgx connection keeps the
// context of the queries it last served. Kept, a heavy document's fork and tree stayed resident
// after the edit that built them had answered, and beside the room its publish loads (LEGION-504).
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

// docTree is the tree of the document the caller sees: the transaction's fork's when there is one
// (joinRead), otherwise the live room's, which it loads, read as of one moment (liveTree): the
// room's peers and the service write the room while a walk reads it. Inside a write transaction
// the tree may be the write's own rendering of the fork (reuseRendering), which every caller -
// VerifyMark, BlockForQuote, SuggestionKind, prevalidateLiveOperations, currentToken and an ApplyOps
// with no operation - only reads, and which a caller must never change.
func (s *Service) docTree(ctx context.Context, artifactID string) (*pmdoc.Node, error) {
	fork, err := s.joinRead(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if fork != nil {
		if rendering, reused := s.reuseRendering(joinedLiveWrite(ctx, artifactID), fork, "docTree"); reused {
			return rendering.tree, nil
		}
		valid, err := s.readFork(fork)
		return valid.Node(), err
	}
	var tree *pmdoc.Node
	var treeErr error
	err = s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		tree, treeErr = s.liveTree(artifactID, doc)
	})
	if treeErr != nil {
		return nil, treeErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return nil, err
	}
	return tree, nil
}

// readFork reads the tree of fork, a write transaction's fork, for that write's operation
// (applyLive, applyMargin) or a read joined to its transaction (docTree, heldTree,
// captureLiveTextAndAuthors), and counts the read (forkTreeCounts.read).
//
// Every one of those reads runs on the request goroutine that holds the fork, and the fork's own
// writes run there too (forkLive, applyLive's mutate, recordLiveOperation), so nothing else reads
// or writes it while the read runs: the condition pmdoc.ReadUnshared states, which reads the live
// text without ygo's ToDelta. The tree it hands back is validated once (pmdoc.ValidDocument), so
// the renders and reads that follow do not validate it again; its Node is the very tree the reader
// made, which this transaction's write may keep (liveWrite.tree, anchorsTree), and a tree outside
// the Proof schema is the document's own refusal, as treeOf makes it.
func (s *Service) readFork(fork *crdt.Doc) (pmdoc.ValidDocument, error) {
	s.forkTrees.read.Add(1)
	return treeOfUnshared(fork)
}

// reuseRendering is write's rendering of fork, when it has one (liveWrite.rendering), for the
// margin write (applyMargin) or the read joined to write's transaction at site at, which takes it in
// place of reading the fork. Each reuse is counted (forkTreeCounts.renderingReused) and handed to
// the test hook (checkRendering).
func (s *Service) reuseRendering(write *liveWrite, fork *crdt.Doc, at string) (renderedTree, bool) {
	rendering, ok := write.rendering(fork)
	if !ok {
		return renderedTree{}, false
	}
	s.forkTrees.renderingReused.Add(1)
	if s.checkRendering != nil {
		s.checkRendering(at, write)
	}
	return rendering, true
}

// creditLiveWrite records whom a joined content change is credited to once its transaction
// commits: its actor alone, as a service mutation that reaches the room directly is credited
// (creditContentChange). A browser connected to the room made none of it, and no version the
// transaction wrote before holds it.
//
// It also takes the room's next credit sequence, so this operation is ordered against the browser
// edits the room observes while the transaction is open: a browser edit that lands after it is
// the later one, and names the person the notice carries (latestEditSource).
func (s *Service) creditLiveWrite(write *liveWrite, actor model.Actor) {
	if write.credits == nil {
		write.credits = make(map[string]model.Actor)
		write.creditedBy = make(map[string]persistence.Version)
	}
	key := actorKey(actor)
	write.credits[key] = actor
	write.creditedBy[key] = write.versions[len(write.versions)-1]
	write.actor = new(actor)
	if write.state != nil {
		// creditSeq is atomic, so taking this operation's number needs no room lock; the capture
		// that compares it with the in-flight credits holds state.mu (captureAuthors).
		write.creditSeq = write.state.creditSeq.Add(1)
	}
}

// publishLiveWrite applies write's updates to the room one operation at a time, in the order
// they were made, as a room transaction and a broadcast each. A browser editor then receives
// them as it would have received the operations themselves: an accepted suggestion's text
// change before the margin projection that closes it, never both in one update. Each update
// travels with its operation's rendering (liveWrite.renderings), which the room's update observer
// may take in place of rendering the room after it.
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
	if s.checkRendering != nil {
		s.checkRendering("publish", write)
	}
	write.releaseFork()
	for index, update := range write.updates {
		err := s.publishLiveUpdate(write.artifactID, update, write.versions[index], write.renderings[index], write.marginRooms[index])
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
// landing before it is. That point is the room as of one moment under its document lock
// (readLive), since the room's peers write it while the check walks it, and the walk
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
	live := s.srv.GetDoc(write.artifactID)
	if live == nil {
		return
	}
	var lost []int
	var lossErr error
	if err := s.readLive(write.artifactID, live, func(doc *crdt.Doc) {
		lost, lossErr = write.loss.lost(doc)
	}); err != nil {
		slog.Warn("dispatch: copy a published write's room to read its text back", "room", write.artifactID, "error", err)
		return
	}
	if lossErr != nil {
		slog.Warn("dispatch: read a published write's text back from its room", "room", write.artifactID, "error", lossErr)
		return
	}
	write.lost, write.lostVerdict = lost, true
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
//
// The origin carries rendering, the operation's own rendering of what the update leaves, and
// inRoom, set where the update reaches the room instance and before it is applied: whether that
// instance's load already held the update's version (roomInstance.loadedHead). The room's update
// observer classifies the update by either when it can (renderedReplica.changedBy). marginRoom is
// the room document a margin write's fork was warm-built from, or the zero pointer
// (liveWrite.marginRooms): the update is tagged margin-only when the document it reaches is that
// one (liveWriteOrigin.marginOnly).
func (s *Service) publishLiveUpdate(room string, update []byte, version persistence.Version, rendering publishedRendering, marginRoom weak.Pointer[crdt.Doc]) error {
	ctx := withOwnerVerified(context.Background())
	slot := s.prepareSuppressedPersistence(room)
	origin := &liveWriteOrigin{slot: slot, rendering: rendering}
	recorded, err := s.applyCaptured(ctx, room, origin, func(doc *crdt.Doc) error {
		instance := s.instanceOf(doc)
		origin.inRoom = instance != nil && int64(version) <= instance.loadedHead
		// Tagged against the document Apply hands here, not the one the room held when the
		// publish began (liveWriteOrigin.marginOnly).
		origin.marginOnly = marginRoom.Value() == doc
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
// its fork, their updates and the renderings beside them, keeping the verdict LostOps reads.
func (s *Service) finishLiveWrite(write *liveWrite) {
	if write.finished {
		return
	}
	write.finished = true
	write.releaseFork()
	write.updates, write.renderings, write.marginRooms = nil, nil, nil
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
