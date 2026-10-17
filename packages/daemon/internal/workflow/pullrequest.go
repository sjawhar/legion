package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullRequestOpened records the pull request a branch opened, and a reopen is the same pull
// request: its fix and blocked attempts are what bound the review rounds, so they are carried
// over rather than rebuilt at zero, which made closing and reopening a way to buy a fresh cap, and
// so is the counted attempt the cap holds a red in (classify.FixAttemptCapHolds), and its newest
// review, which orders every review it will have, and what its base branch requires - its required
// checks, and its required workflows with their runs at the head they were read at - as last read,
// so the reopened head's first settlement is judged by all of it (classify.HeadChecks). A reopen at
// another head than the recorded one - commits pushed while the pull request was closed, which no
// synchronize reports - is judged as a synchronize to that head would be
// (classify.AdvancePullRequestHead): onto a red verdict it counts, onto a green one it leaves the
// counted attempt. A new generation deletes a pull request that is not open, and all of them with
// it.
func (e *Engine) pullRequestOpened(ctx context.Context, tx pgx.Tx, fact intake.PullRequestOpened) (intake.Result, error) {
	issue, err := e.issueForBranch(ctx, tx, fact.Branch)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	pr := record.PullRequest{Issue: issue.Key, Repo: fact.Repo, Number: fact.Number, Branch: fact.Branch, HeadSHA: fact.HeadSHA,
		HeadUpdatedAt: fact.UpdatedAt, Failing: []string{}, State: record.PullRequestOpen}
	recorded, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return intake.Result{}, err
	}
	// An opened or reopened older than the recorded pull request's clock is a late redelivery,
	// whichever pull request it names: a branch can carry a newer pull request after a merge or a
	// park and rerun, and an earlier one's late event must not replace it. The workflow keeps one
	// pull request per branch and assumes one is open at a time; two open at once from one branch,
	// on different bases, are not modelled, and this fence can then keep the earlier one, whose
	// close still tells the architect.
	if recorded != nil && classify.LateLifecycle(fact.UpdatedAt, recorded.HeadUpdatedAt) {
		return intake.Result{}, nil
	}
	if recorded != nil && recorded.Repo == fact.Repo && recorded.Number == fact.Number {
		// GitHub sends opened once per pull request, so an opened for one already recorded is a
		// redelivery whatever its clock; a reopen's clock is left to the fence above. GitHub never
		// reopens a merged pull request, so a reopen of one is older than the merge, which carries
		// no clock to fence it.
		if !fact.Reopened || recorded.State == record.PullRequestMerged {
			return intake.Result{}, nil
		}
		carried := *recorded
		if fact.HeadSHA != recorded.HeadSHA {
			carried = classify.AdvancePullRequestHead(carried, fact.HeadSHA)
		}
		pr.FixAttempts, pr.BlockedAttempts, pr.ReviewSeen = carried.FixAttempts, carried.BlockedAttempts, recorded.ReviewSeen
		pr.HeadCounted, pr.HeadCountedBefore = carried.HeadCounted, carried.HeadCountedBefore
		pr.Required, pr.Workflows, pr.WorkflowsHead = recorded.Required, recorded.Workflows, recorded.WorkflowsHead
		pr.HeadUpdatedAt = classify.LatestClock(recorded.HeadUpdatedAt, fact.UpdatedAt)
	}
	if err := e.store.PutPullRequest(ctx, tx, pr); err != nil {
		return intake.Result{}, err
	}
	if fact.Reopened {
		if err := e.headMoved(ctx, tx, *issue, &pr, true); err != nil {
			return intake.Result{}, err
		}
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

// pullRequestSynchronized records the pull request's new head and what the move leaves standing
// (classify.AdvancePullRequestHead, headMoved), and applies the CI rule to the new head first, in
// whatever phase the issue is (standingRed). GitHub delivers a push and its synchronize in either
// order, and either can complete the carry of a code head's red to the handoff head that replaced
// it (classify.SettlementFor, carriedBack): the push's handler does when the head arrived first
// (push), and the head's arrival does here when its push was classified handoff-only before the
// head was known - the record then says the code head, red, and the arrival of the head above it
// lets the red stand for it. The issue may be in testing, retro or merging by then, as the
// implementer's completion waits on no webhook, so a red that stands sends the work back from
// whichever phase, as the settlement would have had it arrived after both, and nothing below runs.
// The rule is safe in every phase: a phase with no checks_red row - implementing, planning - moves
// nothing (sendBack), in awaiting_merge the rule does not act (standingRed), and a code push between
// the two heads carries nothing. In reviewing the head's arrival then re-asks the round
// (settleRound) once its push is known (classify.HeadPushKnown), since what the approval is judged
// against is the push's paths: the approved head arriving over a handoff push lets the approval
// stand, and the new head of a code push that was on its way shows the approval does not cover it
// (classify.ApprovalStands, classify.CodeOnItsWay). A head whose push is still on its way is left
// to the push's own handler, which re-asks the round the moment the push is classified. Asked
// here, a round stuck at the previous head would be told stuck again at the new one, which the
// push then shows to be the handoff head its approval covers - a notice the architect acts on for
// nothing. A push lost outright leaves the round reported as it was, stuck at the previous head,
// which is told once either way. A synchronize that moves nothing - a redelivery, or one carrying
// no head - changes nothing the verdict or the round reads, and applies and re-asks nothing. In
// implementing a head that arrives once the implementer has completed is the pull request its
// completion waited for.
func (e *Engine) pullRequestSynchronized(ctx context.Context, tx pgx.Tx, fact intake.PullRequestSynchronized) (intake.Result, error) {
	pr, err := e.store.PullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || pr == nil || classify.LateLifecycle(fact.UpdatedAt, pr.HeadUpdatedAt) {
		return intake.Result{}, err
	}
	prior := *pr
	moved := fact.HeadSHA != "" && fact.HeadSHA != pr.HeadSHA
	if moved {
		*pr = classify.AdvancePullRequestHead(*pr, fact.HeadSHA)
	}
	pr.HeadUpdatedAt = classify.LatestClock(pr.HeadUpdatedAt, fact.UpdatedAt)
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if err := e.headMoved(ctx, tx, *issue, pr, false); err != nil {
		return intake.Result{}, err
	}
	if moved {
		if sent, err := e.standingRed(ctx, tx, *issue, pr); err != nil || sent {
			return intake.Result{}, err
		}
	}
	switch issue.Phase {
	case phase.Reviewing:
		if !moved || !classify.HeadPushKnown(prior, pr.HeadSHA) {
			return intake.Result{}, nil
		}
		return intake.Result{}, e.reaskRound(ctx, tx, *issue, pr, prior)
	case phase.Implementing:
		row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleImplementer)
		if err != nil || row.HandoffCommit == "" {
			return intake.Result{}, err
		}
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerImplementationReady, "", row, pr, "")
	}
	return intake.Result{}, nil
}

func (e *Engine) push(ctx context.Context, tx pgx.Tx, fact intake.Push) (intake.Result, error) {
	pr, err := e.store.PullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	prior := *pr
	classification := classify.ClassifyPush(classify.PushPayload{ChangedPaths: fact.ChangedPaths, ChangedPathsTruncated: fact.Truncated})
	*pr = classify.ApplyPush(*pr, record.ClassifiedPush{SHA: fact.After, Before: fact.Before,
		HandoffOnly: classification.HandoffOnly, Unknown: classification.Unknown,
		ByReviewApp: e.byReviewApp(fact.Pusher),
		// A push that does not say it was not forced - a listener that predates the field - is
		// read as forced: its changed paths cannot then be trusted to describe the head it replaced.
		Forced: fact.Forced == nil || *fact.Forced != "false"})
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	// A push classified after its head arrived can complete the carry of a code head's red to the
	// handoff head that replaced it (classify.SettlementFor), which sends the work back as the
	// settlement would have (standingRed), in whatever phase the issue is: GitHub delivers a push
	// webhook and its synchronize in either order, and the implementer's completion waits on no
	// webhook, so the head can move and the issue enter testing, retro or merging before the push is
	// classified. A phase with no checks_red row - implementing, planning - moves nothing (sendBack),
	// and in awaiting_merge the rule does not act (standingRed). In reviewing the push can otherwise
	// be what lets an approval of an earlier head stand, or what shows the current head carries code
	// the approved one does not, so the round is settled again; the round's re-ask is reviewing's
	// alone.
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if moved, err := e.standingRed(ctx, tx, *issue, pr); err != nil || moved {
		return intake.Result{}, err
	}
	if issue.Phase != phase.Reviewing {
		return intake.Result{}, nil
	}
	return intake.Result{}, e.reaskRound(ctx, tx, *issue, pr, prior)
}

// reaskRound re-asks issue's review round (settleRound) after a push, or after the head it left
// arrived, pr being the pull request as the fact left it and prior as the fact found it: the round
// as prior had it is the before a round stuck the same way is deduped against (stuckAs), so a push
// classified late at the head the round was already told stuck at tells nothing again.
func (e *Engine) reaskRound(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest, prior record.PullRequest) error {
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return err
	}
	_, err = e.settleRound(ctx, tx, issue, reviewer, pr, e.reviewRound(issue, reviewer, &prior), byPush)
	return err
}

// merged records the pull request merged, whatever the issue's phase, and advances an issue that
// awaited the merge. A merge before awaiting_merge is the architect's to act on, as the shipped
// daemon routes it: it is told, and the issue moves on to the production check once it reaches
// awaiting_merge (transition). A redelivered merge changes nothing. A merge is the one fact GitHub
// never sends again, so one that lands while the tree lingers still moves the issue on to its
// production check, and starts nobody: re-admission starts its implementer there
// (admit's startMidPhaseChildren), as it does for every mid-phase child.
func (e *Engine) merged(ctx context.Context, tx pgx.Tx, fact intake.PullRequestMerged) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil || pr.State == record.PullRequestMerged {
		return intake.Result{}, err
	}
	pr.State = record.PullRequestMerged
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if issue.Phase == phase.AwaitingMerge {
		lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
		if err != nil {
			return intake.Result{}, err
		}
		if !lingers {
			return intake.Result{}, e.transition(ctx, tx, *issue, TriggerPullRequestMerged, "", record.PhaseRow{}, pr, "")
		}
		row, ok := e.row(issue.Phase, TriggerPullRequestMerged, "", Snapshot{Phase: issue.Phase, HasPR: true})
		if !ok {
			return intake.Result{}, nil
		}
		issue.Phase = row.To
		if err := e.store.PutIssue(ctx, tx, *issue); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.clearHandoff(ctx, tx, issue.Key, RoleFor(row.To))
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-merged", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("pull request #%d merged while %s was in %s, not awaiting_merge", pr.Number, issue.Key, issue.Phase)})
}

// closed records the pull request closed unmerged, which a re-admitted generation drops, and tells
// the architect, whose decision it is whether the work is reopened, reassigned, or cancelled, as the
// shipped daemon routes it. It records the head the close carries, which every synchronize before
// it left, and keeps the close's clock when it is the later one, so a reopen or a synchronize older
// than the close, redelivered late, changes nothing. A close that finds the pull request already
// closed records its head and clock too. When it is newer than the record, it is a second close
// whose reopen has not been delivered yet, and the architect is told; at the record's clock or with
// no clock it is a redelivery, and it tells the architect nothing more. A close of a merged pull
// request changes nothing: GitHub never closes one, so the close is older than the merge, which
// carries no clock to fence it.
func (e *Engine) closed(ctx context.Context, tx pgx.Tx, fact intake.PullRequestClosed) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil || pr.State == record.PullRequestMerged || classify.LateLifecycle(fact.UpdatedAt, pr.HeadUpdatedAt) {
		return intake.Result{}, err
	}
	repeated := classify.RepeatedClose(*pr, fact.UpdatedAt)
	if fact.HeadSHA != "" && fact.HeadSHA != pr.HeadSHA {
		*pr = classify.AdvancePullRequestHead(*pr, fact.HeadSHA)
	}
	pr.State = record.PullRequestClosed
	pr.HeadUpdatedAt = classify.LatestClock(pr.HeadUpdatedAt, fact.UpdatedAt)
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil || repeated {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-closed-unmerged", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("pull request #%d closed without merging while %s was in %s", pr.Number, issue.Key, issue.Phase)})
}
