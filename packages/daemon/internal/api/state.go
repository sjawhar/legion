// Package api serves the Go daemon's HTTP surface and owns the wire shape of its state.
//
// The shape here is designed from the LEGION-208 spec's Issue record, not ported from the
// TypeScript daemon's `packages/contracts/src/legion-daemon-api.ts` (which still carries the
// tossed worker queue and the Dispatch/GitHub mirror). Go is the source of truth for the wire:
// `packages/contracts/src/legion-api.ts` mirrors these types as zod objects, which strip a key
// they do not name on the answer side, and is held to them by the golden fixture in
// `packages/contracts/fixtures/daemon-api/`.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Unrecorded is what this route reads for an issue the workflow does not record, where an
// operator's claim exists and an issue does not (LEGION-272). It is not a workflow phase: no
// issue record holds it, the transition table never reaches it, and no role works it — so it
// lives with the wire shape that carries it rather than in the domain's phases.
const Unrecorded phase.Phase = "unrecorded"

// State is the daemon's own facts, and nothing another system owns (spec: State and store).
type State struct {
	Daemon              DaemonInfo           `json:"daemon"`
	Admission           Admission            `json:"admission"`
	Issues              map[string]Issue     `json:"issues"` // the issue record, keyed by issue key
	PendingStatusWrites []PendingStatusWrite `json:"pendingStatusWrites"`
	// HeldOutboxRows is every due lifecycle outbox row held behind its issue's running lifecycle
	// row (record.Store.HeldRows), in the order the store lists them: next_at, then id. Never null;
	// empty while every due lifecycle row runs (contract 28).
	HeldOutboxRows []HeldOutboxRow `json:"heldOutboxRows"`
	// StrandedClaims is every claim nothing will wake (workflow.Stranded), by issue and then
	// role: the trees that will not move again until someone acts. Never null; empty while every
	// claim is running or waits on something that will wake it.
	StrandedClaims []StrandedClaim `json:"strandedClaims"`
	// WakesWaiting is every wake the daemon's pacing holds until its turn
	// (worker_wakes_per_minute), in claim order: the wave a restart, a re-admission or a plugin
	// round raised, as far as it has not gone out. It is the two places a wake waits read as one
	// list — the launches and plugin tasks the machines hold (supervise.ClaimView.WakeWaiting) and
	// the catch-ups waiting on their outbox rows (WakesWaitingOf). Never null; empty while nothing
	// waits, which is every moment outside a wave. With `daemon.startedAt` and each claim's
	// `updateRound` it is what says whether a wave is still running.
	WakesWaiting []WakeWaiting `json:"wakesWaiting"`
	// ControllerLocator is the project's controller, absent until a session registers as it: with
	// the capability `legion controller start` fetched, or, under `controller: daemon`, with the boot
	// token of the daemon's own controller launch.
	ControllerLocator *ControllerLocator `json:"controllerLocator,omitempty"`
	// AgentSecretsLogin is the daemon's own agent-secrets token login
	// (runtime.kubernetes.agent_secrets), absent when the deployment configures
	// no broker.
	AgentSecretsLogin *AgentSecretsLoginView `json:"agentSecretsLogin,omitempty"`
	// Capabilities is the deployment's capability report (capabilities.Deployment.Report), one row
	// per capability in the table's order: what the worker image was probed for, what the live
	// sessions' reports prove (present), fail (open) or have not reported yet (unchecked), what a
	// ruling withholds, and what the deployment's own configuration closes, decides or leaves open
	// — each open deployment row with the legion.yaml line that records a decision. Never null; a
	// gap is reported here, never refused (contract 16).
	Capabilities []CapabilityState `json:"capabilities"`
	// Repositories is the project's repository mapping as the daemon serves it: the owners its
	// roles' credentials span, each repository it serves with its cache, each mapped repository it
	// leaves out with why (a GitHub App not installed on it), and the caches of repositories the
	// mapping no longer names. Never null: a daemon with no workflow serves none.
	Repositories RepositoriesView `json:"repositories"`
	// Settings is the daemon's resolved legion.yaml with every credential, credential source and
	// credential file path left out, keyed as the file is (SettingsOf). Required, never null
	// inside: every collection in it is emitted non-nil (contract 35).
	Settings Settings `json:"settings"`
	// Consumers is the lag of the daemon's durable consumers (ConsumerLagView), read from
	// NATS at the request after the state's transaction commits, so no NATS read holds the
	// snapshot open. Never null: a daemon with no workflow has none (contract 43).
	Consumers []ConsumerLagView `json:"consumers"`
	// wakesWaitingHidden is set on the copy the state route serves a caller whose plugin is older
	// than the contract that added WakesWaiting (api's withoutWakesWaiting, below server.go's
	// wakesWaitingContract): that reader knows no `wakesWaiting` and refuses the key as surely
	// when it is empty as when it names a wake, so MarshalJSON leaves the key off that one body
	// rather than writing it empty, as Admission.treesHidden does for `trees`.
	wakesWaitingHidden bool
}

// ConsumerLagView is one durable consumer's lag as the state shows it (intake.ConsumerLag): its
// name; how many messages wait for it (Waiting); the stream time of the oldest and its age in whole
// seconds at the read, both null when nothing waits or when that message could not be read; when
// the daemon last handled one of its messages, null before the first; whether it is stalled,
// messages waiting and none handled for 60 seconds; and Unavailable, what NATS did not answer or
// refused, absent when every read answered.
type ConsumerLagView struct {
	Name             string     `json:"name"`
	Waiting          uint64     `json:"waiting"`
	OldestAt         *time.Time `json:"oldestAt"`
	OldestAgeSeconds *int64     `json:"oldestAgeSeconds"`
	LastHandledAt    *time.Time `json:"lastHandledAt"`
	Stalled          bool       `json:"stalled"`
	Unavailable      string     `json:"unavailable,omitempty"`
}

// ConsumerLagViewsOf is lags as the state shows them, consumer for consumer; no lags is an empty
// list.
func ConsumerLagViewsOf(lags []intake.ConsumerLag) []ConsumerLagView {
	views := make([]ConsumerLagView, len(lags))
	for i, lag := range lags {
		view := ConsumerLagView{Name: lag.Name, Waiting: lag.Waiting, Stalled: lag.Stalled, Unavailable: lag.Unavailable}
		if !lag.OldestAt.IsZero() {
			oldestAt, age := lag.OldestAt, int64(lag.OldestAge/time.Second)
			view.OldestAt, view.OldestAgeSeconds = &oldestAt, &age
		}
		if !lag.LastHandledAt.IsZero() {
			handledAt := lag.LastHandledAt
			view.LastHandledAt = &handledAt
		}
		views[i] = view
	}
	return views
}

// RepositoriesView is the project's repository mapping as the daemon serves it. Owners, Mapped,
// Refused, WithoutWebhookApp and Caches are sorted, and never null. Caches are the repository
// caches of repositories the mapping no longer names, each kept until its RetainedUntil (contract
// 32). WithoutWebhookApp is each served repository Dispatch's GitHub App is not known to be
// installed on, with why (contract 43).
type RepositoriesView struct {
	Owners            []string                `json:"owners"`
	Mapped            []MappedRepositoryView  `json:"mapped"`
	Refused           []RefusedRepositoryView `json:"refused"`
	WithoutWebhookApp []WebhookAppGapView     `json:"withoutWebhookApp"`
	Caches            []CacheView             `json:"caches"`
}

// MappedRepositoryView is a repository the daemon serves, as GitHub names it, whether it is the
// project's default in Dispatch, and, under the Kubernetes runtime, its repository cache once the
// runtime has made one (absent before, and under tmux).
type MappedRepositoryView struct {
	Repo    string     `json:"repo"`
	Default bool       `json:"default"`
	Cache   *CacheView `json:"cache,omitempty"`
}

// CacheView is a repository cache as the Kubernetes runtime shows it: its repository and Sandbox;
// its state — starting, cloning, ready, failing with the kubelet's reason, or refused, naming the
// Service of its name that is not its own and why — and, while its pod is Ready behind its own
// Service, its address; from its last health read (the runtime's minute tick) the default
// branch's head, when it last fetched GitHub and the bytes it serves, or why its /healthz could not
// be read; and, for a repository the mapping no longer names, until when it is kept (contract 32).
type CacheView struct {
	Repo          string    `json:"repo"`
	Sandbox       string    `json:"sandbox"`
	State         string    `json:"state"`
	Reason        string    `json:"reason,omitempty"`
	Ready         bool      `json:"ready"`
	Address       string    `json:"address,omitempty"`
	Head          string    `json:"head,omitempty"`
	LastFetchAt   time.Time `json:"lastFetchAt,omitzero"`
	Bytes         int64     `json:"bytes,omitzero"`
	HealthError   string    `json:"healthError,omitempty"`
	RetainedUntil time.Time `json:"retainedUntil,omitzero"`
}

// RefusedRepositoryView is a mapped repository the daemon leaves out, as Dispatch maps it, and why.
type RefusedRepositoryView struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

// WebhookAppGapView is a served repository, as Dispatch maps it, that Dispatch's GitHub App is not
// installed on, or whose check failed, and why. The daemon serves it all the same.
type WebhookAppGapView struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason"`
}

// MarshalJSON keeps the five lists arrays on the wire.
func (r RepositoriesView) MarshalJSON() ([]byte, error) {
	type wire RepositoriesView
	out := wire(r)
	if out.Owners == nil {
		out.Owners = []string{}
	}
	if out.Mapped == nil {
		out.Mapped = []MappedRepositoryView{}
	}
	if out.Refused == nil {
		out.Refused = []RefusedRepositoryView{}
	}
	if out.WithoutWebhookApp == nil {
		out.WithoutWebhookApp = []WebhookAppGapView{}
	}
	if out.Caches == nil {
		out.Caches = []CacheView{}
	}
	return json.Marshal(out)
}

// CapabilityState is one row of that report. Status is "present", "unchecked", "withheld",
// "decided" or "open" (the probe's own table also prints "missing" and "reported", which never
// reach the state); Detail is the row's evidence whatever the status; Decision is the operator's
// reason on a decided row; ConfigLine is, on an open deployment row, the legion.yaml line that
// records a decision (`capabilities.decided.<name>: "<reason>"`); an open live row has none.
// Source is, on a deployment row, "configuration" (what legion.yaml alone decides) or "boot"
// (what boot measured); absent on every other row.
type CapabilityState struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Detail     string `json:"detail"`
	Decision   string `json:"decision,omitempty"`
	ConfigLine string `json:"configLine,omitempty"`
	Source     string `json:"source,omitempty"`
}

// CapabilityStatesOf is report as the state shows it, row for row.
func CapabilityStatesOf(report []capabilities.State) []CapabilityState {
	states := make([]CapabilityState, len(report))
	for i, row := range report {
		states[i] = CapabilityState{Name: string(row.Name), Status: row.Status, Detail: row.Detail, Decision: row.Decision, ConfigLine: row.ConfigLine, Source: string(row.Source)}
	}
	return states
}

// ControllerLocator is the record of the session registered as the project's controller: the
// runtime the daemon runs under, the session, and when it registered. External is true for both
// launches, a wire constant since contract 4: the operator's controller has no process the daemon
// can address, and the daemon's own (`controller: daemon`) is a claim whose process the operator
// routes and the claims list show, as any claim's. The operator's is alive while the Envoy listener
// names that session as the controller role's holder within the liveness window
// (`controller.Prober`).
type ControllerLocator struct {
	Runtime      string    `json:"runtime"`
	External     bool      `json:"external"`
	SessionID    string    `json:"sessionId"`
	RegisteredAt time.Time `json:"registeredAt"`
	// PluginContract is the contract the registered session's plugin declared, 0 when none
	// recorded, always emitted.
	PluginContract int `json:"pluginContract"`
}

// ControllerLocatorOf is record as the state shows it under the runtime runtimeName names: the
// registered session's external record, or nil while no session holds the current capability.
func ControllerLocatorOf(runtimeName string, record controller.Record) *ControllerLocator {
	if !record.Registered() {
		return nil
	}
	return &ControllerLocator{Runtime: runtimeName, External: true, SessionID: record.Session, RegisteredAt: record.RegisteredAt, PluginContract: record.PluginContract}
}

// AgentSecretsLoginView is the daemon's own agent-secrets token login's current status
// (agentsecrets.Client.LoginStatus): State is one of "none" (no login has finished yet), "issued",
// or "failed" (the latest login won no credential). Never a key, a token, a launcher credential, or
// why a login failed — those live only in the daemon's process memory and its log.
type AgentSecretsLoginView struct {
	State string `json:"state"`
}

// MarshalJSON keeps `issues` an object and `pendingStatusWrites`, `heldOutboxRows`,
// `strandedClaims`, `wakesWaiting`, `capabilities` and `consumers` arrays on the wire: a nil Go map
// or slice is `null`, which the plugin's reader refuses. The one exception is a state whose
// waiting wakes are hidden, which carries no `wakesWaiting` key at all; that body is marshaled
// through a value whose own nil `wakesWaiting` field, being `omitempty`, shadows the embedded one
// and so is written by nothing, which is how Admission.MarshalJSON leaves off `trees`.
func (s State) MarshalJSON() ([]byte, error) {
	type wire State
	out := wire(s)
	if out.Issues == nil {
		out.Issues = map[string]Issue{}
	}
	if out.PendingStatusWrites == nil {
		out.PendingStatusWrites = []PendingStatusWrite{}
	}
	if out.HeldOutboxRows == nil {
		out.HeldOutboxRows = []HeldOutboxRow{}
	}
	if out.StrandedClaims == nil {
		out.StrandedClaims = []StrandedClaim{}
	}
	if out.Capabilities == nil {
		out.Capabilities = []CapabilityState{}
	}
	if out.Consumers == nil {
		out.Consumers = []ConsumerLagView{}
	}
	if s.wakesWaitingHidden {
		out.WakesWaiting = nil
		return json.Marshal(struct {
			wire
			WakesWaiting []WakeWaiting `json:"wakesWaiting,omitempty"`
		}{wire: out})
	}
	if out.WakesWaiting == nil {
		out.WakesWaiting = []WakeWaiting{}
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
	Cap int `json:"cap"`
	// Active is every issue holding an admission slot, a child as much as a root: the cap counts
	// the issues at work rather than the trees (dispatch://INFRA-980), so one tree with three
	// working children holds three of these and a tree whose every issue only waits holds none.
	Active []string `json:"active"`
	// Waiting is the admission line, in the line's own order: the issues whose work came back and
	// wait for a slot before their worker is started again, oldest first, then the issues whose
	// first phase can start, by their tree root's Dispatch rank. It carries children as well as
	// roots, for the same reason `active` does.
	Waiting []string `json:"waiting"`
	// Trees is the root of every live tree the daemon judges (record.TreeLive), sorted by key: a
	// tree whose lifecycle stands open under the workflow and does not linger, one whose root
	// waits in the line, and one whose root waits on a person. A slot is no longer one per tree,
	// so a running tree can hold no slot at all — its root accepting or awaiting its merge while
	// its children work, or every issue of it waiting — and `active` alone no longer tells a
	// reader such as the controller which trees run. Never null; empty when no tree is live.
	Trees []string `json:"trees"`
	// Blocked is every issue Legion is waiting to start that waits on a Dispatch issue nobody has
	// finished (INFRA-225), by issue key: a waiting root admission skips and a child the workflow
	// holds at `admitted`, together in one list, each with what it still waits on. A blocked issue
	// the line carries is in Waiting too. Never null; empty when nothing waits on a link.
	Blocked []BlockedIssue `json:"blocked"`
	// treesHidden is set on the copy the state route serves a caller whose plugin is older than
	// the contract that added Trees (api's withoutTrees, below server.go's treeContract): that
	// reader knows no `trees` and refuses the key as surely when it is empty as when it names a
	// tree, so MarshalJSON leaves the key off that one body rather than writing it empty.
	treesHidden bool
}

// BlockedIssue is one issue of Blocked and the issues it waits on that are not done. It is
// record.BlockedIssue: the state's `blocked` and a tick controller notice's are the same list
// read two ways, and the row is declared once, under both.
type BlockedIssue = record.BlockedIssue

// MarshalJSON keeps `active`, `waiting`, `trees` and `blocked` arrays on the wire, empty rather
// than `null` — except on an admission whose trees are hidden, which carries no `trees` key at
// all. That body is marshaled through a value whose own nil `trees` field, being `omitempty`,
// shadows the embedded one and so is written by nothing; every other key of the block, including
// one a later contract adds above, is still written from the one place that writes it.
func (a Admission) MarshalJSON() ([]byte, error) {
	type wire Admission
	out := wire(a)
	if out.Active == nil {
		out.Active = []string{}
	}
	if out.Waiting == nil {
		out.Waiting = []string{}
	}
	if out.Blocked == nil {
		out.Blocked = []BlockedIssue{}
	}
	if a.treesHidden {
		out.Trees = nil
		return json.Marshal(struct {
			wire
			Trees []string `json:"trees,omitempty"`
		}{wire: out})
	}
	if out.Trees == nil {
		out.Trees = []string{}
	}
	return json.Marshal(out)
}

type Issue struct {
	Key        string      `json:"key"`
	Generation uint64      `json:"generation"`
	Phase      phase.Phase `json:"phase"`
	// Status is the status the daemon means the issue to have and writes to Dispatch
	// (record.Issue.Status). It is not the status Dispatch shows: that is
	// record.Issue.DispatchStatus, which this state does not carry. The two differ while a status
	// write is queued, and go on differing where one was lost until the status repair puts it
	// back, so a reader who wants the status Dispatch shows must read Dispatch.
	Status string `json:"status"`
	// Tree is the key of the root of the issue's tree, which equals Key on a root. Since a child
	// holds a slot of its own, neither `admission.active` nor `admission.waiting` says any more
	// which tree an issue belongs to, and a reader needs it to tell a child from a root and to
	// find the members of a tree that lingers. Absent on an issue the workflow does not record
	// (an operator's claim, Unrecorded), which stands in no tree.
	Tree string `json:"tree,omitempty"`
	// Repo is the repository (owner/name) the issue targets and DefaultBranch that repository's
	// default branch, both taken when the daemon first recorded the issue and never changed after.
	// Both are absent on an issue the workflow does not record (an operator's claim, Unrecorded).
	Repo          string `json:"repo,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// HoldReason is why a held issue is held, when its hold has one: `escalated` once its architect
	// sent it to the controller. Absent otherwise, and absent while the issue's tree lingers or is
	// closed: the record keeps the reason, and the view shows it again once the tree is re-admitted.
	HoldReason string     `json:"holdReason,omitempty"`
	Architect  *ClaimView `json:"architect,omitempty"`
	// Workers is keyed by the role that holds the claim; the vocabulary of a claim belongs to
	// `internal/claim`, which the runtime, the worker stream, and the supervisor all speak
	// without importing this package.
	Workers     map[claim.Role]PhaseView `json:"workers"`
	PullRequest *PullRequestView         `json:"pullRequest,omitempty"`
	DesignGate  *GateView                `json:"designGate,omitempty"`
	Slot        *SlotView                `json:"slot,omitempty"`
	// WaitingOn is present only while the root is in awaiting_person: what it waits on from a
	// person, when the wait began and, once a person answered, when and by what act.
	WaitingOn *WaitingOnView `json:"waitingOn,omitempty"`
	// Check is the issue's open production check, the work after its close (record.ProductionCheck):
	// present only while the check is open, and absent once it closed or while none was opened.
	Check *CheckView `json:"check,omitempty"`
	// MemoryGroup is the memory group of the issue's tree (memorygroup.Group): `memory` for a
	// tree whose roles run with the project's memory overlays and instructions, `none` for one
	// that runs without them, and absent while nothing has drawn for the tree — a tree first
	// opened before the setting existed, or one the draw never reached. MemoryDrawMode is the
	// project's memory mode in force when that group was drawn (config.MemoryMode's words) and
	// MemoryDrawnAt when it was drawn, both absent with the group. The draw is made once, on the
	// tree's root record, when the tree's lifecycle row is first created, and every issue of the
	// tree shows its root's: an issue's memory group is its tree's (contract 49).
	MemoryGroup    string     `json:"memoryGroup,omitempty"`
	MemoryDrawMode string     `json:"memoryDrawMode,omitempty"`
	MemoryDrawnAt  *time.Time `json:"memoryDrawnAt,omitempty"`
}

// WaitingOnView is what a root in awaiting_person waits on from a person (record.Awaiting). Kind
// is `ask` with Ask, the id of the oldest question of its tree still with the person, or `gate`
// with GateVersion and ArtifactID, the spec version of its registered document a person has not
// reviewed. Since is Dispatch's time of the hand-back the wait began at,
// absent when unknown. AnsweredAt and AnsweredBy are absent until a person's act answers the wait;
// the root then waits at the front of admission's waiting line for its slot.
type WaitingOnView struct {
	Kind        string        `json:"kind"`
	Ask         string        `json:"ask,omitempty"`
	GateVersion int           `json:"gateVersion,omitempty"`
	ArtifactID  string        `json:"artifactId,omitempty"`
	Since       *time.Time    `json:"since,omitempty"`
	AnsweredAt  *time.Time    `json:"answeredAt,omitempty"`
	AnsweredBy  record.Answer `json:"answeredBy,omitempty"`
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
// it is in its life (`supervise.ClaimState`), its locator, and the capability report its session
// sent on a `claims/ready` of its own while it ran (LEGION-663, INFRA-927). The locator is the
// runtime's own nested shape —
// `{"runtime":"tmux","claim":…,"incarnation":…,"tmux":{"window":…,"pane":…}}` or the `sandbox`
// member for a pod — marshalled by the standard library from `runtime.Locator`; the process
// incarnation lives there and in the capability report's `incarnation`, which is the reporting
// process's: a reader compares it with `locator.incarnation` to tell the running process's report
// from a predecessor's. A socket path never appears in the view. A claim with no process (queued,
// suspended, failed, retired) has no locator; a claim whose session has not reported, or whose
// role never reports (the controller), has no capabilities.
type ClaimView struct {
	Session      string                `json:"session"`
	State        string                `json:"state"`
	Locator      *runtime.Locator      `json:"locator,omitempty"`
	Capabilities *CapabilityReportView `json:"capabilities,omitempty"`
	// PluginContract is the contract the claim's current registration declared, 0 when none
	// recorded, always emitted.
	PluginContract int `json:"pluginContract"`
	// UpdateRound is the update round the claim is in, and absent when none is open: the pod's
	// drift round (`pod:<pod uid>`), which every claim of that pod shares, or the claim's own
	// plugin round (`plugin:<daemon contract>`). Once its worker has answered, the process holding
	// this claim is waiting to be replaced and the daemon holds the claim's next task. UpdateHead
	// is the commit the worker reported as the head of its issue branch at origin when it answered
	// the round, and is absent until it answers and for a worker that reported none (a planner
	// before its first push). It is the worker's word, which nothing verifies.
	UpdateRound string `json:"updateRound,omitempty"`
	UpdateHead  string `json:"updateHead,omitempty"`
}

// CapabilityReportView is one session's capability report as the state shows it: when the session
// measured, the incarnation of the process that reported, the live rows it proved (OK) and the
// rows it did not, each with the fact its check found (Open). OK and Open are never null.
type CapabilityReportView struct {
	MeasuredAt  time.Time       `json:"measuredAt"`
	Incarnation string          `json:"incarnation"`
	OK          []string        `json:"ok"`
	Open        []CapabilityGap `json:"open"`
}

// CapabilityGap is one live row a session's check failed, with the fact the check found.
type CapabilityGap struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// CapabilityReportViewOf is report as the state shows it: its rows partitioned into the names that
// passed and the gaps that did not, each side in the report's (Table) order.
func CapabilityReportViewOf(report capabilities.Report) *CapabilityReportView {
	view := &CapabilityReportView{
		MeasuredAt:  report.MeasuredAt,
		Incarnation: report.Locator.Incarnation,
		OK:          []string{},
		Open:        []CapabilityGap{},
	}
	for _, row := range report.Rows {
		if row.OK {
			view.OK = append(view.OK, string(row.Name))
		} else {
			view.Open = append(view.Open, CapabilityGap{Name: string(row.Name), Detail: row.Detail})
		}
	}
	return view
}

// PhaseView is one phase worker's claim, the handoff of its current phase — the commit its
// completion reported, the Dispatch handoff record it named — and the rounds the phase has run.
type PhaseView struct {
	Claim         ClaimView          `json:"claim"`
	HandoffCommit string             `json:"handoffCommit,omitempty"`
	HandoffRecord *HandoffRecordView `json:"handoffRecord,omitempty"`
	Rounds        int                `json:"rounds"`
}

// HandoffRecordView is the Dispatch handoff record a worker's completion of its current
// file-backed phase named: the record's id, and its version among its phase's records, which
// `dispatch handoff-read --issue <KEY> --phase <word> --version <n>` reads.
type HandoffRecordView struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// PullRequestView is the issue's pull request as the daemon observes it from GitHub.
// ReviewDecision is the latest review round's decision (changes_requested or approved), the one
// the workflow ends the round on when the reviewer completes. It shows from the review that
// decided it until the next round opens or a new generation clears it, so a request for changes
// stays through implementing and testing, and an approval through retro and every phase after
// it; absent until a review in a round decides.
// State is whether the pull request is open, merged, or closed unmerged, always one of the three:
// a merged root waits in awaiting_merge for the rest of its tree, so an operator reads the merge
// here rather than guessing at it.
type PullRequestView struct {
	// Repo is the repository (owner/name) the pull request is in, its issue's.
	Repo           string `json:"repo"`
	Number         int    `json:"number"`
	Head           string `json:"head"`
	State          string `json:"state"`
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

// CheckView is an issue's open production check as the state shows it: the repository, pull
// request and merge commit the check looks for in production (MergeCommit is empty for a pull
// request merged before the daemon recorded merge commits), when it opened, the role that works
// it (Role, the one the check recorded from the project's `production_check` when it opened), how
// many looks that role has taken and whether the latest is still open, what woke the latest look
// (LastWake, absent before the first look wakes), when the next look is due (NextWakeAt), and
// whether the last look has started.
//
// NextWakeAt is the next look's time, whichever names it: the moment the checking agent named for
// itself on a pending report, the retry of a look nobody reported, or — where neither stands —
// the check's own bound, the moment its last look starts. The bound stays readable without a
// field of its own: NextWakeAt is the bound whenever no agent's time is set, OpenedAt is here,
// and `production_check_hours` is in the state's settings.
type CheckView struct {
	Repo        string         `json:"repo"`
	PullRequest int            `json:"pullRequest"`
	MergeCommit string         `json:"mergeCommit"`
	OpenedAt    time.Time      `json:"openedAt"`
	Role        string         `json:"role"`
	Looks       int            `json:"looks"`
	LookOpen    bool           `json:"lookOpen"`
	LastWake    *CheckWakeView `json:"lastWake,omitempty"`
	NextWakeAt  time.Time      `json:"nextWakeAt"`
	Last        bool           `json:"last"`
}

// CheckWakeView is what woke a production check's latest look: Kind names the cause (`merge` for
// the look the close opens, `check wake` for one the checking agent's own wake_at or the retry of
// a look nobody reported starts, and `bound` for the last look; a check whose look was woken
// before the daemon stopped reading production can hold one of the causes that are gone), Detail
// the cause's own detail — for `check wake`, the summary the agent's report gave or the retry's
// own words — and At when.
type CheckWakeView struct {
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// PendingStatusWrite is a Dispatch-status effect the outbox has not finished, due now or backing off
// after a failed attempt: NextAt is the earliest it can run, and Attempts and LastError say why it
// is still pending. An issue's writes run in the order they were made, so a write waits behind an
// older unfinished one for the same issue, which is listed too; the list runs oldest first, so it
// shows each issue's writes in that order. The payload is intentionally opaque at this boundary:
// the workflow owns its exact effect shape.
type PendingStatusWrite struct {
	Issue     string          `json:"issue"`
	Payload   json.RawMessage `json:"payload"`
	Attempts  int             `json:"attempts"`
	NextAt    time.Time       `json:"nextAt"`
	LastError string          `json:"lastError,omitempty"`
}

// HeldOutboxRow is a due lifecycle effect its issue's lifecycle lane cannot run yet, because
// another lifecycle row of the issue is running: the lane runs an issue's lifecycle rows one at a
// time, in the order they were queued, so a start, a suspend or a workspace removal waits while an
// earlier one of its issue is under a live lease. WaitedSeconds counts from the moment the row
// became due (its next_at), so a retried row's earlier backoffs are not counted as waiting on its
// sibling; WaitsOn is the id of the running row — or, for a role's start fenced behind its issue's
// branch row, that branch row while it runs.
type HeldOutboxRow struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Issue         string `json:"issue"`
	WaitedSeconds int64  `json:"waitedSeconds"`
	WaitsOn       int64  `json:"waitsOn"`
}

// StrandedClaim is one claim nothing will wake — a worker of its issue's phase, or its tree's
// architect, in a tree that runs — named by its issue, its role and its state; the issue's own
// entry under `issues` carries the rest. Its state, its role and who suspended it say the move
// that brings it back (Move), and that suspender is no field of it: every plugin parses this
// body with the schema of the contract it was built at, and a session whose plugin predates
// INFRA-1048 refuses the whole state over a key it does not know — a daemon that relaunches no
// stale session (spec R10) would break every such pod the moment a claim is stranded. A plugin
// built after it strips the key instead, which loses the field rather than the state, and the
// drift gate in packages/contracts names it at the next golden write. It is served on the
// operator claim routes, which no plugin calls (OperatorClaim.SuspendedBy), so a reader hands
// Move the suspender it holds: the daemon's own boot the claim's (SuspendersOf), `legion state`
// the operator claim list's.
type StrandedClaim struct {
	Issue string     `json:"issue"`
	Role  claim.Role `json:"role"`
	State string     `json:"state"`
}

// Suspenders is who suspended each claim, by the key its issue and role make: the fact a
// stranded claim's move turns on, which the state body does not carry.
type Suspenders map[string]string

// SuspendersOf is who suspended each suspended claim of stranded, as the daemon's own boot reads
// it from the claims themselves. A failed claim carries no suspension.
func SuspendersOf(stranded []supervise.Claim) Suspenders {
	by := make(Suspenders, len(stranded))
	for _, c := range stranded {
		if c.State == supervise.StateSuspended {
			by[strandedKey(c.Issue, c.Role)] = string(c.SuspendedBy)
		}
	}
	return by
}

// SuspendersOfOperatorClaims is who suspended each claim of the operator claim list, the one
// surface that serves it (OperatorClaim.SuspendedBy): how a reader off the daemon, `legion
// state`, names the move of a stranded claim.
func SuspendersOfOperatorClaims(claims []OperatorClaim) Suspenders {
	by := make(Suspenders, len(claims))
	for _, c := range claims {
		by[strandedKey(c.Issue, c.Role)] = c.SuspendedBy
	}
	return by
}

// Of is who suspended c, "" when nobody did or when this lookup does not name it.
func (s Suspenders) Of(c StrandedClaim) string {
	return s[strandedKey(c.Issue, c.Role)]
}

// strandedKey names one claim of a tree in both lookups: an issue runs one claim of a role.
func strandedKey(issue string, role claim.Role) string {
	return issue + " " + string(role)
}

// StrandedMove is the move that brings a stranded claim back (StrandedClaim.Move). Its value is the
// key the boot's `claims nothing will wake` log line lists the claim under.
type StrandedMove string

const (
	StrandedResume           StrandedMove = "resume"
	StrandedSuspendedWorkers StrandedMove = "suspendedWorkers"
	StrandedReleasedNoResume StrandedMove = "releasedNoResume"
	StrandedRetry            StrandedMove = "retry"
	StrandedWorkflowStart    StrandedMove = "needsWorkflowStart"
)

// StrandedMoves is every move, in the order the boot's log line and `legion state` list them.
func StrandedMoves() []StrandedMove {
	return []StrandedMove{StrandedResume, StrandedSuspendedWorkers, StrandedReleasedNoResume, StrandedRetry, StrandedWorkflowStart}
}

// Move is the move that brings c back, given who suspended it (Suspenders.Of, "" for a claim
// nobody suspended or whose suspender the caller cannot read): the one statement of it that the
// boot's log line and `legion state` both group by:
//
//   - StrandedReleasedNoResume, a claim whose idle pod the daemon released and whose wake never
//     brought it back (workflow.Stranded's ten-minute rule): the pod and its disk are gone, so
//     the move is the operator's reading of why the replacement pod never took a turn, not a
//     resume of the claim — a released claim resumes on its next delivery, and the wake that
//     should have resumed it has already been taken.
//   - StrandedResume, a suspended architect: the operator's resume (`legion claims resume`), which
//     a catch-up follows.
//   - StrandedSuspendedWorkers, a suspended phase worker: the move its claim's suspendedBy calls
//     for (the operator claim view): the boot's start once it is marked the daemon's when it was
//     suspended before 0038, the operator's resume when an operator suspended it and it kept its
//     task or its phase start waits, otherwise the operator's delivery of its phase's resume task.
//   - StrandedRetry, a failed architect: the operator's retry (`legion claims retry`), which
//     relaunches its session with fresh budgets, and a catch-up follows.
//   - StrandedWorkflowStart, a failed phase worker: the retry of its held phase — its architect's
//     retry_or_escalate, or the operator's retry (`legion claims retry`, which applies the same
//     retry) — since a failed claim ignores resume and deliver.
func (c StrandedClaim) Move(suspendedBy string) StrandedMove {
	failed := c.State == string(supervise.StateFailed)
	switch {
	case !failed && suspendedBy == string(supervise.SuspendedByReleased):
		return StrandedReleasedNoResume
	case failed && c.Role == claim.RoleArchitect:
		return StrandedRetry
	case failed:
		return StrandedWorkflowStart
	case c.Role == claim.RoleArchitect:
		return StrandedResume
	default:
		return StrandedSuspendedWorkers
	}
}

// StrandedClaimsOf is stranded as the state shows it, claim for claim.
func StrandedClaimsOf(stranded []supervise.Claim) []StrandedClaim {
	claims := make([]StrandedClaim, len(stranded))
	for i, c := range stranded {
		claims[i] = StrandedClaim{Issue: c.Issue, Role: c.Role, State: string(c.State)}
	}
	return claims
}

// StrandedByMove is each stranded claim named `<issue> <role>`, grouped by its Move under
// suspended, each group in stranded's order; every move has a group, empty when no claim takes
// it.
func StrandedByMove(stranded []StrandedClaim, suspended Suspenders) map[StrandedMove][]string {
	grouped := make(map[StrandedMove][]string, len(StrandedMoves()))
	for _, move := range StrandedMoves() {
		grouped[move] = []string{}
	}
	for _, c := range stranded {
		move := c.Move(suspended.Of(c))
		grouped[move] = append(grouped[move], strandedKey(c.Issue, c.Role))
	}
	return grouped
}

// WakeWaiting is one wake the daemon's pacing holds until its turn, as the state names it
// (State.WakesWaiting): the claim it will wake, that claim's issue and role, what kind of wake it
// is, and the moment the pacing gave it, which is when it runs. The kinds are the three lanes the
// daemon paces, declared once where the machines reserve them (supervise.WakeKind) and each named
// on the wire as the line that wake writes names it (supervise's wakeRan, the outbox's):
//
//   - `launch`, a role process the daemon is about to start, held by the claim's machine;
//   - `plugin-task`, the update task a stale-plugin round queues to a session already running,
//     held by the claim's machine too;
//   - `catch-up`, the notice a start writes to an architect already running in place of a launch,
//     which waits on its own outbox row rather than in a machine.
//
// The project controller's own claim is exempt from the pacing and never waits, so it never
// appears. A wake over a claim whose process still runs appears like any other: it is a wake the
// queue holds, whatever the claim does meanwhile.
type WakeWaiting struct {
	Claim claim.Token        `json:"claim"`
	Issue string             `json:"issue"`
	Role  claim.Role         `json:"role"`
	Kind  supervise.WakeKind `json:"kind"`
	DueAt time.Time          `json:"dueAt"`
}

// WakesWaitingOf is every wake still waiting, from both of the places one can wait, in claim order:
// held are the wakes the daemon's machines hold in memory, each as its machine last published it
// (supervise.Machine.WaitingWake) — the launches and the plugin tasks — and queued are the wakes
// that wait on a durable row, which the caller composes from the store — today the paced catch-ups
// (daemon's pacedCatchUps). The two are merged here rather than at either source so that one read
// of the state is one ordered list, whichever lane is holding a wave back: an operator or a
// controller asking whether a wave is still running must not have to know which lane each wake of
// it is in.
//
// The machines are read as the operator's claim listing and the capability report read theirs: a
// reader of every claim never waits on the decision in flight, and reading the wake from the
// published view rather than from a claim's stored state leaves what every other reader of that
// state makes of it untouched. Outside a wave nothing waits, so nothing is allocated while both
// sides are empty; the state's own marshalling is what keeps the empty list an array on the wire
// (State.MarshalJSON). The sort is stable, so two wakes of one claim — a row and a machine's, or
// two rows — keep the order their sources gave them.
func WakesWaitingOf(held []supervise.WaitingWake, queued []WakeWaiting) []WakeWaiting {
	var wakes []WakeWaiting
	for _, w := range held {
		wakes = append(wakes, WakeWaiting{Claim: w.Claim, Issue: w.Issue, Role: w.Role, Kind: w.Kind, DueAt: w.At})
	}
	wakes = append(wakes, queued...)
	slices.SortStableFunc(wakes, func(a, b WakeWaiting) int { return cmp.Compare(a.Claim, b.Claim) })
	return wakes
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
