package bus

import (
	"strconv"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// LEGION-271. A Dispatch publish carries a JetStream MsgId built from the envelope's dedupe key,
// so the stream recognises a repeat of a key it already holds: it stores nothing new, and the
// publish still reaches the agent's subject. Publish throws that verdict away, which is why
// Dispatch cannot tell a retry the stream already held from one it stored.
// PublishReportingDuplicate returns it. Publish itself keeps its signature: it is an interface
// method in cistore, outbox and webhook.
func TestPublishReportingDuplicateReportsTheStreamsVerdict(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	envelope := func(eventID string) contracts.Envelope {
		return contracts.Envelope{
			EventID:        eventID,
			Source:         "dispatch",
			Topic:          "notifications.agent.ses_dup",
			DedupeKey:      "agent.ses_dup.message-1:steer",
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "a retried targeted message",
			TraceID:        "trace-" + eventID,
		}
	}

	duplicate, err := client.PublishReportingDuplicate(envelope("first"))
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if duplicate {
		t.Fatalf("first publish reported a duplicate; the stream held nothing")
	}

	duplicate, err = client.PublishReportingDuplicate(envelope("second"))
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if !duplicate {
		t.Fatalf("a repeat of a dedupe key the stream already holds must report a duplicate")
	}

	// The control that keeps the first assertion honest: a different key is not a duplicate,
	// so "duplicate" is not simply what the second publish always answers.
	other := envelope("third")
	other.DedupeKey = "agent.ses_dup.message-1:btw"
	duplicate, err = client.PublishReportingDuplicate(other)
	if err != nil {
		t.Fatalf("third publish: %v", err)
	}
	if duplicate {
		t.Fatalf("a different dedupe key must not report a duplicate")
	}

	// An agent-sourced envelope under a key its caller chose carries no MsgId at all, because
	// that key names no event (contracts.DedupeKeyNamesTheUpstreamEvent), so the stream cannot
	// recognise a repeat and never reports one.
	agent := envelope("fourth")
	agent.Source = "agent"
	agent.Topic = "notifications.agent.ses_dup"
	for range 2 {
		duplicate, err = client.PublishReportingDuplicate(agent)
		if err != nil {
			t.Fatalf("agent-sourced publish: %v", err)
		}
		if duplicate {
			t.Fatalf("an agent-sourced publish carries no MsgId and cannot be a duplicate")
		}
	}
}

// PublishRetained stores a payload that is not an envelope on a subject the stream captures, keeps
// one copy of every publish under one MsgId, and refuses a subject the stream does not capture
// rather than waiting out an acknowledgement JetStream never sends.
func TestPublishRetainedKeepsOneCopyPerMsgID(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	const subject = "notifications.legion.infra.agents.ses_handoff"

	for i, want := range []bool{false, true} {
		duplicate, err := client.PublishRetained(subject, []byte(`{"copy":`+strconv.Itoa(i)+`}`), "handoff.key-1")
		if err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
		if duplicate != want {
			t.Fatalf("publish %d duplicate = %t, want %t", i, duplicate, want)
		}
	}
	if duplicate, err := client.PublishRetained(subject, []byte(`{"copy":2}`), "handoff.key-2"); err != nil || duplicate {
		t.Fatalf("another MsgId = %t, %v; want stored", duplicate, err)
	}
	info, err := client.JS().StreamInfo(Stream, &nats.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if got := info.State.Subjects[subject]; got != 2 {
		t.Fatalf("the stream holds %d messages on %s, want 2", got, subject)
	}
	stored, err := client.JS().GetMsg(Stream, info.State.FirstSeq)
	if err != nil {
		t.Fatalf("read the first stored copy: %v", err)
	}
	if string(stored.Data) != `{"copy":0}` {
		t.Fatalf("stored %s, want the first copy", stored.Data)
	}

	if _, err := client.PublishRetained("notifications.team.deploy-squad", []byte(`{}`), ""); err == nil || !strings.Contains(err.Error(), "does not capture") {
		t.Fatalf("a subject the stream does not capture = %v, want refused", err)
	}
}
