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
//   - The route widens (dispatch://INFRA-980 is one such widening: `admission.active` listing
//     every issue holding a slot, children included, `admission.waiting` ordered by tree root,
//     `admission.blocked` naming a parent waiting for its children, an `admission.trees` of the
//     live roots, a `tree` on each issue, and a higher DaemonAPIVersion). A reader that refused
//     an unknown field would stop reading the day a daemon rolls onto a wider route, so nothing
//     here uses a strict decode.
//   - Nothing here takes a slot holder for a root: an issue's slot is read from that issue's own
//     `slot`, never from `admission.active`, and the admission block below is recorded as the
//     daemon sent it rather than read as a list of roots.
//   - Tree is optional and stays optional: a daemon that names no tree is read through its
//     issues' parent chain instead.

// State is a daemon's state as Dispatch records it: its issues, its admission block, the settings
// it reports with where each came from, and nothing else of the document.
type State struct {
	Issues map[string]Issue `json:"issues"`
	// Admission is the daemon's admission block, absent from a document that carries none: the
	// Legion board reads the waiting line's own order from it (the spec's "Rule details": that
	// order puts answered roots first and follows Dispatch's rank after them, so Dispatch's rank
	// alone is not it), the blockers each held issue waits on, and the slot cap its header shows.
	Admission *Admission `json:"admission,omitempty"`
	// Settings is the settings the daemon reports, which hold no secret, each value kept as the
	// bytes the daemon sent: the view shows them and Dispatch models none of them, so a key added
	// to legion.yaml is recorded the day a daemon sends it.
	Settings map[string]json.RawMessage `json:"settings,omitempty"`
	// SettingSources is where each setting's value came from, by the same key: the file or the
	// route, who changed it and when (the spec's contract 4). A daemon before that contract sends
	// none, which is no source rather than an empty one.
	SettingSources map[string]json.RawMessage `json:"settingSources,omitempty"`
}

// Admission is the admission block in the members Dispatch records. Every member is optional and
// every other field of the block is ignored, so a daemon that grows one (dispatch://INFRA-980's
// `trees`, which is read here the day a daemon sends it) is recorded, not refused.
type Admission struct {
	// Cap is the deployment's admission cap, the slots the header counts against.
	Cap *int `json:"cap,omitempty"`
	// Active is every issue holding a slot, which a daemon may widen to children as well as
	// roots (dispatch://INFRA-980). Dispatch counts the slots in use from each issue's own
	// `slot` instead.
	Active []string `json:"active,omitempty"`
	// Waiting is the waiting line in the daemon's own order, which the board's Queue column shows
	// without re-sorting.
	Waiting []string `json:"waiting,omitempty"`
	// Trees is the live tree roots, where the daemon names them (dispatch://INFRA-980).
	Trees []string `json:"trees,omitempty"`
	// Blocked is each issue a blocker link holds, with the issues it waits on.
	Blocked []BlockedIssue `json:"blocked,omitempty"`
}

// BlockedIssue is one entry of `admission.blocked`: the issue, and the issues nobody has finished
// that hold it (record.BlockedIssue on the daemon's side).
type BlockedIssue struct {
	Issue     string   `json:"issue"`
	BlockedBy []string `json:"blockedBy,omitempty"`
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
	// RequiresChecks is kept as the bytes the daemon sent, so an observation records whatever a
	// daemon writes and a shape no reader expects is stored rather than dropped. One reader
	// reads it, and it reads a boolean: the CI-wait segment takes true as "the base branch
	// requires a check" and anything else as no check at all
	// (legion/metrics/segments.go's requiresChecks, mirroring `legionPullRequestView` in
	// packages/contracts/src/legion-api.ts).
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
	// FieldTree is the issue's tree root as the daemon named it (dispatch://INFRA-980). It is
	// observed only for an issue carrying one: a daemon that sends none leaves the reader to the
	// parent chain rather than to a guess.
	FieldTree = "tree"
	// FieldAdmission is the daemon's whole admission block, observed once per read under
	// AdmissionKey rather than on any issue: the waiting line's order, the blockers and the cap
	// belong to the project, not to one issue.
	FieldAdmission = "admission"
	// FieldSettings and FieldSettingSources are the settings the daemon reports and where each
	// came from, observed under AdmissionKey beside the admission block: they belong to the
	// project's daemon, not to an issue, and the settings control on the Legion view reads them.
	FieldSettings       = "settings"
	FieldSettingSources = "setting_sources"
	// ClaimFieldPrefix starts the field of one role's claim state: `claim.architect`,
	// `claim.implementer`.
	ClaimFieldPrefix = "claim."
)

// The phases the daemon reports, which is what a FieldPhase observation holds
// (packages/daemon/internal/phase). A reader of the board or of the metrics names them from
// here, so the two never spell one of them differently.
const (
	PhaseAdmitted       = "admitted"
	PhaseAwaitingPerson = "awaiting_person"
	PhasePlanning       = "planning"
	PhaseImplementing   = "implementing"
	PhaseTesting        = "testing"
	PhaseReviewing      = "reviewing"
	PhaseAccepting      = "accepting"
	PhaseRetro          = "retro"
	PhaseMerging        = "merging"
	PhaseAwaitingMerge  = "awaiting_merge"
	PhaseDone           = "done"
	PhaseHeld           = "held"
)

// MergerPhases are the daemon's three phases the merger works, in the order the workflow runs
// them. Every reader groups them as one — the board's Merging column (api/legion_board.go), the
// merger's bar of metric 2, the word a table names a phase by (`merging`) and the send-back out
// of the merger's phase (legion/metrics/compute.go) — so the grouping is declared here, beside
// the phases themselves, and not again in each of them.
var MergerPhases = []string{PhaseRetro, PhaseMerging, PhaseAwaitingMerge}

// IsMergerPhase is whether the daemon's phase is one the merger works (MergerPhases).
func IsMergerPhase(phase string) bool {
	return slices.Contains(MergerPhases, phase)
}

// JSONString is a JSON text's string value, or "" for a null, a number, or anything else: an
// observation's value is whatever the daemon sent, so a reader of one names what it expects
// rather than trusting the shape.
func JSONString(raw []byte) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// AdmissionKey is the issue key every project-level observation is written under — the admission
// block, the settings and their sources: the empty string, which no issue key can be
// (legion_observations.issue_key is free text with no foreign key, so the rows cost no issue).
const AdmissionKey = ""

// Observation is one field of one issue as a read found it, the value JSON-encoded as
// legion_observations holds it.
type Observation struct {
	IssueKey string
	Field    string
	Value    json.RawMessage
}

// Observations is every observed field of every issue the read carries, by issue key and then by
// field, so two reads of one state produce one list. The project's own three come first, under
// AdmissionKey: the admission block, the settings the daemon reports and where each came from,
// none of which belongs to an issue.
//
// Phase, status, slot, the wait and the pull request's three fields are observed on every issue,
// `null` where the issue carries none, so a slot released or a wait answered is a change like any
// other; a first `null` writes no row (store.go's insert). A claim is observed for each role the
// read names — the architect and each worker — and a tree only for an issue the daemon named one
// for, since a field the daemon does not name has not changed: it is simply not this daemon's.
func (s State) Observations() []Observation {
	keys := slices.Sorted(maps.Keys(s.Issues))
	observations := make([]Observation, 0, len(keys)*8+3)
	for _, project := range []struct {
		field string
		value any
	}{
		{FieldAdmission, s.Admission},
		{FieldSettings, s.Settings},
		{FieldSettingSources, s.SettingSources},
	} {
		observations = append(observations, Observation{
			IssueKey: AdmissionKey, Field: project.field, Value: encodeValue(project.value),
		})
	}
	for _, key := range keys {
		issue := s.Issues[key]
		add := func(field string, value any) {
			observations = append(observations, Observation{IssueKey: key, Field: field, Value: encodeValue(value)})
		}
		add(FieldPhase, issue.Phase)
		add(FieldStatus, issue.Status)
		if issue.Tree != nil {
			add(FieldTree, optional(*issue.Tree))
		}
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
