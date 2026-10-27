package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// senderTestSessions are the live sessions the mention sender's stand-in listener knows, each
// able to receive a steer.
const senderTestSessions = `[{"session_id":"s1","title":"planner","capabilities":["steer"]},` +
	`{"session_id":"s2","title":"reviewer","capabilities":["steer"]},` +
	`{"session_id":"s3","title":"tester","capabilities":["steer"]}]`

// mentionListener is a stand-in Envoy listener for the mention sender's tests. It answers for
// senderTestSessions, resolves role:reviewer to s1, records every send in the order the sends
// arrive, holds a send to a held session until the test releases it, and holds a lookup of a held
// path the same way.
type mentionListener struct {
	mu          sync.Mutex
	sends       []mentionListenerSend
	held        map[string]chan struct{}
	heldLookups map[string]chan struct{}
	arrived     chan string
}

type mentionListenerSend struct {
	session, body string
}

func newMentionListener(t *testing.T) (*httptest.Server, *mentionListener) {
	t.Helper()
	state := &mentionListener{
		held: map[string]chan struct{}{}, heldLookups: map[string]chan struct{}{}, arrived: make(chan string, 64),
	}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			state.mu.Lock()
			gate := state.heldLookups[r.URL.Path]
			state.mu.Unlock()
			if gate != nil {
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			writeSessionRecord(t, w, senderTestSessions, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			_, _ = w.Write([]byte(`{"role":"reviewer","holder":"s1","title":"planner","capabilities":["steer"]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(senderTestSessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession string `json:"target_session"`
				Message       string `json:"message"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode mention send: %v", err)
			}
			state.mu.Lock()
			state.sends = append(state.sends, mentionListenerSend{request.TargetSession, request.Message})
			gate := state.held[request.TargetSession]
			state.mu.Unlock()
			state.arrived <- request.TargetSession
			if gate != nil {
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"` + request.TargetSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	return listener, state
}

// hold makes every send to sessions wait until the returned release is called; release may be
// called more than once.
func (l *mentionListener) hold(sessions ...string) func() {
	gate := make(chan struct{})
	l.mu.Lock()
	for _, session := range sessions {
		l.held[session] = gate
	}
	l.mu.Unlock()
	return sync.OnceFunc(func() { close(gate) })
}

// holdLookups makes every GET of path wait until the returned release is called; release may be
// called more than once.
func (l *mentionListener) holdLookups(path string) func() {
	gate := make(chan struct{})
	l.mu.Lock()
	l.heldLookups[path] = gate
	l.mu.Unlock()
	return sync.OnceFunc(func() { close(gate) })
}

// awaitSend waits for the next send to arrive and returns the session it is for.
func (l *mentionListener) awaitSend(t *testing.T) string {
	t.Helper()
	select {
	case session := <-l.arrived:
		return session
	case <-time.After(10 * time.Second):
		t.Fatal("no mention send reached the listener")
		return ""
	}
}

func (l *mentionListener) sent() []mentionListenerSend {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sends)
}

// unclaimedPending reports whether the comment's attempt for target is pending with no sender's
// claim on it: committed, and not yet taken by any send.
func unclaimedPending(t *testing.T, database *store.Store, commentID, target string) bool {
	t.Helper()
	var unclaimed bool
	if err := database.Pool.QueryRow(context.Background(), `
		select state = 'pending' and claimed_at is null from comment_deliveries
		where comment_id = $1 and target = $2 and attempt = 1
	`, commentID, target).Scan(&unclaimed); err != nil {
		t.Fatalf("read the attempt of %s for %s: %v", commentID, target, err)
	}
	return unclaimed
}

// allSent reports whether every attempt settled sent.
func allSent(attempts []model.CommentDelivery) bool {
	for _, attempt := range attempts {
		if attempt.State != "sent" {
			return false
		}
	}
	return len(attempts) > 0
}

// The listener calls a mention's send makes - the recheck and the send itself, each up to the
// listener client's window - run after the comment's response. A comment mentioning three
// sessions whose sends are all held answers at once, with its attempts pending, and they settle
// once the sends are released.
func TestMentionSendsRunAfterTheCommentHasAnswered(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	release := state.hold("s1", "s2", "s3")
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Mentions after the answer", "before")

	started := time.Now()
	response := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "All three of you.",
		"mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s2"}, {"target": "session:s3"}},
	})
	answered := time.Since(started)
	created := decodeMentionedComment(t, response)
	if answered >= time.Second {
		t.Fatalf("the comment answered after %v with its mention sends held, want under 1s", answered)
	}
	if len(created.Deliveries) != 3 {
		t.Fatalf("the create answered deliveries %#v, want the three attempts", created.Deliveries)
	}
	for _, attempt := range created.Deliveries {
		if attempt.State != "pending" || attempt.Attempt != 1 {
			t.Fatalf("the create answered attempt %#v, want attempt 1 pending", attempt)
		}
	}
	for range 3 {
		state.awaitSend(t)
	}

	release()
	settled := settledCommentDeliveries(t, database, created.ID)
	if len(settled) != 3 || !allSent(settled) {
		t.Fatalf("attempts once the sends were released = %#v, want all three sent", settled)
	}
}

// One recipient gets the comments it is mentioned in in the order they were accepted, also when
// one names it by role and the next by session: both resolve to the same session, and so to the
// same lane. While the first comment's send is held, the second's attempt is not even claimed.
func TestMentionSendsReachOneRecipientInTheOrderItsCommentsWereAccepted(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	release := state.hold("s1")
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Mentions in order", "before")

	first := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "first", "mentions": []map[string]any{{"target": "role:reviewer"}},
	}))
	if session := state.awaitSend(t); session != "s1" {
		t.Fatalf("the first send went to %s, want s1", session)
	}
	second := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "second", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if !unclaimedPending(t, database, second.ID, "session:s1") {
		t.Fatal("the second comment's attempt was taken while the first's send to the same session was still held")
	}

	release()
	settledCommentDeliveries(t, database, first.ID)
	settledCommentDeliveries(t, database, second.ID)
	if sent := state.sent(); len(sent) != 2 || sent[0] != (mentionListenerSend{"s1", "first"}) ||
		sent[1] != (mentionListenerSend{"s1", "second"}) {
		t.Fatalf("sends = %#v, want first then second, both to s1", sent)
	}
}

// A recipient whose send is held - a slow or unanswering session - does not hold back the
// sends to anyone else.
func TestABlockedMentionRecipientDoesNotHoldBackAnother(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	release := state.hold("s1")
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "A slow recipient", "before")

	blocked := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "For the slow one.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	state.awaitSend(t)
	other := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "For the other one.", "mentions": []map[string]any{{"target": "session:s2"}},
	}))
	if settled := waitForCommentDeliveries(t, database, other.ID, "the other recipient's attempt sent", allSent); settled[0].SessionID == nil ||
		*settled[0].SessionID != "s2" {
		t.Fatalf("the other recipient's attempt = %#v, want it sent to s2", settled)
	}
	if attempts := waitForCommentDeliveries(t, database, blocked.ID, "the held attempt", func([]model.CommentDelivery) bool {
		return true
	}); len(attempts) != 1 || attempts[0].State != "pending" {
		t.Fatalf("the held recipient's attempt = %#v, want it still pending behind its held send", attempts)
	}

	release()
	if settled := settledCommentDeliveries(t, database, blocked.ID); !allSent(settled) {
		t.Fatalf("the held recipient's attempt once released = %#v, want it sent", settled)
	}
}

// A comment's mention lookups run at once, so k mentions cost the comment one listener round
// trip rather than k: three lookups of 200 ms each, seen from the listener, all finish within
// 400 ms of the first one starting.
func TestMentionLookupsOfOneCommentRunAtOnce(t *testing.T) {
	const lookup = 200 * time.Millisecond
	var mu sync.Mutex
	looked := map[string]bool{}
	var first, last time.Time
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			session := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
			mu.Lock()
			slow := !looked[session]
			looked[session] = true
			if slow && first.IsZero() {
				first = time.Now()
			}
			mu.Unlock()
			// Only the comment's own lookup of each session is slow; the recheck its send makes
			// later is not this test's.
			if slow {
				time.Sleep(lookup)
			}
			writeSessionRecord(t, w, senderTestSessions, session)
			if slow {
				mu.Lock()
				last = time.Now()
				mu.Unlock()
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Concurrent lookups", "before")

	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "Three lookups.",
		"mentions": []map[string]any{{"target": "session:s1"}, {"target": "session:s2"}, {"target": "session:s3"}},
	}))
	settledCommentDeliveries(t, database, created.ID)
	mu.Lock()
	defer mu.Unlock()
	if span := last.Sub(first); len(looked) != 3 || span >= 2*lookup {
		t.Fatalf("three %v lookups took %v from the first start to the last answer (%d sessions looked up), want under %v",
			lookup, span, len(looked), 2*lookup)
	}
}

// unansweredLookupComment is what one comment whose mention lookup the listener did not answer
// in time came to: its comment.created payload, every send the listener took, and the topics the
// outbox published comment.created to.
type unansweredLookupComment struct {
	payload      model.CommentEventPayload
	sends        []mentionListenerSend
	destinations []string
}

// commentWithAnUnansweredLookup posts a comment mentioning mention on an issue whose route is
// route, while the listener holds every lookup of hungPath unanswered. It checks that the comment
// answers within mentionLookupTimeout's bound with that mention unresolved and its attempt
// pending, then lets the listener answer, waits for the mention sender to send the attempt to
// s1, and runs the outbox until it has published comment.created. The listener resolves every
// session of senderTestSessions, and role:reviewer to s1.
func commentWithAnUnansweredLookup(t *testing.T, route, mention, hungPath string) unansweredLookupComment {
	t.Helper()
	listener, state := newMentionListener(t)
	letAnswer := state.holdLookups(hungPath)
	defer letAnswer()
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "TEST", "An unanswered lookup", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": route,
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	started := time.Now()
	response := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Are you there?", "mentions": []map[string]any{{"target": mention}},
	})
	answered := time.Since(started)
	created := decodeMentionedComment(t, response)
	if bound := mentionLookupTimeout + 500*time.Millisecond; answered >= bound {
		t.Fatalf("the comment answered after %v with its lookup unanswered, want under %v", answered, bound)
	}
	if len(created.Mentions) != 1 || created.Mentions[0].SessionID != nil {
		t.Fatalf("mentions = %#v, want the mention unresolved", created.Mentions)
	}
	if len(created.Deliveries) != 1 || created.Deliveries[0].State != "pending" ||
		created.Deliveries[0].SessionID != nil || created.Deliveries[0].Error != nil {
		t.Fatalf("deliveries = %#v, want one pending attempt with no session and no failure", created.Deliveries)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	var payload *model.CommentEventPayload
	for _, event := range decodeBody[[]struct {
		Type    string                    `json:"type"`
		Payload model.CommentEventPayload `json:"payload"`
	}](t, events) {
		if event.Type == "comment.created" && event.Payload.ID == created.ID {
			payload = &event.Payload
		}
	}
	if payload == nil {
		t.Fatalf("no comment.created event for %s: %s", created.ID, events.Body.String())
	}
	if len(payload.SuppressedAuthors) != 0 || len(payload.Mentions) != 1 || payload.Mentions[0].SessionID != nil {
		t.Fatalf("comment.created suppresses authors %v with mentions %#v, want no author suppressed for the unresolved mention",
			payload.SuppressedAuthors, payload.Mentions)
	}

	letAnswer()
	settled := settledCommentDeliveries(t, database, created.ID)
	if len(settled) != 1 || settled[0].State != "sent" || settled[0].SessionID == nil || *settled[0].SessionID != "s1" {
		t.Fatalf("the mention once the listener answered = %#v, want it sent to s1", settled)
	}
	published := eventDeliveries(t, database, deps.Docs, "comment.created", created.ID)
	if len(published) != 1 {
		t.Fatalf("comment.created deliveries = %#v, want the one event's", published)
	}
	return unansweredLookupComment{payload: *payload, sends: state.sent(), destinations: published[0].Destinations}
}

// A listener whose mention lookup never answers delays the comment by at most
// mentionLookupTimeout, and the mention sender delivers the mention after the response once the
// listener answers. When the issue's route is the very string the comment mentions, both name
// one session, so the event suppresses the route by that string and the session hears the
// comment once, from the mention.
func TestAnUnansweredMentionOfTheRouteItselfSuppressesTheRouteCopy(t *testing.T) {
	got := commentWithAnUnansweredLookup(t, "session:s1", "session:s1", "/v1/registry/s1")
	if !got.payload.SuppressRoute || got.payload.SuppressedRoute != "session:s1" ||
		got.payload.SuppressedRouteSessionID != nil {
		t.Fatalf("comment.created suppresses route %v (%q, session %v), want session:s1 suppressed by its string",
			got.payload.SuppressRoute, got.payload.SuppressedRoute, got.payload.SuppressedRouteSessionID)
	}
	if len(got.sends) != 1 || got.sends[0].session != "s1" {
		t.Fatalf("the listener took sends %#v, want the one mention's, to s1", got.sends)
	}
	if slices.Contains(got.destinations, contracts.AgentSubject("s1")) {
		t.Fatalf("the outbox published comment.created to %v, want s1's topic left out: the mention delivered it",
			got.destinations)
	}
}

// A route that names the mentioned session by a different string - here session:s1, while the
// comment mentions role:reviewer, which s1 holds - is not suppressed when the mention's lookup
// goes unanswered: no session is known to compare. s1 receives the comment twice, once from the
// mention sender and once as the route copy on its own topic, under different dedupe keys.
func TestAnUnansweredMentionOfAnotherStringForTheRouteLeavesTwoCopies(t *testing.T) {
	got := commentWithAnUnansweredLookup(t, "session:s1", "role:reviewer", "/v1/roles/reviewer")
	if got.payload.SuppressRoute || got.payload.SuppressedRoute != "" || got.payload.SuppressedRouteSessionID != nil {
		t.Fatalf("comment.created suppresses route %v (%q, session %v), want no route suppressed",
			got.payload.SuppressRoute, got.payload.SuppressedRoute, got.payload.SuppressedRouteSessionID)
	}
	if len(got.sends) != 1 || got.sends[0].session != "s1" {
		t.Fatalf("the listener took sends %#v, want the one mention's, to s1", got.sends)
	}
	if !slices.Contains(got.destinations, contracts.AgentSubject("s1")) {
		t.Fatalf("the outbox published comment.created to %v, want s1's topic among them as well: s1 gets two copies",
			got.destinations)
	}
}

// A listener that answers no lookup at all delays a mentioned comment by at most
// mentionLookupTimeout, also on an issue whose route is not one of the comment's mentions: the
// route's own lookup runs beside the mentions' lookups, not after them, so the two timeouts do not
// add up.
func TestAListenerThatAnswersNoLookupDelaysARoutedCommentByOneTimeoutAtMost(t *testing.T) {
	listener, state := newMentionListener(t)
	letMentionAnswer := state.holdLookups("/v1/registry/s1")
	defer letMentionAnswer()
	letRouteAnswer := state.holdLookups("/v1/registry/s2")
	defer letRouteAnswer()
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "TEST", "A silent listener", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "session:s2",
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	started := time.Now()
	response := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Is anyone there?", "mentions": []map[string]any{{"target": "session:s1"}},
	})
	answered := time.Since(started)
	created := decodeMentionedComment(t, response)
	if answered >= 2*mentionLookupTimeout {
		t.Fatalf("the comment answered after %v with no lookup answered, want under %v: the route's lookup ran after the mention's",
			answered, 2*mentionLookupTimeout)
	}

	letMentionAnswer()
	letRouteAnswer()
	settledCommentDeliveries(t, database, created.ID)
}

// A process that stops with mention sends still queued leaves their attempts pending with no
// claim. The next process's recovery pass sends such an attempt once, and leaves alone one whose
// claim is live, which another process's sender holds.
func TestMentionRecoverySendsAStrandedAttemptOnceAndLeavesALiveClaimAlone(t *testing.T) {
	listener, state := newMentionListener(t)
	stopping, stop := context.WithCancel(t.Context())
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, lifetime: stopping})
	issue := createInteractionIssue(t, handler, "TEST", "Recovered mentions", "before")

	// The process is stopping: the comments commit, and no send of theirs starts.
	stop()
	stranded := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Accepted while stopping.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	held := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Held by another sender.", "mentions": []map[string]any{{"target": "session:s2"}},
	}))
	if !unclaimedPending(t, database, stranded.ID, "session:s1") || !unclaimedPending(t, database, held.ID, "session:s2") {
		t.Fatal("a comment accepted while the process stopped had its attempt taken")
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set claimed_at = now() where comment_id = $1
	`, held.ID); err != nil {
		t.Fatalf("claim the held attempt as another live sender: %v", err)
	}

	next, stopNext := context.WithCancel(t.Context())
	nextDeps := deps
	nextDeps.Lifetime = next
	restarted := register(http.NewServeMux(), nextDeps)
	settled := waitForCommentDeliveries(t, database, stranded.ID, "the stranded attempt sent", allSent)
	stopNext()
	restarted.waitBackground()

	if settled[0].SessionID == nil || *settled[0].SessionID != "s1" {
		t.Fatalf("the recovered attempt = %#v, want it sent to s1", settled)
	}
	if attempts := waitForCommentDeliveries(t, database, held.ID, "the held attempt", func([]model.CommentDelivery) bool {
		return true
	}); len(attempts) != 1 || attempts[0].State != "pending" {
		t.Fatalf("the attempt another sender holds = %#v, want it left pending", attempts)
	}
	if sent := state.sent(); len(sent) != 1 || sent[0].session != "s1" {
		t.Fatalf("sends = %#v, want the stranded attempt's one send to s1", sent)
	}
}

// A mention past the sender's queue bound is not queued: its attempt stays pending and
// unclaimed, and the recovery pass the sender runs once its queue has drained sends it, once.
func TestAMentionPastTheQueueBoundIsSentOnceTheQueueDrains(t *testing.T) {
	listener, state := newMentionListener(t)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	served.mentions.mu.Lock()
	served.mentions.capacity = 1
	served.mentions.mu.Unlock()
	release := state.hold("s1")
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "A full queue", "before")
	post := func(body string) string {
		return decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
			"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
		})).ID
	}

	sending := post("in flight")
	state.awaitSend(t)
	queued := post("queued")
	refused := post("past the bound")
	if !unclaimedPending(t, database, queued, "session:s1") || !unclaimedPending(t, database, refused, "session:s1") {
		t.Fatal("an attempt behind the held send was taken")
	}

	release()
	for _, comment := range []string{sending, queued, refused} {
		if settled := settledCommentDeliveries(t, database, comment); !allSent(settled) {
			t.Fatalf("comment %s attempts = %#v, want it sent", comment, settled)
		}
	}
	if sent := state.sent(); len(sent) != 3 || sent[0].body != "in flight" || sent[1].body != "queued" ||
		sent[2].body != "past the bound" {
		t.Fatalf("sends = %#v, want each comment sent once, in order", sent)
	}
}
