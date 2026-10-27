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
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

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
// burst of keystrokes is queued by the time it goes on. The call then takes its batch, under the
// room's state lock (planRoomAppend): u and the run of classes queued after it, while each is
// durable work of the room instance u was made in (its cursor), is not stored, and has not been
// taken by its own update's call, at most roomAppendBatchUpdates updates and roomAppendBatchBytes
// bytes, but always u. It hands the batch to PgVersioned.AppendUpdatesWithCredit, which cuts it
// before the first update that does not decode alone and stores the rest in about three round
// trips, one row per update with its own content class and credit, the rows the same updates
// leave stored one at a time (appendWrites). A batch is fixed once taken: when its transaction
// holds the document's lock it can only lose what another transaction stored meanwhile
// (roomAppend.recheck), and an update recorded while it waited goes in a later call's batch. A
// class queued before u is not u's to take, even unstored: it is an update ygo hands on after u, as
// two peers' read loops can (consumeUpdateClass), and it keeps its own call.
//
// What holds:
//
//   - Durable on return. A call returns nil only once its update is committed: by its own
//     transaction, or by another call's that took it in its batch and committed first (its class
//     is then marked stored). ygo's flush still means every update it held is durable.
//   - Stored once. No class is stored by two of the room's appends. A class is stored only by a
//     transaction that holds the room's in-process lock and the document's advisory lock and finds
//     it, under the room's state lock, still queued and not stored (roomAppend.recheck), and every
//     transaction that commits classes marks them stored before it releases those locks
//     (roomAppend.committed): two transactions that hold one class are ordered by the locks, and
//     the second finds it stored. A class leaves its queue only with its own update's call, once
//     that call's append has ended or it dropped the update (consumeUpdateClass, dropUpdate), or
//     with its record, which is released only once every durable class on it has been taken by
//     its own call and stored or dropped (unusedLocked). Every way an update reaches the store
//     keeps to that, as the paths after this list show.
//   - Versions follow the order in which batches commit, and within a batch the queue's order. A
//     batch is u and the run after it, stored at consecutive versions from the head its gate read
//     in the order the room recorded them, its update-log order, the order it applied them. A class
//     queued before u that its own call stores later is at a later version.
//   - A stored class's counts. At its commit an append marks every class it stored stored and
//     finishes the durable append of each durable one on the record that holds the class then (a
//     failed room's handover moves a queued class with its count and its mark,
//     handOverFailedRoomLocked), so a waiter on the room's durable appends sees the batch durable
//     at once. A class's pending count ends when ygo's own call for its update takes it: that call
//     marks its class handed and leaves it queued for its own append, alone or in a batch, to find
//     and mark, and the call for a class another append already stored consumes it, appending
//     nothing and finishing nothing again.
//   - Failure outcomes. A refused gate (ErrRoomBehindHead), or a statement or a commit the database
//     refuses, stores nothing and marks nothing: the call that ran the batch fails the room and
//     returns the error, and every other update of the batch meets the failed room on its own call,
//     which drops it, ends its counts and lands its credit, as any failed room's update. Every
//     credit of the updates the transaction would store lands before it releases the document's
//     lock, committed or not (UpdateCredit.landed does nothing the second time). A batch whose own
//     update another append stored lands none: the rest of it is left to its own calls.
//   - A suppression slot forces the alone path. A published live write's update and a repair's are
//     already durable, and the room's persistence knows each by the suppression slot queued for it,
//     which it matches, in order, against the update it is handed (consumeSuppressedPersistence). A
//     slot is queued before its update is recorded and leaves only with that update's own call,
//     which marks the class handed before it checks the slot. A call that finds a slot of the room
//     queued stores its update alone, slot check first; a batch is taken only while no slot of the
//     room is queued, from classes recorded by then, and stops at a handed class, so it never holds
//     a slot's update, and a slot queued once it is taken is for an update recorded after it. A
//     failed room purges its slots (purgeSuppressedPersistence) and drops what its persistence
//     hands on while it is failed; a call after its record is handed over meets such an update as
//     any other.
//   - No holder of a document lock waits for a RoomAppends connection. A batch takes its connection
//     (store.Pool.RoomAppends) before the document's lock and holds store.HoldsConnection on its
//     context, so nothing under it takes a second connection; and nothing else takes a connection of
//     that pool - settlements, live writes and the withRoomLock writers keep theirs. Whoever holds a
//     lock a batch waits on finishes without one of these connections, so the pool cannot deadlock.
//
// The ways an update reaches the store, and why none stores a class another append stored:
//
//   - A batch (appendBatch) holds its call's own class and the run after it, each found again by
//     recheck. Its own class found stored answers errUpdateStored, and the call stores nothing;
//     another member found stored, or gone with its own call, is left out.
//   - An update stored alone with its class (appendAlone: a class not durable, or one whose call
//     found a suppression slot queued) is a batch of that one class, rechecked and marked the same
//     way: a batch that took it and committed first stored it, and one that commits after it
//     leaves it out.
//   - An update with no class - one no room recorded, as a test hands on, or a repair's whose
//     record was released, since a class that is not durable holds no record - is stored alone
//     with nothing to recheck or mark, and no batch holds it: a batch's classes come from the
//     queue, a durable class stays queued until its own call takes it (it keeps its record
//     pending until then), and ygo hands each update to one call.
//   - A writer's own update - a live write's, a settlement's repair or stamp, the block-id
//     backfill's, a seed's - is stored by its writer in its own transaction (AppendUpdateTx). The
//     room's persistence skips it by its suppression slot, and no batch holds it (a suppression
//     slot forces the alone path, above).
//   - AppendUpdate, which no room's call makes (a test's, a seed's), stores what it is handed and
//     marks nothing: no class holds its update.

// roomAppendLinger is how long after an update was recorded its call waits for the room's queue to
// fill before its batch goes to the store. A keystroke's durability then lags its commit by at most
// this beyond its transaction; a settlement, a shutdown or anything else that waits on the room's
// appends asks for them at once instead (waitForRoomIdle), so it waits out no linger.
const roomAppendLinger = 50 * time.Millisecond

// roomAppendBatchUpdates and roomAppendBatchBytes bound one batch: at most this many updates and
// this many bytes of them, but always the update whose call runs it. A queue that reaches either
// ends its batch's linger (recordUpdateClass).
const (
	roomAppendBatchUpdates = 256
	roomAppendBatchBytes   = 1 << 20
)

// BrowserUpdate is one update an append transaction stores (VersionedStore.AppendUpdatesWithCredit,
// appendWrites): its bytes, whether it changes the document's rendered content, and the browser
// edit's in-flight credit, nil for an update no browser edit credited. Its fields are exported so a
// test store in another package can hand a batch on; UpdateCredit's are not.
type BrowserUpdate struct {
	Update         []byte
	ContentChanged bool
	Credit         *UpdateCredit
}

// errUpdateStored is the answer of an append whose own update another append of the room stored
// while this one waited for the document's lock: it stores nothing, and its call consumes the
// class.
var errUpdateStored = errors.New("the update was stored by another append of its room")

// AppendUpdatesWithCredit validates updates, each alone, and stores them in one transaction, the
// first at the version after the head, and returns that first version. It holds the room's
// in-process lock and a connection of the room-appends pool (store.Pool.RoomAppends) throughout,
// and sends its statements as pgx pipelines, three round trips from the document's session lock to
// its release:
//
//  1. pg_advisory_lock, begin, pg_advisory_xact_lock, the prune recovery (recoverPruneStatement)
//     and the gate's read (queueAppendGate);
//  2. once the gate has passed, the append's writes (appendWrites) and commit;
//  3. pg_advisory_unlock, once the commit's bookkeeping is done: each update's credit held at its
//     version (UpdateCredit.held), afterAppendCommit, the room cursor ctx is marked with advanced
//     once per version, the room's side of the append (roomAppend.committed), and the end of the
//     write the head tracker has followed since the gate (beginOwnWrite), with the head it left.
//     Every stored update's credit lands before this, committed or not.
//
// A room's own call marks ctx with its side of the append (withRoomAppend): the classes updates
// were taken from, one per update. For it an update after the first that does not decode alone
// ends the batch before itself (roomAppend.cut), where any other append fails on it; and once the
// first pipeline holds the document's lock, the append keeps only the classes still queued and
// not stored (roomAppend.recheck), which may answer errUpdateStored. The gate decides on the cursor
// as read once the document's lock is held (appendGate.throughUnderLock). Any database error rolls
// back what is open and still releases the session lock, as lockRoom's does.
func (p *PgVersioned) AppendUpdatesWithCredit(ctx context.Context, room string, updates []BrowserUpdate) (persistence.Version, error) {
	if len(updates) == 0 {
		return 0, errors.New("append document updates: no update to append")
	}
	appending := roomAppendFrom(ctx)
	for index, update := range updates {
		if err := crdt.ApplyUpdateV1(newDocumentCopy(), update.Update, nil); err != nil {
			cut, ok := appending.cut(updates, index)
			if !ok {
				return 0, err
			}
			updates = cut
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	lock := p.acquireRoomLock(room)
	defer p.releaseRoomLock(room, lock)
	lock.Lock()
	defer lock.Unlock()
	appends, err := p.store.Pool.RoomAppends()
	if err != nil {
		return 0, err
	}
	conn, err := appends.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire document append connection: %w", err)
	}
	defer conn.Release()
	// The transaction holds this connection and the document's advisory lock; anything it reads
	// reads through them, never through a second pooled connection.
	ctx, releaseMark := store.HoldsConnection(ctx)
	defer releaseMark()

	trips := 0
	send := func(ctx context.Context, batch *pgx.Batch) error {
		trips++
		return conn.SendBatch(ctx, batch).Close()
	}
	defer func() { appending.spent(trips) }()
	open, landing := true, updates
	defer func() {
		// The credits land while the session lock is still held, whether the transaction committed
		// or not: no version reads the pending authors between a commit and its credits leaving the
		// room's in-flight credits.
		for _, update := range landing {
			update.Credit.landed()
		}
		end := &pgx.Batch{}
		if open {
			end.Queue(`rollback`)
		}
		end.Queue(`select pg_advisory_unlock(hashtext($1))`, room)
		_ = send(context.Background(), end)
	}()
	// The head tracker follows the transaction as this process's write from its gate until the
	// commit's bookkeeping is done, the cursor's advance among it, and before the session lock goes:
	// this deferred call, registered after the one that releases the lock, runs before it.
	write := p.heads.beginOwnWrite(room)
	stored := int64(-1)
	defer func() { write.end(stored >= 0, stored) }()

	first := &pgx.Batch{}
	first.Queue(`select pg_advisory_lock(hashtext($1))`, room)
	first.Queue(`begin`)
	first.Queue(`select pg_advisory_xact_lock(hashtext($1))`, room)
	first.Queue(recoverPruneStatement, room)
	gate := queueAppendGate(first, room, appendingThrough(ctx, room), appendBrowser)
	if err := send(ctx, first); err != nil {
		return 0, fmt.Errorf("begin document update: %w", err)
	}
	updates, err = appending.recheck(updates)
	landing = updates
	if err != nil {
		return 0, err
	}
	gate.throughUnderLock(appendingThrough(ctx, room))
	head, err := gate.head()
	if err != nil {
		return 0, err
	}
	statements, last, err := p.appendWrites(room, head, updates)
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
	for index, update := range updates {
		update.Credit.held(persistence.Version(head + int64(index) + 1))
	}
	if p.afterAppendCommit != nil {
		p.afterAppendCommit(room, last)
	}
	// The room instance's cursor advances here, inside the lock the append holds, rather than
	// where the caller marked the context with it (StoreUpdateContext): a writer that took the
	// lock after this commit released it would otherwise read a cursor behind the head it reads
	// under that lock, and the gate would refuse an agent's edit while nothing was wrong with its
	// room.
	cursor := roomCursorFrom(ctx)
	for version := head + 1; version <= int64(last); version++ {
		advanceRoomCursor(cursor, persistence.Version(version))
	}
	appending.committed(head, int64(last), gate.oldest())
	return persistence.Version(head + 1), nil
}

// roomAppend is a room's side of one append transaction of its browser updates
// (AppendUpdatesWithCredit), which the room's call marks the transaction's context with: the
// service and room it counts and finishes on, and the classes the append stores. A nil roomAppend
// is an append no room made - a test's, a seed's - and does nothing.
type roomAppend struct {
	service *Service
	room    string
	// members are the classes the append's updates were taken from, one per update and in the
	// same order, the first the class of the update whose call makes the append (planRoomAppend);
	// none for an update no class holds. Copies, taken under the room's state lock: each is found
	// again on the queue by its update's slice (sameUpdate).
	members []documentUpdateClass
}

type roomAppendContextKey struct{}

// withRoomAppend marks ctx with appending, the room's side of the append made under it.
func withRoomAppend(ctx context.Context, appending *roomAppend) context.Context {
	return context.WithValue(ctx, roomAppendContextKey{}, appending)
}

// roomAppendFrom is the roomAppend withRoomAppend marked ctx with, nil for none.
func roomAppendFrom(ctx context.Context) *roomAppend {
	appending, _ := ctx.Value(roomAppendContextKey{}).(*roomAppend)
	return appending
}

// cut ends a room's batch before updates[index], the first of them that does not decode alone,
// and answers what is left of updates and true. The update cut is left to its own call, whose
// append it then leads and fails, failing the room. The first update, and any update of an append
// that is not a room's batch, cannot be cut: false.
func (a *roomAppend) cut(updates []BrowserUpdate, index int) ([]BrowserUpdate, bool) {
	if a == nil || index == 0 || len(a.members) != len(updates) {
		return updates, false
	}
	a.members = a.members[:index]
	return updates[:index], true
}

// recheck is what the append stores, read under the room's state lock once its transaction holds
// the room's in-process lock and the document's advisory lock: of updates, those whose class is
// still queued and not stored. Every append that commits a class marks it under those same locks
// (committed), so a class found unstored here is one no other append has stored or can store
// before this transaction ends. The append's own class found stored answers errUpdateStored. A
// member no longer queued is left out: its own call has taken it off, having dropped it (a failed
// room's update) or seen its own append end. The append's own class stays queued while its call
// runs, unless the record goes with it, which only a class that is not durable allows: such a
// class is never another append's, and its update is stored. updates and the members are compacted
// in place: the room's call built both for this append alone.
func (a *roomAppend) recheck(updates []BrowserUpdate) ([]BrowserUpdate, error) {
	if a == nil || len(a.members) == 0 {
		return updates, nil
	}
	var queue []documentUpdateClass
	if state := a.service.lockExistingState(a.room); state != nil {
		defer a.service.unlockState(a.room, state)
		queue = state.updateClasses
	}
	// The members stand in the queue in the order taken, so each is looked for past the one
	// before it.
	kept, from := 0, 0
	for index, member := range a.members {
		at := queuedUpdateLocked(queue, member.update, from)
		if at >= 0 {
			from = at + 1
		}
		switch {
		case at >= 0 && queue[at].stored && index == 0:
			a.members = nil
			return nil, errUpdateStored
		case at >= 0 && queue[at].stored, at < 0 && index > 0:
			continue
		}
		a.members[kept], updates[kept] = member, updates[index]
		kept++
	}
	a.members = a.members[:kept]
	return updates[:kept], nil
}

// queuedUpdateLocked is the index in queue, from from on, of the class that holds the very slice
// update is (sameUpdate), -1 for none. A batch's members are copies of queued classes, so each is
// found by its own slice. Its caller holds the queue's state.mu.
func queuedUpdateLocked(queue []documentUpdateClass, update []byte, from int) int {
	for index := from; index < len(queue); index++ {
		if sameUpdate(queue[index].update, update) {
			return index
		}
	}
	return -1
}

// committed is the room's bookkeeping for a committed append, before the transaction releases the
// document's lock: the figures, INFRA-593's noteStoredUpdates with the head the append left and the
// oldest version the gate read, every class it stored marked stored, with its durable append, if
// it is durable, finished on the record that holds it now (countsFellLocked, which wakes the
// room's waiters once nothing is left to store), and the settlement scheduled once when one of
// the durable ones changed content. The marks are made under the lock, so another append that
// takes the lock next finds them (recheck).
func (a *roomAppend) committed(head, last, oldest int64) {
	if a == nil {
		return
	}
	a.service.appendFigures.committed(int(last - head))
	a.service.noteStoredUpdates(a.room, last, oldest)
	if len(a.members) == 0 {
		return
	}
	settles := false
	if state := a.service.lockExistingState(a.room); state != nil {
		// The members stand in the queue in the order taken, as in recheck.
		from := 0
		for _, member := range a.members {
			at := queuedUpdateLocked(state.updateClasses, member.update, from)
			if at < 0 {
				continue
			}
			from = at + 1
			queued := &state.updateClasses[at]
			if queued.stored {
				continue
			}
			queued.stored = true
			if queued.durable {
				state.durableAppends.Add(-1)
				settles = settles || queued.contentChanged
			}
		}
		a.service.countsFellLocked(a.room, state)
		a.service.unlockState(a.room, state)
	}
	if settles {
		a.service.scheduleSettleAfterAppend(a.room)
	}
}

// spent adds the round trips the append's transaction sent to the room figures.
func (a *roomAppend) spent(trips int) {
	if a != nil {
		a.service.appendFigures.roundTrips.Add(uint64(trips))
	}
}

// batchRunLocked is the run of queue a batch for the class at index at takes: from at to end, the
// first class past at that is stored, handed (its own call stores it), not durable work, or of
// another room instance (cursor) than at's, or that would take the run past
// roomAppendBatchUpdates or roomAppendBatchBytes - at's own class is taken whatever its size. full
// is whether a cap ended it, or the run fills one. Its caller holds the queue's state.mu.
func batchRunLocked(queue []documentUpdateClass, at int) (end int, full bool) {
	cursor := queue[at].cursor
	count, size := 0, 0
	for index := at; index < len(queue); index++ {
		class := queue[index]
		if index > at && (class.stored || class.handed || !class.durable || class.cursor != cursor) {
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

// appendPlan is how a room's call stores its update (planRoomAppend).
type appendPlan int

const (
	// appendAlone stores the update by itself, after its suppression slot check
	// (StoreUpdateContext).
	appendAlone appendPlan = iota
	// appendBatch stores the update with the run of the room's queue after it (batchRunLocked).
	appendBatch
	// appendStored only consumes the update's class, which planRoomAppend does: another call's
	// append stored it.
	appendStored
)

// planRoomAppend is how the call for update stores it, read under room's state lock, and the
// classes its append takes (roomAppend.members): appendStored, and none, for a class another
// append already stored, which it consumes under the same lock (consumeUpdateClassLocked);
// appendAlone for an update with no class, and none, for a class that is not durable or while a
// suppression slot is queued for the room, and the class; appendBatch otherwise, and the class
// with the run after it (batchRunLocked). Every class it finds and does not answer appendStored
// for is marked handed, its pending count ended as a class consumed at the start of its call ends
// it, since the call has taken it; it stays queued for the call's append to find and mark, and no
// other call's batch takes it. With linger, a batch whose run is not full yet answers no classes
// but the channel its linger waits on and how long it waits: until roomAppendLinger after the
// class was recorded, none while the service is stopping, while a wait for the room's appends is
// waiting (appendWaiters, waitForRoomIdle), or once something asked the room's appends to go after
// the class was recorded (appendsFlushed, waitForRoomIdle). The call then asks again without
// linger, and the batch is taken then.
func (s *Service) planRoomAppend(room string, update []byte, linger bool) (plan appendPlan, members []documentUpdateClass, wake <-chan struct{}, wait time.Duration) {
	state := s.lockExistingState(room)
	if state == nil {
		return appendAlone, nil, nil, 0
	}
	defer s.unlockState(room, state)
	at := findUpdateClassLocked(state.updateClasses, update)
	if at < 0 {
		return appendAlone, nil, nil, 0
	}
	class := &state.updateClasses[at]
	if class.stored {
		s.consumeUpdateClassLocked(room, state, at)
		return appendStored, nil, nil, 0
	}
	if !class.handed {
		class.handed = true
		if class.durable {
			state.pendingUpdates--
			s.countsFellLocked(room, state)
		}
	}
	if !class.durable || s.suppressionQueued(room) {
		return appendAlone, []documentUpdateClass{*class}, nil, 0
	}
	end, full := batchRunLocked(state.updateClasses, at)
	if linger && !full && state.appendWaiters == 0 && !s.stopping.Load() && !s.shuttingDown(room) && class.recorded.After(state.appendsFlushed) {
		if wait = time.Until(class.recorded.Add(roomAppendLinger)); wait > 0 {
			return appendBatch, nil, state.appendLinger.wait(), wait
		}
	}
	return appendBatch, slices.Clone(state.updateClasses[at:end]), nil, 0
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

// noteStoredUpdates is told of each committed append of room's browser updates, after its commit
// and before it releases the document's lock: head is the last version it stored, the room's head
// now, and oldest the least version the room stored when its gate read the head, 0 for none. It is
// INFRA-593's hook for the room's stored history and keeps nothing yet.
func (s *Service) noteStoredUpdates(room string, head, oldest int64) {}

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

// storeUpdates stores update with members, the classes planRoomAppend took for its append - update's
// own class and the run after it for a batch, the one class for an update stored alone, none for
// an update with no class - gated on the cursor of update's class, and consumes update's class once
// the append has ended (planRoomAppend ended its pending count when the call took it). Stored, by
// this append or by another that marked it, its append finished the rest; refused, the call fails
// the room and ends the class's durable count and credit as a failed room's update.
func (a *servicePersistenceAdapter) storeUpdates(ctx context.Context, room string, update []byte, members []documentUpdateClass) error {
	var updates []BrowserUpdate
	var cursor *roomCursor
	if len(members) == 0 {
		updates = []BrowserUpdate{{Update: update, ContentChanged: true}}
	} else {
		cursor = members[0].cursor
		updates = make([]BrowserUpdate, len(members))
		for index, class := range members {
			updates[index] = BrowserUpdate{Update: class.update, ContentChanged: class.contentChanged, Credit: class.credit}
		}
	}
	ctx = withRoomAppend(withRoomCursor(ctx, cursor), &roomAppend{service: a.service, room: room, members: members})
	_, err := a.store.AppendUpdatesWithCredit(ctx, room, updates)
	if errors.Is(err, errUpdateStored) {
		err = nil
	}
	consumed, counted, found := a.service.consumeUpdateClass(room, update)
	if err != nil {
		a.service.failRoom(room, err)
	}
	if found && consumed.durable && !consumed.stored {
		a.service.finishDurableAppend(room, counted)
	}
	consumed.credit.landed()
	return err
}

// dropUpdate takes an update the room's persistence does not store off its record: a failed
// room's, or one a suppression slot matched, which its writer made durable. A failed room's update
// is dropped and its browser resends it to the room that replaces this one, but its class is taken
// all the same, so the work the record counts for it ends here. Left counted, it would be an
// append no callback can ever finish: the record - or the one registered in its place, which
// carries the updates the room's persistence never took (handOverFailedRoomLocked) - would wait on
// it for ever, so its settlement would retry without end, never closing a room behind the head
// (reloadRoomBehindHead), and the record would never be released (unusedLocked). A class an append
// stored before the room failed has had its durable count finished already, and ends only its
// pending one.
func (a *servicePersistenceAdapter) dropUpdate(room string, update []byte) {
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found && class.durable && !class.stored {
		a.service.finishDurableAppend(room, counted)
	}
	class.credit.landed()
}
