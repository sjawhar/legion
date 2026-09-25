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
	"errors"
	"fmt"
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
// agent's process, addressed by the locator the runtime itself minted, except `Release`, which is
// about a claim whether or not a process runs for it, and `Observe` and `ReconcileOrphans`, which
// are about all of them.
//
// A locator that no longer names the claim's current process — nothing is there, or something
// else is — is a process already stopped as far as `Suspend` and `Release` are concerned: neither
// acts on it, and neither returns an error for it. `Suspend` leaves a newer incarnation the runtime
// recorded for the claim alone; a process of the claim that no locator records — a sandbox's pod
// the controller recreated — it stops all the same, so it cannot keep running on the claim's token.
// `Release` still ends whatever else the runtime holds for the claim. A state the runtime cannot
// verify is an error, never "stopped".
type Runtime interface {
	// Spawn starts an agent and returns the locator that identifies the process it started —
	// including the incarnation, captured at spawn, that later observations are fenced against.
	Spawn(ctx context.Context, spec SpawnSpec) (Locator, error)
	// Resume starts the same agent again from the session file the spec names, after waiting
	// for the previous incarnation to be Gone. prev is the incarnation the caller recorded, nil
	// when it recorded none, and a hint: a runtime that finds a claim's process by the claim's own
	// name, as a sandbox does, waits out whatever holds that name even when prev is nil. A claim
	// resumes the agent it recorded or none: a fresh agent on a claim that had one is the failure
	// the same-agent refusal exists to catch.
	Resume(ctx context.Context, prev *Locator, spec SpawnSpec) (Locator, error)
	// Suspend stops the process gracefully, within the runtime's stop grace, and keeps everything
	// a later Resume needs: the agent's session, and whatever the runtime holds for the claim. The
	// relaunches that replace a live process — prompt retirement and the registration deadline —
	// retire it with Suspend first; a relaunch after a death calls none, since the process is gone
	// and Spawn and Resume never start a second process on the claim.
	Suspend(ctx context.Context, loc Locator) error
	// Release ends the claim, whether or not a process runs for it. The process k.Locator records,
	// while it is still the claim's, is sent a shutdown frame over the claim's connection and given
	// the runtime's stop grace to end itself; then it, and whatever else the runtime holds for the
	// claim, are gone. k.Locator is nil when no process runs.
	Release(ctx context.Context, k Known) error
	// Probe is one observation of one locator, now. `Uncertain` is a verdict, not a failure: a
	// returned error means the runtime itself could not be asked, and an error is never a
	// statement about the process.
	Probe(ctx context.Context, loc Locator) (Observation, error)
	// Observe is the periodic sweep of every locator the runtime knows about. The channel closes
	// when ctx ends.
	Observe(ctx context.Context) (<-chan Observation, error)
	// ReconcileOrphans ends what this runtime owns that belongs to no known claim — what a crash
	// between spawning a process and persisting its locator leaves behind. known is every claim the
	// daemon has not retired; the located ones also join the watch.
	ReconcileOrphans(ctx context.Context, known []Known, grace time.Duration) error
	// AdoptWorkingCopy hands the agent's working copy the git identity its commits are authored
	// with, in the place the working copy actually lives (which under a sandbox is not a
	// directory the daemon can see).
	AdoptWorkingCopy(ctx context.Context, loc Locator, id GitIdentity) error
	// ControllerLaunch says which side starts the controller under this runtime.
	ControllerLaunch() ControllerLaunch
}

// Known is one claim as a runtime is told of it — each entry of the orphan sweep's known set, and
// the claim Release ends: the claim, and its process when one runs.
type Known struct {
	Claim claim.Token
	// Locator is the claim's process; nil when none runs — suspended, failed, or never launched.
	Locator *Locator
}

// Validate is the agreement every runtime checks before it acts on a Known: a claim, and, when
// there is a locator, a valid one of that same claim. A pair that disagreed would have a runtime
// end one claim's objects while it stopped another claim's process.
func (k Known) Validate() error {
	if k.Claim == "" {
		return errors.New("known claim: no claim token")
	}
	if k.Locator == nil {
		return nil
	}
	if err := k.Locator.Validate(); err != nil {
		return fmt.Errorf("known claim %s: %w", k.Claim, err)
	}
	if k.Locator.Claim != k.Claim {
		return fmt.Errorf("known claim %s: its locator is claim %s's process", k.Claim, k.Locator.Claim)
	}
	return nil
}

// SpawnSpec is everything a runtime needs to start one agent: which claim it is, what it is
// working on, and the environment, secrets, and prompt it starts with. The claim token travels
// with the process because the runtime addresses the agent's connection by it — `Suspend` and
// `Release` send a shutdown frame over `Conns.Conn(loc.Claim)` — and because a locator without it
// could not be matched to the claim it belongs to.
//
// Nothing in it is a place on one runtime's disk. Repository is the issue's repository
// (`owner/repo`), "" for a configuration with none, and each runtime locates the issue's
// workspace under its own root. Env is the agent's plain variables; Secrets never travel as
// values — each is written to a 0600 file and reaches the process as a `<NAME>_FILE` pointer — and
// carries only what is the claim's to carry: a credential every agent of the deployment shares,
// like the Dispatch bearer, is a runtime option. ResumeSessionFile is set only by `Resume`, and it
// names the transcript the same agent continues from. WorkspaceRecoveredFrom names the ref a
// workspace recreated after its volume was lost is recovered from; "" for every other launch.
type SpawnSpec struct {
	Claim                  claim.Token
	Project                string
	Tree                   string
	Issue                  string
	Role                   claim.Role
	Generation             uint64
	BootToken              string
	Env                    map[string]string
	Secrets                map[string]string
	Prompt                 PromptParts
	Repository             string
	ResumeSessionFile      string
	WorkspaceRecoveredFrom string
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

// WorkspaceLostDetail begins the Detail of a Gone whose process never started because its
// workspace-init found the tree volume lost: neither the shared clone nor the session file it was
// to resume (exit code 3, cmd/legion/workspace_init.go). The session went with the volume, so the
// supervisor relaunches the claim as a fresh session rather than resume one that no longer exists.
const WorkspaceLostDetail = "workspace-lost:"

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
