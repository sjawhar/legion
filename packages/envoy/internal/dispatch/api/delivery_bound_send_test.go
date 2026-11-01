package api

import (
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

// resumeSend is one send the bound-send tests' listener answered: the key, the session and the
// urgency it carried.
type resumeSend struct {
	key, target, urgency string
}

// resumeListener is the Envoy listener the bound-send tests send through: one live session, s1,
// that takes every mode and holds role:reviewer, and a record of every send it answered.
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

// strandStoredAttempt sets attempt 1 of the message or comment back to pending, as a process that
// died between its claim and its settle leaves it, claimed and created age ago (`null` claims
// nothing).
func strandStoredAttempt(t *testing.T, database *store.Store, table, column, id, claimed, created string) {
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

	attempt, err := direct.deliverMessageResuming(context.Background(), message, "steer", alice, nil, 1)
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

// The delivery sweep names one attempt and must send that attempt or nothing. A live sender
// holding it - its claim inside the lease - owns the frame, so a bound send makes none and opens
// no attempt beside it, where a Retry would open one; once that claim lapses, the bound send takes
// the attempt under its original number and key.
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

	// A sender claimed attempt 1 a moment ago and is still sending it.
	strandStoredAttempt(t, database, "comment_deliveries", "comment_id", comment.ID, "'0 seconds'", "'5 minutes'")
	listener.forget()
	held, err := direct.deliverResolvedCommentMention(context.Background(), comment, event, mention, comment.Author, 1, direct.newTakeoverRetry())
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
	strandStoredAttempt(t, database, "comment_deliveries", "comment_id", comment.ID, "'5 minutes'", "'5 minutes'")
	resumed, err := direct.deliverResolvedCommentMention(context.Background(), comment, event, mention, comment.Author, 1, direct.newTakeoverRetry())
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

// The urgency a targeted message was sent with lives on its attempt row
// (message_deliveries.urgency, migration 0158), and the claim of a bound send reads it from there
// (pendingMessageDelivery). The delivery sweep therefore carries no urgency of its own - its
// resumeOwedDelivery hands deliverMessageResuming the attempt's number and nothing else - and a
// stranded urgent message it resumes still reaches the listener as urgent, not as a plain steer.
func TestTheDeliverySweepResumesAnAttemptWithTheUrgencyItRecorded(t *testing.T) {
	listener := startResumeListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	direct := directServer(deps)
	issue := createInteractionIssue(t, handler, "TEST", "Stranded and urgent", "before")
	stranded := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Stranded, and urgent", "target": "session:s1", "delivery": "steer", "urgency": "high",
	}, "alice")
	strandStoredAttempt(t, database, "message_deliveries", "message_id", stranded.ID, "'2 minutes'", "'2 minutes'")
	listener.forget()

	if err := direct.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("delivery sweep pass: %v", err)
	}

	want := resumeSend{key: stranded.ID + ":steer", target: "s1", urgency: "high"}
	if sends := listener.answered(); len(sends) != 1 || sends[0] != want {
		t.Fatalf("the sweep sent %#v, want exactly %#v: the urgency the attempt recorded", sends, want)
	}
	if states := attemptStates(t, database, "message_deliveries", "message_id", stranded.ID); !slices.Equal(states, []string{"1:sent"}) {
		t.Fatalf("attempts = %v, want attempt 1 alone, sent", states)
	}
}

// A person's Retry in the same mode, once the claim on a stranded attempt has lapsed, resumes that
// attempt under its own number and key - the send the delivery sweep would have made - so it must
// carry the urgency the attempt recorded, as the sweep's does. A Retry that opens an attempt of
// its own instead - in another mode, or beside a sender still holding the stranded one - is a new
// send of the person's, which asked for no urgency.
func TestARetryCarriesTheUrgencyOfTheAttemptItResumesAndNoneOnANewAttempt(t *testing.T) {
	listener := startResumeListener(t)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	issue := createInteractionIssue(t, handler, "TEST", "Urgent retries", "before")
	urgent := func(body string) model.Message {
		t.Helper()
		return createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": body, "target": "session:s1", "delivery": "steer", "urgency": "high",
		}, "alice")
	}
	for _, test := range []struct {
		name, claimed, delivery string
		wantAttempt             int
		wantSend                func(model.Message) resumeSend
	}{
		{
			name: "a same-mode Retry of a lapsed attempt resumes it", claimed: "'2 minutes'", delivery: "steer",
			wantAttempt: 1,
			wantSend: func(m model.Message) resumeSend {
				return resumeSend{key: m.ID + ":steer", target: "s1", urgency: "high"}
			},
		},
		{
			name: "a Retry in another mode opens a new attempt", claimed: "'2 minutes'", delivery: "btw",
			wantAttempt: 2,
			wantSend: func(m model.Message) resumeSend {
				return resumeSend{key: m.ID + ":btw", target: "s1"}
			},
		},
		{
			name: "a Retry beside a live claim opens a new attempt", claimed: "'0 seconds'", delivery: "steer",
			wantAttempt: 2,
			wantSend: func(m model.Message) resumeSend {
				return resumeSend{key: m.ID + ":steer", target: "s1"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := urgent(test.name)
			strandStoredAttempt(t, database, "message_deliveries", "message_id", message.ID, test.claimed, "'2 minutes'")
			listener.forget()

			retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
				map[string]any{"delivery": test.delivery}, "alice")
			if retry.Code != http.StatusCreated {
				t.Fatalf("retry: status=%d body=%s", retry.Code, retry.Body.String())
			}
			if attempt := decodeBody[model.MessageDelivery](t, retry); attempt.Attempt != test.wantAttempt || attempt.State != "sent" {
				t.Fatalf("retry = attempt %d %s, want attempt %d sent", attempt.Attempt, attempt.State, test.wantAttempt)
			}
			want := test.wantSend(message)
			if sends := listener.answered(); len(sends) != 1 || sends[0] != want {
				t.Fatalf("retry sent %#v, want exactly %#v", sends, want)
			}
		})
	}
}
