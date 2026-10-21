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

// TestEventStreamRelayCatchesUpWhenItsListenerReconnects covers the window a redial leaves: a
// notification sent while the relay has no connection reaches nobody, and nothing sends it again.
// What covers it is the catch-up the listener's onConnected runs after every LISTEN, from the
// cursor the relay has published through rather than from the log's head -- a cursor reset there
// would silently swallow exactly the events the window dropped.
func TestEventStreamRelayCatchesUpWhenItsListenerReconnects(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")

	lifetime, stop := context.WithCancel(t.Context())
	relay := newEventStreamRelay(directServer(deps), lifetime)
	var down atomic.Bool
	down.Store(true)
	relay.dial = func(ctx context.Context) (*pgx.Conn, error) {
		if down.Load() {
			return nil, errors.New("the listener has no connection")
		}
		return database.Pool.Listener(ctx)
	}
	relay.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	relay.coalesce = time.Millisecond
	t.Cleanup(func() {
		stop()
		relay.wait()
	})
	subscription, cancel, err := relay.subscribe(context.Background())
	if err != nil {
		t.Fatalf("subscribe to the relay: %v", err)
	}
	defer cancel()
	opened := relay.at()

	// Committed while the relay has no connection: its notification reaches nobody.
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed while the listener was down",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename issue: status=%d body=%s", response.Code, response.Body.String())
	}
	committed := eventHead(t, database)
	select {
	case event := <-subscription:
		t.Fatalf("the relay published event %d with no listening connection", event.ID)
	case <-time.After(100 * time.Millisecond):
	}
	if at := relay.at(); at != opened {
		t.Fatalf("relay cursor = %d with no connection, want the %d it opened on", at, opened)
	}

	down.Store(false)
	var last int64
	for last < committed {
		select {
		case event, open := <-subscription:
			if !open {
				t.Fatal("the relay's subscription ended")
			}
			if event.ID <= last {
				t.Fatalf("the relay published %d after %d; a catch-up publishes in id order", event.ID, last)
			}
			last = event.ID
		case <-time.After(5 * time.Second):
			t.Fatalf("the relay published through %d after its listener reconnected, want the %d committed while it was down", last, committed)
		}
	}

	// Every later LISTEN catches up the same way, from the cursor as it stands: a redial that
	// reset it would send every open page the whole log again.
	published := relay.at()
	relay.onConnected()
	select {
	case event := <-subscription:
		if event.ID <= published {
			t.Fatalf("a redial republished event %d, at or below the %d the relay had published through", event.ID, published)
		}
	case <-time.After(200 * time.Millisecond):
	}
	if at := relay.at(); at < published {
		t.Fatalf("relay cursor = %d after a redial, want at least the %d it had published through", at, published)
	}
}

// TestLiveAskEventFrameCarriesTheReadTimeFields: an ask payload cannot carry opened_event_id or
// referenced_by_count from the write that appended it, so both are attached at read time
// (attachAskEventFields). Sourcing the live stream through the same read as the replay
// (readEventsAfterID) is what makes the frame a client receives live and the frame it receives
// when it reconnects the same bytes; a hand-rolled live read would hand a page an ask without
// them, and only a reconnect would repair it.
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
