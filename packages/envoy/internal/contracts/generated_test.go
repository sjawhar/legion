package contracts

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEnvelopeSignalQualityFieldsRoundTrip(t *testing.T) {
	want := Envelope{
		EventID:        "evt-1",
		Source:         "agent",
		SourceEventID:  "source-1",
		SourceSession:  "session-1",
		Topic:          "notifications.agent.session-1",
		DedupeKey:      "agent.session-1.source-1",
		IssuedAt:       1_725_686_400_000,
		PayloadSummary: "example update",
		TraceID:        "trace-1",
		Sender: &EnvelopeSender{
			SessionID: "session-1",
			Machine:   "example-host",
			Cwd:       "/workspace",
			Title:     "Controller",
			Roles:     []string{"legion-controller", "operations"},
		},
		InReplyTo:    "evt-0",
		Supersedes:   "evt-old",
		Urgency:      "blocking",
		ExpectsReply: "required",
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	t.Logf("round-trip JSON: %s", encoded)

	var got Envelope
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("signal-quality fields did not round trip: got %#v, want %#v", got, want)
	}
}

func TestEnvelopeSignalQualityFieldsOmitEmpty(t *testing.T) {
	encoded, err := json.Marshal(Envelope{})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode envelope JSON: %v", err)
	}
	for _, key := range []string{"sender", "in_reply_to", "supersedes", "urgency", "expects_reply"} {
		if _, found := fields[key]; found {
			t.Errorf("empty envelope contains %q", key)
		}
	}
}
