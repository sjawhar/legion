package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

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

// roomSyncChannel is one channel and what runs each of its notifications; nil drops them.
type roomSyncChannel struct {
	name     string
	notified func(ctx context.Context, payload string)
}

// roomSync is a process's room-sync handlers, registered on the process's listener hub
// (pglisten.Hub) for every room-sync channel. What the hub calls only queues a notification on
// its handler's lane; the handlers run there (handlers).
type roomSync struct {
	service *Service
	// awareness, issueOpenChanged, query and answer handle the four channels beside the
	// document-change channels the service's catch-up follows (handlers): documentAwarenessChannel,
	// issueOpenChangedChannel, documentQueryChannel and documentAnswerChannel. Nil drops that
	// channel's notifications, and nothing sets them yet outside tests.
	awareness, issueOpenChanged, query, answer func(ctx context.Context, payload string)
	// listening runs after each LISTEN's connected work has run (connected). Nil outside tests: a
	// test waits on it before the write whose notification it expects, since a notification sent
	// before the LISTEN reaches nobody.
	listening func()
}

func newRoomSync(service *Service) *roomSync {
	return &roomSync{service: service}
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
// by the head announcement right before it (roomCatchUps.changed).
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
		{channels: []roomSyncChannel{{name: issueOpenChangedChannel, notified: r.issueOpenChanged}}},
		{channels: []roomSyncChannel{{name: documentQueryChannel, notified: r.query}}},
		{channels: []roomSyncChannel{{name: documentAnswerChannel, notified: r.answer}}},
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
// with work to do gets a lane of its own, and what the hub calls - on its own goroutine, which
// must not block - only queues work there. A channel with no func is still listened on, and its
// notifications are dropped as they arrive; a handler with no work at all gets no lane.
func (r *roomSync) register(hub *pglisten.Hub) (run func(ctx context.Context)) {
	handlers := r.handlers()
	lanes := make([]*roomSyncLane, 0, len(handlers))
	for _, handler := range handlers {
		working := handler.connected != nil || handler.woke != nil
		for _, channel := range handler.channels {
			working = working || channel.notified != nil
		}
		if !working {
			for _, channel := range handler.channels {
				hub.Handle(channel.name, func(string) {})
			}
			continue
		}
		lane := newRoomSyncLane(handler)
		lanes = append(lanes, lane)
		for _, channel := range handler.channels {
			if notified := channel.notified; notified != nil {
				hub.Handle(channel.name, func(payload string) {
					lane.push(func(ctx context.Context) { notified(ctx, payload) })
				})
			} else {
				hub.Handle(channel.name, func(string) {})
			}
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
	if historyReplaced(through, high, low != 0 && low <= through) {
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
	if pending := live.PendingStats(); pending.Items > 0 || pending.DeleteRanges > 0 {
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
	return historyReplaced(through, high, low != 0 && low <= through), nil
}

// scheduleCatchUp queues one catch-up of live, room's instance that is still loading
// (keepInstance): ygo publishes it once its load hook returns, and the catch-up waits for that
// (roomCatchUps.takeLoading).
func (s *Service) scheduleCatchUp(room string, live *crdt.Doc) {
	s.catchUps.schedule(room, live)
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
//   - a browser append the gate refused because the room's history was replaced
//     (servicePersistenceAdapter.StoreUpdateContext), whose catch-up closes the room;
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
