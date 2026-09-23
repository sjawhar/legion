// Package intake turns durable Envoy notifications and Go API observations into one transactional fact path.
package intake

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Fact is a closed vocabulary of observations that can change Legion's workflow record.
// The unexported method keeps arbitrary external values out of the transaction path.
type Fact interface{ isFact() }

// DispatchIssue records Dispatch's complete issue observation. Rank is Dispatch's fractional key,
// compared as bytes — the order rank.Between generates.
type DispatchIssue struct {
	Key    string
	Seq    int64
	Type   string
	Status string
	Title  string
	Parent string
	Rank   string
}

func (DispatchIssue) isFact() {}

// DispatchArtifactKind identifies the three Dispatch artifact observations that affect a gate.
type DispatchArtifactKind string

const (
	DispatchArtifactApproved         DispatchArtifactKind = "approved"
	DispatchArtifactChangesRequested DispatchArtifactKind = "changes_requested"
	DispatchArtifactVersion          DispatchArtifactKind = "version"
)

// DispatchArtifact records one review or version event for a registered design document.
type DispatchArtifact struct {
	Key        string
	ArtifactID string
	Kind       DispatchArtifactKind
	Version    int
	Reason     string
}

func (DispatchArtifact) isFact() {}

// PullRequestOpened registers a pull request whose branch or body identifies a Dispatch issue.
type PullRequestOpened struct {
	Repo      string
	Number    int
	Branch    string
	HeadSHA   string
	Body      string
	URL       string
	UpdatedAt time.Time
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
	Repo     string
	Number   int
	State    string
	CommitID string
	HeadSHA  string
	Author   string
	Body     string
}

func (PullRequestReview) isFact() {}

// CheckRun is one latest-run identity in a checks settlement.
type CheckRun = record.AttemptRun

// PullRequestChecks is Envoy's settled checks observation. Verdict is "red", "green", or empty
// when every check is cancelled; Failing contains the normalized failed check names.
type PullRequestChecks struct {
	Repo       string
	Number     int
	HeadSHA    string
	CheckRuns  []record.AttemptRun
	Generation int64
	Snapshot   string
	Verdict    string
	Failing    []string
	SettledAt  time.Time
}

func (PullRequestChecks) isFact() {}

// PullRequestMerged records GitHub's terminal merged observation.
type PullRequestMerged struct {
	Repo     string
	Number   int
	MergeSHA string
}

func (PullRequestMerged) isFact() {}

// PullRequestClosed records GitHub's terminal, unmerged close observation.
type PullRequestClosed struct {
	Repo   string
	Number int
}

func (PullRequestClosed) isFact() {}

// Push records a branch push. Nil ChangedPaths and Truncated preserve an omitted normalized field.
type Push struct {
	Repo         string
	Branch       string
	After        string
	ChangedPaths *string
	Truncated    *string
}

func (Push) isFact() {}

// HandoffComplete is an API-originated phase-completion fact.
type HandoffComplete struct {
	Issue   string
	Role    claim.Role
	Claim   claim.Token
	Summary string
	Verdict string
	Ready   bool
	Commit  string
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

// GateRegistered records a design document and its current version.
type GateRegistered struct {
	Issue      string
	ArtifactID string
	Version    int
}

func (GateRegistered) isFact() {}

// ClaimFailed is the supervision terminal failure observation.
type ClaimFailed struct {
	Issue string
	Role  claim.Role
}

func (ClaimFailed) isFact() {}

// ClaimReady is the supervision observation that a claimed phase worker is ready.
type ClaimReady struct {
	Issue string
	Role  claim.Role
}

func (ClaimReady) isFact() {}

// Handler changes the workflow record inside the caller-owned transaction.
type Handler interface {
	Apply(context.Context, pgx.Tx, Fact) (Result, error)
}

// Result carries a refusal that is durable: handlers finish and the transaction commits.
type Result struct{ Refusal *Refusal }

// Refusal is a committed API response or JetStream log record, never a transaction failure.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

// ApplyFact is the only transaction path for facts, whether they arrived through JetStream, the
// API, or supervision. It records the event before running handlers, skips duplicates before any
// handler runs, remembers the first refusal while completing every handler, and rolls all writes
// back when any handler returns an error.
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

	fresh, err := record.NewStore().MarkProcessed(ctx, tx, source, eventID)
	if err != nil {
		return Result{}, err
	}
	if !fresh {
		if err := tx.Commit(ctx); err != nil {
			return Result{}, err
		}
		committed = true
		return Result{}, nil
	}

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
	return result, nil
}
