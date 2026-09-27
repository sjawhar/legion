package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// broadcastListener is a fake Envoy listener for the broadcast tests: it advertises the given
// sessions and records every send it is asked to make.
type broadcastListener struct {
	sync.Mutex
	sends []struct{ target, key, body string }
}

func newBroadcastListener(t *testing.T, sessionsJSON string) (*httptest.Server, *broadcastListener) {
	t.Helper()
	state := &broadcastListener{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessionsJSON))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request struct {
				TargetSession  string `json:"target_session"`
				IdempotencyKey string `json:"idempotency_key"`
				Message        string `json:"message"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode broadcast send: %v", err)
			}
			state.Lock()
			state.sends = append(state.sends, struct{ target, key, body string }{
				request.TargetSession, request.IdempotencyKey, request.Message,
			})
			state.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"envelope-` + request.TargetSession + `","recipient":"` + request.TargetSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, state
}

func (l *broadcastListener) targets() []string {
	l.Lock()
	defer l.Unlock()
	targets := make([]string, 0, len(l.sends))
	for _, send := range l.sends {
		targets = append(targets, send.target)
	}
	sort.Strings(targets)
	return targets
}

// broadcastResponse mirrors the create and read shapes the browser consumes.
type broadcastResponse struct {
	ID         string `json:"id"`
	Body       string `json:"body"`
	Delivery   string `json:"delivery"`
	Recipients []struct {
		SessionID string          `json:"session_id"`
		Message   model.Message   `json:"message"`
		Replies   []model.Message `json:"replies"`
	} `json:"recipients"`
	Excluded []struct {
		SessionID string `json:"session_id"`
		Title     string `json:"title"`
		Reason    string `json:"reason"`
	} `json:"excluded"`
}

const broadcastSessions = `[
	{"session_id":"planner","title":"Planner","capabilities":["btw","steer"],"last_seen":300},
	{"session_id":"tester","title":"Tester","capabilities":["btw","steer"],"last_seen":200},
	{"session_id":"reviewer","title":"Reviewer","capabilities":["aside"],"last_seen":100}
]`

// A broadcast is one message per recipient: each live session that advertises the mode gets a
// targeted message and a delivery attempt of its own, and a selection that mixes capabilities
// excludes the sessions that cannot take the mode rather than switching them to another one.
func TestBroadcastSendsOneMessagePerRecipientAndExcludesTheRest(t *testing.T) {
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body":     "Stand down and report status.",
		"delivery": "steer",
		// "planner" twice: a selection that names a session more than once is one recipient.
		"session_ids": []string{"planner", "tester", "reviewer", "ghost", "planner"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[broadcastResponse](t, response)
	if len(created.Recipients) != 2 {
		t.Fatalf("recipients = %#v, want planner and tester once each", created.Recipients)
	}
	messageIDs := map[string]struct{}{}
	for _, recipient := range created.Recipients {
		if recipient.Message.Target == nil || *recipient.Message.Target != "session:"+recipient.SessionID {
			t.Fatalf("%s message target = %v", recipient.SessionID, recipient.Message.Target)
		}
		if recipient.Message.Body != "Stand down and report status." {
			t.Fatalf("%s body = %q", recipient.SessionID, recipient.Message.Body)
		}
		messageIDs[recipient.Message.ID] = struct{}{}
	}
	delivered := awaitBroadcastDeliveries(t, handler, created.ID)
	for _, recipient := range delivered.Recipients {
		attempt := recipient.Message.Deliveries[0]
		if attempt.State != "sent" || attempt.Delivery != "steer" || attempt.SessionID != recipient.SessionID {
			t.Fatalf("%s attempt = %#v, want a steer sent to that session", recipient.SessionID, attempt)
		}
	}
	if len(messageIDs) != 2 {
		t.Fatalf("recipients share a message: %#v", created.Recipients)
	}
	if got := sendTargets(created); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("recipient sessions = %v", got)
	}
	if got := sends.targets(); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("listener sends = %v, want one per recipient", got)
	}

	excluded := map[string]string{}
	for _, item := range created.Excluded {
		excluded[item.SessionID] = item.Reason
	}
	if excluded["reviewer"] != "does not advertise steer" || excluded["ghost"] != "no live session" {
		t.Fatalf("excluded = %#v", created.Excluded)
	}
	if len(excluded) != 2 {
		t.Fatalf("excluded = %#v, want the reviewer and the ghost only", created.Excluded)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/"+created.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read broadcast: status=%d body=%s", read.Code, read.Body.String())
	}
	stored := decodeBody[broadcastResponse](t, read)
	if stored.Delivery != "steer" || stored.Body != "Stand down and report status." {
		t.Fatalf("stored broadcast = %#v", stored)
	}
	if got := sendTargets(stored); !equalStrings(got, []string{"planner", "tester"}) {
		t.Fatalf("stored recipients = %v", got)
	}
	for _, recipient := range stored.Recipients {
		if len(recipient.Message.Deliveries) != 1 || recipient.Message.Deliveries[0].State != "sent" {
			t.Fatalf("%s stored attempts = %#v", recipient.SessionID, recipient.Message.Deliveries)
		}
	}
}

func sendTargets(response broadcastResponse) []string {
	targets := make([]string, 0, len(response.Recipients))
	for _, recipient := range response.Recipients {
		targets = append(targets, recipient.SessionID)
	}
	sort.Strings(targets)
	return targets
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// Each recipient answers its own delivery, and the broadcast view is where every answer is
// read back together.
func TestBroadcastRecipientRepliesAppearInTheBroadcastAndItsSummary(t *testing.T) {
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	created := decodeBody[broadcastResponse](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner", "tester"},
	}, "alice"))
	var plannerMessage string
	for _, recipient := range awaitBroadcastDeliveries(t, handler, created.ID).Recipients {
		if recipient.SessionID == "planner" {
			plannerMessage = recipient.Message.ID
		}
	}
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+plannerMessage+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 1, "body": "Green.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("recipient reply: status=%d body=%s", reply.Code, reply.Body.String())
	}

	stored := decodeBody[broadcastResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/"+created.ID, nil, "alice"))
	answers := map[string]string{}
	for _, recipient := range stored.Recipients {
		for _, answer := range recipient.Replies {
			answers[recipient.SessionID] = answer.Body
		}
	}
	if answers["planner"] != "Green." {
		t.Fatalf("recipient replies = %#v, want the planner's answer under the planner", answers)
	}
	if _, answered := answers["tester"]; answered {
		t.Fatalf("the tester has not answered: %#v", answers)
	}

	// The same recipient answers a second attempt: a retry in another mode opens attempt 2,
	// and a late answer to it must not make the list read two answers from one recipient.
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+plannerMessage+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the planner's delivery: status=%d body=%s", retry.Code, retry.Body.String())
	}
	second := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+plannerMessage+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "planner"}, "attempt": 2, "body": "Still green.",
	})
	if second.Code != http.StatusCreated {
		t.Fatalf("answer the second attempt: status=%d body=%s", second.Code, second.Body.String())
	}

	summaries := decodeBody[[]struct {
		ID         string `json:"id"`
		Body       string `json:"body"`
		Delivery   string `json:"delivery"`
		Recipients int    `json:"recipients"`
		Replies    int    `json:"replies"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts", nil, "alice"))
	if len(summaries) != 1 {
		t.Fatalf("broadcast list = %#v, want the one send", summaries)
	}
	if summaries[0].ID != created.ID || summaries[0].Recipients != 2 || summaries[0].Replies != 1 {
		t.Fatalf("summary = %#v, want two recipients and one of them answered", summaries[0])
	}
}

// Nothing is written when no selected session can take the mode: the sender is told why
// rather than left with an empty broadcast to explain.
func TestBroadcastWithNoReachableRecipientIsRefusedAndWritesNothing(t *testing.T) {
	listener, sends := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "Anyone?", "delivery": "steer", "session_ids": []string{"reviewer", "ghost"},
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"BROADCAST_EMPTY"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, "Reviewer does not advertise steer") ||
		!strings.Contains(body, "ghost no live session") {
		t.Fatalf("the refusal must name each reason: %s", body)
	}
	if got := sends.targets(); len(got) != 0 {
		t.Fatalf("listener sends = %v, want none", got)
	}
	if summaries := decodeBody[[]json.RawMessage](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts", nil, "alice")); len(summaries) != 0 {
		t.Fatalf("broadcast list = %v, want nothing written", summaries)
	}
}

// Broadcasting is human-only, like the one-session route it is built from, and so is reading
// one back.
func TestBroadcastRoutesRefuseBearerCallers(t *testing.T) {
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	created := decodeBody[broadcastResponse](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner"},
	}, "alice"))

	for name, request := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"create": {http.MethodPost, "/api/v1/broadcasts", map[string]any{
			"body": "From a session", "delivery": "btw", "session_ids": []string{"planner"},
			"actor": map[string]string{"kind": "session", "id": "tester"},
		}},
		"list": {http.MethodGet, "/api/v1/broadcasts", nil},
		"read": {http.MethodGet, "/api/v1/broadcasts/" + created.ID, nil},
	} {
		t.Run(name, func(t *testing.T) {
			response := bearerRequest(t, handler, request.method, request.path, request.body)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestBroadcastRejectsMalformedInput(t *testing.T) {
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	tooMany := make([]string, maxBroadcastRecipients+1)
	for index := range tooMany {
		tooMany[index] = "session-" + string(rune('a'+index%26)) + string(rune('a'+index/26))
	}
	for name, input := range map[string]struct {
		body map[string]any
		code string
	}{
		"empty body":   {map[string]any{"body": "   ", "delivery": "btw", "session_ids": []string{"planner"}}, "INVALID_MESSAGE"},
		"unknown mode": {map[string]any{"body": "hi", "delivery": "shout", "session_ids": []string{"planner"}}, "BROADCAST_INPUT"},
		"no sessions":  {map[string]any{"body": "hi", "delivery": "btw", "session_ids": []string{" "}}, "BROADCAST_INPUT"},
		"over the cap": {map[string]any{"body": "hi", "delivery": "btw", "session_ids": tooMany}, "BROADCAST_INPUT"},
		"over the body cap": {
			map[string]any{"body": strings.Repeat("x", maxMessageBody16+1), "delivery": "btw", "session_ids": []string{"planner"}},
			"CAP_EXCEEDED",
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", input.body, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+input.code+`"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	missing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/5a660655-04ad-4ce0-8a9b-93dd03c412b7", nil, "alice")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"BROADCAST_NOT_FOUND"`) {
		t.Fatalf("unknown broadcast: status=%d body=%s", missing.Code, missing.Body.String())
	}
	malformed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/not-a-uuid", nil, "alice")
	if malformed.Code != http.StatusNotFound || !strings.Contains(malformed.Body.String(), `"code":"BROADCAST_NOT_FOUND"`) {
		t.Fatalf("malformed id: status=%d body=%s", malformed.Code, malformed.Body.String())
	}
}

// awaitBroadcastDeliveries reads the broadcast back until every recipient carries an attempt:
// delivery runs behind the create response, so a test that asserts on attempts waits for it.
func awaitBroadcastDeliveries(t *testing.T, handler http.Handler, id string) broadcastResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/broadcasts/"+id, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read broadcast: status=%d body=%s", read.Code, read.Body.String())
		}
		stored := decodeBody[broadcastResponse](t, read)
		settled := len(stored.Recipients) > 0
		for _, recipient := range stored.Recipients {
			if len(recipient.Message.Deliveries) == 0 || recipient.Message.Deliveries[0].State == "pending" {
				settled = false
			}
		}
		if settled {
			return stored
		}
		if time.Now().After(deadline) {
			t.Fatalf("recipients still without a settled attempt: %#v", stored.Recipients)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// LEGION-233 review, P1. Delivery used to run inside the create request, so a request cut off
// partway - a closed tab, a deploy's shutdown, the load balancer's idle timeout - left one
// attempt stranded pending and every later recipient with no attempt at all, and nothing
// recovered them. The send now answers as soon as the messages are committed and delivers
// behind the request, on the server's lifetime rather than the request's.
func TestBroadcastAnswersBeforeDeliveringAndStillReachesEveryRecipient(t *testing.T) {
	release := make(chan struct{})
	var listenerState struct {
		sync.Mutex
		sends int
	}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(broadcastSessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			<-release
			listenerState.Lock()
			listenerState.sends++
			listenerState.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"envelope","recipient":"s"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)

	// Every listener send is blocked, so a handler that delivered inside the request could not
	// answer at all.
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/broadcasts", map[string]any{
		"body": "Status?", "delivery": "btw", "session_ids": []string{"planner", "tester"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[broadcastResponse](t, response)
	listenerState.Lock()
	sends := listenerState.sends
	listenerState.Unlock()
	if sends != 0 {
		t.Fatalf("%d sends completed before the create answered; delivery must run behind it", sends)
	}
	// Every recipient carries its attempt from the moment the broadcast is written, opened
	// pending and unclaimed: a recipient with no attempt could only be moved by a delivery in
	// another mode, which is a second frame wherever the first one landed.
	for _, recipient := range created.Recipients {
		if len(recipient.Message.Deliveries) != 1 {
			t.Fatalf("%s carries %#v, want one attempt opened with the broadcast",
				recipient.SessionID, recipient.Message.Deliveries)
		}
		attempt := recipient.Message.Deliveries[0]
		if attempt.Attempt != 1 || attempt.State != "pending" || attempt.Delivery != "btw" ||
			attempt.SessionID != recipient.SessionID {
			t.Fatalf("%s attempt = %#v, want attempt 1 pending in the broadcast's mode",
				recipient.SessionID, attempt)
		}
	}

	close(release)
	delivered := awaitBroadcastDeliveries(t, handler, created.ID)
	if len(delivered.Recipients) != 2 {
		t.Fatalf("recipients = %#v", delivered.Recipients)
	}
	for _, recipient := range delivered.Recipients {
		// The worker settles the attempt the write opened; it never opens a second one beside it.
		if len(recipient.Message.Deliveries) != 1 || recipient.Message.Deliveries[0].Attempt != 1 ||
			recipient.Message.Deliveries[0].State != "sent" {
			t.Fatalf("%s attempts = %#v, want the one attempt settled sent",
				recipient.SessionID, recipient.Message.Deliveries)
		}
	}
}
