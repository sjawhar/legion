package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// Room sync is how the envoy-dispatch processes on one Postgres learn of each other's document
// work. Postgres is the only channel between them (the message bus can be down while
// the server serves), so each process holds two connections of its own beside its pools:
//
//   - the process's one shared LISTEN connection (a pglisten.Hub that cmd/dispatch builds; the
//     delivery hub and the event stream relay keep listeners of their own), on which the
//     room-sync channels below are registered (Service.RegisterRoomSync). It is named in
//     pg_stat_activity as ListenerApplicationPrefix and the process's id (Service.ProcessID).
//     Holding it is what makes a process a member (roomSyncMembers): a process that dies drops
//     it with its socket, so membership needs no heartbeat and nothing to collect.
//   - one NOTIFY connection (roomSyncSender), which sends what a process announces outside a
//     transaction of its own. It is not a connection of the shared pool, so a request holding one
//     of that pool's transactions sends through it without taking a second (store.ErrNestedAcquire).
//
// A document's durable change is still announced inside the transaction that made it
// (announceDocumentChange, appendUpdateTxClass, Ledger.announceAppends), not through the sender:
// Postgres delivers it only if that transaction commits, which is the guarantee a listener
// reading the stored rows relies on. What a process does with it is catch its own room of the
// document up to the stored head (roomCatchUps), so a room another process's write left behind
// takes that write in and stays open; only a room whose stored history was replaced under it is
// closed and reloaded.

// documentHeadChannel carries each stored change of a document to every envoy-dispatch process,
// as JSON: {"r": artifact id, "v": the head the change left, "p": the sending process's id
// (Service.ProcessID)} (announceDocumentHead). A process drops its own and catches its room of
// that document up when the head is past what the room holds (roomCatchUps.headNotified).
const documentHeadChannel = "dispatch_doc_head"

// documentChangedChannel carries the bare artifact id of a document whose stored history has just
// changed, which is what the builds before documentHeadChannel send and read; every change is
// still announced on it too, beside its documentHeadChannel twin, so those builds keep hearing
// this one's during a roll. A bare notification that is not the twin of this process's own head
// announcement wakes a catch-up (roomCatchUps.changed).
const documentChangedChannel = "dispatch_doc_changed"

// The room-sync channels beside documentChangedChannel. Each has a slot in roomSync.handlers,
// whose handler runs its notifications.
const (
	// documentAwarenessChannel carries a room's awareness (cursors, presence) between processes,
	// and the queries for it; awareness_relay.go has its wire format.
	documentAwarenessChannel = "dispatch_doc_awareness"
	// issueOpenChangedChannel carries the key of an issue that was closed or reopened, sent by the
	// trigger migration 20261101181500 puts on issues.closed_at (Service.issueOpenChanged).
	issueOpenChangedChannel = "dispatch_issue_open_changed"
	// documentQueryChannel carries one process's question about a room to every other member,
	// and documentAnswerChannel each member's answer to it; room_query.go has their wire format.
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

// roomSyncMembership is the from and where of every read of the members: each connection to this
// database that holds a process's shared LISTEN connection. Every Dispatch process connects as one
// database role, so each sees the others' application_name.
const roomSyncMembership = `from pg_stat_activity
	where datname = current_database() and application_name like '` + ListenerApplicationPrefix + `%'`

// roomSyncMembersStatement reads every process that holds its shared LISTEN connection to this
// database (roomSyncMembership).
const roomSyncMembersStatement = `
	select application_name ` + roomSyncMembership + `
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
	return roomSyncMemberIDs(names), nil
}

// roomSyncMemberIDs is the process ids (Service.ProcessID) the members' application names, as
// roomSyncMembership reads them, carry.
func roomSyncMemberIDs(names []string) []string {
	members := make([]string, 0, len(names))
	for _, name := range names {
		members = append(members, strings.TrimPrefix(name, ListenerApplicationPrefix))
	}
	return members
}

// roomSyncHandler is one lane's room-sync channels and what runs their notifications. Every func
// runs on the handler's own goroutine (roomSyncLane), never on the hub's, so a handler that waits
// - on a lock, on the database - delays only its own lane's notifications, and the hub keeps
// reading and pinging the process's listener connection for every subsystem registered on it.
type roomSyncHandler struct {
	// channels are the channels whose notifications run on this lane, interleaved in the order
	// they arrived.
	channels []roomSyncChannel
	// connected runs after each LISTEN stands, ahead of every notification that connection
	// carries; nil for a lane with nothing to catch up.
	connected func(ctx context.Context)
	// nextWake and woke are a clock on the lane's goroutine: nextWake is how long until the
	// handler next wants woke run when no notification comes first, zero for never. Both or
	// neither.
	nextWake func() time.Duration
	woke     func(ctx context.Context)
	// attach, when set, is handed the lane once it exists, for a handler that queues work of its
	// own on it (roomCatchUps.request).
	attach func(lane *roomSyncLane)
}

// roomSyncChannel is one channel and what runs each of its notifications.
type roomSyncChannel struct {
	name     string
	notified func(ctx context.Context, payload string)
}

// roomSync is a process's room-sync handlers, registered on the process's listener hub
// (pglisten.Hub) for every room-sync channel. What the hub calls only queues a notification on
// its handler's lane; the handlers run there (handlers).
type roomSync struct {
	service *Service
	// awareness handles documentAwarenessChannel: the service's awareness relay
	// (awarenessRelay.received). The other handlers are the service's own methods, called where
	// the channel names them (handlers); this one is a field because the awareness tests wrap it
	// to see what the relay has handled.
	awareness func(ctx context.Context, payload string)
	// listening runs after each LISTEN's connected work has run (connected). Nil outside tests: a
	// test waits on it before the write whose notification it expects, since a notification sent
	// before the LISTEN reaches nobody.
	listening func()
}

func newRoomSync(service *Service) *roomSync {
	return &roomSync{service: service, awareness: service.awarenessRelay.received}
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
// place a channel is tied to its handler. documentHeadChannel and documentChangedChannel share
// the catch-up's lane, because the follower tells a write's bare announcement from an old build's
// by the head announcement right before it (roomCatchUps.changed). The issue-open lane re-reads
// the issue of every resident room after each LISTEN (Service.recheckResidentIssues), since a
// close or a reopen notified while the hub had no connection reached nobody.
func (r *roomSync) handlers() []roomSyncHandler {
	catchUps := r.service.catchUps
	return []roomSyncHandler{
		{
			channels: []roomSyncChannel{
				{name: documentHeadChannel, notified: catchUps.headNotified},
				{name: documentChangedChannel, notified: catchUps.changed},
			},
			connected: r.connected,
			nextWake:  catchUps.wakeIn,
			woke:      catchUps.woke,
			attach:    catchUps.attach,
		},
		{channels: []roomSyncChannel{{name: documentAwarenessChannel, notified: r.awareness}}},
		{
			channels:  []roomSyncChannel{{name: issueOpenChangedChannel, notified: r.service.issueOpenChanged}},
			connected: r.service.recheckResidentIssues,
		},
		{channels: []roomSyncChannel{{name: documentQueryChannel, notified: r.service.roomQueries.query}}},
		{channels: []roomSyncChannel{{name: documentAnswerChannel, notified: r.service.roomQueries.answer}}},
	}
}

// connected is what runs after each LISTEN stands, on the catch-up's lane: every notification sent
// while the hub had no connection reached nobody and is never sent again, so each room this
// process has loaded is caught up (roomCatchUps.sweep). The test hook runs after it, so a test
// that waits on the hook waits for the sweep too.
func (r *roomSync) connected(ctx context.Context) {
	r.service.catchUps.sweep(ctx)
	if r.listening != nil {
		r.listening()
	}
}

// register registers every handler's channels on hub and returns what runs the lanes. Each handler
// gets a lane of its own, and what the hub calls - on its own goroutine, which must not block -
// only queues work there.
func (r *roomSync) register(hub *pglisten.Hub) (run func(ctx context.Context)) {
	handlers := r.handlers()
	lanes := make([]*roomSyncLane, 0, len(handlers))
	for _, handler := range handlers {
		lane := newRoomSyncLane(handler)
		lanes = append(lanes, lane)
		for _, channel := range handler.channels {
			notified := channel.notified
			hub.Handle(channel.name, func(payload string) {
				lane.push(func(ctx context.Context) { notified(ctx, payload) })
			})
		}
		if connected := handler.connected; connected != nil {
			hub.OnConnected(func() { lane.push(connected) })
		}
		if handler.attach != nil {
			handler.attach(lane)
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

// roomSyncLane runs one handler's work in arrival order on a goroutine of its own. Its queue has
// no bound, so the hub never waits on a handler: it is as long as the notifications the handler
// has yet to reach. The catch-up's timed wake (roomSyncHandler.nextWake, woke) is this lane's own
// timer too, so no part of the catch-up runs on the hub's goroutine.
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

const (
	// roomSyncPayloadLimit bounds one notification's payload sent through the sender (an
	// awareness batch, a room query's answer), under Postgres's 8000-byte NOTIFY limit with room
	// for the sender's framing.
	roomSyncPayloadLimit = 7_900
	// roomSyncSendTimeout bounds one batch's or one answer's sends, so a sender that cannot reach
	// Postgres holds neither the goroutine sending them, nor the lane answering, nor Shutdown for
	// longer.
	roomSyncSendTimeout = 2 * time.Second
)

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

// announceDocumentHead is the select list that announces a document's stored change to every
// envoy-dispatch process, given the SQL expressions of the document's id (text), the head the
// change leaves (bigint) and the sending process's id (text): a documentHeadChannel notification
// whose payload is {"r": room, "v": head, "p": process}, then the bare id on
// documentChangedChannel, which is what the builds before documentHeadChannel read. Head first and
// bare second, in one select list, so a listener hears the two one after the other (Postgres
// delivers one transaction's notifications together, in the order they were sent): that is how
// room sync tells the bare twin of its own process's announcement from an old build's bare
// announcement (roomCatchUps.changed).
//
// The channels are named in the statement, not passed, so the statement reads as what it is.
func announceDocumentHead(room, head, process string) string {
	return `pg_notify('` + documentHeadChannel + `', json_build_object('r', ` + room + `, 'v', ` + head + `, 'p', ` + process + `)::text), ` +
		`pg_notify('` + documentChangedChannel + `', ` + room + `)`
}

// announceDocumentChangeStatement announces room ($2, as text) at the head its history stands at
// now in the transaction ($1 is the same id as the uuid the rows are keyed by), for the sender $3.
var announceDocumentChangeStatement = `select ` + announceDocumentHead(
	"$2::text", "(select coalesce(max(version), 0) from doc_updates where artifact_id = $1)", "$3::text")

// announceDocumentChange announces room's durable change inside tx, the transaction that made it,
// which holds the document's advisory lock (lockDocumentRoom), with the head that transaction
// leaves and processID, the sending process. It is a rebuild's, a prune's and a deletion's
// announcement, one per transaction; an append folds the same announcement into its own insert
// rather than spending a second round trip on it under that lock (appendUpdateTxClass), and a
// transaction a caller joined announces once at its commit (Ledger.announceAppends). A compaction
// announces nothing: it changes neither the document's state nor its head.
//
// Inside that transaction for two reasons. Postgres delivers a notification only if the
// transaction that sent it commits, so an append the gate refused (ErrRoomBehindHead), or one
// whose commit failed, wakes nobody's catch-up: nothing announces a change the store does not
// hold. And the lock orders the announcement with the history it announces - a listener that
// hears this and reads the stored rows (catchUpRoom) reads a head at or past the version this
// transaction wrote, never one before it.
//
// It is a statement, not a trigger on doc_updates, and there is no queue table: a trigger or a
// table would need a migration, and Dispatch's pre-deploy census refuses a migration touching a
// table at or above 1 GiB, which doc_updates is. Nothing durable is wanted either. What a process
// does with this - catch up the room it holds - is worth nothing once that process is gone, so
// rows nobody would ever read back are the whole cost of a table.
func announceDocumentChange(ctx context.Context, tx pgx.Tx, room, processID string) error {
	if _, err := tx.Exec(ctx, announceDocumentChangeStatement, room, room, processID); err != nil {
		return fmt.Errorf("announce the document's change: %w", err)
	}
	return nil
}
