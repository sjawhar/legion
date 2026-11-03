package main

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// relayFrameRoundTrips is how many frames the measurement below sends, one at a time.
const relayFrameRoundTrips = 200

// The settle grace a stopping relay gives the frames it has unanswered (relaySettleGrace) is
// sized on one frame's round trip: the session's reply to it and the relay's acknowledgement
// after that reply, which is all the grace has to cover. This row measures that round trip on
// the test host, from the moment the frame reaches the session to the moment the consumer's ack
// floor has passed it, over relayFrameRoundTrips frames sent one at a time, and refuses a grace
// that is not comfortably above the slowest of them. It is the frame world's replacement for the
// push-latency measurement the serving-port relay's grace was sized on.
//
// It fails if the p99 round trip reaches a tenth of the grace, or the slowest reaches half of
// it: a session that answers at once must stay well inside the grace, which also has to cover a
// loaded box, and a change that makes a round trip that slow is a change the grace must be
// re-read for. The slowest of 200 is noise on a box running the rest of this package beside it
// (111 ms seen in a full run against 37 ms on a quiet one), so the p99 carries the tight bound.
func TestAFramesRoundTripIsFarInsideTheRelaysSettleGrace(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_frame_latency"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-latency"))
	subject := contracts.AgentSubject(sessionID)
	consumer, ok := bus.SessionConsumerName(sessionID)
	if !ok {
		t.Fatalf("no session consumer name for %s", sessionID)
	}

	ackedPast := func(seq uint64) time.Time {
		deadline := time.Now().Add(10 * time.Second)
		for {
			info, err := rig.harness.client.JS().ConsumerInfo(bus.Stream, consumer)
			if err == nil && info.AckFloor.Stream >= seq {
				return time.Now()
			}
			if time.Now().After(deadline) {
				t.Fatalf("the relay never acked the message at stream sequence %d: %+v, %v", seq, info, err)
			}
			time.Sleep(200 * time.Microsecond)
		}
	}

	roundTrips := make([]time.Duration, 0, relayFrameRoundTrips)
	for range relayFrameRoundTrips {
		_, seq := rig.storeEvent(t, subject)
		var frame relayedFrame
		select {
		case frame = <-standIn.frames:
		case <-time.After(15 * time.Second):
			t.Fatalf("the relay never sent the message at stream sequence %d to the session", seq)
		}
		roundTrips = append(roundTrips, ackedPast(seq).Sub(frame.arrived))
	}

	slices.Sort(roundTrips)
	p50 := roundTrips[len(roundTrips)/2]
	p99 := roundTrips[(len(roundTrips)*99)/100]
	slowest := roundTrips[len(roundTrips)-1]
	t.Logf("%d frame round trips (the session's reply and the relay's ack after it): p50 %s, p99 %s, slowest %s, against relaySettleGrace %s",
		len(roundTrips), p50.Round(time.Microsecond), p99.Round(time.Microsecond), slowest.Round(time.Microsecond), relaySettleGrace)
	if p99 >= relaySettleGrace/10 {
		t.Fatalf("the p99 frame round trip is %s, a tenth of relaySettleGrace (%s) or more: the grace is sized on this round trip and must be re-read",
			p99, relaySettleGrace)
	}
	if slowest >= relaySettleGrace/2 {
		t.Fatalf("the slowest frame round trip is %s, half of relaySettleGrace (%s) or more: the grace is sized on this round trip and must be re-read",
			slowest, relaySettleGrace)
	}
}
