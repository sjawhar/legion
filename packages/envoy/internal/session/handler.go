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
	// Interest path: the stored subscription names this listener, which owns the session.
	if interest != nil && interest.MachineID == machineID {
		delivery, err := deliverer.DeliverWithResult(item, *interest)
		if errors.Is(err, ErrWrongMachine) {
			return HandleAgentResult{OwnedElsewhere: true}
		}
		if err != nil {
			return HandleAgentResult{ShouldNAK: true, Err: err}
		}
		return HandleAgentResult{Delivered: true, Skipped: delivery.Skipped}
	}
	// A stored subscription naming another listener makes the session that listener's, whatever
	// listener the session last registered through: the stored MachineID is the one its topics
	// are fanned out on (store.Registry.Match), so ownership reads the same on both paths.
	if interest != nil && interest.MachineID != "" {
		return HandleAgentResult{OwnedElsewhere: true}
	}

	// No stored subscription names an owner, so the session registry entry does: an interest
	// naming no machine leaves the decision to the entry's MachineID.
	delivery, err := deliverer.DeliverWithResult(item, store.Interest{SessionID: sessionID})
	if errors.Is(err, ErrWrongMachine) {
		return HandleAgentResult{OwnedElsewhere: true}
	}
	if err != nil {
		// Session not found or delivery failed without interest — don't NAK,
		// another listener may own this session.
		return HandleAgentResult{Delivered: false}
	}
	return HandleAgentResult{Delivered: true, Skipped: delivery.Skipped}
}
