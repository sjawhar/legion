// Package intake turns durable Envoy notifications and Go API observations into one transactional fact path.
package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Fact is a closed vocabulary of observations that can change Legion's workflow record.
// The unexported method keeps arbitrary external values out of the transaction path.
type Fact interface{ isFact() }

// DispatchIssue records Dispatch's complete issue observation. Rank is Dispatch's fractional key,
// compared as bytes — the order rank.Between generates. HandedOver is resolved once at decode
// (decodeDispatchFact), from the event's own raw labels: every reader acts on this bool, never on
// a label list of its own.
type DispatchIssue struct {
	Key    string
	Seq    int64
	Type   string
	Status string
	Title  string
	Parent string
	Rank   string
	// HandedOver is whether the event's own labels carried dispatch.LegionLabel.
	HandedOver bool
	// ActorSession is the id of a non-daemon session that wrote the event. DaemonActor tells its
	// own status write from a user or an untyped actor: a pending hand-over fence is consumed only
	// by its daemon-authored todo event.
	ActorSession string
	DaemonActor  bool
	// ResolvedRepo is the repository (owner/name) Dispatch resolves the issue to at the event: its
	// own, else its nearest ancestor's, else its project's default; empty when it resolves to none.
	// The workflow takes it when it first records the issue, and never after.
	ResolvedRepo string
	// Blockers is the keys of the issues the event says this issue waits on, in Dispatch's own
	// order. An event retained from a Dispatch that did not yet send them carries none, as an issue
	// waiting on nothing does: no reader needs to tell those two apart.
	Blockers []string
	// ResolvedRepoMissing is an event JetStream retained from before Dispatch resolved an issue's
	// repository: its payload has no resolved_repo, so ResolvedRepo says nothing. The consumer's
	// PrepareRepository fills ResolvedRepo from the issue's record, or from Dispatch for an issue
	// not yet recorded, and clears this; the workflow fills a recorded issue's from its record, and
	// never records an issue from a fact still missing it.
	ResolvedRepoMissing bool
	// Ancestry is the keys of the issue's Dispatch parent chain, from its parent upward to the top,
	// stopping only at a repeated key or back at the issue: the chain release_children's own walk
	// reads. It is read from Dispatch when the fact is prepared, outside the fact lock, for a
	// labelled todo of an unrecorded issue or a recorded root under a parent, recorded or not;
	// empty for any other fact. Admission records it, and keeps such a root out of its waiting line
	// while any live root is on it (record.Waiting).
	Ancestry []string
	// Reread is the Ancestry read again, when an event of an issue the workflow does not record, or
	// a fresh event of a recorded issue whose Dispatch parent differs from its record's, is
	// prepared, of every labelled todo root whose recorded Ancestry holds the issue's key, keyed by
	// that root, for each one the read changes (nil for none now): a person may have moved that
	// very issue, and the root under it has no event of its own. Nil when none changes. Admission
	// writes each before anything else it applies; the workflow reads none of it.
	Reread map[string][]string
	// ClaimedBy is who the observation shows holding the issue's Dispatch claim, `<kind>:<id>`, or
	// "" for nobody, and ClaimKnown says the observation showed the claim at all: an event retained
	// from a Dispatch that did not yet send it, or an event or read whose claim names no actor
	// (dispatch.IssueSummary.ClaimUnread), says nothing of it. blockers.Mirror writes a known claim
	// at the observation's Seq, behind the claim's own fence (record.PutDispatchClaim).
	ClaimedBy  string
	ClaimKnown bool
	// Observed is a fact made from Dispatch's view of an issue the workflow did not record when the
	// fact was prepared (ObservedIssue), not from one event, so no actor is known to have written
	// its state. It reaches only an issue still unrecorded when it applies, and only while no later
	// event of the issue has applied (the mirror's sequence, record.DispatchIssueSeq): ApplyFact,
	// under the fact lock, applies Delivered in its place once the issue is recorded
	// (release_children, or another delivery, recording it meanwhile), or nothing when Delivered is
	// nil, so a state the view shows never reaches a record without the actor of the event that
	// wrote it.
	Observed bool
	// Delivered is the event an Observed fact was prepared in place of, as it was delivered, or the
	// event a route's own status write made (api's recordOwnStatusWrite); nil for a view prepared in
	// place of no event (release_children's view of a child Dispatch already showed todo).
	Delivered *DispatchIssue
	// StatusMove is a status Dispatch confirmed the daemon wrote: the fact an API route applies for
	// its own write (OwnStatusWrite), at the sequence Dispatch answered it with. The workflow takes
	// its status as a move even when the record already shows Dispatch at that status: the record
	// can be behind events the consumer has not delivered (a session's status and the daemon's
	// re-assert of its own), so Dispatch's status at the event before this one is not known, and the
	// edit test (a status equal to the one Dispatch showed before) does not hold for it. Never set by
	// a decoded event.
	StatusMove bool
}

// ObservedIssue is issue as Dispatch's detail shows it now, as an Observed fact at the sequence
// Dispatch has run to: its status, title, parent, rank, hand-over and repository, with no event's
// type or actor, since no one event wrote that state. release_children's todo child and an
// unrecorded issue's admission event are applied from it.
func ObservedIssue(issue dispatch.Issue) DispatchIssue {
	parent := ""
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	return DispatchIssue{Key: issue.Key, Seq: issue.LastSeq, Status: issue.Status, Title: issue.Title, Parent: parent,
		Rank: issue.Rank, HandedOver: issue.HandedOver, ResolvedRepo: issue.ResolvedRepo, Blockers: issue.BlockedBy,
		ClaimedBy: issue.Claim.Holder(), ClaimKnown: !issue.ClaimUnread, Observed: true}
}

// OwnStatusWrite is the event the daemon's own status write makes, as the Dispatch consumer
// decodes it (decodeDispatchEvent): written is the issue Dispatch answered that write with
// (dispatch.Client.SetStatus), the state the event carries, at the event's sequence, and its actor
// is the daemon's own session. An API route that writes a status applies it in the request, so the
// record does not wait for the event to come back through the consumer; the event, at a sequence no
// later than this one, then finds its fence.
func OwnStatusWrite(written dispatch.Issue) DispatchIssue {
	event := ObservedIssue(written)
	event.Observed, event.DaemonActor, event.StatusMove, event.Type = false, true, true, "issue.updated"
	if written.Status == "done" {
		event.Type = "issue.closed"
	}
	return event
}

// ListedIssue is issue as Dispatch's issue listing shows it, as a fact at the issue's sequence, with
// no event's type or actor: what a boot's reconcile records or prepares from.
func ListedIssue(issue dispatch.IssueSummary) DispatchIssue {
	parent := ""
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	return DispatchIssue{Key: issue.Key, Seq: issue.LastSeq, Status: issue.Status, Title: issue.Title, Parent: parent,
		Rank: issue.Rank, HandedOver: issue.HandedOver, ResolvedRepo: issue.ResolvedRepo, Blockers: issue.BlockedBy,
		ClaimedBy: issue.ClaimedBy, ClaimKnown: !issue.ClaimUnread}
}

// Handover is the outbox's direct completion of a hand-over whose Dispatch state is already todo
// with the legion label, so PATCH would make no event. Admission validates the durable fence.
type Handover struct {
	Issue      string
	Tree       string
	Generation uint64
}

func (Handover) isFact() {}

// ChildAdoption is an architect's release of children recorded outside its tree Tree, the root
// key its grant names: each a root that queued in admission before a person made it a child of
// Tree, under the Dispatch parent the release read. Admission adopts every one into Tree or, when
// any cannot join it (workflow's AdoptionRefusal, or Tree is not live), refuses the whole fact and
// writes nothing.
type ChildAdoption struct {
	Tree     string
	Children []AdoptedChild
}

// AdoptedChild is one child a ChildAdoption moves into its tree, with its Dispatch parent.
type AdoptedChild struct {
	Issue  string
	Parent string
}

func (ChildAdoption) isFact() {}

func (DispatchIssue) isFact() {}

// RepoMappingChanged is Dispatch's settings.repo_project.updated on the daemon's own project: a
// repository was mapped to the project, unmapped, moved or made its default. It is no workflow
// fact: the consumer has the daemon read the mapping again (ConsumerSpec.RepositoryMapping) and
// applies nothing.
type RepoMappingChanged struct {
	Project string
}

func (RepoMappingChanged) isFact() {}

// DispatchArtifactKind identifies the three Dispatch artifact observations that affect a gate.
type DispatchArtifactKind string

const (
	DispatchArtifactApproved         DispatchArtifactKind = "approved"
	DispatchArtifactChangesRequested DispatchArtifactKind = "changes_requested"
	DispatchArtifactVersion          DispatchArtifactKind = "version"
)

// DispatchArtifact records one review or version event for a registered design document, from a
// live event or from the daemon's own read of Dispatch's approval (the gate seed). Every source
// applies alike: a key per review, not the source, keeps the architect from hearing one changes
// request twice (the workflow's tellChanges). Mark places the event in the issue's Dispatch event log: the event's
// sequence and Dispatch's time of it, from the event itself, or, for a review, from the read's
// latest review (dispatch.Approval's Seq and At), whichever of them that read gave. A version the
// gate seed read from Dispatch carries none, and says instead whether that read showed the
// document's approval request waiting on the agent (RequestWaitsOnAgent), which is what a newer
// version does to it: a version with neither a mark nor that cannot be placed against a root's
// wait on its gate, and answers none.
type DispatchArtifact struct {
	Key                 string
	ArtifactID          string
	Kind                DispatchArtifactKind
	Version             int
	Reason              string
	Mark                record.EventMark
	RequestWaitsOnAgent bool
}

func (DispatchArtifact) isFact() {}

// GateAboveDispatch is the gate seed of a generation (Generation) finding the document at a version
// below the one the seed was queued at (Above): Read is what the seed read of Dispatch, as the fact
// it would otherwise apply, at Dispatch's latest version. Dispatch's versions only rise and the
// seed reads after the daemon applied whatever moved the gate, so the gate stands above any version
// Dispatch wrote, and is reset to Dispatch's.
type GateAboveDispatch struct {
	Generation uint64
	Above      int
	Read       DispatchArtifact
}

func (GateAboveDispatch) isFact() {}

// DispatchAskOpened is an ask a session or a person opened on an issue, with its kind (Kind:
// record.AskQuestion, record.AskApproval, or "" for a kind this daemon does not know, such as an
// event retained from before Dispatch had the two), the document an approval request asks a person
// to approve (Artifact, "" for a question; every ask.* event of an approval ask carries it), the
// session that opened it (AuthorSession, "" for a person), and its event's place in the issue's
// Dispatch event log (Mark). On a root it opens the ask's mirror row (record.RootAsk).
type DispatchAskOpened struct {
	Key           string
	Ask           string
	Kind          string
	Artifact      string
	AuthorSession string
	Mark          record.EventMark
}

func (DispatchAskOpened) isFact() {}

// DispatchAskRestored is Dispatch's settlement restoring a decision block it retracted: the ask
// opened again (Open), or closed again with the answer given before the retraction. Nobody acted,
// so it moves the ask's mirror row alone and is never a person's act.
type DispatchAskRestored struct {
	Key      string
	Ask      string
	Kind     string
	Artifact string
	Open     bool
	Mark     record.EventMark
}

func (DispatchAskRestored) isFact() {}

// DispatchAskClosed is an answered or resolved ask (State `answered` or `resolved`), its kind and
// document as DispatchAskOpened names them, the actor who closed it, and where Dispatch recorded
// that act (Mark).
type DispatchAskClosed struct {
	Key      string
	Ask      string
	Kind     string
	Artifact string
	State    string
	Actor    dispatch.Actor
	Mark     record.EventMark
}

func (DispatchAskClosed) isFact() {}

// DispatchAskReply is a comment on an ask (comment.created, or comment.answered for a reply sent
// through a delivery's callback), or Dispatch's ask.handed_back of an approval request (Kind
// record.AskApproval and its Artifact; a comment names neither), with whose turn it leaves the ask
// on (WaitingOn: `agent` or `human` on an open ask, a hand-back `human`, and "" on a closed one),
// the actor who made it, and its event's place in the issue's Dispatch event log (Mark).
type DispatchAskReply struct {
	Key       string
	Ask       string
	Kind      string
	Artifact  string
	WaitingOn string
	Actor     dispatch.Actor
	Mark      record.EventMark
}

func (DispatchAskReply) isFact() {}

// DispatchApprovalMoved is an approval request of Artifact that a version of its document moved
// past the version its agent last handed to the person (Dispatch's ask.edited of an approval ask
// whose requested_version is below its version), at Mark: Dispatch shows such a request waiting on
// its agent until the agent hands it back. Nobody replied, so it is never a person's act.
type DispatchApprovalMoved struct {
	Key      string
	Ask      string
	Artifact string
	Mark     record.EventMark
}

func (DispatchApprovalMoved) isFact() {}

// DispatchClaim is the holder Dispatch records for an issue at Seq, from its issue.claimed or
// issue.released event: ClaimedBy is `<kind>:<id>`, or "" when the release cleared the claim.
// blockers.Mirror writes it to the claims mirror (record.PutDispatchClaim) behind the claim's own
// fence, whether or not the issue has a mirrored row yet.
type DispatchClaim struct {
	Key       string
	Seq       int64
	ClaimedBy string
}

func (DispatchClaim) isFact() {}

// DispatchConsumerPosition is one measurement of the Dispatch consumer's own position: its ack
// floor as a stream sequence — the point before which every matching message is acknowledged,
// redeliveries and naks included — and whether it is idle, nothing pending or unacknowledged at
// all. The daemon's boot-owned poll reads one (Consumers.DispatchPosition) for Reconcile, and
// again on a ticker while admission holds anything back, applying each changed reading through
// ApplyFact as a synthetic fact — never decoded from a real Dispatch event. Admission is the only
// handler that acts on it: it releases every hold whose target the position has Reached, applies
// each released key's own listing snapshot, and promotes, all in the same transaction.
type DispatchConsumerPosition struct {
	AckFloorStream int64
	Idle           bool
}

func (DispatchConsumerPosition) isFact() {}

// Reached says whether the consumer at this position has caught up to target, a notification
// stream sequence: its ack floor is at or past it, or it is idle. Idle covers a target the ack
// floor alone can never reach — one set past the consumer's own last matching message, since the
// stream also carries GitHub subjects the Dispatch consumer's filter never matches, or past
// messages that landed before a consumer created at DeliverNewPolicy existed, which it will never
// be given.
func (p DispatchConsumerPosition) Reached(target int64) bool {
	return p.AckFloorStream >= target || p.Idle
}

// IssueStatus is one issue of the daemon's project as Dispatch shows it now: its key, its status,
// the keys it says it is blocked by, and the Dispatch sequence that view stands at. The periodic
// turn's re-read carries them on its ControllerTick, and blockers.Mirror writes each through the
// same sequence-fenced write an event's own row takes, so a view older than an event already
// applied changes nothing. ClaimedBy and ClaimKnown are DispatchIssue's: the issue read behind
// this view shows the claim unless it names no actor.
type IssueStatus struct {
	Key        string
	Status     string
	Blockers   []string
	Seq        int64
	ClaimedBy  string
	ClaimKnown bool
}

// ControllerTick is the daemon's periodic wake for the project's controller, applied as a
// synthetic fact every `controller_wake_interval_seconds` and never decoded from an event.
// Admission is the only handler that acts on it; when it wakes the controller is admission's rule
// (admit.Admission.wakeController and its callers).
type ControllerTick struct {
	// Observed is what the turn's own re-read of Dispatch saw: the issues anything is currently
	// waiting on, as Dispatch answers for them now (workflowRuntime.tickController reads them
	// before the fact is applied, since no handler may perform I/O). The mirror handler writes
	// them, ahead of the handlers that judge from the mirror in this same transaction, so a
	// blocker whose closing event never arrived is caught here — at the cost of one wake interval
	// rather than standing until the next boot's listing. Empty is the tick as it always was: an
	// ordinary wake that writes nothing, which is almost every turn, and also what a failed read
	// leaves.
	Observed []IssueStatus
}

func (ControllerTick) isFact() {}

// PullRequestOpened registers a pull request whose branch or body identifies a Dispatch issue.
type PullRequestOpened struct {
	Repo      string
	Number    int
	Branch    string
	HeadSHA   string
	Body      string
	URL       string
	UpdatedAt time.Time
	// Reopened is GitHub's reopened action; false is opened, which GitHub sends once per pull
	// request.
	Reopened bool
}

func (PullRequestOpened) isFact() {}

// PullRequestSynchronized records a new observed head, including the fields the shipped reducer
// reads to fence stale observations and find its issue.
type PullRequestSynchronized struct {
	Repo      string
	Number    int
	Branch    string
	HeadSHA   string
	Body      string
	UpdatedAt time.Time
}

func (PullRequestSynchronized) isFact() {}

// PullRequestReview is a submitted review on a pull request.
type PullRequestReview struct {
	Repo   string
	Number int
	// ID is GitHub's review id, assigned when the review is created: a pending review (a draft)
	// keeps the id it was created with when it is submitted later. Zero when the listener did not
	// carry it.
	ID int64
	// SubmittedAt is when the review was submitted, zero when the listener did not carry it or
	// carried one that could not be read. Reviews are ordered by it, then by ID
	// (record.ReviewOrder).
	SubmittedAt time.Time
	State       string
	CommitID    string
	HeadSHA     string
	Author      string
	// AuthorCanWrite is whether Author has write access or higher to the repository, as GitHub's
	// collaborator permission answers it (ConsumerSpec.ReviewPermission reads it before the fact is
	// applied, since the workflow decides inside a transaction and performs no I/O). It is never
	// looked up, and stays false, for a review that does not decide (Decides), one with no author,
	// one on a pull request the daemon does not record, and one the review App submitted, which
	// decides by its login and the reviewer's session (workflow's DecidesRound). It is also false
	// for an author GitHub does not give write access.
	AuthorCanWrite bool
	// BodyTruncated is whether Envoy's normalizer capped the review's body before the daemon saw
	// it (`body_truncated: "true"`), which can cut a long review's trailing Legion footer off
	// (LegionSession). ConsumerSpec.ReviewBody restores the full body from GitHub for a truncated
	// review-App review before the fact is applied, and clears this.
	BodyTruncated bool
	Body          string
}

func (PullRequestReview) isFact() {}

// Decides says whether the review's state can decide a review round: an approval or a request for
// changes, in whatever case its producer spelled it. A comment decides nothing, whoever writes it.
// Intake reads the author's permission only for a review that decides, and the workflow lets only
// such a review decide, so both ask this.
func (r PullRequestReview) Decides() bool {
	state := strings.ToLower(r.State)
	return state == "approved" || state == "changes_requested"
}

// legionFooterMarker opens the Legion footer every Legion role appends to a pull-request review:
// a JSON object naming the session and phase that wrote it (skills/legion-worker/SKILL.md, the
// reviewer template prompts/roles/reviewer.md).
const legionFooterMarker = "<!-- legion:"

// LegionSession is the session named in the last Legion footer in the review's body: the session
// field of the JSON object between the last occurrence of legionFooterMarker and the next `-->`.
// It is "" when the body carries no footer, the last one's JSON cannot be parsed, or its session
// is empty. The last occurrence wins, since an earlier footer quoted in the body (a reply, a
// reviewer's own draft) is not the one that closes it.
func (r PullRequestReview) LegionSession() string {
	at := strings.LastIndex(r.Body, legionFooterMarker)
	if at < 0 {
		return ""
	}
	rest := r.Body[at+len(legionFooterMarker):]
	end := strings.Index(rest, "-->")
	if end < 0 {
		return ""
	}
	var footer struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal([]byte(rest[:end]), &footer); err != nil {
		return ""
	}
	return footer.Session
}

// Order is where the review falls among the pull request's reviews (record.ReviewOrder): when it
// was submitted, then GitHub's review id. The workflow orders a deciding review against the pull
// request's newest by it, and the daemon's read of a pull request's reviews orders what GitHub
// lists by it, so both order reviews alike.
func (r PullRequestReview) Order() record.ReviewOrder {
	return record.ReviewOrder{SubmittedAt: r.SubmittedAt, ID: r.ID}
}

// CheckRun is one latest-run identity in a checks settlement.
type CheckRun = record.AttemptRun

// PullRequestChecks is Envoy's settled checks observation: the latest run of each check, and the
// checks the settlement names as failed (any failing conclusion) and as cancelled, kept apart since
// a cancellation is a property of the run - a later push can cancel it - where a failure is of the
// code. What they come to is the base branch's required set's to say (classify.HeadVerdict).
type PullRequestChecks struct {
	Repo       string
	Number     int
	HeadSHA    string
	CheckRuns  []record.AttemptRun
	Generation int64
	Snapshot   string
	Failing    []string
	Cancelled  []string
}

func (PullRequestChecks) isFact() {}

// RequiredChecks is what a pull request's base branch requires, as the daemon read it from GitHub
// with its implement App's installation token (requiredchecks.Required): never decoded from an
// event, applied as a synthetic fact by the daemon's own read of each open pull request
// (workflowRuntime.watchRequiredChecks). Names is the required checks, an empty Names a base that
// requires none. Workflows is each workflow its rulesets require, with its latest run's result on
// WorkflowsHead (requiredchecks.Workflows); both are empty when the base requires no workflow.
type RequiredChecks struct {
	Repo          string
	Number        int
	Names         []string
	Workflows     []record.RequiredWorkflow
	WorkflowsHead string
}

func (RequiredChecks) isFact() {}

// PullRequestMergeability is GitHub's lazily computed verdict for whether a pull request's head
// can be merged into Base without a conflict, decoded from GitHub's nullable `mergeable` field on
// readPullRequest's own /pulls/{number} read, which also names the base branch: never
// decoded from an event, applied as a synthetic fact by the daemon's own read of each open pull
// request (workflowRuntime.readRequiredChecks), before requiredFor's own rulesets and workflow
// reads run. Mergeable is record.MergeabilityUnknown while GitHub is still computing it,
// record.MergeabilityMergeable or record.MergeabilityConflicting once it has.
type PullRequestMergeability struct {
	Repo      string
	Number    int
	Base      string
	Mergeable record.Mergeability
}

func (PullRequestMergeability) isFact() {}

// PullRequestMerged records GitHub's terminal merged observation.
type PullRequestMerged struct {
	Repo     string
	Number   int
	MergeSHA string
}

func (PullRequestMerged) isFact() {}

// PullRequestClosed records GitHub's unmerged close observation: the head the pull request closed
// at, and its updated_at, which orders the close against a reopen and the synchronizes before it.
type PullRequestClosed struct {
	Repo      string
	Number    int
	HeadSHA   string
	UpdatedAt time.Time
}

func (PullRequestClosed) isFact() {}

// MergeFailed is the merge queue taking issue's pull request Number out at Head, the head its
// accepted READY named, because the merge failed (ghautomerge.MergeFailed), as the issue's
// ready_merge outbox row read it from GitHub: never decoded from an event. Reason names the removal
// and each failing run of the merge_group CI on the commit the queue built.
type MergeFailed struct {
	Issue  string
	Number int
	Head   string
	Reason string
}

func (MergeFailed) isFact() {}

// Push records a branch push. Nil ChangedPaths and Truncated preserve an omitted normalized field.
type Push struct {
	Repo   string
	Branch string
	// Before is the head the push replaced, After the head it left.
	Before       string
	After        string
	ChangedPaths *string
	Truncated    *string
	// Forced is the listener's "true" or "false" for whether the push rewrote history; absent from
	// a listener that predates the field.
	Forced *string
	// Pusher is the push's pusher login (the listener's `pusher`, GitHub's pusher.name).
	Pusher string
}

func (Push) isFact() {}

// WorkflowRunCompleted is Envoy's `workflow_run` `completed` envelope: attempt Attempt of GitHub
// Actions run RunID of the workflow file Path (GitHub's `workflow_run.path`, the run's top-level
// `.github/workflows/` file) in Repo finished on HeadBranch, whatever its conclusion.
// Envoy publishes none for a run of a pull request. The workflow compares it with the project's
// deploy workflows (config.Project.Deploys) and queues a GitHub read of the run for each open
// production check it may wake; the envelope is anyone's who holds an Envoy bearer, so nothing in
// it is trusted beyond choosing what to read.
type WorkflowRunCompleted struct {
	Repo       string
	Path       string
	RunID      int64
	Attempt    int
	HeadBranch string
}

func (WorkflowRunCompleted) isFact() {}

// HandoffComplete is an API-originated phase-completion fact.
type HandoffComplete struct {
	Issue string
	Role  claim.Role
	Claim claim.Token
	// Generation is the run whose task the worker took, which the daemon reads from the claim —
	// the delivery whose turn is running, or the run the claim is left serving once that turn
	// ends. A run that was interrupted — a child taken back to todo, its stop refused by the
	// runtime — keeps working and reports a completion of the run that is over; nothing on the
	// worker's own request tells the two apart, since the claim token is stable across runs and
	// the session is kept across a suspend. The route refuses a claim that has taken no task, so
	// a fact reaching the workflow always names a run.
	Generation uint64
	Summary    string
	Verdict    string
	Ready      bool
	Commit     string
	// MergeNote is READY's by-hand note, empty for any other completion: the MANUAL_MERGE_REQUIRED
	// answer the route's READY checks give a pull request whose change touches a path the project's
	// merge.by_hand names. The workflow keeps it with the merger's packet and tells it at its head.
	MergeNote string
	// RecordedHead and RecordedHeadAt are the head the daemon's record held for the issue's pull
	// request when the route read it for READY, and the record's clock then
	// (record.PullRequest.HeadUpdatedAt). The record follows GitHub only by webhook and the
	// daemon's two-minute read of each pull request, so it can lag:
	// the workflow accepts READY while the record is still exactly that snapshot, or already at
	// Commit, and otherwise refuses it, since a push landed in between. Both fields are compared, so
	// a record that left this head and came back to it (a force push and its undo) is a moved one.
	RecordedHead   string
	RecordedHeadAt time.Time
	// HeadAt is GitHub's updated_at for the pull request in the route's read, which showed Commit
	// as its head. A READY the workflow records advances the record to Commit at this clock, so a
	// synchronize older than it, delivered late or again, changes nothing (classify.LateLifecycle).
	HeadAt time.Time
}

func (HandoffComplete) isFact() {}

// BackwardMove asks the workflow to return to an earlier phase.
type BackwardMove struct {
	Issue     string
	Requester claim.Role
	To        phase.Phase
	Reason    string
}

func (BackwardMove) isFact() {}

// RetryOrEscalateDecision is the architect's response to a held phase.
type RetryOrEscalateDecision string

const (
	RetryDecision    RetryOrEscalateDecision = "retry"
	EscalateDecision RetryOrEscalateDecision = "escalate"
)

// RetryOrEscalate is an API-originated response to a held phase.
type RetryOrEscalate struct {
	Issue    string
	Decision RetryOrEscalateDecision
}

func (RetryOrEscalate) isFact() {}

// SignOff records the architect's post-production-check approval.
type SignOff struct{ Issue string }

func (SignOff) isFact() {}

// GateRegistered records a design document and its current version, with what register_gate read
// of Dispatch's approval of it, so a person's review given before the architect registered is
// applied at registration. ApprovedVersion is the version Dispatch showed approved, set only when
// that approval is of Dispatch's latest version, and 0 otherwise: the gate opens at registration,
// keeping that approval's place in the issue's Dispatch event log (ApprovedMark).
// ChangesRequestedVersion is the version of a changes request Dispatch showed on its latest
// version, with its reason and the review's place in the issue's Dispatch event log
// (ChangesRequestedMark), and 0 otherwise: the gate closes with the architect told.
// DispatchVersion is the document's latest version as that read found it: Version is the caller's,
// and the gate is registered at the newer of the two. Since is the issue's event sequence read
// before the approval, with no time, so every review the read did not show comes after it
// (record.EventMark.After): a review the gate already applied after Since is newer than the read.
// RequestAsk is the approval request the read found open (dispatch.Approval.AskID, "" when none
// was), and RequestWaitsOnAgent says it waited on the agent (a reply or a newer version since it
// was handed to the person): the registration writes that request's mirror row as the read found
// it, at Since.
type GateRegistered struct {
	Issue                   string
	ArtifactID              string
	Version                 int
	ApprovedVersion         int
	ApprovedMark            record.EventMark
	ChangesRequestedVersion int
	ChangesReason           string
	ChangesRequestedMark    record.EventMark
	DispatchVersion         int
	Since                   record.EventMark
	RequestAsk              string
	RequestWaitsOnAgent     bool
}

func (GateRegistered) isFact() {}

// CloseRoot is a tree root's architect ending its tree before the tree's first phase starts, with
// the reason it gives: a human decided no change at the design gate, or the issue is moot.
type CloseRoot struct {
	Issue  string
	Reason string
}

func (CloseRoot) isFact() {}

// Message is what the close posts on the issue: that its architect closed it before its first
// phase, and the reason it gave.
func (f CloseRoot) Message() string {
	return "Closed by its architect before its first phase: " + f.Reason
}

// ClaimFailed is the supervision terminal failure observation. Reason is the persisted cause when
// the supervisor has one, so workflow notices can say what failed after a daemon restart.
type ClaimFailed struct {
	Issue  string
	Role   claim.Role
	Reason string
}

func (ClaimFailed) isFact() {}

// ClaimReady is a claim ready to be prompted, from either of two sources: the supervision
// observation of a launch's ready, its agent having taken its Envoy role and said it can be
// prompted (workflowRuntime.applyTerminal), and a start the outbox runs that finds a tree's root
// architect already running, whose launch has no second ready (the outbox's start). Launch is the
// claim's launch generation the ready belongs to, the running one for the second source.
type ClaimReady struct {
	Issue  string
	Role   claim.Role
	Launch uint64
}

func (ClaimReady) isFact() {}

// TaskConfirmed is a role's agent taking the work the daemon gave it: a turn of its pending
// delivery started (supervise's confirm). The workflow changes nothing on it; admission takes it
// as every other fact, so a root whose wait was held because its architect had not yet taken its
// task is decided here rather than at the next tick (workflow's ReconcileAwaiting).
type TaskConfirmed struct {
	Issue      string
	Role       claim.Role
	Generation uint64
}

func (TaskConfirmed) isFact() {}

// GateRequestFound is the boot catch-up's read of the approval request Dispatch shows open on a
// root's registered spec document (dispatch.Approval.AskID), for a gate whose document the asks'
// mirror holds no approval request of: a gate a daemon of an earlier release registered before the
// mirror existed, or one whose request the daemon never saw an event of. Since is the issue's
// Dispatch sequence read before the approval, and WaitsOnAgent says the request waited on the
// agent. The workflow writes the request's mirror row as the read found it, at Since, when it holds
// none; when the gate is closed and armed and the request waits on the person, that is the
// architect's hand-back of the spec, declared as a registration declares it.
type GateRequestFound struct {
	Issue        string
	Generation   uint64
	ArtifactID   string
	RequestAsk   string
	WaitsOnAgent bool
	Since        int64
}

func (GateRequestFound) isFact() {}

// CatchUpApplied is the boot catch-up of a root finishing: every event of its Dispatch event log
// up to Through (0 when the catch-up applied nothing and moves no cursor) has been applied. The
// workflow raises the root's catch-up cursor to Through; admission, which held the root out of its
// decision while its window was applied, decides it at this fact, once.
type CatchUpApplied struct {
	Issue   string
	Through int64
}

func (CatchUpApplied) isFact() {}

// LingerExpired records the generation whose post-close linger elapsed.
type LingerExpired struct {
	Issue      string
	Generation uint64
}

func (LingerExpired) isFact() {}

// DaemonBooted records a boot of the daemon: every stored claim has its machine again, and the
// boot replayed what its claims' terminal states owe the workflow. Boot is the boot's record id
// (daemon_boot), which makes each boot's fact its own. Revision is the daemon's own build commit,
// stamped into a release build and empty outside one, where the boot wakes no production check;
// DispatchBuild is the build commit Dispatch's /healthz reported to the boot, empty when it
// reported none. The workflow compares each against the open checks of the repository the daemon
// is built from (workflow.Config.BuiltFrom), one check_compare row per check.
type DaemonBooted struct {
	Boot          int64
	Revision      string
	DispatchBuild string
}

func (DaemonBooted) isFact() {}

// CheckReport is the implementer's report on one look of its closed issue's production check: the
// `legion` tool's `handoff_complete` on an issue whose check is open (api/handoff.go), carrying no
// commit. Verdict is pass, pending or fail; Summary is what the implementer saw. Look is the look
// the route read as the check's current one, so a report of a look its bound or a hold already
// ended is refused rather than recorded against the look after it.
type CheckReport struct {
	Issue   string
	Claim   claim.Token
	Look    int
	Verdict string
	Summary string
}

func (CheckReport) isFact() {}

// IssueDeployed is Dispatch's `issue.deployed`: its delivery timeline first knew the production
// run that shipped the pull request Repo#Number of issue Issue, RunID, which completed at
// CompletedAt. It wakes the issue's open production check once.
type IssueDeployed struct {
	Issue       string
	Repo        string
	Number      int
	RunID       int64
	CompletedAt time.Time
}

func (IssueDeployed) isFact() {}

// ServerStarted is Dispatch's `server.started`: Dispatch started on the build Commit names. The
// workflow compares the build against the open checks of the repository the daemon is built from,
// as it compares the daemon's own revision at boot; an event with no commit is decoded to none.
type ServerStarted struct {
	Commit string
}

func (ServerStarted) isFact() {}

// CheckWake is a compare that found a reason to wake issue Issue's production check (the
// check_compare outbox row's GitHub read): a build that carries the check's merge, or a deploy
// workflow's job whose named step succeeded after the pull request merged. Look is the look the
// compare was queued for, Cause what asked for it (`daemon boot`, `dispatch start`, `dispatch
// build` or `deploy run`) and Detail the build's commit or the deploy run and its job. OpenedAt is
// the check the executor read the merge from (its OpenedAt, the check instance's identity): the
// workflow ignores a wake of a check a later merge has replaced, and a zero OpenedAt names none.
// The workflow wakes the check by the one wake rule, or keeps the cause for the next look while
// one is open.
type CheckWake struct {
	Issue    string
	Look     int
	Cause    string
	Detail   string
	OpenedAt time.Time
}

func (CheckWake) isFact() {}

// CheckLookEnded is a look of issue Issue's production check ending with no report: Cause is
// `look bound` (the check_look row due at the look's bound), `operator hold` (a check start the
// outbox met an operator's suspension on) or `claim failed` (the implementer's claim failed in
// the look). Look names the look, so a row of a look already ended changes nothing, and OpenedAt
// the check instance (its OpenedAt), so a row of a check a later merge of the same issue replaced
// — whose looks count from one again — changes nothing either; a zero OpenedAt names none.
type CheckLookEnded struct {
	Issue    string
	Look     int
	Cause    string
	OpenedAt time.Time
}

func (CheckLookEnded) isFact() {}

// CheckBound is the whole production check's bound of issue Issue coming due (the check_bound
// row at its bound_at): the check's last look starts, or the bound defers while a look is open.
type CheckBound struct {
	Issue string
}

func (CheckBound) isFact() {}

// Handler changes the workflow record inside the caller-owned transaction.
type Handler interface {
	Apply(context.Context, pgx.Tx, Fact) (Result, error)
}

// Result carries a refusal that is durable: handlers finish and the transaction commits. Duplicate
// reports that the event id was already processed, so no handler ran and nothing changed.
type Result struct {
	Refusal   *Refusal
	Duplicate bool
}

type commitHooksKey struct{}

// WithCommitHooks scopes OnCommit to one transaction: the returned function runs, in order, every
// hook registered on the returned context, and the transaction's owner calls it once the
// transaction has committed, never when it rolls back. ApplyFact is that owner for every fact.
// The hook list is not goroutine-safe: a transaction's handlers run one after another on one
// goroutine, and OnCommit must only be called on that goroutine.
func WithCommitHooks(ctx context.Context) (context.Context, func()) {
	hooks := &[]func(){}
	return context.WithValue(ctx, commitHooksKey{}, hooks), func() {
		for _, hook := range *hooks {
			hook()
		}
	}
}

// OnCommit runs fn once the transaction ctx scopes has committed. Handler state kept in memory,
// and the journal line saying what a fact did, must never claim more than what committed: a later
// handler's failure rolls the transaction back, and a fact retried after a failed commit would say
// it twice. OnCommit panics on a context no WithCommitHooks scopes, since a handler run outside
// ApplyFact has no commit to wait for.
func OnCommit(ctx context.Context, fn func()) {
	hooks, ok := ctx.Value(commitHooksKey{}).(*[]func())
	if !ok {
		panic("intake.OnCommit outside a transaction's commit scope (intake.WithCommitHooks)")
	}
	*hooks = append(*hooks, fn)
}

// CommitScoped reports whether ctx is within a transaction's commit scope (WithCommitHooks): a
// caller that wants a hook to run once the write it just made has committed can tell a transaction
// that will commit from a write made outside any scope, which has no commit to wait for.
func CommitScoped(ctx context.Context) bool {
	_, ok := ctx.Value(commitHooksKey{}).(*[]func())
	return ok
}

// Refusal is a committed API response or JetStream log record, never a transaction failure.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

// ChildInOtherTree is the 409 CHILD_IN_OTHER_TREE refusal of key, a child recorded in the tree of
// tree, when it is asked to join want: it is that tree's, not want's. A release of children and
// the workflow's adoption both answer it.
func ChildInOtherTree(key, tree, want string) *Refusal {
	return &Refusal{Status: 409, Code: "CHILD_IN_OTHER_TREE", Message: fmt.Sprintf("%s runs in the tree of %s, not %s", key, tree, want)}
}

// ChildNotAdoptable is the 409 CHILD_NOT_ADOPTABLE refusal of key, a recorded root, when it is
// asked to join the tree of tree and cannot, for reason. The workflow's adoption and a release of
// children both answer it.
func ChildNotAdoptable(key, tree, reason string) *Refusal {
	return &Refusal{Status: 409, Code: "CHILD_NOT_ADOPTABLE", Message: fmt.Sprintf("%s cannot join the tree of %s: %s", key, tree, reason)}
}

// ApplyFact is the only transaction path for facts, whether they arrived through JetStream, the
// API, or supervision. It records the event before running handlers, skips duplicates before any
// handler runs, remembers the first refusal while completing every handler, and rolls all writes
// back when any handler returns an error. An Observed issue fact is settled first, under the lock
// (settleObserved): the handlers see its view only while the workflow records no such issue.
//
// Facts apply one at a time. Handlers read and rewrite whole records, and admission's slots span
// every tree, so two facts in flight together would each act on what the other is about to change;
// the transaction-scoped advisory lock makes the second wait for the first to commit.
//
// The lock is deliberately one lock for every fact, not one per tree. A per-tree lock does not
// hold: admission's promotion writes the issue record, admission slot and outbox rows of a tree
// other than the fact's — the candidate it promotes off the waiting line — so it would mutate a
// tree whose lock nobody holds, racing that tree's own handler on the same read-modify-write.
// Making per-tree locking correct means promotion taking the candidate tree's lock as well, which
// is cross-tree lock ordering, which is a deadlock a daemon supervising long-lived agents would
// hold until a human noticed. Serial facts are a throughput cost; that is the trade being made.
func ApplyFact(ctx context.Context, pool *pgxpool.Pool, source, eventID string, fact Fact, handlers ...Handler) (Result, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Result{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err := SerializeFacts(ctx, tx); err != nil {
		return Result{}, fmt.Errorf("serialize fact %s/%s: %w", source, eventID, err)
	}

	fresh, err := record.NewStore().MarkProcessed(ctx, tx, source, eventID)
	if err != nil {
		return Result{}, err
	}
	if !fresh {
		if err := tx.Commit(ctx); err != nil {
			return Result{}, err
		}
		committed = true
		return Result{Duplicate: true}, nil
	}

	fact, apply, err := settleObserved(ctx, tx, fact)
	if err != nil {
		return Result{}, err
	}
	if !apply {
		handlers = nil
	}
	ctx, committedHooks := WithCommitHooks(ctx)
	var result Result
	for _, handler := range handlers {
		candidate, err := handler.Apply(ctx, tx, fact)
		if err != nil {
			return Result{}, err
		}
		if result.Refusal == nil && candidate.Refusal != nil {
			result.Refusal = candidate.Refusal
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	committed = true
	committedHooks()
	return result, nil
}

// Applied is which of eventIDs from source ApplyFact has already applied, read in one query and
// outside the fact lock. A fact's processed_events row commits with the fact and is never removed,
// so an id read here as applied is one ApplyFact would only take the lock to skip. A caller that
// replays many facts it has most likely applied before (the boot's terminal replay) applies only
// the rest: the duplicates cost one read between them, not a transaction each queued on the lock
// behind every live fact.
func Applied(ctx context.Context, pool *pgxpool.Pool, source string, eventIDs []string) (map[string]bool, error) {
	applied := make(map[string]bool, len(eventIDs))
	if len(eventIDs) == 0 {
		return applied, nil
	}
	rows, err := pool.Query(ctx, `select event_id from processed_events where source = $1 and event_id = any($2::text[])`, source, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("read which %s facts are applied: %w", source, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read which %s facts are applied: %w", source, err)
	}
	for _, id := range ids {
		applied[id] = true
	}
	return applied, nil
}

// settleObserved is the fact ApplyFact applies in fact's place, under the fact lock, and whether it
// applies one: an Observed issue fact's view, as a plain observation, while the workflow records no
// issue of its key, and once one is recorded the event it was prepared in place of (Delivered),
// carrying the view's Ancestry and Reread, which the preparation read for the view alone,
// or nothing when it was prepared in place of none. Every other fact applies as it is, so no
// handler sees Observed or Delivered.
//
// A view older than an event of the issue the daemon has already applied (the mirror, which every
// applied issue event writes first, holds a later sequence) applies nothing while the issue is
// unrecorded: Dispatch has said something since, which the view does not show — a person's
// withdrawal after the release's write, say — and that later event, applied, recorded no issue.
func settleObserved(ctx context.Context, tx pgx.Tx, fact Fact) (Fact, bool, error) {
	issue, ok := fact.(DispatchIssue)
	if !ok || !issue.Observed {
		return fact, true, nil
	}
	store := record.NewStore()
	recorded, err := store.Issue(ctx, tx, issue.Key)
	if err != nil {
		return nil, false, fmt.Errorf("read the record of %s before applying Dispatch's view of it: %w", issue.Key, err)
	}
	switch {
	case recorded == nil:
		mirrored, found, err := store.DispatchIssueSeq(ctx, tx, issue.Key)
		if err != nil {
			return nil, false, err
		}
		if found && mirrored > issue.Seq {
			return nil, false, nil
		}
		issue.Observed, issue.Delivered = false, nil
		return issue, true, nil
	case issue.Delivered == nil:
		return nil, false, nil
	default:
		delivered := *issue.Delivered
		delivered.Ancestry, delivered.Reread = issue.Ancestry, issue.Reread
		return delivered, true, nil
	}
}

// SerializeFacts takes, for tx's life, the lock every fact applies under (ApplyFact): until tx
// commits or rolls back, no fact resolves a repository and commits an issue recorded on it. The
// boot's reconcile, which records issues in its own transaction, takes it, as does a refresh of
// the repository mapping across its live-record snapshot, its subscription update and its
// publication, so a repository an admission is recording an issue on is never unsubscribed under
// it.
func SerializeFacts(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext('legion.apply_fact'))")
	return err
}
