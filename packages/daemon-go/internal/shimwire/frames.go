// Package shimwire is the wire between a phase worker's `legion worker-shim` and the daemon:
// the NDJSON framing both ends read and write, the frames they exchange, the rpc_chunk transport
// for a frame too large for one line, and the shim-side delivery dedupe.
//
// It is shared by `cmd/legion worker-shim` and `internal/stream`, and it is where the protocol's
// behaviour is kept rather than re-derived at each end. Every rule here is one the shipped
// TypeScript already enforces — packages/daemon/src/daemon/{line-reader,socket-writer,worker-rpc,
// worker-stream-listener}.ts and packages/daemon/src/cli/worker-shim.ts — cited at the rule it
// keeps. The structure is Go's: a TypeScript file is a source of behaviour, never of shape.
package shimwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// The frame type names, verbatim from the shipped protocol.
const (
	TypeHello                  = "hello"
	TypeHelloAck               = "hello_ack"
	TypeNegotiateProtocol      = "negotiate_protocol"
	TypePrompt                 = "prompt"
	TypeResponse               = "response"
	TypeGetState               = "get_state"
	TypeAgentStart             = "agent_start"
	TypeAgentEnd               = "agent_end"
	TypeShutdown               = "shutdown"
	TypeAdoptWorkingCopy       = "adopt-working-copy"
	TypeAdoptWorkingCopyResult = "adopt-working-copy-result"
	TypeRPCChunk               = "rpc_chunk"
)

// The protocol's sizes, verified against the shipped files: the largest plain line including its
// newline (worker-rpc.ts:8-12), the base64 payload of one chunk (worker-rpc.ts:16-18), the
// largest frame a chunk sequence may carry (worker-rpc.ts:13-15), and the bytes a connection may
// send before its first newline (worker-stream-listener.ts:11-13).
const (
	MaxFrameBytes       = 1 << 20
	ChunkPayloadBytes   = 256 << 10
	MaxReassembledBytes = 64 << 20
	MaxHelloBytes       = 4096
)

var (
	// ErrNotAFrame is a line that is not a JSON object. An unknown frame *type* is not this: the
	// shim is a bridge and forwards what it does not model.
	ErrNotAFrame = errors.New("shimwire: line is not a JSON object")
	// ErrMalformedFrame is a frame of a known type whose fields are not what its only sender
	// sends. Validate reports it; decoding does not, so a bridge can still forward.
	ErrMalformedFrame = errors.New("shimwire: malformed frame")
)

// Frame is one decoded line. The concrete types below model every frame the shim and the daemon
// exchange; anything else decodes to Raw and crosses unchanged.
type Frame interface {
	// FrameType is the frame's `type` member on the wire.
	FrameType() string
}

// Hello authenticates a reverse-dialed shim: its first line, carrying the boot token the daemon
// minted for the pane (worker-stream-listener.ts:139-148).
type Hello struct {
	BootToken string `json:"bootToken"`
}

// HelloAck accepts the hello. The shim spawns OMP only after it (worker-stream-listener.ts:14).
type HelloAck struct{}

// NegotiateProtocol opens a connection's RPC protocol version (worker-rpc.ts:430).
type NegotiateProtocol struct {
	ID              string `json:"id"`
	ProtocolVersion int    `json:"protocolVersion"`
}

// Prompt delivers one task to OMP. DeliveryID names the logical delivery the daemon is retrying
// — the id the dedupe keys on — while ID is this attempt's request (worker-rpc.ts:452).
type Prompt struct {
	ID         string `json:"id"`
	DeliveryID string `json:"deliveryId"`
	Message    string `json:"message"`
}

// Response is OMP's answer to a request, carrying the request's own id. Data is the answer's
// payload, left raw because its shape belongs to the command (worker-rpc.ts:369-377).
type Response struct {
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// StateData is the get_state answer's payload. IsStreaming is absent rather than false when the
// worker does not report it, which is the difference between "idle" and "unknown"
// (worker-rpc.ts:476-485).
type StateData struct {
	IsStreaming *bool `json:"isStreaming"`
}

// GetState asks the worker whether a turn is running.
type GetState struct {
	ID string `json:"id"`
}

// AgentStart is OMP beginning a turn. DeliveryID is set only on the shim's own synthetic replay
// of a start it already observed (worker-shim.ts:365); OMP never sets it.
type AgentStart struct {
	DeliveryID string `json:"deliveryId,omitempty"`
}

// AgentEnd is OMP finishing a turn. Its real form carries the whole transcript, which is why it
// is the frame that arrives chunked.
type AgentEnd struct{}

// Shutdown asks the shim to close the wrapped process's stdin. The shim answers it and never
// forwards it (worker-shim.ts:323-326).
type Shutdown struct{}

// AdoptWorkingCopy asks the shim to run the working-copy adoption in its own workspace under the
// given jj identity. The daemon names the identity and the budget, never a command or a path.
type AdoptWorkingCopy struct {
	ID        string `json:"id"`
	JJUser    string `json:"jjUser"`
	JJEmail   string `json:"jjEmail"`
	TimeoutMs int    `json:"timeoutMs"`
}

// AdoptWorkingCopyResult answers AdoptWorkingCopy (worker-shim.ts:332-341).
type AdoptWorkingCopyResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// RPCChunk is one slice of a logical frame too large for a single line. Data is base64 of the
// frame's Index-th ChunkPayloadBytes slice; ByteLength is the whole frame's size. See chunks.go.
type RPCChunk struct {
	ChunkID    string `json:"chunkId"`
	Index      int    `json:"index"`
	Count      int    `json:"count"`
	ByteLength int    `json:"byteLength"`
	Data       string `json:"data"`
}

// Raw is a frame this package does not model: OMP's tool and error events, and whatever a later
// OMP adds. It keeps the line verbatim and re-encodes to exactly those bytes, so a bridge that
// decodes to inspect a frame can still pass it on unchanged.
type Raw struct {
	Type string
	JSON json.RawMessage
}

func (Hello) FrameType() string                  { return TypeHello }
func (HelloAck) FrameType() string               { return TypeHelloAck }
func (NegotiateProtocol) FrameType() string      { return TypeNegotiateProtocol }
func (Prompt) FrameType() string                 { return TypePrompt }
func (Response) FrameType() string               { return TypeResponse }
func (GetState) FrameType() string               { return TypeGetState }
func (AgentStart) FrameType() string             { return TypeAgentStart }
func (AgentEnd) FrameType() string               { return TypeAgentEnd }
func (Shutdown) FrameType() string               { return TypeShutdown }
func (AdoptWorkingCopy) FrameType() string       { return TypeAdoptWorkingCopy }
func (AdoptWorkingCopyResult) FrameType() string { return TypeAdoptWorkingCopyResult }
func (RPCChunk) FrameType() string               { return TypeRPCChunk }
func (r Raw) FrameType() string                  { return r.Type }

func (f Hello) MarshalJSON() ([]byte, error) {
	type plain Hello
	return marshalFrame(TypeHello, plain(f))
}

func (HelloAck) MarshalJSON() ([]byte, error) { return marshalFrame(TypeHelloAck, struct{}{}) }

func (f NegotiateProtocol) MarshalJSON() ([]byte, error) {
	type plain NegotiateProtocol
	return marshalFrame(TypeNegotiateProtocol, plain(f))
}

func (f Prompt) MarshalJSON() ([]byte, error) {
	type plain Prompt
	return marshalFrame(TypePrompt, plain(f))
}

func (f Response) MarshalJSON() ([]byte, error) {
	type plain Response
	return marshalFrame(TypeResponse, plain(f))
}

func (f GetState) MarshalJSON() ([]byte, error) {
	type plain GetState
	return marshalFrame(TypeGetState, plain(f))
}

func (f AgentStart) MarshalJSON() ([]byte, error) {
	type plain AgentStart
	return marshalFrame(TypeAgentStart, plain(f))
}

func (AgentEnd) MarshalJSON() ([]byte, error) { return marshalFrame(TypeAgentEnd, struct{}{}) }

func (Shutdown) MarshalJSON() ([]byte, error) { return marshalFrame(TypeShutdown, struct{}{}) }

func (f AdoptWorkingCopy) MarshalJSON() ([]byte, error) {
	type plain AdoptWorkingCopy
	return marshalFrame(TypeAdoptWorkingCopy, plain(f))
}

func (f AdoptWorkingCopyResult) MarshalJSON() ([]byte, error) {
	type plain AdoptWorkingCopyResult
	return marshalFrame(TypeAdoptWorkingCopyResult, plain(f))
}

func (f RPCChunk) MarshalJSON() ([]byte, error) {
	type plain RPCChunk
	return marshalFrame(TypeRPCChunk, plain(f))
}

func (r Raw) MarshalJSON() ([]byte, error) { return r.JSON, nil }

// Validate refuses a hello the listener would refuse (worker-stream-listener.ts:140-148).
func (f Hello) Validate() error {
	if f.BootToken == "" {
		return fmt.Errorf("%w: hello carries no bootToken", ErrMalformedFrame)
	}
	return nil
}

// Validate refuses an adoption request the shim would refuse. The daemon is its only sender, so
// a malformed one is a bug to name rather than a condition to paper over
// (worker-shim.ts:171-180).
func (f AdoptWorkingCopy) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: adopt-working-copy carries no id", ErrMalformedFrame)
	case f.JJUser == "" || f.JJEmail == "":
		return fmt.Errorf("%w: adopt-working-copy carries no jj identity", ErrMalformedFrame)
	case f.TimeoutMs <= 0:
		return fmt.Errorf("%w: adopt-working-copy timeoutMs is %d", ErrMalformedFrame, f.TimeoutMs)
	}
	return nil
}

// Decode reads one line as a frame. A type this package models decodes to its own Go type; any
// other object — including one with no type at all — decodes to Raw, because the shim is a
// bridge and a frame it does not understand still has somewhere to go. Only a line that is not a
// JSON object, or a modelled frame whose members are of the wrong JSON type, is an error.
//
// The returned frame owns its bytes: line may be reused as soon as Decode returns.
func Decode(line []byte) (Frame, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: %.80q", ErrNotAFrame, line)
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &head); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotAFrame, err)
	}
	switch head.Type {
	case TypeHello:
		return decodeInto[Hello](trimmed)
	case TypeHelloAck:
		return HelloAck{}, nil
	case TypeNegotiateProtocol:
		return decodeInto[NegotiateProtocol](trimmed)
	case TypePrompt:
		return decodeInto[Prompt](trimmed)
	case TypeResponse:
		return decodeInto[Response](trimmed)
	case TypeGetState:
		return decodeInto[GetState](trimmed)
	case TypeAgentStart:
		return decodeInto[AgentStart](trimmed)
	case TypeAgentEnd:
		return AgentEnd{}, nil
	case TypeShutdown:
		return Shutdown{}, nil
	case TypeAdoptWorkingCopy:
		return decodeInto[AdoptWorkingCopy](trimmed)
	case TypeAdoptWorkingCopyResult:
		return decodeInto[AdoptWorkingCopyResult](trimmed)
	case TypeRPCChunk:
		return decodeInto[RPCChunk](trimmed)
	default:
		return Raw{Type: head.Type, JSON: bytes.Clone(trimmed)}, nil
	}
}

// decodeInto unmarshals a modelled frame. The `plain` alias is what keeps this from re-entering
// the frame's own MarshalJSON side: only the fields are read here.
func decodeInto[T Frame](line []byte) (Frame, error) {
	var frame T
	if err := json.Unmarshal(line, &frame); err != nil {
		return nil, fmt.Errorf("decode the %s frame: %w", frame.FrameType(), err)
	}
	return frame, nil
}

// marshalFrame renders a frame's own fields plus its type member. No frame struct carries a type
// field: the type is the frame's identity, not data a caller could set to something else. Every
// name it is given is a constant of this package, none of which needs JSON escaping.
func marshalFrame(frameType string, fields any) ([]byte, error) {
	rest, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	head := `{"type":"` + frameType + `"`
	if len(rest) == len("{}") {
		return []byte(head + "}"), nil
	}
	out := make([]byte, 0, len(head)+len(rest))
	out = append(out, head...)
	out = append(out, ',')
	return append(out, rest[1:]...), nil
}
