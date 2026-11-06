package main

// The consumer a delete took comes back: to a session whose registration raced that delete, even
// where the delete was made on another listener, and at the position the session had acked
// through rather than at the start its record names.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
)

// A registration answered 200 with consumer delivery holds a consumer however a delete on
// another listener interleaves with it. The listener's session lock is one process's, and every
// listener's reaper judges every record of the bucket, so the delete here is made through a
// second sessionDelivery over the same stream, which that lock does not reach: it deletes the
// record the registration is replacing, deletes the consumer the registration has just created,
// and reads the bucket again to find no record to restore from, since the registration has not
// written its entry yet. The registration then writes that entry and must come away with a
// consumer, which is the one thing a session is delivered through.
//
// The interleaving is built rather than waited for: afterRegisterFilter runs under the
// registering listener's session lock, after that registration's consumer call and before the row
// and the entry it writes, and the whole of the other listener's pass runs from there, so its
// record read really precedes the entry write. Before this listener read the consumer again after
// that write, the session was answered 200 with no consumer at all.
func TestARegistrationRacedByAnotherListenersDeleteKeepsItsConsumer(t *testing.T) {
	registering := newSessionConsumerReapRig(t)
	deleting := newSessionConsumerReapRig(t)
	const sessionID = "ses_two_listeners"
	registering.register(t, sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The second listener's copy of the record, read from the bucket rather than waited for from
	// its watch: its pass judges what its copy holds, and deletes at the revision it judged.
	if _, err := deleting.sessions.Refresh(ctx, sessionID); err != nil {
		t.Fatalf("the second listener could not read the record of %s: %v", sessionID, err)
	}
	// Only the second listener's clock is past the gone horizon, so its pass judges the record
	// the registration below replaces while the first listener still reads the session as live.
	deleting.now.advance(3 * time.Minute)
	registering.requireConsumer(t, sessionID, "after its first registration")

	reaped := make(chan int, 1)
	var raced sync.Once
	registering.delivery.afterRegisterFilter = func() {
		raced.Do(func() {
			reaped <- deleting.reap(t)
			// The window this registration is in: its consumer is created, its entry is not
			// written, and the other listener has taken the consumer away and restored nothing.
			if info, err := registering.consumers.Info(ctx, sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
				t.Errorf("the consumer of %s once the other listener's pass had deleted it: %+v, %v; want it gone, which is the window this registration answers in",
					sessionID, info, err)
			}
		})
	}
	answer := registering.register(t, sessionID)

	if answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer {
		t.Fatalf("the registration that raced the other listener's delete was answered delivery %+v, want %s",
			answer.Delivery, deliveryModeConsumer)
	}
	if records := <-reaped; records != 1 {
		t.Fatalf("the other listener's pass deleted %d records, want the one it judged gone", records)
	}
	registering.requireConsumer(t, sessionID, "after the delete another listener made while it registered")
	if _, state := registering.sessions.Lookup(sessionID); state != session.Live {
		t.Fatalf("%s is %s after the registration that raced the other listener's delete, want %s", sessionID, state, session.Live)
	}
}

// A session that races a delete is offered nothing it has acknowledged. The consumer deleted here
// has every frame it delivered acked, and its record names the start it was created at, far below
// that floor; the restore reads the floor under the session's lock and before the delete, and
// creates the consumer again at the sequence after it. Restoring at the record's start instead
// hands the session every frame it has written back again, up to the stream's 72 hours, and holds
// what is published next behind that backlog.
func TestARestoredConsumerDoesNotOfferTheAckedFramesAgain(t *testing.T) {
	rig := newRelayRig(t)
	sessionID := "ses_acked_" + id.New()
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	var written []contracts.Envelope
	var floor uint64
	for range 5 {
		item, seq := rig.storeEvent(t, agent)
		written = append(written, item)
		floor = seq
	}
	requireFrames(t, standIn.collect(t, len(written), 10*time.Second), written...)
	awaitAcked(t, rig.consumers, sessionID, floor)
	recorded := rig.entry(t, sessionID).ConsumerStart
	if recorded > floor {
		t.Fatalf("the entry of %s records consumer start %d, above the floor %d it has acked through: this row needs a record whose start is below the floor",
			sessionID, recorded, floor)
	}

	// The delete a clean stop racing its own relaunch makes, and the one a reaper pass makes of a
	// record a registration has written again: the record is in the bucket, so the consumer is
	// restored.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rig.deps.consumers.deleteConsumer(ctx, rig.deps, sessionID); err != nil {
		t.Fatalf("delete the consumer of %s: %v", sessionID, err)
	}

	info, err := rig.consumers.Info(ctx, sessionID)
	if err != nil {
		t.Fatalf("read the consumer restored for %s: %v", sessionID, err)
	}
	if info.Config.OptStartSeq != floor+1 {
		t.Fatalf("the restored consumer of %s starts at %d, want %d, the sequence after the floor it had acked through; its record names start %d",
			sessionID, info.Config.OptStartSeq, floor+1, recorded)
	}
	if info.NumPending != 0 {
		t.Fatalf("the restored consumer of %s holds %d messages for delivery, want none: it has acked every one the stream holds for it",
			sessionID, info.NumPending)
	}
	standIn.none(t, time.Second)
}
