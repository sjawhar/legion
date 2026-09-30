// Package record owns the durable Postgres-backed facts of Legion's issue workflow.
package record

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// AttemptRun is the latest check-run id the daemon observed for one check name.
type AttemptRun struct {
	Name string `json:"name"`
	ID   int64  `json:"id"`
}

// ClassifiedPush is one branch push as its changed paths classify it: a fact about two commits,
// the head it replaced (Before) and the head it left (SHA), true whenever it is learned.
type ClassifiedPush struct {
	SHA string `json:"sha"`
	// Before is the head the push replaced, empty when the push did not say.
	Before      string `json:"before,omitempty"`
	HandoffOnly bool   `json:"handoffOnly"`
	Unknown     string `json:"unknown,omitempty"`
	// ByReviewApp is whether the push was the review App's (its pusher is the review App's bot login).
	ByReviewApp bool `json:"byReviewApp,omitempty"`
	// Forced is a push that rewrote history, or did not say whether it did. Its changed paths list
	// the commits it added since the merge base, not what it did to the head it replaced, so it
	// never carries an approval across.
	Forced bool `json:"forced,omitempty"`
}

// MayChangeCode is whether the push may have changed anything outside .legion/ in the head it
// replaced.
func (p ClassifiedPush) MayChangeCode() bool {
	return !p.HandoffOnly || p.Forced
}

// Issue is one durable workflow record. Status is the last Dispatch status the daemon observed.
type Issue struct {
	Key                 string
	Tree                string
	Project             string
	Title               string
	Parent              *string
	Phase               phase.Phase
	Generation          uint64
	Status              string
	Rank                string
	LingerUntil         *time.Time
	Hold                *Hold
	LastDispatchSeq     int64
	ReadyPendingVersion *int
	// HandedOver is whether the issue carried LegionLabel when Dispatch last showed it: what hands
	// a root, or an orphan admitted as one, to Legion. A child running under its tree needs none.
	HandedOver bool
}

// Hold is a held issue's hold: the phase it left, and why it is held when the hold has a reason.
// An issue whose phase is not held has none, so ending a hold (Issue.Hold = nil) ends its reason.
type Hold struct {
	From   phase.Phase
	Reason HoldReason
}

// HoldReason is why a held issue is held.
type HoldReason string

// HoldEscalated is a hold its architect sent to the controller.
const HoldEscalated HoldReason = "escalated"

// PhaseRow is the durable part of a role's work on an issue; live claim facts are joined for state.
type PhaseRow struct {
	Issue         string
	Role          claim.Role
	Claim         claim.Token
	HandoffCommit string
	Rounds        int
	Verdict       string
	// Summary is what the role reported with its completion. The merger's is its READY packet,
	// which the daemon posts when the issue reaches awaiting_merge.
	Summary string
	// LastHandoff is the carrying commit the role last reported for a file-backed phase. Unlike
	// HandoffCommit it survives the next phase's start, so a completion reporting it again is known
	// to carry no handoff written since.
	LastHandoff string
	// Decision is the review round's decision, on the reviewer's row: the newest review GitHub
	// reported for the round that carried one, kept until the round ends, since the reviewer's
	// completion can come after the review it posted. Nil until a review decides.
	Decision *ReviewDecision
}

// ReviewDecision is what one review decided: its state (changes_requested or approved), its body,
// handed to the next implementer, and the head it was written on, whose code an approval approves.
type ReviewDecision struct {
	State string `json:"state"`
	Body  string `json:"body,omitempty"`
	Head  string `json:"head,omitempty"`
}

// ReviewOrder is where a review falls among the pull request's reviews: when it was submitted, then
// GitHub's review id. The id alone is not enough, since GitHub assigns it when a review is created
// and a draft keeps it when it is submitted later. SubmittedAt is zero for a review a listener that
// predates submitted_at carried, one whose time could not be read, and every mark recorded before
// the field. Among reviews that all carry a time the order does not depend on delivery; with an
// untimed review in play it is not transitive, so the outcome can depend on the order reviews are
// delivered in. No stored mark can recover a time it never had. The order decides a round only
// among reviews that arrive before it ends (workflow's review).
type ReviewOrder struct {
	SubmittedAt time.Time
	ID          int64
}

// After is whether o was submitted after other. Submission times decide when both reviews have one
// and they differ; otherwise the ids do, so a review without a time is ordered by id against any
// other, rather than losing to every review that has one.
func (o ReviewOrder) After(other ReviewOrder) bool {
	if !o.SubmittedAt.IsZero() && !other.SubmittedAt.IsZero() && !o.SubmittedAt.Equal(other.SubmittedAt) {
		return o.SubmittedAt.After(other.SubmittedAt)
	}
	return o.ID > other.ID
}

// PullRequest is the daemon's latest GitHub observation for one issue's pull request.
type PullRequest struct {
	Issue   string
	Repo    string
	Number  int
	Branch  string
	HeadSHA string
	// HeadUpdatedAt is the latest updated_at among the lifecycle observations applied (opened,
	// reopened, synchronize or closed; one with no clock never lowers it): an older one is a late
	// redelivery and changes nothing (classify.LateLifecycle).
	HeadUpdatedAt       time.Time
	HeadUpdatedAtSource string
	// CheckedHead is the head whose CI settlement Verdict, Failing, FailingStatuses, CheckRuns,
	// Generation, Snapshot and Reconciled record: the current head, or a head the current one
	// replaced through pushes that changed only .legion/ (a handoff push starts no CI of its own,
	// so the code head's settlement is the head's), or an earlier head whose settlement no longer
	// counts (classify.HeadVerdict).
	CheckedHead     string
	Verdict         string
	Failing         []string
	FailingStatuses []string
	FixAttempts     int
	BlockedAttempts int
	CheckRuns       []AttemptRun
	Generation      int64
	Snapshot        string
	Reconciled      bool
	// Pushes are the branch's pushes as they were classified: every push that changed only .legion/
	// and said which head it replaced, which carry an approval across, and the pushes whose heads
	// have not arrived yet. A push that may change code is spent once its head arrives.
	Pushes      []ClassifiedPush
	HeadCounted string
	// PlannedRed is whether the newest head that changed a path outside .legion/ was the review
	// App's (the tester's red tests): a red on it is planned, so the next head is not a fix attempt.
	PlannedRed bool
	// ReviewSeen is the newest deciding review (changes requested or approved) GitHub reported for
	// the pull request: a deciding review not after it was submitted before one already processed,
	// and records nothing. A comment decides nothing and leaves it as it is. It lasts as long as the
	// pull request's record: across rounds, a reopen, and a new generation while the pull request
	// is open; a new generation deletes one that is not.
	ReviewSeen ReviewOrder
	State      PullRequestState
}

// PullRequestState is whether a pull request is open, merged, or closed unmerged.
type PullRequestState string

const (
	PullRequestOpen   PullRequestState = "open"
	PullRequestMerged PullRequestState = "merged"
	PullRequestClosed PullRequestState = "closed"
)

// DesignGate records the current document version and the version a human approved, if any.
type DesignGate struct {
	Issue           string
	ArtifactID      string
	LatestVersion   int
	ApprovedVersion *int
}

// Slot is an active admission slot.
type Slot struct {
	Issue      string
	Index      int
	AdmittedAt time.Time
}

// OutboxRow is one effect that has not yet been completed. A claimed row carries a non-empty
// lease token and its expiry; callers must present that exact token to finish or retry it.
type OutboxRow struct {
	ID         int64
	Kind       OutboxKind
	Issue      string
	Payload    json.RawMessage
	Attempts   int
	NextAt     time.Time
	LastError  string
	CreatedAt  time.Time
	LeaseToken string
	LeaseUntil *time.Time
}

// Store contains only transactional operations. The caller owns each transaction so intake can
// atomically deduplicate an event, update the record, and enqueue all of its effects.
type Store interface {
	MarkProcessed(ctx context.Context, tx pgx.Tx, source, eventID string) (fresh bool, err error)
	Issue(ctx context.Context, tx pgx.Tx, key string) (*Issue, error)
	Issues(ctx context.Context, tx pgx.Tx) ([]Issue, error)
	PutIssue(ctx context.Context, tx pgx.Tx, issue Issue) error
	Phases(ctx context.Context, tx pgx.Tx, issue string) ([]PhaseRow, error)
	PutPhase(ctx context.Context, tx pgx.Tx, phase PhaseRow) error
	PullRequest(ctx context.Context, tx pgx.Tx, issue string) (*PullRequest, error)
	PullRequestByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*PullRequest, error)
	PullRequestByNumber(ctx context.Context, tx pgx.Tx, repo string, number int) (*PullRequest, error)
	PutPullRequest(ctx context.Context, tx pgx.Tx, pr PullRequest) error
	// SessionClaimsTree says whether the agent session holds a claim in the tree.
	SessionClaimsTree(ctx context.Context, tx pgx.Tx, tree, session string) (bool, error)
	// ClearGeneration drops the facts one generation of an issue owns: a merged or closed pull
	// request, the fix-attempt counts and planned mark of a still-open one (the next generation runs on the same
	// branch and pull request), its design gate, each role's handoff, review rounds, verdict and
	// summary, and a READY the gate refused. Each role keeps its claim and its last handoff, so a
	// commit an earlier generation reported is never new again.
	ClearGeneration(ctx context.Context, tx pgx.Tx, issue string) error
	ClearTreeGeneration(ctx context.Context, tx pgx.Tx, tree string) error
	Gate(ctx context.Context, tx pgx.Tx, issue string) (*DesignGate, error)
	PutGate(ctx context.Context, tx pgx.Tx, gate DesignGate) error
	Slots(ctx context.Context, tx pgx.Tx) ([]Slot, error)
	PutSlot(ctx context.Context, tx pgx.Tx, slot Slot) error
	ReleaseSlot(ctx context.Context, tx pgx.Tx, issue string) error
	// ControllerRegistered says whether a session holds project's current controller registration
	// (the controllers table), which `legion controller start` makes.
	ControllerRegistered(ctx context.Context, tx pgx.Tx, project string) (bool, error)
	// ControllerNoticePending says whether project's outbox still holds an unpublished controller
	// notice of kind, so a wake is never queued behind an identical one.
	ControllerNoticePending(ctx context.Context, tx pgx.Tx, project string, kind NoticeKind) (bool, error)
	Enqueue(ctx context.Context, tx pgx.Tx, row OutboxRow) error
	ClaimDue(ctx context.Context, tx pgx.Tx, project string, now time.Time, limit int, leaseFor time.Duration) ([]OutboxRow, error)
	FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error
	RetryOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, nextAt time.Time, lastErr string) error
	PendingStatusWrites(ctx context.Context, tx pgx.Tx, project string) ([]OutboxRow, error)
	// WaitingNotices is every notice row of project's issues not yet due at now, oldest first.
	WaitingNotices(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]OutboxRow, error)
	// ExpediteOutbox makes row id due at now if it was due later.
	ExpediteOutbox(ctx context.Context, tx pgx.Tx, id int64, now time.Time) error
	// DropCatchUps deletes every catch-up notice row of issue the outbox still holds, a re-held copy
	// and a row the runner has leased included. A leased row may be mid-publish: the runner executes
	// it from memory, and the notice executor publishes it only while OutboxLeased still finds it,
	// so a dropped row is delivered only when the drop lands during its publish, a duplicate the
	// fresh catch-up follows.
	DropCatchUps(ctx context.Context, tx pgx.Tx, issue string) error
	// OutboxLeased reports whether row id is still in the outbox under leaseToken: false once
	// another transaction deleted it while the runner held it (DropCatchUps).
	OutboxLeased(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) (bool, error)
	// TreeIssues is every issue of tree, the root included, by key.
	TreeIssues(ctx context.Context, tx pgx.Tx, tree string) ([]Issue, error)
	// EarlierNotices is every unfinished notice row of tree's issues written before row id, oldest
	// first: the notices a later one of the tree must not overtake for the architect they share.
	EarlierNotices(ctx context.Context, tx pgx.Tx, tree string, id int64) ([]OutboxRow, error)
	// RoleRun reads one role's run of an issue generation: its queued supervise rows and the claim
	// with token.
	RoleRun(ctx context.Context, tx pgx.Tx, token claim.Token, issue string, role claim.Role, generation uint64) (RoleRun, error)
}

// RoleRun is what the store holds of one role's run of an issue generation: the role's supervise
// rows for the generation still queued, oldest first (finishing deletes a row, so each is one the
// outbox has not run), and this daemon's claim on the role, nil when it has none.
type RoleRun struct {
	Queued []QueuedSupervise
	Claim  *RoleClaim
}

// QueuedSupervise is one queued supervise row: its id, which orders it against the others and
// against the newest start run on the claim, and its request.
type QueuedSupervise struct {
	ID      int64
	Request SuperviseRequest
}

// RoleClaim is a claim as RoleRun reads it: its state, the newest start the outbox ran against it
// (last_start_row), and the task it holds, nil for none, with the task's generation, phase and
// confirmation.
type RoleClaim struct {
	State        supervise.ClaimState
	LastStartRow int64
	Pending      *supervise.Delivery
}

// TreeLingers says whether tree lingers after its close: its root is recorded with a linger
// deadline, which re-admission clears. Linger holds every member where it stood, so nothing in such
// a tree transitions, starts a worker, or records a completion.
func TreeLingers(ctx context.Context, store Store, tx pgx.Tx, tree string) (bool, error) {
	root, err := store.Issue(ctx, tx, tree)
	return root != nil && root.Lingers(), err
}

// Lingers says whether the root lingers after its close: its linger deadline is set, and
// re-admission clears it.
func (i Issue) Lingers() bool { return i.LingerUntil != nil }

// LingersAt says whether the root lingers after the close of its generation: it lingers, and
// re-admission has not moved it to a later generation. A row a linger's expiry wrote acts in that
// linger only, not in the tree's next run nor in a later linger of it.
func (i Issue) LingersAt(generation uint64) bool {
	return i.Lingers() && i.Generation == generation
}

// ParentOf is an observed parent key as a record holds it: nil for none. Dispatch says "no parent"
// with an empty string, and the record says it with a nil pointer.
func ParentOf(key string) *string {
	if key == "" {
		return nil
	}
	return &key
}
