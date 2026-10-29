package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
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
	})
	if err != nil {
		t.Fatalf("new API dependencies for the second process: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)
	return mux
}

// TestEventCommittedOnAnotherProcessReachesAnOpenEventFeed is what the live feed is for with two
// envoy-dispatch processes on one Postgres: a page whose socket landed on process B sees an event
// process A committed, on the subscription it already has. Nothing of A's reaches B but the row
// itself -- B's relay hears the commit on `listen dispatch_events` (migration 0107) and reads it
// back out of the log. The broker fan-out the feed used to read would have ended at A.
func TestEventCommittedOnAnotherProcessReachesAnOpenEventFeed(t *testing.T) {
	first, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, first, "TEST", "Issue", "A spec")
	second := httptest.NewServer(secondProcessHandler(t, database))
	t.Cleanup(second.Close)
	client := openLiveSocketAt(t, gws.DefaultDialer, "ws"+strings.TrimPrefix(second.URL, "http")+"/ws/live", withBearer("agent-token")).read()
	subscribeEvents(t, client, "events", nil)

	renameIssue(t, first, issue.Key, "Renamed on the other process")
	committed := eventHead(t, database)

	for id := int64(0); id != committed; {
		id = nextEventID(t, client, "events")
		if id > committed {
			t.Fatalf("the feed on the second process sent id %d before the event %d committed on the first", id, committed)
		}
	}
}

// newCountedRelay is a relay of its own on deps' database, on a lifetime the test ends and waits
// out, whose every page read is reported on reads, after it returns, with the id it read after.
func newCountedRelay(t *testing.T, deps Deps) (*eventStreamRelay, chan int64) {
	t.Helper()
	lifetime, stop := context.WithCancel(t.Context())
	relay := newEventStreamRelay(directServer(deps), lifetime)
	relay.coalesce = time.Millisecond
	reads := make(chan int64, 1024)
	relay.readAfter = func(ctx context.Context, after int64) ([]model.Event, error) {
		page, err := relay.server.readEventsAfterID(ctx, after, eventRelayPage)
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
	subscription, err := relay.subscribe(context.Background(), eventStreamBuffer)
	if err != nil {
		t.Fatalf("subscribe to the relay: %v", err)
	}
	t.Cleanup(subscription.cancel)
	return subscription.events, subscription.cancel, subscription.head
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

// relayListenBackend selects, in pg_stat_activity, the backends whose last statement is the
// relay's LISTEN. The delivery hub listens on its two channels in one statement, so its backend
// is not selected.
const relayListenBackend = `datname = current_database() and query = 'listen ` + dispatchEventsChannel + `'`

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
			seedStreamEvents(t, database, key, 1, 0)
		}
	}
	idle := func(when string) {
		t.Helper()
		commit()
		if count := len(reads); count != 0 {
			t.Fatalf("the relay read the event log %d times %s", count, when)
		}
		if count := countBackends(t, database, relayListenBackend); count != 0 {
			t.Fatalf("%d relay LISTEN backends %s, want none", count, when)
		}
	}

	idle("before anyone subscribed")

	subscription, cancel, head := subscribeRelay(t, relay)
	waitRelayRead(t, reads)
	if count := countBackends(t, database, relayListenBackend); count != 1 {
		t.Fatalf("%d relay LISTEN backends with one subscriber, want 1", count)
	}
	last := seedStreamEvents(t, database, key, 1, 0)
	receiveRelayedThrough(t, subscription, head, last)
	cancel()
	waitForBackends(t, database, 0, relayListenBackend)
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

	committed := seedStreamEvents(t, database, key, 1, 0)
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
		committed := seedStreamEvents(t, database, key, 1, 0)
		receiveRelayedThrough(t, subscription, head, committed)
		cancelSecond()
	}
}

// TestEventStreamRelayCatchesUpWhenItsListenerReconnects covers the window a redial leaves: a
// notification sent while the relay has no connection reaches nobody, and nothing sends it again.
// What covers it is the catch-up the listener's onConnected runs after every LISTEN, from the
// cursor the relay has published through rather than from the log's head -- a cursor reset there
// would silently swallow exactly the events the window dropped from every open page's feed.
func TestEventStreamRelayCatchesUpWhenItsListenerReconnects(t *testing.T) {
<<<<<<< conflict 1 of 13
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
 	handler, database, deps := newTestServer(t, testServerOptions{})
-	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
-
-	lifetime, stop := context.WithCancel(t.Context())
-	relay := newEventStreamRelay(directServer(deps), lifetime)
+	key := createStreamIssue(t, handler)
+	relay, _ := newCountedRelay(t, deps)
+++++++ vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	// An issue with no spec, whose document settles nothing, so nothing commits but the test's own.
	issue := createPlainIssue(t, rig.handler, "TEST")
	relay := rig.served.eventRelay
>>>>>>> conflict 1 of 13 ends
	var down atomic.Bool
	down.Store(true)
	var dials atomic.Int64
	relay.dial = func(ctx context.Context) (*pgx.Conn, error) {
		if down.Load() {
			return nil, errors.New("the listener has no connection")
		}
<<<<<<< conflict 2 of 13
+++++++ zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
		conn, err := database.Pool.Listener(ctx)
		if err == nil {
			dials.Add(1)
		}
		return conn, err
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
-		return database.Pool.Listener(ctx)
+		return rig.database.Pool.Listener(ctx)
>>>>>>> conflict 2 of 13 ends
	}
	relay.backoff = func(int) time.Duration { return 5 * time.Millisecond }
<<<<<<< conflict 3 of 13
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
-	relay.coalesce = time.Millisecond
-	t.Cleanup(func() {
-		stop()
-		relay.wait()
-	})
-	subscription, cancel, err := relay.subscribe(context.Background())
-	if err != nil {
-		t.Fatalf("subscribe to the relay: %v", err)
-	}
-	defer cancel()
-	opened, _ := relay.at()
+	subscription, _, opened := subscribeRelay(t, relay)
+++++++ vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
	relay.coalesce = time.Millisecond

	// The subscription starts the relay, with no listening connection. It resumes from before the
	// newest event, so once that event reaches the page its catch-up has read its last page: what
	// reaches the page after it can only come from the relay.
	client := rig.dial(t, withBearer("agent-token"))
	before := eventHead(t, rig.database)
	subscribeEvents(t, client, "events", new(before-1))
	if id := nextEventID(t, client, "events"); id != before {
		t.Fatalf("the catch-up sent event %d, want the head %d", id, before)
	}
	opened, _ := relay.at()
>>>>>>> conflict 3 of 13 ends

	// Committed while the relay has no connection: its notification reaches nobody.
<<<<<<< conflict 4 of 13
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
-	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
-		"title": "Renamed while the listener was down",
-	}, "alice"); response.Code != http.StatusOK {
-		t.Fatalf("rename issue: status=%d body=%s", response.Code, response.Body.String())
-	}
-	committed := eventHead(t, database)
+	committed := seedStreamEvents(t, database, key, 1, 0)
 	select {
 	case event := <-subscription:
-		t.Fatalf("the relay published event %d with no listening connection", event.ID)
+		t.Fatalf("the relay published event %d with no listening connection", event.Event.ID)
 	case <-time.After(100 * time.Millisecond):
 	}
-	if at, _ := relay.at(); at != opened {
-		t.Fatalf("relay cursor = %d with no connection, want the %d it opened on", at, opened)
-	}
-
+++++++ vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
	renameIssue(t, rig.handler, issue, "Renamed while the listener was down")
	committed := eventHead(t, rig.database)
	client.quiet(t, 100*time.Millisecond, "the relay with no listening connection")
	if at, _ := relay.at(); at != opened {
		t.Fatalf("relay cursor = %d with no connection, want the %d it opened on", at, opened)
	}

>>>>>>> conflict 4 of 13 ends
	down.Store(false)
<<<<<<< conflict 5 of 13
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
-	var last int64
-	for last < committed {
-		select {
-		case event, open := <-subscription:
-			if !open {
-				t.Fatal("the relay's subscription ended")
-			}
-			if event.ID <= last {
-				t.Fatalf("the relay published %d after %d; a catch-up publishes in id order", event.ID, last)
-			}
-			last = event.ID
-		case <-time.After(5 * time.Second):
-			t.Fatalf("the relay published through %d after its listener reconnected, want the %d committed while it was down", last, committed)
-		}
-	}
+	receiveRelayedThrough(t, subscription, opened, committed)
+++++++ vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
	for last := before; last < committed; {
		id := nextEventID(t, client, "events")
		if id <= last {
			t.Fatalf("the feed sent %d after %d; a catch-up publishes in id order", id, last)
		}
		last = id
	}
>>>>>>> conflict 5 of 13 ends

	// Every later LISTEN catches up the same way, from the cursor as it stands: a redial that
	// reset it would send every open page the whole log again.
<<<<<<< conflict 6 of 13
+++++++ zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
	var terminated int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from (
			select pg_terminate_backend(pid) from pg_stat_activity where `+relayListenBackend+`
		) as terminated
	`).Scan(&terminated); err != nil || terminated != 1 {
		t.Fatalf("terminate the relay's LISTEN backend: terminated %d (%v), want 1", terminated, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for dials.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the relay did not redial after its connection was terminated")
		}
		time.Sleep(5 * time.Millisecond)
	}
	next := seedStreamEvents(t, database, key, 1, 0)
	receiveRelayedThrough(t, subscription, committed, next)
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
 	published, _ := relay.at()
 	relay.onConnected()
-	select {
-	case event := <-subscription:
-		if event.ID <= published {
-			t.Fatalf("a redial republished event %d, at or below the %d the relay had published through", event.ID, published)
-		}
-	case <-time.After(200 * time.Millisecond):
-	}
+	client.quiet(t, 200*time.Millisecond, "a redial with nothing committed since")
 	if at, _ := relay.at(); at < published {
 		t.Fatalf("relay cursor = %d after a redial, want at least the %d it had published through", at, published)
 	}
>>>>>>> conflict 6 of 13 ends
}

// TestEventStreamRelayRecoversWhenTheEventLogIsReset: the browser suite empties the event log
// between its scenarios (`truncate ... restart identity cascade`, e2e/seed.ts), so the new rows'
// ids restart at 1, below the cursor the relay has published through, and `id > cursor` matches
// nothing. A relay that only ever published forward would fall silent there for good: every live
// update of every later scenario would be lost, with no error anywhere. The relay reads the reset
<<<<<<< conflict 7 of 13
+++++++ zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
// through eventCursorCheck and ends its generation with every subscription
// it holds -- the stream's answer to a reader it can no longer serve in order, which sends a
// browser through its reconnect -- and the next subscription starts a generation on the new log.
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
 // the way the delivery hub does (eventCursorCheck), moves its cursor to the new log's head and
-// ends every open subscription -- the stream's answer to a reader it can no longer serve in
-// order, which sends a browser through its reconnect -- and serves the events committed after the
-// reset to the connections opened after it.
+// drops every subscriber -- the stream's answer to a reader it can no longer serve in order --
+// which ends every open events subscription with an ended frame (behind), so the page subscribes
+// again; and it serves the events committed after the reset to the subscriptions made after it.
>>>>>>> conflict 7 of 13 ends
func TestEventStreamRelayRecoversWhenTheEventLogIsReset(t *testing.T) {
<<<<<<< conflict 8 of 13
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
 	handler, database, deps := newTestServer(t, testServerOptions{})
-	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
-	rename := func(title string) {
-		t.Helper()
-		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
-			"title": title,
-		}, "alice"); response.Code != http.StatusOK {
-			t.Fatalf("rename issue to %q: status=%d body=%s", title, response.Code, response.Body.String())
-		}
-	}
-
-	lifetime, stop := context.WithCancel(t.Context())
-	relay := newEventStreamRelay(directServer(deps), lifetime)
-	relay.coalesce = time.Millisecond
-	t.Cleanup(func() {
-		stop()
-		relay.wait()
-	})
-	subscription, cancel, err := relay.subscribe(context.Background())
-	if err != nil {
-		t.Fatalf("subscribe to the relay: %v", err)
-	}
-	defer cancel()
+	key := createStreamIssue(t, handler)
+	relay, _ := newCountedRelay(t, deps)
+	subscription, _, opened := subscribeRelay(t, relay)
+++++++ vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	rig.served.eventRelay.coalesce = time.Millisecond
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	client := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, client, "events", nil)
>>>>>>> conflict 8 of 13 ends

	// A cursor well above the ids the reset log will hand out.
<<<<<<< conflict 9 of 13
+++++++ zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
	committed := seedStreamEvents(t, database, key, 1, 0)
	receiveRelayedThrough(t, subscription, opened, committed)
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
-	rename("Renamed before the reset")
-	committed := eventHead(t, database)
+	renameIssue(t, rig.handler, issue.Key, "Renamed before the reset")
+	committed := eventHead(t, rig.database)
 	for last := int64(0); last < committed; {
-		select {
-		case event, open := <-subscription:
-			if !open {
-				t.Fatal("the relay's subscription ended before the event log was reset")
-			}
-			last = event.ID
-		case <-time.After(5 * time.Second):
-			t.Fatalf("the relay published through %d before the reset, want the %d committed", last, committed)
-		}
+		last = nextEventID(t, client, "events")
 	}
>>>>>>> conflict 9 of 13 ends

	if _, err := rig.database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	// TRUNCATE notifies nobody: it is the refill's own insert that fires migration 0107's trigger
	// and wakes the relay onto a log that restarts at id 1.
<<<<<<< conflict 10 of 13
+++++++ zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
	refilled := seedStreamEvents(t, database, key, 1, 0)
	if refilled >= committed {
		t.Fatalf("event %d committed after the reset is not below the pre-reset %d: the reset reused no id, so this test proves nothing", refilled, committed)
	}
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
-	rename("Renamed after the reset")
+	renameIssue(t, rig.handler, issue.Key, "Renamed after the reset")
>>>>>>> conflict 10 of 13 ends

<<<<<<< conflict 11 of 13
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
-	// The subscription open across the reset ends. An event above the pre-reset head is a row of
-	// the old log committed between the drain above and the TRUNCATE -- a document settling, say;
-	// one at or below it would be the relay republishing the reused ids into a reader that has
-	// already seen those ids stand for other events.
+	// The subscription open across the reset ends without a frame of the new log, whose ids
+	// stand for other events than the ones its reader was sent.
 	for open := true; open; {
 		select {
 		case event, more := <-subscription:
 			if !more {
 				open = false
-			} else if event.ID <= committed {
-				t.Fatalf("the relay published event %d into a subscription whose log was reset under it", event.ID)
+			} else {
+				t.Fatalf("the relay published event %d into a subscription whose log was reset under it", event.Event.ID)
 			}
 		case <-time.After(5 * time.Second):
 			t.Fatal("the subscription open across the reset was never ended: its reader waits on a stream whose next frame can only lie")
+++++++ vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
	// The subscription open across the reset ends. An event above the pre-reset head is a row of
	// the old log committed between the drain above and the TRUNCATE -- a document settling, say;
	// one at or below it would be the relay republishing the reused ids to a page that has already
	// seen those ids stand for other events.
	for ended := false; !ended; {
		switch frame := client.next(t, eventFeedWait, "the end of the subscription open across the reset").(type) {
		case LiveEventFrame:
			if id := mustEventFrameID(t, frame); id <= committed {
				t.Fatalf("the feed sent event %d to a subscription whose log was reset under it", id)
			}
		case LiveEnded:
			if frame.Sub != "events" || frame.Reason != "behind" {
				t.Fatalf("ended = %+v, want the events subscription's, behind", frame)
			}
			ended = true
		default:
			t.Fatalf("frame = %+v, want the events subscription's end", frame)
>>>>>>> conflict 11 of 13 ends
		}
	}
	// The reset ended the generation: its LISTEN connection closes though its subscriber has not
	// cancelled.
	waitForBackends(t, database, 0, relayListenBackend)

<<<<<<< conflict 12 of 13
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
-	// And the relay publishes again, from the new log, to a connection opened after the reset.
-	reopened, cancelReopened, err := relay.subscribe(context.Background())
-	if err != nil {
-		t.Fatalf("subscribe to the relay after the reset: %v", err)
-	}
-	defer cancelReopened()
-	rename("Renamed for the reopened connection")
-	head := eventHead(t, database)
-	for last := int64(0); last < head; {
-		select {
-		case event, open := <-reopened:
-			if !open {
-				t.Fatal("the relay ended a subscription opened after the reset")
-			}
-			last = event.ID
-		case <-time.After(5 * time.Second):
-			t.Fatalf("the relay published through %d after the reset, want the %d committed since: it is silent on the new log", last, head)
-		}
-	}
+	// And the next subscription starts on the new log.
+	reopened, _, head := subscribeRelay(t, relay)
+	if head != refilled {
+		t.Fatalf("a subscription after the reset has head %d, want the new log's %d", head, refilled)
+	}
+	next := seedStreamEvents(t, database, key, 1, 0)
+	receiveRelayedThrough(t, reopened, head, next)
+++++++ vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
	// And the relay publishes again, from the new log, to the page's subscription made after the
	// reset.
	subscribeEvents(t, client, "events", nil)
	renameIssue(t, rig.handler, issue.Key, "Renamed for the resubscribed page")
	head := eventHead(t, rig.database)
	for last := int64(0); last < head; {
		last = nextEventID(t, client, "events")
	}
}

// mustEventFrameID is the decimal id an event frame names.
func mustEventFrameID(t *testing.T, frame LiveEventFrame) int64 {
	t.Helper()
	var event feedEvent
	if err := json.Unmarshal(frame.Event, &event); err != nil {
		t.Fatalf("decode the event of frame %+v: %v", frame, err)
	}
	return event.ID
>>>>>>> conflict 12 of 13 ends
}

// TestLiveAskEventFrameCarriesTheReadTimeFields: an ask payload cannot carry opened_event_id or
// referenced_by_count from the write that appended it, so both are attached at read time
<<<<<<< conflict 13 of 13
+++++++ zusvmkwt be5cee56 "test(dispatch): a held fallback copy outlives a NATS outage longer than the dead-letter threshold and arrives once"
// (attachAskEventFields). Sourcing the live stream through the same row read as the replay
// (readEventRows) and the same encoding (encodeSSEFrame) is what makes the frame a client
// receives live and the frame it receives when it reconnects the same bytes; a hand-rolled live
// read would hand a page an ask without them, and only a reconnect would repair it.
%%%%%%% diff from: mmlowqmk d4cbcfe6 "fix: tests that fail and pass on a re-run are fixed at their cause, with the product races behind them (#2396)"
\\\\\\\        to: vvkzzusr f9c01e92 "chore: fold changelog fragments [skip ci]"
-// (attachAskEventFields). Sourcing the live stream through the same read as the replay
-// (readEventsAfterID) is what makes the frame a client receives live and the frame it receives
-// when it reconnects the same bytes; a hand-rolled live read would hand a page an ask without
-// them, and only a reconnect would repair it.
+// (attachAskEventFields). Sourcing the live feed through the same read as the replay
+// (readEventsAfterID) is what makes the frame a page receives live and the frame it receives when
+// it resumes the same bytes; a hand-rolled live read would hand a page an ask without them, and
+// only a resubscribe would repair it.
>>>>>>> conflict 13 of 13 ends
func TestLiveAskEventFrameCarriesTheReadTimeFields(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Ask fields", "A spec")
	since := eventHead(t, rig.database)
	live := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, live, "events", new(since))

	created := sessionRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which path?",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	liveFrame, _ := eventOfType(t, live, "events", "ask.opened")

	// The same event, replayed by a subscription resuming from before it.
	replay := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, replay, "events", new(since))
	replayedFrame, _ := eventOfType(t, replay, "events", "ask.opened")

	if liveFrame.ID != replayedFrame.ID || !bytes.Equal(liveFrame.Event, replayedFrame.Event) {
		t.Fatalf("live ask.opened frame\n\t%s %s\ndiffers from the replayed one\n\t%s %s", liveFrame.ID, liveFrame.Event, replayedFrame.ID, replayedFrame.Event)
	}
	var event struct {
		Payload map[string]json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(liveFrame.Event, &event); err != nil {
		t.Fatalf("decode the live ask.opened event %s: %v", liveFrame.Event, err)
	}
	for _, field := range []string{"opened_event_id", "referenced_by_count"} {
		value, ok := event.Payload[field]
		if !ok || string(value) == "null" {
			t.Fatalf("live ask.opened payload = %s, want a read-time %s", liveFrame.Event, field)
		}
	}
}

// liveEventSeed is one event a test writes directly, oldest first.
type liveEventSeed struct {
	kind    string
	actor   string
	payload string
	at      time.Time
}

// seedLiveEvents writes each event on issue LIVE-1, oldest first, and returns the ids they took.
func seedLiveEvents(t *testing.T, database *store.Store, seeds []liveEventSeed) []int64 {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name) values ('LIVE', 'Live') on conflict do nothing`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('LIVE-1', 'LIVE', 1, 'x', '{"kind":"user","id":"alice"}', 'U') on conflict do nothing`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	ids := make([]int64, 0, len(seeds))
	for _, seed := range seeds {
		var id int64
		if err := database.Pool.QueryRow(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			select 'LIVE-1', coalesce(max(seq), 0) + 1, $1, $2::jsonb, $3::jsonb, false, $4
			from events where issue_key = 'LIVE-1'
			returning id
		`, seed.kind, seed.actor, seed.payload, seed.at).Scan(&id); err != nil {
			t.Fatalf("insert %s event: %v", seed.kind, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// liveUser is the actor of the events a test writes.
const liveUser = `{"kind":"user","id":"alice"}`

// eventCursorCheck reads the newest event's id and time, and the time of the event at the cursor;
// once the cursor's row is gone (a reset of the log), cursorAt is nil.
func TestEventCursorCheckReadsTheNewestEventAndTheCursorsRow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()

	maxID, maxAt, cursorAt, err := eventCursorCheck(ctx, database.Pool, 0)
	if err != nil {
		t.Fatalf("eventCursorCheck on an empty log: %v", err)
	}
	if maxID != 0 || !maxAt.IsZero() || cursorAt != nil {
		t.Fatalf("an empty log = %d, %v, %v; want 0, zero, nil", maxID, maxAt, cursorAt)
	}

	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.created", actor: liveUser, payload: `{}`, at: base},
		{kind: "artifact.version", actor: liveUser, payload: `{}`, at: base.Add(time.Minute)},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(2 * time.Minute)},
	})
	maxID, maxAt, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[1])
	if err != nil {
		t.Fatalf("eventCursorCheck: %v", err)
	}
	if maxID != ids[2] || !maxAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("newest event = %d at %v, want %d at %v", maxID, maxAt, ids[2], base.Add(2*time.Minute))
	}
	if cursorAt == nil || !cursorAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("the cursor's row time = %v, want %v", cursorAt, base.Add(time.Minute))
	}

	if _, err := database.Pool.Exec(ctx, `delete from events where id = $1`, ids[1]); err != nil {
		t.Fatalf("delete the cursor's row: %v", err)
	}
	maxID, maxAt, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[1])
	if err != nil {
		t.Fatalf("eventCursorCheck once the cursor's row is gone: %v", err)
	}
	if maxID != ids[2] || !maxAt.Equal(base.Add(2*time.Minute)) || cursorAt != nil {
		t.Fatalf("with the cursor's row gone = %d, %v, %v; want %d, %v, nil", maxID, maxAt, cursorAt, ids[2], base.Add(2*time.Minute))
	}

	if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	reset := seedLiveEvents(t, database, []liveEventSeed{{kind: "issue.created", actor: liveUser, payload: `{}`, at: base.Add(time.Hour)}})
	maxID, _, cursorAt, err = eventCursorCheck(ctx, database.Pool, ids[2])
	if err != nil {
		t.Fatalf("eventCursorCheck after a reset: %v", err)
	}
	if maxID != reset[0] || maxID >= ids[2] || cursorAt != nil {
		t.Fatalf("after a reset = %d, %v; want the new newest id %d below the cursor %d, and nil", maxID, cursorAt, reset[0], ids[2])
	}
}

// cursorCheck is one answer of eventCursorCheck.
type cursorCheck struct {
	maxID    int64
	maxAt    time.Time
	cursorAt *time.Time
}

func (c cursorCheck) equal(other cursorCheck) bool {
	if c.maxID != other.maxID || !c.maxAt.Equal(other.maxAt) {
		return false
	}
	if c.cursorAt == nil || other.cursorAt == nil {
		return c.cursorAt == nil && other.cursorAt == nil
	}
	return c.cursorAt.Equal(*other.cursorAt)
}

func (c cursorCheck) String() string {
	cursorAt := "no row"
	if c.cursorAt != nil {
		cursorAt = c.cursorAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("newest id %d at %s, cursor's row %s", c.maxID, c.maxAt.UTC().Format(time.RFC3339), cursorAt)
}

// resetAfterFirstStatement is a queryer that resets the event log the moment the first statement
// read through it has answered, which is a reset committing in the middle of whatever read that
// statement began.
type resetAfterFirstStatement struct {
	queryer
	reset func()
	fired bool
}

func (q *resetAfterFirstStatement) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return scanThen{row: q.queryer.QueryRow(ctx, sql, args...), after: func() {
		if !q.fired {
			q.fired = true
			q.reset()
		}
	}}
}

// scanThen is a row that runs after once its Scan has answered.
type scanThen struct {
	row   pgx.Row
	after func()
}

func (r scanThen) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	r.after()
	return err
}

// A reset of the event log (e2e/seed.ts's `truncate events restart identity` and its refill) that
// commits while eventCursorCheck is reading must leave the answer describing one log, the old one
// or the new one. A mixed answer - the old log's newest id with no row at the cursor in the new
// one - is one the relay would take for a reset to the old log's head: it would move its cursor
// there, and the new log's lower ids would then read as a second reset that closed the
// subscription opened after the first (TestEventStreamRelayRecoversWhenTheEventLogIsReset).
func TestEventCursorCheckReadsOneLogWhenTheLogIsResetMidCheck(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.created", actor: liveUser, payload: `{}`, at: base},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(time.Minute)},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(2 * time.Minute)},
	})
	cursor := ids[1]
	check := func(q queryer) cursorCheck {
		t.Helper()
		maxID, maxAt, cursorAt, err := eventCursorCheck(ctx, q, cursor)
		if err != nil {
			t.Fatalf("eventCursorCheck: %v", err)
		}
		return cursorCheck{maxID: maxID, maxAt: maxAt, cursorAt: cursorAt}
	}

	oldLog := check(database.Pool)
	resetting := &resetAfterFirstStatement{queryer: database.Pool, reset: func() {
		if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
			t.Fatalf("reset the event log: %v", err)
		}
		seedLiveEvents(t, database, []liveEventSeed{{kind: "issue.created", actor: liveUser, payload: `{}`, at: base.Add(time.Hour)}})
	}}
	got := check(resetting)
	if !resetting.fired {
		t.Fatal("the check read nothing through the queryer, so no reset landed inside it")
	}
	newLog := check(database.Pool)

	if !got.equal(oldLog) && !got.equal(newLog) {
		t.Fatalf("a reset during the check answered %v; want the old log's answer (%v) or the new log's (%v), not two logs in one answer",
			got, oldLog, newLog)
	}
}
