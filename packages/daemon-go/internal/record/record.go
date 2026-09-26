// Package record owns the durable Postgres-backed facts of Legion's issue workflow.
package record

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
	HeldFrom            *phase.Phase
	LastDispatchSeq     int64
	ReadyPendingVersion *int
}

// PhaseRow is the durable part of a role's work on an issue; live claim facts are joined for state.
type PhaseRow struct {
	Issue         string
	Role          claim.Role
	Claim         claim.Token
	HandoffCommit string
	Rounds        int
	Verdict       string
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
	Issue               string
	Repo                string
	Number              int
	Branch              string
	HeadSHA             string
	HeadUpdatedAt       time.Time
	HeadUpdatedAtSource string
	Verdict             string
	Failing             []string
	FailingStatuses     []string
	FixAttempts         int
	BlockedAttempts     int
	CheckRuns           []AttemptRun
	Generation          int64
	Snapshot            string
	Reconciled          bool
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
	// branch and pull request), its design gate, and each role's handoff, review rounds, and
	// verdict. Each role keeps its claim and its last handoff, so a commit an earlier generation
	// reported is never new again.
	ClearGeneration(ctx context.Context, tx pgx.Tx, issue string) error
	ClearTreeGeneration(ctx context.Context, tx pgx.Tx, tree string) error
	Gate(ctx context.Context, tx pgx.Tx, issue string) (*DesignGate, error)
	PutGate(ctx context.Context, tx pgx.Tx, gate DesignGate) error
	Slots(ctx context.Context, tx pgx.Tx) ([]Slot, error)
	PutSlot(ctx context.Context, tx pgx.Tx, slot Slot) error
	ReleaseSlot(ctx context.Context, tx pgx.Tx, issue string) error
	Enqueue(ctx context.Context, tx pgx.Tx, row OutboxRow) error
	ClaimDue(ctx context.Context, tx pgx.Tx, project string, now time.Time, limit int, leaseFor time.Duration) ([]OutboxRow, error)
	FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error
	RetryOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, nextAt time.Time, lastErr string) error
	PendingStatusWrites(ctx context.Context, tx pgx.Tx, project string) ([]OutboxRow, error)
}

// ParentOf is an observed parent key as a record holds it: nil for none. Dispatch says "no parent"
// with an empty string, and the record says it with a nil pointer.
func ParentOf(key string) *string {
	if key == "" {
		return nil
	}
	return &key
}
