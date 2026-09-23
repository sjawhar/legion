package stream

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// agentEndFrame is the frame that arrives chunked: the only worker frame whose size grows with
// the whole transcript (worker-rpc.ts:65-68).
func agentEndFrame(t *testing.T, size int) []byte {
	t.Helper()
	head := `{"type":"agent_end","messages":[{"role":"user","content":"`
	tail := `"}]}`
	return []byte(head + strings.Repeat("x", size-len(head)-len(tail)) + tail)
}

func (p *peer) sendChunked(chunkID string, frame []byte) {
	p.t.Helper()
	chunks, err := shimwire.SplitChunks(chunkID, frame)
	if err != nil {
		p.t.Fatalf("SplitChunks: %v", err)
	}
	for _, chunk := range chunks {
		p.send(chunk)
	}
}

// prompt runs Prompt against the peer through the negotiation it sends first, and returns the
// prompt frame the peer received and the channel Prompt's return lands on.
func (h *harness) prompt(p *peer, deliveryID, message string) (shimwire.Prompt, <-chan error) {
	h.t.Helper()
	conn := h.conn()
	result := async(func() error { return conn.Prompt(context.Background(), deliveryID, message) })
	p.negotiate()
	return p.expect(shimwire.TypePrompt).(shimwire.Prompt), result
}

// The ack is Prompt's return: `{success:true}` returns nil, and the frame on the wire carries the
// delivery id, the message, and a request id of its own (worker-rpc.ts:452).
func TestPromptReturnsNilOnTheAck(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	frame, result := h.prompt(p, "delivery-1", "verify <#41> & report")
	if frame.DeliveryID != "delivery-1" || frame.Message != "verify <#41> & report" || frame.ID == "" {
		t.Fatalf("prompt frame = %#v", frame)
	}
	select {
	case err := <-result:
		t.Fatalf("Prompt returned before the ack: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("Prompt = %v, want nil on the ack", err)
	}
}

// `{success:false}` is a refusal: the connection is fine and the agent said no.
func TestPromptReturnsATypedRefusal(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	frame, result := h.prompt(p, "delivery-1", "verify #41")
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: "Agent is busy"})
	err := awaitResult(t, result)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Command != shimwire.TypePrompt || refused.Reason != "Agent is busy" {
		t.Fatalf("Prompt = %v, want a RefusedError for prompt carrying %q", err, "Agent is busy")
	}
	if isClosed(err) {
		t.Fatal("a refusal reads as a transport failure")
	}
}

// The connection closing before OMP answers is a transport error, never an ack and never a
// refusal.
func TestPromptReturnsATransportErrorWhenTheConnectionClosesFirst(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	_, result := h.prompt(p, "delivery-1", "verify #41")
	p.conn.Close()
	err := awaitResult(t, result)
	var refused *RefusedError
	if !isClosed(err) || errors.As(err, &refused) {
		t.Fatalf("Prompt = %v, want ErrClosed", err)
	}
	// And after the close, at once, with nothing written.
	if event := h.next(); event != (Closed{Claim: testClaim}) {
		t.Fatalf("event = %#v, want Closed", event)
	}
	if err := conn.Prompt(context.Background(), "delivery-2", "again"); !isClosed(err) {
		t.Fatalf("Prompt on a closed connection = %v, want ErrClosed", err)
	}
}

func TestPromptHonoursItsContextAndTheRPCTimeout(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	ctx, cancel := context.WithCancel(context.Background())
	result := async(func() error { return conn.Prompt(ctx, "delivery-1", "verify #41") })
	p.negotiate()
	p.expect(shimwire.TypePrompt)
	cancel()
	if err := awaitResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt after its context was cancelled = %v, want context.Canceled", err)
	}

	short := startListener(t, harnessOptions{rpcTimeout: 50 * time.Millisecond})
	q := dial(t, short.listener.Addr())
	q.hello(testToken)
	q.expect(shimwire.TypeHelloAck)
	short.next()
	quiet := short.conn()
	err := awaitResult(t, async(func() error { return quiet.Prompt(context.Background(), "delivery-1", "verify #41") }))
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("Prompt with no answer = %v, want ErrNoAnswer", err)
	}
}

// The supervisor charges a prompt the agent refused and re-sends one the transport lost at no
// charge, telling them apart by runtime.ErrPromptRefused alone (LEGION-60). So only OMP's
// `{success:false}` for the prompt itself is that error; a refused negotiation, a refused
// get_state, a close, the RPC timeout, and the caller's own deadline are not.
func TestOnlyAPromptRefusalIsErrPromptRefused(t *testing.T) {
	var refusal *RefusedError
	for _, test := range []struct {
		name       string
		rpcTimeout time.Duration
		// drive answers (or does not) whatever the connection sent, and returns the error the
		// call under test returned.
		drive func(h *harness, p *peer) error
		// isCase is the failure the case exists to produce, so no row passes by producing
		// another.
		isCase  func(error) bool
		refused bool
	}{
		{name: "OMP refuses the prompt", refused: true,
			drive: func(h *harness, p *peer) error {
				frame, result := h.prompt(p, "delivery-1", "verify #41")
				p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: "Agent is busy"})
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.As(err, &refusal) && refusal.Command == shimwire.TypePrompt },
		},
		{name: "the connection closes before the ack",
			drive: func(h *harness, p *peer) error {
				_, result := h.prompt(p, "delivery-1", "verify #41")
				p.conn.Close()
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.Is(err, ErrClosed) },
		},
		{name: "no answer within the RPC timeout", rpcTimeout: 300 * time.Millisecond,
			drive: func(h *harness, p *peer) error {
				_, result := h.prompt(p, "delivery-1", "verify #41")
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.Is(err, ErrNoAnswer) },
		},
		{name: "the caller's deadline",
			drive: func(h *harness, p *peer) error {
				conn := h.conn()
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				defer cancel()
				result := async(func() error { return conn.Prompt(ctx, "delivery-1", "verify #41") })
				p.negotiate()
				p.expect(shimwire.TypePrompt)
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.Is(err, context.DeadlineExceeded) },
		},
		{name: "OMP refuses the negotiation ahead of the prompt",
			drive: func(h *harness, p *peer) error {
				conn := h.conn()
				result := async(func() error { return conn.Prompt(context.Background(), "delivery-1", "verify #41") })
				request := p.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
				p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeNegotiateProtocol, Success: false, Error: "unsupported"})
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool {
				return errors.As(err, &refusal) && refusal.Command == shimwire.TypeNegotiateProtocol
			},
		},
		{name: "OMP refuses a get_state",
			drive: func(h *harness, p *peer) error {
				conn := h.conn()
				result := async(func() error { _, err := conn.GetState(context.Background()); return err })
				p.negotiate()
				request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
				p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: false, Error: "busy"})
				return awaitResult(h.t, result)
			},
			isCase: func(err error) bool { return errors.As(err, &refusal) && refusal.Command == shimwire.TypeGetState },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startListener(t, harnessOptions{rpcTimeout: test.rpcTimeout})
			err := test.drive(h, h.connect(testToken))
			if !test.isCase(err) {
				t.Fatalf("the call returned %v, not the failure this case is about", err)
			}
			if got := errors.Is(err, runtime.ErrPromptRefused); got != test.refused {
				t.Fatalf("errors.Is(%v, runtime.ErrPromptRefused) = %v, want %v", err, got, test.refused)
			}
		})
	}
}

func TestPromptRefusesAnEmptyDeliveryID(t *testing.T) {
	h := startListener(t, harnessOptions{})
	h.connect(testToken)
	if err := h.conn().Prompt(context.Background(), "", "verify #41"); err == nil {
		t.Fatal("Prompt sent a prompt with no delivery id, which the shim cannot dedupe")
	}
}

// The turn facts are events, in wire order, and the ack is not one of them: an acked prompt,
// then OMP's agent_start and agent_end, then the close, is exactly Hello, TurnStart, TurnEnd,
// Closed (F4, B4).
func TestEventsCarryTheTurnInWireOrderAndNoAck(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := dial(t, h.listener.Addr())
	p.hello(testToken)
	p.expect(shimwire.TypeHelloAck)
	frame, result := h.prompt(p, "delivery-1", "verify #41")
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	p.send(shimwire.AgentStart{})
	p.send(shimwire.Raw{Type: "tool_execution_start", JSON: json.RawMessage(`{"type":"tool_execution_start","toolName":"bash"}`)})
	p.send(shimwire.AgentEnd{})
	p.conn.Close()
	want := []Event{
		Hello{Claim: testClaim, Generation: testGeneration},
		TurnStart{Claim: testClaim},
		TurnEnd{Claim: testClaim},
		Closed{Claim: testClaim},
	}
	var got []Event
	for range want {
		got = append(got, h.next())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	if rest := h.stop(); len(rest) != 0 {
		t.Fatalf("further events = %#v, want none", rest)
	}
}

// Only the shim's replay of a start it already observed carries the delivery id
// (worker-shim.ts:365), and the event carries it exactly when the frame did.
func TestTurnStartCarriesTheDeliveryIDOnlyWhenTheFrameDid(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	p.send(shimwire.AgentStart{DeliveryID: "delivery-1"})
	p.send(shimwire.AgentStart{})
	if event := h.next(); event != (TurnStart{Claim: testClaim, DeliveryID: "delivery-1"}) {
		t.Fatalf("replayed start = %#v, want its delivery id", event)
	}
	if event := h.next(); event != (TurnStart{Claim: testClaim}) {
		t.Fatalf("OMP's own start = %#v, want no delivery id", event)
	}
}

// An agent_start can precede the ack of the prompt that caused it; the event is emitted when
// the frame arrives, and Prompt still returns on the ack (worker-rpc.test.ts:349-365).
func TestAStartThatPrecedesTheAckIsStillEmitted(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	frame, result := h.prompt(p, "delivery-1", "verify #41")
	p.send(shimwire.AgentStart{})
	if event := h.next(); event != (TurnStart{Claim: testClaim}) {
		t.Fatalf("event = %#v, want TurnStart", event)
	}
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

// OMP can refuse a prompt after acknowledging it — "Agent is busy" once it finds a turn it did
// not start (worker-rpc.ts:359-368). The ack already returned, so the refusal is an event:
// exactly one, naming the delivery the refused request carried.
func TestALateRefusalIsOneEventCarryingTheDeliveryID(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	frame, result := h.prompt(p, "delivery-1", "verify #41")
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	p.send(shimwire.AgentStart{})
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: "Agent is busy"})
	// The same refusal again is no longer late for anything.
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: false, Error: "Agent is busy"})
	p.conn.Close()
	want := []Event{
		TurnStart{Claim: testClaim},
		LateRefusal{Claim: testClaim, DeliveryID: "delivery-1", Error: "Agent is busy"},
		Closed{Claim: testClaim},
	}
	var got []Event
	for range want {
		got = append(got, h.next())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	if rest := h.stop(); len(rest) != 0 {
		t.Fatalf("further events = %#v, want none", rest)
	}
	logs := h.logs.Lines()
	if len(logs) != 2 ||
		!strings.HasPrefix(logs[0], "worker-stream: prompt refused after its acknowledgement") ||
		!strings.Contains(logs[0], "deliveryId=delivery-1") ||
		!strings.HasPrefix(logs[1], "worker-stream: refusal for a prompt request this connection is not waiting on") {
		t.Fatalf("logs = %q, want the late refusal and then the unattributable repeat", logs)
	}
}

// A refusal that belongs to no acknowledged prompt on this connection — an id it never sent, a
// prompt whose turn already ended (worker-rpc.test.ts:320-347), or one acknowledged on a
// connection that has since closed, replayed from the shim's backlog on its replacement —
// reverses nothing: no event, one log line.
func TestARefusalForNoAcknowledgedPromptEmitsNothingAndLogsOnce(t *testing.T) {
	refusal := func(id string) shimwire.Response {
		return shimwire.Response{ID: id, Command: shimwire.TypePrompt, Success: false, Error: "Agent is busy"}
	}
	for _, test := range []struct {
		name  string
		drive func(h *harness, p *peer) (*peer, string)
	}{
		{"an unknown request id", func(h *harness, p *peer) (*peer, string) {
			return p, "never-sent"
		}},
		{"after the turn ended", func(h *harness, p *peer) (*peer, string) {
			frame, result := h.prompt(p, "delivery-1", "verify #41")
			p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
			if err := awaitResult(h.t, result); err != nil {
				h.t.Fatalf("Prompt: %v", err)
			}
			p.send(shimwire.AgentStart{})
			p.send(shimwire.AgentEnd{})
			h.next()
			h.next()
			return p, frame.ID
		}},
		{"after a later prompt superseded it", func(h *harness, p *peer) (*peer, string) {
			first, result := h.prompt(p, "delivery-1", "verify #41")
			p.send(shimwire.Response{ID: first.ID, Command: shimwire.TypePrompt, Success: true})
			if err := awaitResult(h.t, result); err != nil {
				h.t.Fatalf("Prompt: %v", err)
			}
			conn := h.conn()
			async(func() error { return conn.Prompt(context.Background(), "delivery-2", "verify #42") })
			p.expect(shimwire.TypePrompt) // left unanswered: the connection closes under it
			return p, first.ID
		}},
		{"after the connection closed", func(h *harness, p *peer) (*peer, string) {
			frame, result := h.prompt(p, "delivery-1", "verify #41")
			p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
			if err := awaitResult(h.t, result); err != nil {
				h.t.Fatalf("Prompt: %v", err)
			}
			p.conn.Close()
			if event := h.next(); event != (Closed{Claim: testClaim}) {
				h.t.Fatalf("event = %#v, want Closed", event)
			}
			return h.connect(testToken), frame.ID
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startListener(t, harnessOptions{})
			p, id := test.drive(h, h.connect(testToken))
			p.send(refusal(id))
			p.conn.Close()
			if event := h.next(); event != (Closed{Claim: testClaim}) {
				t.Fatalf("event = %#v, want only Closed", event)
			}
			logs := h.logs.Lines()
			if len(logs) != 1 || !strings.HasPrefix(logs[0], "worker-stream: refusal for a prompt request this connection is not waiting on") {
				t.Fatalf("logs = %q, want one line for the unattributable refusal", logs)
			}
		})
	}
}

// rpc_chunk sequences between whole frames reassemble through shimwire and dispatch exactly as
// a plain line would: a chunked agent_end ends the turn, a chunked response answers its request.
func TestChunkSequencesInterleavedWithWholeFramesReassemble(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()

	p.send(shimwire.AgentStart{})
	p.sendChunked("agent-end-1", agentEndFrame(t, 3<<20+17))
	var state runtime.ConnState
	result := async(func() error {
		var err error
		state, err = conn.GetState(context.Background())
		return err
	})
	p.negotiate()
	request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
	p.send(shimwire.AgentStart{})
	// A get_state answer too large for one line: padded data, chunked like any other frame.
	big := `{"id":"` + request.ID + `","type":"response","command":"get_state","success":true,"data":{"isStreaming":true,"pad":"` +
		strings.Repeat("y", shimwire.MaxFrameBytes) + `"}}`
	p.sendChunked("state-1", []byte(big))
	p.sendChunked("agent-end-2", agentEndFrame(t, shimwire.MaxFrameBytes+5))

	if err := awaitResult(t, result); err != nil {
		t.Fatalf("GetState answered by a chunked response: %v", err)
	}
	if !state.IsStreaming {
		t.Fatal("GetState lost the chunked answer's isStreaming")
	}
	want := []Event{TurnStart{Claim: testClaim}, TurnEnd{Claim: testClaim}, TurnStart{Claim: testClaim}, TurnEnd{Claim: testClaim}}
	var got []Event
	for range want {
		got = append(got, h.next())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	if lines := h.logs.Lines(); len(lines) != 0 {
		t.Fatalf("logs = %q, want none", lines)
	}
}

// A whole frame arriving mid-sequence means the sender abandoned it: the sequence is dropped
// and logged, never joined to what follows, and the whole frame is still read
// (worker-rpc.ts:348-349).
func TestAWholeFrameInterruptingASequenceDropsTheSequence(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	chunks, err := shimwire.SplitChunks("agent-end-1", agentEndFrame(t, 3<<20))
	if err != nil {
		t.Fatal(err)
	}
	p.send(chunks[0])
	p.send(chunks[1])
	p.send(shimwire.AgentStart{})
	for _, chunk := range chunks[2:] {
		p.send(chunk) // the tail of a sequence whose head is gone: refused, not a frame
	}
	p.send(shimwire.AgentEnd{})
	if event := h.next(); event != (TurnStart{Claim: testClaim}) {
		t.Fatalf("event = %#v, want the interrupting TurnStart", event)
	}
	if event := h.next(); event != (TurnEnd{Claim: testClaim}) {
		t.Fatalf("event = %#v, want the plain agent_end's TurnEnd and nothing from the dropped sequence", event)
	}
	logs := h.logs.Lines()
	if len(logs) == 0 || !strings.Contains(logs[0], "abandoned after 2 of") {
		t.Fatalf("logs = %q, want the abandoned sequence named first", logs)
	}
}

func TestGetStateReadsIsStreaming(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		h := startListener(t, harnessOptions{})
		p := h.connect(testToken)
		conn := h.conn()
		var state runtime.ConnState
		result := async(func() error {
			var err error
			state, err = conn.GetState(context.Background())
			return err
		})
		p.negotiate()
		request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
		p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: true,
			Data: mustJSON(t, map[string]any{"isStreaming": streaming, "model": "m"})})
		if err := awaitResult(t, result); err != nil || state.IsStreaming != streaming {
			t.Fatalf("GetState = %#v, %v; want IsStreaming %v", state, err, streaming)
		}
	}
}

// An answer that does not say whether a turn is running is not "idle": the daemon's restart
// decision hangs on it, so it is an error rather than a guess (worker-rpc.ts:476-485).
func TestGetStateRefusesAnAnswerWithoutIsStreaming(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	result := async(func() error { _, err := conn.GetState(context.Background()); return err })
	p.negotiate()
	request := p.expect(shimwire.TypeGetState).(shimwire.GetState)
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeGetState, Success: true, Data: mustJSON(t, map[string]any{"model": "m"})})
	if err := awaitResult(t, result); err == nil {
		t.Fatal("GetState read an answer without isStreaming as a state")
	}
}

// Negotiation is protocol v2, sent once per connection — before the first request OMP answers,
// so every holder of a runtime.Conn gets a negotiated stream without asking for it.
func TestNegotiationIsSentOnceAheadOfTheFirstAgentRequest(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	result := async(func() error { return conn.Negotiate(context.Background()) })
	p.negotiate()
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if err := awaitResult(t, async(func() error { return conn.Negotiate(context.Background()) })); err != nil {
		t.Fatalf("a second Negotiate: %v", err)
	}
	result = async(func() error { return conn.Prompt(context.Background(), "delivery-1", "verify #41") })
	frame := p.expect(shimwire.TypePrompt).(shimwire.Prompt) // no second negotiate_protocol first
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

func TestANegotiationOMPRefusesFailsTheRequestBehindIt(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	result := async(func() error { return conn.Prompt(context.Background(), "delivery-1", "verify #41") })
	request := p.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypeNegotiateProtocol, Success: false, Error: "unsupported"})
	err := awaitResult(t, result)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Command != shimwire.TypeNegotiateProtocol {
		t.Fatalf("Prompt behind a refused negotiation = %v, want its RefusedError", err)
	}
	// Not negotiated, so the next request negotiates again.
	result = async(func() error { return conn.Prompt(context.Background(), "delivery-1", "verify #41") })
	p.negotiate()
	frame := p.expect(shimwire.TypePrompt).(shimwire.Prompt)
	p.send(shimwire.Response{ID: frame.ID, Command: shimwire.TypePrompt, Success: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("Prompt after a successful negotiation: %v", err)
	}
}

// A response to the wrong command is not an answer the request can use.
func TestAnAnswerForAnotherCommandIsAnError(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	result := async(func() error { return conn.Negotiate(context.Background()) })
	request := p.expect(shimwire.TypeNegotiateProtocol).(shimwire.NegotiateProtocol)
	p.send(shimwire.Response{ID: request.ID, Command: shimwire.TypePrompt, Success: true})
	if err := awaitResult(t, result); err == nil {
		t.Fatal("Negotiate accepted a prompt response as its answer")
	}
}

// Shutdown is a frame the shim answers itself by closing OMP's stdin; nothing comes back.
func TestShutdownWritesTheFrame(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	if err := conn.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	p.expect(shimwire.TypeShutdown)
	p.conn.Close()
	if event := h.next(); event != (Closed{Claim: testClaim}) {
		t.Fatalf("event = %#v, want Closed", event)
	}
	if err := conn.Shutdown(context.Background()); !isClosed(err) {
		t.Fatalf("Shutdown on a closed connection = %v, want ErrClosed", err)
	}
}

func TestAdoptWorkingCopyAsksTheShimAndReadsItsResult(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	identity := runtime.GitIdentity{Name: "legion-bot", Email: "bot@legion.dev"}

	result := async(func() error { return conn.AdoptWorkingCopy(context.Background(), identity, 3*time.Second) })
	request := p.expect(shimwire.TypeAdoptWorkingCopy).(shimwire.AdoptWorkingCopy)
	if request.JJUser != "legion-bot" || request.JJEmail != "bot@legion.dev" || request.TimeoutMs != 3000 || request.ID == "" {
		t.Fatalf("adopt-working-copy frame = %#v", request)
	}
	p.send(shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: true})
	if err := awaitResult(t, result); err != nil {
		t.Fatalf("AdoptWorkingCopy: %v", err)
	}

	result = async(func() error { return conn.AdoptWorkingCopy(context.Background(), identity, 3*time.Second) })
	request = p.expect(shimwire.TypeAdoptWorkingCopy).(shimwire.AdoptWorkingCopy)
	p.send(shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: false, Error: "jj metaedit exited 1"})
	err := awaitResult(t, result)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "jj metaedit exited 1" {
		t.Fatalf("AdoptWorkingCopy = %v, want a RefusedError carrying the shim's error", err)
	}

	if err := conn.AdoptWorkingCopy(context.Background(), runtime.GitIdentity{}, time.Second); err == nil {
		t.Fatal("AdoptWorkingCopy sent a frame with no identity")
	}
}
