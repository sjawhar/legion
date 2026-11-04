// This file holds main's TestListenerDeliveryHandler_DedupesSuccessfulDelivery, which main has at
// 2a89b063 (packages/envoy/cmd/listener/main_test.go:3230-3251) and the merge of main over #2674
// (77957c8e) dropped from this branch's main_test.go. At fde2b129 it fails: the second copy is
// counted skipped again (delivered{delivery_status="skipped"} = 2, dedupe = 0), because this branch's
// fan-out writes no ledger record for a session that takes the message on its own subscription
// (TestASessionThisListenerDoesNotPromptCostsNoLedgerWrite). INFRA-557 tester's round 29.

package main

import (
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A copy of a message a session has already taken is not resolved a second time: the dedupe key
// the first delivery recorded is what the next copy is judged against. The core handler is what
// drives it, as it drives every delivery to a session that takes its messages off its own
// subscription: the durable acknowledges such a stored message before it decodes it
// (durableNeedsDelivery, takesStoredMessagesItself).
func TestListenerDeliveryHandler_DedupesSuccessfulDelivery(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	item := listenerTestEnvelope(contracts.AgentSubject("ses_success"), "dedupe-success")
	harness.registerTarget(t, "ses_success", item.Topic)
	data := marshalListenerEnvelope(t, item)

	harness.coreHandler(&natsgo.Msg{Data: data})
	harness.coreHandler(&natsgo.Msg{Data: data})

	body := metricsBody(t, harness)
	if got := counterValue(t, body, deliveredSeries("skipped")); got != 1 {
		t.Fatalf("%s = %d, want 1 - the first copy reaches the session on its own subscription:\n%s", deliveredSeries("skipped"), got, body)
	}
	if got := counterValue(t, body, deliveredSeries("dedupe")); got != 1 {
		t.Fatalf("%s = %d, want 1 - the second copy is judged against the key the first recorded:\n%s", deliveredSeries("dedupe"), got, body)
	}
}
