package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullRequestOpened records the pull request a branch opened, and a reopen is the same pull
// request: it keeps its newest review, which orders every review it will have, and its pushes and
// head run, which are facts about commits. A reopen at
// another head than the recorded one - commits pushed while the pull request was closed, which no
// synchronize reports - is judged as a synchronize to that head would be
// (classify.AdvancePullRequestHead). A new generation
// deletes a pull request that is not open, and all of them with it. An issue that waits to close on
// its recorded merge (awaitsMerge) keeps that merge: GitHub never sends the merge again, so a pull
// request opened from its branch is not recorded, while an issue set back to todo reruns and its
// branch's new pull request replaces the merged one.
func (e *Engine) pullRequestOpened(ctx context.Context, tx pgx.Tx, fact intake.PullRequestOpened) (intake.Result, error) {
	issue, err := e.issueForBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	pr := record.PullRequest{Issue: issue.Key, Repo: fact.Repo, Number: fact.Number, Branch: fact.Branch, Base: fact.Base, HeadSHA: fact.HeadSHA,
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
	if recorded != nil && recorded.State == record.PullRequestMerged && recorded.Number != fact.Number && awaitsMerge(*issue) {
		e.logOnCommit(ctx, "workflow: a pull request opened from a branch whose issue waits to close on its merge is not recorded",
			"issue", issue.Key, "merged", recorded.Number, "opened", fact.Number)
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
		// The reopen keeps the pushes - facts about commits - and the head run, so a push that
		// arrives after the reopen settles the move the reopen made as it would have had it
		// arrived first (classify.Reopen); the CI settlement starts afresh.
		carried := classify.Reopen(*recorded, fact.HeadSHA)
		pr.ReviewSeen = recorded.ReviewSeen
		pr.Pushes, pr.Moves, pr.Run = carried.Pushes, carried.Moves, carried.Run
		// The reopen keeps the base and the push onto it the next read still owes an answer for: a
		// pull request reopens onto the base it was opened on, and a fact that names none (a
		// listener that predates the field) must not forget the recorded one.
		pr.BaseMoved = recorded.BaseMoved
		if pr.Base == "" {
			pr.Base = recorded.Base
		}
		pr.HeadUpdatedAt = classify.LatestClock(recorded.HeadUpdatedAt, fact.UpdatedAt)
	}
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
	return intake.Result{}, e.implementationReady(ctx, tx, *issue, row, &pr)
}

// pullRequestSynchronized records the pull request's new head and what the move leaves standing
// (classify.AdvancePullRequestHead). It reads no CI result: a settlement moves nothing,
// in any phase, and the roles that work the issue were told what it said when it arrived (checks,
// dispatch://INFRA-714/spec R1). In reviewing the head's arrival re-asks the round
// (settleRound) once its push is known (classify.HeadPushKnown), since what the approval is judged
// against is the push's paths: the approved head arriving over a handoff push lets the approval
// stand, and the new head of a code push that was on its way shows the approval does not cover it
// (classify.ApprovalStands, classify.CodeOnItsWay). A head whose push is still on its way is left
// to the push's own handler, which re-asks the round the moment the push is classified. Asked
// here, a round stuck at the previous head would be told stuck again at the new one, which the
// push then shows to be the handoff head its approval covers - a notice the architect acts on for
// nothing. The one exception is a head the reviewer's recorded approval names itself: no push can
// change what an approval of the head itself approves, so the round is re-asked and ends, as it
// would have had the head arrived before the approval; otherwise a push lost outright would leave
// the round stuck at the previous head with an approval of the head it now stands at. Any other
// push lost outright leaves the round reported as it was, stuck at the previous head, which is told
// once either way, and the per-issue read's compare carries the approval when only the
// issue's handoffs changed (daemon's readApprovalCarry). A synchronize that moves nothing - a
// redelivery, or one carrying no head - changes nothing the round reads, and applies
// and re-asks nothing. A synchronize that moves no head because it is older than the newest applied
// (classify.LateLifecycle, by GitHub's clock alone, as the release before), or because it returned
// the branch to the recorded head, still names the
// push its move took, by its sender, while a head run is open (classify.NameMove): an arrival whose
// synchronize said it moved from a head the record never held waits for it. Late, it records that
// alone, and moves and sets nothing else. In
// implementing a head that arrives once the implementer has completed is the pull request its
// completion waited for.
func (e *Engine) pullRequestSynchronized(ctx context.Context, tx pgx.Tx, fact intake.PullRequestSynchronized) (intake.Result, error) {
	pr, err := e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	prior := *pr
	// GitHub names the base on every pull-request event, so a synchronize is also where a base
	// retargeted on GitHub reaches the record; one that names none (a listener that predates the
	// field, or the daemon's own read of the head) leaves the recorded base as it is, for the
	// two-minute pass to fill. A late redelivery that writes nothing records no base either, as it
	// records nothing else.
	if fact.Base != "" {
		pr.Base = fact.Base
	}
	named := record.Named{Before: fact.Before, SHA: fact.HeadSHA, Sender: fact.Sender, At: fact.UpdatedAt}
	if classify.LateLifecycle(fact.UpdatedAt, pr.HeadUpdatedAt) {
		if *pr = classify.NameMove(*pr, named); pr.Run == prior.Run {
			return intake.Result{}, nil
		}
		return intake.Result{}, e.store.PutPullRequest(ctx, tx, *pr)
	}
	arrival := classify.Arrival{Before: fact.Before, Sender: fact.Sender, At: fact.UpdatedAt}
	moved := classify.MovesBy(*pr, fact.HeadSHA)
	if moved {
		*pr = classify.AdvancePullRequestHead(*pr, arrival, fact.HeadSHA)
	} else {
		*pr = classify.NameMove(*pr, named)
	}
	pr.HeadUpdatedAt = classify.LatestClock(pr.HeadUpdatedAt, fact.UpdatedAt)
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	switch issue.Phase {
	case phase.Reviewing:
		if !moved {
			return intake.Result{}, nil
		}
		if !classify.HeadPushKnown(prior, pr.HeadSHA) {
			reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
			if err != nil || reviewer.Decision == nil || reviewer.Decision.State != "approved" || reviewer.Decision.Head != pr.HeadSHA {
				return intake.Result{}, err
			}
		}
		return intake.Result{}, e.reaskRound(ctx, tx, *issue, pr, prior)
	case phase.Implementing:
		row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleImplementer)
		if err != nil || row.HandoffCommit == "" {
			return intake.Result{}, err
		}
		return intake.Result{}, e.implementationReady(ctx, tx, *issue, row, pr)
	}
	return intake.Result{}, nil
}

func (e *Engine) push(ctx context.Context, tx pgx.Tx, fact intake.Push) (intake.Result, error) {
	pr, err := e.pullRequestByBranch(ctx, tx, fact.Repo, fact.Branch)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	prior := *pr
	classification := classify.ClassifyPush(classify.PushPayload{ChangedPaths: fact.ChangedPaths, ChangedPathsTruncated: fact.Truncated}, pr.Issue)
	push := record.ClassifiedPush{SHA: fact.After, Before: fact.Before,
		HandoffOnly: classification.HandoffOnly, Unknown: classification.Unknown,
		ByReviewApp: e.byReviewApp(fact.Pusher),
		// A push that does not say it was not forced - a listener that predates the field - is
		// read as forced: its changed paths cannot then be trusted to describe the head it replaced.
		Forced: fact.Forced == nil || *fact.Forced != "false", Sender: fact.Sender}
	var dropped classify.Dropped
	*pr, dropped = classify.ApplyPushDropping(*pr, push)
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	// Past its bound the record lets the oldest push waiting for a link go (classify.ApplyPush): a
	// webhook behind it never arrived, and the move it would have linked into is judged without it.
	// A move past its bound on the pushes of its two heads made again lets the oldest go too: the
	// next arrival by that move takes the oldest kept. Past the bound on pushes of the same two heads
	// on their way, before their move is made, the oldest after the first goes.
	for _, gone := range dropped.Waiting {
		e.warnOnCommit(ctx, "workflow: a pull request holds the most pushes waiting for a link; the oldest is let go",
			"issue", pr.Issue, "pullRequest", pr.Number, "before", gone.Before, "after", gone.SHA)
	}
	if gone := dropped.Repeat; gone != nil {
		e.warnOnCommit(ctx, "workflow: a move holds the most pushes of its two heads made again; the oldest is let go",
			"issue", pr.Issue, "pullRequest", pr.Number, "before", gone.Before, "after", gone.SHA)
	}
	if gone := dropped.Copy; gone != nil {
		e.warnOnCommit(ctx, "workflow: a pull request holds the most pushes of the same two heads on their way; the oldest after the first is let go",
			"issue", pr.Issue, "pullRequest", pr.Number, "before", gone.Before, "after", gone.SHA)
	}
	// A push changes which settlement stands for the head - it can complete the carry of a code
	// head's settlement to the handoff head that replaced it (classify.SettlementFor) - and that
	// moves nothing: the roles that work the issue were told what the settlement said when it
	// arrived (checks, dispatch://INFRA-714/spec R1). In reviewing the push can be what lets an
	// approval of an earlier head stand, or what shows the current head carries code the approved
	// one does not, so the round is settled again; the round's re-ask is reviewing's alone.
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if issue.Phase != phase.Reviewing {
		return intake.Result{}, nil
	}
	// The carry is queued before the round is re-asked: a round the push leaves stuck below the
	// approved head is told so, as it was while the two-minute pass made this compare, and the
	// row's fact ends it once GitHub answers that nothing outside the issue's handoffs changed.
	if err := e.carryApproval(ctx, tx, *issue, pr); err != nil {
		return intake.Result{}, err
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

// merged records the pull request merged, with its merge commit, whatever the issue's phase, and
// closes an issue that reached merging (closeMerged): the merger merges, and the merge is what
// ends that phase. A merge before merging is the architect's to act on, as the shipped daemon
// routes it: it is told, and the merge closes the issue once it reaches merging (transition). A
// redelivered merge changes nothing.
func (e *Engine) merged(ctx context.Context, tx pgx.Tx, fact intake.PullRequestMerged) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil || pr.State == record.PullRequestMerged {
		return intake.Result{}, err
	}
	pr.State, pr.MergeSHA = record.PullRequestMerged, fact.MergeSHA
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if issue.Phase == phase.Merging {
		return intake.Result{}, e.closeMergedTelling(ctx, tx, *issue, pr)
	}
	return intake.Result{}, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-merged", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("pull request #%d merged while %s was in %s, not merging", pr.Number, issue.Key, issue.Phase)})
}

// waitingForTree is what a root's architect is told when the merge of its pull request finds the
// rest of the tree still running: the close is waiting, and for whom.
func waitingForTree(issue string, number int, waiting []string) string {
	return fmt.Sprintf("pull request #%d merged, and %s's close is waiting for %s, still in the workflow; %s closes once the last of them leaves, or take one out of the workflow with park_child",
		number, issue, strings.Join(waiting, ", "), issue)
}

// closeMergedTelling closes issue on its merged pull request (closeMerged) and, when the close
// waited for the rest of the tree instead, tells the root's architect once. It is the two places
// a wait is newly entered — the merge arriving at an issue in merging (merged), and an
// issue reaching merging on a pull request already merged (transition) — and nowhere else:
// notices never de-duplicate (notice writes a row per call) and closeMerged is re-entered on every
// child's departure (closeAwaitingRoot) and on every boot (sweepMerged), so those two tell nobody.
func (e *Engine) closeMergedTelling(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	waiting, err := e.closeMerged(ctx, tx, issue, pr)
	if err != nil || len(waiting) == 0 {
		return err
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "pr-merged", Role: claim.RoleArchitect,
		Reason: waitingForTree(issue.Key, pr.Number, waiting)})
}

// closeMerged closes issue, which awaits its merge in merging (or is held from merging, a hold the
// upgrade's data move left, which the close clears), on its merged pull request pr, and reports
// the issues it waited for instead, empty when it closed. In a running tree the close is the
// table's row to done (transition), which leaves: a child's close retires five of its claims and
// opens its production check in the implementer's live session, and a root's lingers its tree and
// opens its check behind the linger's rows (leave). A merge is the one fact GitHub never sends
// again, so one that lands while the tree lingers, where no transition runs, still closes the
// issue: it writes what leave would that a lingering tree allows — the phase done, the status
// done, the pull request disarmed, a child's architect told child-closed — and opens the check,
// whose first look is a wake, since the linger suspended the implementer. It retires nobody and
// suspends nothing: the linger's own rows already did, and the tree's close retires every member's
// claims. The boot's sweep closes a migrated issue the same way (booted).
//
// A root's close waits for the rest of its tree: the linger it would begin suspends every member
// mid-turn, so a child's finished phase would be refused 409 TREE_LINGERING and its work lost. This
// wait is where the daemon keeps a tree's order, so nothing beneath an issue holds its acceptance
// (signOff). It reads the flat tree (treeMembers), not the parent relation, because the linger
// stops every member of the tree, whatever it hangs under. It is kept inside the branch where the
// tree does not linger: a member of a tree that already lingers must still close at its merge,
// which suspends nothing further and is the one thing that still moves there.
func (e *Engine) closeMerged(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) ([]string, error) {
	if issue.Phase == phase.Held && issue.Hold != nil && issue.Hold.From == phase.Merging {
		issue.Phase, issue.Hold = phase.Merging, nil
	}
	if issue.Phase != phase.Merging {
		return nil, nil
	}
	lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree)
	if err != nil {
		return nil, err
	}
	if !lingers {
		waiting, err := e.openBeside(ctx, tx, issue)
		if err != nil {
			return nil, err
		}
		if len(waiting) > 0 {
			e.logOnCommit(ctx, "workflow: a merged root's close waits for the rest of its tree", "issue", issue.Key, "tree", issue.Tree,
				"pullRequest", pr.Number, "waitingFor", strings.Join(waiting, ", "))
			return waiting, nil
		}
		return nil, e.transition(ctx, tx, issue, TriggerPullRequestMerged, "", record.PhaseRow{}, pr, "")
	}
	row, ok := e.row(issue.Phase, TriggerPullRequestMerged, "", Snapshot{Phase: issue.Phase, HasPR: true})
	if !ok {
		return nil, nil
	}
	if err := e.status(ctx, tx, issue, row.Status); err != nil {
		return nil, err
	}
	issue.Phase, issue.Hold = row.To, nil
	if row.Status != "" {
		issue.Status = row.Status
	}
	if err := e.store.PutIssue(ctx, tx, issue); err != nil {
		return nil, err
	}
	e.logOnCommit(ctx, "workflow: a lingering tree's member closed at its merge", "issue", issue.Key, "tree", issue.Tree, "pullRequest", pr.Number, "status", issue.Status)
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: fmt.Sprintf("%s is done", issue.Key)}); err != nil {
			return nil, err
		}
	}
	return nil, e.openCheck(ctx, tx, issue, *pr, true)
}

// openBeside is the sorted keys of the other members of issue's tree that are still in the
// workflow (InWorkflow) when issue is that tree's root, and nothing at all when it is not: only a
// root's close lingers the tree, so only a root waits for it.
func (e *Engine) openBeside(ctx context.Context, tx pgx.Tx, issue record.Issue) ([]string, error) {
	if !claim.IsTreeRoot(issue.Key, issue.Tree) {
		return nil, nil
	}
	members, err := e.treeMembers(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	open := []string{}
	for _, member := range members {
		if member.Key != issue.Key && InWorkflow(member) {
			open = append(open, member.Key)
		}
	}
	slices.Sort(open)
	return open, nil
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
		*pr = classify.AdvancePullRequestHead(*pr, classify.Arrival{}, fact.HeadSHA)
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
