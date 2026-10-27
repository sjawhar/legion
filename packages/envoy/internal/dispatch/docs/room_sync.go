package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// Room sync is how the envoy-dispatch processes on one Postgres learn of each other's document
// work (INFRA-556). Postgres is the only channel between them (the message bus can be down while
// the server serves), so each process holds two connections of its own beside its pools:
//
//   - the process's one shared LISTEN connection (a pglisten.Hub that cmd/dispatch builds and
//     every listening subsystem registers its channels on), on which the room-sync channels
//     below are registered (Service.RegisterRoomSync). It is named in pg_stat_activity as
//     ListenerApplicationPrefix and the process's id (Service.ProcessID). Holding it is what
//     makes a process a member (roomSyncMembers): a process that dies drops it with its socket,
//     so membership needs no heartbeat and nothing to collect.
//   - one NOTIFY connection (roomSyncSender), which sends what a process announces outside a
//     transaction of its own. It is not a connection of the shared pool, so a request holding one
//     of that pool's transactions sends through it without taking a second (store.ErrNestedAcquire).
//
// A document's durable change is still announced inside the transaction that made it
// (notifyDocumentChanged, appendUpdateTxClass), not through the sender: Postgres delivers it only
// if that transaction commits, which is the guarantee a listener reading the head relies on.

// documentChangedChannel carries the artifact id of a document whose stored history has just
// changed, to every envoy-dispatch process. Its payload is the bare artifact id, which is what
// every build that listens on it reads. A process whose room for that document is now behind the
// stored head closes it (staleRoomEvictor), and the browser reconnects and re-syncs from the
// durable document.
const documentChangedChannel = "dispatch_doc_changed"

// The room-sync channels beside documentChangedChannel. Each has a slot in roomSync.handlers;
// a slot with no handler is still listened on, and drops what arrives on its channel.
const (
	// documentAwarenessChannel carries a room's awareness (cursors, presence) between processes.
	documentAwarenessChannel = "dispatch_doc_awareness"
	// issueOpenChangedChannel carries the key of an issue that was closed or reopened.
	issueOpenChangedChannel = "dispatch_issue_open_changed"
	// documentQueryChannel carries one process's question about a room to every other member,
	// and documentAnswerChannel each member's answer to it.
	documentQueryChannel  = "dispatch_doc_query"
	documentAnswerChannel = "dispatch_doc_answer"
)

// ListenerApplicationPrefix and roomSyncSenderApplicationPrefix begin the application_name of a
// process's shared LISTEN connection (its pglisten.Hub, which cmd/dispatch dials under this
// prefix and Service.ProcessID) and of its NOTIFY sender; each is followed by the process's id.
// The membership query matches the listener's prefix, space included, so the sender
// ("dispatch-sender ...") is never counted as a member. Postgres truncates an application_name
// past 63 bytes; the listener's, a 36-byte uuid included, is 54, and the sender's 52.
const (
	ListenerApplicationPrefix       = "dispatch-listener "
	roomSyncSenderApplicationPrefix = "dispatch-sender "
)

// roomSyncMembersStatement reads every process that holds its shared LISTEN connection to this
// database. Every Dispatch process connects as one database role, so each sees the others'
// application_name.
const roomSyncMembersStatement = `
	select application_name from pg_stat_activity
	where datname = current_database() and application_name like '` + ListenerApplicationPrefix + `%'
	order by application_name`

// roomSyncMembers returns the ids (Service.ProcessID) of the processes whose shared listener
// holds a connection right now, this process's own included while its hub does. It reads
// through q, so a caller inside a transaction passes that transaction and takes no second
// connection of the pool. A process whose hub is redialing is not a member until its next
// LISTEN stands.
func roomSyncMembers(ctx context.Context, q Queryer) ([]string, error) {
	rows, err := q.Query(ctx, roomSyncMembersStatement)
	if err != nil {
		return nil, fmt.Errorf("read the room-sync members: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the room-sync members: %w", err)
	}
	members := make([]string, 0, len(names))
	for _, name := range names {
		members = append(members, strings.TrimPrefix(name, ListenerApplicationPrefix))
	}
	return members, nil
}

// roomSyncHandler is one room-sync channel and what runs its notifications. Every func runs on
// the channel's own goroutine (roomSyncLane), never on the hub's, so a handler that waits - on a
// lock, on the database - delays only its own channel's notifications, and the hub keeps reading
// and pinging the process's listener connection for every subsystem registered on it.
type roomSyncHandler struct {
	channel string
	// notified runs once per notification on channel, in the order they arrived; nil drops them.
	notified func(ctx context.Context, payload string)
	// connected runs after each LISTEN stands, ahead of every notification that connection
	// carries; nil for a channel with nothing to catch up.
	connected func(ctx context.Context)
	// nextWake and woke are a clock on the lane's goroutine: nextWake is how long until the
	// handler next wants woke run when no notification comes first, zero for never. Both or
	// neither.
	nextWake func() time.Duration
	woke     func(ctx context.Context)
}

// roomSync is a process's room-sync handlers, registered on the process's listener hub
// (pglisten.Hub) for every room-sync channel. What the hub calls only queues a notification on
// its channel's lane; the handlers run there (handlers).
type roomSync struct {
	service *Service
	// evictor decides documentChangedChannel's notifications: it closes a room another
	// process's write left behind the stored head.
	evictor *staleRoomEvictor
	// awareness, issueOpenChanged, query and answer handle the four channels beside
	// documentChangedChannel (handlers): documentAwarenessChannel, issueOpenChangedChannel,
	// documentQueryChannel and documentAnswerChannel. Nil drops that channel's notifications, and
	// nothing sets them yet outside tests.
	awareness, issueOpenChanged, query, answer func(ctx context.Context, payload string)
	// listening runs after each LISTEN's connected work has run (connected). Nil outside tests: a
	// test waits on it before the write whose notification it expects, since a notification sent
	// before the LISTEN reaches nobody.
	listening func()
}

func newRoomSync(service *Service) *roomSync {
	return &roomSync{service: service, evictor: newStaleRoomEvictor(service)}
}

// ProcessID is this process's id among the envoy-dispatch processes on its database, minted once
// when the service is built. cmd/dispatch names the process's listener connection with it, after
// ListenerApplicationPrefix, which is what makes the process a member (roomSyncMembers).
func (s *Service) ProcessID() string {
	return s.processID
}

// RegisterRoomSync registers every room-sync channel on hub, the process's one shared listener,
// and returns run, which runs the handlers off it. hub must not be running yet (pglisten.Hub
// takes no registration once Run starts). run does the handlers' work until ctx ends and returns
// once every one of them has stopped; the caller runs it beside hub.Run. Notifications the hub
// hears before run starts wait on their lanes for it. It is per-process by design: it serves the
// rooms resident in this process, and the hub's connection is this process's membership
// (roomSyncMembers).
func (s *Service) RegisterRoomSync(hub *pglisten.Hub) (run func(ctx context.Context)) {
	return newRoomSync(s).register(hub)
}

// handlers is every channel room sync LISTENs on and what runs each one's notifications: the one
// place a channel is tied to its handler.
func (r *roomSync) handlers() []roomSyncHandler {
	return []roomSyncHandler{
		{
			channel:   documentChangedChannel,
			notified:  r.evictor.notified,
			connected: r.connected,
			nextWake:  r.evictor.wakeIn,
			woke:      r.evictor.woke,
		},
		{channel: documentAwarenessChannel, notified: r.awareness},
		{channel: issueOpenChangedChannel, notified: r.issueOpenChanged},
		{channel: documentQueryChannel, notified: r.query},
		{channel: documentAnswerChannel, notified: r.answer},
	}
}

// connected is what runs after each LISTEN stands, on documentChangedChannel's lane: every
// notification sent while the hub had no connection reached nobody and is never sent again, so
// each room this process has loaded is examined again (staleRoomEvictor.sweep). The test hook
// runs after it, so a test that waits on the hook waits for the sweep too.
func (r *roomSync) connected(ctx context.Context) {
	r.evictor.sweep(ctx)
	if r.listening != nil {
		r.listening()
	}
}

// register registers every handler's channel on hub and returns what runs the lanes. Each handler
// with work to do gets a lane of its own, and what the hub calls - on its own goroutine, which
// must not block - only queues work there. A channel whose handler is all nil gets no lane: it is
// still listened on, and its notifications are dropped as they arrive.
func (r *roomSync) register(hub *pglisten.Hub) (run func(ctx context.Context)) {
	handlers := r.handlers()
	lanes := make([]*roomSyncLane, 0, len(handlers))
	for _, handler := range handlers {
		if handler.notified == nil && handler.connected == nil && handler.woke == nil {
			hub.Handle(handler.channel, func(string) {})
			continue
		}
		lane := newRoomSyncLane(handler)
		lanes = append(lanes, lane)
		if notified := handler.notified; notified != nil {
			hub.Handle(handler.channel, func(payload string) {
				lane.push(func(ctx context.Context) { notified(ctx, payload) })
			})
		} else {
			hub.Handle(handler.channel, func(string) {})
		}
		if connected := handler.connected; connected != nil {
			hub.OnConnected(func() { lane.push(connected) })
		}
	}
	return func(ctx context.Context) {
		ctx = store.WithTransactionTracking(ctx)
		var running sync.WaitGroup
		for _, lane := range lanes {
			running.Add(1)
			go func() {
				defer running.Done()
				lane.run(ctx)
			}()
		}
		running.Wait()
	}
}

// roomSyncLane runs one channel's work in arrival order on a goroutine of its own. Its queue has
// no bound, so the hub never waits on a handler: it is as long as the notifications the handler
// has yet to reach. The evictor's timed wake (roomSyncHandler.nextWake, woke) is this lane's own
// timer too, so no part of the evictor runs on the hub's goroutine.
type roomSyncLane struct {
	handler roomSyncHandler
	mu      sync.Mutex
	queue   []func(context.Context)
	// ready holds a token while queue may be non-empty; push never blocks on it.
	ready chan struct{}
}

func newRoomSyncLane(handler roomSyncHandler) *roomSyncLane {
	return &roomSyncLane{handler: handler, ready: make(chan struct{}, 1)}
}

func (l *roomSyncLane) push(work func(context.Context)) {
	l.mu.Lock()
	l.queue = append(l.queue, work)
	l.mu.Unlock()
	select {
	case l.ready <- struct{}{}:
	default:
	}
}

func (l *roomSyncLane) pop() func(context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 {
		return nil
	}
	work := l.queue[0]
	l.queue[0] = nil
	l.queue = l.queue[1:]
	return work
}

// run does the lane's work until ctx ends: everything queued, in order, and the handler's wake
// whenever its clock comes due with nothing queued.
func (l *roomSyncLane) run(ctx context.Context) {
	for ctx.Err() == nil {
		if work := l.pop(); work != nil {
			work(ctx)
			continue
		}
		var timer *time.Timer
		var wake <-chan time.Time
		if l.handler.nextWake != nil && l.handler.woke != nil {
			if wait := l.handler.nextWake(); wait > 0 {
				timer = time.NewTimer(wait)
				wake = timer.C
			}
		}
		select {
		case <-ctx.Done():
		case <-l.ready:
		case <-wake:
			l.handler.woke(ctx)
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// errRoomSyncSenderClosed is a Notify after the document service shut down.
var errRoomSyncSenderClosed = errors.New("the room-sync sender is closed")

// roomSyncSender is the process's NOTIFY connection: a dedicated connection of its own, opened at
// the first send and opened again after one fails. Sends go one at a time, in the order they take
// their turn. It never touches the shared pool, so a request inside one of that pool's
// transactions can send through it, and what it sends is delivered at once rather than at that
// transaction's commit.
type roomSyncSender struct {
	dial func(ctx context.Context) (*pgx.Conn, error)
	// turn holds a token while a send or the close owns conn; waiting for it honours the
	// caller's ctx, which a mutex would not.
	turn   chan struct{}
	conn   *pgx.Conn
	closed bool
}

// newRoomSyncSender builds service's sender, named for its process; the store is reached at the
// first send, not here.
func newRoomSyncSender(service *Service) *roomSyncSender {
	name := roomSyncSenderApplicationPrefix + service.processID
	return &roomSyncSender{
		dial: func(ctx context.Context) (*pgx.Conn, error) {
			return service.store.Pool.Dedicated(ctx, name)
		},
		turn: make(chan struct{}, 1),
	}
}

// Notify sends payload on channel through the sender's connection. A send that fails on a
// connection an earlier send opened is sent once more on a new one: that connection may have
// died while idle, and the error is the first anyone hears of it. A refusal from Postgres itself
// (a payload over its limit) keeps the connection and is not retried.
func (s *roomSyncSender) Notify(ctx context.Context, channel, payload string) error {
	select {
	case s.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.turn }()
	if s.closed {
		return errRoomSyncSenderClosed
	}
	reused := s.conn != nil
	err := s.send(ctx, channel, payload)
	if err != nil && reused && s.conn == nil && ctx.Err() == nil {
		// The connection failed rather than Postgres refusing the send (send keeps the
		// connection for a refusal), so this is the first anyone heard of its death.
		err = s.send(ctx, channel, payload)
	}
	return err
}

// send is one attempt, on the turn's holder. A connection that fails for any reason but a refusal
// of this one statement is closed and forgotten, so the next attempt dials.
func (s *roomSyncSender) send(ctx context.Context, channel, payload string) error {
	if s.conn == nil {
		conn, err := s.dial(ctx)
		if err != nil {
			return fmt.Errorf("open the room-sync sender's connection: %w", err)
		}
		s.conn = conn
	}
	if _, err := s.conn.Exec(ctx, `select pg_notify($1, $2)`, channel, payload); err != nil {
		if !statementRefused(s.conn, err) {
			s.drop()
		}
		return fmt.Errorf("notify %s: %w", channel, err)
	}
	return nil
}

// statementRefused is whether err is Postgres refusing one statement on conn, which stays usable:
// an ERROR the server sent, on a connection still open. A FATAL or PANIC is the server ending the
// session (an administrator's pg_terminate_backend, a restart), and anything else is the socket.
func statementRefused(conn *pgx.Conn, err error) bool {
	var refused *pgconn.PgError
	return errors.As(err, &refused) && refused.Severity == "ERROR" && !conn.IsClosed()
}

// drop closes the connection within pglisten's ping timeout, so a half-open socket's Terminate
// cannot hold the turn.
func (s *roomSyncSender) drop() {
	if s.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pglisten.DefaultPingTimeout)
	defer cancel()
	_ = s.conn.Close(ctx)
	s.conn = nil
}

// close closes the connection and refuses every later send. Service.Shutdown calls it once the
// rooms are drained, the last point the document service sends anything.
func (s *roomSyncSender) close() {
	s.turn <- struct{}{}
	defer func() { <-s.turn }()
	s.closed = true
	s.drop()
}

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
// It decides documentChangedChannel's notifications on that channel's lane (roomSync.handlers),
// one goroutine of its own, and that decision is two reads of max(version), not one: an unlocked
// read through the pool, which holds nothing, and - for a room that read finds behind - a second
// read under the document's advisory xact lock (roomBehindDurableHead), which waits for whatever
// writer holds that lock, the other process's append among them. Every decision runs on that one
// lane - each notification, the sweep after each LISTEN, and each wake - so no two decisions of
// the evictor ever run side by side, and one such wait is every other room's document-changed
// notification waiting too. The hub's own goroutine waits for none of it: it only queues.
//
// The locked confirmation is real and it stays, because without it a write of this process's
// own, between its commit and its own cursor advance, reads as another process's and a healthy
// room is closed mid-write (documentChanged). The interval (staleRoomEvictInterval) is asked
// before it, so a burst on the other process does not pay for one locked confirmation per
// notification only to be refused afterwards: the notifications that reach the lock are the ones
// an eviction can follow, and the eviction itself is a room close, which is rare and is work the
// close must do anyway.
type staleRoomEvictor struct {
	service *Service
	// now is the clock the interval is measured on, the service's; a test moves its own.
	now func() time.Time
	// handled runs after each notification is decided. Nil outside tests: a test reads it to
	// know that notification was decided, and how.
	handled func(artifactID string, evicted bool)

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
		now:      service.now,
		evicted:  map[string]time.Time{},
		deferred: map[string]time.Time{},
		attempts: map[string]int{},
	}
}

// sweep re-examines every room this process has loaded; it runs after each LISTEN stands
// (roomSync.connected). It covers the hub's own window rather than leaving the room behind
// to its idle timeout - roomIdleTimeout is how long a room stays resident after its last peer
// leaves, so a room with a browser in it never reaches it. The notifications sent while the
// hub had no connection - a redial backs off as far as thirty seconds
// (pglisten.RedialBackoff) - each reached nobody, nothing sends them again, and a document nobody
// writes again notifies nobody again. service.roomLoads is every room this process has loaded,
// and nothing here takes an entry out of it: an entry goes when its document is collected and not
// before (keepInstance), so this ranges over every room loaded since the last collection. An
// entry whose room has gone costs the interval's lookup and the server's (mayEvict, srv.GetDoc)
// and no database read, since the head is read only for a room that is still resident.
func (e *staleRoomEvictor) sweep(ctx context.Context) {
	e.service.roomLoads.Range(func(room, _ any) bool {
		e.documentChanged(ctx, room.(string))
		return true
	})
}

// woke is the lane's wake (roomSyncHandler.woke, asked of wakeIn): the rooms waiting to be
// re-examined, re-examined on the lane once their windows have passed. Nothing else would
// re-examine them - every notification inside the window is refused, the last of a burst is one
// of those, and a document nobody writes again notifies nobody again - so without this the
// refusal is a notification dropped.
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
func (e *staleRoomEvictor) woke(ctx context.Context) {
	for _, room := range e.dueRooms() {
		e.documentChanged(ctx, room)
	}
}

// notified is documentChangedChannel's handler: each notification's artifact id, decided on the
// lane.
func (e *staleRoomEvictor) notified(ctx context.Context, artifactID string) {
	evicted := e.documentChanged(ctx, artifactID)
	if e.handled != nil {
		e.handled(artifactID, evicted)
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

// wakeIn is the lane's clock (roomSyncHandler.nextWake): how long until the earliest room waiting
// to be re-examined may be - one the interval refused, or one armed again after an attempt that
// reached no verdict - and zero, never, while none waits. The scan is of those rooms, at most one
// entry per room resident here, once each time the lane runs out of queued notifications.
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
// The wake is a timer on documentChangedChannel's lane (roomSyncLane.run), not on the hub's
// goroutine, so it neither cuts short nor stands in for the hub's ping: that ping keeps to
// the time since the connection last proved a round trip whatever the lane does.
const staleRoomRetryBackoffMax = time.Minute

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
// re-armed at that moment would have the lane re-run the whole decision as fast as it loops for
// as long as whatever failed keeps failing. On the interval, a room that goes on failing is
// retried as often as a burst's notifications are refused and no oftener. Each attempt in a row
// after the first is further out than the last (staleRoomRetryBackoff), to a ceiling that bounds
// one room's retry rate and nothing besides: a room that keeps getting no answer costs one wake a
// minute instead of one every staleRoomEvictInterval for the life of the process
// (staleRoomRetryBackoffMax). The count is the room's own and ends at that room's next verdict
// (reachedVerdict), so the ordinary case - one read that failed, answered at the next window - is
// armed one interval out every time.
//
// Through the evictor's one mutex, on the lane, like every other decision here: nothing is armed
// from a timer or a goroutine of its own.
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
