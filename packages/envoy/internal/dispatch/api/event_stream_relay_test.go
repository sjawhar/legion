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

// TestEventStreamRelayCatchesUpWhenItsListenerReconnects covers the window a redial leaves: a
// notification sent while the relay has no connection reaches nobody, and nothing sends it again.
// What covers it is the catch-up the listener's onConnected runs after every LISTEN, from the
// cursor the relay has published through rather than from the log's head -- a cursor reset there
// would silently swallow exactly the events the window dropped from every open page's feed.
func TestEventStreamRelayCatchesUpWhenItsListenerReconnects(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	// An issue with no spec, whose document settles nothing, so nothing commits but the test's own.
	issue := createPlainIssue(t, rig.handler, "TEST")
	relay := rig.served.eventRelay
	var down atomic.Bool
	down.Store(true)
	relay.dial = func(ctx context.Context) (*pgx.Conn, error) {
		if down.Load() {
			return nil, errors.New("the listener has no connection")
		}
		return rig.database.Pool.Listener(ctx)
	}
	relay.backoff = func(int) time.Duration { return 5 * time.Millisecond }
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

	// Committed while the relay has no connection: its notification reaches nobody.
	renameIssue(t, rig.handler, issue, "Renamed while the listener was down")
	committed := eventHead(t, rig.database)
	client.quiet(t, 100*time.Millisecond, "the relay with no listening connection")
	if at, _ := relay.at(); at != opened {
		t.Fatalf("relay cursor = %d with no connection, want the %d it opened on", at, opened)
	}

	down.Store(false)
	for last := before; last < committed; {
		id := nextEventID(t, client, "events")
		if id <= last {
			t.Fatalf("the feed sent %d after %d; a catch-up publishes in id order", id, last)
		}
		last = id
	}

	// Every later LISTEN catches up the same way, from the cursor as it stands: a redial that
	// reset it would send every open page the whole log again.
	published, _ := relay.at()
	relay.onConnected()
	client.quiet(t, 200*time.Millisecond, "a redial with nothing committed since")
	if at, _ := relay.at(); at < published {
		t.Fatalf("relay cursor = %d after a redial, want at least the %d it had published through", at, published)
	}
}

// TestEventStreamRelayRecoversWhenTheEventLogIsReset: the browser suite empties the event log
// between its scenarios (`truncate ... restart identity cascade`, e2e/seed.ts), so the new rows'
// ids restart at 1, below the cursor the relay has published through, and `id > cursor` matches
// nothing. A relay that only ever published forward would fall silent there for good: every live
// update of every later scenario would be lost, with no error anywhere. The relay reads the reset
// the way the delivery hub does (eventCursorCheck), moves its cursor to the new log's head and
// drops every subscriber -- the stream's answer to a reader it can no longer serve in order --
// which ends every open events subscription with an ended frame (behind), so the page subscribes
// again; and it serves the events committed after the reset to the subscriptions made after it.
func TestEventStreamRelayRecoversWhenTheEventLogIsReset(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	rig.served.eventRelay.coalesce = time.Millisecond
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	client := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, client, "events", nil)

	// A cursor well above the ids the reset log will hand out.
	renameIssue(t, rig.handler, issue.Key, "Renamed before the reset")
	committed := eventHead(t, rig.database)
	for last := int64(0); last < committed; {
		last = nextEventID(t, client, "events")
	}

	if _, err := rig.database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	// TRUNCATE notifies nobody: it is the refill's own insert that fires migration 0107's trigger
	// and wakes the relay onto a log that restarts at id 1.
	renameIssue(t, rig.handler, issue.Key, "Renamed after the reset")

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
		}
	}

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
}

// TestLiveAskEventFrameCarriesTheReadTimeFields: an ask payload cannot carry opened_event_id or
// referenced_by_count from the write that appended it, so both are attached at read time
// (attachAskEventFields). Sourcing the live feed through the same read as the replay
// (readEventsAfterID) is what makes the frame a page receives live and the frame it receives when
// it resumes the same bytes; a hand-rolled live read would hand a page an ask without them, and
// only a resubscribe would repair it.
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
