package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The wake report's wire shape is what the Legion daemon's decoder reads
// (packages/daemon/internal/intake/decode.go, decodeListenerWake): a kind, the role token or the
// session, and the listener's own clock in epoch milliseconds. A role report names no session
// and an agent report no role, so a daemon never matches one against the wrong claim.
func TestTheWakeReportsWireShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report wakeReport
		want   map[string]any
	}{
		{
			name:   "a role message",
			report: wakeReport{Kind: wakeKindRole, Role: "legion-legion-legion-208-implementer", At: 1762070400000},
			want:   map[string]any{"kind": "role_message", "role": "legion-legion-legion-208-implementer", "at": float64(1762070400000)},
		},
		{
			name:   "an agent message",
			report: wakeReport{Kind: wakeKindAgent, Session: "session-1", At: 1762070400000},
			want:   map[string]any{"kind": "agent_message", "session": "session-1", "at": float64(1762070400000)},
		},
		{
			name:   "the liveness report",
			report: wakeReport{Kind: wakeKindReady, At: 1762070400000},
			want:   map[string]any{"kind": "listener_ready", "at": float64(1762070400000)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.report)
			if err != nil {
				t.Fatalf("encode the report: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("decode the report: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("report = %v, want exactly %v", got, tc.want)
			}
			for key, value := range tc.want {
				if got[key] != value {
					t.Fatalf("report[%s] = %v, want %v", key, got[key], value)
				}
			}
		})
	}
}

// A report with no bus to publish on is dropped, never a panic: a listener built without a
// client in a test answers its 404s as it always did.
func TestAWakeReportWithNoBusIsDropped(t *testing.T) {
	publishWakeReport(nil, logging.New("test"), wakeReport{Kind: wakeKindReady})
	reportAgentWake(nil, logging.New("test"), "")
	if !bus.StreamCaptures(bus.LegionWakeTopic) {
		t.Fatalf("the stream does not capture the wake topic")
	}
}

// Every wake report the listener publishes is an envelope the stream takes and a Legion daemon
// reads. The report's payload was proven above and the feature was still dead: an envelope
// missing one required field is dropped by publishWakeReport's own Validate, and a dropped
// listener_ready holds every daemon's release gate shut for ever (the daemon's
// workflowRuntime.listenerLive). So this reads what actually crossed the bus, for each of the
// three kinds, and checks every field contracts.Envelope.Validate requires — source_event_id,
// the one that was missing, among them.
func TestEveryWakeReportCrossesTheBusAsAValidEnvelope(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	subscription, err := conn.SubscribeSync(bus.LegionWakeTopic)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", bus.LegionWakeTopic, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	for _, report := range []wakeReport{
		{Kind: wakeKindRole, Role: "legion-legion-legion-208-implementer"},
		{Kind: wakeKindAgent, Session: "session-1"},
		{Kind: wakeKindReady},
	} {
		publishWakeReport(client, logging.New("test"), report)
		message, err := subscription.NextMsg(10 * time.Second)
		if err != nil {
			t.Fatalf("the %s report never reached %s: %v", report.Kind, bus.LegionWakeTopic, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode the %s report's envelope: %v", report.Kind, err)
		}
		if err := item.Validate(); err != nil {
			t.Fatalf("the %s report's envelope is not valid: %v", report.Kind, err)
		}
		for _, required := range []struct{ field, value string }{
			{"event_id", item.EventID},
			{"source", item.Source},
			{"source_event_id", item.SourceEventID},
			{"topic", item.Topic},
			{"dedupe_key", item.DedupeKey},
			{"payload_summary", item.PayloadSummary},
			{"trace_id", item.TraceID},
			{"payload", item.Payload},
		} {
			if strings.TrimSpace(required.value) == "" {
				t.Errorf("the %s report's envelope carries no %s", report.Kind, required.field)
			}
		}
		if item.IssuedAt == 0 {
			t.Errorf("the %s report's envelope carries no issued_at", report.Kind)
		}
		if item.Source != "envoy" || item.Topic != bus.LegionWakeTopic {
			t.Errorf("the %s report crossed as source %q on %q, want envoy on %s",
				report.Kind, item.Source, item.Topic, bus.LegionWakeTopic)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
			t.Fatalf("decode the %s report's payload: %v", report.Kind, err)
		}
		if payload["kind"] != report.Kind {
			t.Errorf("the %s report's payload says kind %v", report.Kind, payload["kind"])
		}
	}
}
