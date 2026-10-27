package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
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
