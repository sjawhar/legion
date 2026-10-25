package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// INFRA-258. An issue message aimed at a session delivers inside its request, so a process that
// died between its commit and its answer owes that delivery and must never make the message twice.
// Its key holds the message, so the repeat past the lease neither runs the write again nor answers
// KEY_COMMITTED: it sends what is owed, once, and answers the message with its deliveries.
func TestARepeatOfAnIssueMessageDeliversWhatItOwesAndWritesNothing(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createTestIssue(t, handler, "KEYD", "Keyed issue messages")
	const body = "Status before the deploy?"
	input := map[string]any{"body": body, "target": "session:s1", "delivery": "steer"}

	// What the stopped process committed: the message, and its key holding it, with no attempt
	// and no stored answer.
	author, err := json.Marshal(model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var messageID string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, target) values ($1, $2, $3, 'session:s1')
		returning id::text
	`, issue, author, body).Scan(&messageID); err != nil {
		t.Fatalf("commit the message: %v", err)
	}
	holdRequestKey(t, database, heldKey{
		login: "alice", key: "press-1", pattern: "/api/v1/issues/{key}/messages",
		request: jsonRequest(t, http.MethodPost, "/api/v1/issues/"+issue+"/messages", input,
			map[string]string{"key": issue}),
		leasedAgo: 2 * time.Minute,
		recovery:  messageRecovery{MessageID: messageID, Delivery: "steer"},
	})

	repeat := keyedRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/messages", input, "alice", "press-1")
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("the repeat: status=%d body=%s, want the message replayed", repeat.Code, repeat.Body.String())
	}
	answered := decodeBody[model.Message](t, repeat)
	if answered.ID != messageID || len(answered.Deliveries) != 1 || answered.Deliveries[0].Attempt != 1 ||
		answered.Deliveries[0].State != "sent" || answered.Deliveries[0].SessionID != "s1" {
		t.Fatalf("the repeat answered %#v, want message %s with attempt 1 sent to s1", answered, messageID)
	}
	if keys := keysOf(sends); !equalStrings(keys, []string{messageID + ":steer"}) {
		t.Fatalf("listener sends = %v, want the message once, in its own mode", keys)
	}
	if messages := countMessages(t, database); messages != 1 {
		t.Fatalf("the database holds %d messages, want the one", messages)
	}
	// A later repeat finds nothing owed and sends nothing more.
	if again := keyedRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/messages", input, "alice", "press-1"); again.Code != http.StatusCreated {
		t.Fatalf("a later repeat: status=%d body=%s", again.Code, again.Body.String())
	}
	if got := sends.targets(); !equalStrings(got, []string{"s1"}) {
		t.Fatalf("listener sends = %v, want one frame", got)
	}

	// The live route commits that recovery point itself, which is what leaves a crashed request's
	// key holding its message rather than only marked committed.
	live := map[string]any{"body": "Sent live.", "target": "session:s1", "delivery": "btw"}
	if response := keyedRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/messages", live, "alice", "live-1"); response.Code != http.StatusCreated {
		t.Fatalf("a live keyed message: status=%d body=%s", response.Code, response.Body.String())
	}
	var recovered string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(recovery ->> 'message_id', '') from request_keys where idempotency_key = 'live-1'`,
	).Scan(&recovered); err != nil {
		t.Fatalf("read the live send's key: %v", err)
	}
	if recovered == "" {
		t.Fatal("the live keyed message's key holds no recovery point")
	}
}

// A comment's mentions deliver inside its request the same way. A repeat under its key sends every
// mention the comment still owes - its one attempt pending with no live claim - under that
// attempt's own number, so the listener drops a frame that did land, and answers the comment with
// its mentions and deliveries; it opens no second attempt and writes no second comment.
func TestARepeatOfACommentDeliversItsOwedMentionOnce(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createTestIssue(t, handler, "KEYD", "Keyed comments")
	input := map[string]any{
		"body": "@s1 please look", "delivery": "steer",
		"mentions": []map[string]string{{"target": "session:s1"}},
	}
	post := func() *httptest.ResponseRecorder {
		t.Helper()
		return keyedRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", input, "alice", "mention-1")
	}
	first := post()
	if first.Code != http.StatusCreated {
		t.Fatalf("first: status=%d body=%s", first.Code, first.Body.String())
	}
	comment := decodeBody[model.Comment](t, first)

	// The process stopped before it could record what its send did: the attempt is pending again,
	// its claim long lapsed, and the key holds the comment with no stored answer.
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries
		set state = 'pending', envelope_id = null, error = null, claimed_at = now() - interval '5 minutes'
		where comment_id = $1
	`, comment.ID); err != nil {
		t.Fatalf("strand the mention's attempt: %v", err)
	}
	loseAnswer(t, database, "mention-1")

	repeat := post()
	if repeat.Code != http.StatusCreated || !replayed(repeat) {
		t.Fatalf("the repeat: status=%d body=%s, want the comment replayed", repeat.Code, repeat.Body.String())
	}
	answered := decodeBody[model.Comment](t, repeat)
	if answered.ID != comment.ID || len(answered.Mentions) != 1 || len(answered.Deliveries) != 1 ||
		answered.Deliveries[0].Attempt != 1 || answered.Deliveries[0].State != "sent" {
		t.Fatalf("the repeat answered %#v, want comment %s with its one mention sent on attempt 1", answered, comment.ID)
	}
	// Both sends carry the attempt's own key, which is what makes the second a repeat the stream
	// drops rather than a second frame.
	key := comment.ID + ":session:s1:steer"
	if keys := keysOf(sends); !equalStrings(keys, []string{key, key}) {
		t.Fatalf("listener sends = %v, want the one attempt's key twice", keys)
	}
	var comments, attempts int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from comments where issue_key = $1),
		       (select count(*) from comment_deliveries where comment_id = $2)
	`, issue, comment.ID).Scan(&comments, &attempts); err != nil {
		t.Fatal(err)
	}
	if comments != 1 || attempts != 1 {
		t.Fatalf("the issue holds %d comments and %d attempts, want one of each", comments, attempts)
	}
}

// A comment whose mention was delivered owes nothing: its repeat sends nothing more and answers the
// comment as it stands. A comment that committed with no mention at all is answered the same way,
// without a listener call.
func TestARepeatOfADeliveredCommentSendsNothingMore(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createTestIssue(t, handler, "KEYD", "Keyed comments")
	for name, test := range map[string]struct {
		key   string
		input map[string]any
		sends int
	}{
		"a delivered mention": {"delivered", map[string]any{
			"body": "@s1 look", "delivery": "btw", "mentions": []map[string]string{{"target": "session:s1"}},
		}, 1},
		"no mention": {"plain", map[string]any{"body": "no one in particular"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(sends.targets())
			post := func() *httptest.ResponseRecorder {
				t.Helper()
				return keyedRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", test.input, "alice", test.key)
			}
			first := post()
			if first.Code != http.StatusCreated {
				t.Fatalf("first: status=%d body=%s", first.Code, first.Body.String())
			}
			again := post()
			if again.Code != http.StatusCreated || !replayed(again) {
				t.Fatalf("the repeat: status=%d body=%s, want the comment replayed", again.Code, again.Body.String())
			}
			if decodeBody[model.Comment](t, again).ID != decodeBody[model.Comment](t, first).ID {
				t.Fatalf("the repeat answered another comment: %s", again.Body.String())
			}
			if sent := len(sends.targets()) - before; sent != test.sends {
				t.Fatalf("%d sends, want %d: a repeat owes nothing", sent, test.sends)
			}
		})
	}
}

// A keyed write's replay answers the resource as it stands in the first answer's shape: the members
// the first answer carried that a fresh read does not - a write's advice, an ask reply's
// ask_waiting_on - are kept, and everything the fresh read holds is the fresh value.
func TestAReplayKeepsTheFirstAnswersShape(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createTestIssue(t, handler, "KEYS", "Replay shape")
	first := keyedRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": "The first answer's shape."}, "alice", "shape-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first: status=%d body=%s", first.Code, first.Body.String())
	}
	stored := decodeBody[map[string]json.RawMessage](t, first)
	if _, carried := stored["advice"]; !carried {
		t.Fatalf("the first answer carried no advice to keep: %s", first.Body.String())
	}
	// A body nobody read back from the database would hide a replay that answered the stored bytes.
	if _, err := database.Pool.Exec(context.Background(),
		`update comments set body = 'Edited since.' where id = $1`,
		string(decodeBody[model.Comment](t, first).ID)); err != nil {
		t.Fatal(err)
	}
	again := keyedRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
		map[string]any{"body": "The first answer's shape."}, "alice", "shape-1")
	if again.Code != http.StatusCreated || !replayed(again) {
		t.Fatalf("the repeat: status=%d body=%s", again.Code, again.Body.String())
	}
	replay := decodeBody[map[string]json.RawMessage](t, again)
	if _, kept := replay["advice"]; !kept {
		t.Fatalf("the replay dropped the first answer's advice: %s", again.Body.String())
	}
	if got := decodeBody[model.Comment](t, again).Body; got != "Edited since." {
		t.Fatalf("the replay answered %q, want the comment as it stands", got)
	}
	if code := again.Header().Get(contracts.IdempotencyKeyHeader); code != "shape-1" {
		t.Fatalf("the replay's key header = %q, want shape-1", code)
	}
}
