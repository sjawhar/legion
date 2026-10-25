package session

import (
	"errors"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// HandleAgentReason names why a delivery attempt did not reach the session, for the one question
// the caller cannot answer from Delivered alone: whether waiting could change the answer. The
// three causes reach the caller as one undelivered result otherwise, and it would have to
// re-derive them from the error text to tell them apart.
type HandleAgentReason uint8

const (
	// ReasonNone is every outcome that is none of the three causes below: a push that landed, a
	// self-subscribed skip (the session reads the message off its own subscription), and a
	// session another listener owns (ErrWrongMachine, which its callers answer before the
	// mapping below is reached).
	ReasonNone HandleAgentReason = iota
	// ReasonNoSessionRecord is a session the resolution found no record of. A caller holding a
	// record source that trails the store - or one whose read did not answer - may ask again.
	ReasonNoSessionRecord
	// ReasonNoPushTarget is a session whose record exists and names nothing to push to: it
	// registered before its server bound a serve port. That record is rewritten when the port
	// is bound, so asking again is not asking the same question - a caller may wait the port
	// out, and the listener's delivery does, deferring such a message for its whole retry
	// window before it gives up (cmd/listener/delivery.go).
	ReasonNoPushTarget
	// ReasonPushFailed is a record that named a serve port the push did not reach: an
	// unreachable peer, a session process restarting, a connection refused. The attempt failed
	// and learned nothing about the session, which is an unverified absence like the two above,
	// so a caller may ask again and the listener's delivery retries it in the same window.
	ReasonPushFailed
)

// reasonFor maps the delivery sentinels onto the reason the caller acts on. Every other error -
// a refused push, an unreachable machine - is ReasonPushFailed: it is a failure of this attempt,
// not a statement about the session's record, and a later attempt may well land. ErrWrongMachine
// never reaches here: both call sites answer it before they map, since a session another listener
// owns is not this one's to retry.
func reasonFor(err error) HandleAgentReason {
	switch {
	case errors.Is(err, ErrNoSessionRecord):
		return ReasonNoSessionRecord
	case errors.Is(err, ErrNoPushTarget):
		return ReasonNoPushTarget
	default:
		return ReasonPushFailed
	}
}

// HandleAgentResult captures the outcome of agent message handling.
type HandleAgentResult struct {
	Delivered bool
	Skipped   bool
	ShouldNAK bool
	Err       error
	// Reason tells apart the three causes of an undelivered message that carries no error for
	// the caller to act on. It is set on every outcome a failed delivery produced - the two
	// sentinels and the push that failed on the wire - including the NAKing interest path, so
	// the caller reads one field rather than branching on which path produced the result.
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
	// Interest path: direct delivery if interest matches this machine
	if interest != nil && interest.MachineID == machineID {
		delivery, err := deliverer.DeliverWithResultForEntry(item, *interest, entry)
		if err != nil {
			if errors.Is(err, ErrWrongMachine) {
				return HandleAgentResult{Delivered: false}
			}
			return HandleAgentResult{ShouldNAK: true, Err: err, Reason: reasonFor(err)}
		}
		return HandleAgentResult{Delivered: true, Skipped: delivery.Skipped}
	}

	// No interest on this machine — deliver through the resolved session entry directly, which
	// carries the port/machine the registry holds.
	synth := store.Interest{SessionID: sessionID, MachineID: machineID}
	delivery, err := deliverer.DeliverWithResultForEntry(item, synth, entry)
	if err != nil {
		if errors.Is(err, ErrWrongMachine) {
			return HandleAgentResult{Delivered: false}
		}
		// Session not found or delivery failed without interest — don't NAK,
		// another listener may own this session.
		return HandleAgentResult{Delivered: false, Reason: reasonFor(err)}
	}
	return HandleAgentResult{Delivered: true, Skipped: delivery.Skipped}
}
