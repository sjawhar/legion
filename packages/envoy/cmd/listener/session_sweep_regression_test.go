package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A brand-new session that registers after a message was stored for one of its subjects must
// receive that message once, as sjawhar/legion#2720's sweep delivered it on main: the case is a
// session whose first registration follows messages already stored for its subjects (its NATS
// connection was down while they were published, or a send raced its first registration), with no
// process before it to have persisted a resume point.
//
// INFRA-771 removes the /v1/replay sweep that back-filled this. At the head a registration with no
// usable resume point creates the session's consumer at the stream's end
// (TestSessionConsumerRegistrationWithoutAUsableResumePointStartsAtTheStreamsEnd), so the stored
// message is in no consumer and this test times out collecting it. It is the regression INFRA-514's
// architect ruled must be fixed in this pull request; the resumed implementer makes it pass.
func TestSessionConsumerABrandNewSessionReceivesAMessageStoredForItBeforeItRegistered(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_brand_new_sweep"
	agent := contracts.AgentSubject(sessionID)

	// Stored for the session before it ever registered, so no resume point names it.
	stored, _ := rig.storeEvent(t, agent)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)

	// A brand-new registration: consumer mode, no resume point.
	answer := rig.subscribe(t, consumerRegistration(sessionID, "e"))
	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", answer.raw)
	}

	frames := standIn.collect(t, 1, 5*time.Second)
	requireFrames(t, frames, stored)
	standIn.none(t, time.Second)
}
