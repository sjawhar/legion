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
	// self-subscribed skip (the session reads the message off its own subscription), and the
	// ErrWrongMachine answer both paths keep for a session another listener owns - which no
	// production deliverer produces: the one producer of ErrWrongMachine is a test transport
	// (TestE2E_AgentToAgent_RemoteMachine, internal/integration/e2e_test.go), so in a deployed
	// listener an undelivered, error-free result never carries this reason.
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
	// ReasonPushFailed is a record that named a serve port the push did not land on: an
	// unreachable peer, a session process restarting, a connection refused, or a server that
	// answered with anything but 204. The attempt failed and cannot tell whether the session
	// took the prompt, so a caller may ask again only at the cost of repeating it; the
	// listener's delivery retries it once (pushFailedDeferrals, cmd/listener/delivery.go).
	ReasonPushFailed
)

// reasonFor maps the delivery sentinels onto the reason the caller acts on. Every other error -
// a refused push, an unreachable machine - is ReasonPushFailed: it is a failure of this attempt,
// not a statement about the session's record, and a later attempt may well land. ErrWrongMachine
// never reaches here: both call sites answer it before they map, as defence only, since nothing
// outside a test returns it (see ReasonNone).
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
	// the caller to act on: the registry path's undelivered result, which the listener's delivery
	// retries or gives up on by this field alone. The NAKing interest path sets it too, but its
	// result carries Err and ShouldNAK, and the listener answers that result by those before it
	// ever reads Reason (cmd/listener/delivery.go, agentSubjectDelivery), so there it is read by
	// nothing.
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
