package stream

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

var _ runtime.Conn = (*Conn)(nil)

var (
	// ErrClosed is the transport failing under a request: the connection closed before the
	// answer came, or a write to it failed. It is never an acknowledgement and never a refusal.
	ErrClosed = errors.New("worker stream closed")
	// ErrNoAnswer is a request whose answer did not arrive within the RPC timeout
	// (worker_rpc_timeout_seconds) — the bound the shipped client puts on every request
	// (worker-rpc.ts:407-421).
	ErrNoAnswer = errors.New("no answer on the worker stream within the RPC timeout")
)

// RefusedError is the far side answering a request with a refusal — OMP's `{success:false}`, or
// a shim's failed adoption. The connection is fine; the agent said no.
//
// A refused prompt is runtime.ErrPromptRefused, which is what the supervisor charges a prompt
// budget on. No other refusal is — a refused negotiation ahead of the prompt, a refused get_state
// or adoption — so the match is on the command rather than an unconditional Unwrap.
type RefusedError struct {
	Claim   claim.Token
	Command string
	Reason  string
}

func (e *RefusedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("worker-stream: %s refused %s", e.Claim, e.Command)
	}
	return fmt.Sprintf("worker-stream: %s refused %s: %s", e.Claim, e.Command, e.Reason)
}

func (e *RefusedError) Is(target error) bool {
	return target == runtime.ErrPromptRefused && e.Command == shimwire.TypePrompt
}

// The RPC protocol version the daemon speaks: v2 is the one with rpc_chunk, without which OMP
// writes a whole transcript's agent_end as one line no reader here accepts (worker-rpc.ts:430).
const protocolVersion = 2

// Conn is one claim's live connection: requests out, answers and turn facts back. It satisfies
// runtime.Conn, which is how everything above the stream holds it.
//
// Every request OMP answers is preceded, once per connection, by the protocol negotiation, so a
// holder of a runtime.Conn never has to know the protocol has versions. Requests are bounded by
// the caller's context and by the RPC timeout, whichever ends first.
type Conn struct {
	claim      claim.Token
	writer     *shimwire.Writer
	rpcTimeout time.Duration
	log        *slog.Logger
	events     *eventQueue

	mu      sync.Mutex
	pending map[string]pendingRequest
	// acked is the prompt OMP last answered `{success:true}` on this connection, until its turn
	// ends or another prompt supersedes it: the only prompt a later `{success:false}` can take
	// back (worker-rpc.ts:359-368). It is set by the reader when the answer is read, so it
	// follows wire order rather than the order the waiting goroutine wakes in.
	acked  *ackedPrompt
	closed bool
	done   chan struct{}

	// negotiation is a one-slot semaphore rather than a mutex so a request queued behind another
	// goroutine's negotiation still answers to its own context.
	negotiation chan struct{}
	negotiated  bool
}

type pendingRequest struct {
	answer chan shimwire.Frame
	// deliveryID is set for a prompt, and names the delivery a late refusal would take back.
	deliveryID string
}

type ackedPrompt struct {
	requestID  string
	deliveryID string
}

func newConn(nc net.Conn, token claim.Token, rpcTimeout time.Duration, log *slog.Logger, events *eventQueue) *Conn {
	return &Conn{
		claim:       token,
		writer:      shimwire.NewWriter(connWriter{nc: nc, timeout: rpcTimeout}),
		rpcTimeout:  rpcTimeout,
		log:         log,
		events:      events,
		pending:     map[string]pendingRequest{},
		done:        make(chan struct{}),
		negotiation: make(chan struct{}, 1),
	}
}

// Negotiate opens protocol v2 on this connection. It is sent once: a connection already
// negotiated answers at once, and one whose negotiation failed tries again on the next call.
func (c *Conn) Negotiate(ctx context.Context) error {
	select {
	case c.negotiation <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("worker-stream: %s %s: %w", c.claim, shimwire.TypeNegotiateProtocol, ctx.Err())
	}
	defer func() { <-c.negotiation }()
	if c.negotiated {
		return nil
	}
	id := rand.Text()
	if _, err := c.call(ctx, shimwire.NegotiateProtocol{ID: id, ProtocolVersion: protocolVersion}, id, ""); err != nil {
		return err
	}
	c.negotiated = true
	return nil
}

// Prompt hands OMP a task and returns on its acknowledgement — which is not delivery: OMP
// answers `{success:true}` before the turn starts, and the turn is observed on Events(). OMP
// refusing the prompt is a *RefusedError that is runtime.ErrPromptRefused; every other failure —
// ErrClosed, ErrNoAnswer, the context, a refused negotiation — is transport and is not.
func (c *Conn) Prompt(ctx context.Context, deliveryID, message string) error {
	if deliveryID == "" {
		return fmt.Errorf("worker-stream: %s: a prompt needs a delivery id, the one thing the shim dedupes a retry on", c.claim)
	}
	if err := c.Negotiate(ctx); err != nil {
		return err
	}
	id := rand.Text()
	_, err := c.call(ctx, shimwire.Prompt{ID: id, DeliveryID: deliveryID, Message: message}, id, deliveryID)
	return err
}

// GetState asks OMP whether a turn is running. An answer that does not say is an error, never
// "not streaming": a restart decides between confirming a delivery and re-sending it on this.
func (c *Conn) GetState(ctx context.Context) (runtime.ConnState, error) {
	if err := c.Negotiate(ctx); err != nil {
		return runtime.ConnState{}, err
	}
	id := rand.Text()
	response, err := c.call(ctx, shimwire.GetState{ID: id}, id, "")
	if err != nil {
		return runtime.ConnState{}, err
	}
	var data shimwire.StateData
	if len(response.Data) > 0 {
		if err := json.Unmarshal(response.Data, &data); err != nil {
			return runtime.ConnState{}, fmt.Errorf("worker-stream: %s get_state answer: %w", c.claim, err)
		}
	}
	if data.IsStreaming == nil {
		return runtime.ConnState{}, fmt.Errorf("worker-stream: %s get_state answer carries no data.isStreaming", c.claim)
	}
	return runtime.ConnState{IsStreaming: *data.IsStreaming}, nil
}

// Shutdown asks the shim to end OMP: SIGTERM, then SIGKILL once the shim's grace runs out. The
// shim acts on the frame itself and answers nothing (internal/shim); whether the process ended is
// the runtime's to observe.
func (c *Conn) Shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return fmt.Errorf("worker-stream: %s %s: %w", c.claim, shimwire.TypeShutdown, ErrClosed)
	}
	return c.write(shimwire.Shutdown{})
}

// AdoptWorkingCopy asks the shim to set the author of its workspace's working copy. The shim
// runs the command within timeout; the answer is awaited for that plus the RPC timeout, the
// round trip (worker-rpc.ts:492-505).
func (c *Conn) AdoptWorkingCopy(ctx context.Context, identity runtime.GitIdentity, timeout time.Duration) error {
	id := rand.Text()
	frame := shimwire.AdoptWorkingCopy{ID: id, JJUser: identity.Name, JJEmail: identity.Email, TimeoutMs: int(timeout.Milliseconds())}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("worker-stream: %s: %w", c.claim, err)
	}
	answer, err := c.request(ctx, frame, id, "", timeout+c.rpcTimeout)
	if err != nil {
		return err
	}
	switch answer := answer.(type) {
	case shimwire.AdoptWorkingCopyResult:
		if answer.OK {
			return nil
		}
		return &RefusedError{Claim: c.claim, Command: shimwire.TypeAdoptWorkingCopy, Reason: answer.Error}
	case shimwire.Response:
		// A shim that does not answer the frame itself forwards it, and OMP refuses a command
		// it does not know.
		if !answer.Success {
			return &RefusedError{Claim: c.claim, Command: shimwire.TypeAdoptWorkingCopy, Reason: answer.Error}
		}
	}
	return fmt.Errorf("worker-stream: %s %s answered by a %s frame", c.claim, shimwire.TypeAdoptWorkingCopy, answer.FrameType())
}

// call is a request OMP answers with a response to the same command, successful.
func (c *Conn) call(ctx context.Context, frame shimwire.Frame, id, deliveryID string) (shimwire.Response, error) {
	answer, err := c.request(ctx, frame, id, deliveryID, c.rpcTimeout)
	if err != nil {
		return shimwire.Response{}, err
	}
	command := frame.FrameType()
	response, ok := answer.(shimwire.Response)
	if !ok {
		return shimwire.Response{}, fmt.Errorf("worker-stream: %s %s answered by a %s frame", c.claim, command, answer.FrameType())
	}
	if response.Command != command {
		return shimwire.Response{}, fmt.Errorf("worker-stream: %s %s answered by a %s response", c.claim, command, response.Command)
	}
	if !response.Success {
		return shimwire.Response{}, &RefusedError{Claim: c.claim, Command: command, Reason: response.Error}
	}
	return response, nil
}

// request writes frame and waits for the frame that answers id.
func (c *Conn) request(ctx context.Context, frame shimwire.Frame, id, deliveryID string, bound time.Duration) (shimwire.Frame, error) {
	answer := make(chan shimwire.Frame, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("worker-stream: %s %s: %w", c.claim, frame.FrameType(), ErrClosed)
	}
	c.pending[id] = pendingRequest{answer: answer, deliveryID: deliveryID}
	if deliveryID != "" {
		// A new prompt supersedes the last one's claim to a late refusal (worker-rpc.ts:450).
		c.acked = nil
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(frame); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, bound, ErrNoAnswer)
	defer cancel()
	select {
	case got := <-answer:
		return got, nil
	case <-c.done:
		// The reader hands an answer over before it ends, so one that raced the close is
		// still the answer.
		select {
		case got := <-answer:
			return got, nil
		default:
		}
		return nil, fmt.Errorf("worker-stream: %s %s: %w before it was answered", c.claim, frame.FrameType(), ErrClosed)
	case <-ctx.Done():
		return nil, fmt.Errorf("worker-stream: %s %s: %w", c.claim, frame.FrameType(), context.Cause(ctx))
	}
}

func (c *Conn) write(frame shimwire.Frame) error {
	if err := c.writer.WriteFrame(frame); err != nil {
		return fmt.Errorf("worker-stream: %s %s: %w", c.claim, frame.FrameType(), err)
	}
	return nil
}

// read is the connection's reader: every line from the shim, rpc_chunk sequences reassembled,
// dispatched in wire order until the stream ends.
func (c *Conn) read(src io.Reader) {
	reader := shimwire.NewReader(src)
	var chunks shimwire.Reassembler
	for {
		line, err := reader.ReadLine()
		if err != nil {
			if errors.Is(err, shimwire.ErrLineTooLong) {
				c.log.Warn("worker-stream: closing a stream that sent a line over the frame limit", "claim", c.claim, "error", err)
			}
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			c.logNotAFrame(line, err)
			continue
		}
		if chunk, ok := frame.(shimwire.RPCChunk); ok {
			whole, err := chunks.Push(chunk)
			if err != nil {
				c.log.Warn("worker-stream: dropped an rpc_chunk sequence", "claim", c.claim, "error", err)
				continue
			}
			if whole == nil {
				continue
			}
			if frame, err = shimwire.Decode(whole); err != nil {
				c.logNotAFrame(whole, err)
				continue
			}
		} else if err := chunks.Interrupt(); err != nil {
			c.log.Warn("worker-stream: dropped an rpc_chunk sequence", "claim", c.claim, "error", err)
		}
		c.dispatch(frame)
	}
}

// logNotAFrame names a line the reader could not use without quoting it: a line that is not
// JSON at all could be anything the wrapped process printed.
func (c *Conn) logNotAFrame(line []byte, err error) {
	if errors.Is(err, shimwire.ErrNotAFrame) {
		c.log.Warn("worker-stream: ignoring a line that is not a JSON object", "claim", c.claim, "bytes", len(line))
		return
	}
	c.log.Warn("worker-stream: ignoring a malformed frame", "claim", c.claim, "error", err)
}

// dispatch acts on one whole frame. OMP's other events — tool calls, message updates, errors —
// are not facts the daemon supervises on, and are dropped here.
func (c *Conn) dispatch(frame shimwire.Frame) {
	switch frame := frame.(type) {
	case shimwire.AgentStart:
		c.events.push(TurnStart{Claim: c.claim, DeliveryID: frame.DeliveryID})
	case shimwire.AgentEnd:
		c.mu.Lock()
		c.acked = nil
		c.mu.Unlock()
		c.events.push(TurnEnd{Claim: c.claim})
	case shimwire.Response:
		c.answer(frame)
	case shimwire.AdoptWorkingCopyResult:
		c.mu.Lock()
		request, waiting := c.pending[frame.ID]
		delete(c.pending, frame.ID)
		c.mu.Unlock()
		if waiting {
			request.answer <- frame
		}
	}
}

// answer routes a response to the request waiting on its id. One nobody is waiting on is either
// a success for a request that gave up — the shim fans OMP's one answer out to every request id
// of a retried delivery (worker-shim.ts:254-270), and a retry's earlier ids are long abandoned —
// or a refusal, which reverses the acknowledged prompt it names or, naming none, is logged.
func (c *Conn) answer(response shimwire.Response) {
	c.mu.Lock()
	request, waiting := c.pending[response.ID]
	var late *ackedPrompt
	switch {
	case waiting:
		delete(c.pending, response.ID)
		if request.deliveryID != "" && response.Command == shimwire.TypePrompt && response.Success {
			c.acked = &ackedPrompt{requestID: response.ID, deliveryID: request.deliveryID}
		}
	case response.Command == shimwire.TypePrompt && !response.Success && c.acked != nil && c.acked.requestID == response.ID:
		late, c.acked = c.acked, nil
	}
	c.mu.Unlock()

	switch {
	case waiting:
		request.answer <- response
	case late != nil:
		c.log.Warn("worker-stream: prompt refused after its acknowledgement",
			"claim", c.claim, "deliveryId", late.deliveryID, "error", response.Error)
		c.events.push(LateRefusal{Claim: c.claim, DeliveryID: late.deliveryID, Error: response.Error})
	case response.Command == shimwire.TypePrompt && !response.Success:
		c.log.Warn("worker-stream: refusal for a prompt request this connection is not waiting on",
			"claim", c.claim, "request", response.ID, "error", response.Error)
	}
}

// finish marks the connection ended once its reader has returned: every request still waiting
// is released with ErrClosed, and every later one fails at once.
func (c *Conn) finish() {
	c.mu.Lock()
	c.closed = true
	c.acked = nil
	c.mu.Unlock()
	close(c.done)
}

// connWriter is the connection as the frame writer sees it. Every write is bounded by the RPC
// timeout — a shim that stops reading must not hold a request forever — and a write that fails
// closes the connection: a line written in part leaves the peer mid-frame, and nothing after it
// could be read as the frame it is. The frame writer makes exactly one Write per frame under its
// own lock, so the deadline set here is that frame's alone.
type connWriter struct {
	nc      net.Conn
	timeout time.Duration
}

func (w connWriter) Write(p []byte) (int, error) {
	if err := w.nc.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrClosed, err)
	}
	n, err := w.nc.Write(p)
	if err != nil {
		w.nc.Close()
		return n, fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return n, nil
}
