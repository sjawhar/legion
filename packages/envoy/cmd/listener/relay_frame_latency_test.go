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
// It gates on one thing: the slowest of the round trips must stay under half the grace. That is
// what the grace protects - a frame whose reply and ack do not fit inside it is left in doubt
// and sent again - and half of the grace is the margin the measured times ask for: beside the
// rest of this package the slowest round trip measured 120.8 ms, three times under the 375 ms
// bound, against 37.2 ms on a quiet box.
//
// The percentiles are logged and not gated. p99 of 200 samples is the second-slowest sample, so
// a gate on it fails whenever any two of them are slow, which a run beside this package's other
// tests produces at random: p99 was 79.3 ms and 87.0 ms in two of five such runs, against
// 5.8-13.8 ms alone. That is the box being busy, not the round trip changing.
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
	p90 := roundTrips[(len(roundTrips)*90)/100]
	p99 := roundTrips[(len(roundTrips)*99)/100]
	slowest := roundTrips[len(roundTrips)-1]
	t.Logf("%d frame round trips (the session's reply and the relay's ack after it): p50 %s, p90 %s, p99 %s, slowest %s, against relaySettleGrace %s",
		len(roundTrips), p50.Round(time.Microsecond), p90.Round(time.Microsecond), p99.Round(time.Microsecond), slowest.Round(time.Microsecond), relaySettleGrace)
	if slowest >= relaySettleGrace/2 {
		t.Fatalf("the slowest frame round trip is %s, half of relaySettleGrace (%s) or more: the grace is sized on this round trip and must be re-read",
			slowest, relaySettleGrace)
	}
}
