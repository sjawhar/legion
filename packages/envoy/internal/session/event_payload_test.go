package session_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// dispatchEnvelope is one Dispatch event as its outbox publishes it: the event, flat, with the
// payload the test gives it.
func dispatchEnvelope(t *testing.T, eventType string, payload map[string]any) contracts.Envelope {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"id": 41, "seq": 7, "issue_key": "CORE-1", "type": eventType,
		"actor": map[string]string{"kind": "user", "id": "alice"}, "payload": payload,
		"created_at": "2026-09-07T04:41:12Z", "notify": true, "quiet": false,
	})
	if err != nil {
		t.Fatalf("encode the event: %v", err)
	}
	return contracts.Envelope{
		EventID:        "dispatch-41",
		Source:         "dispatch",
		Topic:          "notifications.dispatch.issue.CORE-1." + eventType,
		IssuedAt:       1788756072000,
		PayloadSummary: "alice commented",
		Payload:        string(encoded),
	}
}

// A comment body longer than one event carries rides as its first part, and the payload says so.
// The session holds no Dispatch client, so what it is given is a line naming the route that
// answers the whole comment; the same for a message.
func TestTextNotesABodyTheEventCarriesInPart(t *testing.T) {
	deliverer := session.Deliverer{}
	for _, tc := range []struct {
		eventType string
		route     string
	}{
		{"comment.created", "GET /api/v1/comments/8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"},
		{"suggestion.accepted", "GET /api/v1/comments/8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"},
		{"message.created", "GET /api/v1/messages/8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"},
	} {
		t.Run(tc.eventType, func(t *testing.T) {
			item := dispatchEnvelope(t, tc.eventType, map[string]any{
				"id":             "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b",
				"body":           "The first paragraph.",
				"body_truncated": true,
			})
			got := deliverer.Text(item)
			want := "Note: the body is its first part only; read it whole with " + tc.route
			if !strings.Contains(got, want) {
				t.Fatalf("Text() does not carry %q:\n%s", want, got)
			}
		})
	}

	whole := dispatchEnvelope(t, "comment.created", map[string]any{
		"id": "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b", "body": "All of it.",
	})
	if got := deliverer.Text(whole); strings.Contains(got, "first part") {
		t.Fatalf("Text() notes a cut body on an event that carries its body whole:\n%s", got)
	}
}

// An event the message bus refused whole is published as a stand-in with its payload left out,
// so the session is told that rather than reading the short fields as the whole event.
func TestTextNotesAnOmittedPayload(t *testing.T) {
	item := dispatchEnvelope(t, "comment.created", map[string]any{
		"id": "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b", "payload_omitted": true,
	})
	got := session.Deliverer{}.Text(item)
	want := "Note: this event was too large for the message bus, so it carries its short fields only"
	if !strings.Contains(got, want) {
		t.Fatalf("Text() does not carry %q:\n%s", want, got)
	}
}

// A comment mention is delivered as a frame wrapping the event, so the note is read off the
// wrapped event just as it is off a flat one.
func TestTextNotesACutBodyInsideADeliveryFrame(t *testing.T) {
	event := dispatchEnvelope(t, "comment.created", map[string]any{
		"id": "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b", "body": "Head.", "body_truncated": true,
	})
	var wrapped map[string]any
	if err := json.Unmarshal([]byte(event.Payload), &wrapped); err != nil {
		t.Fatalf("decode the event: %v", err)
	}
	frame, err := json.Marshal(map[string]any{
		"event":    wrapped,
		"delivery": map[string]any{"attempt": 1, "mode": "btw"},
	})
	if err != nil {
		t.Fatalf("encode the delivery frame: %v", err)
	}
	event.Payload = string(frame)
	got := session.Deliverer{}.Text(event)
	want := "read it whole with GET /api/v1/comments/8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	if !strings.Contains(got, want) {
		t.Fatalf("Text() does not carry %q:\n%s", want, got)
	}
}

// Only Dispatch writes these two fields, so no other source's payload is read for them: a
// GitHub payload that happens to hold body_truncated (its webhook normalizer writes one, as a
// string) adds no line.
func TestTextReadsTheFieldsOnDispatchEnvelopesAlone(t *testing.T) {
	item := dispatchEnvelope(t, "comment.created", map[string]any{
		"id": "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b", "body": "Head.", "body_truncated": true,
	})
	item.Source = "github"
	got := session.Deliverer{}.Text(item)
	if strings.Contains(got, "first part") {
		t.Fatalf("Text() read a Dispatch field off a GitHub envelope:\n%s", got)
	}
}
