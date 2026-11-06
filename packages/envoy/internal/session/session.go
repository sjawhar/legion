package session

import (
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// ErrNoSessionRecord is returned when a delivery found nothing of the session it names to deliver
// through: no session entry was resolved at all, or the entry resolved records no way the session
// receives a message - no consumer of its own, and no subscription it holds itself, which is what
// a session registered through a plugin older than both leaves behind. Neither cause says the
// session is gone. A caller that resolved the entry from a cache alone cannot conclude that - the
// cache trails the bucket it follows - and an entry that records neither way is rewritten by the
// session's next registration, so a caller may wait rather than give the message up, and the
// listener's delivery does, deferring such a message for its whole retry window
// (cmd/listener/unplaceable_delivery.go).
var ErrNoSessionRecord = errors.New("no session record")

// DeliveryResult describes a completed delivery attempt.
type DeliveryResult struct {
	Skipped bool
	// Relayed is set with Skipped by Resolve for a message the session's own consumer holds, so
	// the relay of that consumer is what delivers it (ConsumerRelaysStored): the caller records
	// nothing about it, neither an attempt nor a dedupe key, since it delivered nothing. A
	// dedupe key says the session has the message, and only a delivery may say that.
	Relayed bool
}

// ConsumerRelaysStored reports whether the session's own consumer holds the stored message of
// stream sequence storedSeq on topic, so the relay of that consumer (cmd/listener's sessionRelays)
// is what delivers it and no other path may record anything about it: a dedupe key written for a
// message this listener handed to nobody would have the next copy of the same event skipped as
// one the session already has, and an attempt record would have a redelivery of it skipped as one
// already on its way. A topic the stream does not capture is in no consumer whatever the
// session's delivery is, so the session takes such a message off its own subscription.
func (e SessionEntry) ConsumerRelaysStored(topic string, storedSeq uint64) bool {
	return e.ConsumerHoldsStored(storedSeq) && bus.StreamCaptures(topic)
}

// Resolve decides what a delivery of item to sessionID's session at entry comes to: the session's
// own consumer relays the message, the session takes it off its own NATS subscription, or the
// entry records no way of receiving at all. A result with Skipped set needs nothing of this
// delivery, because the session receives the message another way, and an error is the delivery's
// failure. A nil entry produces ErrNoSessionRecord, the same result a failed lookup produces, so
// a caller can avoid a second registry read.
//
// It is a package function because the decision holds no state at all: every caller resolves the
// entry it decides on and hands it over, so the record the decision acts on is the one that
// caller read, and nothing of the listener it runs in enters the decision.
//
// storedSeq is the stream sequence of the durable path's copy of a stored message, 0 for any
// other delivery. A message is stored on the subject its envelope names as its topic, so that
// topic and the sequence together decide whether a session on consumer delivery is already held
// this message by its own consumer (SessionEntry.ConsumerRelaysStored). A core path's message is
// not the durable's copy of a stored one, so its caller passes a sequence of 0, which leaves a
// session on consumer delivery its consumer's copy whatever that copy's sequence is.
func Resolve(item contracts.Envelope, sessionID string, entry *SessionEntry, storedSeq uint64) (DeliveryResult, error) {
	if entry == nil {
		return DeliveryResult{}, fmt.Errorf("%w for session %s", ErrNoSessionRecord, sessionID)
	}
	if entry.ConsumerRelaysStored(item.Topic, storedSeq) {
		// The session's consumer holds this stored message, and the listener that owns the session
		// relays it from there: no other path delivers it, and none records anything about it. A
		// topic the stream does not store is in no consumer, and so is a durable message stored
		// below the consumer's start; such a message reaches the session on its own subscription,
		// which is where a team or other ad-hoc topic arrives, by the branch below.
		return DeliveryResult{Skipped: true, Relayed: true}, nil
	}
	if entry.SelfSubscribed {
		// The session holds its own NATS subscription and the message reaches it there, so this
		// is a delivered outcome with nothing for this listener to do. A session on consumer
		// delivery took the branch above for a message its consumer holds and takes this one for
		// a message no consumer of its own can carry.
		return DeliveryResult{Skipped: true}, nil
	}
	// The entry records neither way of receiving: nothing reaches this session until it registers
	// again (ErrNoSessionRecord).
	return DeliveryResult{}, fmt.Errorf("%w for session %s", ErrNoSessionRecord, sessionID)
}
