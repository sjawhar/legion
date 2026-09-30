// Package intake turns durable Envoy notifications and Go API observations into one transactional fact path.
package intake

import (
	"context"
	"fmt"
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
	// ActorSession is the id of the session that wrote the event, when a session did; empty for a
	// user or any other actor kind.
	ActorSession string
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

// ControllerTick is the daemon's periodic wake for the project's controller, applied as a
// synthetic fact every `controller_wake_interval_seconds` and never decoded from an event.
// Admission is the only handler that acts on it: while a slot stands free and a controller is
// registered, it wakes the controller to walk for work and post the day's report.
type ControllerTick struct{}

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
	Body        string
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

// PullRequestClosed records GitHub's unmerged close observation: the head the pull request closed
// at, and its updated_at, which orders the close against a reopen and the synchronizes before it.
type PullRequestClosed struct {
	Repo      string
	Number    int
	HeadSHA   string
	UpdatedAt time.Time
}

func (PullRequestClosed) isFact() {}

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

// ClaimFailed is the supervision terminal failure observation.
type ClaimFailed struct {
	Issue string
	Role  claim.Role
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

// LingerExpired records the generation whose post-close linger elapsed.
type LingerExpired struct {
	Issue      string
	Generation uint64
}

func (LingerExpired) isFact() {}

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
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext('legion.apply_fact'))"); err != nil {
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
