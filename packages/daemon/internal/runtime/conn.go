package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// ErrPromptRefused is Oh My Pi answering a prompt frame with `success: false`. Prompt wraps it
// for a refusal and for nothing else, because the two failures are charged differently: an agent
// that refuses prompts spends its prompt budget (a worker that refused every prompt would
// otherwise be retried forever), while a prompt lost to the transport was never the agent's
// answer and is sent again at no charge. A refusal also carries the words Oh My Pi gave
// (RefusalReason), since one of them — a turn already running — is no refusal of the work.
var ErrPromptRefused = errors.New("runtime: the agent refused the prompt")

// ErrAdoptRefused is the agent's shim answering AdoptWorkingCopy with `ok: false`: the working
// copy could not be given the role's App identity — a jj wedged on a stale lock, a metaedit that
// failed, the shim's own command timeout. AdoptWorkingCopy wraps it for a refusal and for nothing
// else, and the supervisor charges it to the same prompt budget ErrPromptRefused is charged to.
// The two are the far side's own answer about this workspace, so asking again in a minute changes
// nothing by itself, and an unbounded retry of either is a silent stop: the task is never
// delivered, nothing fails, and the claim waits for hours with no one told (INFRA-1480, where two
// planning tasks were held 31 and 33 hours by a stale lock). Charged, the claim runs out of budget
// and fails, which the workflow reports.
//
// What is not this, and is charged nothing because it is no answer about the workspace: no answer
// within the RPC timeout, a closed connection, a frame lost on the way — the prompt never reached
// the far side — and a failure to mint the role's identity, which is ErrIdentityUnavailable
// below.
//
// Every `ok: false` is this, the shim's own command timeout included, and the refusal's text is
// never read to decide: a jj wedged on a stale lock reports as that timeout, and that is the very
// fault the measured incident was.
var ErrAdoptRefused = errors.New("runtime: the shim refused to adopt the working copy")

// ErrIdentityUnavailable is the daemon failing to mint the role's GitHub App identity for the
// working copy it is about to adopt: supervise's Machine.adopt wraps it around the error its
// Identity dep gave, and around nothing else. Like a lost frame it is charged nothing — the token
// is minted daemon-side, before any frame leaves, so the failure is no answer about this
// workspace or this agent — and the delivery waits for the next hello, sweep, turn end or ready.
//
// It is a sentinel of its own rather than one more unnamed uncharged failure because the
// supervisor's log is where an operator diagnoses a send that never lands, and these two are
// diagnosed in different places: a frame lost on the way is the cluster's wiring, while this is
// an outage of the GitHub App, which no amount of looking at the network shows. Reported as a
// transport loss, it sends that operator to the wrong side of the daemon.
var ErrIdentityUnavailable = errors.New("runtime: the role's GitHub App identity could not be minted")

// RefusalReason is the words the agent refused with — Oh My Pi's `error` beside its
// `success: false` — read from the error in err's chain that carries them, or "" when none does.
// The stream's refusal carries them (stream.RefusedError), and so does the fake connection's.
func RefusalReason(err error) string {
	var refusal interface{ RefusalReason() string }
	if errors.As(err, &refusal) {
		return refusal.RefusalReason()
	}
	return ""
}

// ErrTokenRefused is the agent's shim answering AgentSecretsToken without a token: it could not
// read its projected token, or it was built before the request and Oh My Pi refused the frame as
// an unknown command. Asking the same shim again changes neither.
var ErrTokenRefused = errors.New("runtime: the shim did not read its projected token")

// ErrRescueUnsupported is a shim that does not know `rescue-unpushed`: one built before the push
// check, which forwards the frame to its agent, and the agent refuses it as a command it does not
// know. Nothing was checked and nothing was pushed. Every other failure of the check is not this.
var ErrRescueUnsupported = errors.New("runtime: the pod's shim predates the push check")

// RescueRequest is one push check of a pod's disk: the prefix of every rescue branch, which the
// shim numbers from 1, the bound of one workspace's check and push, and the bound of the whole.
type RescueRequest struct {
	Branch           string
	WorkspaceTimeout time.Duration
	Timeout          time.Duration
}

// WorkspaceRescue is one workspace's push check, as the shim that ran it reports it.
type WorkspaceRescue struct {
	// Dir is the workspace's directory on the pod's disk.
	Dir string
	// Status is RescueClean, RescueRescued or RescueFailed.
	Status string
	// Branches is each rescue branch pushed for it, with the commit it holds.
	Branches []RescuedBranch
	// Error is why a failed check failed.
	Error string
}

// RescuedBranch is one rescue branch a push check pushed, and the commit it holds.
type RescuedBranch struct {
	Name, Commit string
}

// The statuses of a workspace's push check.
const (
	RescueClean   = "clean"
	RescueRescued = "rescued"
	RescueFailed  = "error"
)

// WorkspaceBundleTimeout is how long the agent's side is given to build its workspace's bundle:
// a handful of jj reads and one `git bundle create` over the commits no remote branch holds,
// which is a pack of a working copy's own change and a few unpushed commits, never the clone's
// history. It is the runtimes' budget for the request, not a setting: a release that cannot get
// a bundle in this long is one the daemon stops rather than one it waits longer for.
const WorkspaceBundleTimeout = 5 * time.Minute

// Conn is one agent's live connection, as everything above the transport sees it: a way to ask
// the agent to do something, and nothing more. The facts that come back — the acknowledgement,
// the turn starting, the turn ending, the connection closing — are events on the stream, never
// return values here (an acknowledged prompt is not a delivered one, and the difference is the
// whole of LEGION-10).
//
// The interface lives in this package so that the runtime, its fake, and the supervisor depend
// on these methods rather than on the stream's own struct.
type Conn interface {
	// Prompt hands the agent a task and returns when Oh My Pi has acknowledged the frame — or
	// refused it, or the transport failed. A refusal wraps ErrPromptRefused; every other error
	// is transport. The turn it may start is observed on the stream. The delivery id is the
	// daemon's: an agent that receives the same id twice answers it without starting a second
	// turn.
	Prompt(ctx context.Context, deliveryID, message string) error
	// GetState asks the agent what it is doing. The one field the daemon reads back is whether
	// a turn is in flight, which is how a restart tells a delivery already in progress from one
	// that never arrived.
	GetState(ctx context.Context) (ConnState, error)
	// Shutdown asks the agent to end its own process — the graceful half of a stop.
	Shutdown(ctx context.Context) error
	// AdoptWorkingCopy makes the agent's side set the author of its working copy, for a
	// workspace the daemon cannot reach itself.
	AdoptWorkingCopy(ctx context.Context, id GitIdentity, timeout time.Duration) error
	// BundleWorkspace has the agent's side bundle the unpushed work of the workspace every role
	// of issue shares, for a release that is about to take the pod, its disk and that workspace
	// away. A bundle larger than MaxWorkspaceBundleBytes is refused rather than sent, with the
	// size it would have been in the refusal, so the release can stop and record why.
	BundleWorkspace(ctx context.Context, issue string, timeout time.Duration) (WorkspaceBundle, error)
	// RescueUnpushed asks the agent's shim to check every workspace on its pod's disk for work the
	// remote lacks and to push that work to rescue branches, as the agent's role. A shim that does
	// not know the request is ErrRescueUnsupported; every other error is transport or a refusal.
	RescueUnpushed(ctx context.Context, request RescueRequest) ([]WorkspaceRescue, error)
	// AgentSecretsEnrollment hands the agent's shim the secrets broker's enrollment id for its
	// process, which the shim keeps beside the key it generated.
	AgentSecretsEnrollment(ctx context.Context, enrollmentID string) error
	// AgentSecretsToken asks the agent's shim to read its projected service-account token again
	// and returns it as the mount holds it now. A shim that cannot is ErrTokenRefused; every other
	// error is transport.
	AgentSecretsToken(ctx context.Context) (string, error)
	// Sequence is the order the connection was registered in: one registered later has a larger
	// sequence, so a caller can tell a connection newer than another.
	Sequence() uint64
}

// Conns is the directory of live connections, keyed by claim. A claim with no connection is not
// an error: a pane whose agent has not connected yet, or has gone away, is an ordinary state,
// and the caller's answer to it is to wait or to skip the graceful step.
type Conns interface {
	Conn(claim claim.Token) (Conn, bool)
}

// ConnState is the agent's answer to `get_state`, reduced to the one field the daemon acts on.
type ConnState struct {
	IsStreaming bool
}
