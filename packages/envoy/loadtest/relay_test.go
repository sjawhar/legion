package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestJoinDeliveryTimesFromPublish: a receipt that carries the stream's store time gets a delivery
// latency from publish; a core receipt, which carries none, gets none.
func TestJoinDeliveryTimesFromPublish(t *testing.T) {
	writes := []writeRec{{Marker: 1, Kind: kindComment, Issue: "LGA-1", InWindow: true, Sent: 990 * msNs, Arrival: 1000 * msNs, Response: 1100 * msNs, Status: 201}}
	commits := []commitRow{{EventID: 10, Type: "comment.created", Marker: 1, CommitNs: 1050 * msNs}}
	receipts := []receipt{
		{EventID: 10, Recv: 1200 * msNs, Published: 1060 * msNs},
		{EventID: 10, Recv: 1400 * msNs, Published: 1060 * msNs},
		{EventID: 10, Recv: 1300 * msNs},
	}
	got := joinDelivery(writes, commits, receipts, map[string]int{"LGA-1": 3}, 10)
	if got.DeliveryMsFromPublish.N != 2 || got.DeliveryMsFromPublish.P50 != 140 || got.DeliveryMsFromPublish.Max != 340 {
		t.Errorf("delivery from publish %+v, want n 2, p50 140, max 340", got.DeliveryMsFromPublish)
	}
	if got.DeliveryMsFromArrival.N != 3 || got.Missing != 0 {
		t.Errorf("delivery from arrival n %d, missing %d, want 3 and 0", got.DeliveryMsFromArrival.N, got.Missing)
	}
}

// relayFrame is a relay frame of event id on subject, stream sequence seq, stored at storedAt.
func relayFrame(t *testing.T, subject string, seq string, delivered string, storedAt time.Time, id int64) *nats.Msg {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"id": id, "type": "comment.created"})
	data, _ := json.Marshal(contracts.Envelope{EventID: "e", Topic: subject, Payload: string(payload)})
	m := nats.NewMsg("notifications.envoy.deliver.ses-lg-0000")
	m.Data = data
	m.Header.Set(relayHeaderSubject, subject)
	m.Header.Set(relayHeaderStreamSeq, seq)
	m.Header.Set(relayHeaderDelivered, delivered)
	m.Header.Set(relayHeaderStoredAt, storedAt.UTC().Format(time.RFC3339Nano))
	return m
}

// TestRelayHandlerCountsEachSequenceOnce: a session's relay frames are its receipts, each stream
// sequence once, on the agent topic when the frame's subject is the session's agent subject, and
// with the stream's store time; a frame delivered again is counted as a duplicate and redelivery.
func TestRelayHandlerCountsEachSequenceOnce(t *testing.T) {
	ns := &natsSessions{receipts: newCollector[receipt](16), envelopes: envelopeSamples{data: map[string][]byte{}, marked: map[string]bool{}}}
	ns.relay = newRelaySessions(1, 1)
	agent := contracts.AgentSubject("ses-lg-0000")
	h := ns.relay.handler(ns, 0, agent, 1)
	stored := time.Unix(100, 5)
	h(relayFrame(t, "notifications.dispatch.issue.LGA-1.comment", "7", "1", stored, 10))
	h(relayFrame(t, "notifications.dispatch.issue.LGA-1.comment", "7", "2", stored, 10))
	h(relayFrame(t, agent, "8", "1", stored, 10))

	rs := ns.receipts.snapshot()
	if len(rs) != 2 || rs[0].Agent || !rs[1].Agent || rs[0].Published != stored.UnixNano() || rs[0].EventID != 10 {
		t.Fatalf("receipts %+v, want an issue-topic and an agent-topic receipt of event 10 published at %d", rs, stored.UnixNano())
	}
	d := ns.relay.summary()
	if d.Frames != 3 || d.Duplicates != 1 || d.Redelivered != 1 {
		t.Errorf("frames %d duplicates %d redelivered %d, want 3, 1, 1", d.Frames, d.Duplicates, d.Redelivered)
	}
	// The frames carry no reply subject here: each of the two replies per frame is an error.
	if d.ReplyErrors != 6 {
		t.Errorf("reply errors %d, want 6", d.ReplyErrors)
	}
}

// TestRelayAnswersCountTheSessionsOnConsumerDelivery: the summary counts each session by its last
// registration answer -- consumer delivery, an answer naming no consumer, or none at all -- and
// counts the answers that changed a session's mode.
func TestRelayAnswersCountTheSessionsOnConsumerDelivery(t *testing.T) {
	r := newRelaySessions(4, 2)
	if r.epochs[0] == "" || r.epochs[0] == r.epochs[1] {
		t.Fatalf("epochs %q: want one random epoch per connection", r.epochs)
	}
	r.answered(0, sessionDeliveryConsumer, "stream-a")
	r.answered(1, sessionDeliveryConsumer, "stream-a")
	// An answer that named no delivery field at all, which only a listener before INFRA-771 sends.
	r.answered(1, "", "")
	r.answered(2, "core", "")
	d := r.summary()
	if d.ConsumerSessions != 1 || d.NoConsumerSessions != 1 || d.Unanswered != 2 || d.ModeChanges != 1 {
		t.Errorf("summary %+v, want 1 consumer, 1 without a consumer, 2 unanswered, 1 mode change", d)
	}
	if !r.relayed(0) || r.relayed(1) || r.relayed(3) {
		t.Errorf("relayed: session 0 %v, 1 %v, 3 %v; want true, false, false", r.relayed(0), r.relayed(1), r.relayed(3))
	}
}

// TestRelayResumePointFollowsDoneFrames: as the plugin's, a session's registration carries no
// resume point before it answered a frame done, then its stream and highest done sequence; an
// answer naming another stream starts the resume point again.
func TestRelayResumePointFollowsDoneFrames(t *testing.T) {
	ns := &natsSessions{receipts: newCollector[receipt](16), envelopes: envelopeSamples{data: map[string][]byte{}, marked: map[string]bool{}}}
	ns.relay = newRelaySessions(1, 1)
	r := ns.relay
	r.answered(0, sessionDeliveryConsumer, "stream-a")
	if _, ok := r.delivery(0)["resume"]; ok {
		t.Fatalf("resume point before any frame: %v", r.delivery(0))
	}
	h := r.handler(ns, 0, contracts.AgentSubject("ses-lg-0000"), 1)
	for _, seq := range []string{"12", "9"} {
		m := relayFrame(t, "notifications.dispatch.issue.LGA-1.comment", seq, "1", time.Unix(100, 0), 10)
		m.Header.Set(relayHeaderStreamID, "stream-a")
		h(m)
	}
	resume, _ := r.delivery(0)["resume"].(map[string]any)
	if resume["stream_id"] != "stream-a" || resume["seq"] != uint64(12) {
		t.Errorf("resume point %v, want stream-a at 12", resume)
	}
	r.answered(0, sessionDeliveryConsumer, "stream-b")
	if _, ok := r.delivery(0)["resume"]; ok {
		t.Errorf("resume point after the stream changed: %v", r.delivery(0))
	}
}

// TestTablesShowSessionDelivery: the session-delivery table shows the sessions on consumer
// delivery, the latency from publish, and the listener's and NATS's trends; a step with no
// delivery summary shows "-" where consumer delivery has no value.
func TestTablesShowSessionDelivery(t *testing.T) {
	consumers, pulls, sessionPulls := 1003.0, 40.0, 33.3
	consumer := stepData{name: "after", sum: summary{
		Config:   summaryConfig{Step: "after"},
		Events:   eventsSummary{DeliveryMsFromPublish: stat{N: 10, P50: 12, P99: 80}, DeliveryMsFromArrival: stat{N: 10, P50: 900, P99: 4000}},
		Sessions: sessionsSummary{Delivery: &sessionDeliverySummary{ConsumerSessions: 1000, Frames: 50}},
		Resources: &resourcesSummary{JSConsumersMax: &consumers, PullRequestsPerS: &pulls, SessionPullRequestsPerS: &sessionPulls,
			RSSMiBMax: map[string]float64{"listener": 90}, Trends: map[string]trend{"listener": {CoresFirstThird: 0.1, CoresLastThird: 0.12, RSSMiBFirstThird: 80, RSSMiBLastThird: 85}}},
	}}
	unmeasured := stepData{name: "before", sum: summary{Config: summaryConfig{Step: "before"}}}
	md := renderTables([]stepData{consumer, unmeasured})
	text := section(md, "Session delivery, listener and NATS")
	cells := rowCells(text, "after")
	if cells == nil || cells[1] != "1000 / 0" || cells[2] != "12 / 80" || cells[6] != "0.100 / 0.120" ||
		cells[7] != "80 / 85 (90)" || cells[10] != "1,003" || cells[11] != "40.0 / 33.3" {
		t.Errorf("the consumer step's row: %q\n%s", cells, text)
	}
	if cells := rowCells(text, "before"); cells == nil || cells[1] != "-" || cells[10] != "-" {
		t.Errorf("the step with no delivery summary: %q\n%s", cells, text)
	}
	if !strings.Contains(text, "Envoy-Stored-At") {
		t.Errorf("no note says where the latency from publish comes from:\n%s", text)
	}
	checkRowWidths(t, md)
}
