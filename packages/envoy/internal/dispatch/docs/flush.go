package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A room's updates are stored in the order the room committed them. The room's update
// observer gives each update its commit position in the room instance as it drains the room's
// log (renderedReplica.seq, documentUpdateClass.seq), and every durable writer of the room stores,
// under the document's advisory lock and before an append of its own, every update of the
// instance through the position it read the room at that no writer has stored yet (flushThrough).
// The room's own persistence is one such writer, through the position its batch takes, its own
// update's at least (roomBatchThrough, StoreUpdateContext), so the order ygo hands it updates in
// never decides the stored order.

// roomPoint is an exact position in one room instance's commits: cursor names the instance (its
// roomCursor), and seq is the position of the last commit a read of it holds (readPoint). The zero
// point is a read of a document with no room instance, which has nothing unstored.
type roomPoint struct {
	cursor *roomCursor
	seq    uint64
}

// atPoint reads live, the room document resident for room, as of an exact commit position: take
// reads the room's replica under the replica's lock (renderedReplica.atPoint), and the call
// answers point, the position take read it at, read, what the replica knows of that state beyond
// the state itself (replicaRead), and settles, whether an update the drain classified is one the
// room's settlement follows. It arms no settlement for them: each read above it does what it
// must with settles, since a settlement's own read may arm none (treePoint). It waits for the
// room's replica as the room's update observer does, but holds it only while it drains the
// room's log and take reads it: its caller does its database work after, holding no replica
// lock.
//
// copied is the room's state, encoded under the document's own lock, for the reads take could
// not be run for: a document whose load listed no replica, which has had no update recorded, so
// its state is taken at position 0 and nothing is known of it beyond the state, and a copy the
// replica could not start from. Its caller decodes that state holding no lock. A replica holding
// no state is started from the copy the read takes from the room, and take then reads it
// (renderedReplica.atPoint), so copied is nil for every read take ran on. The caller holds no
// state lock and is not inside a Yjs transaction on live.
func (s *Service) atPoint(room string, live *crdt.Doc, take func(point roomPoint, doc *crdt.Doc)) (copied []byte, point roomPoint, read replicaRead, settles bool, err error) {
	if live == nil {
		return nil, roomPoint{}, replicaRead{}, false, errDocUnloaded
	}
	point = roomPoint{cursor: s.cursorOf(live)}
	replica := s.replicaOf(live)
	if replica == nil {
		return crdt.EncodeStateAsUpdateV1(live, nil), point, replicaRead{}, false, nil
	}
	var drained uint64
	copied, drained, read, settles = replica.atPoint(room, live, func(seq uint64, doc *crdt.Doc) {
		take(roomPoint{cursor: point.cursor, seq: seq}, doc)
	})
	point.seq = drained
	return copied, point, read, settles, nil
}

// readPoint is live, the room document resident for room, as of an exact commit position: its
// state, encoded under the replica's lock, the position it holds every update through, and what
// the room's replica knows of that state beyond the state itself (atPoint, replicaRead). A
// replica holding no state is started from a copy taken under the document's own lock, which
// the replica then encodes; a document whose load listed no replica has its state taken under
// the document's own lock instead (atPoint). The updates the drain classified arm the
// room's settlement, as their update observers would have.
func (s *Service) readPoint(room string, live *crdt.Doc) ([]byte, roomPoint, replicaRead, error) {
	var encoded []byte
	copied, point, read, settles, err := s.atPoint(room, live, func(_ roomPoint, doc *crdt.Doc) {
		encoded = crdt.EncodeStateAsUpdateV1(doc, nil)
	})
	if settles {
		s.scheduleSettle(room)
	}
	if err != nil {
		return nil, roomPoint{}, replicaRead{}, err
	}
	if copied != nil {
		encoded = copied
	}
	return encoded, point, read, nil
}

// forkPoint is live, the room document resident for room, forked for a write whose items client
// authors (warmFork, rebuildFork): the room's replica cloned under the replica's lock at an exact
// commit position, with that position and what the replica knows of the state it cloned (atPoint,
// replicaRead). The drain, what it classifies and what it arms are the read point's.
//
// The clone is the document a decode of the room's state builds - byte for byte in its encoding,
// its state vector and its delete set (crdt.Doc.Clone), since nothing encodes or decodes the
// state - and is built with the options that decode uses (forkOptions), so the two cannot drift.
// Clone takes the replica document's read lock, never live's, which is the order every read of
// the replica takes - the replica's lock, then the document's, as an encode takes it - and it
// must not run inside a transaction on the replica, which only a holder of the replica's mu
// writes. A clone differs from the decoded copy for one document alone, a source with an
// UndoManager attached, which keeps the content of a deleted struct the decode collects; the
// replica has none.
//
// A replica holding no state is started from the copy that read takes from the room, and the
// fork is then cloned from it like any other. A room whose load listed no replica has no
// document to clone, nor has a copy the replica could not start from: that copy is decoded here,
// holding no lock (buildFork). Either way the caller is handed the document that decode builds.
//
// at, when it is not nil, is the point the room must still stand at (rebuildFork): a read that
// drains the room to another point clones nothing and answers errForkReleased, so the replica's
// lock is held for no clone its caller throws away. The caller holds no state lock and is not
// inside a Yjs transaction on live.
func (s *Service) forkPoint(room string, live *crdt.Doc, client crdt.ClientID, at *roomPoint) (*crdt.Doc, roomPoint, replicaRead, error) {
	var fork *crdt.Doc
	copied, point, read, settles, err := s.atPoint(room, live, func(point roomPoint, doc *crdt.Doc) {
		if at == nil || *at == point {
			fork = doc.Clone(forkOptions(client)...)
		}
	})
	if settles {
		s.scheduleSettle(room)
	}
	switch {
	case err != nil:
		return nil, roomPoint{}, replicaRead{}, err
	case at != nil && *at != point:
		return nil, roomPoint{}, replicaRead{}, errForkReleased
	case fork != nil:
		return fork, point, read, nil
	}
	fork, err = buildFork(client, copied)
	if err != nil {
		return nil, roomPoint{}, replicaRead{}, fmt.Errorf("fork live document: %w", err)
	}
	return fork, point, read, nil
}

// treePoint is the tree live, the room document resident for room, holds as of an exact commit
// position (readSettlementTree): the tree the room's replica holds, read under the replica's lock
// through pmdoc.ReadUnshared (treeOfUnshared), with the position it was read at (atPoint). Only a
// holder of the replica's mu reads or writes the replica (renderedReplica), which is what
// ReadUnshared's lock-free walk of each text asks for, and the tree it hands back is the caller's
// own (ownedValue), as pmdoc.Read's is. A replica holding no state is started from the copy the
// read takes from the room, and its tree is then walked. A room whose load listed no replica has
// no tree to walk, nor has a copy the replica could not start from: that copy is decoded and
// read here, holding no lock (stateTree).
//
// It arms no settlement for the updates it classified and reports instead whether any is one a
// settlement follows: the settlement reading them is the one they would arm, and arming another
// moves the room's generation under it, which ends it as superseded. It reports that whether or
// not the tree could be read, so its caller arms what the drain classified exactly where a caller
// of readPoint would. The caller holds no state lock and is not inside a Yjs transaction on live.
func (s *Service) treePoint(room string, live *crdt.Doc) (*pmdoc.Node, roomPoint, bool, error) {
	var tree *pmdoc.Node
	var treeErr error
	copied, point, _, settles, err := s.atPoint(room, live, func(_ roomPoint, doc *crdt.Doc) {
		var valid pmdoc.ValidDocument
		if valid, treeErr = treeOfUnshared(doc); treeErr == nil {
			tree = valid.Node()
		}
	})
	if err != nil {
		return nil, roomPoint{}, settles, err
	}
	if copied != nil {
		tree, treeErr = stateTree(copied)
	}
	return tree, point, settles, treeErr
}

// stateTree is the tree of state, a room's state as a read point copied it (readPoint): a
// version's capture of the room it versions, and the tree read of a room that has no replica to
// read, or whose copy its replica could not start from (treePoint).
func stateTree(state []byte) (*pmdoc.Node, error) {
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		return nil, fmt.Errorf("decode the room's read state: %w", err)
	}
	return treeOf(doc)
}

// committedSince reports whether live, the room document resident for room, has committed anything
// since point, an earlier read point of it (readPoint). It drains the room's log as readPoint does,
// classifying what it drains, but copies nothing, so a reader that needs the room's state only when
// the room has moved pays for the copy only then. A point of another room instance, or of none,
// is one the room has moved past, and so is any point of a room whose load listed no replica,
// which cannot say. The caller holds no state lock and is not inside a Yjs transaction on live.
func (s *Service) committedSince(room string, live *crdt.Doc, point roomPoint) bool {
	if point.cursor == nil || point.cursor != s.cursorOf(live) {
		return true
	}
	replica := s.replicaOf(live)
	if replica == nil {
		return true
	}
	seq, settles := replica.drainedSeq(room)
	if settles {
		s.scheduleSettle(room)
	}
	return seq != point.seq
}

// RoomFlush is what one transaction stored of one room instance - a joined transaction's
// flushThrough calls, or a room's own batch (appendRoomTx, through flushRun) - the one
// cursor names: the classes, in commit order, with the version each was appended at, the class it
// was stored with (changed) and the credit its append took (credits; storedClasses). repairs are
// the versions of the repairs a settlement wrote into the instance and stored between its flushes
// (storeRepair). state is the record whose queue the classes were last taken from, which a joined
// transaction's commit holds locked while it commits and marks them (commitConsuming). through is
// the position through which the transaction has stored every unstored durable class. service is
// the service the flushed room belongs to, which the store calls back into to run the flush
// (AppendRoomFlush).
//
// It crosses VersionedStore, which a test store outside this package wraps, so it is exported
// with its fields unexported: such a store can only pass it on, as it can an UpdateCredit.
type RoomFlush struct {
	service  *Service
	room     string
	cursor   *roomCursor
	state    *roomState
	through  uint64
	classes  []*queuedClass
	versions []persistence.Version
	changed  []bool
	credits  []*UpdateCredit
	repairs  []persistence.Version
}

// appended is the run of versions one transaction appended for the instance, ascending: the room
// updates the flush stored, the repairs it stored between them, and own, the versions the caller
// appended for that same instance itself - a write's of this transaction (withOwnAppends,
// takeRoomCursors). It is the one fold of the two, which every reader of what a transaction
// appended for one room instance takes. A nil flush stored none of them, so the run is own alone.
func (f *RoomFlush) appended(own []persistence.Version) []persistence.Version {
	if f == nil || (len(f.versions) == 0 && len(f.repairs) == 0) {
		return own
	}
	if len(f.repairs) == 0 && len(own) == 0 {
		return f.versions
	}
	appended := slices.Concat(f.versions, f.repairs, own)
	slices.Sort(appended)
	return appended
}

// heldThrough is the durable version the flushed instance holds every update through as the
// flush's transaction sees it: the instance's cursor with the versions the transaction has
// appended for it folded in (appended). The transaction's appends' gate reads it (appendingCursor,
// and appendRoomTx for a room's own batch), and so does its fork read's judgment of the room
// (roomBehindHead), each through transactionCursor where the transaction carries a ledger.
func (f *RoomFlush) heldThrough() int64 {
	return foldAppended(f.cursor.heldThrough(), f.appended(nil))
}

// storedContentChange reports whether f stored an update as a change to the document's content,
// for which a settlement is owed, whether or not the room's shadow measured it again. A nil flush
// stored nothing.
func (f *RoomFlush) storedContentChange() bool {
	return f != nil && slices.Contains(f.changed, true)
}

// foldAppended folds into base, the version a document stands at, each of versions, ascending,
// that follows it: the run of versions one transaction appended past it.
func foldAppended(base int64, versions []persistence.Version) int64 {
	for _, version := range versions {
		if int64(version) == base+1 {
			base++
		}
	}
	return base
}

// flushThrough stores, in tx, every update of flush's room instance that its update observer
// recorded at a commit position through `through` and no writer has stored yet: in commit order,
// each with its class and its credit (storedClasses: the ones its observer recorded, or for a
// browser update stored after a live write it was measured without, the ones the room's shadow
// measures), as the one insert statement of one row per update that every append of a room writes
// (AppendRoomUpdatesTx, appendWrites, whose UpdateCredit.take writes each credit's authors), gated
// on that instance's cursor as any append of the room is (appendingCursor). It stores the run
// whole or fails: the gate cuts a browser's run at the first update its document's closed issue
// no longer admits and stores the prefix before it (gateAppend), and a flush the gate cut that
// way answers ErrIssueClosed and records nothing on flush, so the transaction that joined this
// flush rolls back and stores nothing of the run - neither the prefix nor the writer's own
// update, which it would otherwise derive from a room state holding a keystroke the store
// refused. The prefix rule is the room's own batch path's (appendRoomTx): the room stores what
// it took in time and meets the gate again on the rest, which refuses it and closes the room for
// its issue. A run this stores whole is recorded on flush, and the transaction's commit marks
// those classes stored and lands their credits (markStored); a transaction that rolls back marks
// nothing, and the room's persistence stores them later.
//
// Every flush runs on a transaction or a session that already holds the room's advisory lock,
// taken before the head it is gated on was read: the room's own persistence worker holds the
// session-level lock on its connection across the whole transaction (AppendRoomFlush,
// appendRoomTx), and every caller of Ledger.flushThrough took the transaction-level lock in the
// same transaction - a live write's one read of the document (readFork, lockedHead), a version
// capture's (captureLiveTextAndAuthors) and the write's own (captureWrite, through its read), a
// settlement's (lockSettlementCursor) and the block-id backfill's (lockDocumentRoom), which a
// repair's flush runs under too (storeRepair). So the append this makes takes no lock of its own:
// a second acquisition would cost a round trip and grant what the caller holds (appendUpdatesTx).
// The caller calls this before its own append. The classes are read under that lock, and every
// writer marks what it stored before the lock passes on, so no update is stored twice. Every
// class is measured before the append: it renders and decodes nothing between the read of the
// room's queue and the write, so the flush is the lock, that read, the measures, and one
// statement.
func (s *Service) flushThrough(ctx context.Context, tx pgx.Tx, flush *RoomFlush, through uint64) error {
	updates, taking := s.flushRun(flush, through)
	if len(updates) == 0 {
		return nil
	}
	first, stored, err := s.persistence.AppendRoomUpdatesTx(ctx, tx, flush.room, flush, updates)
	if err != nil {
		return err
	}
	if stored < len(updates) {
		return fmt.Errorf("%w: room %s's document belongs to a closed issue, which refused %d of the %d updates this transaction flushed",
			ErrIssueClosed, flush.room, len(updates)-stored, len(updates))
	}
	flush.appendedRun(taking, updates, first)
	return nil
}

// flushRun is the run a flush of flush's room instance through `through` stores, read under the
// room's state lock: the classes its update observer recorded at a commit position through that
// one which no writer has stored yet and this flush has not stored already, in commit order, and
// the updates they are stored as - each class's bytes with the content class and credit its
// append takes (storedClasses). It records on flush the record it read the queue from and how far
// the flush has stored, whether or not it took anything.
func (s *Service) flushRun(flush *RoomFlush, through uint64) ([]BrowserUpdate, []*queuedClass) {
	state := s.lockExistingState(flush.room)
	if state == nil {
		return nil, nil
	}
	var taking []*queuedClass
	for _, class := range state.updateClasses {
		if class.cursor != flush.cursor || !class.durable || class.seq > through || class.stored.Load() ||
			class.seq <= flush.through {
			continue
		}
		taking = append(taking, class)
	}
	window := state.shadowWindowLocked()
	s.unlockState(flush.room, state)
	flush.state = state
	flush.through = max(flush.through, through)
	if len(taking) == 0 {
		return nil, nil
	}
	if s.beforeFlushMeasure != nil {
		s.beforeFlushMeasure(flush.room)
	}
	stored := storedClasses(flush.room, window, taking)
	updates := make([]BrowserUpdate, len(taking))
	for index, class := range taking {
		updates[index] = BrowserUpdate{
			Update:         class.update,
			ContentChanged: stored[index].changed,
			Credit:         stored[index].credit,
			Observed:       class.observed,
		}
	}
	return updates, taking
}

// appendedRun records on f the run its transaction appended, taking's classes stored as updates at
// consecutive versions from first.
func (f *RoomFlush) appendedRun(taking []*queuedClass, updates []BrowserUpdate, first persistence.Version) {
	for index, class := range taking {
		f.classes = append(f.classes, class)
		f.versions = append(f.versions, first+persistence.Version(index))
		f.changed = append(f.changed, updates[index].ContentChanged)
		f.credits = append(f.credits, updates[index].Credit)
	}
}

// markStored marks what f stored once its transaction has committed: each class stored, the
// credit its append took held at the class's version (UpdateCredit.held), and its in-flight
// credit landed (landed), whether its append took that credit or the room's shadow measured its
// update as no change. A credit whose state locked reports its caller holds lands through
// landedLocked; the others are returned, for the caller to land once it holds no state lock. A nil
// locked holds none.
func (f *RoomFlush) markStored(locked func(*roomState) bool) (unlanded []*UpdateCredit) {
	for index, class := range f.classes {
		class.stored.Store(true)
		f.credits[index].held(f.versions[index])
		credit := class.credit
		if credit == nil {
			continue
		}
		if locked != nil && locked(credit.state) {
			credit.landedLocked()
		} else {
			unlanded = append(unlanded, credit)
		}
	}
	return unlanded
}

// flushThrough is Service.flushThrough for a transaction joined to l (or settlement's own), which
// keeps one flush per document for the whole transaction: its commit advances the instance's
// cursor with the versions the flush appended (takeRoomCursors), marks the classes stored
// (commitConsuming), and publishes none of them, since the room holds them already. A write of the
// same transaction to the document folds them into the head it is gated on (withOwnAppends). A
// transaction flushes one instance of a document: a later read point of another instance, one
// that replaced the room since, is refused, as the gate would refuse its appends.
//
// It is what every writer that records durable state without a write of its own - a settlement's
// version, a snapshot's, a named version's, a repair's (storeRepair) - stores before it records
// the room's state at a read point of it (readPoint): the stored updates then fold to that state,
// so the head after them, which the version records as its doc_update_version, holds every update
// its text does. A write of its own stores them ahead of its first append, for the same reason
// (recordLiveOperation). A point of no room instance has nothing unstored and stores nothing: a
// cold fork, read from the store, and a document no room of this process holds.
func (l *Ledger) flushThrough(ctx context.Context, tx pgx.Tx, room string, through roomPoint) error {
	if through.cursor == nil {
		return nil
	}
	flush := l.flushFor(room)
	if flush == nil {
		flush = &RoomFlush{service: l.service, room: room, cursor: through.cursor}
		l.flushes = append(l.flushes, flush)
		if write := l.liveWriteFor(room); write != nil {
			write.flushed = flush
		}
	} else if flush.cursor != through.cursor {
		return fmt.Errorf("%w: room %s was replaced since this transaction stored its updates", ErrRoomBehindHead, room)
	}
	return l.service.flushThrough(ctx, tx, flush, through.seq)
}

// flushFor is the flush this transaction keeps for room, nil while it has flushed nothing of it
// and for no transaction at all.
func (l *Ledger) flushFor(room string) *RoomFlush {
	if l == nil {
		return nil
	}
	for _, flush := range l.flushes {
		if flush.room == room {
			return flush
		}
	}
	return nil
}

// errUnclassifiedRepair refuses to store a repair whose commit position in its room is unknown:
// the room's update observer recorded no class for it (classifyDrained).
var errUnclassifiedRepair = errors.New("the room recorded no class for a repair written into it")

// storeRepair stores, in l's transaction, update, a repair a settlement wrote into the room
// instance cursor names (applySuppressed), at its place in the room's commit order: every update
// the room committed before it that no writer has stored first (flushThrough), then the repair,
// with the class the room measured for it (repairClass). Stored ahead of those updates, the repair
// would sit before updates made without it, and they after it. Its version joins the flush's
// (RoomFlush.repairs), which gate the transaction's later appends and which the instance takes in
// just before the settlement commits (commitSettlement): a settlement can still end without
// committing - its room closed under it - and the room's persistence then stores the room's
// updates against the head the store still stands at.
func (l *Ledger) storeRepair(ctx context.Context, tx pgx.Tx, room string, cursor *roomCursor, update []byte) error {
	seq, changed, err := l.service.repairClass(room, cursor, update)
	if err != nil {
		return err
	}
	if err := l.flushThrough(ctx, tx, room, roomPoint{cursor: cursor, seq: seq - 1}); err != nil {
		return err
	}
	version, err := l.service.persistence.AppendUpdateTx(ctx, tx, room, update, changed)
	if err != nil {
		return err
	}
	flush := l.flushFor(room)
	flush.repairs = append(flush.repairs, version)
	return nil
}

// repairClass is the commit position the room's update observer recorded for update, a repair
// written into the instance cursor names, and whether it changed the rendered markdown: the class
// the observer measured against the room as the commit before it left it (classifyDrained), which
// is the state the stored updates before the repair fold to once they are stored in commit order.
// A repair inside the window of the room's shadow is measured on the shadow, as each update stored
// after a live write it was made without is (storedClasses). The observer runs on the repair's
// committing goroutine before its Apply returns, so the class is queued by the time the repair's
// writer asks.
func (s *Service) repairClass(room string, cursor *roomCursor, update []byte) (uint64, bool, error) {
	state := s.lockExistingState(room)
	if state == nil {
		return 0, false, errUnclassifiedRepair
	}
	var repair *queuedClass
	for _, class := range state.updateClasses {
		if !class.durable && class.cursor == cursor && bytes.Equal(class.update, update) {
			repair = class
		}
	}
	window := state.shadowWindowLocked()
	s.unlockState(room, state)
	if repair == nil {
		return 0, false, errUnclassifiedRepair
	}
	if window.shadow != nil && window.provisional(cursor, repair.seq) {
		return repair.seq, window.shadow.measure(room, repair, window.repairs), nil
	}
	return repair.seq, repair.contentChanged, nil
}
