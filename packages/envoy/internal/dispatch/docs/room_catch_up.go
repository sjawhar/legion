package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
	"weak"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"
)

// Room catch-up is what a process does with the durable change another process announces
// (room_sync.go): it brings its own room of that document up to the stored head, so a room a
// remote write left behind takes that write in and stays open, and only a room whose stored
// history was replaced under it is closed and reloaded. roomCatchUps is the follower that runs it,
// on the lane room sync gives the document-change channels.

// documentHead is a documentHeadChannel payload: the document (R), the head its change left (V)
// and the process that made it (P).
type documentHead struct {
	R string `json:"r"`
	V int64  `json:"v"`
	P string `json:"p"`
}

// remoteUpdateOrigin tags the Yjs transaction a catch-up applies to a room (catchUpRoom): rows
// another process stored, already durable, which the room's persistence must not store again. It
// carries the suppression slot that holds them from it, finished by the room's update observer
// before ygo's persistence observer hands the update on (onLoadDocument), exactly as a published
// live write's is (liveWriteOrigin). Being non-zero sized keeps each origin distinct, since ygo
// compares origins by interface equality.
type remoteUpdateOrigin struct{ slot *suppressSlot }

// catchUpOutcome is what one catch-up of a room found (catchUpRoom).
type catchUpOutcome int

const (
	// catchUpCurrent: the room already held every stored row; nothing was applied.
	catchUpCurrent catchUpOutcome = iota
	// catchUpApplied: the room took in the stored rows it lacked, as one batch.
	catchUpApplied
	// catchUpReplaced: the stored history is no longer the one the room holds a prefix of
	// (historyReplaced), so no row can bring it up to date: the room is closed and reloaded.
	catchUpReplaced
	// catchUpGone: the room no longer holds the instance asked about, or none at all.
	catchUpGone
)

// catchUpStatement reads, in one statement and so one snapshot, the bounds of room $1's stored
// history - its lowest version and its head (a prune's checkpoint ceiling while one stands, as
// headVersion has it) - and every row past $2, the version the room's instance holds every update
// through, up to that head. The bounds come back on every row, and on a row of their own with no
// version when there is nothing past $2.
const catchUpStatement = `
	with bounds as (
		select coalesce(min(version), 0) as low,
		       coalesce((select ceiling from doc_checkpoints where artifact_id = $1), max(version), 0) as high
		from doc_updates where artifact_id = $1
	)
	select bounds.low, bounds.high, stored.version, stored.update
	from bounds
	left join doc_updates stored
		on stored.artifact_id = $1 and stored.version > $2 and stored.version <= bounds.high
	order by stored.version`

// storedRow is one stored update a catch-up read.
type storedRow struct {
	version int64
	update  []byte
}

// readCatchUp is catchUpStatement through q: the lowest stored version and the head (0 for none),
// and the rows past through in version order.
func readCatchUp(ctx context.Context, q Queryer, room string, through int64) (low, high int64, rows []storedRow, err error) {
	result, err := q.Query(ctx, catchUpStatement, room, through)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("read the document's stored updates past its room: %w", err)
	}
	defer result.Close()
	for result.Next() {
		var version *int64
		var update []byte
		if err := result.Scan(&low, &high, &version, &update); err != nil {
			return 0, 0, nil, fmt.Errorf("read the document's stored updates past its room: %w", err)
		}
		if version != nil {
			rows = append(rows, storedRow{version: *version, update: update})
		}
	}
	if err := result.Err(); err != nil {
		return 0, 0, nil, fmt.Errorf("read the document's stored updates past its room: %w", err)
	}
	return low, high, rows, nil
}

// catchUpRoom brings live, room's resident instance, up to the stored head: it reads the stored
// rows past the instance's cursor through q (catchUpStatement, one snapshot), skips the ones the
// instance already holds - its own browsers' appends and published live writes made while it was
// behind (roomCursor.above) - and applies the rest as one batch, the way publishLiveUpdate applies
// a committed write: a Yjs transaction tagged remoteUpdateOrigin, under a suppression slot so the
// room's persistence does not store again what is already stored, then a broadcast to the room's
// peers. Each applied row then enters the instance's hold (so an upload on this process consumes
// its pending-author rows, as for a reload) and its cursor, which folds whatever the instance
// appended past the gap. The hold and the cursor move after the update is in the document, not
// before: a derived write reads the cursor under the document's advisory lock, which this does not
// hold, and a cursor ahead of the document would let a settlement version text missing the rows
// it says are held.
//
// It writes through srv.GetDoc and roomServer.holdOpen, never Server.Apply, which would load an
// absent room and wait on a failed one: a room that has gone, or that a close holds, is left
// alone (catchUpGone), and the room that replaces it loads at the head. One catch-up of an
// instance runs at a time (roomInstance.catchUp); the caller holds the document's advisory lock
// only when it already did (a settlement, reading through its own transaction), and the follower
// (roomCatchUps) never takes it while it holds that mutex. A history that is no longer the one the
// instance holds a prefix of is reported, not applied (catchUpReplaced): the caller closes the
// room. A batch that cannot be merged, applied or broadcast fails the room, which reloads from
// the store, and is returned as the error.
//
// After a batch the document must have nothing pending: every row past the cursor was applied,
// and each depends only on rows before it, so an item left waiting for a missing dependency is a
// store that holds a state the document never held (plan D3.8). That is logged at ERROR, naming
// the room and the versions, and the room is left as it is.
func (s *Service) catchUpRoom(ctx context.Context, q Queryer, room string, live *crdt.Doc) (catchUpOutcome, error) {
	instance := s.instanceOf(live)
	if instance == nil {
		return catchUpGone, nil
	}
	instance.catchUp.Lock()
	defer instance.catchUp.Unlock()
	if s.srv.GetDoc(room) != live {
		return catchUpGone, nil
	}
	through, above := instance.cursor.held()
	low, high, rows, err := readCatchUp(ctx, q, room, through)
	if s.catchUps.afterRead != nil {
		s.catchUps.afterRead(room)
	}
	if err != nil {
		return 0, err
	}
	if storedHistoryReplaced(through, low, high) {
		return catchUpReplaced, nil
	}
	versions := make([]int64, 0, len(rows))
	updates := make([][]byte, 0, len(rows))
	for _, row := range rows {
		if _, held := slices.BinarySearch(above, row.version); held {
			continue
		}
		versions = append(versions, row.version)
		updates = append(updates, row.update)
	}
	if len(updates) == 0 {
		return catchUpCurrent, nil
	}
	batch, err := mergeUpdates(updates)
	if err != nil {
		err = fmt.Errorf("merge the stored updates a catch-up applies: %w", err)
		s.failRoom(room, err)
		return 0, err
	}
	applied, open, err := s.applyRemoteBatch(room, live, batch)
	if !open {
		return catchUpGone, nil
	}
	if err != nil {
		err = fmt.Errorf("apply the stored updates a catch-up read: %w", err)
		s.failRoom(room, err)
		return 0, err
	}
	for _, version := range versions {
		instance.hold.add(version)
		advanceRoomCursor(&instance.cursor, persistence.Version(version))
	}
	if parks(live) {
		pending := live.PendingStats()
		slog.Error("dispatch: a document room holds updates it cannot place after catching up to the stored head",
			"room", room, "versions", versions, "pending_items", pending.Items, "pending_delete_ranges", pending.DeleteRanges)
	}
	if applied == nil {
		return catchUpApplied, nil
	}
	if err := s.srv.BroadcastUpdate(withOwnerVerified(WithoutRecoveryWait(ctx)), room, applied); err != nil &&
		!errors.Is(err, websocket.ErrRoomNotFound) && !errors.Is(err, websocket.ErrServerShutdown) && !errors.Is(err, ErrServiceUnavailable) {
		// The room holds what its browsers did not receive; failing it closes them, and they sync
		// the stored document when they reconnect.
		err = fmt.Errorf("broadcast the stored updates a catch-up applied: %w", err)
		s.failRoom(room, err)
		return 0, err
	}
	return catchUpApplied, nil
}

// applyRemoteBatch applies batch to live under a suppression slot of its own, holding the room
// open (holdOpen) so no close retires its persistence worker under the commit. It returns what the
// room recorded for the batch's transaction - nil when the batch changed nothing, whose slot it
// cancels, since a slot left queued holds the worker at the room's next update - and whether the
// room still held live; a room that does not writes nothing.
func (s *Service) applyRemoteBatch(room string, live *crdt.Doc, batch []byte) (applied []byte, open bool, err error) {
	release, open := s.srv.holdOpen(room, live)
	if !open {
		return nil, false, nil
	}
	defer release()
	slot := s.prepareSuppressedPersistence(room)
	origin := &remoteUpdateOrigin{slot: slot}
	unsubscribe := live.OnUpdate(func(update []byte, updateOrigin any) {
		if updateOrigin == origin {
			applied = append([]byte(nil), update...)
		}
	})
	err = crdt.ApplyUpdateV1(live, batch, origin)
	unsubscribe()
	if applied == nil {
		s.cancelSuppressedPersistence(room, slot)
	}
	return applied, true, err
}

// historyReplacedUnderLock reads, under the document's advisory lock, whether room's stored history
// is replaced under live's cursor (historyReplaced), for the follower to confirm a verdict before
// it closes the room. Read without the lock the verdict can be this process's own write between
// its cursor advance and its commit - a live write records its versions on the cursor before it
// commits (Ledger.takeRoomCursors), and so does settlement's own repair - which reads as a head
// below the cursor; under the lock no such write is in flight. The follower calls it holding no
// catch-up mutex, so the lock order (advisory lock, then that mutex) holds.
func (s *Service) historyReplacedUnderLock(ctx context.Context, room string, live *crdt.Doc) (bool, error) {
	cursor := s.cursorOf(live)
	if cursor == nil {
		return false, nil
	}
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return false, err
	}
	through := cursor.heldThrough()
	var low, high int64
	if err := tx.QueryRow(ctx, `
		select coalesce(min(version), 0), coalesce(max(version), 0) from doc_updates where artifact_id = $1
	`, room).Scan(&low, &high); err != nil {
		return false, fmt.Errorf("read the document's stored bounds: %w", err)
	}
	return storedHistoryReplaced(through, low, high), nil
}

// storedHistoryReplaced is historyReplaced for a room held through through, against the stored
// history's bounds: low, its lowest version, and high, its head, each 0 for an empty history.
func storedHistoryReplaced(through, low, high int64) bool {
	return historyReplaced(through, high, low != 0 && low <= through)
}

// catchUpRetryInterval is the first wait before a room whose catch-up reached no verdict - a read
// that failed, a close of a replaced history that failed or that something of this process held
// off - is tried again, doubling for each such attempt in a row up to catchUpRetryBackoffMax.
// Nothing else comes back for such a room: the notification that brought it here is spent, and a
// document nobody writes again notifies nobody again. At the ceiling a room that keeps getting no
// answer costs one wake a minute rather than one every interval for the life of the process. It
// adds no jitter, for it is one process re-reading one of its own rooms, which nothing else is in
// step with.
const (
	catchUpRetryInterval   = 2 * time.Second
	catchUpRetryBackoffMax = time.Minute
)

// catchUpRetryBackoff is the wait before the attempt-th try in a row (from 1) of a room whose
// catch-ups reach no verdict.
func catchUpRetryBackoff(attempt int) time.Duration {
	wait := catchUpRetryInterval
	for n := 1; n < attempt && wait < catchUpRetryBackoffMax; n++ {
		wait *= 2
	}
	return min(wait, catchUpRetryBackoffMax)
}

// catchUpLoadingWait is how often, and catchUpLoadingTimeout for how long, a catch-up scheduled for
// a room still loading (keepInstance) looks again for ygo to publish it.
const (
	catchUpLoadingWait    = 10 * time.Millisecond
	catchUpLoadingTimeout = 5 * time.Second
)

// roomCatchUps is the follower: it keeps this process's rooms at the stored head as other
// processes append, on documentChangedChannel's lane (roomSync.handlers), one goroutine that also
// carries documentHeadChannel, so the two channels' notifications are decided in the order they
// arrived. What wakes a catch-up of a room:
//
//   - a documentHeadChannel notification from another process for a head past the room's cursor
//     (headNotified); this process's own (p is its id) and one the room already holds wake none;
//   - a bare documentChangedChannel notification (changed): an old build's write during a roll
//     sends only that, and so does every write as the twin of its documentHeadChannel
//     notification, which is dropped when the head before it was this process's own;
//   - each LISTEN that stands, for every room this process has loaded (sweep): a notification
//     sent while the hub had no connection reached nobody;
//   - each room instance that starts (schedule), whose load can have raced an append;
//   - a retry, for a room whose last catch-up reached no verdict (woke).
//
// Requests coalesce per room: a burst of notifications for one room that arrive before its
// catch-up runs costs that one catch-up, and a catch-up that finds the room at the head ends with
// that one read.
type roomCatchUps struct {
	service *Service
	// now is the clock retries are timed on, the service's; a test moves its own.
	now func() time.Time
	// decided runs after each notification is decided, afterRead after each catch-up's read. Nil
	// outside tests: a test reads them to know a notification was decided and whether it read.
	decided   func(room string)
	afterRead func(room string)
	// ownHead is the room of the last documentHeadChannel notification, when this process sent it:
	// the bare notification right after it is its twin (announceDocumentHead). Only the lane reads
	// and writes it.
	ownHead string

	mu sync.Mutex
	// lane is the lane that runs the catch-ups (attach), nil until room sync is registered.
	lane *roomSyncLane
	// pending holds each room whose catch-up is queued on the lane and has not started: a request
	// for it meanwhile adds nothing.
	pending map[string]bool
	// loading holds, for each room whose instance scheduled a catch-up while still loading, that
	// instance and how long to wait for ygo to publish it.
	loading map[string]loadingRoom
	// deferred is when each room waiting to be tried again may be, and attempts how many tries in
	// a row reached no verdict for it; a verdict takes the room out of both (settled).
	deferred map[string]time.Time
	attempts map[string]int
}

// loadingRoom is a room instance that scheduled its own catch-up while ygo was still loading it.
type loadingRoom struct {
	instance weak.Pointer[crdt.Doc]
	until    time.Time
}

func newRoomCatchUps(service *Service) *roomCatchUps {
	return &roomCatchUps{
		service:  service,
		now:      service.now,
		pending:  map[string]bool{},
		loading:  map[string]loadingRoom{},
		deferred: map[string]time.Time{},
		attempts: map[string]int{},
	}
}

// attach makes lane the one that runs the catch-ups.
func (c *roomCatchUps) attach(lane *roomSyncLane) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lane = lane
}

// schedule queues a catch-up of live, room's instance still loading (keepInstance). Before room
// sync is registered there is no lane and nothing to queue: the sweep after its first LISTEN
// catches the room up.
func (c *roomCatchUps) schedule(room string, live *crdt.Doc) {
	c.mu.Lock()
	if c.lane == nil {
		c.mu.Unlock()
		return
	}
	c.loading[room] = loadingRoom{instance: weak.Make(live), until: c.now().Add(catchUpLoadingTimeout)}
	c.mu.Unlock()
	c.request(room)
}

// request queues one catch-up of room on the lane, unless one is queued already. Before room sync
// is registered there is no lane, and nothing is queued: the sweep after its first LISTEN catches
// up every room this process has loaded.
func (c *roomCatchUps) request(room string) {
	c.mu.Lock()
	if c.pending[room] || c.lane == nil {
		c.mu.Unlock()
		return
	}
	c.pending[room] = true
	lane := c.lane
	c.mu.Unlock()
	lane.push(func(ctx context.Context) {
		c.mu.Lock()
		delete(c.pending, room)
		c.mu.Unlock()
		c.catchUp(ctx, room)
	})
}

// headNotified is documentHeadChannel's handler.
func (c *roomCatchUps) headNotified(_ context.Context, payload string) {
	var head documentHead
	if err := json.Unmarshal([]byte(payload), &head); err != nil || head.R == "" {
		c.ownHead = ""
		slog.Warn("dispatch: drop a document head notification that does not decode", "payload", payload, "error", err)
		return
	}
	defer c.noteDecided(head.R)
	if head.P == c.service.processID {
		c.ownHead = head.R
		return
	}
	c.ownHead = ""
	if live := c.service.srv.GetDoc(head.R); live != nil {
		if cursor := c.service.cursorOf(live); cursor != nil && head.V <= cursor.heldThrough() {
			return
		}
	}
	c.request(head.R)
}

// changed is documentChangedChannel's handler: the bare artifact id. It is the twin of the
// documentHeadChannel notification right before it when that one was this process's own, and is
// dropped; any other is an old build's write, or a new build's whose head this process had no
// reason to read for - a prune lowering the head reads as a head the room already holds - and
// asks for a catch-up, which a burst coalesces.
func (c *roomCatchUps) changed(_ context.Context, room string) {
	defer c.noteDecided(room)
	own := c.ownHead == room
	c.ownHead = ""
	if own {
		return
	}
	c.request(room)
}

func (c *roomCatchUps) noteDecided(room string) {
	if c.decided != nil {
		c.decided(room)
	}
}

// sweep catches up every room this process has loaded; it runs after each LISTEN stands
// (roomSync.connected). service.roomLoads is every room loaded since its document was last
// collected (keepInstance), so this ranges over every resident room; an entry whose room has gone
// costs a lookup and no database read.
func (c *roomCatchUps) sweep(ctx context.Context) {
	c.service.roomLoads.Range(func(room, _ any) bool {
		c.catchUp(ctx, room.(string))
		return true
	})
}

// woke is the lane's wake (roomSyncHandler.woke, asked of wakeIn): each room whose retry is due,
// caught up again.
func (c *roomCatchUps) woke(ctx context.Context) {
	for _, room := range c.dueRooms() {
		c.catchUp(ctx, room)
	}
}

// catchUp is one catch-up of room's resident instance on the lane (catchUpRoom, read through the
// pool), and what follows from it: a replaced history closes the room (closeReplaced), and a
// catch-up that reached no verdict is tried again later (armRetry). A room with no instance
// resident has nothing to catch up, but one still loading is looked for again shortly.
func (c *roomCatchUps) catchUp(ctx context.Context, room string) {
	service := c.service
	live := service.srv.GetDoc(room)
	if !c.takeLoading(room, live) {
		return
	}
	if live == nil {
		c.settled(room)
		return
	}
	outcome, err := service.catchUpRoom(ctx, service.store.Pool, room, live)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			slog.Warn("dispatch: catch up a document room to the stored head", "room", room, "error", err)
		}
		c.armRetry(room)
	case outcome == catchUpReplaced:
		c.closeReplaced(ctx, room, live)
	default:
		c.settled(room)
	}
}

// takeLoading reports whether the catch-up of room should run now, live being the instance the
// room holds (nil for none): a room whose instance scheduled this catch-up while loading and is
// not published yet is looked for again in catchUpLoadingWait, until that instance is collected or
// catchUpLoadingTimeout passes.
func (c *roomCatchUps) takeLoading(room string, live *crdt.Doc) bool {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	loading, ok := c.loading[room]
	if !ok {
		return true
	}
	if live == nil {
		if instance := loading.instance.Value(); instance != nil && now.Before(loading.until) {
			c.deferred[room] = now.Add(catchUpLoadingWait)
			return false
		}
	}
	delete(c.loading, room)
	return true
}

// closeReplaced closes live, room's instance whose stored history was replaced (catchUpReplaced),
// once a read under the document's lock confirms it (historyReplacedUnderLock), so the next access
// loads the document at the head. A verdict the locked read does not confirm was this process's
// own write in flight, and the room is caught up again instead. The close is closeRoomBehindHead's,
// which an open live write of this process holds off; held off or failed, the room is tried again.
func (c *roomCatchUps) closeReplaced(ctx context.Context, room string, live *crdt.Doc) {
	service := c.service
	replaced, err := service.historyReplacedUnderLock(ctx, room, live)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: confirm a document room's stored history was replaced", "room", room, "error", err)
		}
		c.armRetry(room)
		return
	}
	if !replaced {
		c.settled(room)
		c.request(room)
		return
	}
	closed, err := service.closeRoomBehindHead(ctx, room, live)
	switch {
	case errors.Is(err, errRoomCloseHeldOff):
		c.armRetry(room)
	case err != nil:
		slog.Warn("dispatch: close a document room whose stored history was replaced", "room", room, "error", err)
		c.armRetry(room)
	case closed:
		c.settled(room)
		slog.Info("dispatch: closed a document room whose stored history was replaced", "room", room)
	default:
		// Another close or another instance answers for the room now (closeRoomBehindHead).
		c.settled(room)
	}
}

// wakeIn is the lane's clock (roomSyncHandler.nextWake): how long until the earliest room waiting
// to be tried again may be, and zero, never, while none waits.
func (c *roomCatchUps) wakeIn() time.Duration {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var earliest time.Time
	for _, at := range c.deferred {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	if earliest.IsZero() {
		return 0
	}
	// A time already passed asks for the shortest wait there is, rather than the zero that would
	// read as "never".
	return max(earliest.Sub(now), time.Nanosecond)
}

// dueRooms takes the rooms whose retry is due out of the deferred set, for the wake to catch up.
func (c *roomCatchUps) dueRooms() []string {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var due []string
	for room, at := range c.deferred {
		if !now.Before(at) {
			due = append(due, room)
			delete(c.deferred, room)
		}
	}
	return due
}

// armRetry gives room another try after one that reached no verdict, further out for each such
// try in a row (catchUpRetryBackoff).
func (c *roomCatchUps) armRetry(room string) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[room]++
	c.deferred[room] = now.Add(catchUpRetryBackoff(c.attempts[room]))
}

// settled ends room's run of tries that reached no verdict, and any retry still waiting for it:
// the try that got here answered for the room.
func (c *roomCatchUps) settled(room string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.attempts, room)
	delete(c.deferred, room)
}
