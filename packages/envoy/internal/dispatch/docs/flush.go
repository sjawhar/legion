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
	var replica *renderedReplica
	if listed, ok := s.replicas.Load(weak.Make(live)); ok {
		replica = listed.(weak.Pointer[renderedReplica]).Value()
	}
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

// roomFlush is what one transaction's flushThrough calls stored of one room instance, the one
// cursor names: the classes, in commit order, with the version each was appended at. state is the
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
}

// heldThrough is the durable version the flushed instance holds every update through as the
// flush's transaction sees it: the instance's cursor with the versions the flush has appended
// folded in, which gates each next append of the flush (appendingCursor).
func (f *roomFlush) heldThrough() int64 {
	return foldAppended(f.cursor.heldThrough(), f.versions)
}

// storedContentChange reports whether f stored an update that changed the document's content, for
// which a settlement is owed. A nil flush stored nothing.
func (f *roomFlush) storedContentChange() bool {
	return f != nil && slices.ContainsFunc(f.classes, func(class *queuedClass) bool { return class.contentChanged })
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
// each with the class and the in-flight credit its observer recorded (AppendRoomUpdateTx, whose
// UpdateCredit.take writes the credit's authors), each gated on that instance's cursor as any
// append of the room is (appendingCursor). It records them on flush, and the transaction's commit
// marks them stored and lands their credits (markStored); a transaction that rolls back marks
// nothing, and the room's persistence stores them later.
//
// The caller holds the document's advisory lock in tx from before it read the head, and calls this
// before its own append. The classes are read under that lock, and every writer marks what it
// stored before the lock passes on, so no update is stored twice. Between its appends it renders
// and decodes nothing: it is the lock, the read of the room's queue, and the appends.
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
	s.unlockState(flush.room, state)
	flush.state = state
	flush.through = max(flush.through, through)
	flush.ran = true
	gated := context.WithValue(ctx, flushGateContextKey{}, flush)
	for _, class := range taking {
		version, err := s.persistence.AppendRoomUpdateTx(gated, tx, flush.room, class.update, class.contentChanged, class.credit)
		if err != nil {
			return err
		}
		flush.classes = append(flush.classes, class)
		flush.versions = append(flush.versions, version)
	}
	return nil
}

// markStored marks what f stored once its transaction has committed: each class stored, and its
// credit held at the class's version, then landed (UpdateCredit.held, landed). A credit whose state
// locked reports its caller holds lands through landedLocked; the others are returned, for the
// caller to land once it holds no state lock. A nil locked holds none.
func (f *roomFlush) markStored(locked func(*roomState) bool) (unlanded []*UpdateCredit) {
	for index, class := range f.classes {
		class.stored.Store(true)
		credit := class.credit
		if credit == nil {
			continue
		}
		credit.held(f.versions[index])
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

// flushFor is the flush this transaction keeps for room, nil while it has flushed nothing of it.
func (l *Ledger) flushFor(room string) *roomFlush {
	for _, flush := range l.flushes {
		if flush.room == room {
			return flush
		}
	}
	return nil
}
