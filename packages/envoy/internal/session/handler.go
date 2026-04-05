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
// to the file registry. Returns the result so the caller can decide ACK vs NAK.
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

	// Fallback: file registry lookup
	entry, err := deliverer.Find(sessionID)
	if err != nil || entry == nil {
		return HandleAgentResult{Delivered: false}
	}

	fallback := store.Interest{SessionID: sessionID, Dir: entry.Dir, MachineID: machineID}
	if err := deliverer.Deliver(item, fallback); err != nil {
		if errors.Is(err, ErrWrongMachine) {
			return HandleAgentResult{Delivered: false}
		}
		return HandleAgentResult{ShouldNAK: true, Err: err}
	}
	return HandleAgentResult{Delivered: true}
}
