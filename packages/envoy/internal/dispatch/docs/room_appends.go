package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A room's browser updates reach the store through ygo's per-room persistence worker, which calls
// StoreUpdateContext once per update, in order (PersistCoalesceWindow = -1 keeps one call, and one
// stored row, per update), and returns from a flush - a CloseRoom, the idle sweep, Shutdown - only
// once every update it buffered has had its call return. A person typing makes an update every
// keystroke or so. Stored in a transaction each, on a connection of the shared request pool, every
// keystroke cost the API a connection and about ten round trips under the document's lock. The
// room's queued updates are stored together instead, which is what this file does.
//
// The batch. Every update a room commits has a class its update observer recorded before ygo's
// persistence observer queues the same update (onLoadDocument), so when ygo calls for an update u,
// the room's class queue already holds u and every update the room recorded after it. Unless
// something waits on the room, u's call lingers until roomAppendLinger after u was recorded, so a
// burst of keystrokes is queued by the time it goes on. The call then reads, under the room's state
// lock, the commit position its batch runs through (roomBatchThrough): the last of the run of
// classes queued after u that is durable work of the room instance u was made in (its cursor), is
// not stored, and does not take the run past roomAppendBatchUpdates updates or roomAppendBatchBytes
// bytes - u's own position at least. It hands that position to AppendRoomFlush, whose transaction
// stores every update of the instance recorded at a position through it that no writer has stored
// yet (Service.flushRun, the run a transaction's Service.flushThrough stores too), in the room's
// commit order, as one insert statement of one row per update with its own content class and
// credit (appendWrites), and one pair of notifications.
// The whole transaction is about three round trips under the document's lock, on a connection of
// the room-appends pool.
//
// What holds:
//
//   - Durable on return. A call returns nil only once its update is committed: by its own
//     transaction, or by another writer's that stored it first and marked its class stored
//     (RoomFlush.markStored). ygo's flush still means every update it held is durable.
//   - Stored once. No class is stored by two transactions. A class is stored only by a transaction
//     that holds the document's advisory lock and finds it, under the room's state lock, still
//     queued and not stored (Service.flushRun), and every such transaction marks what it
//     stored before it releases that lock, so a transaction that takes the lock next finds the
//     marks. A class leaves the queue only with its own update's call (finishUpdateClass), so a
//     call whose update another writer stored finds the mark and appends nothing.
//   - Versions follow the order in which transactions commit, and within one the room's commit
//     order: a flush's run is stored at consecutive versions from the head its gate read, in the
//     order the room recorded the updates, their update-log order (documentUpdateClass.seq).
//   - A stored class's counts. A durable class's append is counted from the moment its observer
//     recorded it (queueUpdateClass) until its own call returns (finishUpdateClass), whether that
//     call stored the update or found it stored by another writer, so a wait for the room's
//     appends (waitForRoomIdle) ends only once ygo has handed every update over.
//   - Failure outcomes. A refused gate (ErrRoomBehindHead), or a statement or a commit the database
//     refuses, stores nothing and marks nothing: the call that ran the batch fails the room and
//     returns the error, and every other update of the batch meets the failed room on its own call,
//     which drops it, ends its counts and lands its credit, as any failed room's update. The
//     call's own credit lands before the transaction releases the document's lock, committed or
//     not; the other updates' credits land with their own calls unless the transaction committed
//     them (UpdateCredit.landed does nothing the second time).
//   - A suppression slot forces the lone position. A published live write's update and a repair's
//     are already durable, and the room's persistence knows each by the suppression slot queued for
//     it, which it matches, in order, against the update it is handed
//     (consumeSuppressedPersistence); their classes are queued stored, so no flush takes them. A
//     call that finds a slot of the room queued runs its batch through its own update's position
//     alone.
//   - No holder of a document lock waits for a RoomAppends connection. A batch takes its connection
//     (store.Pool.RoomAppends) before the document's lock and holds store.HoldsConnection on its
//     context, so nothing under it takes a second connection; and nothing else takes a connection of
//     that pool - settlements, live writes and the withRoomLock writers keep theirs. Whoever holds a
//     lock a batch waits on finishes without one of these connections, so the pool cannot deadlock.
//
// The ways an update reaches the store, and why none stores a class another transaction stored:
//
//   - A room's batch (AppendRoomFlush) stores the run its flush reads under the document's lock,
//     which holds no class another writer has marked stored.
//   - An update with no class - one no room recorded, as a test hands on, or a repair's whose
//     record was released - is stored alone (AppendUpdatesWithCredit) with nothing to mark: no
//     flush holds it, since a flush's run comes from the queue.
//   - A writer's own update - a live write's, a settlement's repair or stamp, the block-id
//     backfill's, a seed's - is stored by its writer in its own transaction (AppendUpdateTx), which
//     stores the room's unstored updates before it (Ledger.flushThrough). The room's persistence
//     skips it by its suppression slot, and its class is queued stored.
//   - AppendUpdate, which no room's call makes (a test's, a seed's), stores what it is handed and
//     marks nothing: no class holds its update.

// roomAppendLinger is how long after an update was recorded its call waits for the room's queue to
// fill before its batch goes to the store. A keystroke's durability then lags its commit by at most
// this beyond its transaction; a settlement, a shutdown or anything else that waits on the room's
// appends asks for them at once instead (waitForRoomIdle), as does another process's room query
// of this room (askRoomAppends), so neither waits out a linger.
const roomAppendLinger = 50 * time.Millisecond

// roomAppendBatchUpdates and roomAppendBatchBytes bound one batch: at most this many updates and
// this many bytes of them, but always the update whose call runs it. A queue that reaches either
// ends its batch's linger (queueUpdateClass).
const (
	roomAppendBatchUpdates = 256
	roomAppendBatchBytes   = 1 << 20
)

// BrowserUpdate is one update an append transaction stores (appendWrites): its bytes, whether it
// changes the document's rendered content, the browser edit's in-flight credit, nil for an update
// no browser edit credited, and when the room's observer took it, which the gate judges against
// the document's issue close (observedBeforeClose); zero for an update no room observed. Its
// fields are exported so a test store in another package can hand a batch on; UpdateCredit's are
// not.
type BrowserUpdate struct {
	Update         []byte
	ContentChanged bool
	Credit         *UpdateCredit
	Observed       time.Time
}

// AppendRoomFlush is the room's persistence worker's append of its own update (StoreUpdateContext),
// the whole flush its transaction runs: under the document's lock, in one transaction, it stores
// every update of the room instance flush names through `through`, the last commit position of the
// run its batch takes (roomBatchThrough), that no writer has stored yet, in the room's commit order
// (Service.flushRun). The update bytes are not passed: the room's queue holds them, with the
// class and the credit its update observer recorded for each (documentUpdateClass). An update
// another writer has stored already appends nothing. Each version it appends advances the
// instance's cursor inside the lock, and once the transaction has committed each class is marked
// stored and its credit held and landed before the lock is released, so the next holder of the lock
// finds them stored, and the room's shadow is freed once no class it measures is left unstored
// (dropDrainedShadowLocked). credit is the worker's own update's, which lands inside the lock
// however the transaction ends; the credits of the other updates land only with their committed
// appends, and stay with their own updates' calls otherwise. The version returned is the last the
// flush appended, zero when it appended none.
func (p *PgVersioned) AppendRoomFlush(ctx context.Context, room string, flush *RoomFlush, through uint64, credit *UpdateCredit) (persistence.Version, error) {
	_, err := p.appendRoomTx(ctx, room, flush, through, credit, nil, true)
	if err != nil || len(flush.versions) == 0 {
		return 0, err
	}
	return flush.versions[len(flush.versions)-1], nil
}

// AppendUpdatesWithCredit validates updates, each alone, and stores them in one transaction, the
// first at the version after the head, and returns that first version: the append of a room's
// update no class holds, and of an update a caller hands the store itself (AppendUpdate). It runs
// the same transaction a room's batch does, with the updates given rather than read off the room's
// queue, and is gated on the room cursor ctx is marked with, if any.
func (p *PgVersioned) AppendUpdatesWithCredit(ctx context.Context, room string, updates []BrowserUpdate) (persistence.Version, error) {
	return p.appendUpdates(ctx, room, updates, true)
}

// appendUpdates is AppendUpdatesWithCredit's append, with owes, whether the transaction marks the
// document owing a settlement: a parked append writes no owed row (AppendParkedUpdate).
func (p *PgVersioned) appendUpdates(ctx context.Context, room string, updates []BrowserUpdate, owes bool) (persistence.Version, error) {
	if len(updates) == 0 {
		return 0, fmt.Errorf("append document updates: no update to append")
	}
	for _, update := range updates {
		if err := validateUpdate(update.Update, p.maxPendingItems); err != nil {
			return 0, err
		}
	}
	return p.appendRoomTx(ctx, room, nil, 0, nil, updates, owes)
}

// appendRoomTx is the transaction a room's updates are stored in: it holds the room's in-process
// lock and a connection of the room-appends pool (store.Pool.RoomAppends) throughout, and sends its
// statements as pgx pipelines but for the unlock it ends with, three round trips from the
// document's session lock to its release, and four on the rare append that finds a prune's
// checkpoint:
//
//  1. pg_advisory_lock, begin and the gate's read (queueAppendGate), with one more round trip,
//     the prune's rest and the gate read again, on the rare append whose gate read finds a
//     checkpoint (appendGateRead.pruneCeiling, recoverPruneTx's two statements);
//  2. once the gate has passed (gateAppend), the append's writes (appendWrites) and commit;
//  3. pg_advisory_unlock on its own, once the commit's bookkeeping is done: each stored class
//     marked stored with its credit held at its version (RoomFlush.markStored),
//     afterAppendCommit, the room instance's cursor advanced once per version and the room's
//     figures. credit lands before this, committed or not. Once the locks are released, a
//     committed transaction notes its last version to the compaction queue (PgVersioned.noted).
//     A path that returns before its commit spends one trip more than the list says: the
//     rollback of what it left open goes out ahead of the unlock, under the unlock's own bound.
//
// No pg_advisory_xact_lock is taken: the session already holds the document's lock on this very
// connection, so a second acquisition would grant what the session has (appendUpdatesTx's rule
// for a flush, which runs under a lock its caller took).
//
// What it stores is flush's run through `through`, read under the document's lock once the first
// pipeline holds it (Service.flushRun), or, for a nil flush, given, whose credits are held at
// their versions once the commit lands and land, committed or not, with credit's; first is then
// the version given's first update took. owes is whether the transaction marks the document
// owing a settlement (appendWrites). A run that holds nothing commits nothing. Any database
// error rolls back what is open, under the bound the unlock runs under (duty.LeaseCall), and
// still gives the session lock back, as lockRoom's does and through the same freeRoomLock: a
// connection whose lock did not free is closed rather than returned to the room-appends pool
// (finish).
func (p *PgVersioned) appendRoomTx(ctx context.Context, room string, flush *RoomFlush, through uint64, credit *UpdateCredit, given []BrowserUpdate, owes bool) (first persistence.Version, err error) {
	conn, ctx, release, err := p.holdRoom(ctx, room, true, func(ctx context.Context) (*pgxpool.Conn, error) {
		appends, err := p.store.Pool.RoomAppends()
		if err != nil {
			return nil, err
		}
		conn, err := appends.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire document append connection: %w", err)
		}
		return conn, nil
	})
	if err != nil {
		return 0, err
	}
	// stored is the last version the transaction committed, -1 while it has committed none. The
	// compaction queue is told of it once the document's locks are gone (PgVersioned.noted),
	// since the note waits on nothing the append holds: deferred before release, it runs after it.
	stored := int64(-1)
	defer func() {
		if stored >= 0 {
			p.noted(room, stored)
		}
	}()
	defer release()

	trips := 0
	send := func(ctx context.Context, batch *pgx.Batch) error {
		trips++
		return conn.SendBatch(ctx, batch).Close()
	}
	open := false
	// land ends the credits this call carries: its own and those of the updates it was given.
	// Every return reaches it, the ones that never open a transaction included, and the call's
	// own credit lands while the session lock is still held, whether the transaction committed or
	// not, so no version reads the pending authors between a commit and its credits leaving the
	// room's in-flight credits: finish lands them first, and this deferred call is what the paths
	// that never reach finish land on (UpdateCredit.landed does nothing the second time).
	land := func() {
		credit.landed()
		for _, update := range given {
			update.Credit.landed()
		}
	}
	defer land()
	// finish ends the append however it ends, in this order: the credits land; then a transaction
	// still open is rolled back, which is a trip of a path that returns before its commit alone,
	// since the common path's commit went out with its writes; then the session lock is given
	// back, the one trip the common path ends with; and last the round trips spent, the unlock's
	// among them, go to the room's figures.
	//
	// The rollback runs under duty.LeaseCall, the bound the unlock after it runs under, and on
	// neither ctx, which the caller that gave up has already ended, nor a background context: on
	// a half-open connection an unbounded rollback waits for the kernel to give the link up,
	// minutes of it, while this call holds the room's in-process lock, its connection of the
	// room-appends pool and the document's session lock, so the room's next writer waits that out
	// too. At the bound pgx closes the connection, FreeSessionLock answers ErrSessionClosed, and
	// Postgres drops the lock with the session.
	//
	// The unlock goes through freeRoomLock, as every other holder of a document's session lock
	// gives one back (lockRoom): it is bounded, so a half-open connection holds neither this call
	// nor the room's next writer until TCP gives it up, its answer is read, and a connection whose
	// lock did not free is closed rather than handed back to the room-appends pool, where the next
	// batch could take a lock the session already holds and unlock it twice over.
	finish := func() {
		land()
		if open {
			if p.beforeAppendRollback != nil {
				p.beforeAppendRollback(room, conn)
			}
			rollback := &pgx.Batch{}
			rollback.Queue(`rollback`)
			rollbackCtx, cancel := duty.LeaseCall()
			_ = send(rollbackCtx, rollback)
			cancel()
		}
		if p.beforeAppendUnlock != nil {
			p.beforeAppendUnlock(room, conn)
		}
		// The unlock is a trip of this append like any other, counted whatever it answers.
		trips++
		freeRoomLock(conn, room)
		if flush != nil {
			flush.service.appendFigures.roundTrips.Add(uint64(trips))
		}
	}

	opening := &pgx.Batch{}
	opening.Queue(`select pg_advisory_lock(hashtext($1))`, room)
	opening.Queue(`begin`)
	gate := queueAppendGate(opening, room)
	// The opening pipeline is what takes the session lock and opens the transaction, so the end
	// is deferred around it rather than before it, as lockRoom defers its unlock only once its
	// lock statement has answered. pgx answers a batch whose context has already ended without a
	// byte leaving the client, and closes the connection where the context ends mid-flight, so on
	// a context's error the session holds no lock this call can give back: ending there would roll
	// back nothing, unlock a lock never taken - a false answer that logs an error and destroys a
	// healthy connection - and count a trip never sent. Any other failure may have left the lock
	// taken, since the lock is the pipeline's first statement and a session-level lock outlives
	// the implicit transaction a later statement's error rolls back, so that one ends as every
	// other path that returns before its commit does.
	open = true
	if err := send(ctx, opening); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			finish()
		}
		return 0, fmt.Errorf("begin document update: %w", err)
	}
	defer finish()
	read, err := gate.result()
	if err != nil {
		return 0, err
	}
	if read.pruneCeiling != nil {
		// A prune its crash left half done: the rest of it (recoverPruneTx's two statements), and
		// the gate read again on the head that leaves, cost this append one more round trip
		// (takePruneCheckpointStatement's comment says why the first pipeline does not hold the
		// delete).
		finishing := &pgx.Batch{}
		store.QueueExec(finishing, takePruneCheckpointWhat, takePruneCheckpointStatement, room)
		store.QueueExec(finishing, finishPruneWhat, finishPruneStatement, room, *read.pruneCeiling)
		gate = queueAppendGate(finishing, room)
		if err := send(ctx, finishing); err != nil {
			return 0, fmt.Errorf("finish the document's prune: %w", err)
		}
		if read, err = gate.result(); err != nil {
			return 0, err
		}
	}
	// The cursor is read here, after the opening pipeline returned, under the lock that pipeline
	// took: neither it nor the head can move while this transaction holds it.
	heldThrough, gated := appendingThrough(ctx, room, flush)
	var updates []BrowserUpdate
	var taking []*queuedClass
	if flush != nil {
		updates, taking = flush.service.flushRun(flush, through)
	} else {
		updates = given
	}
	if len(updates) == 0 {
		return 0, nil
	}
	head, admitted := read.latest, len(updates)
	if gated {
		var replaced bool
		head, admitted, replaced, err = gateAppend(ctx, conn, room, heldThrough, appendBrowser, updates, read, p.maxPendingItems)
		if err != nil {
			return 0, err
		}
		if replaced {
			p.markRunReplaced(room, roomCursorFrom(ctx), flush)
		}
	}
	// A closed issue admits only the updates the room took in time (gateAppend): the rest stay
	// queued, and each meets the gate again on its own call.
	updates = updates[:admitted]
	taking = taking[:min(admitted, len(taking))]
	first = persistence.Version(head + 1)
	statements, last, err := p.appendWrites(ctx, room, head, updates, owes)
	if err != nil {
		return 0, err
	}
	second := &pgx.Batch{}
	for _, statement := range statements {
		statement.queue(second)
	}
	second.Queue(`commit`)
	if err := send(ctx, second); err != nil {
		return 0, fmt.Errorf("commit document update: %w", err)
	}
	open, stored = false, int64(last)
	if p.afterAppendCommit != nil {
		for version := head + 1; version <= int64(last); version++ {
			p.afterAppendCommit(room, persistence.Version(version))
		}
	}
	if flush == nil {
		for index, update := range updates {
			update.Credit.held(first + persistence.Version(index))
		}
		// The room instance's cursor advances here, inside the lock the append holds, rather than
		// where the caller marked the context with it (StoreUpdateContext): a writer that took the
		// lock after this commit released it would otherwise read a cursor behind the head it
		// reads under that lock, and the gate would refuse an agent's edit while nothing was wrong
		// with its room.
		cursor := roomCursorFrom(ctx)
		for version := head + 1; version <= int64(last); version++ {
			advanceRoomCursor(cursor, persistence.Version(version))
		}
		return first, nil
	}
	flush.appendedRun(taking, updates, persistence.Version(head+1))
	advanceRoomCursorThrough(flush.cursor, flush.appended(nil))
	for _, unlanded := range flush.markStored(nil) {
		unlanded.landed()
	}
	state := flush.state
	state.mu.Lock()
	dropped := state.dropDrainedShadowLocked()
	flush.service.unlockState(room, state)
	dropped.release()
	flush.service.appendFigures.committed(len(updates))
	return first, nil
}

// roomBatchThrough is the commit position the call for class runs its batch through: the position
// of the last class of the run its batch takes, read under room's state lock (batchRunLocked), and
// class's own position when a suppression slot is queued for the room, which keeps a slot's update
// out of the run, or when the queue no longer holds the class. With linger, a run that is not full
// yet answers no position but the channel its linger waits on and how long it waits: until
// roomAppendLinger after the class was recorded, none while the service is stopping, while a wait
// for the room's appends is waiting (appendWaiters, waitForRoomIdle, askRoomAppends), or once
// something asked the room's appends to go after the class was recorded (appendsFlushed). The call
// then asks again without linger, and the run is read then.
func (s *Service) roomBatchThrough(room string, class *queuedClass, linger bool) (through uint64, wake <-chan struct{}, wait time.Duration) {
	state := s.lockExistingState(room)
	if state == nil {
		return class.seq, nil, 0
	}
	defer s.unlockState(room, state)
	at := slices.Index(state.updateClasses, class)
	if at < 0 || s.suppressionQueued(room) {
		return class.seq, nil, 0
	}
	end, full := batchRunLocked(state.updateClasses, at)
	if linger && !full && state.appendWaiters == 0 && !s.stopping.Load() && !s.shuttingDown(room) && class.observed.After(state.appendsFlushed) {
		if wait = time.Until(class.observed.Add(roomAppendLinger)); wait > 0 {
			return 0, state.appendLinger.wait(), wait
		}
	}
	return state.updateClasses[end-1].seq, nil, 0
}

// askRoomAppends asks room's pending browser appends to go now rather than wait their linger out
// (roomAppendLinger), as a wait for the room's own appends does (waitForRoomIdle): it records
// appendsFlushed, so no update the room recorded before the ask lingers any more, wakes every
// linger already waiting, and counts the asker among the room's append waiters until done is
// called, so nothing lingers while it waits. A room this process holds no record of has nothing
// to ask of: done is then a call that does nothing. Another process's room query asks for this
// before it waits for the room's updates to be stored (roomQueries.query), which it has only
// roomQueryWait to answer in - ten times the linger, but a wait the asker spends holding
// whatever it asked the flush for.
func (s *Service) askRoomAppends(room string) (done func()) {
	state := s.lockExistingState(room)
	if state == nil {
		return func() {}
	}
	state.appendsFlushed = time.Now()
	state.appendWaiters++
	state.appendLinger.wake()
	s.unlockState(room, state)
	return func() {
		state.mu.Lock()
		state.appendWaiters--
		s.unlockState(room, state)
	}
}

// batchRunLocked is the run of queue a batch for the class at index at takes: from at to end, the
// first class past at that is stored, handed (its own call stores it), not durable work, or of
// another room instance (cursor) than at's, or that would take the run past
// roomAppendBatchUpdates or roomAppendBatchBytes - at's own class is taken whatever its size. full
// is whether a cap ended it, or the run fills one. Its caller holds the queue's state.mu.
func batchRunLocked(queue []*queuedClass, at int) (end int, full bool) {
	cursor := queue[at].cursor
	count, size := 0, 0
	for index := at; index < len(queue); index++ {
		class := queue[index]
		if index > at && (class.stored.Load() || class.handed || !class.durable || class.cursor != cursor) {
			return index, false
		}
		if count == roomAppendBatchUpdates || (count > 0 && size+len(class.update) > roomAppendBatchBytes) {
			return index, true
		}
		count++
		size += len(class.update)
	}
	return len(queue), count == roomAppendBatchUpdates || size >= roomAppendBatchBytes
}

// suppressionQueued is whether a suppression slot is queued for room's persistence. Its caller
// holds room's state.mu, which is taken before suppressMu, as failRoomLocked takes the two.
func (s *Service) suppressionQueued(room string) bool {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	return len(s.suppressed[room]) > 0
}

// warmRoomAppends opens a connection of the room-appends pool (store.Pool.RoomAppends) when the
// pool holds none, on a goroutine of its own, so the next batch of a room's browser updates finds
// one waiting. A room's load asks for it (onLoadDocument). The pool opens on demand and keeps no
// floor of idle connections (store.Pool.separate), so without it the first batch after the process
// starts, or after the pool's connections have idled out, dials its connection between its
// update's commit in the room and its durability - tens of milliseconds every wait for that
// append waits out too: a settlement's, which versions the update only once it lands, and
// Shutdown's drain. One warm-up runs at a time (warmingAppends).
func (s *Service) warmRoomAppends() {
	appends, err := s.store.Pool.RoomAppends()
	if err != nil || appends.Stat().TotalConns() > 0 || !s.warmingAppends.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.warmingAppends.Store(false)
		if conn, err := appends.Acquire(context.Background()); err == nil {
			conn.Release()
		}
	}()
}

// roomAppendBatchSizes are the upper bounds of the batch-size histogram's buckets: 1, 2, 3-4, 5-8,
// and so on to 129-256, roomAppendBatchUpdates.
var roomAppendBatchSizes = [...]int{1, 2, 4, 8, 16, 32, 64, 128, 256}

// roomAppendFigures counts the rooms' browser appends for the room figures: append transactions
// committed (batches, a lone update's included), the updates they stored, the round trips their
// transactions sent from the document's session lock to its release, failed ones included, and how
// many committed batches stored how many updates (sizes, one count per bucket of
// roomAppendBatchSizes).
type roomAppendFigures struct {
	batches    atomic.Uint64
	updates    atomic.Uint64
	roundTrips atomic.Uint64
	sizes      [len(roomAppendBatchSizes)]atomic.Uint64
}

// committed counts one committed batch of size updates.
func (f *roomAppendFigures) committed(size int) {
	f.batches.Add(1)
	f.updates.Add(uint64(size))
	f.sizes[min(sort.SearchInts(roomAppendBatchSizes[:], size), len(f.sizes)-1)].Add(1)
}

// dropUpdate takes an update the room's persistence does not store off its record: a failed
// room's, or one a suppression slot matched, which its writer made durable. A failed room's update
// is dropped and its browser resends it to the room that replaces this one, but its class is taken
// all the same, so the work the record counts for it ends here, and the update leaves its
// instance's unstored updates. Left counted, it would be an append no callback can ever finish:
// the record - or the one registered in its place, which carries the updates the room's
// persistence never took (handOverFailedRoomLocked) - would wait on it for ever, so its settlement
// would retry without end, and the record would never be released (unusedLocked).
func (a *servicePersistenceAdapter) dropUpdate(room string, update []byte) {
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found {
		a.service.finishUpdateClass(room, counted, class)
	}
	class.instance.unlistUnstored(class)
	class.credit.landed()
}
