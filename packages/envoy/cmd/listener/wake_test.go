package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

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
	publishWakeReport(context.Background(), nil, logging.New("test"), wakeReport{Kind: wakeKindReady})
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
		publishWakeReport(context.Background(), client, logging.New("test"), report)
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

// R3: a wake report is retried until the bus takes it. Losing one is not "one wake late": the
// daemon arms a backstop for a phase worker of an open phase alone, so a released architect, and
// a worker whose phase is closed, have nothing else that brings their pod back, and the daemon's
// stranded report passes over a released claim no wake ever touched. A report dropped while the
// stream is out of reach — a reconnect, a stream not yet reconciled — would leave such a tree
// stalled with nothing saying so.
//
// The bus here is out of reach exactly that way: a client of an account whose stream does not
// exist yet cannot publish to it at all, and the stream arrives while the report is retrying.
func TestAWakeReportIsRetriedUntilTheBusTakesIt(t *testing.T) {
	url := testnats.URL(t)
	client, err := bus.Connect([]string{url})
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	t.Cleanup(client.Close)
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		publishWakeReport(context.Background(), client, logging.New("test"),
			wakeReport{Kind: wakeKindAgent, Session: "session-released"})
	}()

	// The stream arrives only once one whole attempt has run out, as a reconnect or a
	// deployment's first reconcile brings it back: a report published once, however long its own
	// bound, is gone by then, and only a retry still has it to send.
	time.Sleep(wakeReportAttempt + 500*time.Millisecond)
	owner, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	t.Cleanup(owner.Close)

	// What the report is read from is the stream, not the subject: a publish the stream never
	// acknowledged still reaches a core subscriber of that subject, and a wake the stream does
	// not hold reaches no daemon's durable and survives no daemon restart (R3).
	subscription, err := owner.JS().SubscribeSync(bus.LegionWakeTopic, nats.DeliverAll(), nats.OrderedConsumer())
	if err != nil {
		t.Fatalf("read %s from the stream: %v", bus.LegionWakeTopic, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	message, err := subscription.NextMsg(60 * time.Second)
	if err != nil {
		t.Fatalf("the stream never stored the agent_message report: %v", err)
	}
	var item contracts.Envelope
	if err := json.Unmarshal(message.Data, &item); err != nil {
		t.Fatalf("decode the report's envelope: %v", err)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("the retried report's envelope is not valid: %v", err)
	}
	select {
	case <-reported:
	case <-time.After(30 * time.Second):
		t.Fatal("the report's goroutine is still retrying after the stream took it")
	}
}

// Security[untrusted-input]: the listener's own wake subject is refused to every caller of the
// publish route. A Legion daemon acts on what it reads there — it wakes the pod a role token or
// a session names, and holds its whole release gate open on a `listener_ready` report — and a
// caller of this route chooses every field of the envelope and the payload, source "envoy"
// included, so a session that could publish here could wake pods at will and keep that gate open
// with a report dated in the future.
func TestThePublishRouteRefusesTheLegionWakeTopic(t *testing.T) {
	recorder, _ := publishThrough(&listenerDeps{}, `{"topic":"`+bus.LegionWakeTopic+`","message":"listener_ready","source":"envoy"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("publishing %s answered %d; want it refused with 400, body %s", bus.LegionWakeTopic, recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "the listener alone reports a Legion pod wake") {
		t.Fatalf("the refusal = %s, want it to say the listener alone reports a wake", recorder.Body)
	}
}
