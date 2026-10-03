package intake

import (
	"context"
	"testing"
)

// A Dispatch consumer recreated after messages already landed on the stream — deleted and
// reopened, or a first boot against a stream that already carries history — starts at
// DeliverNewPolicy: it will never receive those older messages, so its ack floor can never reach
// a target the listing read against them. DispatchPosition's idle, not the ack floor alone, is
// what tells Reconcile nothing more is ever coming for a record those messages already advanced
// past.
func TestDispatchPositionIsIdleForARecreatedConsumerBehindPreExistingMessages(t *testing.T) {
	js, _ := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.comment.created", envelopeJSON(t, "dispatch-comment", "dispatch",
		`{"id":2,"issue_key":"CAPTURE-3","seq":9,"notify":true,"type":"comment.created","payload":{"body":"hi"}}`))

	consumers, err := OpenConsumers(context.Background(), js, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	target, err := consumers.DispatchTarget(context.Background())
	if err != nil {
		t.Fatalf("DispatchTarget: %v", err)
	}
	if target < 2 {
		t.Fatalf("target = %d, want at least 2 (the two messages already published)", target)
	}
	position, err := consumers.DispatchPosition(context.Background())
	if err != nil {
		t.Fatalf("DispatchPosition: %v", err)
	}
	if !position.Idle {
		t.Fatalf("idle = false for a freshly (re)created consumer with nothing it can ever deliver from before it existed, want true")
	}
	if position.AckFloorStream >= target {
		t.Fatalf("ack_floor = %d, target = %d; want ack_floor short of target so idle is what closes the gap, not the floor", position.AckFloorStream, target)
	}
}
