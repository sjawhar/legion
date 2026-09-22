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
// answer and is sent again at no charge.
var ErrPromptRefused = errors.New("runtime: the agent refused the prompt")

// Conn is one agent's live connection, as everything above the transport sees it: a way to ask
// the agent to do something, and nothing more. The facts that come back — the acknowledgement,
// the turn starting, the turn ending, the connection closing — are events on the stream, never
// return values here (an acknowledged prompt is not a delivered one, and the difference is the
// whole of LEGION-10).
//
// The interface lives in this package so that the runtime, its fake, and the supervisor depend
// on these four methods rather than on the stream's own struct.
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
