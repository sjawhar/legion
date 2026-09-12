package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"time"
)

func createIssueMessage(t *testing.T, handler http.Handler, issueKey string, input map[string]any, login string) model.Message {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/messages", input, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Message](t, response)
}

func TestCreateMessageAlwaysReturnsDeliveriesArray(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Message deliveries", "before")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "An ordinary message",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	deliveries, ok := payload["deliveries"]
	if !ok || string(deliveries) != "[]" {
		t.Fatalf("deliveries = %s, want []", deliveries)
	}
}

func newTargetedMessageHandler(t *testing.T, envoyURL string) (http.Handler, *store.Store) {
	t.Helper()
	database := openEmptyTestStore(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}, "bob": {}},
		},
		AgentToken: "agent-token", RepoProjectsRaw: "owner/repo=TEST", ServerURL: "https://dispatch.example",
		EnvoyURL: envoyURL, Docs: documentService, Events: broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux, database
}

func bearerRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode bearer request: %v", err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer agent-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestCreateMessageRejectsMalformedMissingAndCrossIssueInReplyTo(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Message threading", "before")
	other := createInteractionIssue(t, handler, "OTHER", "Other issue", "before")
	otherMessage := createIssueMessage(t, handler, other.Key, map[string]any{"body": "on the other issue"}, "alice")

	malformed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "reply", "in_reply_to": "not-a-uuid",
	}, "alice")
	if malformed.Code != http.StatusBadRequest || !strings.Contains(malformed.Body.String(), `"code":"MESSAGE_INPUT"`) {
		t.Fatalf("malformed in_reply_to: status=%d body=%s", malformed.Code, malformed.Body.String())
	}

	crossIssue := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "reply", "in_reply_to": otherMessage.ID,
	}, "alice")
	if crossIssue.Code != http.StatusBadRequest || !strings.Contains(crossIssue.Body.String(), `"code":"MESSAGE_INPUT"`) {
		t.Fatalf("cross-issue in_reply_to: status=%d body=%s", crossIssue.Code, crossIssue.Body.String())
	}

	missing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "reply", "in_reply_to": "5a660655-04ad-4ce0-8a9b-93dd03c412b7",
	}, "alice")
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), `"code":"MESSAGE_INPUT"`) {
		t.Fatalf("nonexistent in_reply_to: status=%d body=%s", missing.Code, missing.Body.String())
	}

	if rows := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice"); strings.Count(rows.Body.String(), `"type":"message.created"`) != 0 {
		t.Fatalf("rejected replies must not append events: %s", rows.Body.String())
	}
}

func TestCreateMessageReplyCarriesReplyBodyAndGetMessageReturnsChain(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Message threading", "before")
	root := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Ship the build tonight"}, "alice")
	reply := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Sounds good", "in_reply_to": root.ID}, "bob")
	if reply.InReplyTo == nil || *reply.InReplyTo != root.ID {
		t.Fatalf("reply.InReplyTo = %v, want %s", reply.InReplyTo, root.ID)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	if !strings.Contains(events.Body.String(), `"reply_body":"Ship the build tonight"`) {
		t.Fatalf("message.created event missing reply_body preview: %s", events.Body.String())
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/"+root.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("get message: status=%d body=%s", read.Code, read.Body.String())
	}
	decoded := decodeBody[struct {
		Message model.Message   `json:"message"`
		Replies []model.Message `json:"replies"`
	}](t, read)
	if decoded.Message.ID != root.ID {
		t.Fatalf("get message id = %s, want %s", decoded.Message.ID, root.ID)
	}
	if len(decoded.Replies) != 1 || decoded.Replies[0].ID != reply.ID {
		t.Fatalf("get message replies = %#v, want [%s]", decoded.Replies, reply.ID)
	}

	notFound := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/5a660655-04ad-4ce0-8a9b-93dd03c412b7", nil, "alice")
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("get missing message: status=%d body=%s", notFound.Code, notFound.Body.String())
	}

	otherIssue := createInteractionIssue(t, handler, "OTHER", "Different issue", "before")
	wrongIssue := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+otherIssue.Key+"/messages/"+root.ID, nil, "alice")
	if wrongIssue.Code != http.StatusNotFound {
		t.Fatalf("get message via wrong issue: status=%d body=%s", wrongIssue.Code, wrongIssue.Body.String())
	}
}

func TestMessageReferencingAnotherMessageCreatesAToKindMessageRef(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Message refs", "before")
	target := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "The decision"}, "alice")
	citing := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "See dispatch://" + issue.Key + "/message/" + target.ID,
	}, "bob")

	var toKind, toID string
	if err := database.Pool.QueryRow(context.Background(), `
		select to_kind, to_id from refs where from_kind = 'message' and from_id = $1
	`, citing.ID).Scan(&toKind, &toID); err != nil {
		t.Fatalf("read stored reference: %v", err)
	}
	if toKind != "message" || toID != target.ID {
		t.Fatalf("reference = (%q, %q), want (message, %s)", toKind, toID, target.ID)
	}
}

func TestTargetedMessageDeliveryRetriesAndAcceptsOnlyTargetReplies(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			if !live {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw"],"last_seen":42}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode listener send: %v", err)
			}
			sent = append(sent, request)
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Targeted message", "before")

	created := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "target": "session:s1", "delivery": "btw", "urgency": "high",
	}, "alice")
	if created.Target == nil || *created.Target != "session:s1" || len(created.Deliveries) != 1 {
		t.Fatalf("targeted message = %#v", created)
	}
	if delivery := created.Deliveries[0]; delivery.Attempt != 1 || delivery.State != "sent" ||
		delivery.EnvelopeID == nil || *delivery.EnvelopeID != "envelope-1" {
		t.Fatalf("first delivery = %#v", delivery)
	}
	if len(sent) != 1 || sent[0]["source"] != "dispatch" || sent[0]["target_session"] != "s1" ||
		sent[0]["expects_reply"] != "required" || sent[0]["idempotency_key"] != created.ID+":1" {
		t.Fatalf("listener request = %#v", sent)
	}
	payload, ok := sent[0]["payload"].(string)
	if !ok {
		t.Fatalf("listener payload = %#v, want JSON string", sent[0])
	}
	var frame struct {
		Event struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		} `json:"event"`
		Delivery struct {
			Attempt int    `json:"attempt"`
			Mode    string `json:"mode"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatalf("decode listener payload frame: %v", err)
	}
	if frame.Event.Type != "message.created" || frame.Event.Payload.ID != created.ID ||
		frame.Delivery.Attempt != 1 || frame.Delivery.Mode != "btw" {
		t.Fatalf("listener payload frame = %s", payload)
	}

	forbidden := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Don't interrupt", "target": "session:s1", "delivery": "btw",
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if forbidden.Code != http.StatusBadRequest || !strings.Contains(forbidden.Body.String(), `"code":"ACTOR_KIND"`) {
		t.Fatalf("bearer target = %d %s", forbidden.Code, forbidden.Body.String())
	}

	live = false
	failed := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Retry me", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if len(failed.Deliveries) != 1 || failed.Deliveries[0].State != "failed" ||
		failed.Deliveries[0].Error == nil || *failed.Deliveries[0].Error != "no live session s1" {
		t.Fatalf("failed delivery = %#v", failed.Deliveries)
	}

	live = true
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+failed.ID+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry delivery: status=%d body=%s", retry.Code, retry.Body.String())
	}
	retried := decodeBody[model.MessageDelivery](t, retry)
	if retried.Attempt != 2 || retried.Delivery != "steer" || retried.State != "sent" {
		t.Fatalf("retried delivery = %#v", retried)
	}

	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+failed.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "body": "Ship it.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply: status=%d body=%s", reply.Code, reply.Body.String())
	}
	answered := decodeBody[model.Message](t, reply)
	if answered.InReplyTo == nil || *answered.InReplyTo != failed.ID {
		t.Fatalf("answer = %#v", answered)
	}
	repeated := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+failed.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "body": "Ship it.",
	})
	if repeated.Code != http.StatusOK || decodeBody[model.Message](t, repeated).ID != answered.ID {
		t.Fatalf("repeated reply: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
	wrongSession := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+failed.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s2"}, "attempt": 2, "body": "No.",
	})
	if wrongSession.Code != http.StatusForbidden || !strings.Contains(wrongSession.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("wrong session reply: status=%d body=%s", wrongSession.Code, wrongSession.Body.String())
	}
}

func TestTargetedMessageRejectsInvalidDeliveryInputBeforePersisting(t *testing.T) {
	handler, _ := newTargetedMessageHandler(t, "")
	issue := createInteractionIssue(t, handler, "TEST", "Targeted input", "before")

	for _, input := range []map[string]any{
		{"body": "Missing target", "delivery": "btw"},
		{"body": "Invalid target", "target": "agent:s1", "delivery": "btw"},
		{"body": "Invalid delivery", "target": "session:s1", "delivery": "interrupt"},
	} {
		response := dispatchRequest(
			t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", input, "alice",
		)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"MESSAGE_INPUT"`) {
			t.Fatalf("input %v: status=%d body=%s", input, response.Code, response.Body.String())
		}
	}

	for _, input := range []map[string]any{
		{
			"body": "Bearer target", "target": "session:s1", "delivery": "btw",
			"actor": map[string]any{"kind": "session", "id": "s1"},
		},
		{
			"body": "Bearer delivery", "delivery": "btw",
			"actor": map[string]any{"kind": "session", "id": "s1"},
		},
	} {
		response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", input)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"ACTOR_KIND"`) {
			t.Fatalf("bearer input %v: status=%d body=%s", input, response.Code, response.Body.String())
		}
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if strings.Contains(events.Body.String(), `"type":"message.created"`) {
		t.Fatalf("invalid target delivery must not persist a message: %s", events.Body.String())
	}
}

func TestTargetedMessageRecordsCapabilityAndListenerFailures(t *testing.T) {
	capabilities := []string{"aside", "btw"}
	sendStatus := http.StatusOK
	sendError := ""
	sendCalls := 0
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"session_id": "s1", "title": "planner", "capabilities": capabilities,
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			sendCalls++
			w.WriteHeader(sendStatus)
			if sendError == "" {
				_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
				return
			}
			_, _ = w.Write([]byte(`{"error":"` + sendError + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Targeted failures", "before")

	capabilities = []string{}
	missingCapability := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can you receive BTW?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if got := missingCapability.Deliveries; len(got) != 1 || got[0].State != "failed" ||
		got[0].Error == nil || *got[0].Error != "session s1 (planner) does not advertise btw" {
		t.Fatalf("missing capability delivery = %#v", got)
	}
	if sendCalls != 0 {
		t.Fatalf("missing capability sent %d listener requests", sendCalls)
	}

	capabilities = []string{"aside", "btw"}
	sendStatus = http.StatusNotFound
	sendError = "no live session s1"
	listenerFailure := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can you receive now?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if got := listenerFailure.Deliveries; len(got) != 1 || got[0].State != "failed" ||
		got[0].Error == nil || *got[0].Error != "no live session s1" {
		t.Fatalf("listener 404 delivery = %#v", got)
	}
}

func TestTargetedMessagePrefixesUnavailableListenerErrors(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		handler, _ := newTargetedMessageHandler(t, "http://127.0.0.1:1")
		issue := createInteractionIssue(t, handler, "TEST", "Refused listener", "before")
		message := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "Can you receive?", "target": "session:s1", "delivery": "steer",
		}, "alice")
		if got := message.Deliveries; len(got) != 1 || got[0].State != "failed" ||
			got[0].Error == nil || !strings.HasPrefix(*got[0].Error, "envoy listener: ") {
			t.Fatalf("refused delivery = %#v", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		cancelled := make(chan struct{})
		listener := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
			close(cancelled)
		}))
		defer listener.Close()
		handler, _ := newTargetedMessageHandler(t, listener.URL)
		issue := createInteractionIssue(t, handler, "TEST", "Timeout listener", "before")
		message := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "Can you receive?", "target": "session:s1", "delivery": "steer",
		}, "alice")
		if got := message.Deliveries; len(got) != 1 || got[0].State != "failed" ||
			got[0].Error == nil || !strings.HasPrefix(*got[0].Error, "envoy listener: ") {
			t.Fatalf("timeout delivery = %#v", got)
		}
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("listener request was not cancelled after the Dispatch timeout")
		}
	})
}

func TestTargetedMessageResolvesRolesAndEnforcesReplyBoundaries(t *testing.T) {
	roleRequests := 0
	roleLive := true
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/roles/legion-planner":
			roleRequests++
			if !roleLive {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no holder for role legion-planner"}`))
				return
			}
			_, _ = w.Write([]byte(`{"role":"legion-planner","holder":"s1","title":"planner","capabilities":["aside","btw"]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatalf("decode listener delivery: %v", err)
			}
			if input["target_session"] != "s1" {
				t.Fatalf("role delivery target = %v, want s1", input["target_session"])
			}
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Role delivery", "before")

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "target": "role:legion-planner", "delivery": "btw",
	}, "alice")
	if got := message.Deliveries; len(got) != 1 || got[0].SessionID != "s1" || got[0].State != "sent" {
		t.Fatalf("role delivery = %#v", got)
	}
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry role delivery: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if roleRequests != 2 {
		t.Fatalf("role resolution requests = %d, want one per attempt", roleRequests)
	}

	roleLive = false
	noHolder := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Who is planning?", "target": "role:legion-planner", "delivery": "btw",
	}, "alice")
	if got := noHolder.Deliveries; len(got) != 1 || got[0].State != "failed" ||
		got[0].Error == nil || *got[0].Error != "no holder for role legion-planner" {
		t.Fatalf("missing role holder delivery = %#v", got)
	}

	wrongSession := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s2"}, "attempt": 1, "body": "No.",
	})
	if wrongSession.Code != http.StatusForbidden || !strings.Contains(wrongSession.Body.String(), `"code":"REPLY_FORBIDDEN"`) {
		t.Fatalf("wrong session reply: status=%d body=%s", wrongSession.Code, wrongSession.Body.String())
	}

	failedReply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "error": "No active model on session",
	})
	if failedReply.Code != http.StatusOK {
		t.Fatalf("ephemeral failure reply: status=%d body=%s", failedReply.Code, failedReply.Body.String())
	}
	failedAttempt := decodeBody[model.MessageDelivery](t, failedReply)
	if failedAttempt.State != "failed" || failedAttempt.Error == nil || *failedAttempt.Error != "No active model on session" ||
		failedAttempt.Delivery != "steer" || failedAttempt.SessionID != "s1" || failedAttempt.CreatedAt.IsZero() {
		t.Fatalf("ephemeral failure attempt = %#v", failedAttempt)
	}
	beforeRepeatedError := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	repeatedError := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "error": "No active model on session",
	})
	repeatedAttempt := decodeBody[model.MessageDelivery](t, repeatedError)
	if repeatedError.Code != http.StatusOK || repeatedAttempt.MessageID != failedAttempt.MessageID ||
		repeatedAttempt.Attempt != failedAttempt.Attempt || repeatedAttempt.Delivery != failedAttempt.Delivery ||
		repeatedAttempt.SessionID != failedAttempt.SessionID || repeatedAttempt.Error == nil ||
		*repeatedAttempt.Error != *failedAttempt.Error || !repeatedAttempt.CreatedAt.Equal(failedAttempt.CreatedAt) {
		t.Fatalf("repeated failed reply = %#v, want stored attempt %#v", repeatedAttempt, failedAttempt)
	}
	afterRepeatedError := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if strings.Count(afterRepeatedError.Body.String(), `"type":"message.delivery"`) != strings.Count(beforeRepeatedError.Body.String(), `"type":"message.delivery"`) {
		t.Fatalf("repeated failed reply appended an event: before=%s after=%s", beforeRepeatedError.Body.String(), afterRepeatedError.Body.String())
	}
	bodyAfterError := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "body": "Too late.",
	})
	if bodyAfterError.Code != http.StatusConflict || !strings.Contains(bodyAfterError.Body.String(), `"code":"ATTEMPT_FAILED"`) {
		t.Fatalf("body after failed reply: status=%d body=%s", bodyAfterError.Code, bodyAfterError.Body.String())
	}
	for _, path := range []string{
		"/api/v1/messages/5a660655-04ad-4ce0-8a9b-93dd03c412b7/reply",
		"/api/v1/messages/" + message.ID + "/reply",
	} {
		attempt := 1
		if strings.HasPrefix(path, "/api/v1/messages/"+message.ID) {
			attempt = 99
		}
		response := bearerRequest(t, handler, http.MethodPost, path, map[string]any{
			"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": attempt, "body": "Ship it.",
		})
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"MESSAGE_NOT_FOUND"`) {
			t.Fatalf("missing message or attempt %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}

	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Ship it.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[model.Message](t, answered)
	before := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	repeated := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "A different retry body.",
	})
	if repeated.Code != http.StatusOK || decodeBody[model.Message](t, repeated).ID != answer.ID {
		t.Fatalf("repeated reply: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
	after := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if strings.Count(after.Body.String(), `"type":"message.answered"`) != strings.Count(before.Body.String(), `"type":"message.answered"`) {
		t.Fatalf("repeated reply appended an event: before=%s after=%s", before.Body.String(), after.Body.String())
	}

	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	closedRetry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if closedRetry.Code != http.StatusConflict || !strings.Contains(closedRetry.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("delivery to closed issue: status=%d body=%s", closedRetry.Code, closedRetry.Body.String())
	}

	closedReply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "body": "Too late.",
	})
	if closedReply.Code != http.StatusConflict || !strings.Contains(closedReply.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("reply to closed issue: status=%d body=%s", closedReply.Code, closedReply.Body.String())
	}
}

func TestCreateMessageAuthenticatesBeforeReadingTheBody(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/TEST-1/messages", strings.NewReader("{"))
	request.Header.Set("Authorization", "Bearer invalid")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), `"code":"UNAUTHORIZED"`) {
		t.Fatalf("invalid bearer malformed JSON: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
