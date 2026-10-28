package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// refusedPublish is an envelope of the shape the publish route builds for a role-lane publish,
// carrying payload as its machine payload.
func refusedPublish(payload string) contracts.Envelope {
	return contracts.Envelope{
		EventID:        "evt-refused",
		Source:         "agent",
		SourceEventID:  "src-refused",
		Topic:          contracts.RoleTopicPrefix + "unheld",
		DedupeKey:      "publish.refused",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "please review",
		Payload:        payload,
		TraceID:        "trace-refused",
	}
}

// exceptionPayloadOf decodes the exception's own payload, the shape the Legion daemon's consumer
// reads (packages/daemon/internal/daemon/notice_exceptions.go, roleLaneException).
func exceptionPayloadOf(t *testing.T, exception contracts.Envelope) deliveryExceptionPayload {
	t.Helper()
	var payload deliveryExceptionPayload
	if err := json.Unmarshal([]byte(exception.Payload), &payload); err != nil {
		t.Fatalf("decode the exception payload: %v", err)
	}
	return payload
}

// The exception carries the refused publish's machine payload, since that is what a consumer
// re-reads the message from, but only while it fits refusalExceptionBound. A larger payload is
// left out instead: carrying it would make an envelope past the server's max payload, which NATS
// refuses whole on every attempt, so a refusal nothing was wrong with would log
// exceptionPublishFailedLine, which the deployed publish-failure alarm matches. Everything a
// consumer of a refusal needs beside the payload -- the topic, the reason, the dedupe key and the
// one-line summary -- is carried whatever the payload's size. The delivery path passes no bound
// and carries the payload whole, as it always has.
func TestARefusalExceptionCarriesThePayloadOnlyWhileItFitsItsBound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		bound   int
		carried string
	}{
		{"a payload inside the bound", `{"kind":"pr-blocked"}`, refusalExceptionBound, `{"kind":"pr-blocked"}`},
		{"a payload exactly at the bound", strings.Repeat("p", refusalExceptionBound), refusalExceptionBound, strings.Repeat("p", refusalExceptionBound)},
		{"a payload one byte past the bound", strings.Repeat("p", refusalExceptionBound+1), refusalExceptionBound, ""},
		{"no bound carries it whole", strings.Repeat("p", refusalExceptionBound+1), 0, strings.Repeat("p", refusalExceptionBound+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := refusedPublish(tc.payload)
			exception, err := deliveryExceptionEnvelope(item, string(roleHolderUnclaimed), "", tc.bound)
			if err != nil {
				t.Fatalf("build the exception: %v", err)
			}
			payload := exceptionPayloadOf(t, exception)
			if payload.Payload != tc.carried {
				t.Fatalf("the exception carries %d bytes of payload, want %d", len(payload.Payload), len(tc.carried))
			}
			if payload.OriginalTopic != item.Topic || payload.EventID != item.EventID ||
				payload.Reason != string(roleHolderUnclaimed) || payload.DedupeKey != item.DedupeKey ||
				payload.PayloadSummary != item.PayloadSummary || payload.Source != item.Source {
				t.Fatalf("the exception payload = %+v, want the refused publish's own fields whatever the payload's size", payload)
			}
			if exception.Topic != bus.ExceptionTopicPrefix+item.Topic || exception.Source != "envoy" || exception.SourceEventID != item.EventID {
				t.Fatalf("the exception envelope = %+v, want it keyed to the refused topic and sourced envoy", exception)
			}
		})
	}
}

// A refusal's exception is best effort: the 404 is already written when it is published, so a
// publish that fails is logged and nothing else -- no retry, and nothing of the answer changes. A
// closed client is the failure every other cause reaches this branch the same way.
func TestARefusalExceptionThatCannotBePublishedIsOnlyLogged(t *testing.T) {
	client := setupPublishTestClient(t)
	logs := &cmdtest.LockedBuffer{}
	logger := logging.NewWithWriter("test", logs)
	client.Close()

	publishRefusalException(client, logger, refusedPublish(""), roleHolderResult{state: roleHolderUnclaimed})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), exceptionPublishFailedLine) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("a refusal exception that could not be published logged %q, want %q", logs.String(), exceptionPublishFailedLine)
}

// The exception goes out behind the 404, never in front of it: publishRefusalException hands the
// publish to a goroutine of its own and returns, so the handler returns and net/http sends the
// answer it was holding. Here the bus is reconnecting, the state that made an in-line publish
// delay the 404 past the route's budget and the Envoy client's per-attempt deadline: an in-line
// publish waits there for a connection until the bus's 5 s publish bound runs out, and this call
// must return long before that.
func TestARefusalExceptionDoesNotWaitForTheBus(t *testing.T) {
	server, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus: %v", err)
	}
	t.Cleanup(client.Close)
	server.Stop(t)
	deadline := time.Now().Add(10 * time.Second)
	for client.Connected() || !client.Conn.IsReconnecting() {
		if time.Now().After(deadline) {
			t.Fatal("the client is not reconnecting 10 s after its server stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client.Connected() || !client.Conn.IsReconnecting() {
		t.Fatal("the client must be reconnecting, not connected or closed")
	}

	started := time.Now()
	publishRefusalException(client, logging.NewWithWriter("test", &cmdtest.LockedBuffer{}), refusedPublish(""), roleHolderResult{state: roleHolderUnclaimed})
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("publishRefusalException took %s against a reconnecting bus, want it back within 500ms: the 404 waits for it",
			elapsed)
	}
}

// A non-live state outside roleholder is not a refusal reason the Legion daemon knows, so it raises
// no exception. This keeps a third listener state from becoming a malformed role-lane exception.
func TestARefusalExceptionIsRaisedOnlyForASharedNoHolderReason(t *testing.T) {
	client := setupPublishTestClient(t)
	item := refusedPublish("{}")
	item.Topic = contracts.RoleTopicPrefix + "unknown-refusal"
	probe, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + item.Topic)
	if err != nil {
		t.Fatalf("subscribe to the exception lane: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}

	publishRefusalException(client, logging.NewWithWriter("test", io.Discard), item, roleHolderResult{state: roleHolderState("not_live")})

	if message, err := probe.NextMsg(250 * time.Millisecond); err == nil {
		t.Fatalf("an unlisted refusal state raised an exception: %s", message.Data)
	} else if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("wait for an exception: %v", err)
	}
}

// The bound applies where the refusal publishes, not only in the helper: a refused publish past
// refusalExceptionBound reaches the exception lane of a live bus without its payload, and with the
// fields a consumer of the refusal needs.
func TestARefusalExceptionLeavesAnOversizedPayloadOutOnTheBus(t *testing.T) {
	client := setupPublishTestClient(t)
	item := refusedPublish(strings.Repeat("p", refusalExceptionBound+1))
	item.Topic = contracts.RoleTopicPrefix + "oversized-refusal"
	probe, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + item.Topic)
	if err != nil {
		t.Fatalf("subscribe to the exception lane: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception subscription: %v", err)
	}

	publishRefusalException(client, logging.NewWithWriter("test", io.Discard), item, roleHolderResult{state: roleHolderUnclaimed})

	message, err := probe.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("read the refusal exception: %v", err)
	}
	var exception contracts.Envelope
	if err := json.Unmarshal(message.Data, &exception); err != nil {
		t.Fatalf("decode the refusal exception: %v", err)
	}
	payload := exceptionPayloadOf(t, exception)
	if payload.Payload != "" {
		t.Fatalf("the exception carries %d bytes of a %d-byte payload, want none past the %d-byte bound",
			len(payload.Payload), len(item.Payload), refusalExceptionBound)
	}
	if payload.OriginalTopic != item.Topic || payload.Reason != string(roleHolderUnclaimed) ||
		payload.DedupeKey != item.DedupeKey || payload.PayloadSummary != item.PayloadSummary {
		t.Fatalf("the exception payload = %+v, want the refused publish's own fields", payload)
	}
}
