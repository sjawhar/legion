package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// acceptedAttempt is one delivery attempt as the accept route and every message read answer it;
// the accept alone adds the message's stored body, which is what the session injects.
type acceptedAttempt struct {
	Attempt     int          `json:"attempt"`
	Delivery    string       `json:"delivery"`
	SessionID   string       `json:"session_id"`
	State       string       `json:"state"`
	RequestedBy *model.Actor `json:"requested_by"`
	AcceptedAt  *time.Time   `json:"accepted_at"`
	AcceptedAs  *string      `json:"accepted_as"`
	Body        string       `json:"body"`
}

// acceptDelivery is the session's one-shot claim that it took attempt `attempt` of a message as
// its user's own turn, the call pi-envoy makes before it injects anything.
func acceptDelivery(t *testing.T, handler http.Handler, messageID string, attempt int, session string) *httptest.ResponseRecorder {
	t.Helper()
	return bearerRequest(t, handler, http.MethodPost, fmt.Sprintf("/api/v1/messages/%s/deliveries/%d/accept", messageID, attempt), map[string]any{
		"actor": map[string]any{"kind": "session", "id": session},
	})
}

func responseCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	return body.Code
}

// acceptedEvents is every message.accepted event about a message, oldest first, as the event
// log holds it.
func acceptedEvents(t *testing.T, database *store.Store, messageID string) []map[string]any {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select payload, actor from events
		where type = 'message.accepted' and payload ->> 'message_id' = $1
		order by id
	`, messageID)
	if err != nil {
		t.Fatalf("read message.accepted events: %v", err)
	}
	defer rows.Close()
	events := []map[string]any{}
	for rows.Next() {
		var payload, actor []byte
		if err := rows.Scan(&payload, &actor); err != nil {
			t.Fatalf("scan message.accepted event: %v", err)
		}
		event := map[string]any{}
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode message.accepted payload: %v", err)
		}
		var by map[string]any
		if err := json.Unmarshal(actor, &by); err != nil {
			t.Fatalf("decode message.accepted actor: %v", err)
		}
		event["actor"] = by
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read message.accepted events: %v", err)
	}
	return events
}

// requester is who a stored attempt records as having asked for it, or nil for an attempt that
// records nobody.
func requester(t *testing.T, database *store.Store, messageID string, attempt int) *model.Actor {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select requested_by from message_deliveries where message_id = $1 and attempt = $2
	`, messageID, attempt).Scan(&raw); err != nil {
		t.Fatalf("read attempt %d's requester: %v", attempt, err)
	}
	if raw == nil {
		return nil
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		t.Fatalf("decode attempt %d's requester %s: %v", attempt, raw, err)
	}
	return &actor
}

func retryDelivery(t *testing.T, handler http.Handler, messageID, delivery string, login string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+messageID+"/deliveries", map[string]any{"delivery": delivery}, login)
}

func bearerRetryDelivery(t *testing.T, handler http.Handler, messageID, delivery, session string) *httptest.ResponseRecorder {
	t.Helper()
	return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+messageID+"/deliveries", map[string]any{
		"delivery": delivery, "actor": map[string]any{"kind": "session", "id": session},
	})
}

// A person's direct message is taken as the session's own turn at most once, and only by the
// session its attempt went to: the accept records it on the attempt, every read of the thread
// then says so, and one message.accepted event tells the page, owned by the conversation.
func TestAcceptRecordsAPersonsFreshAttemptAsTheSessionsTurnOnce(t *testing.T) {
	handler, database, root, _, _ := directConversationFrom(t, "alice")

	accepted := acceptDelivery(t, handler, root.ID, 1, "s1")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	attempt := decodeBody[acceptedAttempt](t, accepted)
	if attempt.AcceptedAs == nil || *attempt.AcceptedAs != "user_turn" || attempt.AcceptedAt == nil {
		t.Fatalf("accepted attempt = %+v, want accepted_as user_turn with accepted_at set", attempt)
	}
	// The accept never settles the attempt: that is the send's own receipt.
	if attempt.Attempt != 1 || attempt.SessionID != "s1" || attempt.State != "sent" {
		t.Fatalf("accepted attempt = %+v, want attempt 1 to s1 still sent", attempt)
	}
	if attempt.RequestedBy == nil || attempt.RequestedBy.Kind != "user" || attempt.RequestedBy.ID != "alice" {
		t.Fatalf("accepted attempt's requested_by = %+v, want alice", attempt.RequestedBy)
	}
	// The session injects what Dispatch stored and the mode it stored, never what a frame says.
	if attempt.Body != "Where is the dashboard?" || attempt.Delivery != "aside" {
		t.Fatalf("accepted attempt = %+v, want the stored body sent as an aside", attempt)
	}

	read := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/"+root.ID+"?session=s1", nil)
	var thread struct {
		Message struct {
			Deliveries []acceptedAttempt `json:"deliveries"`
		} `json:"message"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &thread); err != nil || len(thread.Message.Deliveries) != 1 {
		t.Fatalf("thread read: status=%d body=%s", read.Code, read.Body.String())
	}
	if got := thread.Message.Deliveries[0]; got.AcceptedAs == nil || *got.AcceptedAs != "user_turn" {
		t.Fatalf("the thread reads attempt 1 as %+v, want it accepted as the session's turn", got)
	}

	events := acceptedEvents(t, database, root.ID)
	if len(events) != 1 {
		t.Fatalf("message.accepted events = %v, want one", events)
	}
	want := map[string]any{
		"message_id": root.ID, "attempt": float64(1), "session_id": "s1",
		"accepted_as": "user_turn", "target": "session:s1",
		"actor": map[string]any{"kind": "session", "id": "s1"},
	}
	if fmt.Sprint(events[0]) != fmt.Sprint(want) {
		t.Fatalf("message.accepted = %v, want %v", events[0], want)
	}

	again := acceptDelivery(t, handler, root.ID, 1, "s1")
	if again.Code != http.StatusConflict || responseCode(t, again) != "ACCEPT_ALREADY_ACCEPTED" {
		t.Fatalf("second accept: status=%d body=%s, want 409 ACCEPT_ALREADY_ACCEPTED", again.Code, again.Body.String())
	}
	if len(acceptedEvents(t, database, root.ID)) != 1 {
		t.Fatal("a refused accept appended a message.accepted event")
	}
}

// Every attempt that is not a person's fresh, latest, never-accepted Send or Aside of a message
// that person wrote, to this session, is refused, naming the check it failed, and nothing is
// recorded. The bearer's retry and the stale attempt are the two ways a session could otherwise
// make a person's earlier message a fresh turn in another session: a retry any bearer can
// request, and a frame forged later naming an attempt a person asked for long ago. The session's
// own reply a person retried, a person's BTW retry and a failed attempt are each a message pi-envoy
// never takes as a turn, so the page must never say one reached the conversation. Another
// person's retry is refused too: only the author's own Send or Aside becomes their turn.
func TestAcceptRefusesEveryAttemptThatIsNotAPersonsFreshLatestDeliveryToThisSession(t *testing.T) {
	for _, test := range []struct {
		name string
		// prepare returns the message and the attempt of it that `session` then accepts.
		prepare func(t *testing.T, handler http.Handler, database *store.Store, root model.Message,
			reply func(string) *httptest.ResponseRecorder) (string, int)
		session string
		status  int
		code    string
	}{
		{
			name: "another session's attempt",
			prepare: func(_ *testing.T, _ http.Handler, _ *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				return root.ID, 1
			},
			session: "s2", status: http.StatusForbidden, code: "ACCEPT_FORBIDDEN",
		},
		{
			name: "an attempt that does not exist",
			prepare: func(_ *testing.T, _ http.Handler, _ *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				return root.ID, 9
			},
			session: "s1", status: http.StatusNotFound, code: "MESSAGE_NOT_FOUND",
		},
		{
			name: "the session's own reply, which a person retried",
			prepare: func(t *testing.T, handler http.Handler, _ *store.Store, _ model.Message, reply func(string) *httptest.ResponseRecorder) (string, int) {
				answered := reply("On it.")
				if answered.Code != http.StatusCreated {
					t.Fatalf("session's reply: status=%d body=%s", answered.Code, answered.Body.String())
				}
				own := decodeBody[model.Message](t, answered)
				if retry := retryDelivery(t, handler, own.ID, "steer", "alice"); retry.Code != http.StatusCreated {
					t.Fatalf("person's retry of the session's reply: status=%d body=%s", retry.Code, retry.Body.String())
				}
				return own.ID, 1
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_NOT_WRITTEN_BY_PERSON",
		},
		{
			name: "a person's BTW retry",
			prepare: func(t *testing.T, handler http.Handler, _ *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				if retry := retryDelivery(t, handler, root.ID, "btw", "alice"); retry.Code != http.StatusCreated {
					t.Fatalf("person's BTW retry: status=%d body=%s", retry.Code, retry.Body.String())
				}
				return root.ID, 2
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_NOT_ASIDE_OR_STEER",
		},
		{
			name: "an attempt the session reported failed",
			prepare: func(t *testing.T, handler http.Handler, _ *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				failed := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply", map[string]any{
					"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "error": "the host refused the message",
				})
				if failed.Code != http.StatusOK || decodeBody[acceptedAttempt](t, failed).State != "failed" {
					t.Fatalf("session's error report: status=%d body=%s", failed.Code, failed.Body.String())
				}
				return root.ID, 1
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_FAILED",
		},
		{
			name: "an attempt a later one superseded",
			prepare: func(t *testing.T, handler http.Handler, _ *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				if retry := retryDelivery(t, handler, root.ID, "steer", "alice"); retry.Code != http.StatusCreated {
					t.Fatalf("person's retry: status=%d body=%s", retry.Code, retry.Body.String())
				}
				return root.ID, 1
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_SUPERSEDED",
		},
		{
			name: "a message the session already took",
			prepare: func(t *testing.T, handler http.Handler, _ *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				if accepted := acceptDelivery(t, handler, root.ID, 1, "s1"); accepted.Code != http.StatusOK {
					t.Fatalf("first accept: status=%d body=%s", accepted.Code, accepted.Body.String())
				}
				if retry := retryDelivery(t, handler, root.ID, "steer", "alice"); retry.Code != http.StatusCreated {
					t.Fatalf("person's retry: status=%d body=%s", retry.Code, retry.Body.String())
				}
				return root.ID, 2
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_ALREADY_ACCEPTED",
		},
		{
			name: "a retry a session requested",
			prepare: func(t *testing.T, handler http.Handler, _ *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				if retry := bearerRetryDelivery(t, handler, root.ID, "steer", "s-attacker"); retry.Code != http.StatusCreated {
					t.Fatalf("bearer retry: status=%d body=%s", retry.Code, retry.Body.String())
				}
				return root.ID, 2
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_NOT_REQUESTED_BY_PERSON",
		},
		{
			name: "another person's retry",
			prepare: func(t *testing.T, handler http.Handler, _ *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				if retry := retryDelivery(t, handler, root.ID, "steer", "bob"); retry.Code != http.StatusCreated {
					t.Fatalf("bob's retry of alice's message: status=%d body=%s", retry.Code, retry.Body.String())
				}
				return root.ID, 2
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_NOT_REQUESTED_BY_AUTHOR",
		},
		{
			name: "an attempt that records no requester",
			prepare: func(t *testing.T, _ http.Handler, database *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				if _, err := database.Pool.Exec(context.Background(), `
					update message_deliveries set requested_by = null where message_id = $1 and attempt = 1
				`, root.ID); err != nil {
					t.Fatalf("clear the requester: %v", err)
				}
				return root.ID, 1
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_NOT_REQUESTED_BY_PERSON",
		},
		{
			name: "an attempt older than a minute",
			prepare: func(t *testing.T, _ http.Handler, database *store.Store, root model.Message, _ func(string) *httptest.ResponseRecorder) (string, int) {
				if _, err := database.Pool.Exec(context.Background(), `
					update message_deliveries set created_at = now() - interval '61 seconds'
					where message_id = $1 and attempt = 1
				`, root.ID); err != nil {
					t.Fatalf("age the attempt: %v", err)
				}
				return root.ID, 1
			},
			session: "s1", status: http.StatusConflict, code: "ACCEPT_STALE",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, database, root, reply, _ := directConversationFrom(t, "alice")
			messageID, attempt := test.prepare(t, handler, database, root, reply)
			before := len(acceptedEvents(t, database, messageID))

			refused := acceptDelivery(t, handler, messageID, attempt, test.session)
			if refused.Code != test.status || responseCode(t, refused) != test.code {
				t.Fatalf("accept: status=%d body=%s, want %d %s", refused.Code, refused.Body.String(), test.status, test.code)
			}
			assertNothingAccepted(t, database, messageID, attempt, before)
		})
	}
}

// A person's own retry of their direct message is still their turn when their name reaches
// Dispatch cased differently from the first send: an email names one person however it is cased,
// as canonicalLogin compares it, so the accept's check that the requester is the message's author
// compares people the same way.
func TestAcceptTakesTheAuthorsOwnRetryHoweverTheirLoginIsCased(t *testing.T) {
	handler, database, root, _, _ := directConversationFrom(t, "Alice")
	if retry := retryDelivery(t, handler, root.ID, "steer", "alice"); retry.Code != http.StatusCreated {
		t.Fatalf("alice's retry: status=%d body=%s", retry.Code, retry.Body.String())
	}

	accepted := acceptDelivery(t, handler, root.ID, 2, "s1")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s, want 200", accepted.Code, accepted.Body.String())
	}
	if got := decodeBody[acceptedAttempt](t, accepted); got.AcceptedAs == nil || *got.AcceptedAs != "user_turn" || got.Attempt != 2 {
		t.Fatalf("accepted = %#v, want attempt 2 accepted as user_turn", got)
	}
	if len(acceptedEvents(t, database, root.ID)) != 1 {
		t.Fatalf("message.accepted events = %d, want 1", len(acceptedEvents(t, database, root.ID)))
	}
}

// A person's Send to a session Dispatch finds not live is recorded as failed: no frame of it ever
// left. A frame forged inside the minute naming that attempt must not make it the session's turn,
// or the person, shown "Failed", may already have sent it another way (Deep's first round-2
// construction).
func TestAcceptRefusesASendDispatchCouldNotDeliver(t *testing.T) {
	live := false
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Restart the deploy.", "delivery": "steer",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "failed" || len(sent) != 0 {
		t.Fatalf("the Send to a session that is not live = %+v with %d sends, want one failed attempt and none sent", message.Deliveries, len(sent))
	}
	live = true

	refused := acceptDelivery(t, handler, message.ID, 1, "s1")
	if refused.Code != http.StatusConflict || responseCode(t, refused) != "ACCEPT_FAILED" {
		t.Fatalf("accept: status=%d body=%s, want 409 ACCEPT_FAILED", refused.Code, refused.Body.String())
	}
	assertNothingAccepted(t, database, message.ID, 1, 0)
}

// assertNothingAccepted fails t when a refused accept of attempt `attempt` recorded anything: an
// acceptance on the attempt, or a message.accepted event beyond the `before` already logged.
func assertNothingAccepted(t *testing.T, database *store.Store, messageID string, attempt, before int) {
	t.Helper()
	if got := len(acceptedEvents(t, database, messageID)); got != before {
		t.Fatalf("a refused accept left %d message.accepted events, want %d", got, before)
	}
	var accepted int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from message_deliveries
		where message_id = $1 and attempt = $2 and (accepted_at is not null or accepted_as is not null)
	`, messageID, attempt).Scan(&accepted); err != nil {
		t.Fatalf("read the refused attempt: %v", err)
	}
	if accepted != 0 {
		t.Fatal("a refused accept recorded the attempt as accepted")
	}
}

// Only a person's direct message from the Agents page is ever taken as a session's own turn, so
// only one can be accepted as that: a message on an issue, a broadcast's copy, a reply in a
// broadcast's thread, and an attempt for a session the thread's root does not target are each
// refused, although a person asked for each within the minute and it is the latest attempt,
// never accepted. Otherwise a bearer could have the page say one of them reached the
// conversation.
func TestAcceptRefusesAMessageThatIsNotAPersonsDirectMessageToTheSession(t *testing.T) {
	broadcastCopy := func(t *testing.T) (http.Handler, *store.Store, string) {
		t.Helper()
		listener, _ := newBroadcastListener(t, broadcastSessions)
		handler, database := newTargetedMessageHandler(t, listener.URL)
		created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
			"body": "Status?", "delivery": "steer", "session_ids": []string{"planner"},
		}, "alice"))
		return handler, database, awaitBroadcastDeliveries(t, handler, created.ID).Recipients[0].Message.ID
	}
	for _, test := range []struct {
		name string
		// message is the message whose attempt 1 `session` then accepts.
		message func(t *testing.T) (http.Handler, *store.Store, string)
		session string
	}{
		{
			name: "a message on an issue",
			message: func(t *testing.T) (http.Handler, *store.Store, string) {
				live := true
				sent := []map[string]any{}
				listener := sessionListener(t, &live, &sent)
				t.Cleanup(listener.Close)
				handler, database := newTargetedMessageHandler(t, listener.URL)
				issue := createInteractionIssue(t, handler, "TEST", "Not a direct message", "before")
				return handler, database, createIssueMessage(t, handler, issue.Key, map[string]any{
					"body": "Can this ship?", "target": "session:s1", "delivery": "steer",
				}, "alice").ID
			},
			session: "s1",
		},
		{name: "a broadcast's copy", message: broadcastCopy, session: "planner"},
		{
			name: "a person's reply in a broadcast's thread",
			message: func(t *testing.T) (http.Handler, *store.Store, string) {
				handler, database, copyID := broadcastCopy(t)
				reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/planner/messages", map[string]any{
					"body": "And the logs?", "delivery": "steer", "in_reply_to": copyID,
				}, "alice")
				if reply.Code != http.StatusCreated {
					t.Fatalf("person's reply in the broadcast's thread: status=%d body=%s", reply.Code, reply.Body.String())
				}
				return handler, database, decodeBody[model.Message](t, reply).ID
			},
			session: "planner",
		},
		{
			name: "an attempt for a session the thread's root does not target",
			message: func(t *testing.T) (http.Handler, *store.Store, string) {
				handler, database, root, _, _ := directConversationFrom(t, "alice")
				if _, err := database.Pool.Exec(context.Background(), `
					update message_deliveries set session_id = 's2' where message_id = $1 and attempt = 1
				`, root.ID); err != nil {
					t.Fatalf("re-point the attempt: %v", err)
				}
				return handler, database, root.ID
			},
			session: "s2",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, database, messageID := test.message(t)
			if who := requester(t, database, messageID, 1); who == nil || who.Kind != "user" {
				t.Fatalf("attempt 1 records requested_by %+v, want the person who sent it", who)
			}
			refused := acceptDelivery(t, handler, messageID, 1, test.session)
			if refused.Code != http.StatusConflict || responseCode(t, refused) != "ACCEPT_NOT_DIRECT" {
				t.Fatalf("accept: status=%d body=%s, want 409 ACCEPT_NOT_DIRECT", refused.Code, refused.Body.String())
			}
			assertNothingAccepted(t, database, messageID, 1, 0)
		})
	}
}

// A replayed frame can reach the session twice at once; both reads pass, both accept, and only
// one may inject. The two race on the message's row lock and exactly one wins.
func TestConcurrentAcceptsOfOneAttemptLetExactlyOneWin(t *testing.T) {
	handler, database, root, _, _ := directConversationFrom(t, "alice")
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select 1 from messages where id = $1 for update`, root.ID); err != nil {
		t.Fatalf("hold the message row: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { responses <- acceptDelivery(t, handler, root.ID, 1, "s1") }()
	}
	waitForDatabaseLocks(t, holder, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the message row: %v", err)
	}
	outcomes := []string{}
	for range 2 {
		response := awaitResponse(t, responses)
		outcomes = append(outcomes, fmt.Sprintf("%d %s", response.Code, responseCode(t, response)))
	}
	sort.Strings(outcomes)
	if want := []string{"200 ", "409 ACCEPT_ALREADY_ACCEPTED"}; strings.Join(outcomes, ",") != strings.Join(want, ",") {
		t.Fatalf("two concurrent accepts answered %v, want %v", outcomes, want)
	}
	if got := len(acceptedEvents(t, database, root.ID)); got != 1 {
		t.Fatalf("message.accepted events = %d, want one", got)
	}
}

// The accept, a claim of the message's next attempt and the session's reply to the same attempt
// take the message row and the attempt row in two orders. The accept and the claim take the
// message row and then the attempt row; the reply takes the attempt row and then, through its
// insert's foreign key, the message row FOR KEY SHARE. So the accept and the claim take the message
// row FOR NO KEY UPDATE, which that key share does not wait for: whichever of the two holds the
// message row while the reply holds the attempt, all three land and none deadlocks.
func TestAcceptAClaimAndAReplyOfOneAttemptAllLand(t *testing.T) {
	for _, test := range []struct {
		// first takes the message row while the reply waits for the attempt; second queues behind it.
		first, second string
		// want is each call's status once the three have run.
		want map[string]string
	}{
		{first: "accept", second: "claim", want: map[string]string{"accept": "200 ", "claim": "201 ", "reply": "201 "}},
		// The claim opens attempt 2 before the accept reads, so attempt 1 is no longer the latest.
		{first: "claim", second: "accept", want: map[string]string{"accept": "409 ACCEPT_SUPERSEDED", "claim": "201 ", "reply": "201 "}},
	} {
		t.Run("the "+test.first+" holds the message row", func(t *testing.T) {
			handler, database, root, reply, _ := directConversationFrom(t, "alice")
			ctx := context.Background()
			// The send that carried attempt 1 is still out, so a claim locks the attempt too.
			if _, err := database.Pool.Exec(ctx, `
				update message_deliveries set state = 'pending' where message_id = $1 and attempt = 1
			`, root.ID); err != nil {
				t.Fatalf("hold attempt 1 in its send: %v", err)
			}
			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin holder: %v", err)
			}
			defer holder.Rollback(ctx)
			if _, err := holder.Exec(ctx, `
				select 1 from message_deliveries where message_id = $1 and attempt = 1 for update
			`, root.ID); err != nil {
				t.Fatalf("hold the attempt row: %v", err)
			}
			type outcome struct {
				call     string
				response *httptest.ResponseRecorder
			}
			outcomes := make(chan outcome, 3)
			calls := map[string]func() *httptest.ResponseRecorder{
				"reply":  func() *httptest.ResponseRecorder { return reply("On it.") },
				"accept": func() *httptest.ResponseRecorder { return acceptDelivery(t, handler, root.ID, 1, "s1") },
				"claim":  func() *httptest.ResponseRecorder { return retryDelivery(t, handler, root.ID, "steer", "alice") },
			}
			// The reply queues on the attempt row first, so it takes that row before either other
			// call does, and then needs the message row the first of them holds.
			for queued, call := range []string{"reply", test.first, test.second} {
				go func() { outcomes <- outcome{call: call, response: calls[call]()} }()
				waitForDatabaseLocks(t, holder, queued+1)
			}
			if err := holder.Rollback(ctx); err != nil {
				t.Fatalf("release the attempt row: %v", err)
			}
			got := map[string]string{}
			for range 3 {
				select {
				case landed := <-outcomes:
					got[landed.call] = fmt.Sprintf("%d %s", landed.response.Code, responseCode(t, landed.response))
					if landed.response.Code >= http.StatusInternalServerError {
						t.Errorf("%s: status=%d body=%s", landed.call, landed.response.Code, landed.response.Body.String())
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("the three calls did not all return; returned so far %v", got)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("outcomes = %v, want %v", got, test.want)
			}
		})
	}
}

// pi-envoy acknowledges a frame before Dispatch settles the attempt, so its accept can land while
// the send that carried the frame is still out. The accept takes the pending attempt without
// touching what the settle is scoped to, so the settle still records the send and appends its
// receipt, and the accept's own event is appended for real under the conversation's owner.
func TestAcceptDuringTheSendLeavesTheSettleItsReceipt(t *testing.T) {
	var handler http.Handler
	accepts := []acceptedAttempt{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				Payload string `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode targeted send: %v", err)
			}
			var frame struct {
				Event struct {
					Payload struct {
						ID string `json:"id"`
					} `json:"payload"`
				} `json:"event"`
				Delivery struct {
					Attempt int `json:"attempt"`
				} `json:"delivery"`
			}
			if err := json.Unmarshal([]byte(request.Payload), &frame); err != nil {
				t.Errorf("decode targeted delivery frame: %v", err)
			}
			// The session takes the frame it was just handed, before this send returns.
			accepted := acceptDelivery(t, handler, frame.Event.Payload.ID, frame.Delivery.Attempt, "s1")
			if accepted.Code != http.StatusOK {
				t.Errorf("accept from inside the send: status=%d body=%s", accepted.Code, accepted.Body.String())
			} else {
				accepts = append(accepts, decodeBody[acceptedAttempt](t, accepted))
			}
			_, _ = w.Write([]byte(`{"event_id":"target-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	var database *store.Store
	handler, database = newTargetedMessageHandler(t, listener.URL)

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Take this as typed.", "delivery": "steer",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if len(accepts) != 1 || accepts[0].State != "pending" {
		t.Fatalf("accepts from inside the send = %+v, want one, taken while the attempt was pending", accepts)
	}
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "sent" {
		t.Fatalf("the send settled its attempt as %+v, want sent", message.Deliveries)
	}
	settled := readMessageDelivery(t, database, message.ID, 1)
	if settled.State != "sent" || settled.EnvelopeID == nil || *settled.EnvelopeID != "target-envelope" {
		t.Fatalf("attempt 1 after the settle = %+v, want sent with the listener's envelope", settled)
	}
	var acceptedAs *string
	if err := database.Pool.QueryRow(context.Background(), `
		select accepted_as from message_deliveries where message_id = $1 and attempt = 1
	`, message.ID).Scan(&acceptedAs); err != nil {
		t.Fatalf("read attempt 1's acceptance: %v", err)
	}
	if acceptedAs == nil || *acceptedAs != "user_turn" {
		t.Fatalf("attempt 1's accepted_as after the settle = %v, want user_turn", acceptedAs)
	}
	if receipts := messageDeliveryReceiptRows(t, database, message.ID); len(receipts) != 1 || receipts[0] != (deliveryReceipt{Attempt: 1, State: "sent"}) {
		t.Fatalf("message.delivery receipts = %+v, want the one the settle owes attempt 1", receipts)
	}
	if got := len(acceptedEvents(t, database, message.ID)); got != 1 {
		t.Fatalf("message.accepted events = %d, want one", got)
	}
}

// Whether a person asked for an attempt is recorded where the attempt is opened, from the actor
// the send already carries: a direct message, a retry by a person or by a session, a person's
// reply in a direct thread or in an issue thread it inherits its delivery from, a retry in a new
// mode that supersedes a stranded attempt, and a broadcast. A resumed attempt keeps the person
// who first asked for it, whoever resumes it.
func TestEveryPathThatOpensAnAttemptRecordsWhoAskedForIt(t *testing.T) {
	alice := &model.Actor{Kind: "user", ID: "alice"}
	bob := &model.Actor{Kind: "user", ID: "bob"}
	session := &model.Actor{Kind: "session", ID: "s2"}
	assertRequester := func(t *testing.T, database *store.Store, messageID string, attempt int, want *model.Actor) {
		t.Helper()
		got := requester(t, database, messageID, attempt)
		if (got == nil) != (want == nil) || (got != nil && (got.Kind != want.Kind || got.ID != want.ID)) {
			t.Fatalf("attempt %d of %s records requested_by %+v, want %+v", attempt, messageID, got, want)
		}
	}

	t.Run("direct message, retries and a direct reply", func(t *testing.T) {
		handler, database, root, _, _ := directConversationFrom(t, "alice")
		assertRequester(t, database, root.ID, 1, alice)
		if retry := retryDelivery(t, handler, root.ID, "steer", "bob"); retry.Code != http.StatusCreated {
			t.Fatalf("person's retry: status=%d body=%s", retry.Code, retry.Body.String())
		}
		assertRequester(t, database, root.ID, 2, bob)
		if retry := bearerRetryDelivery(t, handler, root.ID, "steer", "s2"); retry.Code != http.StatusCreated {
			t.Fatalf("bearer retry: status=%d body=%s", retry.Code, retry.Body.String())
		}
		assertRequester(t, database, root.ID, 3, session)
		reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
			"body": "And the logs?", "delivery": "aside", "in_reply_to": root.ID,
		}, "bob")
		if reply.Code != http.StatusCreated {
			t.Fatalf("person's reply in the direct thread: status=%d body=%s", reply.Code, reply.Body.String())
		}
		assertRequester(t, database, decodeBody[model.Message](t, reply).ID, 1, bob)
	})

	t.Run("a reply that inherits its thread's delivery", func(t *testing.T) {
		live := true
		sent := []map[string]any{}
		listener := sessionListener(t, &live, &sent)
		t.Cleanup(listener.Close)
		handler, database := newTargetedMessageHandler(t, listener.URL)
		issue := createInteractionIssue(t, handler, "TEST", "Inherited delivery", "before")
		question := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "Can this ship?", "target": "session:s1", "delivery": "steer",
		}, "alice")
		reply := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "And the rollback plan?", "in_reply_to": question.ID,
		}, "bob")
		if len(reply.Deliveries) != 1 {
			t.Fatalf("the inherited reply's deliveries = %+v, want one", reply.Deliveries)
		}
		assertRequester(t, database, question.ID, 1, alice)
		assertRequester(t, database, reply.ID, 1, bob)
	})

	t.Run("a new mode supersedes a stranded attempt; the same mode resumes it", func(t *testing.T) {
		handler, database, root, _, _ := directConversationFrom(t, "alice")
		strandMessageAttempt(t, database, root.ID)
		// The same mode resumes attempt 1 under its own number: whoever resumes it, alice is still
		// the person who asked for it.
		if resumed := bearerRetryDelivery(t, handler, root.ID, "aside", "s2"); resumed.Code != http.StatusCreated || decodeBody[acceptedAttempt](t, resumed).Attempt != 1 {
			t.Fatalf("same-mode retry of a stranded attempt: status=%d, want attempt 1 resumed", resumed.Code)
		}
		assertRequester(t, database, root.ID, 1, alice)

		strandMessageAttempt(t, database, root.ID)
		superseded := retryDelivery(t, handler, root.ID, "steer", "bob")
		if superseded.Code != http.StatusCreated || decodeBody[acceptedAttempt](t, superseded).Attempt != 2 {
			t.Fatalf("mode-change retry of a stranded attempt: status=%d, want attempt 2", superseded.Code)
		}
		assertRequester(t, database, root.ID, 1, alice)
		assertRequester(t, database, root.ID, 2, bob)
	})

	t.Run("a broadcast", func(t *testing.T) {
		listener, _ := newBroadcastListener(t, broadcastSessions)
		handler, database := newTargetedMessageHandler(t, listener.URL)
		created := decodeBody[broadcastResponse](t, postBroadcast(t, handler, map[string]any{
			"body": "Status?", "delivery": "steer", "session_ids": []string{"planner", "tester"},
		}, "alice"))
		for _, recipient := range awaitBroadcastDeliveries(t, handler, created.ID).Recipients {
			assertRequester(t, database, recipient.Message.ID, 1, alice)
		}
	})
}
