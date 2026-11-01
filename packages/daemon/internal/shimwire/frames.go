// Package shimwire is the wire between a phase worker's `legion worker-shim` and the daemon:
// the NDJSON framing both ends read and write, the frames they exchange, the rpc_chunk transport
// for a frame too large for one line, and the shim-side delivery dedupe.
//
// It is shared by `cmd/legion worker-shim` and `internal/stream`, and it is where the protocol's
// behaviour is kept rather than re-derived at each end. Every rule here was ported from the
// TypeScript daemon LEGION-223 removed; a `worker-rpc.ts:N`, `line-reader.ts:N`,
// `socket-writer.ts:N`, `worker-stream-listener.ts:N` or `worker-shim.ts:N` citation at a rule
// names that source in this repository's history. The structure is Go's.
package shimwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	TypePromptResult           = "prompt_result"
	TypeShutdown               = "shutdown"
	TypeAdoptWorkingCopy       = "adopt-working-copy"
	TypeAdoptWorkingCopyResult = "adopt-working-copy-result"
	TypeRPCChunk               = "rpc_chunk"
	// hello2 is the hello every shim sends since DaemonAPIVersion 8: the boot token and, under a
	// runtime that enrolls pods with the secrets broker, the pod's key thumbprint and
	// projected token. A daemon that predates it decodes it as Raw and refuses "malformed hello";
	// this daemon refuses the v1 hello by name (internal/stream).
	TypeHello2                       = "hello2"
	TypeAgentSecretsEnrollment       = "agent-secrets-enrollment"
	TypeAgentSecretsEnrollmentResult = "agent-secrets-enrollment-result"
	TypeAgentSecretsToken            = "agent-secrets-token"
	TypeAgentSecretsTokenResult      = "agent-secrets-token-result"
	TypeLauncherHello                = "launcher_hello"
	TypeLauncherHelloAck             = "launcher_hello_ack"
	TypeLauncherState                = "launcher_state"
	TypeLauncherStart                = "launcher_start"
	TypeLauncherStartResult          = "launcher_start_result"
	TypeLauncherStop                 = "launcher_stop"
	TypeLauncherStopResult           = "launcher_stop_result"
	// The workspace bundle's two directions: the daemon asking one role's shim for the unpushed
	// work of the workspace every role of its issue shares, and the daemon handing that bundle to
	// the launcher of the pod that replaces the released one, in slices under the frame limit.
	TypeBundleWorkspace       = "bundle-workspace"
	TypeBundleWorkspaceResult = "bundle-workspace-result"
	TypeLauncherBundle        = "launcher_bundle"
	TypeLauncherBundleResult  = "launcher_bundle_result"
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
// hello. The daemon relays both to the broker and keeps neither past the enrollment; it asks for
// the token again (AgentSecretsToken) when the hello's would expire before it enrolls.
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

// AgentSecretsToken asks the shim to read its projected token again: the kubelet rewrites the
// file before the token expires, and a token read at the hello can expire before the daemon
// enrolls with it. The shim answers AgentSecretsTokenResult with the same ID. A shim built before
// this frame forwards it to OMP, which answers it as an unknown command.
type AgentSecretsToken struct {
	ID string `json:"id"`
}

// AgentSecretsTokenResult answers AgentSecretsToken: the token as the shim just read it, or why it
// could not.
type AgentSecretsTokenResult struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	PodToken string `json:"podToken,omitempty"`
	Error    string `json:"error,omitempty"`
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
// Code is the process's exit status, or one of two values no process exits with: -1 when the
// launcher has no status for it (killed by a signal, which Signal then names, or never waited on),
// and LauncherExitSetupFailed.
type LauncherExit struct {
	Generation uint64 `json:"generation"`
	Code       int    `json:"code"`
	Signal     string `json:"signal,omitempty"`
}

// LauncherExitSetupFailed is the Code of an issue pod role's generation whose role-init failed
// before it started the shim: setting up the role's namespaces, or starting the shim itself, which
// role-init alone tells the launcher by its exit status (launcher.RoleInitSetupFailedExit). Its
// reason is role-init's own line in the role's log. It is apart from every code a shim exits
// with, so the daemon never reads a setup failure as the agent's own exit.
const LauncherExitSetupFailed = -2

// LauncherState is the launcher's actual child state at hello and after every child transition.
type LauncherState struct {
	Child    *LauncherChild `json:"child,omitempty"`
	LastExit *LauncherExit  `json:"lastExit,omitempty"`
}

// LauncherStart starts one worker-shim child. The daemon owns the generation and request ID; a
// repeated request is idempotent only when its payload, Files included, is unchanged. Files are
// the generation's credentials, the boot token among them: the launcher writes each, owner-only
// and exclusively, into a fresh directory for this generation in its own memory-backed storage
// before the child starts, and removes the directory when the child ends — so a role started in a
// running pod never waits on the kubelet to propagate a Secret. Argv and Env name those files by
// path; no credential is ever in Argv, Env, or an error.
type LauncherStart struct {
	ID         string            `json:"id"`
	Generation uint64            `json:"generation"`
	Argv       []string          `json:"argv"`
	Env        []string          `json:"env"`
	Files      map[string]string `json:"files,omitempty"`
	ResumeFile string            `json:"resumeFile,omitempty"`
}

// GenerationDir is the directory under a launcher's private directory that holds one generation's
// Files. Both ends name it: the daemon in the Argv and Env that point at a file, the launcher when
// it writes them.
func GenerationDir(generation uint64) string { return "g" + strconv.FormatUint(generation, 10) }

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

// AgentStart is OMP beginning a turn. DeliveryID is set only on the shim's own synthetic replay
// of a start it already observed (worker-shim.ts:365); OMP never sets it.
type AgentStart struct {
	DeliveryID string `json:"deliveryId,omitempty"`
}

// AgentEnd is OMP finishing a turn. Its real form carries the whole transcript, which is why it
// is the frame that arrives chunked.
type AgentEnd struct{}

// PromptResult is Oh My Pi reporting how one prompt's work ended, and the only frame that names
// the prompt a turn belonged to: ID is the `prompt` command's own request id, where AgentEnd
// carries no prompt identity at all ("that frame carries no prompt identity, so correlate
// prompts through `prompt_result`", omp://rpc.md). An accepted prompt completes exactly once,
// either as `data.agentInvoked: false` on its response — it finished locally and started no turn
// — or as this frame, always written after that response.
//
// AgentInvoked is false for a prompt that ran no turn of its own, and Error is Oh My Pi's reason
// when the work ended in one — an object, never a string (PromptError). Oh My Pi's own
// prompt_result carries more than these three members — `status`, its word for how the work
// ended, and `sessionSettled` among them — which nothing in the daemon reads; the decode drops
// them.
type PromptResult struct {
	ID           string       `json:"id"`
	AgentInvoked bool         `json:"agentInvoked"`
	Error        *PromptError `json:"error,omitempty"`
}

// PromptError is how a prompt's work failed, as the pinned Oh My Pi declares it: `{message,
// provider?, model?, httpStatus?, retryable}`, the "failure detail of a `prompt_result` with
// `status: \"error\"`". Oh My Pi writes it for a prompt whose last assistant message ended with
// stopReason "error" — a provider request its own retries could not get through, where
// Retryable says it may still succeed later — and for one it failed outright before any turn.
//
// Only Message is read, as the reason a failed prompt is recorded with (PromptResult.Reason).
// The other members are modelled so the frame survives this package whole, the way every other
// frame does: a decode that kept only what today's reader wants would quietly drop the provider,
// the model and the status behind a failure from anything that re-encodes it.
type PromptError struct {
	Message    string `json:"message"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Retryable  bool   `json:"retryable"`
}

// Reason is Oh My Pi's message for work that ended in an error, and the empty string for work
// that did not: the one thing the daemon reads of the failure.
func (f PromptResult) Reason() string {
	if f.Error == nil {
		return ""
	}
	return f.Error.Message
}

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

// BundleWorkspace asks the shim to bundle the unpushed work of the workspace it runs in, so the
// daemon can keep it while the released pod, its disk and that workspace go. The daemon names the
// issue, the largest bundle it will take and the budget, never a command or a path: the shim
// builds the bundle with the jj and git of the workspace it already works in.
type BundleWorkspace struct {
	ID        string `json:"id"`
	Issue     string `json:"issue"`
	MaxBytes  int    `json:"maxBytes"`
	TimeoutMs int    `json:"timeoutMs"`
}

// BundleWorkspaceResult answers BundleWorkspace with the bundle and the three facts a restore
// needs beside it: the commit the working copy sat on, the heads the bundle carries, and each
// local bookmark that pointed into the saved set. A bundle larger than the request's MaxBytes is
// refused rather than sent, with SizeBytes naming what it would have been, so the caller can stop
// the release and record why. The result crosses the shim's lane as an rpc_chunk sequence when it
// is larger than one line.
type BundleWorkspaceResult struct {
	ID        string            `json:"id"`
	OK        bool              `json:"ok"`
	Error     string            `json:"error,omitempty"`
	SizeBytes int               `json:"sizeBytes"`
	Bundle    []byte            `json:"bundle,omitempty"`
	Head      string            `json:"head,omitempty"`
	Tips      []string          `json:"tips,omitempty"`
	Bookmarks map[string]string `json:"bookmarks,omitempty"`
	At        time.Time         `json:"at"`
}

// LauncherBundle is one slice of a saved workspace bundle on its way to the launcher of the pod
// that replaces a released one. The launcher has no rpc_chunk lane of its own, so the slicing is
// the frame's: Payload is the Index-th LauncherBundlePayloadBytes of the whole body
// (SplitWorkspaceBundle), Count how many slices there are, and SHA256 the digest of the body the
// slices rebuild, which the launcher checks before it writes anything. ID names the sequence, so a
// repeated slice is recognised as a repeated launcher_start id is.
type LauncherBundle struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Count   int    `json:"count"`
	Payload []byte `json:"payload"`
	SHA256  string `json:"sha256"`
}

// LauncherBundleResult answers one LauncherBundle. Held is the launcher holding the whole,
// digest-checked body: the confirmation the daemon deletes the saved bundle on, which it answers
// the last slice of the sequence with. The restore itself runs in the roles container before the
// first role of the pod starts.
type LauncherBundleResult struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
	OK    bool   `json:"ok"`
	Held  bool   `json:"held,omitempty"`
	Error string `json:"error,omitempty"`
}

// WorkspaceBundle is the saved work the LauncherBundle slices carry: the git bundle's own bytes,
// the commit the working copy sat on, the heads the bundle carries and each local bookmark that
// pointed into the saved set. It is no frame of its own — it is the body a sequence rebuilds.
type WorkspaceBundle struct {
	Bundle    []byte            `json:"-"`
	Head      string            `json:"head"`
	Tips      []string          `json:"tips,omitempty"`
	Bookmarks map[string]string `json:"bookmarks,omitempty"`
	At        time.Time         `json:"at"`
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
func (AgentStart) FrameType() string                   { return TypeAgentStart }
func (AgentEnd) FrameType() string                     { return TypeAgentEnd }
func (PromptResult) FrameType() string                 { return TypePromptResult }
func (Shutdown) FrameType() string                     { return TypeShutdown }
func (AdoptWorkingCopy) FrameType() string             { return TypeAdoptWorkingCopy }
func (AdoptWorkingCopyResult) FrameType() string       { return TypeAdoptWorkingCopyResult }
func (RPCChunk) FrameType() string                     { return TypeRPCChunk }
func (r Raw) FrameType() string                        { return r.Type }
func (Hello2) FrameType() string                       { return TypeHello2 }
func (AgentSecretsEnrollment) FrameType() string       { return TypeAgentSecretsEnrollment }
func (AgentSecretsEnrollmentResult) FrameType() string { return TypeAgentSecretsEnrollmentResult }
func (AgentSecretsToken) FrameType() string            { return TypeAgentSecretsToken }
func (AgentSecretsTokenResult) FrameType() string      { return TypeAgentSecretsTokenResult }
func (LauncherHello) FrameType() string                { return TypeLauncherHello }
func (LauncherHelloAck) FrameType() string             { return TypeLauncherHelloAck }
func (LauncherState) FrameType() string                { return TypeLauncherState }
func (LauncherStart) FrameType() string                { return TypeLauncherStart }
func (LauncherStartResult) FrameType() string          { return TypeLauncherStartResult }
func (LauncherStop) FrameType() string                 { return TypeLauncherStop }
func (LauncherStopResult) FrameType() string           { return TypeLauncherStopResult }
func (BundleWorkspace) FrameType() string              { return TypeBundleWorkspace }
func (BundleWorkspaceResult) FrameType() string        { return TypeBundleWorkspaceResult }
func (LauncherBundle) FrameType() string               { return TypeLauncherBundle }
func (LauncherBundleResult) FrameType() string         { return TypeLauncherBundleResult }

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

func (f PromptResult) MarshalJSON() ([]byte, error) {
	type plain PromptResult
	return marshalFrame(TypePromptResult, plain(f))
}

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

func (f AgentSecretsToken) MarshalJSON() ([]byte, error) {
	type plain AgentSecretsToken
	return marshalFrame(TypeAgentSecretsToken, plain(f))
}

func (f AgentSecretsTokenResult) MarshalJSON() ([]byte, error) {
	type plain AgentSecretsTokenResult
	return marshalFrame(TypeAgentSecretsTokenResult, plain(f))
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

func (f BundleWorkspace) MarshalJSON() ([]byte, error) {
	type plain BundleWorkspace
	return marshalFrame(TypeBundleWorkspace, plain(f))
}

func (f BundleWorkspaceResult) MarshalJSON() ([]byte, error) {
	type plain BundleWorkspaceResult
	return marshalFrame(TypeBundleWorkspaceResult, plain(f))
}

func (f LauncherBundle) MarshalJSON() ([]byte, error) {
	type plain LauncherBundle
	return marshalFrame(TypeLauncherBundle, plain(f))
}

func (f LauncherBundleResult) MarshalJSON() ([]byte, error) {
	type plain LauncherBundleResult
	return marshalFrame(TypeLauncherBundleResult, plain(f))
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

// issueKeyShape is a Dispatch issue key, the one shape an issue has. The shim puts the issue into
// a revset (`remote_bookmarks(exact:"legion/<issue>")`), so a frame whose issue is not one is
// refused here rather than quoted into a command.
var issueKeyShape = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// Validate refuses a bundle request the shim would refuse. The daemon is its only sender.
func (f BundleWorkspace) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: bundle-workspace carries no id", ErrMalformedFrame)
	case !issueKeyShape.MatchString(f.Issue):
		return fmt.Errorf("%w: bundle-workspace issue %q is not an issue key", ErrMalformedFrame, f.Issue)
	case f.MaxBytes <= 0:
		return fmt.Errorf("%w: bundle-workspace maxBytes is %d", ErrMalformedFrame, f.MaxBytes)
	case f.TimeoutMs <= 0:
		return fmt.Errorf("%w: bundle-workspace timeoutMs is %d", ErrMalformedFrame, f.TimeoutMs)
	}
	return nil
}

// Validate refuses a bundle result the daemon could not act on: one answering no request, a
// refusal with no reason, or a bundle whose bytes and declared size disagree.
func (f BundleWorkspaceResult) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: bundle-workspace-result carries no id", ErrMalformedFrame)
	case !f.OK && f.Error == "":
		return fmt.Errorf("%w: bundle-workspace-result refuses without a reason", ErrMalformedFrame)
	case f.OK && f.Head == "":
		return fmt.Errorf("%w: bundle-workspace-result carries no head", ErrMalformedFrame)
	case f.OK && f.SizeBytes != len(f.Bundle):
		return fmt.Errorf("%w: bundle-workspace-result says %d bytes and carries %d", ErrMalformedFrame, f.SizeBytes, len(f.Bundle))
	}
	return nil
}

// Validate refuses a bundle slice the launcher could not place in its sequence.
func (f LauncherBundle) Validate() error {
	switch {
	case f.ID == "":
		return fmt.Errorf("%w: launcher_bundle carries no id", ErrMalformedFrame)
	case f.Count <= 0:
		return fmt.Errorf("%w: launcher_bundle count is %d", ErrMalformedFrame, f.Count)
	case f.Index < 0 || f.Index >= f.Count:
		return fmt.Errorf("%w: launcher_bundle index %d of %d", ErrMalformedFrame, f.Index, f.Count)
	case len(f.Payload) == 0:
		return fmt.Errorf("%w: launcher_bundle %d carries no payload", ErrMalformedFrame, f.Index)
	case len(f.Payload) > LauncherBundlePayloadBytes:
		return fmt.Errorf("%w: launcher_bundle payload is %d bytes, limit %d", ErrMalformedFrame, len(f.Payload), LauncherBundlePayloadBytes)
	case !digestShape.MatchString(f.SHA256):
		return fmt.Errorf("%w: launcher_bundle sha256 %q is not a hex SHA-256", ErrMalformedFrame, f.SHA256)
	}
	return nil
}

// digestShape is a hex-encoded SHA-256: 64 lowercase hex characters.
var digestShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LauncherBundlePayloadBytes is how much of a bundle's body one launcher_bundle frame carries.
// Base64 of it, plus the frame's other members, stays well inside MaxFrameBytes, which is the
// whole reason the sequence exists: the launcher reads one frame per line and nothing reassembles
// rpc_chunk on its side.
const LauncherBundlePayloadBytes = 256 << 10

// SplitWorkspaceBundle slices b into the launcher_bundle sequence that carries it, every slice
// declaring the digest of the whole body so the launcher can refuse one that did not arrive as it
// was sent. id names the sequence.
func SplitWorkspaceBundle(id string, b WorkspaceBundle) ([]LauncherBundle, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: a launcher_bundle sequence needs an id", ErrMalformedFrame)
	}
	body, err := marshalWorkspaceBundle(b)
	if err != nil {
		return nil, err
	}
	digest := BundleDigest(body)
	count := (len(body) + LauncherBundlePayloadBytes - 1) / LauncherBundlePayloadBytes
	frames := make([]LauncherBundle, 0, count)
	for index := range count {
		end := min((index+1)*LauncherBundlePayloadBytes, len(body))
		frames = append(frames, LauncherBundle{
			ID: id, Index: index, Count: count, Payload: body[index*LauncherBundlePayloadBytes : end], SHA256: digest,
		})
	}
	return frames, nil
}

// ParseWorkspaceBundle reads the body a launcher_bundle sequence rebuilt: one JSON line naming
// the saved commits, then the git bundle's own bytes. The bundle is kept raw rather than encoded
// into that line so a 40 MiB bundle is base64'd once, by the slices that carry it, and not twice.
func ParseWorkspaceBundle(body []byte) (WorkspaceBundle, error) {
	head, bundle, found := bytes.Cut(body, []byte("\n"))
	if !found {
		return WorkspaceBundle{}, fmt.Errorf("%w: a workspace bundle body carries no header line", ErrMalformedFrame)
	}
	var b WorkspaceBundle
	if err := json.Unmarshal(head, &b); err != nil {
		return WorkspaceBundle{}, fmt.Errorf("%w: a workspace bundle header: %w", ErrMalformedFrame, err)
	}
	if len(bundle) == 0 {
		return WorkspaceBundle{}, fmt.Errorf("%w: a workspace bundle body carries no bundle", ErrMalformedFrame)
	}
	b.Bundle = bundle
	return b, nil
}

// BundleDigest is the digest a launcher_bundle sequence declares for the body it carries.
func BundleDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// marshalWorkspaceBundle renders b as the body ParseWorkspaceBundle reads. JSON escapes its own
// newlines, so the header line is one line.
func marshalWorkspaceBundle(b WorkspaceBundle) ([]byte, error) {
	if b.Head == "" {
		return nil, fmt.Errorf("%w: a workspace bundle carries no head", ErrMalformedFrame)
	}
	if len(b.Bundle) == 0 {
		return nil, fmt.Errorf("%w: a workspace bundle carries no bundle", ErrMalformedFrame)
	}
	head, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	body := make([]byte, 0, len(head)+1+len(b.Bundle))
	body = append(body, head...)
	body = append(body, '\n')
	return append(body, b.Bundle...), nil
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
	case len(f.Argv) == 0:
		return fmt.Errorf("%w: launcher_start carries no argv", ErrMalformedFrame)
	}
	for name := range f.Files {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
			return fmt.Errorf("%w: launcher_start file name %q is not a plain file name", ErrMalformedFrame, name)
		}
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

// Validate refuses a token request with no request id: the daemon is its only sender.
func (f AgentSecretsToken) Validate() error {
	if f.ID == "" {
		return fmt.Errorf("%w: agent-secrets-token carries no id", ErrMalformedFrame)
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
	case TypePromptResult:
		return decodeInto[PromptResult](trimmed)
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
	case TypeAgentSecretsToken:
		return decodeInto[AgentSecretsToken](trimmed)
	case TypeAgentSecretsTokenResult:
		return decodeInto[AgentSecretsTokenResult](trimmed)
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
	case TypeBundleWorkspace:
		return decodeInto[BundleWorkspace](trimmed)
	case TypeBundleWorkspaceResult:
		return decodeInto[BundleWorkspaceResult](trimmed)
	case TypeLauncherBundle:
		return decodeInto[LauncherBundle](trimmed)
	case TypeLauncherBundleResult:
		return decodeInto[LauncherBundleResult](trimmed)
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
