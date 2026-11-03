package legion

import (
	"encoding/json"
	"maps"
	"slices"
)

// The Go mirror of the part of a Legion daemon's `GET /legion/v1/state` Dispatch records. The
// daemon owns the wire shape (packages/daemon/internal/api/state.go, mirrored for TypeScript
// readers in packages/contracts/src/legion-api.ts); this names only the fields Dispatch observes
// and lets encoding/json ignore every other one, which is what makes the decode loose:
//
//   - dispatch://INFRA-980 is about to widen that route — `admission.active` will list every
//     issue holding a slot, children included, `admission.waiting` will order by tree root,
//     `admission.blocked` will also list a parent waiting for its children, a new
//     `admission.trees` will list the live roots, each issue gains a `tree` naming its root, and
//     DaemonAPIVersion goes from 46 to 47. A reader that refused an unknown field would stop
//     reading the day a daemon rolls onto it, so nothing here uses a strict decode.
//   - Nothing here assumes a slot holder is a root: an issue's slot is read from that issue's own
//     `slot`, never from `admission.active`, which is why this type names no admission at all.
//   - Tree is optional and stays optional: a daemon before INFRA-980 sends none.

// State is a daemon's state as Dispatch records it: its issues, and nothing else of the document.
type State struct {
	Issues map[string]Issue `json:"issues"`
}

// Issue is one issue of the daemon's record, in the fields Dispatch observes.
type Issue struct {
	Key    string `json:"key"`
	Phase  string `json:"phase"`
	Status string `json:"status"`
	// Tree is the issue's tree root (dispatch://INFRA-980), absent from every daemon before it: a
	// pointer so "the daemon sent no tree field" and "the daemon named a root" stay apart.
	Tree *string `json:"tree,omitempty"`
	// Slot is the admission slot the issue holds, absent while it holds none.
	Slot *Slot `json:"slot,omitempty"`
	// WaitingOn is what a root in awaiting_person waits on from a person, kept as the bytes the
	// daemon sent: Dispatch records the wait, it does not model it, so a field added to it is
	// recorded rather than dropped.
	WaitingOn   json.RawMessage   `json:"waitingOn,omitempty"`
	Architect   *Claim            `json:"architect,omitempty"`
	Workers     map[string]Worker `json:"workers,omitempty"`
	PullRequest *PullRequest      `json:"pullRequest,omitempty"`
}

// Slot is the admission slot an issue holds.
type Slot struct {
	Index int `json:"index"`
}

// Claim is one role's claim, in the one field Dispatch observes: where it is in its life
// (supervise.ClaimState).
type Claim struct {
	State string `json:"state"`
}

// Worker is one phase worker of an issue, keyed by its role in Issue.Workers.
type Worker struct {
	Claim Claim `json:"claim"`
}

// PullRequest is the issue's pull request as the daemon observes it from GitHub.
type PullRequest struct {
	State         string `json:"state"`
	ChecksVerdict string `json:"checksVerdict,omitempty"`
	// RequiresChecks is kept as the bytes the daemon sent, with no shape assumed: no daemon
	// released today carries it, and Dispatch records it the moment one does rather than
	// guessing now whether it is a boolean, a list or an object.
	RequiresChecks json.RawMessage `json:"requiresChecks,omitempty"`
}

// The fields a read observes, one row of legion_observations each where the value moved.
const (
	FieldPhase                     = "phase"
	FieldStatus                    = "status"
	FieldSlot                      = "slot"
	FieldWaitingOn                 = "waiting_on"
	FieldPullRequestState          = "pull_request.state"
	FieldPullRequestChecksVerdict  = "pull_request.checks_verdict"
	FieldPullRequestRequiresChecks = "pull_request.requires_checks"
	// ClaimFieldPrefix starts the field of one role's claim state: `claim.architect`,
	// `claim.implementer`.
	ClaimFieldPrefix = "claim."
)

// Observation is one field of one issue as a read found it, the value JSON-encoded as
// legion_observations holds it.
type Observation struct {
	IssueKey string
	Field    string
	Value    json.RawMessage
}

// Observations is every observed field of every issue the read carries, by issue key and then by
// field, so two reads of one state produce one list.
//
// Phase, status, slot, the wait and the pull request's three fields are observed on every issue,
// `null` where the issue carries none, so a slot released or a wait answered is a change like any
// other; a first `null` writes no row (store.go's insert). A claim is observed for each role the
// read names — the architect and each worker — since a role the daemon does not name has not
// changed, it is simply not this phase's.
func (s State) Observations() []Observation {
	keys := slices.Sorted(maps.Keys(s.Issues))
	observations := make([]Observation, 0, len(keys)*8)
	for _, key := range keys {
		issue := s.Issues[key]
		add := func(field string, value any) {
			observations = append(observations, Observation{IssueKey: key, Field: field, Value: encodeValue(value)})
		}
		add(FieldPhase, issue.Phase)
		add(FieldStatus, issue.Status)
		var slot any
		if issue.Slot != nil {
			slot = issue.Slot.Index
		}
		add(FieldSlot, slot)
		add(FieldWaitingOn, rawOrNil(issue.WaitingOn))
		if issue.Architect != nil {
			add(ClaimFieldPrefix+"architect", optional(issue.Architect.State))
		}
		for _, role := range slices.Sorted(maps.Keys(issue.Workers)) {
			add(ClaimFieldPrefix+role, optional(issue.Workers[role].Claim.State))
		}
		var state, verdict, requires any
		if pull := issue.PullRequest; pull != nil {
			state, verdict, requires = optional(pull.State), optional(pull.ChecksVerdict), rawOrNil(pull.RequiresChecks)
		}
		add(FieldPullRequestState, state)
		add(FieldPullRequestChecksVerdict, verdict)
		add(FieldPullRequestRequiresChecks, requires)
	}
	return observations
}

// encodeValue is value as the jsonb column holds it: its JSON, or `null` for a value that does not
// encode, since a column that takes no half-written value is better left saying the read found
// nothing than refusing every other field of the issue.
func encodeValue(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return encoded
}

// optional is a string field's value, or nil where the daemon left it out: every one of these is
// `omitempty` on the wire, so an empty string is an absent field rather than a value.
func optional(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// rawOrNil is a raw JSON field's value, or nil where the daemon left it out.
func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
