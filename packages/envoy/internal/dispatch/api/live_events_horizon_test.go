package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// relayStartSettle is the moment the relay's first start is left to read the log once it has begun
// (firstRelayCursor): its floor mark and the log's gaps, a few local statements. An event committed
// after awaitRelayStart therefore commits after that read, and so stands above the cursor the start
// answers, which is what the tests of that cursor turn on.
const relayStartSettle = 300 * time.Millisecond

// awaitRelayStart returns once a subscription has reached relay's first cursor read and that read
// has had relayStartSettle to run. Every caller holds the horizon back (a transaction open
// elsewhere on the instance, or an event id still open in this database), so the start is waiting
// on Horizon.Await by then and cannot finish until the caller lets it.
func awaitRelayStart(t *testing.T, relay *eventStreamRelay) {
	t.Helper()
	pollUntil(t, eventFeedWait, "the subscription reaches the relay's first start", func() bool {
		relay.mu.Lock()
		defer relay.mu.Unlock()
		return relay.starting != nil
	})
	time.Sleep(relayStartSettle)
}

// holdEventWriterTransactionID opens a transaction in Dispatch's own database and takes a
// transaction id, as every write of the event log holds one from before it draws its id
// (events.Broker.Append). While it is open, no mark taken now is older than every transaction id
// held in this database, so the relay's first start reads the log's ids from the boot mark's floor
// (firstRelayFloor), and no horizon is proven either. It rolls back when release runs or when the
// test ends.
func holdEventWriterTransactionID(t *testing.T, database *store.Store) (release func()) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the transaction that holds an event writer's id: %v", err)
	}
	var xid string
	if err := tx.QueryRow(t.Context(), `select pg_current_xact_id()::text`).Scan(&xid); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("take a transaction id in Dispatch's own database: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Rollback(context.Background()) }) }
	t.Cleanup(release)
	return release
}

// liveHorizonHeartbeat is the heartbeat of these tests' servers: a socket waits two heartbeats for
// its page's pong (serveLiveSocket), and a loaded test machine can hold the test client's pong back
// past the 200 ms a 100 ms heartbeat would allow, which ends the socket with no close frame (1006)
// and fails the test for a reason not its own.
const liveHorizonHeartbeat = time.Second

// awaitHeartbeat waits for client's next frame, which must be a heartbeat: the socket is open and
// alive, and nothing of the subscription has arrived, since one still waiting for its relay
// subscription is owed nothing yet, its events_start included.
func awaitHeartbeat(t *testing.T, client *socketClient, within time.Duration, while string) {
	t.Helper()
	data := client.take(t, within, "a heartbeat "+while)
	if _, beat := decodeSocketFrame(t, data).(LiveHeartbeat); !beat {
		t.Fatalf("the socket sent %.200s %s, want nothing but its heartbeat", data, while)
	}
}

// expectNoEventAbove fails the test when sub is sent an event above floor within hold, or its
// socket ends; sub's events at or below floor, and the socket's heartbeats, are let through.
func expectNoEventAbove(t *testing.T, client *socketClient, sub string, floor int64, hold time.Duration, while string) {
	t.Helper()
	deadline := time.After(hold)
	for {
		select {
		case data := <-client.frames:
			switch frame := decodeSocketFrame(t, data).(type) {
			case LiveHeartbeat:
			case LiveEventFrame:
				if id := mustEventFrameID(t, frame); frame.Sub != sub || id > floor {
					t.Fatalf("%s was sent event %d %s, want nothing above %d", sub, id, while, floor)
				}
			default:
				t.Fatalf("%s was sent %+v %s, want nothing above event %d", sub, frame, while, floor)
			}
		case <-client.ended:
			t.Fatalf("the socket ended (%v) %s", client.err, while)
		case <-deadline:
			return
		}
	}
}

// expectEventsInOrder reads sub's events until every id in want has arrived, in want's order, and
// fails on an id at or below one already sent (a repeat, or ids out of order), on an id above the
// next wanted one before it (the wanted one skipped), and on a subscription that ends or falls
// silent for eventFeedWait first. Ids want does not name are let through: a document settling
// beside the test's own writes appends events of its own.
func expectEventsInOrder(t *testing.T, client *socketClient, sub string, want ...int64) {
	t.Helper()
	var last int64
	for next := 0; next < len(want); {
		id := mustEventFrameID(t, client.event(t, eventFeedWait, sub))
		switch {
		case id <= last:
			t.Fatalf("%s was sent event %d after %d: a repeat, or ids out of order", sub, id, last)
		case id == want[next]:
			next++
		case id > want[next]:
			t.Fatalf("%s was sent event %d before event %d, which it skipped", sub, id, want[next])
		}
		last = id
	}
}

// TestAColdSubscriptionIsSentAnIdStillOpenWhenItSubscribed: a cold subscription asks for no
// backlog, but it is owed every event that commits after it began, and a lower id still open when
// it subscribed is one of them. tx A appends on issue X (id a) and stays open, tx B appends on
// issue Y (id b > a) and commits, and the subscription starts. Started at the log's newest id (b),
// the live loop drops a when A commits, for good, since a is at or below that start. Started at the
// relay's published cursor, which never passes an id still open, it is sent a and then b.
func TestAColdSubscriptionIsSentAnIdStillOpenWhenItSubscribed(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	first := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	second := createInteractionIssue(t, rig.handler, "OTHER", "Other issue", "A spec")
	// This subscription starts the relay before A opens: a relay starting while A is open waits for
	// A to end before it answers its first subscriber.
	subscribeEvents(t, rig.dial(t, withBearer("agent-token")), "starter", nil)

	low, high, commitLow := appendOutOfOrder(t, rig.database, rig.served.deps.Events, first.Key, second.Key)
	client := rig.dial(t, withBearer("agent-token"))
	if start := subscribeEvents(t, client, "cold", nil); start >= low {
		t.Fatalf("a cold subscription starts after event %d, at or above the %d still open when it subscribed", start, low)
	}
	// Long enough for the relay to read b's commit while A is still open.
	expectNoEventAbove(t, client, "cold", low-1, time.Second, "while the lower id is still open")

	commitLow()
	expectEventsInOrder(t, client, "cold", low, high)
}

// TestAnEventsSubscriptionStaysAliveWhileTheHorizonIsHeld: the first subscription on a fresh
// process starts the relay, whose first cursor waits for a horizon proven after the call, and so
// for every transaction that holds a transaction id anywhere on the Postgres instance: another
// database's job, another package's test. A subscription's events_start waits with it -- it names
// the point the relay hands over at -- but the page's socket is its own: it stays open and
// heartbeats while that wait lasts, as does one made meanwhile, so no page is dropped for a
// transaction it has nothing to do with. Once the holder ends, each is sent its events_start, whose
// replay is complete for a since of the log as it stands, and the event committed during the hold.
func TestAnEventsSubscriptionStaysAliveWhileTheHorizonIsHeld(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	since := eventHead(t, rig.database)

	release := holdClusterTransactionID(t, 10*time.Second)
	first := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, first, "events", &since)
	awaitHeartbeat(t, first, eventFeedWait, "while the horizon is held")
	second := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, second, "events", &since)
	awaitHeartbeat(t, second, eventFeedWait, "while the first subscription waits on the held horizon")

	renameIssue(t, rig.handler, issue.Key, "Renamed while the horizon is held")
	committed := eventHead(t, rig.database)
	release()

	for name, client := range map[string]*socketClient{"the first": first, "the one made while it waited": second} {
		start := eventsStart(t, client, eventFeedWait, "events")
		if start.Since != strconv.FormatInt(since, 10) || !start.ReplayComplete {
			t.Errorf("%s subscription's events_start = %+v, want a complete replay from %d", name, start, since)
			continue
		}
		expectEventsInOrder(t, client, "events", committed)
	}
}

// TestAnEventsSubscriptionOnARelayStillOnAReplacedLogWaitsForItsReset: after the log is reset
// (e2e/seed.ts truncates it between browser scenarios) the relay keeps the old log's cursor until
// its reset check has run, which waits for a horizon proven after the reset, so a subscription can
// take its relay subscription in between, on a published cursor above the new log's newest id.
// Served from there it would drop the new log's events up to that id, and then the relay's reset
// would end it, sending the page through a resubscribe and the whole refresh a gap owes. So it
// waits for that reset and subscribes again, with its socket open and heartbeating meanwhile: a
// cold subscription starts then, and one resuming from an id of the new log is replayed every event
// after it. Either way the new log's events reach the page on the subscription it already has.
//
// The new log can also have climbed past the old cursor by then, as the browser suite's next
// scenario does while it seeds: the cursor then stands at or below the new log's newest id, on an
// id whose row carries another created_at than the relay read. Started there, the subscription
// would be sent its events_start and then be ended by the relay's reset, which the page answers by
// subscribing again on the same socket. It waits for that reset too.
func TestAnEventsSubscriptionOnARelayStillOnAReplacedLogWaitsForItsReset(t *testing.T) {
	for _, subscription := range []struct {
		name      string
		since     *int64
		climbPast bool
	}{
		{name: "cold", since: nil},
		{name: "resuming from the new log's start", since: new(int64(0))},
		{name: "cold, the new log past the old cursor", since: nil, climbPast: true},
	} {
		t.Run(subscription.name, func(t *testing.T) { replacedLogConnection(t, subscription.since, subscription.climbPast) })
	}
}

func replacedLogConnection(t *testing.T, since *int64, climbPast bool) {
	t.Helper()
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")

	// The first subscription starts the relay; once it has been sent an event, the relay's cursor
	// stands on the old log's newest id and its start-up read is behind it.
	first := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, first, "events", nil)
	renameIssue(t, rig.handler, issue.Key, "Renamed before the reset")
	old := eventHead(t, rig.database)
	expectEventsInOrder(t, first, "events", old)

	// The reset's first event wakes the relay's reset check, which then waits on the held horizon.
	release := holdClusterTransactionID(t, 30*time.Second)
	if _, err := rig.database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	renameIssue(t, rig.handler, issue.Key, "Renamed after the reset")
	afterReset := eventHead(t, rig.database)
	for i := 0; climbPast && afterReset <= old; i++ {
		renameIssue(t, rig.handler, issue.Key, fmt.Sprintf("Renamed past the old cursor %d", i))
		afterReset = eventHead(t, rig.database)
	}
	if !climbPast && afterReset >= old {
		t.Fatalf("the new log's head %d is not below the old log's %d: the relay's cursor would not stand above it, so this test proves nothing", afterReset, old)
	}

	second := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, second, "events", since)
	awaitHeartbeat(t, second, eventFeedWait, "while the relay still stood on the replaced log")
	release()
	if start := eventsStartID(t, second, "events"); start > afterReset {
		t.Fatalf("the subscription starts after event %d, above the new log's newest id %d: it was served from the replaced log's cursor", start, afterReset)
	}
	if since != nil {
		expectEventsInOrder(t, second, "events", afterReset)
	}
	renameIssue(t, rig.handler, issue.Key, "Renamed once the relay has reset")
	expectEventsInOrder(t, second, "events", eventHead(t, rig.database))
}

// TestAColdSubscriptionThatStartsTheRelayIsSentAnEventCommittedWhileItStarts: the first
// subscription of a process starts the relay, whose first cursor is that subscription's start
// point. Here a horizon is already proven (by a since read of the references graph), so the start
// reads the log's gaps only above it, and an event committed before the subscribe but not yet
// proven makes the start wait for every transaction open on the instance. The first cursor is the
// log's newest id as the start read it, so an event that commits during that wait stands above it
// and reaches the subscription; a cursor taken under the horizon proven after the wait would cover
// it instead.
func TestAColdSubscriptionThatStartsTheRelayIsSentAnEventCommittedWhileItStarts(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	// A since read answers a mention only once the horizon is proven past its event, so the
	// mention's answer is the proof this test starts from.
	other := createInteractionIssue(t, rig.handler, "OTHER", "Other issue", "A spec")
	if response := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+other.Key+"/messages", map[string]any{
		"body": "See dispatch://" + issue.Key, "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create the mentioning message: status=%d body=%s", response.Code, response.Body.String())
	}
	awaitGraphEdges(t, rig.handler, url.Values{"to": {"dispatch://" + issue.Key}, "since": {"0"}, "kind": {"mentions"}}, 1)

	release := holdClusterTransactionID(t, 30*time.Second)
	// An event committed before the subscribe but above the proven horizon, which the hold keeps
	// from being proven: a first cursor awaited after it must wait for the hold to end.
	renameIssue(t, rig.handler, issue.Key, "Renamed before the subscribe")

	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	renameIssue(t, rig.handler, issue.Key, "Renamed while the relay starts")
	committed := eventHead(t, rig.database)
	release()

	eventsStartID(t, client, "events")
	expectEventsInOrder(t, client, "events", committed)
}

// TestTheFirstColdSubscriptionOfAFreshProcessIsSentAnEventCommittedWhileTheRelayStarts: the same
// start as the test above, on a process where no reader has proven a horizon yet, which is every
// process at boot. The delivery hub proves none at boot: it runs nothing until its first subscriber
// (newDeliveryHub). A page's first subscription carries no since and runs no refresh of its own, so
// an event that commits after the page's own reads and while the relay's first start waits on a
// transaction open elsewhere on the instance must still reach it, or the page never shows it.
func TestTheFirstColdSubscriptionOfAFreshProcessIsSentAnEventCommittedWhileTheRelayStarts(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")

	release := holdClusterTransactionID(t, 30*time.Second)
	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	renameIssue(t, rig.handler, issue.Key, "Renamed while the relay starts")
	committed := eventHead(t, rig.database)
	release()

	eventsStartID(t, client, "events")
	expectEventsInOrder(t, client, "events", committed)
}

// TestTheFirstColdSubscriptionIsSentAnEventCommittedWhileAWriteFromBeforeBootHoldsTheStart: the
// same start, held this time by a write of Dispatch's own database that took its transaction id
// before the boot mark (a restart under load). The first start waits for a mark that counts, which
// none does until that write ends, and its floor is then the newest such mark, taken after the
// write: an event that committed after the subscription began and before the write ended stands
// at or below that floor, and so below the cursor the subscription starts at, and must still reach
// the subscription.
func TestTheFirstColdSubscriptionIsSentAnEventCommittedWhileAWriteFromBeforeBootHoldsTheStart(t *testing.T) {
	var release func()
	// persistence runs on the test's database before register takes the boot mark.
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, persistence: func(database *store.Store) docs.VersionedStore {
		release = holdEventWriterTransactionID(t, database)
		return nil
	}}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")

	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	renameIssue(t, rig.handler, issue.Key, "Renamed while the relay starts")
	committed := eventHead(t, rig.database)
	release()

	eventsStartID(t, client, "events")
	expectEventsInOrder(t, client, "events", committed)
}

// eventsTuplesRead is how many rows of events every statement so far has read, by a sequential
// scan of the table or through its primary key: the cost of a read of the event log, whichever
// plan Postgres chose for it. A backend keeps its statistics until it has been idle for up to ten
// seconds or exits, so the read first ends every other connection to this database but one that
// holds a transaction id (a test's open writer): each flushes its statistics as it exits, and the
// pools dial again on their next use.
func eventsTuplesRead(t *testing.T, database *store.Store) int64 {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `
		select pg_terminate_backend(pid, 5000) from pg_stat_activity
		where datname = current_database() and backend_type = 'client backend' and pid <> pg_backend_pid()
			and pid not in (select pid from pg_locks where locktype = 'transactionid' and mode = 'ExclusiveLock')
	`); err != nil {
		t.Fatalf("end the other connections so they flush their statistics: %v", err)
	}
	var read int64
	if err := database.Pool.QueryRow(t.Context(), `
		select coalesce((select seq_tup_read from pg_stat_user_tables where relname = 'events'), 0)
			+ coalesce((select idx_tup_read from pg_stat_user_indexes where indexrelname = 'events_pkey'), 0)
	`).Scan(&read); err != nil {
		t.Fatalf("read the events table's statistics: %v", err)
	}
	return read
}

// TestTheRelaysFirstStartDoesNotReadTheWholeEventLog: the relay's first start runs on every process
// at boot (nothing proves a horizon before the first subscription) and holds that subscription's
// events_start, and every live event, until it ends. Its cost must not grow with the log: on
// production's millions of events a read of every id from 0 keeps the first subscription and every
// resuming one's events waiting for seconds after each restart (5,000,000 ids: 3.8 s on a 2-vCPU
// pod, against 0.015 s before this change). The start may read the newest events and the ones still
// open, not the log's whole history: here it may read at most a tenth of a log of 200,000
// committed, published events, measured by the rows of events Postgres counts as read, by a
// sequential scan or through the primary key, while the first cold subscription starts.
func TestTheRelaysFirstStartDoesNotReadTheWholeEventLog(t *testing.T) {
	const logSize = 200_000
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat}, nil)
	createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into events (seq, type, actor, payload, notify, published_at)
		select 0, 'test.filler', '{"kind":"system","id":"test"}', '{}', false, now()
		from generate_series(1, $1)
	`, logSize); err != nil {
		t.Fatalf("fill the event log: %v", err)
	}
	if _, err := rig.database.Pool.Exec(t.Context(), `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	before := eventsTuplesRead(t, rig.database)
	subscribeEvents(t, rig.dial(t, withBearer("agent-token")), "events", nil)
	time.Sleep(1500 * time.Millisecond)
	if read := eventsTuplesRead(t, rig.database) - before; read > logSize/10 {
		t.Fatalf("the relay's first start read %d rows of events on a log of %d events, want at most %d: its cost grows with the whole log", read, logSize, logSize/10)
	}
}

// TestTheRelaysFirstStartDoesNotReadTheWholeEventLogWhileAWriterFromBeforeBootIsOpen: the same
// bound when a write transaction in Dispatch's database took its transaction id before the
// server's boot mark and is still open when the first subscription starts. That is a restart
// under load: write transactions hold their ids for seconds, and pages resubscribe within a
// second of the new process coming up. The start has to wait for that writer anyway, since the
// safe horizon cannot pass it (Horizon.Await), and a mark taken once it has ended bounds the read
// as well as the boot mark would have, so the writer is no reason to read every id from 0. On
// 5,000,000 ids with writers holding their ids for 3 s, that read made the first subscription wait
// 6.0 s after a restart, against 3.1 s when the boot mark held.
func TestTheRelaysFirstStartDoesNotReadTheWholeEventLogWhileAWriterFromBeforeBootIsOpen(t *testing.T) {
	const logSize = 200_000
	var release func()
	// persistence runs on the test's database before register takes the boot mark: the writer's
	// transaction id is older than the mark, as a write open across a restart's boot is.
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat, persistence: func(database *store.Store) docs.VersionedStore {
		release = holdEventWriterTransactionID(t, database)
		return nil
	}}, nil)
	pollUntil(t, eventFeedWait, "the server's boot mark is taken", func() bool {
		select {
		case <-rig.served.bootMark:
			return true
		default:
			return false
		}
	})
	createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into events (seq, type, actor, payload, notify, published_at)
		select 0, 'test.filler', '{"kind":"system","id":"test"}', '{}', false, now()
		from generate_series(1, $1)
	`, logSize); err != nil {
		t.Fatalf("fill the event log: %v", err)
	}
	if _, err := rig.database.Pool.Exec(t.Context(), `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	before := eventsTuplesRead(t, rig.database)
	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	// The writer ends a second into the start, as a write holding its id for seconds does.
	time.Sleep(time.Second)
	release()
	eventsStartID(t, client, "events")
	time.Sleep(1500 * time.Millisecond)
	if read := eventsTuplesRead(t, rig.database) - before; read > logSize/10 {
		t.Fatalf("the relay's first start read %d rows of events on a log of %d events while a writer from before the boot mark was open, want at most %d: it read the whole log", read, logSize, logSize/10)
	}
}

// TestTheFirstColdSubscriptionIsSentAnEventThatDrewItsIdBeforeIt: an event can draw its id before
// the first subscription and commit after it, below an id that had already committed. The relay's
// first start must not start above it: it reads the log's gaps when it starts, and once every id is
// final starts below the lowest one that has filled, so that event reaches the subscription, first.
func TestTheFirstColdSubscriptionIsSentAnEventThatDrewItsIdBeforeIt(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, rig.handler, "OTHER", "Other issue", "A spec")

	low, _, commitLow := appendOutOfOrder(t, rig.database, rig.served.deps.Events, issue.Key, other.Key)
	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	// The start's gap read finds low missing and then waits on it: low's own transaction holds a
	// transaction id in this database, so no horizon is proven while it is open.
	awaitRelayStart(t, rig.served.eventRelay)
	commitLow()

	eventsStartID(t, client, "events")
	if id := mustEventFrameID(t, client.event(t, eventFeedWait, "events")); id != low {
		t.Fatalf("the first cold subscription was sent event %d first, want %d, which committed after it subscribed", id, low)
	}
}

// TestTheFirstColdSubscriptionSkipsAnIdThatRolledBack: the relay's first start reads the gaps among
// the log's ids, since an event that commits after the first subscription can stand in one, and
// starts below the lowest gap that has filled once every id is final. An id whose transaction
// rolled back leaves a gap that never fills, and must move the cursor nowhere: taken for a late
// event, it would send the subscription every event above it, the backlog a cold subscription asked
// not to get.
//
// The gap must be one the start reads, so a write of this database holds a transaction id from
// before the start's own mark, which is what has the start read every id above the boot mark's
// floor rather than only above the log's newest id (firstRelayFloor). With no write open, no event
// id can still be open, the floor stands at the newest id and the start reads no gap at all.
func TestTheFirstColdSubscriptionSkipsAnIdThatRolledBack(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	// An id drawn and never written, as a rolled-back write leaves one, then an event above it.
	if _, err := rig.database.Pool.Exec(t.Context(), `select nextval('events_id_seq')`); err != nil {
		t.Fatalf("draw an event id: %v", err)
	}
	renameIssue(t, rig.handler, issue.Key, "Renamed above the gap")
	backlog := eventHead(t, rig.database)
	release := holdEventWriterTransactionID(t, rig.database)

	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", nil)
	awaitRelayStart(t, rig.served.eventRelay)
	release()
	if start := eventsStartID(t, client, "events"); start != backlog {
		t.Fatalf("the first cold subscription starts after event %d, want the log's newest id %d: the gap that rolled back moved its cursor", start, backlog)
	}

	renameIssue(t, rig.handler, issue.Key, "Renamed after the subscribe")
	live := eventHead(t, rig.database)
	for id := int64(0); id != live; {
		id = nextEventID(t, client, "events")
		if id <= backlog {
			t.Fatalf("the cold subscription was sent event %d, backlog at or below the head %d it started at, above an id that rolled back", id, backlog)
		}
	}
}

// TestShutdownEndsAnEventsSubscriptionWaitingOnTheHorizon: a live socket ends when the server's
// lifetime does (1012, as a restart does), and the shutdown waits for the hub and the relay. A
// socket whose events subscription is still waiting for the first horizon, held back by a
// transaction open elsewhere on the instance, ends then too, instead of holding the shutdown until
// that transaction ends.
func TestShutdownEndsAnEventsSubscriptionWaitingOnTheHorizon(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{streamHeartbeat: liveHorizonHeartbeat}, nil)
	createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	since := eventHead(t, rig.database)

	holdClusterTransactionID(t, 30*time.Second)
	client := rig.dial(t, withBearer("agent-token"))
	sendEventsSubscribe(t, client, "events", &since)
	awaitHeartbeat(t, client, eventFeedWait, "while the horizon is held")

	rig.stop()
	if code, _ := client.closeCode(t, time.Second); code != 1012 {
		t.Fatalf("the socket of a subscription waiting on the horizon closed with %d, want 1012", code)
	}
}

// holdEventInsert opens a transaction that takes its transaction id and inserts one event, as
// events.Broker.Append does, and holds it open: an event insert in flight, holding the lowest id
// not yet final. release commits it; it is rolled back when the test ends otherwise. It answers
// the event's id.
func holdEventInsert(t *testing.T, database *store.Store) (id int64, release func()) {
	t.Helper()
	tx, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the event insert: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `select pg_current_xact_id()`); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("take the event insert's transaction id: %v", err)
	}
	if err := tx.QueryRow(t.Context(), `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'test.open', '{"kind":"system","id":"test"}', '{}', false)
		returning id
	`).Scan(&id); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatalf("insert the open event: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if err := tx.Commit(context.Background()); err != nil {
				t.Errorf("commit the open event: %v", err)
			}
		})
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return id, release
}

// TestTheRelaysFirstStartReadsAboveThePersistedHorizonWhileAnEventInsertFromBeforeBootIsOpen: a
// deploy under load restarts the process while an event insert is open, so no mark the new process
// takes counts until that insert ends, and the first start cannot bound its read of the log by
// one. Every event at or below the horizon the process before it proved is final, whichever process
// proved it, so the start reads only above the horizon that process persisted (event_horizon),
// not the whole log, and is still sent the open event and what committed while it waited, in id
// order. Here the log holds 200,000 events and the start may read at most a tenth of them.
func TestTheRelaysFirstStartReadsAboveThePersistedHorizonWhileAnEventInsertFromBeforeBootIsOpen(t *testing.T) {
	const logSize = 200_000
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	if _, err := database.Pool.Exec(t.Context(), `
		insert into events (seq, type, actor, payload, notify, published_at)
		select 0, 'test.filler', '{"kind":"system","id":"test"}', '{}', false, now()
		from generate_series(1, $1)
	`, logSize); err != nil {
		t.Fatalf("fill the event log: %v", err)
	}
	if _, err := database.Pool.Exec(t.Context(), `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	// The process before proves and persists a horizon past the fill, as its relay or hub does.
	before := events.NewHorizon(database.Pool)
	before.PersistProven()
	newest := eventHead(t, database)
	for deadline := time.Now().Add(eventFeedWait); ; time.Sleep(10 * time.Millisecond) {
		if _, err := before.Safe(t.Context()); err != nil {
			t.Fatalf("observe the horizon: %v", err)
		}
		persisted, err := before.Persisted(t.Context())
		if err != nil {
			t.Fatalf("read the persisted horizon: %v", err)
		}
		if persisted >= newest {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the persisted horizon = %d after %s, want at least %d", persisted, eventFeedWait, newest)
		}
	}

	// An event insert that opened before the next process boots, and outlives its first start.
	open, release := holdEventInsert(t, database)
	read := eventsTuplesRead(t, database)
	server := httptest.NewServer(secondProcessHandler(t, database))
	t.Cleanup(server.Close)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/live", withBearer("agent-token")).read()
	sendEventsSubscribe(t, client, "events", nil)
	time.Sleep(300 * time.Millisecond)
	renameIssue(t, first, issue.Key, "Renamed while the insert is open")
	committed := eventHead(t, database)
	release()

	eventsStartID(t, client, "events")
	expectEventsInOrder(t, client, "events", open, committed)
	server.Close()
	time.Sleep(1500 * time.Millisecond)
	if read = eventsTuplesRead(t, database) - read; read > logSize/10 {
		t.Fatalf("the relay's first start read %d rows of events on a log of %d events while an event insert from before boot was open, want at most %d: it read the whole log, not above the persisted horizon", read, logSize, logSize/10)
	}
}
