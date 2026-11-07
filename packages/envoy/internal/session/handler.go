package session

import (
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// HandleAgentResult captures the outcome of agent message handling: the message the session's own
// consumer holds and its relay delivers (Relayed), the session another listener owns
// (OwnedElsewhere), a failure the caller NAKs (Err), or none of the three - the zero result - for
// a session this listener answers for whose record says no way of receiving, which the caller
// holds and retries rather than NAKs (cmd/listener/unplaceable_delivery.go).
type HandleAgentResult struct {
	// Relayed reports a message the session's own consumer holds, which the relay of that
	// consumer delivers: the caller records nothing about it, neither an attempt nor a dedupe
	// key, since it delivered nothing. A dedupe key says the session has the message, and only
	// a delivery may say that.
	Relayed bool
	// OwnedElsewhere reports a session whose stored subscription names another listener. That
	// listener reads the same inbox message from its own durable and delivers it, so this one
	// acknowledges it with nothing to report: no delivery, no exception, no NAK.
	OwnedElsewhere bool
	// Err is a resolution the interest path made and failed (Resolve): the one outcome whose
	// caller NAKs the message for redelivery.
	Err error
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
		if err := Resolve(item, sessionID, entry, storedSeq); err != nil {
			return HandleAgentResult{Err: err}
		}
		return HandleAgentResult{Relayed: true}
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
	if err := Resolve(item, sessionID, entry, storedSeq); err != nil {
		// Resolve fails only with ErrNoSessionRecord. The zero result carries no error, so the
		// listener defers the message for its retry window (cmd/listener/unplaceable_delivery.go)
		// rather than NAK it.
		return HandleAgentResult{}
	}
	return HandleAgentResult{Relayed: true}
}
