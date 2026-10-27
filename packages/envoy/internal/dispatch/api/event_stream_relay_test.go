package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// secondProcessHandler mounts a second API stack on the database the first one is on, as the
// second envoy-dispatch task of a deployment is: its own pool (storetest.SecondProcess), its own
// broker, its own document service and its own event stream relay. The two share Postgres and
// nothing else, which is the whole point -- an in-process fan-out cannot cross between them.
func secondProcessHandler(t *testing.T, database *store.Store) http.Handler {
	t.Helper()
	second := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: second, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown the second process's document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store:       second,
		Identity:    headerIdentity(second),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL:   "https://dispatch.example",
		Docs:        documentService,
		Events:      broker,
		Lifetime:    t.Context(),
	})
	if err != nil {
		t.Fatalf("new API dependencies for the second process: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, deps))
	return mux
}

// TestEventCommittedOnAnotherProcessReachesAnOpenStream is what the live stream is for with two
// envoy-dispatch processes on one Postgres: a reader whose connection landed on process B sees an
// event process A committed, on the connection it already has. Nothing of A's reaches B but the
// row itself -- B's relay hears the commit on `listen dispatch_events` (migration 0107) and reads
// it back out of the log. The broker fan-out this stream used to read would have ended at A.
func TestEventCommittedOnAnotherProcessReachesAnOpenStream(t *testing.T) {
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	second := httptest.NewServer(secondProcessHandler(t, database))
	t.Cleanup(second.Close)
	scanner := openSSEStream(t, second.URL+"/api/v1/events")

	if response := dispatchRequest(t, first, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed on the other process",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename issue on the first process: status=%d body=%s", response.Code, response.Body.String())
	}
	committed := eventHead(t, database)

	for id := int64(0); id != committed; {
		id = sseFrameID(t, readSSEFrame(t, scanner))
		if id > committed {
			t.Fatalf("stream on the second process sent id %d before the event %d committed on the first", id, committed)
		}
	}
}

// relayTestBuffer is the channel size a relay test subscribes with.
const relayTestBuffer = 64

// newCountedRelay is a relay of its own on deps' database, on a lifetime the test ends and waits
// out, whose every page read is reported on reads, after it returns, with the id it read after.
func newCountedRelay(t *testing.T, deps Deps) (*eventStreamRelay, chan int64) {
	t.Helper()
	lifetime, stop := context.WithCancel(t.Context())
	relay := newEventStreamRelay(directServer(deps), lifetime)
	relay.coalesce = time.Millisecond
	reads := make(chan int64, 1024)
	relay.readAfter = func(ctx context.Context, after int64) ([]model.Event, error) {
		page, err := relay.server.readEventsAfterID(ctx, after)
		reads <- after
		return page, err
	}
	t.Cleanup(func() {
		stop()
		relay.wait()
	})
	return relay, reads
}

// subscribeRelay subscribes to the relay, cancelled when the test ends if not before.
func subscribeRelay(t *testing.T, relay *eventStreamRelay) (<-chan relayedEvent, func(), int64) {
	t.Helper()
	subscription, cancel, head, err := relay.subscribe(context.Background(), relayTestBuffer)
	if err != nil {
		t.Fatalf("subscribe to the relay: %v", err)
	}
	t.Cleanup(cancel)
	return subscription, cancel, head
}

// insertRelayEvent commits one event on the issue in one statement, so one notification, and
// answers its id.
func insertRelayEvent(t *testing.T, database *store.Store, issueKey string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		with bumped as (update issues set last_seq = last_seq + 1 where key = $1 returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, last_seq, 'test.relayed', jsonb_build_object('kind', 'session', 'id', 'seed'), '{}'::jsonb, false
		from bumped
		returning id
	`, issueKey).Scan(&id); err != nil {
		t.Fatalf("insert an event: %v", err)
	}
	return id
}

// receiveRelayed is the subscription's next event.
func receiveRelayed(t *testing.T, subscription <-chan relayedEvent) relayedEvent {
	t.Helper()
	select {
	case event, open := <-subscription:
		if !open {
			t.Fatal("the relay's subscription ended")
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("the relay published nothing in 5s")
	}
	return relayedEvent{}
}

// receiveRelayedThrough reads the subscription until the event last, failing on an id out of
// order or at or below after.
func receiveRelayedThrough(t *testing.T, subscription <-chan relayedEvent, after, last int64) {
	t.Helper()
	for previous := after; previous != last; {
		id := receiveRelayed(t, subscription).Event.ID
		if id <= previous {
			t.Fatalf("the relay published %d after %d; it publishes each event once, in id order", id, previous)
		}
		previous = id
	}
}

// waitRelayRead waits for the relay's next page read.
func waitRelayRead(t *testing.T, reads <-chan int64) {
	t.Helper()
	select {
	case <-reads:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay read nothing in 5s")
	}
}

// relayListenBackends counts the database's backends whose last statement is the relay's LISTEN.
// The delivery hub listens on three channels in one statement, so its backend is not counted.
func relayListenBackends(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from pg_stat_activity where datname = current_database() and query = $1
	`, "listen "+dispatchEventsChannel).Scan(&count); err != nil {
		t.Fatalf("count the relay's LISTEN backends: %v", err)
	}
	return count
}

// waitForRelayListenBackends waits for the count of the relay's LISTEN backends to reach want: a
// stopped generation closes its connection from its own goroutine, and the backend leaves
// pg_stat_activity once it has exited.
func waitForRelayListenBackends(t *testing.T, database *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := relayListenBackends(t, database)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d LISTEN %s backends after 5s, want %d", got, dispatchEventsChannel, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEventStreamRelayReadsNothingWithoutASubscriber: a process nobody streams from holds no
// LISTEN connection for the event log and reads none of it, before its first stream and after its
// last one closes. A relay that kept running after its last subscriber left would read the log
// after every commit until the process exited.
func TestEventStreamRelayReadsNothingWithoutASubscriber(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	key := createStreamIssue(t, handler)
	relay, reads := newCountedRelay(t, deps)
	commit := func() {
		t.Helper()
		for range 3 {
			insertRelayEvent(t, database, key)
		}
	}
	idle := func(when string) {
		t.Helper()
		commit()
		if count := len(reads); count != 0 {
			t.Fatalf("the relay read the event log %d times %s", count, when)
		}
		if count := relayListenBackends(t, database); count != 0 {
			t.Fatalf("%d relay LISTEN backends %s, want none", count, when)
		}
	}

	idle("before anyone subscribed")

	subscription, cancel, head := subscribeRelay(t, relay)
	waitRelayRead(t, reads)
	if count := relayListenBackends(t, database); count != 1 {
		t.Fatalf("%d relay LISTEN backends with one subscriber, want 1", count)
	}
	last := insertRelayEvent(t, database, key)
	receiveRelayedThrough(t, subscription, head, last)
	cancel()
	waitForRelayListenBackends(t, database, 0)
	for len(reads) > 0 {
		<-reads
	}

	idle("after its last subscriber left")
}

// TestEventStreamRelayReadsEachCommitOnceForEverySubscriber: one commit costs the process one read
// of the event log and one encoding of its frame, however many streams are open. Every subscriber
// receives the same frame bytes.
func TestEventStreamRelayReadsEachCommitOnceForEverySubscriber(t *testing.T) {
	const subscribers = 3
	handler, database, deps := newTestServer(t, testServerOptions{})
	key := createStreamIssue(t, handler)
	relay, reads := newCountedRelay(t, deps)
	subscriptions := make([]<-chan relayedEvent, subscribers)
	for index := range subscriptions {
		subscriptions[index], _, _ = subscribeRelay(t, relay)
	}
	// The generation's catch-up after its LISTEN, which finds nothing new.
	waitRelayRead(t, reads)

	committed := insertRelayEvent(t, database, key)
	frames := make([][]byte, subscribers)
	for index, subscription := range subscriptions {
		event := receiveRelayed(t, subscription)
		if event.Event.ID != committed {
			t.Fatalf("subscriber %d received event %d, want the committed %d", index, event.Event.ID, committed)
		}
		frames[index] = event.Frame
	}
	if count := len(reads); count != 1 {
		t.Fatalf("the relay read the event log %d times for one commit with %d subscribers, want once", count, subscribers)
	}
	for index := 1; index < subscribers; index++ {
		if &frames[index][0] != &frames[0][0] {
			t.Fatalf("subscriber %d received a frame encoded apart from subscriber 0's", index)
		}
	}
}

// TestEventStreamRelaySubscribeRacingTheLastCancelReceivesTheNextEvent: a subscription made while
// the last one is being cancelled either joins the running generation before it stops or starts a
// new one; either way it receives the next commit.
func TestEventStreamRelaySubscribeRacingTheLastCancelReceivesTheNextEvent(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	key := createStreamIssue(t, handler)
	relay, _ := newCountedRelay(t, deps)
	for range 20 {
		_, cancelFirst, _ := subscribeRelay(t, relay)
		cancelled := make(chan struct{})
		go func() {
			defer close(cancelled)
			cancelFirst()
		}()
		subscription, cancelSecond, head := subscribeRelay(t, relay)
		<-cancelled
		committed := insertRelayEvent(t, database, key)
		receiveRelayedThrough(t, subscription, head, committed)
		cancelSecond()
	}
}

// TestEventStreamRelayCatchesUpWhenItsListenerReconnects covers the window a redial leaves: a
// notification sent while the relay has no connection reaches nobody, and nothing sends it again.
// What covers it is the catch-up the listener's onConnected runs after every LISTEN, from the
// cursor the relay has published through rather than from the log's head -- a cursor reset there
// would silently swallow exactly the events the window dropped.
func TestEventStreamRelayCatchesUpWhenItsListenerReconnects(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	key := createStreamIssue(t, handler)
	relay, _ := newCountedRelay(t, deps)
	var down atomic.Bool
	down.Store(true)
	var dials atomic.Int64
	relay.dial = func(ctx context.Context) (*pgx.Conn, error) {
		if down.Load() {
			return nil, errors.New("the listener has no connection")
		}
		conn, err := database.Pool.Listener(ctx)
		if err == nil {
			dials.Add(1)
		}
		return conn, err
	}
	relay.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	subscription, _, opened := subscribeRelay(t, relay)

	// Committed while the relay has no connection: its notification reaches nobody.
	committed := insertRelayEvent(t, database, key)
	select {
	case event := <-subscription:
		t.Fatalf("the relay published event %d with no listening connection", event.Event.ID)
	case <-time.After(100 * time.Millisecond):
	}
	down.Store(false)
	receiveRelayedThrough(t, subscription, opened, committed)

	// Every later LISTEN catches up the same way, from the cursor as it stands: a redial that
	// reset it would send every open page the whole log again.
	var terminated int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from (
			select pg_terminate_backend(pid) from pg_stat_activity where datname = current_database() and query = $1
		) as terminated
	`, "listen "+dispatchEventsChannel).Scan(&terminated); err != nil || terminated != 1 {
		t.Fatalf("terminate the relay's LISTEN backend: terminated %d (%v), want 1", terminated, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for dials.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the relay did not redial after its connection was terminated")
		}
		time.Sleep(5 * time.Millisecond)
	}
	next := insertRelayEvent(t, database, key)
	receiveRelayedThrough(t, subscription, committed, next)
}

// TestEventStreamRelayRecoversWhenTheEventLogIsReset: the browser suite empties the event log
// between its scenarios (`truncate ... restart identity cascade`, e2e/seed.ts), so the new rows'
// ids restart at 1, below the cursor the relay has published through, and `id > cursor` matches
// nothing. A relay that only ever published forward would fall silent there for good: every live
// update of every later scenario would be lost, with no error anywhere. The relay reads the reset
// the way the delivery hub does (eventCursorCheck) and ends its generation with every subscription
// it holds -- the stream's answer to a reader it can no longer serve in order, which sends a
// browser through its reconnect -- and the next subscription starts a generation on the new log.
func TestEventStreamRelayRecoversWhenTheEventLogIsReset(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	key := createStreamIssue(t, handler)
	relay, _ := newCountedRelay(t, deps)
	subscription, _, opened := subscribeRelay(t, relay)

	// A cursor well above the ids the reset log will hand out.
	committed := insertRelayEvent(t, database, key)
	receiveRelayedThrough(t, subscription, opened, committed)

	if _, err := database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	// TRUNCATE notifies nobody: it is the refill's own insert that fires migration 0107's trigger
	// and wakes the relay onto a log that restarts at id 1.
	refilled := insertRelayEvent(t, database, key)
	if refilled >= committed {
		t.Fatalf("event %d committed after the reset is not below the pre-reset %d: the reset reused no id, so this test proves nothing", refilled, committed)
	}

	// The subscription open across the reset ends without a frame of the new log, whose ids
	// stand for other events than the ones its reader was sent.
	for open := true; open; {
		select {
		case event, more := <-subscription:
			if !more {
				open = false
			} else {
				t.Fatalf("the relay published event %d into a subscription whose log was reset under it", event.Event.ID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the subscription open across the reset was never ended: its reader waits on a stream whose next frame can only lie")
		}
	}
	// The reset ended the generation: its LISTEN connection closes though its subscriber has not
	// cancelled.
	waitForRelayListenBackends(t, database, 0)

	// And the next subscription starts on the new log.
	reopened, _, head := subscribeRelay(t, relay)
	if head != refilled {
		t.Fatalf("a subscription after the reset has head %d, want the new log's %d", head, refilled)
	}
	next := insertRelayEvent(t, database, key)
	receiveRelayedThrough(t, reopened, head, next)
}

// TestLiveAskEventFrameCarriesTheReadTimeFields: an ask payload cannot carry opened_event_id or
// referenced_by_count from the write that appended it, so both are attached at read time
// (attachAskEventFields). Sourcing the live stream through the same row read as the replay
// (readEventRows) and the same encoding (encodeSSEFrame) is what makes the frame a client
// receives live and the frame it receives when it reconnects the same bytes; a hand-rolled live
// read would hand a page an ask without them, and only a reconnect would repair it.
func TestLiveAskEventFrameCarriesTheReadTimeFields(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask fields", "A spec")
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	since := eventHead(t, database)
	live := openSSEStream(t, fmt.Sprintf("%s/api/v1/events?since=%d", httpServer.URL, since))

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which path?",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	liveFrame := readSSEFrameOfType(t, live, "ask.opened")

	// The same event, replayed by a connection resuming from before it.
	replay := openSSEStream(t, fmt.Sprintf("%s/api/v1/events?since=%d", httpServer.URL, since))
	replayedFrame := readSSEFrameOfType(t, replay, "ask.opened")

	if strings.Join(liveFrame, "\n") != strings.Join(replayedFrame, "\n") {
		t.Fatalf("live ask.opened frame\n\t%#v\ndiffers from the replayed one\n\t%#v", liveFrame, replayedFrame)
	}
	var event struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(liveFrame[2], "data: ")), &event); err != nil {
		t.Fatalf("decode the live ask.opened event %q: %v", liveFrame[2], err)
	}
	for _, field := range []string{"opened_event_id", "referenced_by_count"} {
		value, ok := event.Payload[field]
		if !ok || string(value) == "null" {
			t.Fatalf("live ask.opened payload = %s, want a read-time %s", liveFrame[2], field)
		}
	}
}

// readSSEFrameOfType reads frames until one carries the named event type.
func readSSEFrameOfType(t *testing.T, scanner *bufio.Scanner, eventType string) []string {
	t.Helper()
	for range maxSSEReplay {
		frame := readSSEFrame(t, scanner)
		if len(frame) == 3 && frame[1] == "event: "+eventType {
			return frame
		}
	}
	t.Fatalf("no %s frame arrived", eventType)
	return nil
}
