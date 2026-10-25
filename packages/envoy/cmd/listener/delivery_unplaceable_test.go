package main

import "testing"

// TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins the arithmetic the unplaceable retry
// rests on. The window (defaultUnplaceableRetryWindow), its NAK delay (unplaceableRetryDelay),
// the durable's MaxDeliver (consumerMaxDeliver, durable.go) and the attempt cache's TTL
// (attemptCacheTTL, main.go) are four constants in three files, and raising any one of them
// alone silently breaks a property no delivery test would report: the message simply stops
// arriving, with no failure and no record anywhere.
func TestUnplaceableRetryWindowFitsTheBoundsItDependsOn(t *testing.T) {
	deliveries := int(defaultUnplaceableRetryWindow / unplaceableRetryDelay)
	// The deliveries this handler does not ask for: the first one, and one whose handler died
	// before it finalized and whose AckWait therefore expired. Both count against max_deliver.
	margin := consumerMaxDeliver - unplaceableMaxDeliveries
	if deliveries+margin > consumerMaxDeliver {
		t.Fatalf("a %s window retried every %s is %d deliveries, %d with the margin, which is past the durable's MaxDeliver of %d: JetStream would stop redelivering silently - nothing here consumes its advisory - before this listener wrote its give-up record, which is the loss this bound exists to prevent",
			defaultUnplaceableRetryWindow, unplaceableRetryDelay, deliveries, deliveries+margin, consumerMaxDeliver)
	}
	if defaultUnplaceableRetryWindow >= attemptCacheTTL {
		t.Fatalf("a %s retry window reaches the attempt cache's %s TTL: a message's own retries must never outlive the attempt entry whose clearing keeps each redelivery from being swallowed as a listener attempt-dedupe skip",
			defaultUnplaceableRetryWindow, attemptCacheTTL)
	}
}
