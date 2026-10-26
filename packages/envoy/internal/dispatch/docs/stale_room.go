package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// documentChangedChannel carries the artifact id of a document whose stored history has just
// changed, to every envoy-dispatch process. Two processes serve one Postgres (INFRA-66) and do
// not share a live document: a room resident in one of them never receives the other's updates,
// so without this a reader sits on text another process replaced until the room leaves at
// roomIdleTimeout. The process that holds a room for that document closes it instead
// (staleRoomEvictor), and the browser reconnects and re-syncs from the durable document.
const documentChangedChannel = "dispatch_doc_changed"

// notifyDocumentChangedStatement names the channel itself, not a parameter, so the statement
// reads as what it is; the artifact id is the parameter, because it is a value.
const notifyDocumentChangedStatement = `select pg_notify('` + documentChangedChannel + `', $1)`

// notifyDocumentChanged announces room's durable change inside tx, the transaction that made it,
// which holds the document's advisory lock (lockDocumentRoom). It is a rebuild's announcement,
// one per rebuild; an append folds the same pg_notify into its own insert rather than spending a
// second round trip on it under that lock (appendUpdateTxClass).
//
// Inside that transaction for two reasons. Postgres delivers a notification only if the
// transaction that sent it commits, so an append the gate refused (ErrRoomBehindHead), or one
// whose commit failed, evicts nobody's room: nothing announces a change the store does not hold.
// And the lock orders the announcement with the history it announces - a listener that hears this
// and reads the head (currentUpdateCursor) reads one at or past the version this transaction
// wrote, never one before it.
//
// It is a statement, not a trigger on doc_updates, and there is no queue table: a trigger or a
// table would need a migration, and Dispatch's pre-deploy census refuses a migration touching a
// table at or above 1 GiB, which doc_updates is. Nothing durable is wanted either. What a process
// does with this - close the room it holds - is worth nothing once that process is gone, so rows
// nobody would ever read back are the whole cost of a table.
func notifyDocumentChanged(ctx context.Context, tx pgx.Tx, room string) error {
	if _, err := tx.Exec(ctx, notifyDocumentChangedStatement, room); err != nil {
		return fmt.Errorf("notify the document's change: %w", err)
	}
	return nil
}

// staleRoomEvictInterval is the least time between two evictions of one room. Every browser
// update is its own append (New sets PersistCoalesceWindow = -1), so one person typing on the
// other process notifies several times a second; evicting on each would close the room under its
// readers again before the first reconnect had finished re-syncing, and a room under steady
// editing elsewhere would never be readable here at all. Two seconds is the service's own idea of
// "the writing has paused" - the settle delay New defaults to - and it is far past the round trip
// an eviction asks of a browser (a close, a reconnect, a sync). What it costs is a room left
// behind by the writes inside the window, and it costs it only until that window ends: the room
// is remembered as the interval refuses it and re-examined then (woke), whether or not anything
// writes the document again - the last notification of a burst is one of the refused ones, and
// nothing else comes back for it. Meanwhile every durable write from that room is refused
// (ErrRoomBehindHead), so what a reader of it sees is stale, never written over the document.
const staleRoomEvictInterval = 2 * time.Second

// staleRoomEvictor closes the rooms this process holds for documents another process has written.
// It holds one LISTEN connection on documentChangedChannel and does its whole decision on that
// connection's goroutine, and that decision is two reads of max(version), not one: an unlocked
// read through the pool, which holds nothing, and - for a room that read finds behind - a second
// read under the document's advisory xact lock (roomBehindDurableHead), which waits for whatever
// writer holds that lock, the other process's append among them. pglisten calls OnNotify inline
// on its one listener goroutine, so one such wait is every other room's notification waiting too.
//
// This comment used to call the whole decision "a map lookup and one indexed read of max(version)",
// and that cheapness is what justifies deciding on the listener goroutine at all. The comment was
// the wrong one of the two: the locked confirmation is real and it stays, because without it a
// write of this process's own, between its commit and its own cursor advance, reads as another
// process's and a healthy room is closed mid-write (documentChanged). What the code had wrong was
// the order - it took that lock before it asked the interval (staleRoomEvictInterval), so a burst
// on the other process paid for one locked confirmation per notification only to be refused
// afterwards. The interval is asked first now, so the notifications that reach the lock are the
// ones an eviction can follow; the eviction itself is a room close, which is rare and is work the
// close must do anyway.
type staleRoomEvictor struct {
	service *Service
	// dial, listenIdle, pingTimeout and backoff are the listener's (pglisten.Listener); dial is
	// the store's own Listener, and a test sets them otherwise before run.
	dial                    func(ctx context.Context) (*pgx.Conn, error)
	listenIdle, pingTimeout time.Duration
	backoff                 func(attempt int) time.Duration
	// now is the clock the interval is measured on, the service's; a test moves its own.
	now func() time.Time
	// listening runs after each LISTEN succeeds and the sweep that follows it (connected), and
	// handled after each notification is decided. Nil outside tests: a test waits on listening
	// before the write whose notification it expects, since a notification sent before the LISTEN
	// reaches nobody, and reads handled to know that notification was decided, and how.
	listening func()
	handled   func(artifactID string, evicted bool)

	mu sync.Mutex
	// evicted is when each room was last evicted, holding only the rooms evicted within the
	// interval: every record drops the entries past it, so this is at most the rooms evicted in
	// the last staleRoomEvictInterval.
	evicted map[string]time.Time
	// deferred is when each room waiting to be re-examined may be: a room the interval refused,
	// at its last eviction plus staleRoomEvictInterval (mayEvict), and a room whose
	// re-examination reached no verdict, one backoff past the attempt that got no answer
	// (armRetry). The wake takes out every room whose window has passed (dueRooms), and only a
	// re-examination that reached a verdict leaves it out, so this holds at most one entry per
	// room resident here.
	deferred map[string]time.Time
	// attempts is how many re-examinations in a row have reached no verdict for each room: what
	// armRetry backs that room's next window off by. The first such attempt makes the entry, and
	// that room's next verdict takes it out (reachedVerdict, recordEviction), so this holds at
	// most one entry per room in deferred.
	attempts map[string]int
}

func newStaleRoomEvictor(service *Service) *staleRoomEvictor {
	return &staleRoomEvictor{
		service:  service,
		dial:     service.store.Pool.Listener,
		now:      service.now,
		evicted:  map[string]time.Time{},
		deferred: map[string]time.Time{},
		attempts: map[string]int{},
	}
}

// RunStaleRoomEviction closes each room this process holds whose document another envoy-dispatch
// process has written, until ctx ends. It is per-process by design: it serves the rooms resident
// in this process, which are this process's own browsers' (cmd/dispatch's loop list says so).
func (s *Service) RunStaleRoomEviction(ctx context.Context) {
	newStaleRoomEvictor(s).run(ctx)
}

// run listens until ctx ends, and covers the listener's own two windows here rather than leaving
// the room behind to its idle timeout: roomIdleTimeout is how long a room stays resident after
// its last peer leaves, so a room with a browser in it never reaches it, and the reader of a room
// left behind would hold text the store replaced for as long as that tab stayed open. The
// notifications sent while the listener had no connection reach nobody and are never sent again,
// so every room this process has loaded is re-examined after each LISTEN succeeds (connected);
// and a notification the interval refused is re-examined when that room's window ends, on this
// same goroutine, through the wait's deadline (wakeIn, woke).
func (e *staleRoomEvictor) run(ctx context.Context) {
	ctx = store.WithTransactionTracking(ctx)
	listener := &pglisten.Listener{
		Owner:       "dispatch",
		Dial:        e.dial,
		Channels:    []string{documentChangedChannel},
		ListenIdle:  e.listenIdle,
		PingTimeout: e.pingTimeout,
		Backoff:     e.backoff,
		OnConnected: e.connected(ctx),
		OnNotify:    e.notified(ctx),
		NextWake:    e.wakeIn,
		OnWake:      e.woke(ctx),
	}
	listener.Run(ctx)
}

// connected is the listener's OnConnected: every room this process has loaded, re-examined after
// each LISTEN stands. It is what covers the notifications sent while the listener had no
// connection - a redial backs off as far as thirty seconds (pglisten.RedialBackoff) - because
// each of those reached nobody, nothing sends them again, and a document nobody writes again
// notifies nobody again. service.roomLoads is every room this process has loaded, and nothing
// here takes an entry out of it: an entry goes when its document is collected and not before
// (keepInstance), so this ranges over every room loaded since the last collection. An entry
// whose room has gone costs the interval's lookup and the server's (mayEvict, srv.GetDoc) and no
// database read, since the head is read only for a room that is still resident.
//
// The test hook runs after the sweep, so a test that waits on it waits for the sweep too.
func (e *staleRoomEvictor) connected(ctx context.Context) func() {
	return func() {
		e.service.roomLoads.Range(func(room, _ any) bool {
			e.documentChanged(ctx, room.(string))
			return true
		})
		if e.listening != nil {
			e.listening()
		}
	}
}

// woke is the listener's OnWake: the rooms waiting to be re-examined, re-examined on the
// listener's own goroutine once their windows have passed. Nothing else would re-examine them -
// every notification inside the window is refused, the last of a burst is one of those, and a
// document nobody writes again notifies nobody again - so without this the refusal is a
// notification dropped.
//
// This is inside the interval's rule, not around it: the rule is the least time between two
// evictions of one room (staleRoomEvictInterval), and a room re-examined here has waited out
// exactly that window. It then goes through the whole decision again - GetDoc, the cursor, the
// open live write, the unlocked read and the locked confirmation - so a room that is no longer
// behind, no longer resident or no longer this process's evicts nothing and simply leaves the set.
// A room whose re-examination reaches no verdict - a read that failed, an eviction that failed, an
// open live write of this process - does not leave it either: dueRooms has spent that room's
// window, and armRetry gives it another, further out for each attempt in a row, so the next wake
// tries it again, and a room that goes on getting no answer is tried once a minute rather than
// every window for the life of the process (staleRoomRetryBackoffMax).
func (e *staleRoomEvictor) woke(ctx context.Context) func() {
	return func() {
		for _, room := range e.dueRooms() {
			e.documentChanged(ctx, room)
		}
	}
}

// notified is the listener's OnNotify: each notification's artifact id, decided on the
// listener's own goroutine.
func (e *staleRoomEvictor) notified(ctx context.Context) func(channel, artifactID string) {
	return func(_, artifactID string) {
		evicted := e.documentChanged(ctx, artifactID)
		if e.handled != nil {
			e.handled(artifactID, evicted)
		}
	}
}

// documentChanged closes this process's room for artifactID when the write that announced the
// change left that room behind the document's durable head, and reports whether it did.
//
// Every append notifies, this process's own included: the statement cannot tell who will hear it.
// The room's cursor is what tells them apart - the durable version the resident instance holds
// every update through (roomCursor). This process's own write advances it before it drops the
// document's advisory lock: a browser's append inside the session lock it appended under
// (appendUpdate), a transaction's write before the commit that drops its lock
// (Ledger.takeRoomCursors), a settlement's own repair likewise (settleRoomWithin). The publish
// of a committed write records its version again as it lands in the room (publishLiveUpdate).
// Another process's write advances no cursor here, so its room stands behind the head.
//
// The read through the pool is the cheap half: it holds nothing, and a room it finds at the head
// is a room nothing is wrong with. A room it finds behind is read again under that lock
// (roomBehindDurableHead), where this process's own writes are either finished - cursor advanced
// - or still holding the lock, so a difference seen there is one no write of this process
// explains. Without the second read the window between a write's commit and its own cursor
// advance reads as another process's change, and the evictor closes a healthy room mid-write.
//
// Equality, not `cursor < head`, for refuseRoomBehindHead's reason: a prune or a deletion lowers
// the head, and a room resident across one stands ahead of a history the store no longer has,
// which is as stale as standing behind it.
//
// A return from here is one of three things, and which of them it is decides whether a deferred
// room's window is spent. The interval's refusal is neither a verdict nor an attempt: it read
// nothing, so it says nothing about the room, and it touches neither reachedVerdict nor armRetry
// - mayEvict has already deferred the room on a window of its own, and a burst of refusals that
// armed the retry instead would push the backoff out for a room nothing is wrong with. A verdict
// - no room here, no cursor, the room at the head, a room behind the head that the close
// (closeRoomBehindHead) closed, or a room that no longer holds the instance this decision read -
// says what was to become of the room, so the window that brought it here has done its work, and
// the run of attempts that got no answer before it, if there was one, ends with it
// (reachedVerdict). A head read that failed, a close that failed, or an open live write of this
// process is an attempt that got no answer, so the room is armed again (armRetry) and the next
// wake tries it: the notification that brought it here is already spent - the last of a burst is
// one the interval refuses - and a dropped attempt leaves a connected reader holding text the
// store replaced, with nothing left to re-examine the room (roomIdleTimeout counts from the last
// peer leaving, and that reader has not left).
func (e *staleRoomEvictor) documentChanged(ctx context.Context, artifactID string) bool {
	// The interval first, ahead of the room lookup, the unlocked head read, and the locked
	// confirmation behind that: a room the interval refuses evicts nothing whatever those reads
	// find, so asking it here gives every notification the same answer - and the same handled
	// note - as asking it last did. What it saves is the waiting. A burst on the other process
	// is several notifications a second for a room that really is behind, so the unlocked read
	// passes each of them through to the confirmation under the document's lock, behind the
	// append that is writing it, only for the interval to refuse them.
	if !e.mayEvict(artifactID) {
		return false
	}
	service := e.service
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		// No room here for that document: the notification was for a room the other process
		// holds, or for a document nobody has open.
		e.reachedVerdict(artifactID)
		return false
	}
	cursor := service.cursorOf(live)
	if cursor == nil {
		// A resident document no room load listed (keepInstance): nothing says what it holds,
		// and a room is not closed on a guess.
		e.reachedVerdict(artifactID)
		return false
	}
	// An open live write of this process is still landing: it moves this room's cursor at its
	// commit and then publishes into the room, and a room closed under it would take its publish
	// with it. The lock the confirmation below takes would wait for a write that holds it, but an
	// open write holds no lock between its operations, and the room it forked is the room this
	// would close.
	//
	// This process's other writers are not asked about here. A settlement or a browser append
	// takes the document's lock, and either advances this instance's cursor before it drops it or
	// is refused on a room behind the head, so the confirmation under that lock already orders the
	// evictor with them (hasOpenLiveWrite). Deferring to them as well phase-locked the evictor
	// with a settlement's retry (dispatch://INFRA-396): a room whose window ended in the
	// millisecond the settlement its reload armed began was deferred one staleRoomEvictInterval,
	// the settlement was refused by the gate and retried on the same two-second cadence, and the
	// two met again, leaving a reader on text the store replaced for a second window, or until the
	// settlement's third refusal failed the room.
	//
	// An attempt, not a verdict: the room is left exactly as stale as it was found, and a live
	// write that appends nothing (no changes, a mutate that failed, an operation rolled back)
	// commits no history and so announces none. Dropped here, that room is left behind the head
	// with a connected reader on it and its window spent. Armed, it costs one more re-examination
	// a window later, by which time the write is finished. The close reads it again under the
	// state lock that reserves it (closeRoomBehindHead), so a write that opens in between is not
	// closed under either.
	if service.hasOpenLiveWrite(artifactID) {
		e.armRetry(artifactID)
		return false
	}
	head, err := currentUpdateCursor(ctx, service.store.Pool, artifactID)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: read the head of a document that changed", "room", artifactID, "error", err)
		}
		e.armRetry(artifactID)
		return false
	}
	if cursor.heldThrough() == head {
		e.reachedVerdict(artifactID)
		return false
	}
	through, head, err := service.roomBehindDurableHead(ctx, artifactID, cursor)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: read the head of a document that changed under its lock", "room", artifactID, "error", err)
		}
		e.armRetry(artifactID)
		return false
	}
	if through == head {
		e.reachedVerdict(artifactID)
		return false
	}
	// The service's own forced reload, of the instance this decision read: it closes the room,
	// flushing what the room still owes the store, and settles the resident record, so the next
	// access loads the durable document. The room's browsers are closed with it and reconnect to
	// the reloaded room. It reserves the room before it closes it, so a write that opens in the
	// window between this decision and the close is refused rather than dropped with the closed
	// room (reserveRoomCloseLocked), and it closes live alone: a room that replaced live (a
	// settlement's own reload, reloadRoomBehindHead, a failed room's eviction, or ygo's idle sweep
	// and a load) is left as it is.
	closed, err := service.closeRoomBehindHead(ctx, artifactID, live)
	if errors.Is(err, errRoomHasOpenLiveWrite) {
		// A live write opened on the room between the check above and the hold that reserves
		// the close: the same attempt with no answer as that check's, so the room is armed again.
		e.armRetry(artifactID)
		return false
	}
	if err != nil {
		slog.Warn("dispatch: close a document room another process's write left behind", "room", artifactID, "error", err)
		e.armRetry(artifactID)
		return false
	}
	if !closed {
		// Another close or another instance answers for the room now, so this window has done
		// its work. The room no longer holds live - something closed it after this decision read
		// it, and whatever instance is there now loaded from the store after that read, so a write
		// that leaves it behind notifies again; closed by name, that instance would be taken from
		// the browser that has just reconnected to it - or the room holds live but is another
		// close's: it failed, and its own eviction reloads it (evictFailed), or a refused
		// settlement has reserved it, and that close either closes it or arms the settlement
		// again on it (reloadRoomBehindHead, closeReservedRoom).
		e.reachedVerdict(artifactID)
		return false
	}
	e.recordEviction(artifactID)
	slog.Info("dispatch: closed a document room another process's write left behind",
		"room", artifactID, "room_through", through, "head", head)
	return true
}

// roomBehindDurableHead reads cursor, room's instance cursor, against the document's durable head
// under the document's advisory lock, and returns the two: equal is a room that holds the whole
// stored history, and anything else is a room an append this process never made left behind.
//
// The lock is what makes the answer exact. Every durable writer of this process takes it and
// advances the instance's cursor before it drops it, so under it no write of this process's own
// sits between its commit and its cursor; and no append can land between the head read here and
// the cursor read beside it. The transaction ends with this read, before the caller closes the
// room: a close flushes what the room still owes the store, whose appends take that same lock.
func (s *Service) roomBehindDurableHead(ctx context.Context, room string, cursor *roomCursor) (int64, int64, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return 0, 0, err
	}
	head, err := currentUpdateCursor(ctx, tx, room)
	if err != nil {
		return 0, 0, err
	}
	return cursor.heldThrough(), head, nil
}

// mayEvict is the interval's check: it reports whether artifactID may be evicted now, and defers
// the room it refuses - the room and the moment its window ends - so that refusal is a wait
// rather than a notification dropped (wakeIn, woke). It is a wait the room keeps: the wake
// spends a room's window only on a re-examination that reached a verdict, and one that got no
// answer arms the room again (documentChanged, armRetry). One map lookup for a room it lets
// through, which is what lets documentChanged ask it before it reads the database at all; a room
// it refuses pays one store beside that, and that room is one no eviction can follow anyway.
func (e *staleRoomEvictor) mayEvict(artifactID string) bool {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	last, evicted := e.evicted[artifactID]
	if !evicted || now.Sub(last) >= staleRoomEvictInterval {
		return true
	}
	e.deferred[artifactID] = last.Add(staleRoomEvictInterval)
	return false
}

// wakeIn is the listener's NextWake: how long until the earliest room waiting to be re-examined
// may be - one the interval refused, or one armed again after an attempt that reached no verdict
// - and zero, never, so the listener waits out its idle period, while none waits. The scan is of
// those rooms, at most one entry per room resident here, on a loop that already spends a round
// trip per round.
func (e *staleRoomEvictor) wakeIn() time.Duration {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	var earliest time.Time
	for _, at := range e.deferred {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	if earliest.IsZero() {
		return 0
	}
	// A window that has already passed - one room's re-examination can outlast another's window -
	// asks for the shortest wait there is, rather than the zero that would read as "never".
	return max(earliest.Sub(now), time.Nanosecond)
}

// dueRooms takes the rooms whose window has passed out of the deferred set, for the wake to
// re-examine. Taking them out here is what drains the set: documentChanged puts a room back only
// through a refusal of its own or an attempt of its own that reached no verdict (mayEvict,
// armRetry), and each of those carries a window of its own.
func (e *staleRoomEvictor) dueRooms() []string {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	var due []string
	for room, at := range e.deferred {
		if !now.Before(at) {
			due = append(due, room)
			delete(e.deferred, room)
		}
	}
	return due
}

// recordEviction stamps artifactID's eviction, and drops every room whose last eviction is past
// the interval as it goes. The pruning is here rather than in mayEvict because this is the only
// method that adds an entry: every call leaves behind only the entries inside the interval, so
// the map holds at most the rooms this process evicted within one staleRoomEvictInterval, and
// once the evictions stop it stops growing with them. It also keeps mayEvict to its one lookup,
// which is the point of asking it first.
//
// Only an eviction that happened is stamped (closeRoomBehindHead closed the room): a close that
// failed left the stale room resident, and stamping it would refuse that room for the rest of
// the interval over an eviction this process never made. Nor is such a room left to its next
// notification, which may never come - the write that announced this one can be the last: the
// failed close arms it for the next window instead (armRetry), so it is a wake away.
func (e *staleRoomEvictor) recordEviction(artifactID string) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for room, at := range e.evicted {
		if now.Sub(at) >= staleRoomEvictInterval {
			delete(e.evicted, room)
		}
	}
	e.evicted[artifactID] = now
	// The eviction is the re-examination a deferred window for this room was waiting for, so that
	// window is spent, and so is any run of attempts that got no answer before it: a write that
	// leaves the reloaded room behind again notifies again, the refusal that would follow defers
	// a window of its own, and the next attempt that gets no answer is armed one
	// staleRoomEvictInterval out, like the first (armRetry).
	delete(e.deferred, artifactID)
	delete(e.attempts, artifactID)
}

// staleRoomRetryBackoffMax is the ceiling of armRetry's backoff, and what it bounds is one
// room's retry rate. A room whose re-examinations keep reaching no verdict is armed again after
// each of them with no notification in between, so without a ceiling climbing away from
// staleRoomEvictInterval such a room would re-read the head through the pool - and, while it is
// still behind, read it again under the document's lock - every two seconds for the life of the
// process. At the ceiling it costs one wake a minute instead, which is as often as a room
// nothing is writing to needs looking at.
//
// Twice pglisten.DefaultListenIdle, rather than a figure of its own, because the listener's idle
// period is the other cadence in play: a lone room re-armed at the ceiling asks for a wake past
// the point where that listener's ping falls due, so the wake costs the connection nothing. The
// ping does not depend on that. The listener keeps it on the time since its connection last
// proved a round trip and cuts a wake short where the ping falls due, whatever ended the wait
// (pglisten.Listener.waitFor, listen) - which is where the invariant has to live, because no
// ceiling here could carry it: wakeIn returns the minimum over every deferred room, and rooms
// armed at their own times hold that minimum under the idle period for ever however far out
// each room's own window is. Three rooms twenty seconds apart, at this ceiling, ask for a wake
// every twenty seconds between them.
//
// Nothing needed this before armRetry, which is worth keeping: the deferred set drained whenever
// the notifications stopped - every window in it was one a notification opened, and a
// re-examination that reached a verdict took it out - so a wake implied traffic had just
// arrived, and traffic is a connection proving itself.
const staleRoomRetryBackoffMax = 2 * pglisten.DefaultListenIdle

// staleRoomRetryBackoff is the wait before the attempt-th re-examination in a row of one room
// whose attempts keep reaching no verdict (from 1): one staleRoomEvictInterval, doubling to
// staleRoomRetryBackoffMax. It is pglisten.RedialBackoff's shape written out rather than that
// function called, because this one starts at the evictor's own interval rather than a second,
// and adds no jitter: the jitter is there to keep the tasks of a rolling deploy from redialing
// one database in step, and this is one process re-reading one of its own rooms, which nothing
// else is in step with.
func staleRoomRetryBackoff(attempt int) time.Duration {
	wait := staleRoomEvictInterval
	for n := 1; n < attempt && wait < staleRoomRetryBackoffMax; n++ {
		wait *= 2
	}
	return min(wait, staleRoomRetryBackoffMax)
}

// armRetry gives artifactID a window to be re-examined in after an attempt that reached no
// verdict - a head read that failed, a close that failed or closed nothing, or an open live write
// of this process (documentChanged). The room is as stale as that attempt found it, the
// notification that carried the attempt is spent, and a document nobody writes again notifies
// nobody again, so the wake this window asks for is the only thing that comes back for it
// (wakeIn, woke).
//
// The first window is one staleRoomEvictInterval from now, rather than the window that has just
// passed: wakeIn asks for the shortest wait there is for a window already behind it, so a room
// re-armed at that moment would have the listener re-run the whole decision as fast as it loops
// for as long as whatever failed keeps failing. On the interval, a room that goes on failing is
// retried as often as a burst's notifications are refused and no oftener. Each attempt in a row
// after the first is further out than the last (staleRoomRetryBackoff), to a ceiling that bounds
// one room's retry rate and nothing besides: a room that keeps getting no answer costs one wake a
// minute instead of one every staleRoomEvictInterval for the life of the process
// (staleRoomRetryBackoffMax). It carries nothing for the listener's ping, which hangs on the time
// since that connection last proved a round trip, cuts any wake short where it falls due and
// fires there whatever ended the wait (pglisten.Listener.waitFor, listen). The count is the
// room's own and ends at that room's next verdict (reachedVerdict), so the ordinary case - one
// read that failed, answered at the next window - is armed one interval out every time.
//
// Through the evictor's one mutex, on the listener's own goroutine, like every other decision
// here: nothing is armed from a timer or a goroutine of its own.
func (e *staleRoomEvictor) armRetry(artifactID string) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempts[artifactID]++
	e.deferred[artifactID] = now.Add(staleRoomRetryBackoff(e.attempts[artifactID]))
}

// reachedVerdict ends artifactID's run of attempts that reached none, so the next one is armed
// one staleRoomEvictInterval out again (armRetry): the backoff answers a room whose
// re-examinations go on getting no answer, and a verdict is an answer - the reads worked, or
// there is no room left to read for.
func (e *staleRoomEvictor) reachedVerdict(artifactID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.attempts, artifactID)
}
