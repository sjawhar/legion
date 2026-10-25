package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// eventFeedWait bounds a wait for one frame of the event feed: a frame that never arrives fails the
// test by name rather than holding it until the package's own timeout, which would take every other
// test's result with it.
const eventFeedWait = 30 * time.Second

// eventHead is the newest committed event id: what a page resuming now would name as its since.
func eventHead(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var head int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&head); err != nil {
		t.Fatalf("read the event log's head: %v", err)
	}
	return head
}

// subscribeEvents subscribes client to the event feed under sub, resuming after since when it is
// not nil, and answers the id the subscription's events_start names: the one it starts after.
func subscribeEvents(t *testing.T, client *socketClient, sub string, since *int64) int64 {
	t.Helper()
	resume := ""
	if since != nil {
		resume = `{"since":"` + strconv.FormatInt(*since, 10) + `"}`
	}
	client.send(t, liveSubscribe(sub, eventsView(), resume))
	frame := client.next(t, eventFeedWait, "the events_start of "+sub)
	start, ok := frame.(LiveEventsStart)
	if !ok || start.Sub != sub {
		t.Fatalf("the first frame of %s = %+v, want its events_start", sub, frame)
	}
	id, err := strconv.ParseInt(start.Since, 10, 64)
	if err != nil {
		t.Fatalf("events_start.since = %q, not a decimal event id: %v", start.Since, err)
	}
	return id
}

// feedEvent is what a test reads of the event an event frame carries.
type feedEvent struct {
	ID       int64           `json:"id"`
	IssueKey *string         `json:"issue_key"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
}

// nextEvent reads sub's next frame, which must be one of its events, and answers it with the event
// it carries, whose id must be the one the frame names.
func nextEvent(t *testing.T, client *socketClient, sub string) (LiveEventFrame, feedEvent) {
	t.Helper()
	frame := client.event(t, eventFeedWait, sub)
	var event feedEvent
	if err := json.Unmarshal(frame.Event, &event); err != nil {
		t.Fatalf("decode the event of %s's frame %+v: %v", sub, frame, err)
	}
	if frame.ID != strconv.FormatInt(event.ID, 10) {
		t.Fatalf("%s's frame names id %q for event %d", sub, frame.ID, event.ID)
	}
	return frame, event
}

// nextEventID is the id of sub's next event.
func nextEventID(t *testing.T, client *socketClient, sub string) int64 {
	t.Helper()
	_, event := nextEvent(t, client, sub)
	return event.ID
}

// eventOfType reads sub's events until one is of eventType.
func eventOfType(t *testing.T, client *socketClient, sub, eventType string) (LiveEventFrame, feedEvent) {
	t.Helper()
	for range maxEventReplay {
		frame, event := nextEvent(t, client, sub)
		if event.Type == eventType {
			return frame, event
		}
	}
	t.Fatalf("no %s event reached %s", eventType, sub)
	return LiveEventFrame{}, feedEvent{}
}

// renameIssue retitles key as alice, failing the test unless the API answers 200.
func renameIssue(t *testing.T, handler http.Handler, key, title string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]string{
		"title": title,
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename %s to %q: status=%d body=%s", key, title, response.Code, response.Body.String())
	}
}

// seedEvents writes count events of key's straight into the log, a backlog to page through, each
// payload padded with pad bytes, and moves the issue's sequence past them so the API's next write
// on it appends after them.
func seedEvents(t *testing.T, database *store.Store, key string, count, pad int) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, issues.last_seq + value, 'seeded', jsonb_build_object('kind', 'session', 'id', 'seed'),
			jsonb_build_object('seq', value, 'pad', repeat('x', $3::int)), false
		from issues, generate_series(1, $2::int) as value
		where issues.key = $1
	`, key, count, pad); err != nil {
		t.Fatalf("seed %d events of %s: %v", count, key, err)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set last_seq = last_seq + $2::int where key = $1`, key, count); err != nil {
		t.Fatalf("move %s's sequence past the seeded events: %v", key, err)
	}
}

// createPlainIssue creates project and an issue in it with no spec, as alice, and answers the
// issue's key: two events, project.created and issue.created, and no document to settle.
func createPlainIssue(t *testing.T, handler http.Handler, project string) string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": project, "name": project + " project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "Issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

func TestTheEventFeedReplaysThenStreamsACommittedEvent(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createPlainIssue(t, rig.handler, "TEST")

	client := rig.dial(t, withBearer("agent-token"))
	if start := subscribeEvents(t, client, "events", new(int64(0))); start != 0 {
		t.Fatalf("events_start.since = %d, want the 0 the page resumed from", start)
	}
	if _, project := nextEvent(t, client, "events"); project.Type != "project.created" {
		t.Fatalf("first replayed event = %+v, want project.created", project)
	}
	if _, replayed := nextEvent(t, client, "events"); replayed.Type != "issue.created" || replayed.IssueKey == nil || *replayed.IssueKey != "TEST-1" {
		t.Fatalf("second replayed event = %+v, want issue.created of TEST-1", replayed)
	}

	renameIssue(t, rig.handler, issue, "Renamed")
	if _, live := nextEvent(t, client, "events"); live.Type != "issue.updated" || !strings.Contains(string(live.Payload), `"title":"Renamed"`) {
		t.Fatalf("live event = %+v, want the rename's issue.updated", live)
	}

	// A page resuming after the second event, on a socket of its own, is sent the third.
	resumed := rig.dial(t, withBearer("agent-token"))
	if start := subscribeEvents(t, resumed, "events", new(int64(2))); start != 2 {
		t.Fatalf("the resumed subscription's events_start.since = %d, want 2", start)
	}
	if _, event := nextEvent(t, resumed, "events"); event.ID != 3 || event.Type != "issue.updated" {
		t.Fatalf("resumed replay = %+v, want event 3, the issue.updated", event)
	}
}

// A person's socket and a bearer's are both served the feed.
func TestTheEventFeedServesAPersonAndABearer(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	person := rig.dial(t, asUser("alice"))
	bearer := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, person, "feed", nil)
	subscribeEvents(t, bearer, "feed", nil)
	renameIssue(t, rig.handler, issue.Key, "Seen by both")
	for name, client := range map[string]*socketClient{"the person": person, "the bearer": bearer} {
		if _, event := eventOfType(t, client, "feed", "issue.updated"); !strings.Contains(string(event.Payload), "Seen by both") {
			t.Errorf("%s was sent %+v, want the rename", name, event)
		}
	}
}

func TestTheEventFeedPagesThroughCappedBacklogWithoutDisconnecting(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createPlainIssue(t, rig.handler, "TEST")
	seedEvents(t, rig.database, issue, 1005, 0)
	head := eventHead(t, rig.database)
	if head-1 <= maxEventReplay {
		t.Fatalf("a backlog of %d events after the first fits one page of %d", head-1, maxEventReplay)
	}

	// The backlog after event 1 is more than one capped page; one subscription pages through all of
	// it without ever ending: a capped page that ended the subscription and sent the page to
	// subscribe again would leave a gap where a low id committing between "read this page" and "the
	// next subscription subscribes" could be lost for good.
	client := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, client, "events", new(int64(1)))
	for want := int64(2); want <= head; want++ {
		if id := nextEventID(t, client, "events"); id != want {
			t.Fatalf("paged replay sent event %d, want %d", id, want)
		}
	}
	// The same subscription is still open afterwards: a live event reaches it with no resubscribe.
	renameIssue(t, rig.handler, issue, "Renamed after paging")
	if _, live := nextEvent(t, client, "events"); live.Type != "issue.updated" || !strings.Contains(string(live.Payload), `"title":"Renamed after paging"`) {
		t.Fatalf("live event after the paged backlog = %+v", live)
	}
}

func TestTheEventFeedResumeCursorOrdersConcurrentEventCommits(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	broker := rig.served.deps.Events
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, rig.handler, "OTHER", "Other issue", "A spec")

	ctx := context.Background()
	first, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first transaction: %v", err)
	}
	defer first.Rollback(ctx)
	low, err := broker.Append(ctx, first, model.Event{
		IssueKey: new(issue.Key),
		Type:     "test.concurrent",
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		Payload:  map[string]any{},
	})
	if err != nil {
		t.Fatalf("append first event: %v", err)
	}

	type appendResult struct {
		event model.Event
		err   error
	}
	started := make(chan struct{})
	secondResult := make(chan appendResult, 1)
	go func() {
		second, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			secondResult <- appendResult{err: err}
			return
		}
		defer second.Rollback(ctx)
		close(started)
		event, err := broker.Append(ctx, second, model.Event{
			IssueKey: new(other.Key),
			Type:     "test.concurrent",
			Actor:    model.Actor{Kind: "session", ID: "seed"},
			Payload:  map[string]any{},
		})
		if err == nil {
			err = second.Commit(ctx)
		}
		secondResult <- appendResult{event: event, err: err}
	}()
	<-started
	waitForDatabaseLocks(t, first, 1)

	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit first event: %v", err)
	}
	high := <-secondResult
	if high.err != nil {
		t.Fatalf("append second event: %v", high.err)
	}
	if high.event.ID <= low.ID {
		t.Fatalf("event ids did not follow commit order: first=%d second=%d", low.ID, high.event.ID)
	}

	client := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, client, "events", new(low.ID))
	if id := nextEventID(t, client, "events"); id != high.event.ID {
		t.Fatalf("resume replay sent event %d, want the second committed event %d", id, high.event.ID)
	}
}

// TestTheEventFeedDoesNotResendAnEventAtOrBelowItsSince covers the one id comparison the live loop
// makes. The relay is one per process and can stand behind a subscription opened after it: it then
// publishes ids that subscription's catch-up has already covered, which sentDuringCatchup cannot be
// asked about, since it holds only the ids above since that catch-up sent. Forwarding them would
// show a page an event it has already seen.
//
// The comparison is safe because since is fixed for the subscription, and the commit-order lock
// events.Broker.Append holds until its transaction commits leaves no uncommitted id at or below a
// committed max(id). A late-committing lower id -- what the live loop used to carry a rule against
// -- is now the lock's to prevent and TestOnlyTheBrokerInsertsEvents's to keep.
func TestTheEventFeedDoesNotResendAnEventAtOrBelowItsSince(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	// Long enough that the second subscription below opens between a commit and the read that
	// commit's notification wakes, so the relay really is behind that subscription's since.
	rig.served.eventRelay.coalesce = time.Second
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")

	// The first subscription starts the relay, whose cursor stops at what has committed by now.
	subscribeEvents(t, rig.dial(t, withBearer("agent-token")), "cold", nil)
	renameIssue(t, rig.handler, issue.Key, "Renamed")
	since := eventHead(t, rig.database)

	// This subscription's catch-up covers everything through since, which the relay has not read up
	// to yet: what the relay publishes next is this subscription's own backlog.
	client := rig.dial(t, withBearer("agent-token"))
	if start := subscribeEvents(t, client, "resumed", new(since)); start != since {
		t.Fatalf("events_start.since = %d, want the %d the page resumed from", start, since)
	}
	renameIssue(t, rig.handler, issue.Key, "Renamed again")
	if id := nextEventID(t, client, "resumed"); id <= since {
		t.Fatalf("live event id = %d, want an id above the subscription's since %d", id, since)
	}
}

// TestTheEventFeedCappedCatchupDeliversACommitDuringPagingExactlyOnce: a catch-up that fills a page
// keeps paging on the relay subscription it opened with. An event committing while it pages reaches
// the page on a later catch-up page or live from the relay, whichever sees it first, and
// sentDuringCatchup is what keeps it from arriving on both. A capped page that ended the
// subscription instead, sending the page to subscribe again, would leave a gap between the last
// page and the new subscription for that commit to fall into.
func TestTheEventFeedCappedCatchupDeliversACommitDuringPagingExactlyOnce(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	since := eventHead(t, rig.database)
	// Exactly maxEventReplay events after since, so the first catch-up page fills and a second runs.
	seedEvents(t, rig.database, issue.Key, maxEventReplay, 0)
	client := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, client, "events", new(since))

	// Committed while that catch-up is still paging.
	renameIssue(t, rig.handler, issue.Key, "Renamed")
	live := eventHead(t, rig.database)
	seen := map[int64]bool{}
	for id := int64(0); id != live; {
		id = nextEventID(t, client, "events")
		if seen[id] {
			t.Fatalf("event %d was delivered twice", id)
		}
		seen[id] = true
		if len(seen) > maxEventReplay+100 {
			t.Fatalf("the commit %d made during the catch-up was not delivered in %d frames", live, len(seen))
		}
	}
}

// TestTheEventFeedColdStartSendsNoBacklogAndDeliversTheNextCommit: a cold subscription (no since)
// resolves its own head inside the subscription, after it has subscribed to the relay. It is sent
// nothing already committed, which is the backlog it asked not to get, and everything committing
// after it, which lands in its relay subscription however its catch-up went. A page that read its
// head through a separate request first would lose an event committing between that request and
// this subscription: invisible to the catch-up (id <= since) and to a relay subscription made only
// afterwards.
func TestTheEventFeedColdStartSendsNoBacklogAndDeliversTheNextCommit(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	backlog := eventHead(t, rig.database)
	client := rig.dial(t, withBearer("agent-token"))
	if start := subscribeEvents(t, client, "events", nil); start < backlog {
		t.Fatalf("a cold subscription starts after event %d, below the head %d it opened on", start, backlog)
	}

	renameIssue(t, rig.handler, issue.Key, "Renamed")
	live := eventHead(t, rig.database)
	for id := int64(0); id != live; {
		id = nextEventID(t, client, "events")
		if id <= backlog {
			t.Fatalf("a cold subscription was sent event %d, backlog at or below the head %d it opened on", id, backlog)
		}
	}
}

// A cold subscription's events_start names the head it resolved after subscribing to the relay, so
// a page holding no cursor adopts it and resumes from it without a gap; and the commit right after
// it reaches the subscription.
func TestAColdSubscriptionStartsWithTheHead(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	client := rig.dial(t, withBearer("agent-token"))
	before := eventHead(t, rig.database)
	start := subscribeEvents(t, client, "events", nil)
	if after := eventHead(t, rig.database); start < before || start > after {
		t.Fatalf("a cold subscription starts after event %d, outside the heads %d..%d around its subscribe", start, before, after)
	}

	renameIssue(t, rig.handler, issue.Key, "Renamed after the start")
	for {
		_, event := nextEvent(t, client, "events")
		if event.ID <= start {
			t.Fatalf("event %d reached a subscription that starts after %d", event.ID, start)
		}
		if event.Type == "issue.updated" && strings.Contains(string(event.Payload), "Renamed after the start") {
			return
		}
	}
}

// A since above the head is an id of a log this one replaced (or no id at all): the subscription
// starts at the head, and is served what commits next.
func TestASinceAboveTheHeadStartsAtTheHead(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	client := rig.dial(t, withBearer("agent-token"))
	before := eventHead(t, rig.database)
	start := subscribeEvents(t, client, "events", new(before+1000))
	if after := eventHead(t, rig.database); start < before || start > after {
		t.Fatalf("a since of %d starts after event %d, want the head (%d..%d)", before+1000, start, before, after)
	}
	renameIssue(t, rig.handler, issue.Key, "Renamed above the head")
	if _, event := eventOfType(t, client, "events", "issue.updated"); !strings.Contains(string(event.Payload), "Renamed above the head") {
		t.Fatalf("the clamped subscription was sent %+v, want the rename", event)
	}
}

// TestTheEventFeedResumesAfterTheEventLogIsResetBeneathTheReader: the browser suite empties the event
// log between its scenarios (`truncate ... restart identity cascade`, e2e/seed.ts), so the next
// row's id is 1 again. The relay notices, ends every open subscription (ended, behind), and the page
// subscribes again -- and a page only ever raises its cursor, never lowers it, so it comes back
// naming a since above the new log's head. Without the clamp, catch-up (`id > since`) matches
// nothing and the live loop drops every event of the new log for ever, because they all sit at or
// below that stale since: a reader silent for good, on a socket that stays open and heartbeats, with
// nothing logged and nothing failed.
//
// The assertion is the frame of an event committed after the reset, whose id is below the pre-reset
// head this subscription resumed from. It can reach the page only through a since the subscription
// clamped to the head it resolved; an unclamped since drops it, and the only frame this page then
// ever sees is a heartbeat.
func TestTheEventFeedResumesAfterTheEventLogIsResetBeneathTheReader(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	renameIssue(t, rig.handler, issue.Key, "Renamed before the reset")
	head := eventHead(t, rig.database)
	if _, err := rig.database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}

	// What the page resubscribes with after the relay ended its subscription: the last id it saw,
	// which the reset has left above the whole new log.
	client := rig.dial(t, withBearer("agent-token"))
	if start := subscribeEvents(t, client, "events", new(head)); start >= head {
		t.Fatalf("a since of %d above the reset log's head starts after %d: it was not clamped", head, start)
	}

	renameIssue(t, rig.handler, issue.Key, "Renamed after the reset")
	live := eventHead(t, rig.database)
	if live >= head {
		t.Fatalf("event %d committed after the reset is not below the pre-reset head %d: the reset reused no id, so this test proves nothing", live, head)
	}
	for id := int64(0); id != live; {
		id = nextEventID(t, client, "events")
		if id > live {
			t.Fatalf("event id %d, above the %d committed after the reset", id, live)
		}
	}
}

// gatedStream is a liveStreamer whose SendWait waits for its gate to open, as a socket's does for
// room while its page reads nothing, and then hands each frame to frames.
type gatedStream struct {
	gate   chan struct{}
	frames chan any
}

func newGatedStream() *gatedStream {
	return &gatedStream{gate: make(chan struct{}), frames: make(chan any, 4096)}
}

func (stream *gatedStream) SendWait(ctx context.Context, frame any) error {
	select {
	case <-stream.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case stream.frames <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (stream *gatedStream) Human() bool { return false }

// next is the next frame the subscription sent, within eventFeedWait.
func (stream *gatedStream) next(t *testing.T, what string) any {
	t.Helper()
	select {
	case frame := <-stream.frames:
		return frame
	case <-time.After(eventFeedWait):
		t.Fatalf("no frame within %s, waiting for %s", eventFeedWait, what)
		return nil
	}
}

// A subscription the relay drops, because it fell more than the relay holds for one subscriber
// behind, ends with an ended frame (behind) rather than missing events in silence, and the page's
// resubscribe from the last id it applied is served what it missed and what commits next. The
// subscription here waits for room as a socket's does while its page reads nothing, so it drains
// nothing of what the relay publishes into it.
func TestARelayDropEndsTheSubscriptionAsBehind(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	stream := newGatedStream()
	running := served.startEventsSubscription(ctx, stream, "feed", new(int64(0)))
	t.Cleanup(running.stop)
	relay := served.eventRelay
	pollUntil(t, 5*time.Second, "the subscription subscribes to the relay", func() bool { return relay.stream.SubscriberCount() == 1 })

	// More events in one commit than the relay holds for one subscriber.
	seedEvents(t, database, issue, 100, 0)
	pollUntil(t, 5*time.Second, "the relay drops the subscription that fell behind", func() bool {
		return relay.stream.SubscriberCount() == 0
	})

	// Once there is room again the subscription sends what it catches up on, in id order, and then
	// its end.
	close(stream.gate)
	if start, ok := stream.next(t, "feed's events_start").(LiveEventsStart); !ok || start.Sub != "feed" || start.Since != "0" {
		t.Fatalf("first frame = %+v, want feed's events_start from 0", start)
	}
	head := eventHead(t, database)
	var last int64
	for ended := false; !ended; {
		switch frame := stream.next(t, "the end of feed").(type) {
		case LiveEventFrame:
			id := mustEventFrameID(t, frame)
			if frame.Sub != "feed" || frame.ID != strconv.FormatInt(id, 10) || id <= last {
				t.Fatalf("event frame %+v after %d, want feed's next", frame, last)
			}
			last = id
		case LiveEnded:
			if frame.Sub != "feed" || frame.Reason != "behind" {
				t.Fatalf("ended = %+v, want feed's, behind", frame)
			}
			ended = true
		default:
			t.Fatalf("frame = %+v, want feed's events and then its end", frame)
		}
	}
	if last != head {
		t.Fatalf("the subscription caught up through %d before it ended, want the head %d", last, head)
	}
	select {
	case <-running.(*eventsSubscription).done:
	case <-time.After(5 * time.Second):
		t.Fatal("the dropped subscription's goroutine was still running 5s after its end")
	}

	// The page's resubscribe, from the last id it applied, is served what commits next.
	again := newGatedStream()
	close(again.gate)
	resubscribed := served.startEventsSubscription(ctx, again, "feed", new(last))
	t.Cleanup(resubscribed.stop)
	if start, ok := again.next(t, "the resubscribe's events_start").(LiveEventsStart); !ok || start.Since != strconv.FormatInt(last, 10) {
		t.Fatalf("the resubscribe's first frame = %+v, want its events_start from %d", start, last)
	}
	renameIssue(t, handler, issue, "Renamed after the drop")
	frame, ok := again.next(t, "the rename").(LiveEventFrame)
	if !ok || !strings.Contains(string(frame.Event), "Renamed after the drop") {
		t.Fatalf("the resubscribe was sent %+v, want the rename committed after the drop", frame)
	}
}

// A subscription whose read fails (here, a database that no longer answers) ends with an ended
// frame (behind), so its page subscribes again, rather than going silent; and it waits the relay's
// retry first, so a read that keeps failing is asked again at that pace rather than as fast as the
// page can resubscribe.
func TestAnEventsSubscriptionWhoseReadFailsEndsAsBehind(t *testing.T) {
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	database.Pool.Close()
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	stream := newGatedStream()
	close(stream.gate)
	began := time.Now()
	running := served.startEventsSubscription(ctx, stream, "feed", nil)
	t.Cleanup(running.stop)
	ended, ok := stream.next(t, "feed's end").(LiveEnded)
	if !ok || ended.Sub != "feed" || ended.Reason != "behind" {
		t.Fatalf("the first frame of a subscription whose read failed = %+v, want its end, behind", ended)
	}
	if waited := time.Since(began); waited < eventRelayRetry {
		t.Fatalf("the subscription ended %s after it started, before the relay's retry (%s)", waited, eventRelayRetry)
	}
}

// An event that commits between a subscription's relay subscription and its catch-up read is in
// both: the catch-up sends it (it reads every id above the subscription's since, whatever the head
// was), and the relay publishes it into the subscription's channel. The live loop sends it once,
// because it holds the ids catch-up sent. Here the subscription waits for room at its events_start
// frame, which is after it subscribed to the relay and read the head and before its catch-up, so
// the commit lands exactly in that window.
func TestAnEventCommittedDuringTheCatchUpIsSentOnce(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	issue := createPlainIssue(t, handler, "TEST")
	ctx, err := store.ForConcurrentRead(store.WithTransactionTracking(t.Context()))
	if err != nil {
		t.Fatalf("mark the subscription's context for the pool: %v", err)
	}
	stream := newGatedStream()
	running := served.startEventsSubscription(ctx, stream, "feed", new(int64(0)))
	t.Cleanup(running.stop)
	pollUntil(t, 5*time.Second, "the subscription subscribes to the relay", func() bool {
		return served.eventRelay.stream.SubscriberCount() == 1
	})

	renameIssue(t, handler, issue, "Renamed during the catch-up")
	head := eventHead(t, database)
	close(stream.gate)
	if start, ok := stream.next(t, "feed's events_start").(LiveEventsStart); !ok || start.Since != "0" {
		t.Fatalf("first frame = %+v, want feed's events_start from 0", start)
	}
	seen := map[int64]bool{}
	for last := int64(0); last < head; {
		frame, ok := stream.next(t, "feed's events").(LiveEventFrame)
		if !ok {
			t.Fatalf("frame = %+v, want feed's next event", frame)
		}
		id := mustEventFrameID(t, frame)
		if seen[id] {
			t.Fatalf("event %d was sent twice: the commit during the catch-up reached the page on both paths", id)
		}
		seen[id] = true
		last = max(last, id)
	}
	// Nothing of what it already sent follows.
	select {
	case frame := <-stream.frames:
		t.Fatalf("the subscription sent %+v after the catch-up reached the head %d", frame, head)
	case <-time.After(500 * time.Millisecond):
	}
}

// An unsubscribe or a replacement stops the subscription while its socket stays open: its
// goroutine returns and leaves the relay, and nothing more of it reaches the page.
func TestAStoppedEventsSubscriptionLeavesTheRelayWhileItsSocketStaysOpen(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	issue := createInteractionIssue(t, rig.handler, "TEST", "Issue", "A spec")
	relay := rig.served.eventRelay
	client := rig.dial(t, withBearer("agent-token"))

	subscribeEvents(t, client, "feed", nil)
	subscribeEvents(t, client, "feed", nil)
	pollUntil(t, 5*time.Second, "the replaced subscription leaves the relay", func() bool { return relay.stream.SubscriberCount() == 1 })

	client.send(t, LiveUnsubscribe{Type: "unsubscribe", Sub: "feed"})
	pollUntil(t, 5*time.Second, "the unsubscribed subscription leaves the relay", func() bool { return relay.stream.SubscriberCount() == 0 })
	renameIssue(t, rig.handler, issue.Key, "Renamed after the unsubscribe")
	client.quiet(t, 500*time.Millisecond, "after the page unsubscribed")
	if !client.open() {
		t.Fatalf("the socket ended: %v", client.err)
	}
}

// An events view names its kind and nothing else, and its resume a decimal event id; anything else
// is refused and starts nothing.
func TestAnEventsViewNamesOnlyItsKindAndItsSinceIsAnEventID(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	client := rig.dial(t, asUser("alice"))
	preset, query, at, session := 7, "repo=acme/widgets", "2026-10-14T09:00:00Z", "6f1c5b1e-8a3c-4b7e-9a51-0d2f3c4b5a69"
	for _, test := range []struct {
		name  string
		frame LiveSubscribe
	}{
		{"a window", liveSubscribe("a", LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveEventsView, Window: &DeliveryLiveWindow{Preset: &preset}}}, "")},
		{"a query", liveSubscribe("b", LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveEventsView, Query: &query}}, "")},
		{"an at", liveSubscribe("c", LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveEventsView, At: &at}}, "")},
		{"a session", liveSubscribe("d", LiveView{DeliveryLiveView: DeliveryLiveView{Kind: liveEventsView}, SessionID: &session}, "")},
		{"a since that is not a number", liveSubscribe("e", eventsView(), `{"since":"seven"}`)},
		{"a negative since", liveSubscribe("f", eventsView(), `{"since":"-1"}`)},
		{"an empty since", liveSubscribe("g", eventsView(), `{"since":""}`)},
	} {
		client.send(t, test.frame)
		client.refusal(t, 3*time.Second, test.frame.Sub, "INVALID_FRAME")
	}
	client.quiet(t, 300*time.Millisecond, "after every refused events view")
	if subscribers := rig.served.eventRelay.stream.SubscriberCount(); subscribers != 0 {
		t.Fatalf("the relay holds %d subscriptions after every events view was refused", subscribers)
	}
	// The view and the resume each kind takes, which the refusals above differ from: these start.
	if start := subscribeEvents(t, client, "h", new(int64(0))); start != 0 {
		t.Fatalf("an events view resuming from 0 starts after %d", start)
	}
	subscribeEvents(t, client, "i", nil)
}

// TestTheDisconnectHookSendsTheEventFeedThroughItsResume: the live disconnect hook closes every
// socket with 1012, as a restart does, which drives a page through its reconnect without seeding
// thousands of events: the page reopens its socket and resubscribes from the last id it applied, and
// is sent what committed since.
func TestTheDisconnectHookSendsTheEventFeedThroughItsResume(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{testHooks: true}, nil)
	issue := createPlainIssue(t, rig.handler, "TEST")

	client := rig.dial(t, withBearer("agent-token"))
	subscribeEvents(t, client, "events", new(int64(0)))
	if _, project := nextEvent(t, client, "events"); project.Type != "project.created" {
		t.Fatalf("first replayed event = %+v, want project.created", project)
	}
	if _, replayed := nextEvent(t, client, "events"); replayed.Type != "issue.created" {
		t.Fatalf("second replayed event = %+v, want issue.created", replayed)
	}

	if disconnect := agentRequest(t, rig.handler, http.MethodPost, "/api/v1/live/_test/disconnect", nil, "agent-token"); disconnect.Code != http.StatusOK {
		t.Fatalf("disconnect hook: status=%d body=%s", disconnect.Code, disconnect.Body.String())
	}
	if code, _ := client.closeCode(t, 5*time.Second); code != gws.CloseServiceRestart {
		t.Fatalf("the disconnect hook closed the socket with %d, want 1012", code)
	}
	if subscribers := rig.served.eventRelay.stream.SubscriberCount(); subscribers != 0 {
		t.Fatalf("the relay holds %d subscriptions once the socket closed", subscribers)
	}

	// A page reopening after this close resumes from the last id it applied, not from 0.
	renameIssue(t, rig.handler, issue, "Renamed after disconnect")
	resumed := rig.dial(t, withBearer("agent-token"))
	if start := subscribeEvents(t, resumed, "events", new(int64(2))); start != 2 {
		t.Fatalf("the resumed subscription starts after %d, want 2", start)
	}
	if _, event := nextEvent(t, resumed, "events"); event.ID != 3 || event.Type != "issue.updated" {
		t.Fatalf("resumed replay after the disconnect = %+v, want event 3, the issue.updated", event)
	}
}

// The server-sent event stream and its test hook are gone, test hooks or not: the event feed is the
// live socket's.
func TestTheServerSentEventStreamIsGone(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{testHooks: true}, nil)
	for _, request := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/events"},
		{http.MethodGet, "/api/v1/events?since=0"},
		{http.MethodPost, "/api/v1/events/_test/disconnect"},
	} {
		if response := dispatchRequest(t, rig.handler, request.method, request.target, nil, "alice"); response.Code != http.StatusNotFound {
			t.Errorf("%s %s: status=%d, want 404", request.method, request.target, response.Code)
		}
	}
}

// liveFixtureEvent is the event fixtures/live/event.json's frame carries: a child's status move on
// its parent, as the event log reads it back.
func liveFixtureEvent() model.Event {
	return model.Event{
		ID:        42,
		IssueKey:  new("ACME-1"),
		Project:   "ACME",
		Seq:       7,
		Type:      "child.status",
		Actor:     model.Actor{Kind: "user", ID: "octocat@example.com"},
		Notify:    true,
		CreatedAt: time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC),
		Payload:   json.RawMessage(`{"child_key":"ACME-2","from":"todo","to":"in_progress"}`),
	}
}

// An event frame's event is byte for byte what GET /api/v1/events' data line carried, the event's
// own encoding (json.Marshal of the event the log reads back), so a page reads every event as it
// always has; the socket's writer keeps those bytes; and fixtures/live/event.json, which the
// contracts' and the dashboard's tests read, is that frame.
func TestAnEventFrameCarriesTheSSEBytes(t *testing.T) {
	event := liveFixtureEvent()
	sse, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("encode the event as the event stream did: %v", err)
	}
	frame, err := liveEventFrame("events-1", event)
	if err != nil {
		t.Fatalf("build the event frame: %v", err)
	}
	if frame.Type != "event" || frame.Sub != "events-1" || frame.ID != "42" {
		t.Fatalf("frame = %+v, want events-1's event 42", frame)
	}
	if !bytes.Equal(frame.Event, sse) {
		t.Fatalf("the frame's event\n\t%s\ndiffers from the event stream's data line\n\t%s", frame.Event, sse)
	}
	written, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode the frame as the socket's writer does: %v", err)
	}
	if !bytes.Contains(written, sse) {
		t.Fatalf("the socket writes\n\t%s\nwhich does not carry the event's bytes\n\t%s", written, sse)
	}

	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "live", "event.json"))
	if err != nil {
		t.Fatalf("read the event frame fixture (TestTheLiveFixturesAreWhatTheDiffWrites -update writes it): %v", err)
	}
	var held LiveEventFrame
	if err := json.Unmarshal(fixture, &held); err != nil {
		t.Fatalf("decode the event frame fixture: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, held.Event); err != nil {
		t.Fatalf("compact the fixture's event: %v", err)
	}
	if held.Type != frame.Type || held.Sub != frame.Sub || held.ID != frame.ID || !bytes.Equal(compact.Bytes(), sse) {
		t.Fatalf("fixtures/live/event.json holds %s, want the frame %s", fixture, written)
	}
}
