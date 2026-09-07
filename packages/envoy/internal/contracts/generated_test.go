package contracts

import (
	"encoding/json"
	"reflect"
	"testing"
)

func validSignalQualityEnvelope() Envelope {
	return Envelope{
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
}

func TestEnvelopeSignalQualityFieldsRoundTrip(t *testing.T) {
	want := validSignalQualityEnvelope()

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

func TestEnvelopeValidateRejectsInvalidSignalQualityFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Envelope)
		want   string
	}{
		{
			name: "unrecognized urgency",
			mutate: func(envelope *Envelope) {
				envelope.Urgency = "urgent"
			},
			want: "urgency must be one of: low, med, high, blocking",
		},
		{
			name: "unrecognized expects reply",
			mutate: func(envelope *Envelope) {
				envelope.ExpectsReply = "later"
			},
			want: "expects_reply must be one of: none, optional, required",
		},
		{
			name: "empty sender session ID",
			mutate: func(envelope *Envelope) {
				envelope.Sender.SessionID = ""
			},
			want: "sender.session_id is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope := validSignalQualityEnvelope()
			test.mutate(&envelope)

			err := envelope.Validate()
			if err == nil || err.Error() != test.want {
				t.Errorf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEnvelopeValidateAcceptsSignalQualityFields(t *testing.T) {
	if err := validSignalQualityEnvelope().Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil", err)
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
