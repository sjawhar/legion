// Package runtime is the boundary between the daemon's supervision of an agent and the thing
// that actually runs it — a pane on the daemon's private tmux server today, a pod in a cluster
// at Stage 4.
//
// The boundary carries process facts only: a runtime spawns, stops, and observes processes, and
// says `Alive`, `Gone`, `NotRecordedProcess`, or `Uncertain` about them. What the agent inside
// the process is doing — that it connected, acknowledged a prompt, started or ended a turn — is
// the worker stream's to report, a peer source beside this one. Both reach the supervisor, which
// is the only place the two kinds of fact meet.
//
// The connection contract (`Conn`, `Conns`) lives here rather than in the stream package so that
// this package and its fake depend on methods rather than on the stream's structs; the stream's
// own connection satisfies it.
package runtime

import (
	"context"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// ControllerLaunch says which side starts the interactive controller: the daemon opens it in a
// pane it owns ("daemon", tmux), or the operator starts it on their own machine against the
// daemon's API ("operator", a cluster the operator has no terminal in).
type ControllerLaunch string

const (
	ControllerLaunchDaemon   ControllerLaunch = "daemon"
	ControllerLaunchOperator ControllerLaunch = "operator"
)

// Runtime is what the supervisor has instead of a process table. Every method is about one
// agent's process, addressed by the locator the runtime itself minted, except `Observe` and
// `ReconcileOrphans`, which are about all of them.
type Runtime interface {
	// Spawn starts an agent and returns the locator that identifies the process it started —
	// including the incarnation, captured at spawn, that later observations are fenced against.
	Spawn(ctx context.Context, spec SpawnSpec) (Locator, error)
	// Resume starts the same agent again from the session file the spec names, after waiting
	// for loc's incarnation to be Gone. A claim resumes the agent it recorded or none: a fresh
	// agent on a claim that had one is the failure the same-agent refusal exists to catch.
	Resume(ctx context.Context, loc Locator, spec SpawnSpec) (Locator, error)
	// Suspend stops the process gracefully and leaves the agent's session where it is. The
	// caller keeps the session file; the claim is resumable from it.
	Suspend(ctx context.Context, loc Locator) error
	// Stop ends the process: a shutdown frame over the claim's registered connection when there
	// is one, then a wait, then a kill — of a process verified to still be the one the locator
	// recorded, never of whatever now holds that pane or pid.
	Stop(ctx context.Context, loc Locator, grace time.Duration) error
	// Probe is one observation of one locator, now. `Uncertain` is a verdict, not a failure: a
	// returned error means the runtime itself could not be asked, and an error is never a
	// statement about the process.
	Probe(ctx context.Context, loc Locator) (Observation, error)
	// Observe is the periodic sweep of every locator the runtime knows about. The channel closes
	// when ctx ends.
	Observe(ctx context.Context) (<-chan Observation, error)
	// ReconcileOrphans ends the processes this runtime owns that the daemon does not know about
	// — what a crash between spawning a process and persisting its locator leaves behind.
	ReconcileOrphans(ctx context.Context, known []Locator, grace time.Duration) error
	// AdoptWorkingCopy hands the agent's working copy the git identity its commits are authored
	// with, in the place the working copy actually lives (which under a sandbox is not a
	// directory the daemon can see).
	AdoptWorkingCopy(ctx context.Context, loc Locator, id GitIdentity) error
	// ControllerLaunch says which side starts the controller under this runtime.
	ControllerLaunch() ControllerLaunch
}

// SpawnSpec is everything a runtime needs to start one agent: which claim it is, what it is
// working on, and the environment, secrets, and prompt it starts with. The claim token travels
// with the process because the runtime addresses the agent's connection by it — `Stop` sends a
// shutdown frame over `Conns.Conn(loc.Claim)` — and because a locator without it could not be
// matched to the claim it belongs to.
//
// Env is the pane's plain variables; Secrets never travel as values — each is written to a 0600
// file and reaches the process as a `<NAME>_FILE` pointer. ResumeSessionFile is set only by
// `Resume`, and it names the transcript the same agent continues from.
type SpawnSpec struct {
	Claim             claim.Token
	Project           string
	Tree              string
	Issue             string
	Role              claim.Role
	Generation        uint64
	BootToken         string
	Env               map[string]string
	Secrets           map[string]string
	Prompt            PromptParts
	Workspace         string
	ResumeSessionFile string
}

// PromptParts are the pieces of the agent's system prompt: its role prompt files, the sentence
// that tells it which claim it is, and the operator's deployment instructions. The runtime
// concatenates them into exactly one `--append-system-prompt` word — one, because a second
// occurrence of the flag replaces the first rather than adding to it.
type PromptParts struct {
	RolePromptPaths            []string
	Addressing                 string
	DeploymentInstructionsPath string
}

// ObservationKind is what a runtime can say about a process.
type ObservationKind string

const (
	// Alive: the process the locator recorded is running — its identity re-checked, not merely
	// its slot occupied.
	Alive ObservationKind = "alive"
	// Gone: the process the locator recorded is not running. Nothing is there.
	Gone ObservationKind = "gone"
	// NotRecordedProcess: something is there, and it is not the process the locator recorded —
	// a reissued pane id, a reissued pid, a locator that outlived its tmux server.
	NotRecordedProcess ObservationKind = "not_recorded_process"
	// Uncertain: the runtime could not learn anything that proves the process alive or gone. It
	// is a fourth verdict precisely so that it is never read as either: the supervisor re-arms
	// the probe and counts the streak, and changes no state on it.
	Uncertain ObservationKind = "uncertain"
)

// Observation is one runtime fact about one process, with the moment it was observed and, for a
// verdict that needs one, the detail an operator reads.
type Observation struct {
	Locator Locator
	Kind    ObservationKind
	At      time.Time
	Detail  string
}

// GitIdentity is the author a worker's commits carry.
type GitIdentity struct {
	Name  string
	Email string
}
