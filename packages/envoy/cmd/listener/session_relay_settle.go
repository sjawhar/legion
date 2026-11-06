package main

// The settle a term's end gives a relay's last frames: the grace, and what the stopped relay
// does with the frames the session has not answered when it runs out. The relay itself is in
// session_relay.go; this is the one part of it a take-over's correctness rests on, so it is
// read on its own.

import (
	"context"
	"log/slog"
	"time"
)

const (
	// relaySettleGrace is how long a relay the term's end stopped lets the frames it has not had
	// done or nak for be answered before it gives up on them. The term's end cancels the relay,
	// but a frame the session may already have written must not be left unacked for the pod
	// taking over to publish again: inside this grace the stopped relay goes on hearing the
	// session's replies and acks each frame the session answers done, which lets the consumer's
	// ack floor move past it once nothing below it is held. A frame the grace ends without a
	// done is in doubt (relayFrameInDoubtLine), whether or not the session answered it received:
	// received says the client decoded the frame, not that its host wrote the message, so only
	// done says the session took it and no done is coming. The successor's restart then sends
	// again every frame above the ack floor, each under its own event id: the one in doubt, and
	// any the session answered done above a frame still held (the README's "When a session can
	// see a message twice").
	//
	// The pod taking a released holder's lease over waits relayFormerSettle, this grace and a
	// margin, before it restarts a session's consumer at its ack floor (sessionRelays.relay),
	// so the grace always ends before the successor can publish anything. A holder taken over
	// by force - killed, its node lost, its record expired - settles nothing and is not waited
	// for.
	//
	// 750 ms is that round trip with room for a loaded box. Measured on the test host over 200
	// frames sent one at a time, from the frame reaching the session to the consumer's ack floor
	// passing it: p50 2.44 ms, p99 11.8 ms, slowest 37.2 ms on a quiet box, and p50 2.58 ms,
	// p99 8.26 ms, slowest 111 ms with the rest of this package running beside it
	// (TestAFramesRoundTripIsFarInsideTheRelaysSettleGrace), so the grace is between six and
	// twenty times the slowest of them. The grace is a cost as well as a cover: a frame nothing answers delays
	// nothing but its own session, whose later messages the successor sends only after
	// relayFormerSettle anyway, and it is paid on the shutdown path too, where
	// sessionRelays.stop waits for it out of what the receive stop left of the shutdown's 10 s
	// budget, and is cut short with a warning when that budget is already spent.
	relaySettleGrace = 750 * time.Millisecond
	// relaySettlePoll is how often a settling relay reads whether the frames it waits for have
	// been answered.
	relaySettlePoll = 10 * time.Millisecond
)

// endFrames ends the frames one pull left outstanding. A relay the term's end stopped settles
// them first (settleStoppedFrames); any other end - a retry, a pause, a reconnect - forgets them
// at once, since the relay itself goes on and restarts at the session's ack floor.
func (m *sessionRelays) endFrames(ctx context.Context, run *relayRun) {
	if ctx.Err() != nil && !m.owned.Load() {
		m.settleStoppedFrames(run)
		return
	}
	m.forget(run)
}

// settleStoppedFrames gives the frames a stopped relay has outstanding - those it has had no
// done or nak for - relaySettleGrace to be answered: the reply subscription and the
// session-consumer connection are both still up, so a session that answers done inside it has
// its message acked here, and the consumer's ack floor moves past that message once nothing
// below it is held. A frame the grace ends without a done is in doubt, a frame the session
// answered received included: received says the client decoded the frame, not that its host
// wrote the message, so nothing says the session took it, and one line names it. The pod taking
// over, which waits relayFormerSettle before it restarts the consumer, sends again every frame
// above the ack floor, each under its own event id (the README's "When a session can see a
// message twice").
func (m *sessionRelays) settleStoppedFrames(run *relayRun) {
	deadline := time.Now().Add(relaySettleGrace)
	for m.outstanding(run) > 0 && time.Now().Before(deadline) {
		time.Sleep(relaySettlePoll)
	}
	for _, frame := range m.forget(run) {
		item := frame.envelope()
		m.logger.DeliveryLog(slog.LevelWarn, relayFrameInDoubtLine, run.sessionID, item.Topic, item.EventID, "retrying",
			slog.String("subject", frame.subject), slog.Uint64("stream_seq", frame.streamSeq))
	}
}

// relayFrameInDoubtLine is the line settleStoppedFrames logs for each frame a term's end leaves
// without a done; the row
// TestTwoPodsARelayedFrameHeldPastTheStoppingRelaysSettleBoundIsRepeatedAtMostOnceWithItsId
// reads it.
const relayFrameInDoubtLine = "session relay left a frame in doubt: its settle bound ended with the session's reply still out"
