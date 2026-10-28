package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// documentChangedChannel carries the artifact id of a document whose stored history has just
// changed, to every envoy-dispatch process. Two processes serve one Postgres (INFRA-66) and do
// not share a live document: a room resident in one of them never receives the other's updates,
// so without an announcement a reader sits on text another process replaced until the room leaves
// at roomIdleTimeout. Every write of this build sends documentHeadChannel's notice beside this, in
// the same statement, and that notice is what a process of this build decides its rooms on
// (staleRoomEvictor.headNotified): the process that holds a room the write left behind closes it,
// and the browser reconnects and re-syncs from the durable document. This channel keeps its bare
// payload for the builds before that notice, which listen on it alone, and for what a write of such
// a build has to reach on this one during a roll: a cold read cached before it
// (headTracker.invalidate).
const documentChangedChannel = "dispatch_doc_changed"

// notifyDocumentChangedStatement names the channel itself, not a parameter, so the statement
// reads as what it is; the artifact id is the parameter, because it is a value.
const notifyDocumentChangedStatement = `select pg_notify('` + documentChangedChannel + `', $1)`

// documentHeadChannel carries, beside documentChangedChannel's bare id and in the same statement,
// a documentHeadNotice: which document changed, the head its stored history stands at once the
// sending transaction commits, and which process sent it (Service.ProcessID). A listener can then
// tell a change its room already holds, or one its own process made, from one it has to read.
// documentChangedChannel keeps its bare payload, so a process of an older build, which listens on
// that channel alone, still hears every change while two builds serve one database.
const documentHeadChannel = "dispatch_doc_head"

// notifyDocumentHeadStatement sends both notifications of a write in one statement: the bare id on
// documentChangedChannel ($1) and the documentHeadNotice on documentHeadChannel ($2).
const notifyDocumentHeadStatement = `select pg_notify('` + documentChangedChannel + `', $1), pg_notify('` + documentHeadChannel + `', $2)`

// documentPingChannel carries the stale-room evictor's liveness ping, whose payload is the id of
// the process that sent it (Service.ProcessID) and whose only use is to arrive: every evictor
// listens on it on its LISTEN connection, and a delivery of it, like any other, keeps that
// process's head tracker trusting itself (headTracker.delivered, staleRoomEvictor.pingWhileQuiet).
const documentPingChannel = "dispatch_doc_ping"

// notifyDocumentPingStatement sends one ping on documentPingChannel; the process id is the
// parameter.
const notifyDocumentPingStatement = `select pg_notify('` + documentPingChannel + `', $1)`

// headPingAfter is the least a process's LISTEN may deliver nothing before its evictor pings, and
// the most a ping may take (pingWhileQuiet): each evictor's quiet window is headPingAfter plus a
// jitter drawn uniformly from [0, headPingAfter) afresh after each delivery and each ping
// (drawQuietWindow), so the window runs from one to two seconds and a ping a process sends
// arrives well inside headTrustWindow.
const headPingAfter = time.Second

// documentHeadNotice is documentHeadChannel's payload, encoded as JSON with one-letter keys to keep
// it small inside Postgres's 8000-byte notification limit: {"r":"<artifact uuid>","v":<head after
// commit>,"p":"<process id>"}. Process is empty for a store no Service was built on, which no
// process takes for its own.
type documentHeadNotice struct {
	Room    string `json:"r"`
	Head    int64  `json:"v"`
	Process string `json:"p"`
}

// headNotice is the documentHeadChannel payload of a write of this store that leaves room's stored
// history at head.
func (p *PgVersioned) headNotice(room string, head int64) (string, error) {
	notice, err := json.Marshal(documentHeadNotice{Room: room, Head: head, Process: p.processID})
	if err != nil {
		return "", fmt.Errorf("encode the document's head notification: %w", err)
	}
	return string(notice), nil
}

// notifyDocumentChanged announces room's durable change inside tx, the transaction that made it,
// which holds the document's advisory lock (lockDocumentRoom): documentChangedChannel's bare id
// and documentHeadChannel's notice of head, the head the change leaves the history at, in one
// statement (notifyDocumentHeadStatement). It is the announcement of a rebuild, of a prune's
// checkpoint and of a deletion, one per write; an append folds the same two pg_notify calls into
// its own insert rather than spending a second round trip on them under that lock
// (appendUpdatesStatement). A compaction announces nothing: it changes no content and no head.
//
// Inside that transaction for two reasons. Postgres delivers a notification only if the
// transaction that sent it commits, so an append the gate refused (ErrRoomBehindHead), or one
// whose commit failed, evicts nobody's room: nothing announces a change the store does not hold.
// And the lock orders the announcement with the history it announces - a listener that hears this
// and reads the head (currentUpdateCursor) reads one at or past the version this transaction
// wrote, never one before it, and the notices of one document arrive in the order their writes
// committed, so the head the last of them names is the head the history stands at.
//
// It is a statement, not a trigger on doc_updates, and there is no queue table: a trigger or a
// table would need a migration, and Dispatch's pre-deploy census refuses a migration touching a
// table at or above 1 GiB, which doc_updates is. Nothing durable is wanted either. What a process
// does with this - close the room it holds - is worth nothing once that process is gone, so rows
// nobody would ever read back are the whole cost of a table.
func (p *PgVersioned) notifyDocumentChanged(ctx context.Context, tx pgx.Tx, room string, head int64) error {
	notice, err := p.headNotice(room, head)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, notifyDocumentHeadStatement, room, notice); err != nil {
		return fmt.Errorf("notify the document's change: %w", err)
	}
	return nil
}

// staleRoomEvictInterval is the least time between two evictions of one room. A room's browser
// updates are stored in batches, each lingering at most roomAppendLinger for the updates after its
// own and notifying once, so one person typing on the other process notifies several times a
// second; evicting on each would close the room under its readers again before the first
// reconnect had finished re-syncing, and a room under steady editing elsewhere would never be
// readable here at all. Two seconds is the service's own idea of
// "the writing has paused" - the settle delay New defaults to - and it is far past the round trip
// an eviction asks of a browser (a close, a reconnect, a sync). What it costs is a room left
// behind by the writes inside the window, and it costs it only until that window ends: the room
// is remembered as the interval refuses it and re-examined then (woke), whether or not anything
// writes the document again - the last notification of a burst is one of the refused ones, and
// nothing else comes back for it. Meanwhile every durable write from that room is refused
// (ErrRoomBehindHead), so what a reader of it sees is stale, never written over the document.
const staleRoomEvictInterval = 2 * time.Second

// staleRoomEvictor closes the rooms this process holds for documents another process has written,
// and feeds the service's head tracker (headTracker). It holds one LISTEN connection, on
// documentHeadChannel and documentChangedChannel both, and on documentPingChannel, whose ping it
// sends itself to keep that connection proving it still delivers (pingWhileQuiet). Each notice
// decides what it can on that connection's goroutine, where it costs no query: the tracker's
// record of the head, and a look at the room's cursor (headNotified). A room this process does not
// hold, or one whose cursor stands at the head the notice names, is the whole of nearly every
// notice - every write of this process's own included, which its own commit already moved the
// room's cursor over. What is left is decided off that goroutine, one decision at a time per room
// (decideHead): a write of this process that has not finished its bookkeeping yet is waited for,
// with no query, and anything else is the decision documentChanged makes, which reads max(version)
// twice - an unlocked read through the pool, and for a room that read finds behind a second read
// under the document's advisory xact lock (roomBehindDurableHead), which waits for whatever writer
// holds that lock. pglisten calls OnNotify inline on its one listener goroutine, so a wait there
// would be every other room's notification waiting too; on a room's own goroutine it is that
// room's alone.
//
// The locked confirmation stays, because without it a write of this process's own, between its
// commit and its own cursor advance, reads as another process's and a healthy room is closed
// mid-write (documentChanged). The interval (staleRoomEvictInterval) is asked before either read,
// so a burst on the other process reaches the lock only for the notices an eviction can follow;
// the eviction itself is a room close, which is rare and is work the close must do anyway.
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
	// handled after each documentHeadChannel notice is decided: at once for a notice that needs
	// no query, else when the room's decision ends, once per decision - the notices that arrived
	// while one ran are taken in by the one decision after it. Nil outside tests: a test waits on
	// listening before the write whose notification it expects, since a notification sent before
	// the LISTEN reaches nobody, and reads handled to know that notification was decided, and how.
	listening func()
	handled   func(artifactID string, evicted bool)
	// delivering runs first on each document notification the listener reports - either document
	// channel, not the ping, which nothing decides on - before anything is decided on it;
	// readingHead runs before each read of a document's head the evictor makes for a room it is
	// deciding on, the unlocked read and the confirmation under the document's lock alike. Nil
	// outside tests: a test holds a notification back in delivering, or learns from it that every
	// notification sent before one of its own has been handed on, and counts the database reads
	// the decisions cost in readingHead.
	delivering  func(channel, payload string)
	readingHead func(artifactID string)

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

	// quietWindow is how long, in nanoseconds, the LISTEN may deliver nothing before this
	// evictor pings: headPingAfter plus a jitter drawn afresh after each delivery, each LISTEN
	// and each ping (drawQuietWindow). The listener's goroutine draws it, and so does the ping's,
	// which reads it.
	quietWindow atomic.Int64

	// decidingMu guards deciding: each room with a decision running on its goroutine (decideHead),
	// and the notice a notice that arrived meanwhile left for one more decision after it (nil for
	// none), so a room has at most one decision running and one waiting. decisions counts the
	// goroutines, which run waits for before it returns.
	decidingMu sync.Mutex
	deciding   map[string]*documentHeadNotice
	decisions  sync.WaitGroup
}

func newStaleRoomEvictor(service *Service) *staleRoomEvictor {
	e := &staleRoomEvictor{
		service:  service,
		dial:     service.store.Pool.Listener,
		now:      service.now,
		evicted:  map[string]time.Time{},
		deferred: map[string]time.Time{},
		attempts: map[string]int{},
		deciding: map[string]*documentHeadNotice{},
	}
	e.drawQuietWindow()
	return e
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
//
// The head tracker is told the same two things: it trusts itself from each successful LISTEN
// (connected) and not from the moment the listener dials again, which it does only once the
// connection it had has failed - pglisten reports no loss of its own, and a dial is the first
// thing the listener does after one. A write committed between the loss and that dial notifies
// nobody, and the tracker can trust itself that long; a reconnect starts a new epoch in which
// nothing it heard before counts, and the sweep re-examines every room. A connection that stands
// and delivers nothing is a third case pglisten finds only at its idle ping, so the tracker stops
// trusting itself headTrustWindow after the last delivery, and pingWhileQuiet, on a goroutine of
// its own that ends with ctx, keeps a healthy idle connection delivering while this process holds
// something that trust serves.
//
// That goroutine is a caller of the shared pool beside the listener's and the decisions', so its
// context carries a hold mark of its own (store.WithTransactionTracking), made from the caller's
// context as the listener's is: the ping takes no connection while its caller holds one, and no
// read of the evictor's refuses it.
func (e *staleRoomEvictor) run(ctx context.Context) {
	pingCtx := store.WithTransactionTracking(ctx)
	ctx = store.WithTransactionTracking(ctx)
	listener := &pglisten.Listener{
		Owner: "dispatch",
		Dial: func(ctx context.Context) (*pgx.Conn, error) {
			e.service.heads.disconnected()
			return e.dial(ctx)
		},
		Channels:    []string{documentHeadChannel, documentChangedChannel, documentPingChannel},
		ListenIdle:  e.listenIdle,
		PingTimeout: e.pingTimeout,
		Backoff:     e.backoff,
		OnConnected: e.connected(ctx),
		OnNotify:    e.notified(ctx),
		NextWake:    e.wakeIn,
		OnWake:      e.woke(ctx),
	}
	pinging := make(chan struct{})
	go func() {
		defer close(pinging)
		e.pingWhileQuiet(pingCtx)
	}()
	listener.Run(ctx)
	e.service.heads.disconnected()
	<-pinging
	e.decisions.Wait()
}

// pingWhileQuiet sends a ping on documentPingChannel (notifyDocumentPingStatement), until ctx
// ends, whenever the LISTEN stands, it has delivered nothing for this evictor's quiet window
// (quietWindow), and this process holds something the head tracker's trust serves
// (Service.holdsWhatTheTrackerProtects). The ping arrives on every process's evictor, this one's
// included, so a healthy idle connection goes on delivering and its tracker trusting itself
// (headTracker.delivered), while a half-open one delivers nothing and its tracker stops within
// headTrustWindow. A process that holds nothing the trust serves sends none, and still counts
// what it hears as delivery.
//
// Every delivery draws the window afresh, on every process that hears it, and so does each ping
// its sender, so processes that heard the same ping or notice do not ping together: the first
// holder whose window ends pings, and its ping resets every other window. A deployment therefore
// sends about one ping a second - one per headPingAfter plus the least of its holders' jitters -
// while some process holds a room, a cached read or a preload and no document write is announced,
// and none otherwise. While any process holds something, the longest gap between deliveries is a
// holder's window, under two seconds, plus the ping's own delivery: inside headTrustWindow.
//
// The ping goes through a connection of the shared pool, never the LISTEN connection, which would
// prove only itself, and waits for it at most headPingAfter: a pool too busy to lend a connection
// that soon skips the ping, and if it stays that busy the tracker's trust lapses and cold reads
// and preloads read the store, which is the answer a saturated pool costs and nothing worse.
// After a ping, and while the LISTEN is down or the process holds nothing, it looks again
// headPingAfter later.
func (e *staleRoomEvictor) pingWhileQuiet(ctx context.Context) {
	for {
		wait := headPingAfter
		window := time.Duration(e.quietWindow.Load())
		if quiet, listening := e.service.heads.quiet(); listening && quiet < window {
			wait = window - quiet
		} else if listening && e.service.holdsWhatTheTrackerProtects() {
			e.ping(ctx)
			e.drawQuietWindow()
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// drawQuietWindow draws this evictor's quiet window afresh (quietWindow): headPingAfter plus a
// jitter uniform in [0, headPingAfter), from math/rand/v2's source, which each process seeds on
// its own.
func (e *staleRoomEvictor) drawQuietWindow() {
	e.quietWindow.Store(int64(headPingAfter + rand.N(headPingAfter)))
}

// ping sends one documentPingChannel notification, bounded at headPingAfter. A ping that fails
// is only a ping skipped (pingWhileQuiet), so it is logged at debug level: a saturated pool fails
// one a window.
func (e *staleRoomEvictor) ping(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, headPingAfter)
	defer cancel()
	if _, err := e.service.store.Pool.Exec(pingCtx, notifyDocumentPingStatement, e.service.processID); err != nil && ctx.Err() == nil {
		slog.Debug("dispatch: ping the document listener", "error", err)
	}
}

// holdsWhatTheTrackerProtects is whether this process holds anything the head tracker's trust
// answers without the database: a resident room, a cold read's cached rendering, or a cold
// socket's preload. Tracker entries are not counted: beside those holders the tracker keeps one
// only for a cold read in flight, which ends as a cached rendering or as nothing, and for a write
// of this process, during which nothing about the document is trusted (headTracker). Each is a
// counter or a map's length under its lock, and the preloads a sync.Map whose first entry ends the
// look, so the ping asks it every window without walking the rooms or reading the store.
func (s *Service) holdsWhatTheTrackerProtects() bool {
	if s.residentRooms.Load() > 0 || s.reads.figures().Entries > 0 {
		return true
	}
	held := false
	s.preloads.Range(func(_, _ any) bool {
		held = true
		return false
	})
	return held
}

// connected is the listener's OnConnected: the head tracker's new epoch (headTracker.connected)
// and a fresh quiet window for the ping (drawQuietWindow), the LISTEN being the connection's first
// delivery, then every room this process has loaded, re-examined after each LISTEN stands. It is
// what covers the notifications sent while the listener had no connection - a redial backs off as
// far as thirty seconds (pglisten.RedialBackoff) - because each of those reached nobody, nothing
// sends them again, and a document nobody writes again notifies nobody again. service.roomLoads
// is every room this process has loaded, and nothing here takes an entry out of it: an entry goes
// when its document is collected and not before (keepInstance), so this ranges over every room
// loaded since the last collection. An entry whose room has gone costs the interval's lookup and
// the server's (mayEvict, srv.GetDoc) and no database read, since the head is read only for a
// room that is still resident.
//
// The test hook runs after the sweep, so a test that waits on it waits for the sweep too.
func (e *staleRoomEvictor) connected(ctx context.Context) func() {
	return func() {
		e.service.heads.connected()
		e.drawQuietWindow()
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

// notified is the listener's OnNotify. Every notification, whatever its channel, is first a
// delivery the head tracker counts (headTracker.delivered) and a fresh quiet window for the ping
// (drawQuietWindow), and a documentPingChannel ping is nothing else. A documentHeadChannel notice
// goes to headNotified. A bare documentChangedChannel id only invalidates the head tracker's entry
// for the document (headTracker.invalidate): every write of this build sends the notice beside
// it, which does the room's work, so the bare id alone is an older build's write during a roll,
// which this build's rooms do not follow (their next durable write is refused by the gate, and
// they reload), and which costs no query.
func (e *staleRoomEvictor) notified(ctx context.Context) func(channel, payload string) {
	return func(channel, payload string) {
		e.service.heads.delivered()
		e.drawQuietWindow()
		if channel == documentPingChannel {
			return
		}
		if e.delivering != nil {
			e.delivering(channel, payload)
		}
		switch channel {
		case documentHeadChannel:
			var notice documentHeadNotice
			if err := json.Unmarshal([]byte(payload), &notice); err != nil || notice.Room == "" {
				slog.Warn("dispatch: a document head notification that names no document", "payload", payload, "error", err)
				return
			}
			e.headNotified(ctx, notice)
		case documentChangedChannel:
			e.service.heads.invalidate(payload)
		}
	}
}

// headNotified decides notice on the listener's goroutine as far as it can with no query: the
// head tracker records the head first (headTracker.observe), then a document no room of this
// process holds, or a room that already holds the notice's head (roomHolds), is done. Anything
// else goes to the room's decision goroutine (decideHead), at most one running per room: a notice
// that arrives while one runs is kept for one more decision after it, the latest such notice
// standing for every one before it, so one room's wait never holds the listener or any other room.
func (e *staleRoomEvictor) headNotified(ctx context.Context, notice documentHeadNotice) {
	e.service.heads.observe(notice.Room, notice.Head)
	if e.roomHolds(notice.Room, notice.Head) {
		e.reachedVerdict(notice.Room)
		if e.handled != nil {
			e.handled(notice.Room, false)
		}
		return
	}
	e.decidingMu.Lock()
	if waiting, running := e.deciding[notice.Room]; running {
		if waiting == nil {
			waiting = &documentHeadNotice{}
			e.deciding[notice.Room] = waiting
		}
		*waiting = notice
		e.decidingMu.Unlock()
		return
	}
	e.deciding[notice.Room] = nil
	e.decidingMu.Unlock()
	e.decisions.Add(1)
	go func() {
		defer e.decisions.Done()
		for {
			evicted := e.decideHead(ctx, notice)
			if e.handled != nil && ctx.Err() == nil {
				e.handled(notice.Room, evicted)
			}
			e.decidingMu.Lock()
			waiting := e.deciding[notice.Room]
			if waiting == nil || ctx.Err() != nil {
				delete(e.deciding, notice.Room)
				e.decidingMu.Unlock()
				return
			}
			notice = *waiting
			e.deciding[notice.Room] = nil
			e.decidingMu.Unlock()
		}
	}()
}

// roomHolds is whether a notice that room's history stands at head needs nothing of this process:
// no room of this process holds the document, or the room's cursor lists nothing (keepInstance,
// as documentChanged answers it), or the cursor stands at head, or at the head the tracker holds
// now and trusts (headTracker.head) - a write of this process's own that has finished since the
// notice was sent, whose own notice is still to come and is decided when it does. No query: it
// reads the room server and the room's cursor.
func (e *staleRoomEvictor) roomHolds(room string, head int64) bool {
	live := e.service.srv.GetDoc(room)
	if live == nil {
		return true
	}
	cursor := e.service.cursorOf(live)
	if cursor == nil {
		return true
	}
	through := cursor.heldThrough()
	if through == head {
		return true
	}
	known, trusted := e.service.heads.head(room)
	return trusted && through == known
}

// decideHead is one decision of a room's goroutine (headNotified) on notice, which its room does
// not hold, and reports whether it evicted the room. A notice of this process's own write waits,
// with no query, for this process's writes of the document between their commit and their
// bookkeeping (the tracker's in-flight writes, headTracker.ownWritesDone), and then asks roomHolds
// again: the write that sent it has advanced the room's cursor by then if it was the room's to
// advance. Whatever is still not held - another process's write, or a write of this process that
// left a room behind, such as a settlement's repair into a room loaded meanwhile - is
// documentChanged's decision, with its interval, its locked confirmation and its retries.
func (e *staleRoomEvictor) decideHead(ctx context.Context, notice documentHeadNotice) bool {
	if notice.Process == e.service.processID {
		select {
		case <-e.service.heads.ownWritesDone(notice.Room):
		case <-ctx.Done():
			return false
		}
		if e.roomHolds(notice.Room, notice.Head) {
			e.reachedVerdict(notice.Room)
			return false
		}
	}
	if ctx.Err() != nil {
		return false
	}
	return e.documentChanged(ctx, notice.Room)
}

// documentChanged closes this process's room for artifactID when the write that announced the
// change left that room behind the document's durable head, and reports whether it did.
//
// Every append notifies, this process's own included: the statement cannot tell who will hear it.
// The room's cursor is what tells them apart - the durable version the resident instance holds
// every update through (roomCursor). This process's own write advances it before it drops the
// document's advisory lock: a batch of browser updates inside the session lock it appended under
// (AppendUpdatesWithCredit), a transaction's write before the commit that drops its lock
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
// (closeRoomBehindHead) closed, a room that no longer holds the instance this decision read, or a
// room another close answers for (one that failed, which its own eviction reloads, or one a
// refused settlement reserved for the instance this decision read) - says what was to become of
// the room, so the window that brought it here has done its work, and the run of attempts that
// got no answer before it, if there was one, ends with it (reachedVerdict). A head read that
// failed, a close that failed, or a close something of this process held off (an open live
// write, or a reservation for an instance this room no longer holds) is an attempt that got no
// answer, so the room is armed again (armRetry) and the next wake tries it: the notification that
// brought it here is already spent - the last of a burst is one the interval refuses - and a
// dropped attempt leaves a connected reader holding text the store replaced, with nothing left to
// re-examine the room (roomIdleTimeout counts from the last peer leaving, and that reader has not
// left).
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
	e.countHeadRead(artifactID)
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
	e.countHeadRead(artifactID)
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
	if errors.Is(err, errRoomCloseHeldOff) {
		// Something of this process holds the room open for now: a live write opened on it
		// between the check above and the hold that reserves the close, or a close reserved for
		// the instance live replaced has yet to return. The same attempt with no answer as that
		// check's, so the room is armed again.
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
		// settlement has reserved live itself, and that close either closes it or arms the
		// settlement again on it (reloadRoomBehindHead, closeReservedRoom).
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

// staleRoomDecisionPoll is the longest the listener waits, while a room's decision runs on its
// own goroutine (decideHead), before it asks wakeIn again. The listener computes its wait before
// it waits and pglisten has no way to cut that wait short from outside, so a window a decision
// defers or arms (mayEvict, armRetry) after the wait began would otherwise wait for the next
// notification or the idle ping, up to pglisten.DefaultListenIdle past its end. A wake is a local
// timer and a scan of the deferred rooms, no round trip, and it is asked only while a decision
// runs, which only a write that left a room here behind starts.
const staleRoomDecisionPoll = 100 * time.Millisecond

// wakeIn is the listener's NextWake: how long until the earliest room waiting to be re-examined
// may be - one the interval refused, or one armed again after an attempt that reached no verdict
// - and zero, never, so the listener waits out its idle period, while none waits. While a room's
// decision runs off the listener's goroutine it is at most staleRoomDecisionPoll, so a window that
// decision defers is waited for from when it is made. The scan is of those rooms, at most one
// entry per room resident here, on a loop that already spends a round trip per round.
func (e *staleRoomEvictor) wakeIn() time.Duration {
	e.decidingMu.Lock()
	deciding := len(e.deciding) > 0
	e.decidingMu.Unlock()
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
		if deciding {
			return staleRoomDecisionPoll
		}
		return 0
	}
	// A window that has already passed - one room's re-examination can outlast another's window -
	// asks for the shortest wait there is, rather than the zero that would read as "never".
	wait := max(earliest.Sub(now), time.Nanosecond)
	if deciding {
		return min(wait, staleRoomDecisionPoll)
	}
	return wait
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
// verdict - a head read that failed, a close that failed, or a close something of this process
// held off (documentChanged). The room is as stale as that attempt found it, the
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
// Through the evictor's one mutex, from the decision that got no answer - on the listener's own
// goroutine (connected, woke) or on its room's decision goroutine (decideHead): nothing is armed
// from a timer.
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

// countHeadRead counts one read of artifactID's head the evictor is about to make for a room it
// decides on, the unlocked read or the confirmation under the document's lock, in the room
// figures (Service.evictorQueries), and runs the readingHead hook.
func (e *staleRoomEvictor) countHeadRead(artifactID string) {
	e.service.evictorQueries.Add(1)
	if e.readingHead != nil {
		e.readingHead(artifactID)
	}
}
