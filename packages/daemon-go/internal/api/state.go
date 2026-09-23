// Package api serves the Go daemon's HTTP surface and owns the wire shape of its state.
//
// The shape here is designed from the LEGION-208 spec's Issue record, not ported from the
// TypeScript daemon's `packages/contracts/src/legion-daemon-api.ts` (which still carries the
// tossed worker queue and the Dispatch/GitHub mirror). Go is the source of truth for the wire:
// `packages/contracts/src/legion-go-api.ts` mirrors these types as strict zod objects and is
// held to them by the golden fixture in `packages/contracts/fixtures/daemon-api/`.
package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/phase"
)


// State is the daemon's own facts, and nothing another system owns (spec: State and store).
type State struct {
	Daemon              DaemonInfo           `json:"daemon"`
	Admission           Admission            `json:"admission"`
	Issues              map[string]Issue     `json:"issues"` // the issue record, keyed by issue key
	PendingStatusWrites []PendingStatusWrite `json:"pendingStatusWrites"`
}

// MarshalJSON keeps `issues` an object on the wire: a nil Go map is `null`, which the plugin's
// strict reader refuses.
func (s State) MarshalJSON() ([]byte, error) {
	type wire State
	out := wire(s)
	if out.Issues == nil {
		out.Issues = map[string]Issue{}
	}
	if out.PendingStatusWrites == nil {
		out.PendingStatusWrites = []PendingStatusWrite{}
	}
	return json.Marshal(out)
}

// DaemonInfo is the running daemon itself: which project it coordinates, the schema its store is
// migrated to, and its boot history.
type DaemonInfo struct {
	Project       string    `json:"project"`
	SchemaVersion int       `json:"schemaVersion"`
	Boots         int       `json:"boots"`
	FirstBootAt   time.Time `json:"firstBootAt"`
	StartedAt     time.Time `json:"startedAt"`
}

// Admission is the issue cap and the issues under it, in Dispatch rank order. Concurrency is
// capped on issues, never on workers, so there is no worker queue here.
type Admission struct {
	Cap     int      `json:"cap"`
	Active  []string `json:"active"`
	Waiting []string `json:"waiting"`
}

// MarshalJSON keeps `active` and `waiting` arrays on the wire, empty rather than `null`.
func (a Admission) MarshalJSON() ([]byte, error) {
	type wire Admission
	out := wire(a)
	if out.Active == nil {
		out.Active = []string{}
	}
	if out.Waiting == nil {
		out.Waiting = []string{}
	}
	return json.Marshal(out)
}

type Issue struct {
	Key        string     `json:"key"`
	Generation uint64     `json:"generation"`
	Phase      phase.Phase `json:"phase"`
	// Status is the last Dispatch status the daemon observed for the issue.
	Status      string     `json:"status"`
	Architect   *ClaimView `json:"architect,omitempty"`
	// Workers is keyed by the role that holds the claim; the vocabulary of a claim belongs to
	// `internal/claim`, which the runtime, the worker stream, and the supervisor all speak
	// without importing this package.
	Workers     map[claim.Role]PhaseView `json:"workers"`
	PullRequest *PullRequestView         `json:"pullRequest,omitempty"`
	DesignGate  *GateView                `json:"designGate,omitempty"`
	Slot        *SlotView                `json:"slot,omitempty"`
}

// MarshalJSON keeps `workers` an object on the wire for an issue that has no worker yet.
func (i Issue) MarshalJSON() ([]byte, error) {
	type wire Issue
	out := wire(i)
	if out.Workers == nil {
		out.Workers = map[claim.Role]PhaseView{}
	}
	return json.Marshal(out)
}

// ClaimView is one role claim as the record holds it: the session its agent registered as, where
// it is in its life (`supervise.ClaimState`), and its locator. The locator is the runtime's own
// nested shape — `{"runtime":"tmux","claim":…,"incarnation":…,"tmux":{"window":…,"pane":…}}` or
// the `sandbox` member for a pod — marshalled by the standard library from `runtime.Locator`; the
// process incarnation lives there and nowhere else in the view, and a socket path never does. A
// claim with no process (queued, suspended, failed, retired) has no locator.
type ClaimView struct {
	Session string           `json:"session"`
	State   string           `json:"state"`
	Locator *runtime.Locator `json:"locator,omitempty"`
}


// PhaseView is one phase worker's claim, its committed handoff, and the rounds the phase has run.
type PhaseView struct {
	Claim         ClaimView `json:"claim"`
	HandoffCommit string    `json:"handoffCommit,omitempty"`
	Rounds        int       `json:"rounds"`
}

// PullRequestView is the issue's pull request as the daemon observes it from GitHub.
type PullRequestView struct {
	Number         int    `json:"number"`
	Head           string `json:"head"`
	ChecksVerdict  string `json:"checksVerdict,omitempty"`
	ReviewDecision string `json:"reviewDecision,omitempty"`
	FixAttempts    int    `json:"fixAttempts"`
}

// GateView is the design gate: the spec document, the version Dispatch currently holds, and the
// version a human approved. ApprovedVersion is nil until someone approves one; the gate is open
// exactly when it equals CurrentVersion.
type GateView struct {
	ArtifactID      string `json:"artifactId"`
	CurrentVersion  int    `json:"currentVersion"`
	ApprovedVersion *int   `json:"approvedVersion"`
}

// SlotView is the admission slot the issue occupies and when it took it.
type SlotView struct {
	Index      int       `json:"index"`
	AdmittedAt time.Time `json:"admittedAt"`
}

// PendingStatusWrite is a due Dispatch-status effect the outbox has not finished. The payload is
// intentionally opaque at this boundary: the workflow owns its exact effect shape.
type PendingStatusWrite struct {
	Issue     string          `json:"issue"`
	Payload   json.RawMessage `json:"payload"`
	Attempts  int             `json:"attempts"`
	NextAt    time.Time       `json:"nextAt"`
	LastError string          `json:"lastError,omitempty"`
}

// StateSource maps the daemon's durable and live facts through the transaction the state route
// owns. It does not commit it: the route can refuse one failed projection without publishing a
// partial snapshot.
type StateSource interface {
	State(ctx context.Context, tx pgx.Tx) (State, error)
}

// StateTransactions begins the one snapshot transaction every state response reads through.
type StateTransactions interface {
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}
