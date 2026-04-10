package session

import (
	"errors"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// HandleAgentResult captures the outcome of agent message handling.
type HandleAgentResult struct {
	Delivered bool
	ShouldNAK bool
	Err       error
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
	// Interest path: direct delivery if interest matches this machine
	if interest != nil && interest.MachineID == machineID {
		if err := deliverer.Deliver(item, *interest); err != nil {
			if errors.Is(err, ErrWrongMachine) {
				return HandleAgentResult{Delivered: false}
			}
			return HandleAgentResult{ShouldNAK: true, Err: err}
		}
		return HandleAgentResult{Delivered: true}
	}

	// No interest on this machine — try delivery via session registry directly.
	// The session registry (KV) resolves the port/machine.
	synth := store.Interest{SessionID: sessionID, MachineID: machineID}
	if err := deliverer.Deliver(item, synth); err != nil {
		if errors.Is(err, ErrWrongMachine) {
			return HandleAgentResult{Delivered: false}
		}
		// Session not found or delivery failed without interest — don't NAK,
		// another listener may own this session.
		return HandleAgentResult{Delivered: false}
	}
	return HandleAgentResult{Delivered: true}
}
