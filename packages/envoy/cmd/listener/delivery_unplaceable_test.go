package main

import "testing"

// TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins the arithmetic the unplaceable retry
// rests on. The window (defaultUnplaceableRetryWindow), its NAK delay (unplaceableRetryDelay),
// the durable's MaxDeliver and MaxAckPending (consumerMaxDeliver, consumerMaxAckPending,
// durable.go) and the attempt cache's TTL (attemptCacheTTL, main.go) are five constants in three
// files, and raising any one of them alone silently breaks a property no delivery test would
// report: the message simply stops arriving, with no failure and no record anywhere.
func TestUnplaceableRetryWindowFitsTheBoundsItDependsOn(t *testing.T) {
	// The window is spent as whole deferrals of unplaceableRetryDelay each, counted from the
	// first delivery, so a message's deliveries are the deferrals the window buys plus the
	// first delivery that asked for the first of them.
	deliveries := int(defaultUnplaceableRetryWindow/unplaceableRetryDelay) + 1
	// The margin is the deliveries this handler does not ask for: one whose handler died
	// before it finalized and whose AckWait therefore expired still counts against max_deliver.
	margin := consumerMaxDeliver - unplaceableMaxDeliveries
	if deliveries+margin > consumerMaxDeliver {
		t.Fatalf("a %s window retried every %s is %d deliveries, %d with the margin, which is past the durable's MaxDeliver of %d: JetStream would stop redelivering silently - nothing here consumes its advisory - before this listener wrote its give-up record, which is the loss this bound exists to prevent",
			defaultUnplaceableRetryWindow, unplaceableRetryDelay, deliveries, deliveries+margin, consumerMaxDeliver)
	}
	if defaultUnplaceableRetryWindow >= attemptCacheTTL {
		t.Fatalf("a %s retry window reaches the attempt cache's %s TTL: a message's own retries must never outlive the attempt entry whose clearing keeps each redelivery from being swallowed as a listener attempt-dedupe skip",
			defaultUnplaceableRetryWindow, attemptCacheTTL)
	}
	// A deferred message is NAKed rather than ACKed, so it holds one of the durable's pending
	// slots for its whole window instead of giving it back at once. The cap on how many this
	// handler defers at a time is what keeps those holds from becoming every slot there is.
	if unplaceableDeferralCap*2 >= consumerMaxAckPending {
		t.Fatalf("a cap of %d messages deferred at once against the durable's MaxAckPending of %d leaves most of the pending budget to deferrals: enough concurrently unplaceable messages would hold every slot at once and stall this machine's durable for every topic, including the deliveries that would land on their first attempt",
			unplaceableDeferralCap, consumerMaxAckPending)
	}
}
