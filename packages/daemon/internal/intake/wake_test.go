package intake

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Envoy listener's wake report arrives on one subject of its own, which the daemon's fourth
// durable consumer filters on whole. Its payload names the kind, the role token or the session,
// and the listener's own clock in epoch milliseconds — the shape Envelope.KeptAt has, because
// the stream's store time is the NATS server's clock and the daemon measures a wake against its
// own.
//
// A report of a kind this build has no wake for, one that names neither a role nor a session,
// and one with no time are no fact rather than poison: a listener of a later release can report
// what this daemon cannot act on, and acknowledging it loses nothing. An envelope from another
// source on that subject, and a payload that is not an object, are poison.
func TestDecodeTheListenerWakeReport(t *testing.T) {
	const role = "legion-legion-legion-208-implementer"
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	millis := at.UnixMilli()
	for _, tc := range []struct {
		name    string
		source  string
		payload string
		want    Fact
		wantErr string
	}{
		{
			name:    "a role message",
			source:  "envoy",
			payload: `{"kind":"role_message","role":"` + role + `","at":` + strconv.FormatInt(millis, 10) + `}`,
			want:    ListenerWake{Kind: ListenerWakeRole, Role: role, At: at, Report: "wake-1"},
		},
		{
			name:    "an agent message",
			source:  "envoy",
			payload: `{"kind":"agent_message","session":"session-1","at":` + strconv.FormatInt(millis, 10) + `}`,
			want:    ListenerWake{Kind: ListenerWakeAgent, Session: "session-1", At: at, Report: "wake-1"},
		},
		{
			name:    "the listener's liveness report",
			source:  "envoy",
			payload: `{"kind":"listener_ready","at":` + strconv.FormatInt(millis, 10) + `}`,
			want:    ListenerWake{Kind: ListenerWakeReady, At: at, Report: "wake-1"},
		},
		{name: "a role message naming no role", source: "envoy", payload: `{"kind":"role_message","at":` + strconv.FormatInt(millis, 10) + `}`},
		{name: "an agent message naming no session", source: "envoy", payload: `{"kind":"agent_message","at":` + strconv.FormatInt(millis, 10) + `}`},
		{name: "a kind this build has no wake for", source: "envoy", payload: `{"kind":"something_later","at":` + strconv.FormatInt(millis, 10) + `}`},
		{name: "no time", source: "envoy", payload: `{"kind":"listener_ready"}`},
		{
			name:    "another source",
			source:  "dispatch",
			payload: `{"kind":"listener_ready","at":` + strconv.FormatInt(millis, 10) + `}`,
			wantErr: `the listener wake subject has envelope source "dispatch"`,
		},
		{
			name:    "a payload that is not the report",
			source:  "envoy",
			payload: `["listener_ready"]`,
			wantErr: "decode the listener's wake report",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := decodeMessage(legionWakeSubject, "CAPTURE", capturedRepositories, nil, envelopeJSON(t, "wake-1", tc.source, tc.payload))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("decode = %v, want poison naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode the wake report: %v", err)
			}
			if !reflect.DeepEqual(decoded.Fact, tc.want) {
				t.Fatalf("decoded fact = %#v, want %#v", decoded.Fact, tc.want)
			}
			if tc.want != nil && (decoded.Source != "envoy" || decoded.EventID != "wake-1") {
				t.Fatalf("decoded = %#v, want it from envoy under its event id", decoded)
			}
		})
	}
}

// The wake subject is a new leaf under notifications.legion.>, which the stream already carries,
// and the fourth durable's name is the project's own, beside the other three.
func TestTheWakeConsumerNamesItsProjectOnItsOwnSubject(t *testing.T) {
	if legionWakeSubject != "notifications.legion.wake" {
		t.Fatalf("the wake subject is %q, want notifications.legion.wake", legionWakeSubject)
	}
	if got := wakeConsumerName("CAPTURE"); got != "legion-go-CAPTURE-wake" {
		t.Fatalf("the wake durable is %q, want legion-go-CAPTURE-wake", got)
	}
	for _, other := range []string{dispatchConsumerName("CAPTURE"), dispatchServerConsumerName("CAPTURE"), githubConsumerName("CAPTURE")} {
		if other == wakeConsumerName("CAPTURE") {
			t.Fatalf("the wake durable shares the name %q with another consumer", other)
		}
	}
}

// Security[untrusted-input]: the report's clock is the payload's, and the payload is whatever
// reached the subject. The listener refuses a client publish on its own wake subject
// (packages/envoy/cmd/listener/api.go), but a publisher that reaches the stream another way
// chooses every field of the envelope and the payload, the envelope's source among them, so the
// daemon bounds the time it reads to its own: a `listener_ready` dated in the year 9999 would
// otherwise hold R7's release gate open for ever, since the stamp keeps the greatest time it is
// given (store.RecordListenerReady), and released pods would then miss every role and agent
// message while the listener itself said nothing.
func TestAListenerReportDatedInTheFutureIsReadAsNow(t *testing.T) {
	const year9999 = 253370764800000
	before := time.Now().UTC()
	decoded, err := decodeMessage(legionWakeSubject, "CAPTURE", capturedRepositories, nil,
		envelopeJSON(t, "wake-forged", "envoy", `{"kind":"listener_ready","at":`+strconv.FormatInt(year9999, 10)+`}`))
	if err != nil {
		t.Fatalf("decode the forged report: %v", err)
	}
	wake, ok := decoded.Fact.(ListenerWake)
	if !ok {
		t.Fatalf("decoded fact = %#v, want a listener wake", decoded.Fact)
	}
	after := time.Now().UTC()
	if wake.At.Before(before) || wake.At.After(after) {
		t.Fatalf("the report's time = %s, want it bounded to the daemon's own clock between %s and %s",
			wake.At, before, after)
	}
}
