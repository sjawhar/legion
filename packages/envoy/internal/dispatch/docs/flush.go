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
// The room's own persistence is one such writer, through its own update's position
// (StoreUpdateContext), so the order ygo hands it updates in never decides the stored order.

// roomPoint is an exact position in one room instance's commits: cursor names the instance (its
// roomCursor), and seq is the position of the last commit a read of it holds (readPoint). The zero
// point is a read of a document with no room instance, which has nothing unstored.
type roomPoint struct {
	cursor *roomCursor
	seq    uint64
}

// readPoint is live, the room document resident for room, as of an exact commit position: its
// state, encoded, the position it holds every update through, and what the room's replica knows
// of that state beyond the state itself (renderedReplica.readPoint, replicaRead). It waits for the
// room's replica as the room's update observer does, but holds it only while it drains the room's
// log and copies the replica: its caller does its database work after, holding no replica lock. A
// document whose load listed no replica has had no update recorded, so its state is taken under
// its own lock at position 0, and nothing is known of it beyond the state. The updates it
// classified arm the room's settlement, as their update observers would have. The caller holds no
// state lock and is not inside a Yjs transaction on live.
func (s *Service) readPoint(room string, live *crdt.Doc) ([]byte, roomPoint, replicaRead, error) {
	state, point, read, settles, err := s.readPointQuietly(room, live)
	if settles {
		s.scheduleSettle(room)
	}
	return state, point, read, err
}

// readPointQuietly is readPoint for a settlement's own read of the room, which arms no settlement
// for the updates it classified and reports instead whether any is one a settlement follows: the
// settlement reading them is the one they would arm, and arming another moves the room's
// generation under it, which ends it as superseded.
func (s *Service) readPointQuietly(room string, live *crdt.Doc) ([]byte, roomPoint, replicaRead, bool, error) {
	if live == nil {
		return nil, roomPoint{}, replicaRead{}, false, errDocUnloaded
	}
	point := roomPoint{cursor: s.cursorOf(live)}
	replica := s.replicaOf(live)
	if replica == nil {
		return crdt.EncodeStateAsUpdateV1(live, nil), point, replicaRead{}, false, nil
	}
	state, seq, read, settles := replica.readPoint(room, live)
	point.seq = seq
	return state, point, read, settles, nil
}

// stateTree is the tree of state, a room's state as a read point copied it (readPoint).
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

// RoomFlush is what one transaction's flushThrough calls stored of one room instance, the one
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
// appended for it folded in (appended), which gates each next append of the flush
// (appendingCursor).
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
// measures; AppendRoomUpdateTx, whose UpdateCredit.take writes the credit's authors), each gated on
// that instance's cursor as any append of the room is (appendingCursor). It records them on flush,
// and the transaction's commit marks them stored and lands their credits (markStored); a
// transaction that rolls back marks nothing, and the room's persistence stores them later.
//
// The caller holds the document's advisory lock in tx from before it read the head, and calls this
// before its own append. The classes are read under that lock, and every writer marks what it
// stored before the lock passes on, so no update is stored twice. Every class is measured before
// the first append: between its appends it renders and decodes nothing, so it is the lock, the
// read of the room's queue, the measures, and the appends.
func (s *Service) flushThrough(ctx context.Context, tx pgx.Tx, flush *RoomFlush, through uint64) error {
	state := s.lockExistingState(flush.room)
	if state == nil {
		return nil
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
	stored := storedClasses(flush.room, window, taking)
	for index, class := range taking {
		version, err := s.persistence.AppendRoomUpdateTx(ctx, tx, flush.room, flush, class.update, stored[index].changed, stored[index].credit, class.observed)
		if err != nil {
			return err
		}
		flush.classes = append(flush.classes, class)
		flush.versions = append(flush.versions, version)
		flush.changed = append(flush.changed, stored[index].changed)
		flush.credits = append(flush.credits, stored[index].credit)
	}
	return nil
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
