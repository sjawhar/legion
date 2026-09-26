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

// PendingPush is the branch push still waiting to be classified.
type PendingPush struct {
	SHA         string `json:"sha"`
	HandoffOnly bool   `json:"handoffOnly"`
	Unknown     string `json:"unknown,omitempty"`
	// ByReviewApp is whether the push was the review App's (its pusher is the review App's bot login).
	ByReviewApp bool `json:"byReviewApp,omitempty"`
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
	// Summary is what the role reported with its completion. The merger's is its READY packet,
	// which the daemon posts when the issue reaches awaiting_merge.
	Summary string
	// LastHandoff is the carrying commit the role last reported for a file-backed phase. Unlike
	// HandoffCommit it survives the next phase's start, so a completion reporting it again is known
	// to carry no handoff written since.
	LastHandoff string
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
	ReviewDecision      string
	FixAttempts         int
	BlockedAttempts     int
	CheckRuns           []AttemptRun
	Generation          int64
	Snapshot            string
	Reconciled          bool
	PendingPush         *PendingPush
	HeadCounted         string
	// PlannedRed is whether the newest head that changed a path outside .legion/ was the review
	// App's (the tester's red tests): a red on it is planned, so the next head is not a fix attempt.
	PlannedRed bool
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
	Enqueue(ctx context.Context, tx pgx.Tx, row OutboxRow) error
	ClaimDue(ctx context.Context, tx pgx.Tx, project string, now time.Time, limit int, leaseFor time.Duration) ([]OutboxRow, error)
	FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error
	RetryOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, nextAt time.Time, lastErr string) error
	PendingStatusWrites(ctx context.Context, tx pgx.Tx, project string) ([]OutboxRow, error)
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
// (last_start_row), and whether it holds a task undelivered or unconfirmed, with that task's
// generation and phase.
type RoleClaim struct {
	State             supervise.ClaimState
	LastStartRow      int64
	Pending           bool
	PendingGeneration uint64
	PendingPhase      phase.Phase
}

// TreeLingers says whether tree lingers after its close: its root is recorded with a linger
// deadline, which re-admission clears. Linger holds every member where it stood, so nothing in such
// a tree transitions, starts a worker, or records a completion.
func TreeLingers(ctx context.Context, store Store, tx pgx.Tx, tree string) (bool, error) {
	root, err := store.Issue(ctx, tx, tree)
	if err != nil || root == nil {
		return false, err
	}
	return root.LingerUntil != nil, nil
}

// ParentOf is an observed parent key as a record holds it: nil for none. Dispatch says "no parent"
// with an empty string, and the record says it with a nil pointer.
func ParentOf(key string) *string {
	if key == "" {
		return nil
	}
	return &key
}
