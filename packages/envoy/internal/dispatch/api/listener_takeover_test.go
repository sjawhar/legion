package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// takeOverRoute is a listener route a take-over listener's script names: the registry read, the
// send, or any request at all.
type takeOverRoute string

const (
	takeOverRegistry takeOverRoute = "registry"
	takeOverSend     takeOverRoute = "send"
	takeOverAny      takeOverRoute = "any"
)

// takeOverAnswer is one misbehaviour a take-over listener answers the next request on its route
// with, in place of the answer it would give once the take-over is over.
type takeOverAnswer struct {
	route  takeOverRoute
	answer func(l *takeOverListener, w http.ResponseWriter, r *http.Request, send resumeSend)
}

// takeOverListener is a listener whose replacement is under way: session s1, which takes every
// mode, once it answers, and until then the script it is given - each entry answers the next
// request on its route and is spent - or, while refusing, every request refused. It records every
// send it published, a late answer's included, and how many requests reached each route.
type takeOverListener struct {
	server *httptest.Server

	mu       sync.Mutex
	script   []takeOverAnswer
	refusing bool
	requests map[takeOverRoute]int
	sends    []resumeSend
	// observe runs on every request before it is answered, so a test can read the attempt row
	// as each try finds it.
	observe func()
}

func startTakeOverListener(t *testing.T) *takeOverListener {
	t.Helper()
	listener := &takeOverListener{requests: map[takeOverRoute]int{}}
	listener.server = httptest.NewServer(http.HandlerFunc(listener.serve))
	t.Cleanup(listener.server.Close)
	return listener
}

const takeOverSession = `{"session_id":"s1","title":"planner","roles":[],"capabilities":["btw","aside","steer"]}`

func (l *takeOverListener) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	route := takeOverRoute("")
	var send resumeSend
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
		route = takeOverRegistry
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
		route = takeOverSend
		var request struct {
			TargetSession  string `json:"target_session"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		_ = json.Unmarshal(body, &request)
		send = resumeSend{key: request.IdempotencyKey, target: request.TargetSession}
	}
	l.mu.Lock()
	observe := l.observe
	l.requests[route]++
	var scripted *takeOverAnswer
	switch {
	case l.refusing:
		scripted = &takeOverAnswer{answer: refuseConnection}
	case len(l.script) > 0 && (l.script[0].route == takeOverAny || l.script[0].route == route):
		scripted = &l.script[0]
		l.script = l.script[1:]
	}
	l.mu.Unlock()
	if observe != nil {
		observe()
	}
	if scripted != nil {
		scripted.answer(l, w, r, send)
		return
	}
	switch route {
	case takeOverRegistry:
		_, _ = w.Write([]byte(takeOverSession))
	case takeOverSend:
		l.publish(w, send)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// publish answers a send the way the listener does once it holds the session: a key and
// recipient it has published before is answered as a duplicate.
func (l *takeOverListener) publish(w http.ResponseWriter, send resumeSend) {
	l.mu.Lock()
	duplicate := slices.Contains(l.sends, send)
	l.sends = append(l.sends, send)
	l.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"event_id": "takeover-envelope", "recipient": send.target, "duplicate": duplicate})
}

// refuseConnection drops the connection without an answer, which the client meets as a
// connection the listener did not serve - what a listener pod that is gone or not yet listening
// gives.
func refuseConnection(_ *takeOverListener, w http.ResponseWriter, _ *http.Request, _ resumeSend) {
	connection, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_ = connection.Close()
}

func answerJSON(status int, body string) func(*takeOverListener, http.ResponseWriter, *http.Request, resumeSend) {
	return func(_ *takeOverListener, w http.ResponseWriter, _ *http.Request, _ resumeSend) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (l *takeOverListener) play(script ...takeOverAnswer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.script = script
	l.requests = map[takeOverRoute]int{}
	l.sends = nil
}

func (l *takeOverListener) refuse(refusing bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refusing = refusing
}

func (l *takeOverListener) requestsTo(route takeOverRoute) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests[route]
}

func (l *takeOverListener) published() []resumeSend {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sends)
}

// deliveryReceiptsOf counts the delivery receipts the events hold for attempt 1 of a message or
// comment: receiptType is message.delivery or comment.delivery, field the payload key naming it.
func deliveryReceiptsOf(t *testing.T, database *store.Store, receiptType, field, id string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where type = $1 and payload ->> $2 = $3 and (payload ->> 'attempt')::int = 1
	`, receiptType, field, id).Scan(&count); err != nil {
		t.Fatalf("count the receipts of %s: %v", id, err)
	}
	return count
}

// A listener being replaced refuses connections, answers 503, or does not hold a session yet for
// a few seconds. A send that meets one of those waits out the take-over under the attempt it
// holds and its own key, and reaches the session once the listener answers. Anything else the
// listener says - the session does not take this mode, the send itself is refused - is its
// answer, and the attempt settles failed on that first try.
func TestAListenerTakeOverIsWaitedOutAndARefusalSettlesAtOnce(t *testing.T) {
	listener := startTakeOverListener(t)
	handler, database := newTargetedMessageHandler(t, listener.server.URL, 300*time.Millisecond)

	lateAnswer := func(l *takeOverListener, w http.ResponseWriter, _ *http.Request, send resumeSend) {
		// The listener publishes before it answers, and this answer misses the client's window.
		l.mu.Lock()
		l.sends = append(l.sends, send)
		l.mu.Unlock()
		time.Sleep(900 * time.Millisecond)
		_, _ = w.Write([]byte(`{"event_id":"takeover-late","recipient":"s1"}`))
	}
	for _, tc := range []struct {
		name   string
		script []takeOverAnswer
		// failure is the error a refusal settles with; empty for a take-over the send waits out.
		failure       string
		wantDuplicate bool
		// triesRoute is the route a refusal answers on, which it asks exactly once.
		triesRoute takeOverRoute
	}{
		{name: "the listener refuses the connection", script: []takeOverAnswer{{takeOverAny, refuseConnection}}},
		{name: "the listener is starting", script: []takeOverAnswer{
			{takeOverRegistry, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)},
		}},
		{name: "the listener does not hold the session yet", script: []takeOverAnswer{
			{takeOverRegistry, answerJSON(http.StatusNotFound, `{"error":"no live session s1"}`)},
		}},
		{name: "the send finds no live session yet", script: []takeOverAnswer{
			{takeOverSend, answerJSON(http.StatusNotFound, `{"error":"no live session s1"}`)},
		}},
		{name: "the send meets the listener's starting gate", script: []takeOverAnswer{
			{takeOverSend, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)},
			{takeOverSend, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)},
		}},
		{name: "a gateway answers for the listener", script: []takeOverAnswer{
			{takeOverSend, func(_ *takeOverListener, w http.ResponseWriter, _ *http.Request, _ resumeSend) {
				w.WriteHeader(http.StatusBadGateway)
			}},
		}},
		{name: "the send's receipt timed out", script: []takeOverAnswer{{takeOverSend, lateAnswer}}, wantDuplicate: true},
		{name: "the session does not take the mode", script: []takeOverAnswer{
			{takeOverRegistry, answerJSON(http.StatusOK, `{"session_id":"s1","title":"planner","capabilities":["btw"]}`)},
		}, failure: "session s1 (planner) does not advertise steer", triesRoute: takeOverRegistry},
		{name: "the listener refuses the send", script: []takeOverAnswer{
			{takeOverSend, answerJSON(http.StatusBadRequest, `{"error":"message is required"}`)},
		}, failure: "message is required", triesRoute: takeOverSend},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener.play(tc.script...)
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages",
				map[string]any{"body": tc.name, "delivery": "steer"}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("send: status=%d body=%s", response.Code, response.Body.String())
			}
			message := decodeBody[model.Message](t, response)
			if len(message.Deliveries) != 1 {
				t.Fatalf("deliveries = %#v, want attempt 1 alone", message.Deliveries)
			}
			attempt := message.Deliveries[0]
			states := attemptStates(t, database, "message_deliveries", "message_id", message.ID)
			if tc.failure != "" {
				if attempt.State != "failed" || attempt.Error == nil || *attempt.Error != tc.failure {
					t.Fatalf("attempt = %#v, want failed with %q", attempt, tc.failure)
				}
				if tries := listener.requestsTo(tc.triesRoute); tries != 1 {
					t.Fatalf("the listener's %s was asked %d times, want once: a refusal is the listener's answer", tc.triesRoute, tries)
				}
				if !slices.Equal(states, []string{"1:failed"}) {
					t.Fatalf("attempts = %v, want attempt 1 alone, failed", states)
				}
				return
			}
			if attempt.Attempt != 1 || attempt.State != "sent" || attempt.Duplicate != tc.wantDuplicate || attempt.SessionID != "s1" {
				t.Fatalf("attempt = %#v, want attempt 1 sent to s1 (duplicate %v) once the take-over ended", attempt, tc.wantDuplicate)
			}
			if !slices.Equal(states, []string{"1:sent"}) {
				t.Fatalf("attempts = %v, want attempt 1 alone, sent", states)
			}
			key := message.ID + ":steer"
			for _, send := range listener.published() {
				if send != (resumeSend{key: key, target: "s1"}) {
					t.Fatalf("published %#v, want every send to s1 under %q", listener.published(), key)
				}
			}
			if receipts := deliveryReceiptsOf(t, database, "message.delivery", "message_id", message.ID); receipts != 1 {
				t.Fatalf("attempt 1 has %d receipts, want one", receipts)
			}
		})
	}
}

// With no listener configured there is no take-over to wait out: the send settles failed at once.
func TestASendWithNoListenerConfiguredSettlesAtOnce(t *testing.T) {
	handler, _ := newTargetedMessageHandler(t, "")
	started := time.Now()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages",
		map[string]any{"body": "Nobody listens", "delivery": "steer"}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("send: status=%d body=%s", response.Code, response.Body.String())
	}
	message := decodeBody[model.Message](t, response)
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "failed" {
		t.Fatalf("deliveries = %#v, want attempt 1 failed", message.Deliveries)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the send took %v to fail, want it settled at once: no listener is no take-over", took)
	}
}

// A sender waiting out a take-over renews its claim before each try and settles on the claim it
// renewed last, so the attempt never reads as abandoned while its sender is still trying: the
// resume loop, run during the wait, sends nothing and opens nothing beside it, and the attempt
// reaches its session once, under attempt 1, with one receipt.
func TestASenderWaitingOutAListenerTakeOverRenewsItsClaimBeforeEachTry(t *testing.T) {
	for _, tc := range []struct {
		name, table, column, receipt, path string
		body                               map[string]any
		key                                func(id string) string
	}{
		{
			name: "a direct message", table: "message_deliveries", column: "message_id", receipt: "message.delivery",
			path: "/api/v1/agents/s1/messages",
			body: map[string]any{"body": "Sent during a take-over", "delivery": "steer"},
			key:  func(id string) string { return id + ":steer" },
		},
		{
			// The comment route is the issue's, created below.
			name: "a mention", table: "comment_deliveries", column: "comment_id", receipt: "comment.delivery",
			body: map[string]any{"body": "Mentioned during a take-over", "mentions": []map[string]any{{"target": "session:s1"}}},
			key:  func(id string) string { return id + ":session:s1:steer" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := startTakeOverListener(t)
			handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
			path := tc.path
			if path == "" {
				issue := createInteractionIssue(t, handler, "TEST", "A take-over", "before")
				path = "/api/v1/issues/" + issue.Key + "/comments"
			}
			var claimsMu sync.Mutex
			var claims []time.Time
			listener.mu.Lock()
			listener.observe = func() {
				var claimed *time.Time
				err := database.Pool.QueryRow(context.Background(),
					`select claimed_at from `+tc.table+` where state = 'pending' and attempt = 1`).Scan(&claimed)
				if err == nil && claimed != nil {
					claimsMu.Lock()
					claims = append(claims, *claimed)
					claimsMu.Unlock()
				}
			}
			listener.mu.Unlock()
			listener.refuse(true)

			data, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("encode the request: %v", err)
			}
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dispatch-User", "alice")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				answered <- response
			}()
			// Three tries the sender made while holding the attempt.
			deadline := time.Now().Add(10 * time.Second)
			for {
				claimsMu.Lock()
				seen := len(claims)
				claimsMu.Unlock()
				if seen >= 3 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the sender made %d tries holding its attempt within 10s of the take-over, want 3", seen)
				}
				time.Sleep(20 * time.Millisecond)
			}

			// The attempt is old enough for the resume loop, and only its live claim keeps it the
			// waiting sender's.
			if _, err := database.Pool.Exec(context.Background(),
				`update `+tc.table+` set created_at = created_at - interval '5 minutes' where state = 'pending'`,
			); err != nil {
				t.Fatalf("age the attempt: %v", err)
			}
			if err := directServer(deps).resumeStrandedDeliveries(context.Background()); err != nil {
				t.Fatalf("resume pass during the wait: %v", err)
			}
			var id string
			if err := database.Pool.QueryRow(context.Background(),
				`select `+tc.column+`::text from `+tc.table+` where state = 'pending'`).Scan(&id); err != nil {
				t.Fatalf("read the waiting attempt: %v", err)
			}
			if states := attemptStates(t, database, tc.table, tc.column, id); !slices.Equal(states, []string{"1:pending"}) {
				t.Fatalf("attempts after a resume pass during the wait = %v, want attempt 1 alone, still the waiting sender's", states)
			}

			listener.refuse(false)
			var response *httptest.ResponseRecorder
			select {
			case response = <-answered:
			case <-time.After(15 * time.Second):
				t.Fatal("the request never answered after the take-over ended")
			}
			if response.Code != http.StatusCreated {
				t.Fatalf("send: status=%d body=%s", response.Code, response.Body.String())
			}
			claimsMu.Lock()
			observed := slices.Clone(claims)
			claimsMu.Unlock()
			for index := 1; index < len(observed); index++ {
				if !observed[index].After(observed[index-1]) {
					t.Fatalf("claimed_at as each try found it = %v, want it later at every try: the claim is renewed before each", observed)
				}
			}
			if states := attemptStates(t, database, tc.table, tc.column, id); !slices.Equal(states, []string{"1:sent"}) {
				t.Fatalf("attempts = %v, want attempt 1 alone, sent on the claim renewed last", states)
			}
			if sends := listener.published(); len(sends) != 1 || sends[0] != (resumeSend{key: tc.key(id), target: "s1"}) {
				t.Fatalf("published %#v, want one send to s1 under %q", sends, tc.key(id))
			}
			if receipts := deliveryReceiptsOf(t, database, tc.receipt, tc.column, id); receipts != 1 {
				t.Fatalf("attempt 1 has %d receipts, want one", receipts)
			}
			if !strings.Contains(response.Body.String(), `"state":"sent"`) {
				t.Fatalf("the request answered %s, want its attempt sent", response.Body.String())
			}
		})
	}
}
