package main

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
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
