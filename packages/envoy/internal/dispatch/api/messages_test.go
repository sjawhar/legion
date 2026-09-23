package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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
	database := storetest.Open(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	allowed := map[string]struct{}{"alice": {}, "bob": {}}
	deps, err := NewDeps(DepsInput{
		Store:         database,
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: allowed},
		AllowedLogins: allowed,
		AgentToken:    "agent-token", RepoProjectsRaw: "owner/repo=TEST", ServerURL: "https://dispatch.example",
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
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"last_seen":42}]`))
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
	frame := decodeDeliveryFrame(t, sent[0])
	if frame.Event.Type != "message.created" || frame.Event.Payload.ID != created.ID ||
		frame.Delivery.Attempt != 1 || frame.Delivery.Mode != "btw" {
		t.Fatalf("listener payload frame = %s", sent[0]["payload"])
	}

	// An agent may ask another agent through Dispatch too; the card must show the asking
	// session as the author, never a human, and the frame the target receives says the same.
	asked := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Is the build green?", "target": "session:s1", "delivery": "btw",
		"actor": map[string]any{"kind": "session", "id": "s2"},
	})
	if asked.Code != http.StatusCreated {
		t.Fatalf("bearer target: status=%d body=%s", asked.Code, asked.Body.String())
	}
	agentAsked := decodeBody[model.Message](t, asked)
	if agentAsked.Author.Kind != "session" || agentAsked.Author.ID != "s2" ||
		len(agentAsked.Deliveries) != 1 || agentAsked.Deliveries[0].State != "sent" {
		t.Fatalf("agent-authored targeted message = %#v", agentAsked)
	}
	agentFrame := decodeDeliveryFrame(t, sent[1])
	if agentFrame.Event.Actor.Kind != "session" || agentFrame.Event.Actor.ID != "s2" {
		t.Fatalf("agent frame actor = %#v, want session s2", agentFrame.Event.Actor)
	}

	personalTokenResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{
		"name": "btw-owner",
	}, "alice")
	if personalTokenResponse.Code != http.StatusCreated {
		t.Fatalf("mint personal token: status=%d body=%s", personalTokenResponse.Code, personalTokenResponse.Body.String())
	}
	personalToken := decodeBody[agentTokenResponse](t, personalTokenResponse)
	personalSendIndex := len(sent)
	personalAskedResponse := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Can a personal bearer ask?", "target": "session:s1", "delivery": "btw",
		"actor": map[string]any{"kind": "session", "id": "s3", "owner": "mallory"},
	}, personalToken.Token)
	if personalAskedResponse.Code != http.StatusCreated {
		t.Fatalf("personal-token bearer target: status=%d body=%s", personalAskedResponse.Code, personalAskedResponse.Body.String())
	}
	personalAsked := decodeBody[model.Message](t, personalAskedResponse)
	if personalAsked.Author.Kind != "session" || personalAsked.Author.ID != "s3" ||
		personalAsked.Author.Owner == nil || *personalAsked.Author.Owner != "alice" ||
		len(personalAsked.Deliveries) != 1 || personalAsked.Deliveries[0].State != "sent" {
		t.Fatalf("personal-token targeted message = %#v, want session s3 owned by alice with sent delivery", personalAsked)
	}
	if len(sent) != personalSendIndex+1 {
		t.Fatalf("personal-token target sent %d listener requests, want %d", len(sent), personalSendIndex+1)
	}
	personalFrame := decodeDeliveryFrame(t, sent[personalSendIndex])
	if personalFrame.Event.Actor.Kind != "session" || personalFrame.Event.Actor.ID != "s3" ||
		personalFrame.Event.Actor.Owner == nil || *personalFrame.Event.Actor.Owner != "alice" {
		t.Fatalf("personal-token frame actor = %#v, want session s3 owned by alice", personalFrame.Event.Actor)
	}

	anonymous := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Who is asking?", "target": "session:s1", "delivery": "btw",
	})
	if anonymous.Code != http.StatusBadRequest || !strings.Contains(anonymous.Body.String(), `"code":"ACTOR_KIND"`) {
		t.Fatalf("bearer target without actor = %d %s", anonymous.Code, anonymous.Body.String())
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
	agentRetry := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+failed.ID+"/deliveries", map[string]any{
		"delivery": "btw", "actor": map[string]any{"kind": "session", "id": "s2"},
	})
	if agentRetry.Code != http.StatusCreated {
		t.Fatalf("bearer retry: status=%d body=%s", agentRetry.Code, agentRetry.Body.String())
	}
	if attempt := decodeBody[model.MessageDelivery](t, agentRetry); attempt.Attempt != 3 || attempt.Delivery != "btw" || attempt.State != "sent" {
		t.Fatalf("bearer retried delivery = %#v", attempt)
	}
	retryEventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if retryEventsResponse.Code != http.StatusOK {
		t.Fatalf("read retry events: status=%d body=%s", retryEventsResponse.Code, retryEventsResponse.Body.String())
	}
	retryEvents := decodeBody[[]struct {
		Type    string      `json:"type"`
		Actor   model.Actor `json:"actor"`
		Payload struct {
			MessageID string `json:"message_id"`
			Attempt   int    `json:"attempt"`
		} `json:"payload"`
	}](t, retryEventsResponse)
	foundAgentRetryEvent := false
	for _, event := range retryEvents {
		if event.Type == "message.delivery" && event.Payload.MessageID == failed.ID && event.Payload.Attempt == 3 {
			foundAgentRetryEvent = true
			if event.Actor.Kind != "session" || event.Actor.ID != "s2" {
				t.Fatalf("bearer retry event actor = %#v, want session s2", event.Actor)
			}
		}
	}
	if !foundAgentRetryEvent {
		t.Fatalf("bearer retry event for message %s attempt 3 not found in %#v", failed.ID, retryEvents)
	}
	anonymousRetry := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+failed.ID+"/deliveries", map[string]any{"delivery": "btw"})
	if anonymousRetry.Code != http.StatusBadRequest || !strings.Contains(anonymousRetry.Body.String(), `"code":"ACTOR_KIND"`) {
		t.Fatalf("bearer retry without actor = %d %s", anonymousRetry.Code, anonymousRetry.Body.String())
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
	handler, database := newTargetedMessageHandler(t, "")
	issue := createInteractionIssue(t, handler, "TEST", "Targeted input", "before")

	assertRejected := func(label string, response *httptest.ResponseRecorder, message string) {
		t.Helper()
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", label, response.Code, response.Body.String())
		}
		body := decodeBody[struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}](t, response)
		if body.Code != "MESSAGE_INPUT" || body.Error != message {
			t.Fatalf("%s: error=%#v, want MESSAGE_INPUT %q", label, body, message)
		}
	}

	cases := []struct {
		name    string
		input   map[string]any
		message string
	}{
		{
			name:    "missing target",
			input:   map[string]any{"body": "Missing target", "delivery": "btw"},
			message: "delivery requires target",
		},
		{
			name:    "invalid target",
			input:   map[string]any{"body": "Invalid target", "target": "agent:s1", "delivery": "btw"},
			message: "target must be role:<name> or session:<id>",
		},
		{
			name:    "invalid delivery",
			input:   map[string]any{"body": "Invalid delivery", "target": "session:s1", "delivery": "interrupt"},
			message: "delivery must be one of btw, aside, steer",
		},
		{
			name: "invalid urgency",
			input: map[string]any{
				"body": "Invalid urgency", "target": "session:s1", "delivery": "btw", "urgency": "soon",
			},
			message: "urgency must be one of low, med, high, blocking",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			human := dispatchRequest(
				t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", test.input, "alice",
			)
			assertRejected("human "+test.name, human, test.message)

			bearerInput := make(map[string]any, len(test.input)+1)
			for key, value := range test.input {
				bearerInput[key] = value
			}
			bearerInput["actor"] = map[string]any{"kind": "session", "id": "s1"}
			bearer := bearerRequest(
				t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", bearerInput,
			)
			assertRejected("bearer "+test.name, bearer, test.message)
		})
	}

	var messageCount int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from messages where issue_key = $1
	`, issue.Key).Scan(&messageCount); err != nil {
		t.Fatalf("count rejected messages: %v", err)
	}
	if messageCount != 0 {
		t.Fatalf("invalid target delivery persisted %d messages, want 0", messageCount)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if strings.Contains(events.Body.String(), `"type":"message.created"`) {
		t.Fatalf("invalid target delivery must not append an event: %s", events.Body.String())
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
	unadvertisedSteer := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can you receive a steer?", "target": "session:s1", "delivery": "steer",
	}, "alice")
	if got := unadvertisedSteer.Deliveries; len(got) != 1 || got[0].State != "failed" ||
		got[0].Error == nil || *got[0].Error != "session s1 (planner) does not advertise steer" {
		t.Fatalf("unadvertised steer delivery = %#v", got)
	}
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+missingCapability.ID+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry unadvertised steer: status=%d body=%s", retry.Code, retry.Body.String())
	}
	retriedSteer := decodeBody[model.MessageDelivery](t, retry)
	if retriedSteer.State != "failed" || retriedSteer.Error == nil ||
		*retriedSteer.Error != "session s1 (planner) does not advertise steer" {
		t.Fatalf("retry unadvertised steer = %#v", retriedSteer)
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
			_, _ = w.Write([]byte(`{"role":"legion-planner","holder":"s1","title":"planner","capabilities":["aside","btw","steer"]}`))
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
	// The agent reported a failure for attempt 2 and then recovered: its answer is the truth
	// about the delivery, so the body is recorded and the attempt reads as sent and answered.
	bodyAfterError := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "body": "Not too late.",
	})
	if bodyAfterError.Code != http.StatusCreated {
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

func TestAgentTargetedMessagesRequireHumanAndKeepTheirOwnConversation(t *testing.T) {
	sendStatus := http.StatusOK
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			w.WriteHeader(sendStatus)
			if sendStatus == http.StatusOK {
				_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
				return
			}
			_, _ = w.Write([]byte(`{"error":"no live session s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)

	nonHuman := bearerRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Can I send this?", "delivery": "btw", "actor": map[string]any{"kind": "session", "id": "s2"},
	})
	if nonHuman.Code != http.StatusForbidden || !strings.Contains(nonHuman.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("agent-authenticated creation: status=%d body=%s", nonHuman.Code, nonHuman.Body.String())
	}

	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "First question", "delivery": "btw",
	}, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("first issue-less message: status=%d body=%s", first.Code, first.Body.String())
	}
	firstMessage := decodeBody[model.Message](t, first)
	if firstMessage.Target == nil || *firstMessage.Target != "session:s1" ||
		len(firstMessage.Deliveries) != 1 || firstMessage.Deliveries[0].State != "sent" {
		t.Fatalf("first issue-less message = %#v", firstMessage)
	}
	var sequence int
	if err := database.Pool.QueryRow(context.Background(),
		`select seq from events where issue_key is null order by id limit 1`,
	).Scan(&sequence); err != nil {
		t.Fatalf("read issue-less message event sequence: %v", err)
	}
	if sequence != 0 {
		t.Fatalf("issue-less message event sequence = %d, want 0", sequence)
	}

	sendStatus = http.StatusNotFound
	failed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Second question", "delivery": "btw",
	}, "alice")
	if failed.Code != http.StatusCreated {
		t.Fatalf("failed issue-less message: status=%d body=%s", failed.Code, failed.Body.String())
	}
	failedMessage := decodeBody[model.Message](t, failed)
	if len(failedMessage.Deliveries) != 1 || failedMessage.Deliveries[0].State != "failed" ||
		failedMessage.Deliveries[0].Error == nil || *failedMessage.Deliveries[0].Error != "no live session s1" {
		t.Fatalf("failed issue-less delivery = %#v", failedMessage.Deliveries)
	}

	sendStatus = http.StatusOK
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+failedMessage.ID+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry issue-less delivery: status=%d body=%s", retry.Code, retry.Body.String())
	}
	retried := decodeBody[model.MessageDelivery](t, retry)
	if retried.Attempt != 2 || retried.State != "sent" {
		t.Fatalf("retried issue-less delivery = %#v", retried)
	}

	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+failedMessage.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": retried.Attempt, "body": "It is ready.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply to issue-less message: status=%d body=%s", reply.Code, reply.Body.String())
	}

	if _, err := database.Pool.Exec(context.Background(),
		`update messages set created_at = created_at + interval '1 second' where id = $1`, failedMessage.ID,
	); err != nil {
		t.Fatalf("order issue-less messages: %v", err)
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list agent messages: status=%d body=%s", listed.Code, listed.Body.String())
	}
	messages := decodeBody[[]struct {
		Message model.Message   `json:"message"`
		Replies []model.Message `json:"replies"`
	}](t, listed)
	if len(messages) != 2 || messages[0].Message.ID != failedMessage.ID ||
		len(messages[0].Message.Deliveries) != 2 || len(messages[0].Replies) != 1 ||
		messages[0].Replies[0].Body != "It is ready." || messages[1].Message.ID != firstMessage.ID {
		t.Fatalf("agent messages = %#v", messages)
	}
}

func TestAgentConversationReplyStaysInThatAgentsConversation(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"]},{"session_id":"s2","title":"reviewer","capabilities":["aside","btw","steer"]}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Issue message", "before")

	root := decodeBody[model.Message](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "First question", "delivery": "btw",
	}, "alice"))
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "First answer.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[model.Message](t, answered)

	// A reply to the agent's answer continues s1's conversation and is delivered to s1.
	followUp := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Thanks - one more thing.", "delivery": "steer", "in_reply_to": answer.ID,
	}, "alice")
	if followUp.Code != http.StatusCreated {
		t.Fatalf("follow-up: status=%d body=%s", followUp.Code, followUp.Body.String())
	}
	if got := decodeBody[model.Message](t, followUp); got.InReplyTo == nil || *got.InReplyTo != answer.ID ||
		len(got.Deliveries) != 1 || got.Deliveries[0].SessionID != "s1" {
		t.Fatalf("follow-up = %#v", got)
	}

	// Another agent's conversation cannot claim s1's thread, and an issue message is not an
	// issue-less parent.
	crossAgent := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s2/messages", map[string]any{
		"body": "Hijack", "delivery": "steer", "in_reply_to": answer.ID,
	}, "alice")
	if crossAgent.Code != http.StatusBadRequest || !strings.Contains(crossAgent.Body.String(), `"code":"MESSAGE_INPUT"`) {
		t.Fatalf("cross-agent reply: status=%d body=%s", crossAgent.Code, crossAgent.Body.String())
	}
	issueMessage := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "On the issue"}, "alice")
	crossIssue := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Wrong conversation", "delivery": "steer", "in_reply_to": issueMessage.ID,
	}, "alice")
	if crossIssue.Code != http.StatusBadRequest || !strings.Contains(crossIssue.Body.String(), `"code":"MESSAGE_INPUT"`) {
		t.Fatalf("issue parent for agent reply: status=%d body=%s", crossIssue.Code, crossIssue.Body.String())
	}
}

// deliveryFrame is the {event, delivery} frame a targeted send carries as its `payload`
// string, decoded once here so no test spells a frame field a second time.
type deliveryFrame struct {
	Event struct {
		Type    string      `json:"type"`
		Actor   model.Actor `json:"actor"`
		Payload struct {
			ID           string  `json:"id"`
			InReplyTo    *string `json:"in_reply_to"`
			ReplyBody    string  `json:"reply_body"`
			ThreadTarget *string `json:"thread_target"`
		} `json:"payload"`
	} `json:"event"`
	Delivery struct {
		Attempt int    `json:"attempt"`
		Mode    string `json:"mode"`
	} `json:"delivery"`
}

func decodeDeliveryFrame(t *testing.T, send map[string]any) deliveryFrame {
	t.Helper()
	payload, ok := send["payload"].(string)
	if !ok {
		t.Fatalf("listener payload = %#v, want a JSON string", send)
	}
	var frame deliveryFrame
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatalf("decode delivery frame %s: %v", payload, err)
	}
	return frame
}

// sessionListener is a fake Envoy listener with one live session s1 (planner, aside+btw+steer) that
// records every send; `live` toggles whether s1 is listed.
func sessionListener(t *testing.T, live *bool, sent *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			if !*live {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"session_id":"s1","title":"planner","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"last_seen":42}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode listener send: %v", err)
			}
			*sent = append(*sent, request)
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
}

func TestReplyOnFailedAttemptRecordsTheAnswer(t *testing.T) {
	live := false
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Failed then answered", "before")

	// The listener listed no session, so Dispatch recorded the attempt as failed - yet the
	// message did reach the session, which now answers through the attempt it was handed.
	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Did you get this?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "failed" {
		t.Fatalf("failed delivery = %#v", message.Deliveries)
	}
	before := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	reply := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Loud and clear.",
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply on failed attempt: status=%d body=%s", reply.Code, reply.Body.String())
	}
	answer := decodeBody[model.Message](t, reply)
	if answer.InReplyTo == nil || *answer.InReplyTo != message.ID || answer.Body != "Loud and clear." {
		t.Fatalf("answer = %#v", answer)
	}
	after := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if strings.Count(after.Body.String(), `"type":"message.answered"`) != strings.Count(before.Body.String(), `"type":"message.answered"`)+1 {
		t.Fatalf("answer did not append one message.answered event: before=%s after=%s", before.Body.String(), after.Body.String())
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/"+message.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("get message: status=%d body=%s", read.Code, read.Body.String())
	}
	stored := decodeBody[messageRead](t, read)
	if len(stored.Message.Deliveries) != 1 {
		t.Fatalf("deliveries = %#v", stored.Message.Deliveries)
	}
	if attempt := stored.Message.Deliveries[0]; attempt.State != "sent" || attempt.Error != nil ||
		attempt.ReplyID == nil || *attempt.ReplyID != answer.ID {
		t.Fatalf("answered attempt = %#v, want sent with reply %s and no error", attempt, answer.ID)
	}
	if len(stored.Replies) != 1 || stored.Replies[0].ID != answer.ID {
		t.Fatalf("replies = %#v, want [%s]", stored.Replies, answer.ID)
	}

	repeated := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Loud and clear.",
	})
	if repeated.Code != http.StatusOK || decodeBody[model.Message](t, repeated).ID != answer.ID {
		t.Fatalf("repeated reply: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
}

func TestHumanReplyInheritsTheThreadTarget(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Threaded follow-up", "before")

	root := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Once the build is green.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[model.Message](t, answered)

	// A human's follow-up on the agent's answer names no target; the thread's root does, so
	// the follow-up is delivered to that session the way the thread was last delivered.
	sendsBefore := len(sent)
	followUp := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "It is green now - ship it.", "in_reply_to": answer.ID,
	}, "alice")
	if followUp.InReplyTo == nil || *followUp.InReplyTo != answer.ID {
		t.Fatalf("follow-up = %#v", followUp)
	}
	if followUp.Target == nil || *followUp.Target != "session:s1" {
		t.Fatalf("follow-up target = %v, want session:s1 inherited from the thread root", followUp.Target)
	}
	if len(followUp.Deliveries) != 1 || followUp.Deliveries[0].Delivery != "btw" ||
		followUp.Deliveries[0].State != "sent" || followUp.Deliveries[0].SessionID != "s1" {
		t.Fatalf("follow-up deliveries = %#v, want one sent btw attempt to s1", followUp.Deliveries)
	}
	if len(sent) != sendsBefore+1 || sent[sendsBefore]["target_session"] != "s1" {
		t.Fatalf("listener sends = %#v, want one more to s1", sent[sendsBefore:])
	}
	frame := decodeDeliveryFrame(t, sent[sendsBefore])
	if frame.Event.Type != "message.created" || frame.Event.Payload.ID != followUp.ID ||
		frame.Event.Payload.InReplyTo == nil || *frame.Event.Payload.InReplyTo != answer.ID ||
		frame.Event.Payload.ReplyBody != "Once the build is green." {
		t.Fatalf("follow-up frame = %s, want the reply's id, in_reply_to, and parent preview", sent[sendsBefore]["payload"])
	}

	// The session answers the follow-up through its own attempt, threading under it.
	second := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+followUp.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Shipping.",
	})
	if second.Code != http.StatusCreated {
		t.Fatalf("answer to follow-up: status=%d body=%s", second.Code, second.Body.String())
	}
	if got := decodeBody[model.Message](t, second); got.InReplyTo == nil || *got.InReplyTo != followUp.ID {
		t.Fatalf("second answer = %#v", got)
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/"+root.ID, nil, "alice")
	chain := decodeBody[messageRead](t, read)
	if len(chain.Replies) != 3 || chain.Replies[1].ID != followUp.ID ||
		len(chain.Replies[1].Deliveries) != 1 || chain.Replies[1].Deliveries[0].State != "sent" {
		t.Fatalf("reply chain = %#v, want answer, delivered follow-up, second answer", chain.Replies)
	}

	// An explicit target on the reply wins over the inherited one (here: same session, steer).
	explicit := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Actually, one more thing.", "in_reply_to": answer.ID, "target": "session:s1", "delivery": "steer",
	}, "alice")
	if len(explicit.Deliveries) != 1 || explicit.Deliveries[0].Delivery != "steer" {
		t.Fatalf("explicit delivery = %#v, want steer", explicit.Deliveries)
	}

	// A session's own reply in the thread is never bounced back to the target session, and a
	// human's reply on an untargeted thread stays an ordinary message.
	sendsBefore = len(sent)
	agentReply := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Noted.", "in_reply_to": followUp.ID, "actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if agentReply.Code != http.StatusCreated {
		t.Fatalf("agent reply: status=%d body=%s", agentReply.Code, agentReply.Body.String())
	}
	if got := decodeBody[model.Message](t, agentReply); got.Target != nil || len(got.Deliveries) != 0 {
		t.Fatalf("agent reply = %#v, want no target and no delivery", got)
	}
	plainRoot := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "A note."}, "bob")
	plainReply := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Seen.", "in_reply_to": plainRoot.ID}, "alice")
	if plainReply.Target != nil || len(plainReply.Deliveries) != 0 || len(sent) != sendsBefore {
		t.Fatalf("plain reply = %#v (sends=%d, want %d)", plainReply, len(sent), sendsBefore)
	}
}

// A session answering an issue message through POST /issues/{key}/messages names no target -
// the target would be itself - so the event's own `target` names no conversation. The Agents
// page groups the reply under the thread root it answers, so the event has to name that root's
// target or the open card never refreshes.
func TestUntargetedSessionReplyNamesItsThreadTarget(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Untargeted session reply", "before")

	root := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	reply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Once the build is green.", "in_reply_to": root.ID,
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("session reply: status=%d body=%s", reply.Code, reply.Body.String())
	}
	if stored := decodeBody[model.Message](t, reply); stored.Target != nil {
		t.Fatalf("session reply target = %#v, want none", stored.Target)
	}

	conversation := dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice")
	if conversation.Code != http.StatusOK || !strings.Contains(conversation.Body.String(), "Once the build is green.") {
		t.Fatalf("agent conversation: status=%d body=%s", conversation.Code, conversation.Body.String())
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", events.Code, events.Body.String())
	}
	log := decodeBody[[]struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}](t, events)
	answered := 0
	for _, entry := range log {
		if entry.Type != "message.answered" {
			continue
		}
		answered++
		if entry.Payload["target"] != nil {
			t.Fatalf("message.answered target = %#v, want none", entry.Payload["target"])
		}
		if entry.Payload["thread_target"] != "session:s1" {
			t.Fatalf("message.answered thread_target = %#v, want session:s1", entry.Payload["thread_target"])
		}
	}
	if answered != 1 {
		t.Fatalf("message.answered events = %d, want 1", answered)
	}
}

// threadTargetOf is the thread_target a message's own event carried, and whether the payload
// named one at all. An agent conversation has no issue event log to read, so both come from
// the events row.
func threadTargetOf(t *testing.T, database *store.Store, messageID string) (string, bool) {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events
		where type in ('message.created', 'message.answered') and payload->>'id' = $1
	`, messageID).Scan(&raw); err != nil {
		t.Fatalf("read the event of message %s: %v", messageID, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode the event payload of message %s: %v", messageID, err)
	}
	target, named := payload["thread_target"].(string)
	return target, named
}

// Every reply event names the target of its thread root - the conversation the reply lands in
// - whatever shape the thread has: the root may be the message answered or an ancestor of it,
// the thread may carry no target at all, and an agent conversation has no issue.
func TestReplyEventsNameTheirThreadRootTarget(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Thread targets", "before")

	// The message a session answers is the thread root, so the root's target is its own.
	root := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Once the build is green.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer the root: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[model.Message](t, answered)
	if target, named := threadTargetOf(t, database, answer.ID); !named || target != "session:s1" {
		t.Fatalf("answer thread_target = %q (named=%v), want session:s1", target, named)
	}

	// A session's own reply names no target, so a reply under it has to reach past its parent
	// to the root to name the conversation at all.
	midway := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Working on it.", "in_reply_to": root.ID, "actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if midway.Code != http.StatusCreated {
		t.Fatalf("session reply to the root: status=%d body=%s", midway.Code, midway.Body.String())
	}
	midwayReply := decodeBody[model.Message](t, midway)
	if midwayReply.Target != nil {
		t.Fatalf("session reply target = %#v, want none", midwayReply.Target)
	}
	deep := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Still working.", "in_reply_to": midwayReply.ID, "actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if deep.Code != http.StatusCreated {
		t.Fatalf("deep session reply: status=%d body=%s", deep.Code, deep.Body.String())
	}
	if target, named := threadTargetOf(t, database, decodeBody[model.Message](t, deep).ID); !named || target != "session:s1" {
		t.Fatalf("deep reply thread_target = %q (named=%v), want the root's session:s1", target, named)
	}

	// A human's reply that names no target inherits the root's and names it as the thread's.
	followUp := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "It is green now - ship it.", "in_reply_to": midwayReply.ID,
	}, "alice")
	if followUp.Target == nil || *followUp.Target != "session:s1" {
		t.Fatalf("follow-up target = %v, want session:s1 inherited from the root", followUp.Target)
	}
	if target, named := threadTargetOf(t, database, followUp.ID); !named || target != "session:s1" {
		t.Fatalf("inheriting follow-up thread_target = %q (named=%v), want session:s1", target, named)
	}

	// The reply endpoint walks the same way when the message it answers is not the root.
	deepAnswered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+followUp.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Shipping.",
	})
	if deepAnswered.Code != http.StatusCreated {
		t.Fatalf("answer below the root: status=%d body=%s", deepAnswered.Code, deepAnswered.Body.String())
	}
	if target, named := threadTargetOf(t, database, decodeBody[model.Message](t, deepAnswered).ID); !named || target != "session:s1" {
		t.Fatalf("deep answer thread_target = %q (named=%v), want session:s1", target, named)
	}

	// An untargeted thread is no conversation, so its replies name none.
	plainRoot := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "A note."}, "alice")
	plain := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Seen.", "in_reply_to": plainRoot.ID, "actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if plain.Code != http.StatusCreated {
		t.Fatalf("untargeted reply: status=%d body=%s", plain.Code, plain.Body.String())
	}
	if target, named := threadTargetOf(t, database, decodeBody[model.Message](t, plain).ID); named {
		t.Fatalf("reply under an untargeted root named thread_target %q, want none", target)
	}

	// An agent conversation has no issue, and a reply inside it still names that conversation.
	agentRoot := decodeBody[model.Message](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "First question", "delivery": "btw",
	}, "alice"))
	agentAnswered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+agentRoot.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "First answer.",
	})
	if agentAnswered.Code != http.StatusCreated {
		t.Fatalf("answer in the agent conversation: status=%d body=%s", agentAnswered.Code, agentAnswered.Body.String())
	}
	agentAnswer := decodeBody[model.Message](t, agentAnswered)
	agentFollowUp := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "One more thing.", "delivery": "steer", "in_reply_to": agentAnswer.ID,
	}, "alice")
	if agentFollowUp.Code != http.StatusCreated {
		t.Fatalf("agent conversation follow-up: status=%d body=%s", agentFollowUp.Code, agentFollowUp.Body.String())
	}
	followUpMessage := decodeBody[model.Message](t, agentFollowUp)
	if target, named := threadTargetOf(t, database, followUpMessage.ID); !named || target != "session:s1" {
		t.Fatalf("agent conversation follow-up thread_target = %q (named=%v), want session:s1", target, named)
	}
	// Three deep in that conversation, so the root is two hops above the parent and only the
	// walk can find it.
	agentSecond := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+followUpMessage.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Done.",
	})
	if agentSecond.Code != http.StatusCreated {
		t.Fatalf("second answer in the agent conversation: status=%d body=%s", agentSecond.Code, agentSecond.Body.String())
	}
	if target, named := threadTargetOf(t, database, decodeBody[model.Message](t, agentSecond).ID); !named || target != "session:s1" {
		t.Fatalf("agent conversation second answer thread_target = %q (named=%v), want session:s1", target, named)
	}
}

// A session reads the thread a message belongs to off the delivery frame it is woken with,
// not off the issue event log, so the frame has to name the same conversation the stored
// event does - on the first attempt and on a retry, which derives it from the stored row.
func TestReplyDeliveryFrameNamesItsThreadTarget(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Delivery frame thread", "before")

	// Each step is read off the send it produced rather than off the tail of `sent`, so a
	// step that stopped sending fails here instead of silently re-reading the previous frame.
	threadTargetOfSend := func(step string, sendsBefore int) (string, bool) {
		t.Helper()
		if len(sent) != sendsBefore+1 {
			t.Fatalf("%s listener sends = %d, want %d", step, len(sent), sendsBefore+1)
		}
		target := decodeDeliveryFrame(t, sent[sendsBefore]).Event.Payload.ThreadTarget
		if target == nil {
			return "", false
		}
		return *target, true
	}

	sendsBefore := len(sent)
	root := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	if target, named := threadTargetOfSend("root", sendsBefore); named {
		t.Fatalf("root frame thread_target = %q, want none - a root message is its own thread", target)
	}
	// A session's own reply names no target, so a message hanging under it has a parent whose
	// target is not the root's: only the walk to the root can name the conversation.
	midway := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Once the build is green.", "in_reply_to": root.ID,
		"actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if midway.Code != http.StatusCreated {
		t.Fatalf("session reply to the root: status=%d body=%s", midway.Code, midway.Body.String())
	}
	midwayReply := decodeBody[model.Message](t, midway)
	if midwayReply.Target != nil {
		t.Fatalf("session reply target = %#v, want none", midwayReply.Target)
	}

	sendsBefore = len(sent)
	followUp := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "It is green now - ship it.", "in_reply_to": midwayReply.ID,
	}, "alice")
	if target, named := threadTargetOfSend("follow-up", sendsBefore); !named || target != "session:s1" {
		t.Fatalf("follow-up frame thread_target = %q (named=%v), want session:s1", target, named)
	}

	// A retry derives the thread from the stored message instead of the write that made it,
	// and it is the same deep thread, so the walk is load-bearing on this path too.
	sendsBefore = len(sent)
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+followUp.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the follow-up: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if target, named := threadTargetOfSend("retry", sendsBefore); !named || target != "session:s1" {
		t.Fatalf("retry frame thread_target = %q (named=%v), want session:s1", target, named)
	}
}
