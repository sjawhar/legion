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

// PendingPush is the branch push still waiting to be classified.
type PendingPush struct {
	SHA         string `json:"sha"`
	HandoffOnly bool   `json:"handoffOnly"`
	Unknown     string `json:"unknown,omitempty"`
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
	State               PullRequestState
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
	PutPullRequest(ctx context.Context, tx pgx.Tx, pr PullRequest) error
	DeletePullRequest(ctx context.Context, tx pgx.Tx, issue string) error
	// ClearGeneration drops the facts one generation of an issue owns: a merged or closed pull
	// request, the fix-attempt counts of a still-open one (the next generation runs on the same
	// branch and pull request), its design gate, and each role's handoff, review rounds, and
	// verdict. Each role keeps its claim and its last handoff, so a commit an earlier generation
	// reported is never new again.
	ClearGeneration(ctx context.Context, tx pgx.Tx, issue string) error
	Gate(ctx context.Context, tx pgx.Tx, issue string) (*DesignGate, error)
	PutGate(ctx context.Context, tx pgx.Tx, gate DesignGate) error
	Slots(ctx context.Context, tx pgx.Tx) ([]Slot, error)
	PutSlot(ctx context.Context, tx pgx.Tx, slot Slot) error
	ReleaseSlot(ctx context.Context, tx pgx.Tx, issue string) error
	Enqueue(ctx context.Context, tx pgx.Tx, row OutboxRow) error
	ClaimDue(ctx context.Context, tx pgx.Tx, now time.Time, limit int, leaseFor time.Duration) ([]OutboxRow, error)
	FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error
	RetryOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, nextAt time.Time, lastErr string) error
	PendingStatusWrites(ctx context.Context, tx pgx.Tx) ([]OutboxRow, error)
}
