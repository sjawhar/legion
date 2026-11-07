package stream

import (
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Event is one agent fact the stream observed. The set is sealed: a connection arriving
// (Hello), OMP starting and ending a turn (TurnStart, TurnEnd), one tool call of a turn
// starting or ending (ToolStep), OMP saying how one prompt's work ended (PromptResult), OMP
// taking back a prompt it had acknowledged (LateRefusal), and the connection going away
// (Closed). The acknowledgement of a prompt is not one of them — it is Conn.Prompt's return, so
// the one goroutine that sent the prompt is the one that learns it was taken (B4).
type Event interface{ isEvent() }

// AgentSecretsIdentity is the pod's session identity a hello2 carried (shimwire.AgentSecretsHello):
// the thumbprint of the key the pod generated and its projected token for the secrets broker's
// audience. Nil for a pane, or a pod whose runtime does not enroll.
type AgentSecretsIdentity struct {
	Thumbprint, PodToken string
}

// Hello is a shim's hello accepted: the claim's connection is registered, at the generation its
// boot token was minted for, with the identity the hello carried, if any. Under always-dial the
// shim says hello before it spawns Oh My Pi, so a Hello says the pane's bridge is up, not that the
// agent is.
type Hello struct {
	Claim        claim.Token
	Generation   uint64
	AgentSecrets *AgentSecretsIdentity
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

// ToolStep is one tool call of the agent's turn beginning or ending
// (shimwire.ToolExecutionStart, shimwire.ToolExecutionEnd): the only fact on the wire that says
// an agent mid-turn is still moving. The connection computes nothing from it — the supervisor
// stamps it, measures it and decides what it means — because this is the one event that arrives
// per tool call rather than per turn.
//
// ArgsBytes is the length of the start's arguments as Oh My Pi wrote them and ArgsHash their
// fingerprint (stepArgsHash), both measured here and both zero for an end, which carries no
// arguments. The arguments' own text goes no further than the reader that parsed the frame: it
// routinely holds a credential, and this event waits in two unbounded in-memory queues (the
// queue below and the supervisor's) until a machine handles it. End says which of the two frames
// this is. CallID is what pairs an end with its start, and the only thing that does; the Claude
// Code bridge writes a start with none (internal/claudebridge), which is a step with no call to
// name.
type ToolStep struct {
	Claim     claim.Token
	Tool      string
	CallID    string
	ArgsBytes int
	ArgsHash  string
	End       bool
	// ConnSequence is the registration sequence of the connection the frame arrived on
	// (Conn.Sequence), as LateRefusal carries it: the supervisor compares it with the claim's
	// live connection, and a frame of a connection that has since been replaced says what the
	// agent was doing, not what it is doing now.
	ConnSequence uint64
}

// PromptResult is OMP's prompt_result: the work of one prompt is over. It is the only frame that
// names the prompt a turn belonged to, since agent_end carries no prompt identity
// (omp://rpc.md), and it arrives right after the agent_end of the turn the prompt ran. A turn
// another wake started — a background job's result, with background work on for every role — ends
// with no prompt_result behind it, which is how the supervisor tells that turn from the delivery's
// own.
//
// DeliveryID is the delivery the prompt's request id names (promptDelivery), so a result read on
// a connection that did not send the prompt still names its delivery. AgentInvoked is false for a
// prompt that ran no turn at all. Error is OMP's reason when the work ended in one: the message
// of the frame's error object (shimwire.PromptError), which is all of the failure the daemon
// records.
type PromptResult struct {
	Claim        claim.Token
	DeliveryID   string
	AgentInvoked bool
	Error        string
}

// LateRefusal is OMP answering `{success:false}` for a prompt it had already answered
// `{success:true}` — "Agent is already processing…" once it finds a turn it did not start, and
// whatever a prompt that failed before any turn began says
// (worker-rpc.ts:359-368). DeliveryID is the delivery that prompt carried, so a delivery the
// supervisor counted as started by a turn that was never its own can be taken back. A refusal OMP
// gave while no daemon was connected reaches the next connection from the shim's backlog; the
// prompt's request id names its delivery, so it is a LateRefusal there too. Error is OMP's reason,
// verbatim.
type LateRefusal struct {
	Claim      claim.Token
	DeliveryID string
	Error      string
	// Replayed is a refusal for a prompt this connection did not send, read from the shim's
	// backlog: it answers a prompt an earlier connection sent.
	Replayed bool
	// ConnSequence is the registration sequence of the connection the refusal arrived on
	// (Conn.Sequence): the supervisor compares it with the newest connection it sent the delivery
	// through.
	ConnSequence uint64
}

// Closed is a registered connection gone, whichever side ended it. It is emitted exactly once
// per Hello, after every other event of that connection and before the Hello of any connection
// that replaces it.
type Closed struct {
	Claim claim.Token
}

func (Hello) isEvent()        {}
func (TurnStart) isEvent()    {}
func (TurnEnd) isEvent()      {}
func (ToolStep) isEvent()     {}
func (PromptResult) isEvent() {}
func (LateRefusal) isEvent()  {}
func (Closed) isEvent()       {}

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
