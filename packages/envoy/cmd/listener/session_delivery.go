package main

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// The modes of a registration's delivery answer (deliveryAnswer.Mode).
const (
	// deliveryModeConsumer: the listener holds the session's consumer, filtered on the session's
	// agent subject and its stored followed topics, and relays it on the session's relay subject.
	deliveryModeConsumer = "consumer"
	// deliveryModeCore: the session stays on the core path, its core subscriptions and the
	// durable's Skipped acknowledgement, as before consumer delivery.
	deliveryModeCore = "core"
)

// The reasons of a core delivery answer (deliveryAnswer.Reason), short and machine-readable.
const (
	// deliveryReasonNoGrant: the listener's NATS user may not manage session consumers (a hub whose
	// grant predates them, bus.ErrConsumerNotGranted).
	deliveryReasonNoGrant = "no_grant"
	// deliveryReasonInvalidSessionID: the session id names no consumer (bus.SessionConsumerName).
	deliveryReasonInvalidSessionID = "invalid_session_id"
	// deliveryReasonConsumerError: the consumer could not be read, created or updated, or this
	// listener has no session-consumer connection.
	deliveryReasonConsumerError = "consumer_error"
	// deliveryReasonNotRequested: the registration carried delivery without asking for a consumer.
	deliveryReasonNotRequested = "not_requested"
)

// sessionConsumerCallTimeout bounds the consumer calls a registration makes before it answers.
const sessionConsumerCallTimeout = 5 * time.Second

// deliveryAnswer is the delivery field of a registration's answer (subscribeResponse.Delivery),
// present whenever the registration carried delivery or registered a serving port. A listener of an
// earlier release answers without it, which the plugin reads as core.
type deliveryAnswer struct {
	Mode string `json:"mode"`
	// Subject is the session's relay subject (bus.DeliverSubject), in consumer mode.
	Subject string `json:"subject,omitempty"`
	// StreamID is the stream's identity (bus.StreamIDMetadataKey), in consumer mode: the one a
	// relay frame carries and the plugin's resume point names.
	StreamID string `json:"stream_id,omitempty"`
	// Gap, in consumer mode, is true when this registration created the consumer at a start below
	// the stream's first sequence: messages it would have delivered are gone.
	Gap *bool `json:"gap,omitempty"`
	// Reason says why, in core mode.
	Reason string `json:"reason,omitempty"`
}

// sessionDeliveryOutcome is what sessionDelivery.register did for one registration.
type sessionDeliveryOutcome struct {
	// answer is the registration answer's delivery field, nil when the answer carries none.
	answer *deliveryAnswer
	// relayed is set when the session's entry records consumer delivery
	// (session.DeliveryConsumer): the consumer is held and the relay serves it, so the durable path
	// skips the session's stored messages.
	relayed bool
	// epoch is the plugin's epoch, recorded on the entry when relayed.
	epoch string
}

// sessionRelay is what the relay of session consumers hears from registrations.
type sessionRelay interface {
	// registered reports a registration of sessionID that recorded consumer delivery on its entry.
	// epochChanged is set when the entry it replaced did not hold consumer delivery with the same
	// epoch: the plugin connected anew, or came over from the core path, so the relay restarts
	// the session's consumer at its ack floor before it relays again.
	registered(sessionID string, epochChanged bool)
}

// sessionDelivery manages the session consumers (bus.SessionConsumers) for the listener's
// registrations: it creates each one a registration asks for, keeps its filter on the session's
// agent subject and stored followed topics as they change, and tells the relay of each
// registration. A nil sessionDelivery, or one without consumers (the session-consumer connection
// failed at start), answers every registration that asks with core.
type sessionDelivery struct {
	consumers *bus.SessionConsumers
	logger    *logging.Logger
	// relay hears each registration that records consumer delivery; nil while no relay runs.
	relay sessionRelay
	// servingPortRelayed is set once the relay serves serving-port sessions. Until then a serving
	// port session's consumer is created and kept, and its answer says consumer, but its entry
	// records no consumer delivery, so the durable path keeps pushing to its port.
	servingPortRelayed bool
	// noGrantLogged is set once the first refusal for want of a grant is logged, so a hub without
	// the grant costs one line per listener process rather than one per registration.
	noGrantLogged atomic.Bool
}

// connectSessionDelivery opens the session-consumer connection (bus.ConnectSessionConsumers) on
// urls. A connection that fails is logged once and leaves the listener serving every registration
// on the core path, as a listener of an earlier release does.
func connectSessionDelivery(urls []string, logger *logging.Logger) *sessionDelivery {
	consumers, err := bus.ConnectSessionConsumers(urls)
	if err != nil {
		logger.Error("session consumer connection failed; every session stays on core delivery", slog.String("error", err.Error()))
		return &sessionDelivery{logger: logger}
	}
	return &sessionDelivery{consumers: consumers, logger: logger}
}

// close closes the session-consumer connection, if one is open.
func (m *sessionDelivery) close() {
	if m != nil && m.consumers != nil {
		m.consumers.Close()
	}
}

// register gives a registration the delivery it asks for, once its topics are stored: topics are
// the session's stored interest topics. A registration that carried delivery with consumer set, or
// registered a serving port, gets the session's consumer, created when the stream holds none (at
// the sequence after its resume point when that names the stream's current identity, else at the
// stream's end) and otherwise updated to the current filter, before the registration answers.
func (m *sessionDelivery) register(ctx context.Context, body subscribeBody, topics []string) sessionDeliveryOutcome {
	if body.Delivery == nil && body.Port <= 0 {
		return sessionDeliveryOutcome{}
	}
	if body.Port <= 0 && !body.Delivery.Consumer {
		return coreDeliveryOutcome(deliveryReasonNotRequested)
	}
	if _, ok := bus.SessionConsumerName(body.SessionID); !ok {
		return coreDeliveryOutcome(deliveryReasonInvalidSessionID)
	}
	if m == nil || m.consumers == nil {
		return coreDeliveryOutcome(deliveryReasonConsumerError)
	}
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	streamID, err := m.consumers.StreamID(ctx)
	if err != nil {
		return m.refused(body.SessionID, err)
	}
	var startSeq uint64
	var epoch string
	if body.Delivery != nil {
		epoch = body.Delivery.Epoch
		if resume := body.Delivery.Resume; resume != nil && resume.Seq > 0 && resume.StreamID == streamID {
			startSeq = resume.Seq + 1
		}
	}
	start, err := m.consumers.Ensure(ctx, body.SessionID, sessionConsumerFilter(body.SessionID, topics), startSeq)
	if err != nil {
		return m.refused(body.SessionID, err)
	}
	gap := start.Gap
	return sessionDeliveryOutcome{
		answer: &deliveryAnswer{
			Mode:     deliveryModeConsumer,
			Subject:  bus.DeliverSubject(body.SessionID),
			StreamID: streamID,
			Gap:      &gap,
		},
		relayed: body.Port <= 0 || m.servingPortRelayed,
		epoch:   epoch,
	}
}

// coreDeliveryOutcome is the outcome of a registration left on the core path for reason.
func coreDeliveryOutcome(reason string) sessionDeliveryOutcome {
	return sessionDeliveryOutcome{answer: &deliveryAnswer{Mode: deliveryModeCore, Reason: reason}}
}

// refused is the core outcome of a consumer call for sessionID that failed with err.
func (m *sessionDelivery) refused(sessionID string, err error) sessionDeliveryOutcome {
	if errors.Is(err, bus.ErrConsumerNotGranted) || errors.Is(err, bus.ErrPublishDenied) {
		if m.noGrantLogged.CompareAndSwap(false, true) {
			m.logger.Warn("the NATS user may not manage session consumers; sessions stay on core delivery until the grant carries them",
				slog.String("session_id", sessionID), slog.String("error", err.Error()))
		}
		return coreDeliveryOutcome(deliveryReasonNoGrant)
	}
	m.logger.Warn("session consumer call failed; the session stays on core delivery",
		slog.String("session_id", sessionID), slog.String("error", err.Error()))
	return coreDeliveryOutcome(deliveryReasonConsumerError)
}

// registered tells the relay of a registration that recorded consumer delivery for sessionID
// (sessionRelay.registered).
func (m *sessionDelivery) registered(sessionID string, epochChanged bool) {
	if m != nil && m.relay != nil {
		m.relay.registered(sessionID, epochChanged)
	}
}

// refollow sets the filter of sessionID's consumer to its stored interest topics after a change
// that removed some (an unsubscribe), when the session's entry records consumer delivery; a session
// on the core path, or whose consumer is gone, is left alone, and its next registration sets the
// filter. A failure is logged, not returned: the consumer then still delivers a topic the session
// no longer follows, and the plugin drops a frame no followed topic matches.
func (m *sessionDelivery) refollow(ctx context.Context, d *listenerDeps, sessionID string) {
	if m == nil || m.consumers == nil {
		return
	}
	entry, err := d.sessions.Get(sessionID)
	if err != nil || entry.Delivery != session.DeliveryConsumer {
		return
	}
	var topics []string
	interest, err := d.registry.Get(sessionID)
	switch {
	case err == nil:
		topics = interest.Topics
	case !errors.Is(err, nats.ErrKeyNotFound):
		m.logger.Warn("session consumer filter left unchanged: the session's interests could not be read",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	err = m.consumers.UpdateFilter(ctx, sessionID, sessionConsumerFilter(sessionID, topics))
	if err != nil && !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		m.logger.Warn("session consumer filter update failed", slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
}

// sessionConsumerFilter is the subjects sessionID's consumer filters on: its agent subject and
// every one of topics the stream stores (bus.StreamCaptures). A topic the stream does not store (a
// team or other ad-hoc topic, a role lane) reaches the session over its core subscription only.
// SessionConsumers reduces the list to filters that do not overlap.
func sessionConsumerFilter(sessionID string, topics []string) []string {
	filter := make([]string, 0, len(topics)+1)
	filter = append(filter, contracts.AgentSubject(sessionID))
	for _, topic := range topics {
		if bus.StreamCaptures(topic) {
			filter = append(filter, topic)
		}
	}
	return filter
}
