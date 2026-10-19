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
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// checks applies a CI settlement to the pull request and then decides what it moves
// (decideChecks).
func (e *Engine) checks(ctx context.Context, tx pgx.Tx, fact intake.PullRequestChecks) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	// A handoff push can start no CI of its own (GitHub's skip-checks trailer), so the settlement
	// that stands for the head can be of the code head it replaced, arriving after it. A
	// settlement is for the commit it names, never the head by default.
	candidate := classify.SettlementCandidate{Head: fact.HeadSHA, CheckRuns: fact.CheckRuns, Generation: fact.Generation, Snapshot: fact.Snapshot, Failing: fact.Failing, Cancelled: fact.Cancelled}
	var stands bool
	if *pr, stands = classify.SettlementFor(*pr, candidate); !stands {
		return intake.Result{}, nil
	}
	var applied bool
	*pr, applied = classify.ApplySettlement(*pr, candidate)
	if !applied {
		return intake.Result{}, nil
	}
	return intake.Result{}, e.decideChecks(ctx, tx, pr, false)
}

// requiredChecks records what the pull request's base branch requires, as the daemon read it
// (intake.RequiredChecks): its required checks, and its required workflows with their runs'
// results at the head read. It then decides what the head's verdict now comes to (decideChecks):
// only a required check or workflow makes a head red (classify.HeadVerdict), so a new read can
// send work back for a newly required check or a required workflow that failed, leave standing a
// head red only on checks the base branch never required, and give a head its first verdict when
// its set was never read. A read that says what is recorded changes nothing.
func (e *Engine) requiredChecks(ctx context.Context, tx pgx.Tx, fact intake.RequiredChecks) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil || pr.RequiredReadUnchanged(fact.Names, fact.Workflows, fact.WorkflowsHead) {
		return intake.Result{}, err
	}
	pr.Required = append([]string{}, fact.Names...)
	pr.Workflows = append([]record.RequiredWorkflow{}, fact.Workflows...)
	pr.WorkflowsHead = fact.WorkflowsHead
	return intake.Result{}, e.decideChecks(ctx, tx, pr, true)
}

// mergeability records what GitHub's last read found for whether the pull request's head can be
// merged into its base (intake.PullRequestMergeability, pullRequestMergeability's own GitHub
// answer). In awaiting_merge it withdraws a conflicting head the way decideChecks's AwaitingMerge
// case withdraws a red one (classify.ConflictWithdrawsReady, the same TriggerChecksRed
// transition), since GitHub computes no merge ref for a conflicting head and no CI result will
// ever arrive for it. It decides on every read, not only a changed one: a conflict recorded
// before the issue reaches awaiting_merge - in testing, reviewing, retro or merging, where
// nothing moves yet, since the merger's READY check posts a conflicting head whose required
// checks all succeeded - must still withdraw the READY once the issue gets there. Outside
// awaiting_merge the read is recorded and nothing moves: a conflict is no red for RedSendsBack to
// act on (GitHub runs no checks on it for a verdict to stand), and the tester or implementer
// already at work will see it on its own pass or push. Leaving awaiting_merge is what stops a
// repeated CONFLICTING read from withdrawing twice.
func (e *Engine) mergeability(ctx context.Context, tx pgx.Tx, fact intake.PullRequestMergeability) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	if pr.Mergeability != fact.Mergeable {
		pr.Mergeability = fact.Mergeable
		if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
			return intake.Result{}, err
		}
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil || issue.Phase != phase.AwaitingMerge || !classify.ConflictWithdrawsReady(*pr) {
		return intake.Result{}, err
	}
	reason := fmt.Sprintf("the head conflicts with %s: GitHub runs no checks on it; merge %s forward", fact.Base, fact.Base)
	return intake.Result{}, e.transition(ctx, tx, *issue, TriggerChecksRed, "", record.PhaseRow{}, pr, reason)
}

// decideChecks records pr, its checks verdict changed by a CI settlement or a new read of what the
// base branch requires (requiredRead), and decides, with the issue's phase in hand, what it moves.
// CI is the daemon's own rule, read beside the review round and never by it (standingRed): in
// testing, reviewing, retro and merging a red that stands for the head sends the work back to
// implementing, whatever the tester has found, the round holds or the merger is doing, and takes
// the phase from a role still at work in it (takenOver). The reviewer's approval of the head ends
// its round on its own (reviewRound), as the reviewer reads the code commit's checks on GitHub
// before it approves. In awaiting_merge, the head's own red withdraws the READY
// (classify.RedWithdrawsReady), and the transition tells the merge queue role so (withdrawReady). A
// red the tester planned - its own failing tests, pushed for the implementer to make pass - sends
// nothing back (PlannedRed), and neither does a red in implementing, where the implementer is
// already at work. The implementer's task and the architect's checks-red notice name the red
// required checks and workflows, and the implementer's next push is a counted fix attempt, as any
// new head on a red verdict is (classify.AdvancePullRequestHead).
//
// At an exhausted fix-attempt count, a red within the attempt the count last counted - the counted
// head, or a head pushed on top of it until a push lands on a green verdict - sends nothing back
// before awaiting_merge (classify.FixAttemptCapHolds): the architect decides what follows. A red at
// a head pushed onto green moves the issue as below the cap. In awaiting_merge a red always
// withdraws the READY, whatever the count: no implementer loops there for the cap to bound, and a
// READY must not stand on a head GitHub will not merge. The count is posted and told to the
// architect once (classify.BlockFixAttempt), by the first red at that count that sent nothing back:
// a red that sent the work back is told by its checks-red, and the next push onto it raises the
// count.
func (e *Engine) decideChecks(ctx context.Context, tx pgx.Tx, pr *record.PullRequest, requiredRead bool) error {
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil {
		return err
	}
	moved := false
	switch {
	case issue == nil:
	case issue.Phase == phase.AwaitingMerge:
		if !classify.RedWithdrawsReady(*pr) {
			break
		}
		// A required workflow's result is as old as the pass's last read of the runs, up to two
		// minutes: a run re-run green since, which READY found green on GitHub, still reads red until
		// the next read. So a settlement, which reads no run, withdraws only a red a required check
		// makes. A red only the workflows make is left to the pass's next read, which the cleared
		// head the runs were read at makes a change (record.RequiredReadUnchanged), so it withdraws
		// the READY if the run is still red, and leaves it if the run passed; that read decides the
		// count's report too.
		if !requiredRead && classify.RedOnlyByWorkflows(*pr) {
			pr.WorkflowsHead = ""
			return e.store.PutPullRequest(ctx, tx, *pr)
		}
		if moved, err = e.sendBack(ctx, tx, *issue, pr); err != nil {
			return err
		}
	default:
		if moved, err = e.standingRed(ctx, tx, *issue, pr); err != nil {
			return err
		}
	}
	if moved {
		return nil
	}
	return e.reportExhaustedCount(ctx, tx, issue, pr)
}

// sendBack moves issue back to implementing on the red standing for pr's head (TriggerChecksRed),
// and says whether it moved: a phase with no such row, or a lingering tree, stays where it is.
func (e *Engine) sendBack(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) (bool, error) {
	if err := e.transition(ctx, tx, issue, TriggerChecksRed, "", record.PhaseRow{}, pr, redAt(*pr)); err != nil {
		return false, err
	}
	after, err := e.store.Issue(ctx, tx, issue.Key)
	if err != nil || after == nil {
		return false, err
	}
	return after.Phase != issue.Phase, nil
}

// reportExhaustedCount posts an exhausted fix-attempt count on the issue and tells the architect,
// once per count (classify.BlockFixAttempt). Linger holds a member of a closed tree where it stood
// (record.TreeLingers): the count is recorded on the pull request, and nothing is posted on the
// issue or told to its architect.
func (e *Engine) reportExhaustedCount(ctx context.Context, tx pgx.Tx, issue *record.Issue, pr *record.PullRequest) error {
	var blocked bool
	if *pr, blocked = classify.BlockFixAttempt(*pr, e.cfg.MaxFixAttempts); !blocked {
		return nil
	}
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return err
	}
	if issue != nil {
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return err
		}
	}
	message := fmt.Sprintf("Pull request #%d reached max_fix_attempts=%d.", pr.Number, e.cfg.MaxFixAttempts)
	if err := e.enqueue(ctx, tx, pr.Issue, record.MessagePost{Body: message}); err != nil {
		return err
	}
	return e.notice(ctx, tx, pr.Issue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: message})
}

// redAt is what the red verdict standing for the pull request's head says: the head, and each check
// and workflow the base branch requires that failed there (classify.HeadChecks).
func redAt(pr record.PullRequest) string {
	checks, _ := classify.HeadChecks(pr)
	var red []string
	for _, check := range checks {
		if check.Red() {
			red = append(red, check.Name)
		}
	}
	return "CI is red at " + pr.HeadSHA + ": " + strings.Join(red, ", ")
}

// answerSkew bounds how far GitHub's clock, which stamps a review's submission, and the daemon's,
// which stamps the reviewer's completion (record.PhaseRow.CompletedAt), may disagree. No ordering
// between the two avoids comparing those clocks: the delivery order is the race itself, and the
// completion has no GitHub-side stamp. GitHub's review ids come closest, but GitHub assigns one
// when a review is created, not when it is submitted, so a draft created before the completion
// and submitted after it would read as already seen; reading them would also put a GitHub call on
// the completion path. The bound is sized to skew alone, since a real answer follows the completion
// within the minute the stuck notice, the architect's request and the reviewer's review take. Its
// two costs: a GitHub clock more than answerSkew ahead makes a review submitted before the
// completion read as the reviewer's answer, and tell again; an answer submitted within answerSkew
// of the completion reads as delivered late, and tells only when the round's stuck cause or head
// changes (stuckAs).
const answerSkew = 10 * time.Second

// byReviewer says whether review is the reviewer's own review-App session, not merely a review the
// review App submitted: the controller now holds the review App's token too, so every role's
// review arrives under the same bot login (Config.ReviewAppLogin). What tells the reviewer's
// review from any other review-App session's - the controller's, an architect's, a sibling tree's
// role - is the Legion footer every Legion role appends to a pull-request review
// (intake.PullRequestReview.LegionSession), matched against reviewerSession, the session recorded
// on the reviewer's own claim (record.Store.ClaimSession). With no session recorded - a daemon
// whose claims predate this, or a reviewer claim with no row - the login decides by itself, as it
// always has, since there is nothing recorded to tell the sessions apart.
func (e *Engine) byReviewer(review intake.PullRequestReview, reviewerSession string) bool {
	return e.byReviewApp(review.Author) && (reviewerSession == "" || review.LegionSession() == reviewerSession)
}

// DecidesRound says whether review may decide a review round: it is the reviewer's own review-App
// session (byReviewer, with reviewerSession as ReviewerSession reads it), or its author has write
// access or higher to the repository, which intake read from GitHub before the fact reached this
// transaction (intake.ResolveAuthorCanWrite, for a webhook's review and the daemon's own read of a
// pull request's reviews alike). On a public repository any account can review a pull request, so
// anyone else's review decides nothing. GitHub's 404, or a 403 that is not its rate limit, reads as
// no write access; any other failed read is retried before the review reaches here. A review-App
// review that is not the reviewer's own session decides nothing either: it is set aside, not
// counted as a writer's or dropped as a stranger's. The daemon's read of a pull request's reviews
// asks this too, so it applies only a review this engine would take.
func (e *Engine) DecidesRound(review intake.PullRequestReview, reviewerSession string) bool {
	return e.byReviewer(review, reviewerSession) || review.AuthorCanWrite
}

// ReviewerSession is the session recorded on the claim of issue's reviewer, the session a review-App
// review's Legion footer must name to be the reviewer's own (byReviewer): "" when the reviewer's
// row holds no claim, the claim has no row, or its session is empty, in which case the login decides
// by itself. The daemon's read of a pull request's reviews asks it before DecidesRound, as review
// does, so both judge a review-App review alike.
func (e *Engine) ReviewerSession(ctx context.Context, tx pgx.Tx, issue string) (string, error) {
	reviewer, err := e.phaseRow(ctx, tx, issue, claim.RoleReviewer)
	if err != nil {
		return "", err
	}
	return e.claimSession(ctx, tx, reviewer)
}

// claimSession is the session recorded on row's claim, "" when the row holds no claim.
func (e *Engine) claimSession(ctx context.Context, tx pgx.Tx, row record.PhaseRow) (string, error) {
	if row.Claim == "" {
		return "", nil
	}
	return e.store.ClaimSession(ctx, tx, row.Claim)
}

// reviewersAnswer says whether fact is the reviewer's answer to a round it completed undecided: a
// review from the reviewer's own review-App session (byReviewer), with a body - GitHub records a
// reply on a review thread as a review with no body - more than answerSkew after the completion
// was applied. A review submitted before the completion but delivered after it is not, since the
// completion comes through the API and the review by webhook; nor is one with no submission time,
// or anyone else's, the review App's other sessions included.
func (e *Engine) reviewersAnswer(fact intake.PullRequestReview, reviewer record.PhaseRow, reviewerSession string) bool {
	return e.byReviewer(fact, reviewerSession) && fact.Body != "" && fact.SubmittedAt.After(reviewer.CompletedAt.Add(answerSkew))
}

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
	// reviewerSession is the session recorded on the reviewer's claim (claimSession), "" when none
	// is, in which case byReviewer decides by the login alone. It is read only for a review the
	// review App submitted: no other author's review ever reaches the compare.
	var reviewerSession string
	if e.byReviewApp(fact.Author) {
		reviewerSession, err = e.claimSession(ctx, tx, reviewer)
		if err != nil {
			return intake.Result{}, err
		}
	}
	// The reviewer's answer tells whenever it leaves the round stuck; any other review, only when it
	// changes how the round is stuck.
	before, by := e.reviewRound(*issue, reviewer, pr), byOtherReview
	if e.reviewersAnswer(fact, reviewer, reviewerSession) {
		before, by = round{}, byAnswer
	}
	// Only changes_requested and approved decide anything, and only from the reviewer's own
	// review-App session or an account with write access to the repository (DecidesRound). Any
	// other review orders nothing either, so a comment written after a decision but delivered
	// before it, or an outsider's review, cannot make the decision look old.
	state := strings.ToLower(fact.State)
	decides := fact.Decides()
	if decides && !e.DecidesRound(fact, reviewerSession) {
		if e.byReviewApp(fact.Author) {
			e.warnOnCommit(ctx, "workflow: a review decides nothing: the review App submitted it from a session that is not the reviewer's",
				"issue", issue.Key, "pull_request", pr.Number, "state", state, "footer_session", fact.LegionSession(),
				"reviewer_session", reviewerSession, "reviewer_claim", string(reviewer.Claim), "body_truncated", fact.BodyTruncated)
		} else {
			e.logOnCommit(ctx, "workflow: a review decides nothing: its author is neither the review App nor an account with write access to the repository",
				"issue", issue.Key, "pull_request", pr.Number, "author", fact.Author, "state", state)
		}
		decides = false
	}
	if decides && reviewerSession == "" && e.byReviewApp(fact.Author) {
		e.logOnCommit(ctx, "workflow: the reviewer's session is not recorded; the review App's login decides",
			"issue", issue.Key, "pull_request", pr.Number, "reviewer_claim", string(reviewer.Claim))
	}
	if !decides {
		_, err := e.settleRound(ctx, tx, *issue, reviewer, pr, before, by)
		return intake.Result{}, err
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
		order := fact.Order()
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
	_, err = e.settleRound(ctx, tx, *issue, reviewer, pr, before, by)
	return intake.Result{}, err
}

// The fact that wrote a review-stuck notice, its summary. The architect prompt
// (prompts/go/architect-common.md) reads a notice the reviewer's completion or answer wrote after
// the architect asked as the reviewer answering, and every other as news about the round.
const (
	byCompletion  = "the reviewer's completion"
	byAnswer      = "the reviewer's review"
	byOtherReview = "a review that is not the reviewer's answer"
	byPush        = "a push"
)

// roundOutcome is what a review round comes to after a fact (reviewRound).
type roundOutcome int

const (
	// roundOpen is a round still owed its reviewer's completion or its decision, or waiting on a
	// head a push that may change code is bringing; or an issue not in reviewing, since one held
	// from reviewing ends only once the retry restores it.
	roundOpen roundOutcome = iota
	// roundApproved ends the round: its reviewer completed, and its approval approves the head's code
	// (classify.ApprovalStands). No CI verdict is read: the reviewer reads the code commit's checks on
	// GitHub before it approves, and a red at the head is the daemon's own rule (standingRed).
	roundApproved
	// roundRejected ends the round: its reviewer completed, and it requested changes.
	roundRejected
	// roundStuck is a round its reviewer completed that nothing on its way will end (stuckCause).
	roundStuck
)

// stuckCause is why a completed round is stuck.
type stuckCause int

const (
	// stuckUndecided: no review decided it. A COMMENT decides nothing, and neither does a review in
	// any state but approved or changes_requested, nor one DecidesRound does not count.
	stuckUndecided stuckCause = iota + 1
	// stuckApprovedOtherCode: its approval is of a head whose code the current head may not carry.
	stuckApprovedOtherCode
)

// round is reviewRound's account of a review round. A stuck round names its cause, the head it is
// stuck at, and what the architect and a restarted reviewer are told of it.
type round struct {
	outcome roundOutcome
	cause   stuckCause
	head    string
	reason  string
}

// stuckAs says whether r is stuck in the same way as other: the same cause at the same head. A fact
// that is not the reviewer's own tells only when the round is stuck in a new way, so a second push
// classified late at the same head tells nothing.
func (r round) stuckAs(other round) bool {
	return r.outcome == roundStuck && other.outcome == roundStuck && r.cause == other.cause && r.head == other.head
}

// reviewRound is what issue's review round comes to, row being its reviewer's and pr its pull
// request as a fact left them. A round ends once both of its halves are in: the reviewer's
// completion, which is its handoff landing - part of its phase's contract, as every role's is - and
// the decision it posted on GitHub, recorded on the round. Either may arrive second. The round
// reads no CI verdict: CI is the daemon's separate rule, which sends a red head back to implementing
// from reviewing as from any phase before READY, on the fact that makes the red stand (decideChecks,
// standingRed), and the reviewer reads the code commit's checks on GitHub before it approves. A
// round whose reviewer never completes is not ended by the decision alone: the reviewer's pane gets
// a follow-up turn when a turn ends with its phase open (pi-legion's phase-stall check, at most two
// a stall), and past that the issue stays in reviewing, as a tester's that never completes stays
// in testing. A completed round that nothing on its way would end is stuck, its reason naming the
// head, since the decision it needs is of the head: no review decided it (stuckUndecided), or its
// approval is of other code than the head's (stuckApprovedOtherCode). An approval of the head a
// push that may change code is still on its way from (classify.CodeOnItsWay) keeps the round open:
// the push's new head decides whether the approval stands.
func (e *Engine) reviewRound(issue record.Issue, row record.PhaseRow, pr *record.PullRequest) round {
	if issue.Phase != phase.Reviewing {
		return round{}
	}
	completed := row.HandoffCommit != ""
	decided := ""
	if row.Decision != nil {
		decided = row.Decision.State
	}
	if completed && decided == "changes_requested" {
		return round{outcome: roundRejected}
	}
	if pr == nil {
		return round{}
	}
	switch {
	case completed && decided == "approved" && classify.ApprovalStands(*pr, row.Decision.Head):
		return round{outcome: roundApproved}
	case !completed:
		return round{}
	case decided == "":
		return round{outcome: roundStuck, cause: stuckUndecided, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer completed its round on pull request #%d with no review that decides it: only an APPROVE of head %s or a REQUEST_CHANGES ends the round, from an account with write access to the repository or from the reviewer's own session as the review App (its Legion footer naming the session recorded on the reviewer's claim: a review-App review with no footer or another session's is set aside and decides nothing), and a COMMENT decides nothing",
				pr.Number, pr.HeadSHA)}
	case !classify.ApprovalStands(*pr, row.Decision.Head) && !classify.CodeOnItsWay(*pr):
		return round{outcome: roundStuck, cause: stuckApprovedOtherCode, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, which does not approve head %s: a push since may have changed code, so only an APPROVE of %s or a REQUEST_CHANGES ends the round",
				row.Decision.Head, pr.Number, pr.HeadSHA, pr.HeadSHA)}
	}
	return round{}
}

// standingRed is the CI rule, applied to pr's head, and says whether it moved the issue: a red that
// stands for the head (classify.RedSendsBack) sends the work back to implementing, whatever the
// tester has found, the round holds or the merger is doing, unless the fix-attempt cap holds the
// red (classify.FixAttemptCapHolds): then nothing moves on it, and the architect decides what
// follows (decideChecks's count report), a review round's own decision governing meanwhile. The
// rule runs before awaiting_merge: there only the head's own red withdraws the READY
// (classify.RedWithdrawsReady, decideChecks's AwaitingMerge case), never one carried to the READY
// head from the code head a .legion/-only push replaced, since READY found the head's own CI green
// on GitHub, which is what GitHub merges by. The invariant is that the rule is applied by whichever
// fact makes a red stand for the head, in whatever phase the issue is when it does, and never by a
// review or a completion, which change nothing about the verdict and read none. A red comes to
// stand by a settlement or a read of the required set, the facts that change the verdict
// (decideChecks); by a push classified late, or a head arriving over a push already known, either
// of which can complete the carry of a code head's settlement to the handoff head above it
// (classify.SettlementFor; push, pullRequestSynchronized); and by a hold's retry, which restores a
// phase a settlement reached while the issue was held, where nothing moves (retryOrEscalate). A
// fact that finds no red standing applies nothing, so each applies the rule before whatever else
// it does.
func (e *Engine) standingRed(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) (bool, error) {
	if issue.Phase == phase.AwaitingMerge || pr == nil || !classify.RedSendsBack(*pr) || classify.FixAttemptCapHolds(*pr, e.cfg.MaxFixAttempts) {
		return false, nil
	}
	return e.sendBack(ctx, tx, issue, pr)
}

// settleRound acts on what issue's review round comes to (reviewRound, with row its reviewer's and
// pr its pull request as the fact left them), and says whether it moved the issue. An ended round
// moves on, its request for changes back to implementing; a stuck round is told to the architect,
// in a notice whose summary is by, the fact that wrote it - unless the round was already stuck the
// same way (stuckAs) before that fact, which is before. The reviewer's completion and answer pass
// an empty before, so each that leaves the round stuck is told. The issue stays in reviewing and
// the architect asks the reviewer, at the notice's topic, for the decision. Linger holds a member
// of a closed tree where it stood (record.TreeLingers).
func (e *Engine) settleRound(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest, before round, by string) (bool, error) {
	r := e.reviewRound(issue, row, pr)
	switch r.outcome {
	case roundRejected:
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return false, err
		}
		return true, e.transition(ctx, tx, issue, TriggerReviewRejected, "", row, pr, row.Decision.Body)
	case roundApproved:
		return true, e.transition(ctx, tx, issue, TriggerReviewApproved, "", row, pr, "")
	case roundStuck:
		if r.stuckAs(before) {
			return false, nil
		}
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return false, err
		}
		project, err := claim.ProjectToken(issue.Project)
		if err != nil {
			return false, err
		}
		reviewer, err := claim.NewToken(project, issue.Key, claim.RoleReviewer)
		if err != nil {
			return false, err
		}
		return false, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "review-stuck", Role: claim.RoleReviewer, Phase: phase.Reviewing,
			Summary: by, Reason: r.reason, Topic: notify.RoleTopicPrefix + string(reviewer)})
	}
	return false, nil
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
