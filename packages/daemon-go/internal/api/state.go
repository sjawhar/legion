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
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Phase is the issue's position in the daemon's transition table — the state it sits in, not the
// role working it (`workers` is keyed by role). The daemon advances it from facts it observes;
// no agent chooses it. The order is the transition table's own.
type Phase string

const (
	PhaseAdmitted        Phase = "admitted"
	PhasePlanning        Phase = "planning"
	PhaseImplementing    Phase = "implementing"
	PhaseTesting         Phase = "testing"
	PhaseReviewing       Phase = "reviewing"
	PhaseRetro           Phase = "retro"
	PhaseMerging         Phase = "merging"
	PhaseAwaitingMerge   Phase = "awaiting_merge"
	PhaseProductionCheck Phase = "production_check"
	PhaseDone            Phase = "done"
	PhaseHeld            Phase = "held"
)

// State is the daemon's own facts, and nothing another system owns (spec: State and store).
type State struct {
	Daemon    DaemonInfo       `json:"daemon"`
	Admission Admission        `json:"admission"`
	Issues    map[string]Issue `json:"issues"` // the issue record, keyed by issue key
}

// MarshalJSON keeps `issues` an object on the wire: a nil Go map is `null`, which the plugin's
// strict reader refuses.
func (s State) MarshalJSON() ([]byte, error) {
	type wire State
	out := wire(s)
	if out.Issues == nil {
		out.Issues = map[string]Issue{}
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

// Issue is what the daemon holds per admitted issue (spec: Issue record).
type Issue struct {
	Key        string     `json:"key"`
	Generation uint64     `json:"generation"`
	Phase      Phase      `json:"phase"`
	Architect  *ClaimView `json:"architect,omitempty"`
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

// ProjectClaims is the issue record Stage 2 can answer: every claim filed under the issue it is
// on — the architect's as the issue's architect, every other role's as that role's worker. The
// daemon keeps no issue record of its own until Stage 3, so an issue is here because a claim is on
// it, at `admitted` and generation 0: the phase and the generation are the workflow's to write,
// and nothing at Stage 2 advances either. A locator is validated as it is read back, and one that
// does not validate refuses the projection, naming its claim.
func ProjectClaims(claims []supervise.Claim) (map[string]Issue, error) {
	issues := map[string]Issue{}
	for _, c := range claims {
		if c.Locator != nil {
			if err := c.Locator.Validate(); err != nil {
				return nil, fmt.Errorf("project claim %s: %w", c.Token, err)
			}
		}
		view := ClaimView{Session: c.Session, State: string(c.State), Locator: c.Locator}
		issue, ok := issues[c.Issue]
		if !ok {
			issue = Issue{Key: c.Issue, Phase: PhaseAdmitted}
		}
		if c.Role == claim.RoleArchitect {
			issue.Architect = &view
		} else {
			if issue.Workers == nil {
				issue.Workers = map[claim.Role]PhaseView{}
			}
			issue.Workers[c.Role] = PhaseView{Claim: view}
		}
		issues[c.Issue] = issue
	}
	return issues, nil
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

// StateSource answers the state route. The daemon implements it over its store; the route knows
// nothing else about the daemon.
type StateSource interface {
	State(ctx context.Context) (State, error)
}
