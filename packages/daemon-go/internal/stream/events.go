package stream

import (
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Event is one agent fact the stream observed. The set is sealed: a connection arriving
// (Hello), OMP starting and ending a turn (TurnStart, TurnEnd), OMP taking back a prompt it
// had acknowledged (LateRefusal), and the connection going away (Closed). The acknowledgement
// of a prompt is not one of them — it is Conn.Prompt's return, so the one goroutine that sent the
// prompt is the one that learns it was taken (B4).
type Event interface{ isEvent() }

// Hello is a shim's hello accepted: the claim's connection is registered, at the generation its
// boot token was minted for. Under always-dial the shim says hello before it spawns Oh My Pi, so
// a Hello says the pane's bridge is up, not that the agent is.
type Hello struct {
	Claim      claim.Token
	Generation uint64
}

// TurnStart is OMP's agent_start. DeliveryID is set only when the frame carried one — the shim's
// replay of a start it already observed for that delivery (worker-shim.ts:365); OMP's own start
// never names the prompt that caused it, and matching it to one is the supervisor's rule.
type TurnStart struct {
	Claim      claim.Token
	DeliveryID string
}

// TurnEnd is OMP's agent_end: the turn is over.
type TurnEnd struct {
	Claim claim.Token
}

// LateRefusal is OMP answering `{success:false}` for a prompt it had already answered
// `{success:true}` — "Agent is busy" once it finds a turn it did not start
// (worker-rpc.ts:359-368). DeliveryID is the delivery that prompt carried, so a delivery the
// supervisor counted as started by a turn that was never its own can be taken back. Error is
// OMP's reason, verbatim.
type LateRefusal struct {
	Claim      claim.Token
	DeliveryID string
	Error      string
}

// Closed is a registered connection gone, whichever side ended it. It is emitted exactly once
// per Hello, after every other event of that connection and before the Hello of any connection
// that replaces it.
type Closed struct {
	Claim claim.Token
}

func (Hello) isEvent()       {}
func (TurnStart) isEvent()   {}
func (TurnEnd) isEvent()     {}
func (LateRefusal) isEvent() {}
func (Closed) isEvent()      {}

// eventQueue is the ordered, unbounded hand-off between every connection's reader and the one
// channel Events() returns.
//
// Unbounded because a connection's reader is also the goroutine that answers its requests: a
// reader parked on a full channel would leave a GetState answer unread behind it, and a consumer
// that asked for that state before draining the channel would wait on itself. The shipped
// listener has the same property by construction — its callbacks never wait on a consumer. What
// grows it is one small struct per turn boundary, not per frame.
type eventQueue struct {
	mu     sync.Mutex
	items  []Event
	wake   chan struct{}
	closed bool
	out    chan Event
}

func newEventQueue() *eventQueue {
	q := &eventQueue{wake: make(chan struct{}, 1), out: make(chan Event)}
	go q.forward()
	return q
}

func (q *eventQueue) push(event Event) {
	q.mu.Lock()
	q.items = append(q.items, event)
	q.mu.Unlock()
	q.signal()
}

// close ends the queue: every event already pushed is still delivered, then the channel closes.
// Nothing may push after it; the listener calls it once every connection's reader has returned.
func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) forward() {
	defer close(q.out)
	for range q.wake {
		q.mu.Lock()
		items, closed := q.items, q.closed
		q.items = nil
		q.mu.Unlock()
		for _, event := range items {
			q.out <- event
		}
		if closed {
			return
		}
	}
}
