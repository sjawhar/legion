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
	"regexp"
	"strings"
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
	TypeAbort                  = "abort"
	TypeAdoptWorkingCopy       = "adopt-working-copy"
	TypeAdoptWorkingCopyResult = "adopt-working-copy-result"
	TypeRPCChunk               = "rpc_chunk"
	// hello2 is the hello every shim sends since GoDaemonAPIVersion 8: the boot token and, under a
	// runtime that enrolls pods with the secrets broker (AGENTC-393), the pod's key thumbprint and
	// projected token. A daemon that predates it decodes it as Raw and refuses "malformed hello";
	// this daemon refuses the v1 hello by name (internal/stream).
	TypeHello2                       = "hello2"
	TypeAgentSecretsEnrollment       = "agent-secrets-enrollment"
	TypeAgentSecretsEnrollmentResult = "agent-secrets-enrollment-result"
	TypeLauncherHello       = "launcher_hello"
	TypeLauncherHelloAck    = "launcher_hello_ack"
	TypeLauncherState       = "launcher_state"
	TypeLauncherStart       = "launcher_start"
	TypeLauncherStartResult = "launcher_start_result"
	TypeLauncherStop        = "launcher_stop"
	TypeLauncherStopResult  = "launcher_stop_result"
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

// AgentSecretsHello is the pod's session identity as the shim reports it: the base64url SHA-256
// JWK thumbprint of the P-256 key `agent-secrets keygen` wrote into the pod's key directory, and
// the projected service-account token for the broker's audience, read from its mount at the
// hello. The daemon relays both to the broker and keeps neither past the enrollment.
type AgentSecretsHello struct {
	Thumbprint string `json:"thumbprint"`
	PodToken   string `json:"podToken"`
}

// Hello2 authenticates a reverse-dialed shim: its first line, carrying the boot token the daemon
// minted for the pane or pod, and the pod's identity when the shim was started with
// --agent-secrets-key-dir (a tmux pane never is, and sends none).
type Hello2 struct {
	BootToken    string             `json:"bootToken"`
	AgentSecrets *AgentSecretsHello `json:"agentSecrets,omitempty"`
}

// AgentSecretsEnrollment hands the shim the broker's enrollment id for this pod generation; the
// shim writes it beside the key (`<key dir>/enrollment`, what `agent-secrets` reads) and starts
// the lease renewer, then answers AgentSecretsEnrollmentResult with the same ID.
type AgentSecretsEnrollment struct {
	ID           string `json:"id"`
	EnrollmentID string `json:"enrollmentId"`
}

// AgentSecretsEnrollmentResult answers AgentSecretsEnrollment.
type AgentSecretsEnrollmentResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// LauncherHello authenticates a role launcher independently from its child shim. The daemon
// minted Token belongs to exactly this pod UID, role and Secret epoch; LauncherID changes each
// time Kubernetes restarts the launcher container.
type LauncherHello struct {
	Token      string `json:"token"`
	Sandbox    string `json:"sandbox"`
	Role       string `json:"role"`
	PodUID     string `json:"podUid"`
	LauncherID string `json:"launcherId"`
}

// LauncherHelloAck admits a launcher to receive child process commands.
type LauncherHelloAck struct{}

// LauncherChild is the actual worker-shim child the launcher has running, if any.
type LauncherChild struct {
	Generation uint64 `json:"generation"`
	PID        int    `json:"pid"`
}

// LauncherExit is the last child exit the launcher observed. A launcher reports it after a
// reconnect before it accepts another start, so the daemon never invents Gone from a lost reply.
type LauncherExit struct {
	Generation    uint64 `json:"generation"`
	Code          int    `json:"code"`
	Signal        string `json:"signal,omitempty"`
	WorkspaceLost bool   `json:"workspaceLost,omitempty"`
}

// LauncherState is the launcher's actual child state at hello and after every child transition.
type LauncherState struct {
	Child    *LauncherChild `json:"child,omitempty"`
	LastExit *LauncherExit  `json:"lastExit,omitempty"`
}

// LauncherStart starts one worker-shim child. The daemon owns the generation and request ID; a
// repeated request is idempotent only when its payload is unchanged.
type LauncherStart struct {
	ID         string   `json:"id"`
	Generation uint64   `json:"generation"`
	BootToken  string   `json:"bootToken"`
	Argv       []string `json:"argv"`
	Env        []string `json:"env"`
	ResumeFile string   `json:"resumeFile,omitempty"`
}

// LauncherStartResult is the launcher's answer to LauncherStart.
type LauncherStartResult struct {
	ID                string `json:"id"`
	OK                bool   `json:"ok"`
	RunningGeneration uint64 `json:"runningGeneration,omitempty"`
	Error             string `json:"error,omitempty"`
}

// LauncherStop stops only the child of Generation. The launcher refuses an older generation
// rather than risking a newer role process.
type LauncherStop struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	GraceMs    int    `json:"graceMs"`
}

// LauncherStopResult is the launcher's answer to LauncherStop.
type LauncherStopResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
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

// Abort asks OMP to end the turn it is running: it cancels the turn's tool children and the model
// call, and answers once the agent is idle, keeping the process and its session. Its answer can
// come before the agent_end that ends the turn on the wire, so that agent_end, not the answer, is
// the turn's end.
type Abort struct {
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

// Shutdown asks the shim to end the wrapped process: SIGTERM, then SIGKILL if it is still running
// when the shim's grace runs out. The shim acts on it and never forwards it.
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

func (Hello) FrameType() string                        { return TypeHello }
func (HelloAck) FrameType() string                     { return TypeHelloAck }
func (NegotiateProtocol) FrameType() string            { return TypeNegotiateProtocol }
func (Prompt) FrameType() string                       { return TypePrompt }
func (Response) FrameType() string                     { return TypeResponse }
func (GetState) FrameType() string                     { return TypeGetState }
func (Abort) FrameType() string                        { return TypeAbort }
func (AgentStart) FrameType() string                   { return TypeAgentStart }
func (AgentEnd) FrameType() string                     { return TypeAgentEnd }
func (Shutdown) FrameType() string                     { return TypeShutdown }
func (AdoptWorkingCopy) FrameType() string             { return TypeAdoptWorkingCopy }
func (AdoptWorkingCopyResult) FrameType() string       { return TypeAdoptWorkingCopyResult }
func (RPCChunk) FrameType() string                     { return TypeRPCChunk }
func (r Raw) FrameType() string                        { return r.Type }
func (Hello2) FrameType() string                       { return TypeHello2 }
func (AgentSecretsEnrollment) FrameType() string       { return TypeAgentSecretsEnrollment }
func (AgentSecretsEnrollmentResult) FrameType() string { return TypeAgentSecretsEnrollmentResult }
func (LauncherHello) FrameType() string       { return TypeLauncherHello }
func (LauncherHelloAck) FrameType() string    { return TypeLauncherHelloAck }
func (LauncherState) FrameType() string       { return TypeLauncherState }
func (LauncherStart) FrameType() string       { return TypeLauncherStart }
func (LauncherStartResult) FrameType() string { return TypeLauncherStartResult }
func (LauncherStop) FrameType() string        { return TypeLauncherStop }
func (LauncherStopResult) FrameType() string  { return TypeLauncherStopResult }

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

func (f Abort) MarshalJSON() ([]byte, error) {
	type plain Abort
	return marshalFrame(TypeAbort, plain(f))
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

func (f Hello2) MarshalJSON() ([]byte, error) {
	type plain Hello2
	return marshalFrame(TypeHello2, plain(f))
}

func (f AgentSecretsEnrollment) MarshalJSON() ([]byte, error) {
	type plain AgentSecretsEnrollment
	return marshalFrame(TypeAgentSecretsEnrollment, plain(f))
}

func (f AgentSecretsEnrollmentResult) MarshalJSON() ([]byte, error) {
	type plain AgentSecretsEnrollmentResult
	return marshalFrame(TypeAgentSecretsEnrollmentResult, plain(f))
}

func (f LauncherHello) MarshalJSON() ([]byte, error) {
	type plain LauncherHello
	return marshalFrame(TypeLauncherHello, plain(f))
}

func (LauncherHelloAck) MarshalJSON() ([]byte, error) {
	return marshalFrame(TypeLauncherHelloAck, struct{}{})
}

func (f LauncherState) MarshalJSON() ([]byte, error) {
	type plain LauncherState
	return marshalFrame(TypeLauncherState, plain(f))
}

func (f LauncherStart) MarshalJSON() ([]byte, error) {
	type plain LauncherStart
	return marshalFrame(TypeLauncherStart, plain(f))
}

func (f LauncherStartResult) MarshalJSON() ([]byte, error) {
	type plain LauncherStartResult
	return marshalFrame(TypeLauncherStartResult, plain(f))
}

func (f LauncherStop) MarshalJSON() ([]byte, error) {
	type plain LauncherStop
	return marshalFrame(TypeLauncherStop, plain(f))
}

func (f LauncherStopResult) MarshalJSON() ([]byte, error) {
	type plain LauncherStopResult
	return marshalFrame(TypeLauncherStopResult, plain(f))
}

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

// thumbprintShape is a base64url-encoded SHA-256: 43 characters, no padding (RFC 7638).
var thumbprintShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// Validate refuses a hello2 the listener refuses: no boot token, or an identity that is not a
// thumbprint plus a JWT. The token is not verified here — the broker verifies it — only shaped.
func (f Hello2) Validate() error {
	if f.BootToken == "" {
		return fmt.Errorf("%w: hello2 carries no bootToken", ErrMalformedFrame)
	}
	if f.AgentSecrets == nil {
		return nil
	}
	switch id := f.AgentSecrets; {
	case id.Thumbprint == "":
		return fmt.Errorf("%w: hello2 agentSecrets carries no thumbprint", ErrMalformedFrame)
	case !thumbprintShape.MatchString(id.Thumbprint):
		return fmt.Errorf("%w: hello2 agentSecrets thumbprint is not a base64url SHA-256", ErrMalformedFrame)
	case id.PodToken == "":
		return fmt.Errorf("%w: hello2 agentSecrets carries no podToken", ErrMalformedFrame)
	case strings.Count(id.PodToken, ".") != 2:
		return fmt.Errorf("%w: hello2 agentSecrets podToken is not a JWT", ErrMalformedFrame)
	}
	return nil
}

// Validate refuses an unactionable launcher hello before the listener hands it to the runtime.
func (f LauncherHello) Validate() error {
	switch {
	case f.Token == "":
		return fmt.Errorf("%w: launcher_hello carries no token", ErrMalformedFrame)
	case f.Sandbox == "":
		return fmt.Errorf("%w: launcher_hello carries no sandbox", ErrMalformedFrame)
	case f.Role == "":
		return fmt.Errorf("%w: launcher_hello carries no role", ErrMalformedFrame)
	case f.PodUID == "":
		return fmt.Errorf("%w: launcher_hello carries no podUid", ErrMalformedFrame)
	case f.LauncherID == "":
		return fmt.Errorf("%w: launcher_hello carries no launcherId", ErrMalformedFrame)
	}
	return nil
}

func (f LauncherStart) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: launcher_start carries no id", ErrMalformedFrame)
	case f.Generation == 0:
		return fmt.Errorf("%w: launcher_start carries no generation", ErrMalformedFrame)
	case f.BootToken == "":
		return fmt.Errorf("%w: launcher_start carries no bootToken", ErrMalformedFrame)
	case len(f.Argv) == 0:
		return fmt.Errorf("%w: launcher_start carries no argv", ErrMalformedFrame)
	}
	return nil
}

func (f LauncherStop) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: launcher_stop carries no id", ErrMalformedFrame)
	case f.Generation == 0:
		return fmt.Errorf("%w: launcher_stop carries no generation", ErrMalformedFrame)
	case f.GraceMs <= 0:
		return fmt.Errorf("%w: launcher_stop graceMs is %d", ErrMalformedFrame, f.GraceMs)
	}
	return nil
}

// Validate refuses an enrollment frame with no request id or no enrollment id: the daemon is its
// only sender, so either is a bug to name.
func (f AgentSecretsEnrollment) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: agent-secrets-enrollment carries no id", ErrMalformedFrame)
	case f.EnrollmentID == "":
		return fmt.Errorf("%w: agent-secrets-enrollment carries no enrollmentId", ErrMalformedFrame)
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
	case TypeAbort:
		return decodeInto[Abort](trimmed)
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
	case TypeHello2:
		return decodeInto[Hello2](trimmed)
	case TypeAgentSecretsEnrollment:
		return decodeInto[AgentSecretsEnrollment](trimmed)
	case TypeAgentSecretsEnrollmentResult:
		return decodeInto[AgentSecretsEnrollmentResult](trimmed)
	case TypeLauncherHello:
		return decodeInto[LauncherHello](trimmed)
	case TypeLauncherHelloAck:
		return LauncherHelloAck{}, nil
	case TypeLauncherState:
		return decodeInto[LauncherState](trimmed)
	case TypeLauncherStart:
		return decodeInto[LauncherStart](trimmed)
	case TypeLauncherStartResult:
		return decodeInto[LauncherStartResult](trimmed)
	case TypeLauncherStop:
		return decodeInto[LauncherStop](trimmed)
	case TypeLauncherStopResult:
		return decodeInto[LauncherStopResult](trimmed)
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
