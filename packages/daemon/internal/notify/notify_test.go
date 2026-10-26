package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The controller topic is the one the plugin's controller subscribes to
// (legionControllerNoticeSubject, packages/contracts/src/subject.ts), byte for byte.
func TestControllerTopicIsTheControllersNoticeSubject(t *testing.T) {
	if got := ControllerTopic("legion"); got != "notifications.legion.legion.controller" {
		t.Fatalf("ControllerTopic = %q, want the controller's notice subject", got)
	}
}

// Every publish carries `"handoff": false`, so the listener never hands a daemon notice off to the
// daemon in place of answering a role with no holder (plan 2.2): the field is present and false.
func TestHTTPPublisherSendsPayloadAndOutboxDedupeKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The Envoy listener answers an unauthenticated publish with 401 (packages/envoy/cmd/listener/apiauth.go).
		if r.Header.Get("Authorization") != "Bearer envoy-bearer" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/publish" {
			t.Fatalf("request = %s %s, want POST /v1/messages/publish", r.Method, r.URL.Path)
		}
		var body struct {
			Topic     string  `json:"topic"`
			Message   string  `json:"message"`
			Payload   *string `json:"payload"` // the listener's messageBody (packages/envoy/cmd/listener/api.go:68-80)
			DedupeKey string  `json:"dedupe_key"`
			Handoff   *bool   `json:"handoff"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest) // the listener answers "invalid json"
			return
		}
		if body.Topic != "notifications.legion.LEGION.LEGION-208" || body.Message != "worker-died on LEGION-208" || body.DedupeKey != "legion-outbox:42" {
			t.Fatalf("publish body = %#v", body)
		}
		if body.Handoff == nil || *body.Handoff {
			t.Fatalf("publish handoff = %v, want present and false", body.Handoff)
		}
		var payload map[string]string
		if body.Payload == nil {
			t.Fatal("publish carried no payload")
		}
		if err := json.Unmarshal([]byte(*body.Payload), &payload); err != nil {
			t.Fatalf("decode publish payload: %v", err)
		}
		if payload["kind"] != "worker-died" {
			t.Fatalf("payload = %#v, want worker-died", payload)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := New(server.URL, "envoy-bearer").Publish(context.Background(), "notifications.legion.LEGION.LEGION-208", "worker-died on LEGION-208", map[string]string{"kind": "worker-died"}, "legion-outbox:42"); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestHTTPPublisherRefusalNamesTheListenerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The listener writes the JetStream publish error as {"error": ...} with 500
		// (packages/envoy/cmd/listener/api.go publishHandler); the log line is the only place it surfaces.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"nats: no response from stream"}`))
	}))
	defer server.Close()

	err := New(server.URL, "").Publish(context.Background(), "notifications.legion.LEGION.LEGION-208", "pr-blocked on LEGION-208", map[string]string{"kind": "pr-blocked"}, "legion-outbox:7")
	if err == nil || !strings.Contains(err.Error(), "nats: no response from stream") {
		t.Fatalf("publish error = %v, want the listener's error body", err)
	}
}

// A role publish no session can take is refused 404 with the listener's reason
// (packages/envoy/cmd/listener/api.go writeRoleHolderError): nobody claimed the role, or its
// holder's registration lapsed. That is the one refusal a caller holds a notice for, so it is told
// apart from every other failure; a 404 with no such reason (a route an older listener lacks) is
// not a missing holder, and stays an ordinary failure.
func TestHTTPPublisherTellsANoHolderRefusalFromAnyOther(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		noHolder bool
	}{
		{"nobody claimed the role", http.StatusNotFound, `{"error":"no holder for role legion-demo-controller","reason":"unclaimed"}`, true},
		{"the holder lapsed", http.StatusNotFound, `{"error":"role legion-demo-controller holder ses_a lapsed; claim retained (last seen 1)","reason":"holder_lapsed","holder":"ses_a","last_seen":1,"claim_released":false}`, true},
		{"a route the listener lacks", http.StatusNotFound, `{"error":"not found"}`, false},
		{"the stream is down", http.StatusInternalServerError, `{"error":"nats: no response from stream"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			err := New(server.URL, "").Publish(context.Background(), "notifications.role.legion-demo-controller", "held on DEMO-1", map[string]string{"kind": "held"}, "legion-outbox:9")
			if err == nil || !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("publish error = %v, want the listener's refusal %s", err, tc.body)
			}
			if got := errors.Is(err, ErrNoHolder); got != tc.noHolder {
				t.Fatalf("errors.Is(%v, ErrNoHolder) = %t, want %t", err, got, tc.noHolder)
			}
		})
	}
}

// A message in an agent's name goes through the listener's direct-message route with the fields
// its sendHandler reads (packages/envoy/cmd/listener/api.go messageBody plus target_session): the
// agent as source_session, the envelope it answers as in_reply_to, and the idempotency key the
// listener scopes its dedupe key by; a refusal names the listener's error.
func TestSendPostsADirectMessageInTheAgentsName(t *testing.T) {
	refuse := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer envoy-bearer" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/send" {
			t.Fatalf("request = %s %s, want POST /v1/messages/send", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"target_session": "ses_asker", "source": "agent", "source_session": "ses_tester", "message": "Plum.",
			"in_reply_to": "evt-1", "idempotency_key": "0123456789abcdef0123456789abcdef",
		}
		for key, value := range want {
			if body[key] != value {
				t.Fatalf("send body %s = %q, want %q (body %v)", key, body[key], value, body)
			}
		}
		if len(body) != len(want) {
			t.Fatalf("send body = %v, want exactly %v", body, want)
		}
		if refuse {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session ses_asker"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	message := Message{Target: "ses_asker", SourceSession: "ses_tester", Text: "Plum.", InReplyTo: "evt-1", IdempotencyKey: "0123456789abcdef0123456789abcdef"}
	if err := New(server.URL, "envoy-bearer").Send(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	refuse = true
	if err := New(server.URL, "envoy-bearer").Send(context.Background(), message); err == nil || !strings.Contains(err.Error(), "no live session ses_asker") || !errors.Is(err, ErrRefused) {
		t.Fatalf("refused send = %v, want the listener's refusal, ErrRefused", err)
	}
}
