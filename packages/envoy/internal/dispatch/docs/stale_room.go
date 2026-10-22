package docs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pglisten"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// behind by the writes inside the window: it is evicted by the first notification after the
// window, or leaves at roomIdleTimeout, and meanwhile every durable write from it is refused
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
	// listening runs after each LISTEN succeeds, and handled after each notification is decided.
	// Nil outside tests: a test waits on listening before the write whose notification it
	// expects, since a notification sent before the LISTEN reaches nobody, and reads handled to
	// know that notification was decided, and how.
	listening func()
	handled   func(artifactID string, evicted bool)

	mu sync.Mutex
	// evicted is when each room was last evicted, holding only the rooms evicted within the
	// interval: every record drops the entries past it, so this is at most the rooms evicted in
	// the last staleRoomEvictInterval.
	evicted map[string]time.Time
}

func newStaleRoomEvictor(service *Service) *staleRoomEvictor {
	return &staleRoomEvictor{
		service: service,
		dial:    service.store.Pool.Listener,
		now:     service.now,
		evicted: map[string]time.Time{},
	}
}

// RunStaleRoomEviction closes each room this process holds whose document another envoy-dispatch
// process has written, until ctx ends. It is per-process by design: it serves the rooms resident
// in this process, which are this process's own browsers' (cmd/dispatch's loop list says so).
func (s *Service) RunStaleRoomEviction(ctx context.Context) {
	newStaleRoomEvictor(s).run(ctx)
}

// run listens until ctx ends. Nothing catches up the notifications sent while it had no
// connection: a room left behind by one of them is refused every durable write (ErrRoomBehindHead,
// which is what keeps those writes correct) and leaves at roomIdleTimeout, so the window costs a
// stale reader and nothing else - the same position Dispatch was in before this listener existed.
func (e *staleRoomEvictor) run(ctx context.Context) {
	ctx = store.WithTransactionTracking(ctx)
	listener := &pglisten.Listener{
		Dial:        e.dial,
		Channels:    []string{documentChangedChannel},
		ListenIdle:  e.listenIdle,
		PingTimeout: e.pingTimeout,
		Backoff:     e.backoff,
		OnConnected: e.listening,
		OnNotify:    e.notified(ctx),
	}
	listener.Run(ctx)
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
		return false
	}
	cursor := service.cursorOf(live)
	if cursor == nil {
		// A resident document no room load listed (keepInstance): nothing says what it holds,
		// and a room is not closed on a guess.
		return false
	}
	// A write of this process's own is still landing: it is about to advance this room's cursor,
	// and a room closed under it would take its publish with it. The lock the confirmation below
	// takes would wait for a write that holds it, but an open write holds no lock between its
	// operations, and the room it forked is the room this would close.
	if service.hasWriteInFlight(artifactID) {
		return false
	}
	head, err := currentUpdateCursor(ctx, service.store.Pool, artifactID)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: read the head of a document that changed", "room", artifactID, "error", err)
		}
		return false
	}
	if cursor.heldThrough() == head {
		return false
	}
	through, head, err := service.roomBehindDurableHead(ctx, artifactID, cursor)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: read the head of a document that changed under its lock", "room", artifactID, "error", err)
		}
		return false
	}
	if through == head {
		return false
	}
	// Evict is the service's own forced reload: it closes the room, flushing what the room still
	// owes the store, and drops the resident state, so the next access loads the durable
	// document. The room's browsers are closed with it and reconnect to the reloaded room.
	if err := service.Evict(ctx, artifactID); err != nil {
		slog.Warn("dispatch: close a document room another process's write left behind", "room", artifactID, "error", err)
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

// mayEvict is the interval's check: it reports whether artifactID may be evicted now, and records
// nothing. One map lookup, which is what lets documentChanged ask it before it reads the database
// at all.
func (e *staleRoomEvictor) mayEvict(artifactID string) bool {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	last, evicted := e.evicted[artifactID]
	return !evicted || now.Sub(last) >= staleRoomEvictInterval
}

// recordEviction stamps artifactID's eviction, and drops every room whose last eviction is past
// the interval as it goes. The pruning is here rather than in mayEvict because this is the only
// method that adds an entry: every call leaves behind only the entries inside the interval, so
// the map holds at most the rooms this process evicted within one staleRoomEvictInterval, and
// once the evictions stop it stops growing with them. It also keeps mayEvict to its one lookup,
// which is the point of asking it first.
//
// Only an eviction that happened is stamped (service.Evict returned no error): a close that
// failed left the stale room resident, and the room is retried on its next notification rather
// than refused for the rest of the interval over an eviction this process never made.
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
}
