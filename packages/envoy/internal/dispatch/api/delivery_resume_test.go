package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// resumeSend is one send the resume tests' listener answered: the key, the session and the
// urgency it carried.
type resumeSend struct {
	key, target, urgency string
}

// resumeListener is the Envoy listener the resume tests send through: one live session, s1, that
// takes every mode and holds role:reviewer, and a record of every send it answered.
type resumeListener struct {
	server *httptest.Server
	mu     sync.Mutex
	sends  []resumeSend
}

func startResumeListener(t *testing.T) *resumeListener {
	t.Helper()
	listener := &resumeListener{}
	listener.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const session = `{"session_id":"s1","title":"planner","roles":["reviewer"],"capabilities":["btw","aside","steer"]}`
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			_, _ = w.Write([]byte(session))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer":
			_, _ = w.Write([]byte(`{"role":"reviewer","holder":"s1","title":"planner","capabilities":["btw","aside","steer"]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte("[" + session + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession  string `json:"target_session"`
				IdempotencyKey string `json:"idempotency_key"`
				Urgency        string `json:"urgency"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode a send: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			listener.mu.Lock()
			listener.sends = append(listener.sends, resumeSend{
				key: request.IdempotencyKey, target: request.TargetSession, urgency: request.Urgency,
			})
			listener.mu.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"resume-envelope","recipient":"` + request.TargetSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.server.Close)
	return listener
}

// answered is every send the listener has answered since the last forget.
func (l *resumeListener) answered() []resumeSend {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sends)
}

// forget drops what the listener has answered, so a test counts only what follows.
func (l *resumeListener) forget() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sends = nil
}

// strandAttempt sets attempt 1 of the message or comment back to pending, as a process that died
// between its claim and its settle leaves it, claimed and created age ago (`null` claims nothing).
func strandAttempt(t *testing.T, database *store.Store, table, column, id, claimed, created string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update `+table+`
		set state = 'pending', envelope_id = null, error = null, duplicate = false,
		    claimed_at = now() - `+claimed+`::interval, created_at = now() - `+created+`::interval
		where `+column+` = $1 and attempt = 1
	`, id); err != nil {
		t.Fatalf("strand attempt 1 of %s %s: %v", table, id, err)
	}
}

// attemptStates is every attempt of the message or comment, in order, as "<attempt>:<state>".
func attemptStates(t *testing.T, database *store.Store, table, column, id string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select attempt::text || ':' || state from `+table+` where `+column+` = $1 order by attempt
	`, id)
	if err != nil {
		t.Fatalf("read the attempts of %s %s: %v", table, id, err)
	}
	defer rows.Close()
	var states []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("read an attempt of %s %s: %v", table, id, err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the attempts of %s %s: %v", table, id, err)
	}
	return states
}

// A targeted message's attempt 1 is written in the transaction that stores the message, before
// anything is resolved, so it names no session. The request's own send is bound to that attempt,
// and it must take it - the row names the message's own target - and pin it to the session the
// target resolved to, rather than refusing an attempt whose row names no session.
func TestABoundSendTakesAnUnpinnedAttemptAndPinsTheSessionItsTargetResolvedTo(t *testing.T) {
	listener := startResumeListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	direct := directServer(deps)
	issue := createInteractionIssue(t, handler, "TEST", "For whoever reviews", "before")
	alice := model.Actor{Kind: "user", ID: "alice"}
	author, err := json.Marshal(alice)
	if err != nil {
		t.Fatalf("encode the author: %v", err)
	}
	message, err := scanMessage(database.Pool.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, target) values ($1, $2, 'For whoever reviews', 'role:reviewer')
		returning `+messageColumns, issue.Key, author))
	if err != nil {
		t.Fatalf("store the message: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by)
		values ($1, 1, 'steer', '', 'pending', $2)
	`, message.ID, author); err != nil {
		t.Fatalf("open the unpinned attempt 1: %v", err)
	}

	attempt, err := direct.deliverMessageResuming(context.Background(), message, "steer", nil, alice, nil, 1)
	if err != nil {
		t.Fatalf("bound send of the unpinned attempt 1: %v", err)
	}
	if attempt.Attempt != 1 || attempt.State != "sent" || attempt.SessionID != "s1" {
		t.Fatalf("bound send = %#v, want attempt 1 sent and pinned to s1, the session role:reviewer resolved to", attempt)
	}
	if sends := listener.answered(); len(sends) != 1 || sends[0] != (resumeSend{key: message.ID + ":steer", target: "s1"}) {
		t.Fatalf("listener sends = %#v, want one send to s1 under %q", sends, message.ID+":steer")
	}
	if states := attemptStates(t, database, "message_deliveries", "message_id", message.ID); !slices.Equal(states, []string{"1:sent"}) {
		t.Fatalf("attempts = %v, want attempt 1 alone, sent", states)
	}
}

// The resume loop names one attempt and must send that attempt or nothing. A live sender holding
// it - its claim inside the lease - owns the frame, so the resume sends nothing and opens no
// attempt beside it, where a Retry would open one; once that claim lapses, the resume takes the
// attempt under its original number and key.
func TestACommentResumeSendsNothingBesideALiveSenderAndTakesTheAttemptOnceItLapses(t *testing.T) {
	listener := startResumeListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	direct := directServer(deps)
	issue := createInteractionIssue(t, handler, "TEST", "A resumed mention", "before")
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Resume me, once.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	comment, err := direct.loadComment(context.Background(), database.Pool, created.ID)
	if err != nil {
		t.Fatalf("load the comment: %v", err)
	}
	event, err := direct.loadCommentCreatedEvent(context.Background(), database.Pool, created.ID)
	if err != nil {
		t.Fatalf("load the comment's event: %v", err)
	}
	mention := ResolvedMention{Target: "session:s1", Delivery: "steer"}
	resume := commentClaim{mode: claimResume, attempt: 1}

	// A sender claimed attempt 1 a moment ago and is still sending it.
	strandAttempt(t, database, "comment_deliveries", "comment_id", comment.ID, "'0 seconds'", "'5 minutes'")
	listener.forget()
	held, err := direct.deliverResolvedCommentMention(context.Background(), comment, event, mention, comment.Author, resume)
	if err != nil {
		t.Fatalf("resume beside a live sender: %v", err)
	}
	if held.Attempt != 1 || held.State != "pending" {
		t.Fatalf("resume beside a live sender = %#v, want attempt 1 reported as the live sender left it", held)
	}
	if sends := listener.answered(); len(sends) != 0 {
		t.Fatalf("resume beside a live sender sent %#v, want nothing: the live sender owns the frame", sends)
	}
	if states := attemptStates(t, database, "comment_deliveries", "comment_id", comment.ID); !slices.Equal(states, []string{"1:pending"}) {
		t.Fatalf("attempts after a resume beside a live sender = %v, want attempt 1 alone, still pending", states)
	}

	// That sender never came back.
	strandAttempt(t, database, "comment_deliveries", "comment_id", comment.ID, "'5 minutes'", "'5 minutes'")
	resumed, err := direct.deliverResolvedCommentMention(context.Background(), comment, event, mention, comment.Author, resume)
	if err != nil {
		t.Fatalf("resume the abandoned attempt: %v", err)
	}
	if resumed.Attempt != 1 || resumed.State != "sent" {
		t.Fatalf("resume of the abandoned attempt = %#v, want attempt 1 sent", resumed)
	}
	key := comment.ID + ":session:s1:steer"
	if sends := listener.answered(); len(sends) != 1 || sends[0] != (resumeSend{key: key, target: "s1"}) {
		t.Fatalf("resume sends = %#v, want one send to s1 under the original key %q", sends, key)
	}
	if states := attemptStates(t, database, "comment_deliveries", "comment_id", comment.ID); !slices.Equal(states, []string{"1:sent"}) {
		t.Fatalf("attempts after the resume = %v, want attempt 1 alone, sent", states)
	}
}

// One pass of the resume loop sends every attempt a sender left pending past the claim lease and
// inside the horizon, with the urgency its message was sent with, and leaves the rest as they
// are: a request's own fresh attempt, an attempt older than the horizon (it waits for a person's
// Retry), and a message on a closed issue, whose claim would be refused for good.
func TestTheDeliveryResumePassSendsOnlyTheStrandedAttemptsItOwes(t *testing.T) {
	listener := startResumeListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	direct := directServer(deps)
	open := createInteractionIssue(t, handler, "TEST", "Stranded deliveries", "before")
	closed := createInteractionIssue(t, handler, "SHUT", "Closed since", "before")
	agentMessage := func(body string) model.Message {
		t.Helper()
		return decodeBody[model.Message](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages",
			map[string]any{"body": body, "delivery": "steer"}, "alice"))
	}

	stranded := createIssueMessage(t, handler, open.Key, map[string]any{
		"body": "Stranded, and urgent", "target": "session:s1", "delivery": "steer", "urgency": "high",
	}, "alice")
	strandAttempt(t, database, "message_deliveries", "message_id", stranded.ID, "'2 minutes'", "'2 minutes'")
	mention := decodeMentionedComment(t, postMentionedComment(t, handler, open.Key, map[string]any{
		"body": "A stranded mention.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	strandAttempt(t, database, "comment_deliveries", "comment_id", mention.ID, "'2 minutes'", "'2 minutes'")
	fresh := agentMessage("Its request is still sending it")
	strandAttempt(t, database, "message_deliveries", "message_id", fresh.ID, "null", "'0 seconds'")
	tooOld := agentMessage("Stranded before the horizon")
	strandAttempt(t, database, "message_deliveries", "message_id", tooOld.ID, "'2 hours'", "'2 hours'")
	onClosed := createIssueMessage(t, handler, closed.Key, map[string]any{
		"body": "On an issue closed since", "target": "session:s1", "delivery": "steer",
	}, "alice")
	strandAttempt(t, database, "message_deliveries", "message_id", onClosed.ID, "'2 minutes'", "'2 minutes'")
	if _, err := database.Pool.Exec(context.Background(),
		`update issues set status = 'done', closed_at = now() where key = $1`, closed.Key); err != nil {
		t.Fatalf("close %s: %v", closed.Key, err)
	}
	listener.forget()

	if err := direct.resumeStrandedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume pass: %v", err)
	}

	byKey := func(a, b resumeSend) int { return cmp.Compare(a.key, b.key) }
	sends := listener.answered()
	slices.SortFunc(sends, byKey)
	want := []resumeSend{
		{key: stranded.ID + ":steer", target: "s1", urgency: "high"},
		{key: mention.ID + ":session:s1:steer", target: "s1"},
	}
	slices.SortFunc(want, byKey)
	if !slices.Equal(sends, want) {
		t.Fatalf("the pass sent %#v, want only the stranded message, with its urgency, and the stranded mention: %#v", sends, want)
	}
	for _, row := range []struct {
		name, table, column, id string
		want                    []string
	}{
		{"the stranded message", "message_deliveries", "message_id", stranded.ID, []string{"1:sent"}},
		{"the stranded mention", "comment_deliveries", "comment_id", mention.ID, []string{"1:sent"}},
		{"the fresh attempt", "message_deliveries", "message_id", fresh.ID, []string{"1:pending"}},
		{"the attempt past the horizon", "message_deliveries", "message_id", tooOld.ID, []string{"1:pending"}},
		{"the message on a closed issue", "message_deliveries", "message_id", onClosed.ID, []string{"1:pending"}},
	} {
		if states := attemptStates(t, database, row.table, row.column, row.id); !slices.Equal(states, row.want) {
			t.Errorf("%s: attempts = %v, want %v", row.name, states, row.want)
		}
	}
}
