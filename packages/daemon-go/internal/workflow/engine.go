// Package workflow applies the daemon-owned phase transition table inside intake's transaction.
package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Config supplies the project-scoped workflow limits and the clock used only to stamp durable
// outbox deadlines. Zero limits take their shipped defaults.
type Config struct {
	Project        string
	DesignGate     config.DesignGate
	ReviewRoundCap int
	MaxFixAttempts int
	LingerHours    time.Duration
	Clock          func() time.Time
}

// Engine interprets Table and writes only records and outbox rows through the supplied Store.
type Engine struct {
	store record.Store
	cfg   Config
	log   *slog.Logger
}

var _ intake.Handler = (*Engine)(nil)

// New constructs a workflow engine with the shipped review and fix caps when callers omit them.
func New(store record.Store, cfg Config, log *slog.Logger) *Engine {
	if cfg.DesignGate == "" {
		cfg.DesignGate = config.DesignGateRootIssues
	}
	if cfg.ReviewRoundCap <= 0 {
		cfg.ReviewRoundCap = 3
	}
	if cfg.MaxFixAttempts <= 0 {
		cfg.MaxFixAttempts = 3
	}
	if cfg.LingerHours <= 0 {
		cfg.LingerHours = 72 * time.Hour
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{store: store, cfg: cfg, log: log}
}

func (e *Engine) now() time.Time { return e.cfg.Clock().UTC() }

// Apply changes workflow-owned phase state for one durable intake fact. It never performs I/O;
// every externally visible consequence is an outbox row committed with the record change.
func (e *Engine) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	switch fact := fact.(type) {
	case intake.DispatchIssue:
		return e.dispatchIssue(ctx, tx, fact)
	case intake.GateRegistered:
		return e.gateRegistered(ctx, tx, fact)
	case intake.DispatchArtifact:
		return e.dispatchArtifact(ctx, tx, fact)
	case intake.HandoffComplete:
		return e.handoff(ctx, tx, fact)
	case intake.PullRequestOpened:
		return e.pullRequestOpened(ctx, tx, fact)
	case intake.PullRequestSynchronized:
		return e.pullRequestSynchronized(ctx, tx, fact)
	case intake.Push:
		return e.push(ctx, tx, fact)
	case intake.PullRequestChecks:
		return e.checks(ctx, tx, fact)
	case intake.PullRequestReview:
		return e.review(ctx, tx, fact)
	case intake.PullRequestMerged:
		return e.merged(ctx, tx, fact)
	case intake.PullRequestClosed:
		return e.closed(ctx, tx, fact)
	case intake.ClaimFailed:
		return e.claimFailed(ctx, tx, fact)
	case intake.RetryOrEscalate:
		return e.retryOrEscalate(ctx, tx, fact)
	case intake.BackwardMove:
		return e.backward(ctx, tx, fact)
	case intake.SignOff:
		return e.signOff(ctx, tx, fact)
	case intake.LingerExpired:
		return e.lingerExpired(ctx, tx, fact)
	default:
		return intake.Result{}, nil
	}
}

func (e *Engine) dispatchIssue(ctx context.Context, tx pgx.Tx, fact intake.DispatchIssue) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil {
		return intake.Result{}, err
	}
	if issue == nil {
		return e.recordChildUnderLiveTree(ctx, tx, fact)
	}
	// Admission, which runs after this handler, records every newer observation of a recorded
	// issue (its title, rank, parent, and status) and owns a todo's re-admission. The engine only
	// reacts, to a newer status that takes the issue out of the workflow.
	if fact.Seq != 0 && fact.Seq <= issue.LastDispatchSeq || fact.Status == issue.Status {
		return intake.Result{}, nil
	}
	if staleTreeStatus(fact.Status) {
		return intake.Result{}, e.leave(ctx, tx, *issue, fact.Status)
	}
	if fact.Status == "todo" && e.treeKey(ctx, tx, *issue) != issue.Key {
		return intake.Result{}, e.reenterChild(ctx, tx, *issue, fact)
	}
	return intake.Result{}, nil
}

// recordChildUnderLiveTree owns the otherwise unrecorded-child edge from decision 13. Admission
// records only roots and orphans because this runs first in intake.ApplyFact.
func (e *Engine) recordChildUnderLiveTree(ctx context.Context, tx pgx.Tx, fact intake.DispatchIssue) (intake.Result, error) {
	if fact.Status != "todo" || fact.Parent == "" {
		return intake.Result{}, nil
	}
	parent, err := e.store.Issue(ctx, tx, fact.Parent)
	if err != nil || parent == nil {
		return intake.Result{}, err
	}
	root, err := e.store.Issue(ctx, tx, parent.Tree)
	if err != nil || root == nil {
		return intake.Result{}, err
	}
	live, err := e.liveTree(ctx, tx, *root)
	if err != nil || !live {
		return intake.Result{}, err
	}
	return intake.Result{}, e.enterChild(ctx, tx, *root, fact)
}

// reenterChild takes a recorded child set back to todo into a new run under its live tree, the way
// recordChildUnderLiveTree enters an unrecorded one: the child's previous run's facts are cleared,
// and the tree's architect is told. A child whose tree is not live is an orphan, which admission,
// running after this handler, admits as a root of its own.
func (e *Engine) reenterChild(ctx context.Context, tx pgx.Tx, child record.Issue, fact intake.DispatchIssue) error {
	root, err := e.store.Issue(ctx, tx, child.Tree)
	if err != nil || root == nil {
		return err
	}
	live, err := e.liveTree(ctx, tx, *root)
	if err != nil || !live {
		return err
	}
	if err := e.store.ClearGeneration(ctx, tx, child.Key); err != nil {
		return err
	}
	if err := e.enterChild(ctx, tx, *root, fact); err != nil {
		return err
	}
	return e.notice(ctx, tx, child.Key, record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is todo; it runs again under %s", child.Key, root.Key)})
}

// enterChild records a todo child under root's live tree, admitted at the tree's generation, and
// starts its planning when the tree's gate is open.
func (e *Engine) enterChild(ctx context.Context, tx pgx.Tx, root record.Issue, fact intake.DispatchIssue) error {
	parentKey := fact.Parent
	child := record.Issue{Key: fact.Key, Tree: root.Tree, Project: root.Project, Title: fact.Title, Parent: &parentKey, Phase: phase.Admitted,
		Generation: root.Generation, Status: fact.Status, Rank: fact.Rank, LastDispatchSeq: fact.Seq}
	if err := e.store.PutIssue(ctx, tx, child); err != nil {
		return err
	}
	gate, err := e.store.Gate(ctx, tx, root.Key)
	if err != nil {
		return err
	}
	if gate != nil && classify.DesignGateOpen(*gate) {
		return e.transition(ctx, tx, child, TriggerGateOpened, "", record.PhaseRow{}, nil, "")
	}
	return nil
}

func (e *Engine) gateRegistered(ctx context.Context, tx pgx.Tx, fact intake.GateRegistered) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	gate := record.DesignGate{Issue: fact.Issue, ArtifactID: fact.ArtifactID, LatestVersion: fact.Version}
	if e.cfg.DesignGate == config.DesignGateOff {
		gate = classify.ApplyDesignGateEvent(gate, classify.DesignGateApproved, fact.Version)
	}
	if err := e.store.PutGate(ctx, tx, gate); err != nil {
		return intake.Result{}, err
	}
	if err := e.enqueue(ctx, tx, fact.Issue, record.GateSeed{ArtifactID: fact.ArtifactID, Version: fact.Version, Generation: issue.Generation}); err != nil {
		return intake.Result{}, err
	}
	if !classify.DesignGateOpen(gate) {
		return intake.Result{}, nil
	}
	if err := e.notice(ctx, tx, fact.Issue, record.Notice{Kind: "design-approved", Version: fact.Version}); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.advanceAdmittedTree(ctx, tx, *issue, gate)
}

func (e *Engine) dispatchArtifact(ctx context.Context, tx pgx.Tx, fact intake.DispatchArtifact) (intake.Result, error) {
	gate, err := e.store.Gate(ctx, tx, fact.Key)
	if err != nil || gate == nil || gate.ArtifactID != fact.ArtifactID {
		return intake.Result{}, err
	}
	wasOpen := classify.DesignGateOpen(*gate)
	kind := classify.DesignGateEventKind(fact.Kind)
	updated := classify.ApplyDesignGateEvent(*gate, kind, fact.Version)
	if err := e.store.PutGate(ctx, tx, updated); err != nil {
		return intake.Result{}, err
	}
	isOpen := classify.DesignGateOpen(updated)
	if fact.Kind == intake.DispatchArtifactChangesRequested {
		if err := e.notice(ctx, tx, fact.Key, record.Notice{Kind: "design-changes-requested", Version: fact.Version, Reason: fact.Reason}); err != nil {
			return intake.Result{}, err
		}
	}
	if !wasOpen && isOpen {
		if err := e.notice(ctx, tx, fact.Key, record.Notice{Kind: "design-approved", Version: fact.Version}); err != nil {
			return intake.Result{}, err
		}
		issue, err := e.store.Issue(ctx, tx, fact.Key)
		if err != nil || issue == nil {
			return intake.Result{}, err
		}
		if err := e.advanceAdmittedTree(ctx, tx, *issue, updated); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, e.advancePendingReady(ctx, tx, fact.Key, updated)
}

func (e *Engine) handoff(ctx context.Context, tx pgx.Tx, fact intake.HandoffComplete) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if roleFor(issue.Phase) != fact.Role {
		return refused("HANDOFF_NOT_CURRENT_PHASE", fmt.Sprintf("the %s does not run phase %s of %s; this completion changed nothing", fact.Role, issue.Phase, issue.Key)), nil
	}
	if issue.Phase == phase.Merging && !fact.Ready {
		return refused("READY_REQUIRED", "the merger's completion is READY: run legion handoff complete --ready; this completion changed nothing"), nil
	}
	row, err := e.phaseRow(ctx, tx, fact.Issue, fact.Role)
	if err != nil {
		return intake.Result{}, err
	}
	if phase.FileBacked(issue.Phase) {
		if fact.Commit == row.LastHandoff {
			return refused("HANDOFF_NOT_NEW", fmt.Sprintf("the %s reported commit %s for its previous phase of %s; write and commit this phase's handoff before completing", fact.Role, fact.Commit, issue.Key)), nil
		}
		row.LastHandoff = fact.Commit
	}
	row.Claim, row.HandoffCommit, row.Verdict = fact.Claim, fact.Commit, fact.Verdict
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return intake.Result{}, err
	}
	pr, err := e.store.PullRequest(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	switch issue.Phase {
	case phase.Planning:
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerPlannerCompleted, "", row, pr, "")
	case phase.Implementing:
		if pr == nil {
			return intake.Result{}, nil
		}
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerImplementationReady, "", row, pr, "")
	case phase.Testing:
		if fact.Verdict == "fail" {
			if err := e.recordRound(ctx, tx, fact.Issue); err != nil {
				return intake.Result{}, err
			}
			return intake.Result{}, e.transition(ctx, tx, *issue, TriggerTesterFailed, "", row, pr, "")
		}
		if fact.Verdict == "pass" {
			return intake.Result{}, e.transition(ctx, tx, *issue, TriggerTesterPassed, "", row, pr, "")
		}
	case phase.Retro:
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerRetroCompleted, "", row, pr, "")
	case phase.ProductionCheck:
		// The architect's sign-off, not this completion, moves the issue to done; the completion is
		// what tells the architect the check was recorded.
		return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "phase-finished", Role: fact.Role, Phase: issue.Phase, Summary: fact.Summary})
	case phase.Merging:
		gate, err := e.gateForIssue(ctx, tx, *issue)
		if err != nil {
			return intake.Result{}, err
		}
		if gate == nil || !classify.DesignGateOpen(*gate) {
			version := 0
			if gate != nil {
				version = gate.LatestVersion
			}
			issue.ReadyPendingVersion = &version
			if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
				return intake.Result{}, err
			}
			message := fmt.Sprintf("READY refused: approve design version %d before requesting READY.", version)
			if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "ready-refused", Version: version, Reason: message}); err != nil {
				return intake.Result{}, err
			}
			return intake.Result{Refusal: &intake.Refusal{Status: 409, Code: "DESIGN_GATE_CLOSED", Message: message}}, nil
		}
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerReady, "", row, pr, "")
	}
	return intake.Result{}, nil
}

func (e *Engine) pullRequestOpened(ctx context.Context, tx pgx.Tx, fact intake.PullRequestOpened) (intake.Result, error) {
	issue, err := e.issueForBranch(ctx, tx, fact.Branch)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	pr := record.PullRequest{Issue: issue.Key, Repo: fact.Repo, Number: fact.Number, Branch: fact.Branch, HeadSHA: fact.HeadSHA,
		HeadUpdatedAt: fact.UpdatedAt, HeadUpdatedAtSource: "webhook", Failing: []string{}, FailingStatuses: []string{}, State: record.PullRequestOpen}
	if err := e.store.PutPullRequest(ctx, tx, pr); err != nil {
		return intake.Result{}, err
	}
	if issue.Phase != phase.Implementing {
		return intake.Result{}, nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleImplementer)
	if err != nil || row.HandoffCommit == "" {
		return intake.Result{}, err
	}
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerImplementationReady, "", row, &pr, "")
}

func (e *Engine) pullRequestSynchronized(ctx context.Context, tx pgx.Tx, fact intake.PullRequestSynchronized) (intake.Result, error) {
	pr, err := e.store.PullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	if fact.HeadSHA != "" && fact.HeadSHA != pr.HeadSHA {
		*pr = classify.AdvancePullRequestHead(*pr, fact.HeadSHA)
	}
	pr.HeadUpdatedAt, pr.HeadUpdatedAtSource = fact.UpdatedAt, "webhook"
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil || issue.Phase != phase.Implementing {
		return intake.Result{}, err
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleImplementer)
	if err != nil || row.HandoffCommit == "" {
		return intake.Result{}, err
	}
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerImplementationReady, "", row, pr, "")
}

func (e *Engine) push(ctx context.Context, tx pgx.Tx, fact intake.Push) (intake.Result, error) {
	pr, err := e.store.PullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	*pr = classify.ApplyPush(*pr, fact.After, classify.ClassifyPush(classify.PushPayload{ChangedPaths: fact.ChangedPaths, ChangedPathsTruncated: fact.Truncated}))
	return intake.Result{}, e.store.PutPullRequest(ctx, tx, *pr)
}

func (e *Engine) checks(ctx context.Context, tx pgx.Tx, fact intake.PullRequestChecks) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	if fact.HeadSHA != "" && fact.HeadSHA != pr.HeadSHA {
		return intake.Result{}, nil
	}
	var applied bool
	*pr, applied = classify.ApplySettlement(*pr, classify.SettlementCandidate{CheckRuns: fact.CheckRuns, Generation: fact.Generation, Snapshot: fact.Snapshot, Verdict: fact.Verdict, Failing: fact.Failing})
	if !applied {
		return intake.Result{}, nil
	}
	var blocked bool
	*pr, blocked = classify.BlockFixAttempt(*pr, e.cfg.MaxFixAttempts)
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	if !blocked {
		return intake.Result{}, e.advanceApproved(ctx, tx, *pr)
	}
	message := fmt.Sprintf("Pull request #%d reached max_fix_attempts=%d.", pr.Number, e.cfg.MaxFixAttempts)
	if err := e.enqueue(ctx, tx, pr.Issue, record.MessagePost{Body: message}); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.notice(ctx, tx, pr.Issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: message})
}

func (e *Engine) review(ctx context.Context, tx pgx.Tx, fact intake.PullRequestReview) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	*pr = classify.ApplyReview(*pr, strings.ToLower(fact.State), fact.CommitID)
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil || issue.Phase != phase.Reviewing {
		return intake.Result{}, err
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return intake.Result{}, err
	}
	if pr.ReviewDecision == "changes_requested" {
		if err := e.recordRound(ctx, tx, issue.Key); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerReviewRejected, "", row, pr, fact.Body)
	}
	return intake.Result{}, e.advanceApproved(ctx, tx, *pr)
}

// advanceApproved moves a reviewing issue to retro once its pull request is both approved at the
// current head and green there. Either may come second: the reviewer usually approves the head it
// was just shown, before CI settles on it, so the checks' settlement asks again.
func (e *Engine) advanceApproved(ctx context.Context, tx pgx.Tx, pr record.PullRequest) error {
	if pr.ReviewDecision != "approved" || pr.Verdict != "green" {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil || issue.Phase != phase.Reviewing {
		return err
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return err
	}
	return e.transition(ctx, tx, *issue, TriggerReviewApproved, "", row, &pr, "")
}

// merged records the pull request merged, whatever the issue's phase, and advances an issue that
// awaited the merge.
func (e *Engine) merged(ctx context.Context, tx pgx.Tx, fact intake.PullRequestMerged) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	pr.State = record.PullRequestMerged
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil || issue.Phase != phase.AwaitingMerge {
		return intake.Result{}, err
	}
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerPullRequestMerged, "", record.PhaseRow{}, pr, "")
}

// closed records the pull request closed unmerged; a re-admitted generation drops it.
func (e *Engine) closed(ctx context.Context, tx pgx.Tx, fact intake.PullRequestClosed) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	pr.State = record.PullRequestClosed
	return intake.Result{}, e.store.PutPullRequest(ctx, tx, *pr)
}

func (e *Engine) claimFailed(ctx context.Context, tx pgx.Tx, fact intake.ClaimFailed) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Phase == phase.Held || roleFor(issue.Phase) != fact.Role {
		return intake.Result{}, err
	}
	from := issue.Phase
	issue.Phase, issue.HeldFrom = phase.Held, &from
	if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
		return intake.Result{}, err
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "held", Role: fact.Role, Phase: from}); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "worker-died", Role: fact.Role, Phase: from})
}

func (e *Engine) retryOrEscalate(ctx context.Context, tx pgx.Tx, fact intake.RetryOrEscalate) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Phase != phase.Held || issue.HeldFrom == nil {
		return intake.Result{}, err
	}
	if fact.Decision == intake.EscalateDecision {
		return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "held", Phase: *issue.HeldFrom, Reason: "escalated"})
	}
	if fact.Decision != intake.RetryDecision {
		return intake.Result{}, nil
	}
	from := *issue.HeldFrom
	issue.Phase, issue.HeldFrom = from, nil
	if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.start(ctx, tx, *issue, roleFor(from), task(*issue, record.PhaseRow{}, nil, "retry held phase"))
}

func (e *Engine) backward(ctx context.Context, tx pgx.Tx, fact intake.BackwardMove) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if roleFor(issue.Phase) == "" || roleFor(issue.Phase) != fact.Requester || phaseIndex(fact.To) >= phaseIndex(issue.Phase) || phaseIndex(fact.To) < 0 {
		return intake.Result{Refusal: &intake.Refusal{Status: 409, Code: "BACKWARD_REFUSED", Message: "backward moves require the current role and an earlier workflow phase"}}, nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, fact.Requester)
	if err != nil {
		return intake.Result{}, err
	}
	if err := e.recordRound(ctx, tx, issue.Key); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerBackward, fact.To, row, nil, fact.Reason)
}

// signOff closes the issue once the implementer's production check was recorded: the merge started
// the production check with the implementer's handoff cleared, so a recorded handoff is the check's
// own completion.
func (e *Engine) signOff(ctx context.Context, tx pgx.Tx, fact intake.SignOff) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if issue.Phase != phase.ProductionCheck {
		return refused("SIGNOFF_OUTSIDE_PRODUCTION_CHECK", fmt.Sprintf("%s is in phase %s, not production_check; this sign-off changed nothing", issue.Key, issue.Phase)), nil
	}
	check, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleImplementer)
	if err != nil {
		return intake.Result{}, err
	}
	if check.HandoffCommit == "" {
		return refused("PRODUCTION_CHECK_NOT_RECORDED", fmt.Sprintf("the implementer has not recorded the production check of %s; sign off after its phase-finished notice", issue.Key)), nil
	}
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerSignOff, "", record.PhaseRow{}, nil, "")
}

func (e *Engine) lingerExpired(ctx context.Context, tx pgx.Tx, fact intake.LingerExpired) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil || issue.Generation != fact.Generation || issue.LingerUntil == nil {
		return intake.Result{}, err
	}
	members, err := e.treeMembers(ctx, tx, *issue)
	if err != nil {
		return intake.Result{}, err
	}
	for _, member := range members {
		if err := e.everyClaim(ctx, tx, member, "stop"); err != nil {
			return intake.Result{}, err
		}
		if err := e.enqueue(ctx, tx, member.Key, record.WorkspaceRemove{}); err != nil {
			return intake.Result{}, err
		}
	}
	return intake.Result{}, nil
}

func (e *Engine) transition(ctx context.Context, tx pgx.Tx, issue record.Issue, trigger TriggerKind, target phase.Phase, handoff record.PhaseRow, pr *record.PullRequest, reason string) error {
	row, ok := e.row(issue.Phase, trigger, target, Snapshot{Phase: issue.Phase, HasPR: pr != nil})
	if !ok {
		return nil
	}
	if err := e.status(ctx, tx, issue, row.Status); err != nil {
		return err
	}
	from := issue.Phase
	issue.Phase, issue.HeldFrom = row.To, nil
	if row.Status != "" {
		issue.Status = row.Status
	}
	if trigger == TriggerReady {
		issue.ReadyPendingVersion = nil
	}
	if err := e.store.PutIssue(ctx, tx, issue); err != nil {
		return err
	}
	if err := e.clearHandoff(ctx, tx, issue.Key, roleFor(row.To)); err != nil {
		return err
	}
	if err := e.suspend(ctx, tx, issue, roleFor(from)); err != nil {
		return err
	}
	if err := e.start(ctx, tx, issue, roleFor(row.To), task(issue, handoff, pr, reason)); err != nil {
		return err
	}
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "phase-finished", Role: roleFor(from), Phase: from, Summary: handoff.Verdict}); err != nil {
		return err
	}
	if row.To == phase.Done {
		return e.leave(ctx, tx, issue, "done")
	}
	return nil
}

func (e *Engine) row(from phase.Phase, trigger TriggerKind, target phase.Phase, snapshot Snapshot) (Row, bool) {
	for _, row := range Table {
		if row.From == from && row.Trigger == trigger && (target == "" || row.To == target) && (row.Guard == nil || row.Guard(snapshot)) {
			return row, true
		}
	}
	return Row{}, false
}

func (e *Engine) advanceAdmittedTree(ctx context.Context, tx pgx.Tx, root record.Issue, gate record.DesignGate) error {
	if !classify.DesignGateOpen(gate) {
		return nil
	}
	members, err := e.treeMembers(ctx, tx, root)
	if err != nil {
		return err
	}
	for _, issue := range members {
		if issue.Phase == phase.Admitted {
			if err := e.transition(ctx, tx, issue, TriggerGateOpened, "", record.PhaseRow{}, nil, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// advancePendingReady advances every merger in the tree whose READY was refused while the gate was
// closed, now that a human approved the gate's current version. That version may be later than
// the one the refusal named: the READY stands until the gate reopens, whatever the human revised
// in between.
func (e *Engine) advancePendingReady(ctx context.Context, tx pgx.Tx, rootKey string, gate record.DesignGate) error {
	if !classify.DesignGateOpen(gate) {
		return nil
	}
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if e.treeKey(ctx, tx, issue) != rootKey || issue.Phase != phase.Merging || issue.ReadyPendingVersion == nil || *issue.ReadyPendingVersion > gate.LatestVersion {
			continue
		}
		row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleMerger)
		if err != nil {
			return err
		}
		pr, err := e.store.PullRequest(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		if err := e.transition(ctx, tx, issue, TriggerReady, "", row, pr, "approved design gate"); err != nil {
			return err
		}
	}
	return nil
}

// leave is an issue leaving the workflow for status (done, backlog, or icebox). A root takes its
// tree with it into linger. A child ends only itself: the tree's architect is told, and decides
// what the rest of its tree does, as the shipped daemon routes a child's close to the architect.
func (e *Engine) leave(ctx context.Context, tx pgx.Tx, issue record.Issue, status string) error {
	if e.treeKey(ctx, tx, issue) == issue.Key {
		return e.beginLinger(ctx, tx, issue)
	}
	kind := record.NoticeKind("child-status")
	if status == "done" {
		kind = "child-closed"
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: kind, Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is %s", issue.Key, status)})
}

// beginLinger suspends the root's whole tree and arms its linger deadline; a second call while it
// lingers changes nothing.
func (e *Engine) beginLinger(ctx context.Context, tx pgx.Tx, root record.Issue) error {
	if root.LingerUntil != nil {
		return nil
	}
	until := e.lingerAt()
	root.LingerUntil = &until
	root.Phase = phase.Done
	if err := e.store.PutIssue(ctx, tx, root); err != nil {
		return err
	}
	members, err := e.treeMembers(ctx, tx, root)
	if err != nil {
		return err
	}
	for _, member := range members {
		if err := e.everyClaim(ctx, tx, member, "suspend"); err != nil {
			return err
		}
	}
	row, err := record.NewOutboxRow(root.Key, record.LingerClose{Generation: root.Generation}, until)
	if err != nil {
		return err
	}
	return e.store.Enqueue(ctx, tx, row)
}

// everyClaim enqueues op for every claim an issue can hold: its architect, which admission or the
// tree's architect started, and each phase worker, whether or not it ever reported a handoff. The
// phase rows record only handoffs, so they cannot list the claims; the executor treats a request
// for a claim that does not exist as done.
func (e *Engine) everyClaim(ctx context.Context, tx pgx.Tx, issue record.Issue, op record.SuperviseOp) error {
	for _, role := range claim.Roles {
		if err := e.supervise(ctx, tx, issue, op, role, ""); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) recordRound(ctx context.Context, tx pgx.Tx, issue string) error {
	row, err := e.phaseRow(ctx, tx, issue, claim.RoleImplementer)
	if err != nil {
		return err
	}
	row.Rounds++
	if err := e.store.PutPhase(ctx, tx, row); err != nil {
		return err
	}
	if row.Rounds < e.cfg.ReviewRoundCap {
		return nil
	}
	message := fmt.Sprintf("Issue reached review_round_cap=%d.", e.cfg.ReviewRoundCap)
	if err := e.enqueue(ctx, tx, issue, record.MessagePost{Body: message}); err != nil {
		return err
	}
	return e.notice(ctx, tx, issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: message})
}

func (e *Engine) phaseRow(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (record.PhaseRow, error) {
	rows, err := e.store.Phases(ctx, tx, issue)
	if err != nil {
		return record.PhaseRow{}, err
	}
	for _, row := range rows {
		if row.Role == role {
			return row, nil
		}
	}
	return record.PhaseRow{Issue: issue, Role: role}, nil
}

func (e *Engine) issueForBranch(ctx context.Context, tx pgx.Tx, branch string) (*record.Issue, error) {
	if !strings.HasPrefix(branch, "legion/") {
		return nil, nil
	}
	return e.store.Issue(ctx, tx, strings.TrimPrefix(branch, "legion/"))
}

func (e *Engine) pullRequest(ctx context.Context, tx pgx.Tx, repo string, number int) (*record.PullRequest, error) {
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		pr, err := e.store.PullRequest(ctx, tx, issue.Key)
		if err != nil {
			return nil, err
		}
		if pr != nil && pr.Repo == repo && pr.Number == number {
			return pr, nil
		}
	}
	return nil, nil
}

func (e *Engine) treeKey(_ context.Context, _ pgx.Tx, issue record.Issue) string {
	if issue.Tree != "" {
		return issue.Tree
	}
	return issue.Key
}

func (e *Engine) treeMembers(ctx context.Context, tx pgx.Tx, root record.Issue) ([]record.Issue, error) {
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	members := []record.Issue{}
	for _, issue := range issues {
		if issue.Tree == root.Tree {
			members = append(members, issue)
		}
	}
	return members, nil
}

func (e *Engine) liveTree(ctx context.Context, tx pgx.Tx, root record.Issue) (bool, error) {
	if root.LingerUntil != nil {
		return false, nil
	}
	slots, err := e.store.Slots(ctx, tx)
	if err != nil {
		return false, err
	}
	for _, slot := range slots {
		if slot.Issue == root.Key {
			return true, nil
		}
	}
	issues, err := e.store.Issues(ctx, tx)
	if err != nil {
		return false, err
	}
	for _, waiting := range record.Waiting(issues, slots) {
		if waiting.Key == root.Key {
			return true, nil
		}
	}
	return false, nil
}

func (e *Engine) gateForIssue(ctx context.Context, tx pgx.Tx, issue record.Issue) (*record.DesignGate, error) {
	return e.store.Gate(ctx, tx, e.treeKey(ctx, tx, issue))
}
func staleTreeStatus(status string) bool {
	return status == "done" || status == "backlog" || status == "icebox"
}
func phaseIndex(value phase.Phase) int {
	for index, candidate := range []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Retro, phase.Merging, phase.AwaitingMerge, phase.ProductionCheck} {
		if candidate == value {
			return index
		}
	}
	return -1
}
