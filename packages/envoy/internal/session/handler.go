package session

import (
	"errors"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// HandleAgentResult captures the outcome of agent message handling.
type HandleAgentResult struct {
	Delivered bool
	Skipped   bool
	// Push is set by ResolveAgentMessage for a serving-port session this listener owns, with its
	// entry: the caller pushes the message to the session's port (Deliverer.Push). HandleAgentMessage
	// makes that push itself, and never sets it.
	Push  bool
	Entry SessionEntry
	// OwnedElsewhere reports a session another listener owns (see ErrWrongMachine). That listener
	// reads the same inbox message from its own durable and delivers it, so this one acknowledges
	// it with nothing to report: no delivery, no exception, no NAK.
	OwnedElsewhere bool
	ShouldNAK      bool
	Err            error
}

// HandleAgentMessage handles delivery of an agent-to-agent message.
// It tries the interest path first (KV subscription match), then falls back
// to a direct session registry lookup. Returns the result so the caller can
// decide ACK vs NAK.
func HandleAgentMessage(
	item contracts.Envelope,
	sessionID string,
	machineID string,
	interest *store.Interest,
	deliverer *Deliverer,
) HandleAgentResult {
	return handleAgentMessage(item, sessionID, machineID, interest, deliverer.DeliverWithResult)
}

// ResolveAgentMessage is HandleAgentMessage without the push to a serving port: a session it would
// push to comes back with Push set and its entry, for the caller to push to.
func ResolveAgentMessage(
	item contracts.Envelope,
	sessionID string,
	machineID string,
	interest *store.Interest,
	deliverer *Deliverer,
) HandleAgentResult {
	return handleAgentMessage(item, sessionID, machineID, interest, deliverer.ResolveWithResult)
}

func handleAgentMessage(
	item contracts.Envelope,
	sessionID string,
	machineID string,
	interest *store.Interest,
	deliver func(contracts.Envelope, store.Interest) (DeliveryResult, error),
) HandleAgentResult {
	// Interest path: the stored subscription names this listener, which owns the session.
	if interest != nil && interest.MachineID == machineID {
		delivery, err := deliver(item, *interest)
		if errors.Is(err, ErrWrongMachine) {
			return HandleAgentResult{OwnedElsewhere: true}
		}
		if err != nil {
			return HandleAgentResult{ShouldNAK: true, Err: err}
		}
		return delivered(delivery)
	}
	// A stored subscription naming another listener makes the session that listener's, whatever
	// listener the session last registered through: the stored MachineID is the one its topics
	// are fanned out on (store.Registry.Match), so ownership reads the same on both paths.
	if interest != nil && interest.MachineID != "" {
		return HandleAgentResult{OwnedElsewhere: true}
	}

	// No stored subscription names an owner, so the session registry entry does: an interest
	// naming no machine leaves the decision to the entry's MachineID.
	delivery, err := deliver(item, store.Interest{SessionID: sessionID})
	if errors.Is(err, ErrWrongMachine) {
		return HandleAgentResult{OwnedElsewhere: true}
	}
	if err != nil {
		// Session not found or delivery failed without interest — don't NAK,
		// another listener may own this session.
		return HandleAgentResult{Delivered: false}
	}
	return delivered(delivery)
}

// delivered is the result of a delivery that succeeded, or that Resolve left a push to the caller.
func delivered(delivery DeliveryResult) HandleAgentResult {
	if delivery.Push {
		return HandleAgentResult{Push: true, Entry: delivery.Entry}
	}
	return HandleAgentResult{Delivered: true, Skipped: delivery.Skipped}
}
