package session

import (
	"errors"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// HandleAgentReason names why a delivery attempt did not land, for the one question the caller
// cannot answer from Delivered alone: whether waiting could change the answer. The two outcomes
// reach the caller as one undelivered result otherwise, and it would have to re-derive them from
// the error text to tell them apart.
type HandleAgentReason uint8

const (
	// ReasonNone is every outcome that is not the one below: a delivery that landed, a skip (the
	// session reads the message off its own subscription, or its own consumer relays it), and a
	// session another listener owns, which both paths answer as OwnedElsewhere: that listener
	// delivers the message, so this one has no cause to report.
	ReasonNone HandleAgentReason = iota
	// ReasonNoSessionRecord is a session the resolution found nothing to deliver through: no
	// record of it at all, or a record that names no way the session receives a message
	// (ErrNoSessionRecord). A caller holding a record source that trails the store - or one
	// whose read did not answer - may ask again, and so may one that read a record naming
	// neither way, which the session's next registration rewrites: the listener's delivery
	// waits, deferring such a message for its whole retry window before it gives up
	// (cmd/listener/unplaceable_delivery.go).
	ReasonNoSessionRecord
)

// reasonFor maps the delivery sentinel onto the reason the caller acts on. ErrNoSessionRecord is
// the only error a delivery returns, so the default stands for nothing a deployed listener
// reaches: an error that is not that sentinel is no statement about the session's record, and
// ReasonNone holds no retry open for it.
func reasonFor(err error) HandleAgentReason {
	if errors.Is(err, ErrNoSessionRecord) {
		return ReasonNoSessionRecord
	}
	return ReasonNone
}

// HandleAgentResult captures the outcome of agent message handling.
type HandleAgentResult struct {
	Delivered bool
	Skipped   bool
	// Relayed is set with Delivered and Skipped for a message the session's own consumer holds,
	// which the relay of that consumer delivers (DeliveryResult.Relayed): the caller records
	// nothing about it, neither an attempt nor a dedupe key.
	Relayed bool
	// OwnedElsewhere reports a session whose stored subscription names another listener. That
	// listener reads the same inbox message from its own durable and delivers it, so this one
	// acknowledges it with nothing to report: no delivery, no exception, no NAK.
	OwnedElsewhere bool
	ShouldNAK      bool
	Err            error
	// Reason tells apart the two outcomes of an undelivered message that carries no error for
	// the caller to act on: the registry path's undelivered result, which the listener's delivery
	// retries or gives up on by this field alone. The NAKing interest path sets it too, but its
	// result carries Err and ShouldNAK, and the listener answers that result by those before it
	// ever reads Reason (cmd/listener/delivery.go, agentSubjectDelivery), so there it is read by
	// nothing. An OwnedElsewhere result carries ReasonNone.
	Reason HandleAgentReason
}

// HandleAgentMessage handles delivery of an agent-to-agent message.
// It tries the interest path first (KV subscription match), then falls back
// to the session entry its caller resolved. Returns the result so the caller
// can decide ACK vs NAK.
//
// entry is that resolved session record, nil when none was resolved; both paths deliver through
// it (DeliverWithResultForEntry) rather than resolving it a second time, so the record the caller
// read - the store's own word for it, not just this listener's cache - is the one delivery acts on.
func HandleAgentMessage(
	item contracts.Envelope,
	sessionID string,
	machineID string,
	interest *store.Interest,
	entry *SessionEntry,
	deliverer *Deliverer,
) HandleAgentResult {
	return handleAgentMessage(item, sessionID, machineID, interest, entry, deliverer.DeliverWithResultForEntry)
}

// ResolveStoredAgentMessage is HandleAgentMessage for the durable path's copy of a stored message
// of stream sequence storedSeq. The sequence decides whether a session on consumer delivery is
// already held this message by its own consumer, which relays it (Deliverer.Resolve).
func ResolveStoredAgentMessage(
	item contracts.Envelope,
	sessionID string,
	machineID string,
	interest *store.Interest,
	entry *SessionEntry,
	deliverer *Deliverer,
	storedSeq uint64,
) HandleAgentResult {
	return handleAgentMessage(item, sessionID, machineID, interest, entry, func(item contracts.Envelope, interest store.Interest, entry *SessionEntry) (DeliveryResult, error) {
		// An agent message is stored on the subject it names as its topic, the session's own
		// agent subject.
		return deliverer.Resolve(item, interest, entry, item.Topic, storedSeq)
	})
}

func handleAgentMessage(
	item contracts.Envelope,
	sessionID string,
	machineID string,
	interest *store.Interest,
	entry *SessionEntry,
	deliver func(contracts.Envelope, store.Interest, *SessionEntry) (DeliveryResult, error),
) HandleAgentResult {
	// Interest path: the stored subscription names this listener, which owns the session.
	if interest != nil && interest.MachineID == machineID {
		delivery, err := deliver(item, *interest, entry)
		if err != nil {
			return HandleAgentResult{ShouldNAK: true, Err: err, Reason: reasonFor(err)}
		}
		return delivered(delivery)
	}
	// A stored subscription naming another listener makes the session that listener's: the
	// stored MachineID is the one its topics are fanned out on (store.Registry.Match), and that
	// listener reads this message from its own durable too. The row names the listener the
	// session registered through most recently (store's mergeForUpsert), so this is also the
	// listener it is registered through, give or take a registration still in flight.
	if interest != nil && interest.MachineID != "" {
		return HandleAgentResult{OwnedElsewhere: true}
	}

	// No stored subscription names an owner, so this listener answers for the session: there is
	// nobody else the message can be left to. The entry is the one the caller resolved, delivered
	// through directly rather than read a second time.
	delivery, err := deliver(item, store.Interest{SessionID: sessionID}, entry)
	if err != nil {
		// Session not found or delivery failed without interest — don't NAK,
		// another listener may own this session.
		return HandleAgentResult{Delivered: false, Reason: reasonFor(err)}
	}
	return delivered(delivery)
}

// delivered is the result of a delivery that succeeded.
func delivered(delivery DeliveryResult) HandleAgentResult {
	return HandleAgentResult{Delivered: true, Skipped: delivery.Skipped, Relayed: delivery.Relayed}
}
