package bus

import (
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// LEGION-271. A Dispatch publish carries a JetStream MsgId built from the envelope's dedupe key,
// so a repeat of a key the stream already holds is suppressed at publish. Publish throws that
// verdict away, which is why Dispatch cannot tell a retry that changed nothing from one that
// delivered. PublishReportingDuplicate returns it. Publish itself keeps its signature: it is an
// interface method in cistore, outbox and webhook.
func TestPublishReportingDuplicateReportsTheStreamsVerdict(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
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

	// An agent-sourced envelope carries no MsgId at all (contracts.DedupeKeyNamesTheUpstreamEvent),
	// cannot recognise a repeat and never reports one.
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
