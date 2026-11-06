package main

// How a message on a topic a session follows is judged, and how the fan-out reports a refusal
// of it. The judgement is the durable path's fan-out's alone (fanoutDelivery in delivery.go):
// the relay of a session's own consumer publishes every frame unjudged, which is why a refusal
// of a session on consumer delivery is reported here.

import (
	"errors"
	"log/slog"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// followedTopicVerdict is what a delivery path does with a message on a topic a session follows.
type followedTopicVerdict uint8

const (
	// takeFollowedTopic: deliver the message to the session.
	takeFollowedTopic followedTopicVerdict = iota
	// followedTopicEcho: the session published the message itself.
	followedTopicEcho
	// followedTopicModeUnreadable: the envelope's targeted delivery mode cannot be read
	// unambiguously, so no session can be shown to advertise it.
	followedTopicModeUnreadable
	// followedTopicModeNotAdvertised: the session's live entry does not advertise the mode.
	followedTopicModeNotAdvertised
)

// judgeFollowedTopic is the judgement the durable path's fan-out (fanoutDelivery) makes of a
// message on a topic sessionID follows, against the session entries sessions holds: an echo of
// the session's own message, and a delivery mode its entry does not advertise or one that
// cannot be read, are refused; everything else is the session's to take. The relay of a
// session's own consumer judges nothing it publishes (sessionRelays.relayNATS), which is why
// the fan-out reports a refusal of a session on consumer delivery itself.
//
// A copy the session already took, and one already on its way to it, are no part of this
// judgement: both are the machine's shared delivery ledger's answer, which the fan-out asks for
// and settles itself (ledgerAttempt). They are keyed on the envelope's dedupe key, so one
// upstream event published on several of a session's topics (contracts/normalize.go) is
// reported or recorded once, from either pod of the machine.
//
// mode is the delivery mode the envelope targets, "" when it targets none, and modeErr the failure
// of reading it (envelopeDeliveryMode): both are read once per message rather than once per
// recipient.
//
// It returns the session's live entry whenever one was read, for every verdict and not only the
// ones judged against it: a caller that delivers through the entry does not read it again, and
// the fan-out decides from it whether the session's own consumer relays this message, which it
// must know for a refusal as much as for a delivery (fanoutDelivery). It returns the error behind
// a mode refusal too, for the caller to report.
func judgeFollowedTopic(sessions *session.SessionRegistry, sessionID string, item contracts.Envelope, mode string, modeErr error) (followedTopicVerdict, *session.SessionEntry, error) {
	var target *session.SessionEntry
	if entry, err := sessions.Get(sessionID); err == nil {
		target = &entry
	}
	if item.SourceSession != "" && item.SourceSession == sessionID {
		return followedTopicEcho, target, nil
	}
	if modeErr != nil {
		return followedTopicModeUnreadable, target, modeErr
	}
	if target == nil {
		// No entry is no refusal: the delivery path decides what to do with a session it cannot
		// read, and a mode nobody advertises cannot be judged against an entry there is none of.
		return takeFollowedTopic, nil, nil
	}
	if mode != "" && !hasCapability(target.Capabilities, mode) {
		return followedTopicModeNotAdvertised, target, errors.New("the session does not advertise delivery mode " + mode)
	}
	return takeFollowedTopic, target, nil
}

// fanoutRefusalOutcome is how the fan-out reports a refusal judgeFollowedTopic returned, each
// under the line and the metric status it has always had. keep says whether the pair's ledger
// attempt is one to leave standing: a mode refusal of a session this path resolved and delivers
// to itself keeps it, so an identical redelivery inside dedupe.AttemptedAge is the attempt-dedupe
// skip ledgerAttempt reports rather than a second exception. A refusal whose resolution failed,
// and one of a session whose own consumer relays the message, gives the attempt up. An attempt is
// kept only together with the exception that reports it. An echo is refused before the ledger is
// asked and settles nothing.
func fanoutRefusalOutcome(sessionID string, item contracts.Envelope, verdict followedTopicVerdict, mode string, attempt dedupe.Attempt, keep bool) deliveryOutcome {
	settle := settleClear
	if keep {
		settle = settleKeep
	}
	switch verdict {
	case followedTopicEcho:
		return deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "skipped",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener skip echo", sessionID, item.Topic, item.EventID, "skipped")
			},
		}
	case followedTopicModeUnreadable:
		return deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener delivery mode cannot be read unambiguously", sessionID, item.Topic, item.EventID, "failed")
			},
			exceptionReason:               "delivery_failed",
			fanoutRefusal:                 true,
			settle:                        settle,
			exceptionFailureClearsAttempt: keep,
		}
	default:
		return deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener subscriber does not advertise delivery mode", sessionID, item.Topic, item.EventID, "failed", slog.String("mode", mode))
			},
			exceptionReason:               "delivery_failed",
			fanoutRefusal:                 true,
			settle:                        settle,
			exceptionFailureClearsAttempt: keep,
		}
	}
}
