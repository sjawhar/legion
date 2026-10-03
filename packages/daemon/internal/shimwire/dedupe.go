package shimwire

import (
	"slices"
	"sync"
)

// phase is how far one delivery has got. The daemon retries a delivery it has not seen
// confirmed, and what the shim owes the retry depends on this and nothing else
// (worker-shim.ts:223-231).
type phase uint8

const (
	// phaseSent: the prompt is with OMP and unanswered. A retry waits with it.
	phaseSent phase = iota
	// phaseAcked: OMP accepted the prompt. A retry gets that answer.
	phaseAcked
	// phaseStarted: a turn attributable to this delivery was observed. A retry gets the answer
	// and the start.
	phaseStarted
)

// delivery is the one logical task the shim is carrying, and every request id the daemon has
// used to ask for it.
type delivery struct {
	id         string
	requestIDs []string
	phase      phase
	// turnWasIdle records whether OMP was between turns when the prompt went out. Only then can
	// the next agent_start be attributed to this delivery: a turn already running belongs to
	// something else, and OMP does not carry the prompt id in agent_start (LEGION-144).
	turnWasIdle bool
}

// Dedupe is the shim's delivery dedupe: it answers a retried prompt itself instead of prompting
// OMP twice, and it makes OMP's one answer reach every request that asked for the same delivery
// (worker-shim.ts:184-195).
//
// Its zero value is ready. Unlike the single-threaded shim it is ported from, it is safe for the
// shim's two pumps — the daemon's stream and the wrapped process's stdout are read on separate
// goroutines here, and they share this one record.
type Dedupe struct {
	mu             sync.Mutex
	delivery       *delivery
	turnInProgress bool
}

// DaemonFrame takes a frame the daemon sent and answers two questions: whether OMP should see it,
// and what goes straight back to the daemon instead.
//
// Everything that is not a prompt carrying a deliveryId is forwarded untouched — including a
// prompt from a daemon that predates the dedupe (worker-shim.ts:347-351). The frames the shim
// answers itself, shutdown and adopt-working-copy, are handled before this and never reach it.
func (d *Dedupe) DaemonFrame(frame Frame) (forward bool, replies []Frame) {
	prompt, ok := frame.(Prompt)
	if !ok || prompt.ID == "" || prompt.DeliveryID == "" {
		return true, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.delivery == nil || d.delivery.id != prompt.DeliveryID {
		// A delivery the shim has not seen: record it and let OMP have it. The shim carries one
		// delivery, so a new one supersedes whatever it was carrying (worker-shim.ts:372-377).
		d.delivery = &delivery{
			id:          prompt.DeliveryID,
			requestIDs:  []string{prompt.ID},
			phase:       phaseSent,
			turnWasIdle: !d.turnInProgress,
		}
		return true, nil
	}

	// A retry of the delivery in hand: OMP is never prompted twice for it (worker-shim.ts:352-370).
	if !slices.Contains(d.delivery.requestIDs, prompt.ID) {
		d.delivery.requestIDs = append(d.delivery.requestIDs, prompt.ID)
	}
	if d.delivery.phase == phaseSent {
		// Nothing has come back yet. OMP's own answer will reach this request too, through the
		// fan-out in AgentFrame.
		return false, nil
	}
	replies = []Frame{Response{ID: prompt.ID, Command: TypePrompt, Success: true}}
	if d.delivery.phase == phaseStarted {
		// The replayed start carries the deliveryId, which a real OMP start does not: it is how
		// the daemon knows the turn it is being told about is the one it asked for
		// (worker-shim.ts:365).
		replies = append(replies, AgentStart{DeliveryID: prompt.DeliveryID})
		if !d.turnInProgress {
			// The turn is already over; replaying only its start would leave the daemon waiting
			// for an end that happened before it asked (worker-shim.ts:367).
			replies = append(replies, AgentEnd{})
		}
	}
	return false, replies
}

// AgentFrame takes a frame the wrapped process emitted, on its way to the daemon, and returns
// the frames to write after it — the answers owed to requests that shared its delivery. The
// frame itself is forwarded by the caller, unchanged (worker-shim.ts:249-292).
func (d *Dedupe) AgentFrame(frame Frame) []Frame {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch f := frame.(type) {
	case Response:
		return d.observeResponse(f)
	case RPCChunk:
		// A logical agent_end is the only worker frame that grows with the whole transcript and
		// therefore the one the encoder chunks; the replay must preserve its idle outcome
		// (worker-shim.ts:271-274).
		d.turnInProgress = false
	case AgentStart:
		if !d.turnInProgress && d.delivery != nil && d.delivery.turnWasIdle &&
			(d.delivery.phase == phaseSent || d.delivery.phase == phaseAcked) {
			d.delivery.phase = phaseStarted
		}
		d.turnInProgress = true
	case AgentEnd:
		d.turnInProgress = false
	}
	return nil
}

// observeResponse advances the delivery on OMP's own answer and fans that answer out to every
// other request id that asked for it (worker-shim.ts:254-270).
func (d *Dedupe) observeResponse(response Response) []Frame {
	if response.Command != TypePrompt || d.delivery == nil ||
		!slices.Contains(d.delivery.requestIDs, response.ID) {
		return nil
	}
	// Read before the refusal clears the record: the requests still waiting are owed the answer
	// either way.
	requestIDs := d.delivery.requestIDs
	if !response.Success {
		// A refused prompt is not a delivery; nothing will ever start for it.
		d.delivery = nil
	} else if d.delivery.phase == phaseSent {
		d.delivery.phase = phaseAcked
	}
	var fanout []Frame
	for _, requestID := range requestIDs {
		if requestID == response.ID {
			continue
		}
		duplicate := response
		duplicate.ID = requestID
		fanout = append(fanout, duplicate)
	}
	return fanout
}
