package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// checks applies a CI settlement to the pull request and then decides, with the issue's phase in
// hand, what it moves. An exhausted fix-attempt count is posted and told to the architect. Else
// the open review round is asked first (advanceReview), since the settlement can be what an
// approval waits for, and a round whose halves are in ends as it always does, its decision reaching
// the next implementer and its round counted. Only a settlement that ended no round can send the
// issue back to implementing (classify.RedSendsBack), from testing or reviewing, the phases whose
// TriggerChecksRed rows the table has. The implementer's task and the architect's checks-red
// notice name the failing checks, and the implementer's next push is a counted fix attempt, as any
// new head on a red verdict is (classify.AdvancePullRequestHead). A settlement that leaves a
// completed round stuck, where it was not stuck so before, tells the architect (tellStuckReview).
func (e *Engine) checks(ctx context.Context, tx pgx.Tx, fact intake.PullRequestChecks) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	prior := *pr
	// A handoff push can start no CI of its own (GitHub's skip-checks trailer), so the settlement
	// that stands for the head can be of the code head it replaced, arriving after it. A
	// settlement is for the commit it names, never the head by default.
	candidate := classify.SettlementCandidate{Head: fact.HeadSHA, CheckRuns: fact.CheckRuns, Generation: fact.Generation, Snapshot: fact.Snapshot, Verdict: fact.Verdict, Failing: fact.Failing}
	var stands bool
	if *pr, stands = classify.SettlementFor(*pr, candidate); !stands {
		return intake.Result{}, nil
	}
	var applied bool
	*pr, applied = classify.ApplySettlement(*pr, candidate)
	if !applied {
		return intake.Result{}, nil
	}
	var blocked bool
	*pr, blocked = classify.BlockFixAttempt(*pr, e.cfg.MaxFixAttempts)
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	if blocked {
		// Linger holds a member of a closed tree where it stood (record.TreeLingers): the exhausted
		// count is recorded on the pull request, and nothing is posted on the issue or told to its
		// architect.
		if issue != nil {
			if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
				return intake.Result{}, err
			}
		}
		message := fmt.Sprintf("Pull request #%d reached max_fix_attempts=%d.", pr.Number, e.cfg.MaxFixAttempts)
		if err := e.enqueue(ctx, tx, pr.Issue, record.MessagePost{Body: message}); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.notice(ctx, tx, pr.Issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: message})
	}
	if issue == nil {
		return intake.Result{}, nil
	}
	ended, err := e.advanceReview(ctx, tx, *issue, pr)
	if err != nil || ended {
		return intake.Result{}, err
	}
	if !classify.RedSendsBack(*pr) {
		return intake.Result{}, e.tellStuckReviewSince(ctx, tx, *issue, &prior, pr)
	}
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerChecksRed, "", record.PhaseRow{}, pr, redAt(*pr))
}

// redAt is what the red verdict standing for the pull request's head says: the head, and the checks
// failing there. A red that names no failing check (classify.EffectiveOutcome keeps the verdict
// when nothing names one) says so plainly rather than ending in an empty list.
func redAt(pr record.PullRequest) string {
	reason := "CI is red at " + pr.HeadSHA
	if failing := append(append([]string(nil), pr.Failing...), pr.FailingStatuses...); len(failing) > 0 {
		reason += ": " + strings.Join(failing, ", ")
	}
	return reason
}

// answerSkew is how much later than the reviewer's completion a review must be submitted to be its
// answer to a round it left undecided (review).
const answerSkew = 2 * time.Minute

func (e *Engine) review(ctx context.Context, tx pgx.Tx, fact intake.PullRequestReview) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return intake.Result{}, err
	}
	// A review the reviewer submitted after completing its round (the review App's, with a body,
	// submitted after the completion was applied) is its answer to a round it left undecided, and
	// every one that leaves the round stuck is told. Anything else tells only when it changes why
	// the round is stuck: a review submitted before the completion but delivered after it, since the
	// completion comes through the API and the review by webhook; a reply on a review thread, which
	// GitHub records as a review with no body; a review with no submission time; anyone else's.
	// GitHub stamps the review and the daemon stamps the completion, and no ordering between the two
	// avoids comparing their clocks: the delivery order is the race itself, and the completion has no
	// GitHub-side stamp. answerSkew bounds the two clocks' disagreement, far above an NTP-synced skew
	// and far below the architect's request and the reviewer's reply after the stuck notice.
	before := ""
	if !e.byReviewApp(fact.Author) || fact.Body == "" || !fact.SubmittedAt.After(reviewer.CompletedAt.Add(answerSkew)) {
		before = stuckReview(*issue, reviewer, pr)
	}
	// Only changes_requested and approved decide anything; a comment orders nothing either, so a
	// comment written after a decision but delivered before it cannot make the decision look old.
	state := strings.ToLower(fact.State)
	if state != "changes_requested" && state != "approved" {
		return intake.Result{}, e.tellStuckReview(ctx, tx, *issue, reviewer, pr, before)
	}
	// Deciding reviews are ordered by when they were submitted, then by GitHub's review id
	// (record.ReviewOrder). Among reviews that all carry a time and arrive before the round ends,
	// the newest decides whatever order they are delivered in; with a review without a time in
	// play, the outcome can depend on delivery order. A round ends once both of its halves are in,
	// so a review arriving after that decides nothing for it. The pull request keeps the newest it
	// has had across rounds, so a review submitted before one already processed - redelivered, or
	// from an earlier round - records nothing. A review without an id is ordered by when it
	// arrives.
	if fact.ID != 0 {
		order := record.ReviewOrder{SubmittedAt: fact.SubmittedAt, ID: fact.ID}
		if !order.After(pr.ReviewSeen) {
			return intake.Result{}, nil
		}
		pr.ReviewSeen = order
		if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
			return intake.Result{}, err
		}
	}
	// The decision belongs to the review round, not to the pull request's head: the reviewer's own
	// handoff push is a new head, and the round must still know, when the reviewer completes, what
	// the review decided, on which head, and what it said for the next round's implementer. The
	// round is open while the issue is in reviewing, and while it is held from reviewing, since the
	// retry puts it back there with the reviewer's work kept. A review outside the round decides
	// nothing and counts no round. An approval is judged against the head it names when the
	// review ends (classify.ApprovalStands).
	held := issue.Phase == phase.Held && issue.Hold != nil && issue.Hold.From == phase.Reviewing
	if issue.Phase != phase.Reviewing && !held {
		return intake.Result{}, nil
	}
	reviewer.Decision = &record.ReviewDecision{State: state, Body: fact.Body, Head: fact.CommitID}
	if err := e.store.PutPhase(ctx, tx, reviewer); err != nil {
		return intake.Result{}, err
	}
	ended, err := e.advanceReview(ctx, tx, *issue, pr)
	if err != nil || ended {
		return intake.Result{}, err
	}
	return intake.Result{}, e.tellStuckReview(ctx, tx, *issue, reviewer, pr, before)
}

// advanceReview ends a review once both of its halves are in, and says whether it did: the
// reviewer's completion, which is its handoff landing — part of its phase's contract, as every
// role's is — and the decision it posted on GitHub, recorded on the round. Either may arrive
// second, and an approval also waits for the head's checks to settle green, which the settlement
// asks about in turn. A review whose reviewer never completes is not ended by the decision alone:
// the reviewer's pane gets one follow-up turn when a turn ends with its phase open (pi-envoy's
// phase-stall check), and past that the issue stays in reviewing, as a tester's that never
// completes stays in testing. A round its reviewer completed that no decision ends stays in
// reviewing too, and its architect is told (tellStuckReview).
func (e *Engine) advanceReview(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) (bool, error) {
	// Only a round in reviewing ends: one held from reviewing ends after the retry restores it.
	if issue.Phase != phase.Reviewing {
		return false, nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil || row.HandoffCommit == "" || row.Decision == nil {
		return false, err
	}
	switch row.Decision.State {
	case "changes_requested":
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return false, err
		}
		if err := e.recordRound(ctx, tx, issue.Key); err != nil {
			return false, err
		}
		return true, e.transition(ctx, tx, issue, TriggerReviewRejected, "", row, pr, row.Decision.Body)
	case "approved":
		if pr == nil || classify.HeadVerdict(*pr) != "green" || !classify.ApprovalStands(*pr, row.Decision.Head) {
			return false, nil
		}
		return true, e.transition(ctx, tx, issue, TriggerReviewApproved, "", row, pr, "")
	}
	return false, nil
}

// stuckReview says why issue's review round stays open with nothing on its way that would end it,
// its reviewer (row) having completed it: no review decided it (a COMMENT decides nothing, and
// neither does a review in any state but approved or changes_requested), or its approval cannot end
// it, CI being red at the head, which in reviewing only the round decides (classify.RedSendsBack),
// or the head carrying code the approved head does not (classify.ApprovalStands). It is "" for an
// issue not in reviewing, a round still owed its reviewer's completion, one that ends
// (advanceReview), and one waiting on the head's checks or on a head a push that may change code is
// bringing. The reason names the head, since the decision the round needs is of the head.
func stuckReview(issue record.Issue, row record.PhaseRow, pr *record.PullRequest) string {
	if issue.Phase != phase.Reviewing || row.HandoffCommit == "" || pr == nil {
		return ""
	}
	if row.Decision == nil {
		return fmt.Sprintf("the reviewer completed its round on pull request #%d with no review that decides it: only an APPROVE of head %s or a REQUEST_CHANGES ends the round, and a COMMENT decides nothing",
			pr.Number, pr.HeadSHA)
	}
	if row.Decision.State != "approved" {
		return ""
	}
	switch classify.HeadVerdict(*pr) {
	case "red":
		return fmt.Sprintf("the reviewer approved %s on pull request #%d, but %s; a red at the reviewer's own head is its round's to decide, with a REQUEST_CHANGES naming the failing checks",
			row.Decision.Head, pr.Number, redAt(*pr))
	case "green":
		if classify.ApprovalStands(*pr, row.Decision.Head) || classify.CodeOnItsWay(*pr) {
			return ""
		}
		return fmt.Sprintf("the reviewer approved %s on pull request #%d, which does not approve head %s: a push since may have changed code, so only an APPROVE of %s or a REQUEST_CHANGES ends the round",
			row.Decision.Head, pr.Number, pr.HeadSHA, pr.HeadSHA)
	}
	return ""
}

// tellStuckReview tells issue's architect that its review round is stuck (stuckReview, with row
// the reviewer's and pr the pull request as the fact left them), unless it was stuck for the same
// reason, before, when the fact arrived: the reviewer's own completion or review passes "", since
// each of its acts that leaves the round stuck is told. The issue stays in reviewing; the architect
// asks the reviewer for the decision. Linger holds a member of a closed tree where it stood
// (record.TreeLingers), so nothing is told of it.
func (e *Engine) tellStuckReview(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest, before string) error {
	reason := stuckReview(issue, row, pr)
	if reason == "" || reason == before {
		return nil
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	return e.notice(ctx, tx, issue.Key, record.Notice{Kind: "review-stuck", Role: claim.RoleReviewer, Phase: phase.Reviewing, Reason: reason})
}

// tellStuckReviewSince is tellStuckReview for a fact that is not the reviewer's own and that moved
// the pull request from prior to pr: it tells only when the fact changed why the round is stuck.
func (e *Engine) tellStuckReviewSince(ctx context.Context, tx pgx.Tx, issue record.Issue, prior, pr *record.PullRequest) error {
	if issue.Phase != phase.Reviewing {
		return nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return err
	}
	return e.tellStuckReview(ctx, tx, issue, row, pr, stuckReview(issue, row, prior))
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
