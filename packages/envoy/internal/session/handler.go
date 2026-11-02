package session

import (
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

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
}

// HandleAgentMessage handles delivery of an agent-to-agent message.
// It tries the interest path first (KV subscription match), then falls back
// to the session entry its caller resolved. Returns the result so the caller
// can decide ACK vs NAK.
//
// entry is that resolved session record, nil when none was resolved; both paths deliver through
// it (Resolve) rather than resolving it a second time, so the record the caller read - the
// store's own word for it, not just this listener's cache - is the one delivery acts on.
//
// storedSeq is the stream sequence the delivery resolves under: the durable path's copy of a
// stored message carries its own, and a core path's message, which is not the durable's copy of
// anything, resolves under 0, which leaves a session on consumer delivery its consumer's copy
// whatever that copy's sequence is. An agent message is stored on the subject it names as its
// topic, the session's own agent subject, so Resolve reads that topic for both paths below.
func HandleAgentMessage(
	item contracts.Envelope,
	sessionID string,
	machineID string,
	interest *store.Interest,
	entry *SessionEntry,
	storedSeq uint64,
) HandleAgentResult {
	// Interest path: the stored subscription names this listener, which owns the session.
	if interest != nil && interest.MachineID == machineID {
		delivery, err := Resolve(item, sessionID, entry, storedSeq)
		if err != nil {
			return HandleAgentResult{ShouldNAK: true, Err: err}
		}
		return delivered(delivery)
	}
	// A stored subscription naming another listener makes the session that listener's: the
	// stored MachineID is the one its topics are fanned out on (store.Registry.Match), and that
	// listener reads this message from its own durable too. The row names the listener the
	// session registered through most recently (store's mergeForUpsert), so that is also the
	// listener it is registered through, give or take a registration still in flight.
	if interest != nil && interest.MachineID != "" {
		return HandleAgentResult{OwnedElsewhere: true}
	}

	// No stored subscription names an owner, so this listener answers for the session: there is
	// nobody else the message can be left to. The entry is the one the caller resolved, delivered
	// through directly rather than read a second time.
	delivery, err := Resolve(item, sessionID, entry, storedSeq)
	if err != nil {
		// Resolve fails only with ErrNoSessionRecord. The result carries no error, so the
		// listener defers the message for its retry window (cmd/listener/unplaceable_delivery.go)
		// rather than NAK it.
		return HandleAgentResult{Delivered: false}
	}
	return delivered(delivery)
}

// delivered is the result of a delivery that succeeded.
func delivered(delivery DeliveryResult) HandleAgentResult {
	return HandleAgentResult{Delivered: true, Skipped: delivery.Skipped, Relayed: delivery.Relayed}
}
