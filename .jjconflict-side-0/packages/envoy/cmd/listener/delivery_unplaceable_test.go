package main

import (
	"testing"
	"time"
)

// TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins the arithmetic the unplaceable retry
// rests on. The window (defaultUnplaceableRetryWindow), its NAK delay (unplaceableRetryDelay),
// the deferral slots' expiry (unplaceableDeferralExpiry), the durable's MaxDeliver,
// MaxAckPending and AckWait (consumerMaxDeliver, consumerMaxAckPending, consumerAckWait,
// durable.go) and the attempt cache's TTL (attemptCacheTTL, main.go) are seven constants in
// three files, and changing any one of them alone silently breaks a property no delivery test
// would report: a message simply stops arriving, or the durable's pending budget stops being
// bounded, with no failure and no record anywhere.
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
	// listener defers at a time is what keeps those holds from becoming every slot there is.
	// Twice the cap is also what one restart can leave pending at once: the new process starts with
	// an empty deferral set while the old one's deferrals wait out their NAK delay
	// (unplaceableDeferrals), so this bound is what keeps that moment under MaxAckPending too.
	if unplaceableDeferralCap*2 >= consumerMaxAckPending {
		t.Fatalf("a cap of %d messages deferred at once against the durable's MaxAckPending of %d leaves most of the pending budget to deferrals: enough concurrently unplaceable messages would hold every slot at once and stall this machine's durable for every topic, including the deliveries that would land on their first attempt",
			unplaceableDeferralCap, consumerMaxAckPending)
	}
	// A deferred message stays pending for its NAK delay, and for the durable's AckWait when the
	// NAK is lost on the wire, before a delivery of it can reach this listener and give its slot
	// back. An expiry no longer than that would reclaim the slot of a message the durable is
	// still legitimately holding, and the set would count fewer deferrals than this listener has.
	if legitimatelyPending := unplaceableRetryDelay + consumerAckWait; unplaceableDeferralExpiry <= legitimatelyPending {
		t.Fatalf("a deferral slot expires after %s, no later than the %s a deferred message can legitimately stay pending (its %s NAK delay plus the durable's %s AckWait): a slot would be reclaimed while its message is still held, and the cap would stop bounding the durable's pending deferrals",
			unplaceableDeferralExpiry, legitimatelyPending, unplaceableRetryDelay, consumerAckWait)
	}
}

// TestAnOrphanedDeferralSlotIsReclaimedAtItsExpiry pins the expiry that keeps the deferral set
// from leaking. A slot leaves the set when its own message is delivered here again
// (agentSubjectDelivery's release), and a message the stream drops or purges, or whose
// redelivery reaches another subscriber, never is: without an expiry each such slot is held for
// the life of the process, and unplaceableDeferralCap of them turn the retry off until a
// restart. The set is filled with such orphans on a stepped clock, read one nanosecond before
// their expiry - still held, so a slot whose message may yet come back is never reclaimed early -
// and read again at the expiry, where a new message takes a slot and the orphans are gone. A
// sequence whose own entry expired is taken afresh, with an expiry counted from the new take.
func TestAnOrphanedDeferralSlotIsReclaimedAtItsExpiry(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	clock := start
	set := newUnplaceableDeferrals()
	set.now = func() time.Time { return clock }
	for seq := uint64(1); seq <= unplaceableDeferralCap; seq++ {
		if !set.take(seq) {
			t.Fatalf("taking orphan slot %d of %d was refused", seq, unplaceableDeferralCap)
		}
	}
	fresh := uint64(unplaceableDeferralCap + 1)
	if set.take(fresh) {
		t.Fatalf("a new message took a slot from a set holding the cap of %d - the cap no longer bounds the deferrals", unplaceableDeferralCap)
	}

	clock = start.Add(unplaceableDeferralExpiry - time.Nanosecond)
	if set.take(fresh) {
		t.Fatalf("a new message took a slot %s after the orphans were taken, one nanosecond before their %s expiry - a slot was reclaimed while its message could still legitimately come back",
			clock.Sub(start), unplaceableDeferralExpiry)
	}
	if got := set.size(); got != unplaceableDeferralCap {
		t.Fatalf("the set holds %d slots one nanosecond before the orphans' expiry, want all %d of them", got, unplaceableDeferralCap)
	}

	clock = start.Add(unplaceableDeferralExpiry)
	if !set.take(fresh) {
		t.Fatalf("a new message was refused a slot at the orphans' %s expiry - %d slots whose messages never came back still hold the set full, and the retry stays off until a restart",
			unplaceableDeferralExpiry, unplaceableDeferralCap)
	}
	if got := set.size(); got != 1 {
		t.Fatalf("the set holds %d slots after the orphans expired and one new message was taken, want only that message's", got)
	}

	// An orphan's own sequence, delivered again at last after its entry expired, is not held:
	// it takes a new slot whose expiry runs from this take, not from the one that expired.
	retaken := start.Add(unplaceableDeferralExpiry)
	if !set.take(1) {
		t.Fatal("an expired orphan's own sequence was refused a slot in a set holding one entry")
	}
	clock = retaken.Add(unplaceableDeferralExpiry - time.Nanosecond)
	if got := set.size(); got != 2 {
		t.Fatalf("the set holds %d slots one nanosecond before the re-taken sequence's fresh expiry, want 2 - the new message's and the re-taken one's", got)
	}
	clock = retaken.Add(unplaceableDeferralExpiry)
	if got := set.size(); got != 0 {
		t.Fatalf("the set holds %d slots at the fresh expiry of both, want none", got)
	}
}
