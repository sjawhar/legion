package docs

import (
	"context"
	"fmt"
	"slices"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
)

// A room's updates are stored in the order the room committed them (INFRA-707). The room's update
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
// state, encoded, and the position it holds every update through (renderedReplica.readPoint). It
// waits for the room's replica as the room's update observer does, but holds it only while it
// drains the room's log and copies the replica: its caller does its database work after, holding
// no replica lock. A document whose load listed no replica has had no update recorded, so its
// state is taken under its own lock at position 0. The updates it classified arm the room's
// settlement, as their update observers would have. The caller holds no state lock and is not
// inside a Yjs transaction on live.
func (s *Service) readPoint(room string, live *crdt.Doc) ([]byte, roomPoint, error) {
	if live == nil {
		return nil, roomPoint{}, errDocUnloaded
	}
	point := roomPoint{cursor: s.cursorOf(live)}
	replica := s.liveReplica(live)
	if replica == nil {
		return crdt.EncodeStateAsUpdateV1(live, nil), point, nil
	}
	state, seq, settles, _ := replica.readPoint(room, live, replica.classify)
	if settles {
		s.scheduleSettle(room)
	}
	point.seq = seq
	return state, point, nil
}

// liveReplica is the replica the update observer of live, a room's resident document, keeps, nil
// for a document whose load listed none.
func (s *Service) liveReplica(live *crdt.Doc) *renderedReplica {
	if listed, ok := s.replicas.Load(weak.Make(live)); ok {
		return listed.(weak.Pointer[renderedReplica]).Value()
	}
	return nil
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
	replica := s.liveReplica(live)
	if replica == nil {
		return true
	}
	seq, settles := replica.drainedSeq(room, replica.classify)
	if settles {
		s.scheduleSettle(room)
	}
	return seq != point.seq
}

// roomFlush is what one transaction's flushThrough calls stored of one room instance, the one
// cursor names: the classes, in commit order, with the version each was appended at, the class it
// was stored with (changed) and the credit its append took (credits; storedClasses). state is the
// record whose queue the classes were last taken from, which a joined transaction's commit holds
// locked while it commits and marks them (commitConsuming). through is the position through which
// the transaction has stored every unstored durable class, once ran is set.
type roomFlush struct {
	room     string
	cursor   *roomCursor
	state    *roomState
	through  uint64
	ran      bool
	classes  []*queuedClass
	versions []persistence.Version
	changed  []bool
	credits  []*UpdateCredit
}

// heldThrough is the durable version the flushed instance holds every update through as the
// flush's transaction sees it: the instance's cursor with the versions the flush has appended
// folded in, which gates each next append of the flush (appendingCursor).
func (f *roomFlush) heldThrough() int64 {
	return foldAppended(f.cursor.heldThrough(), f.versions)
}

// storedContentChange reports whether f stored an update as a change to the document's content,
// for which a settlement is owed, whether or not the room's shadow measured it again. A nil flush
// stored nothing.
func (f *roomFlush) storedContentChange() bool {
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

// flushGateContextKey marks the context of flushThrough's appends with the flush they belong to,
// whose instance gates them (appendingCursor).
type flushGateContextKey struct{}

func flushGateFrom(ctx context.Context) *roomFlush {
	flush, _ := ctx.Value(flushGateContextKey{}).(*roomFlush)
	return flush
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
func (s *Service) flushThrough(ctx context.Context, tx pgx.Tx, flush *roomFlush, through uint64) error {
	state := s.lockExistingState(flush.room)
	if state == nil {
		return nil
	}
	var taking []*queuedClass
	for _, class := range state.updateClasses {
		if class.cursor != flush.cursor || !class.durable || class.seq > through || class.stored.Load() ||
			(flush.ran && class.seq <= flush.through) {
			continue
		}
		taking = append(taking, class)
	}
	window := state.shadowWindowLocked()
	s.unlockState(flush.room, state)
	flush.state = state
	flush.through = max(flush.through, through)
	flush.ran = true
	stored := storedClasses(flush.room, window, taking)
	gated := context.WithValue(ctx, flushGateContextKey{}, flush)
	for index, class := range taking {
		version, err := s.persistence.AppendRoomUpdateTx(gated, tx, flush.room, class.update, stored[index].changed, stored[index].credit)
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
func (f *roomFlush) markStored(locked func(*roomState) bool) (unlanded []*UpdateCredit) {
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

// workerFlush is the flush the room's persistence runs for its own update (StoreUpdateContext):
// through is that update's commit position.
type workerFlush struct {
	service *Service
	flush   *roomFlush
	through uint64
}

type workerFlushContextKey struct{}

// withWorkerFlush marks ctx, the context of the room's persistence's append of its own update, with
// the flush that append runs in its transaction (PgVersioned.appendFlushing).
func withWorkerFlush(ctx context.Context, service *Service, flush *roomFlush, through uint64) context.Context {
	return context.WithValue(ctx, workerFlushContextKey{}, &workerFlush{service: service, flush: flush, through: through})
}

func workerFlushFrom(ctx context.Context) *workerFlush {
	worker, _ := ctx.Value(workerFlushContextKey{}).(*workerFlush)
	return worker
}

// flushThrough is Service.flushThrough for a transaction joined to l (or settlement's own), which
// keeps one flush per document for the whole transaction: its commit advances the instance's
// cursor with the versions the flush appended (takeRoomCursors), marks the classes stored
// (commitConsuming), and publishes none of them, since the room holds them already. A write of the
// same transaction to the document folds them into the head it is gated on (withOwnAppends). A
// transaction flushes one instance of a document: a later read point of another instance, one
// that replaced the room since, is refused, as the gate would refuse its appends.
func (l *Ledger) flushThrough(ctx context.Context, tx pgx.Tx, room string, through roomPoint) error {
	flush := l.flushFor(room)
	if flush == nil {
		flush = &roomFlush{room: room, cursor: through.cursor}
		l.flushes = append(l.flushes, flush)
		if write := l.liveWriteFor(room); write != nil {
			write.flushed = flush
		}
	} else if flush.cursor != through.cursor {
		return fmt.Errorf("%w: room %s was replaced since this transaction stored its updates", ErrRoomBehindHead, room)
	}
	return l.service.flushThrough(ctx, tx, flush, through.seq)
}

// flushRead stores, in l's transaction, the room's updates through write's read point that no
// writer has stored yet (flushThrough): what write's fork holds beyond the store, stored ahead of
// every append of the write (window A), so the write's own updates follow, in the store, the very
// state they were made on. The write holds the document's advisory lock from that read on
// (readFork), so nothing it stores here can have been stored by another writer since. A second
// call stores nothing more, since the point never moves (forkLive). A cold fork has no read point
// and stores nothing: it was read from the store.
func (l *Ledger) flushRead(ctx context.Context, write *liveWrite) error {
	if write.point.cursor == nil {
		return nil
	}
	return l.flushThrough(ctx, l.tx, write.artifactID, write.point)
}

// flushFor is the flush this transaction keeps for room, nil while it has flushed nothing of it.
func (l *Ledger) flushFor(room string) *roomFlush {
	for _, flush := range l.flushes {
		if flush.room == room {
			return flush
		}
	}
	return nil
}
