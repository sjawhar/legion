package workflow

import (
	"context"
	"fmt"
	"slices"
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
	prior := *pr
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
	return intake.Result{}, e.decideChecks(ctx, tx, pr, prior, byChecks)
}

// requiredChecks records what the pull request's base branch requires, as the daemon read it
// (intake.RequiredChecks): its required checks, and its required workflows with their runs'
// results at the head read. It then decides what the head's verdict now comes to (decideChecks):
// only a required check or workflow makes a head red (classify.HeadVerdict), so a new read can end
// a round that waited on a red the base branch never required or on a required workflow's run, send
// work back for a newly required check or a required workflow that failed, and give a head its
// first verdict when its set was never read. A read that says what is recorded changes nothing.
func (e *Engine) requiredChecks(ctx context.Context, tx pgx.Tx, fact intake.RequiredChecks) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil || pr.RequiredReadUnchanged(fact.Names, fact.Workflows, fact.WorkflowsHead) {
		return intake.Result{}, err
	}
	prior := *pr
	pr.Required = append([]string{}, fact.Names...)
	pr.Workflows = append([]record.RequiredWorkflow{}, fact.Workflows...)
	pr.WorkflowsHead = fact.WorkflowsHead
	return intake.Result{}, e.decideChecks(ctx, tx, pr, prior, byRequired)
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
// awaiting_merge the read is recorded and nothing moves: no round is open to decide a conflict
// the way RedSendsBack decides a red, and the tester or implementer already at work will see it
// on its own pass or push. Leaving awaiting_merge is what stops a repeated CONFLICTING read from
// withdrawing twice.
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

// decideChecks records pr, its checks verdict changed from prior's by a CI settlement or a new
// read of what the base branch requires (the fact by names), and decides, with the issue's phase
// in hand, what it moves. An exhausted fix-attempt count is posted and told to the architect. In
// reviewing, the round decides what the verdict comes to (reviewRound, settleRound): it can be what
// an approval waits for, a red at a code head sends the work back, and a round the verdict leaves
// stuck another way than before is told. In awaiting_merge, the head's own red withdraws the READY
// (classify.RedWithdrawsReady), and the transition tells the merge queue role so (withdrawReady).
// In testing, a red at a code head (classify.RedSendsBack) sends the work back. A red only the
// project's declared review workflows make (Config.ReviewWorkflows) sends nothing back from testing
// or reviewing (classify.RedOnlyByReviewWorkflows): the tester finishes, and the reviewer's round
// adjudicates the workflows' findings, its start task naming the red (the tester's pass) and its
// stuck round telling the architect (stuckApprovedWorkflowRed). Any other red required workflow
// sends the work back as a red required check does. The implementer's task and the architect's
// checks-red notice name the red required checks and workflows, and the implementer's next push is
// a counted fix attempt, as any new head on a red verdict is (classify.AdvancePullRequestHead).
func (e *Engine) decideChecks(ctx context.Context, tx pgx.Tx, pr *record.PullRequest, prior record.PullRequest, by string) error {
	var blocked bool
	*pr, blocked = classify.BlockFixAttempt(*pr, e.cfg.MaxFixAttempts)
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return err
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil {
		return err
	}
	if blocked {
		// Linger holds a member of a closed tree where it stood (record.TreeLingers): the exhausted
		// count is recorded on the pull request, and nothing is posted on the issue or told to its
		// architect.
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
	if issue == nil {
		return nil
	}
	switch issue.Phase {
	case phase.Reviewing:
		reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
		if err != nil {
			return err
		}
		_, err = e.settleRound(ctx, tx, *issue, reviewer, pr, e.reviewRound(*issue, reviewer, &prior), by)
		return err
	case phase.AwaitingMerge:
		if !classify.RedWithdrawsReady(*pr) {
			return nil
		}
		// A required workflow's result is as old as the pass's last read of the runs, up to two
		// minutes: a run re-run green since, which READY found green on GitHub, still reads red until
		// the next read. So a settlement, which reads no run, withdraws only a red a required check
		// makes. A red only the workflows make is left to the pass's next read, which the cleared
		// head the runs were read at makes a change (record.RequiredReadUnchanged), so it withdraws
		// the READY if the run is still red, and leaves it if the run passed.
		if by != byRequired && classify.RedOnlyByWorkflows(*pr) {
			pr.WorkflowsHead = ""
			return e.store.PutPullRequest(ctx, tx, *pr)
		}
	default:
		if !classify.RedSendsBack(*pr, e.cfg.ReviewWorkflows) {
			return nil
		}
	}
	return e.transition(ctx, tx, *issue, TriggerChecksRed, "", record.PhaseRow{}, pr, redAt(*pr))
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

// reviewWorkflowsToAdjudicate is what a red only declared review workflows make
// (classify.RedOnlyByReviewWorkflows) asks of the review round, after the red itself (redAt): the
// reviewer replies on each open thread the workflows' runs opened, tells the implementer which of
// them it accepted (an Envoy message to the implementer's role topic, or its review body), since
// only the pull request's author's App may resolve a thread and the implementer resolves them with
// plain gh, and re-runs each failed run once they are resolved, since such a workflow starts only
// on a push; its approval ends the round once the head reads green.
const reviewWorkflowsToAdjudicate = "; only declared review workflows are red, so their findings are the review round's to decide: " +
	"the reviewer replies on each of their open threads (its reason, or a REQUEST_CHANGES naming the defect), " +
	"tells the implementer which threads it accepted (Envoy to the implementer's role topic, or its review body), " +
	"which the implementer resolves with plain gh as the pull request's author, " +
	"and re-runs each failed run (rerun-failed-jobs) once they are resolved, and its approval of the head ends the round once the head reads green"

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
// review arrives under the same bot login (`legion-reviewer[bot]`). What tells the reviewer's
// review from any other review-App session's - the controller's, an architect's, a sibling tree's
// role - is the Legion footer every Legion role appends to a pull-request review
// (intake.PullRequestReview.LegionSession), matched against reviewerSession, the session recorded
// on the reviewer's own claim (record.Store.ClaimSession). With no session recorded - a daemon
// whose claims predate this, or a reviewer claim with no row - the login decides by itself, as it
// always has, since there is nothing recorded to tell the sessions apart.
func (e *Engine) byReviewer(review intake.PullRequestReview, reviewerSession string) bool {
	return e.byReviewApp(review.Author) && (reviewerSession == "" || review.LegionSession() == reviewerSession)
}

// decidesRound says whether review may decide a review round: it is the reviewer's own review-App
// session (byReviewer), or its author has write access or higher to the repository, which intake
// read from GitHub before the fact reached this transaction
// (intake.PullRequestReview.AuthorCanWrite). On a public repository any account can review a pull
// request, so anyone else's review decides nothing. GitHub's 404, or a 403 that is not its rate
// limit, reads as no write access; any other failed read is retried before the review reaches
// here. A review-App review that is not the reviewer's own session decides nothing either: it is
// set aside, not counted as a writer's or dropped as a stranger's.
func (e *Engine) decidesRound(review intake.PullRequestReview, reviewerSession string) bool {
	return e.byReviewer(review, reviewerSession) || review.AuthorCanWrite
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
	// reviewerSession is the session recorded on the reviewer's claim, "" when the row has no claim,
	// the claim has no row, or its session is empty, in which case byReviewer decides by the login
	// alone. It is read only for a review the review App submitted: no other author's review ever
	// reaches the compare.
	var reviewerSession string
	if e.byReviewApp(fact.Author) && reviewer.Claim != "" {
		reviewerSession, err = e.store.ClaimSession(ctx, tx, reviewer.Claim)
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
	// review-App session or an account with write access to the repository (decidesRound). Any
	// other review orders nothing either, so a comment written after a decision but delivered
	// before it, or an outsider's review, cannot make the decision look old.
	state := strings.ToLower(fact.State)
	decides := fact.Decides()
	if decides && !e.decidesRound(fact, reviewerSession) {
		if e.byReviewApp(fact.Author) {
			e.logOnCommit(ctx, "workflow: a review decides nothing: the review App submitted it from a session that is not the reviewer's",
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
	byChecks      = "a CI result"
	byRequired    = "a read of the checks the base branch requires"
	byPush        = "a push"
)

// roundOutcome is what a review round comes to after a fact (reviewRound).
type roundOutcome int

const (
	// roundOpen is a round still owed its reviewer's completion or its decision, or waiting on the
	// head's checks or on a head a push that may change code is bringing; or an issue not in
	// reviewing, since one held from reviewing ends only once the retry restores it.
	roundOpen roundOutcome = iota
	// roundApproved ends the round: its reviewer completed, and its approval approves the head's code
	// (classify.ApprovalStands) on green checks.
	roundApproved
	// roundRejected ends the round: its reviewer completed, and it requested changes.
	roundRejected
	// roundSentBack is CI red at a head a push that may change code made (classify.RedSendsBack): the
	// work goes back to implementing whatever the round holds, since only a red at the reviewer's own
	// handoff head, or one only declared review workflows make, is the round's to decide.
	roundSentBack
	// roundStuck is a round its reviewer completed that nothing on its way will end (stuckCause).
	roundStuck
)

// stuckCause is why a completed round is stuck.
type stuckCause int

const (
	// stuckUndecided: no review decided it. A COMMENT decides nothing, and neither does a review in
	// any state but approved or changes_requested, nor one decidesRound does not count.
	stuckUndecided stuckCause = iota + 1
	// stuckApprovedRed: its approval stands on CI red at the reviewer's own head.
	stuckApprovedRed
	// stuckApprovedWorkflowRed: its approval of the head stands on CI red only declared review
	// workflows make (classify.RedOnlyByReviewWorkflows), which the reviewer answers by adjudicating
	// their findings and re-running them (reviewWorkflowsToAdjudicate). A completion, or a read that
	// finds a later attempt of a red run still red, is told again (round.runs), so the architect sees
	// a re-run that stayed red and takes it to a human.
	stuckApprovedWorkflowRed
	// stuckApprovedOtherCode: its approval is of a head whose code the current head may not carry.
	stuckApprovedOtherCode
	// stuckApprovedPending: its approval waits on a required check the reviewer's own head settled
	// without a result for (cancelled, or not reported), which no later settlement may bring.
	stuckApprovedPending
)

// round is reviewRound's account of a review round. A stuck round names its cause, the head it is
// stuck at, and what the architect and a restarted reviewer are told of it; one stuck on red review
// workflows also names the run and attempt of each red one (redRuns).
type round struct {
	outcome roundOutcome
	cause   stuckCause
	head    string
	runs    string
	reason  string
}

// stuckAs says whether r is stuck in the same way as other: the same cause at the same head, on the
// same runs. A fact that is not the reviewer's own tells only when the round is stuck in a new way,
// so a second red settlement at the same head, failing other checks, tells nothing, while a re-run
// of a red review workflow that ends red again is a new attempt, and tells.
func (r round) stuckAs(other round) bool {
	return r.outcome == roundStuck && other.outcome == roundStuck && r.cause == other.cause && r.head == other.head && r.runs == other.runs
}

// reviewRound is what issue's review round comes to, row being its reviewer's and pr its pull
// request as a fact left them. A round ends once both of its halves are in: the reviewer's
// completion, which is its handoff landing - part of its phase's contract, as every role's is - and
// the decision it posted on GitHub, recorded on the round. Either may arrive second, and an
// approval also waits for the head's checks to settle green. A red at a code head sends the work
// back first. A round whose reviewer never completes is not ended by the decision alone: the
// reviewer's pane gets one follow-up turn when a turn ends with its phase open (pi-legion's
// phase-stall check), and past that the issue stays in reviewing, as a tester's that never completes
// stays in testing. A completed round that nothing on its way would end is stuck, its reason naming
// the head, since the decision it needs is of the head. An approved round under a red only declared
// review workflows make is stuck on their findings, which the reviewer adjudicates and re-runs
// (stuckApprovedWorkflowRed), once its approval is of the head's code; the run that passes on the
// head ends it. An approval of other code is stuck on that first (stuckApprovedOtherCode), since a
// green head would not end the round either. An approved round whose head's own settlement left a
// required check pending or without a result, or whose head has no run of a required workflow, is
// stuck too: the check may never report (a run nobody reruns, a check only a commit status
// reports), nor the workflow run, and a later settlement or run that passes it still ends the
// round. A required workflow still running, or not read at the head yet, keeps the round open until
// the daemon's next read of it.
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
	verdict := classify.HeadVerdict(*pr)
	// pending names the required checks and workflows the head's own CI left without a result, when
	// they are all that keeps it from a verdict.
	var pending []string
	if verdict == "" && pr.CheckedHead == pr.HeadSHA {
		pending = pendingAt(*pr)
	}
	reviewRed := classify.RedOnlyByReviewWorkflows(*pr, e.cfg.ReviewWorkflows)
	switch {
	case completed && decided == "approved" && verdict == "green" && classify.ApprovalStands(*pr, row.Decision.Head):
		return round{outcome: roundApproved}
	case classify.RedSendsBack(*pr, e.cfg.ReviewWorkflows):
		return round{outcome: roundSentBack}
	case !completed:
		return round{}
	case decided == "":
		return round{outcome: roundStuck, cause: stuckUndecided, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer completed its round on pull request #%d with no review that decides it: only an APPROVE of head %s or a REQUEST_CHANGES, from the review App or an account with write access to the repository, ends the round, and a COMMENT decides nothing",
				pr.Number, pr.HeadSHA)}
	case verdict == "red" && !reviewRed:
		return round{outcome: roundStuck, cause: stuckApprovedRed, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, but %s; a red at the reviewer's own head is its round's to decide, with a REQUEST_CHANGES naming the failing checks",
				row.Decision.Head, pr.Number, redAt(*pr))}
	case (verdict == "green" || reviewRed || len(pending) > 0) && !classify.ApprovalStands(*pr, row.Decision.Head) && !classify.CodeOnItsWay(*pr):
		return round{outcome: roundStuck, cause: stuckApprovedOtherCode, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, which does not approve head %s: a push since may have changed code, so only an APPROVE of %s or a REQUEST_CHANGES ends the round",
				row.Decision.Head, pr.Number, pr.HeadSHA, pr.HeadSHA)}
	case reviewRed && classify.ApprovalStands(*pr, row.Decision.Head):
		return round{outcome: roundStuck, cause: stuckApprovedWorkflowRed, head: pr.HeadSHA, runs: redRuns(*pr),
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, but %s%s",
				row.Decision.Head, pr.Number, redAt(*pr), reviewWorkflowsToAdjudicate)}
	case len(pending) > 0 && classify.ApprovalStands(*pr, row.Decision.Head):
		return round{outcome: roundStuck, cause: stuckApprovedPending, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, but CI at %s settled with no passing result for %s; the approval stands, and the round ends when a later settlement or run on the head passes them",
				row.Decision.Head, pr.Number, pr.HeadSHA, strings.Join(pending, ", "))}
	}
	return round{}
}

// redRuns names the run and attempt of each required workflow red at the pull request's head, as
// classify judges it (classify.WorkflowStanding, the judgment HeadVerdict counts): a re-run keeps
// its run's id and raises its attempt, so a re-run that ends red again is a round stuck anew
// (stuckAs), while a run classify does not count as red changes nothing.
func redRuns(pr record.PullRequest) string {
	var runs []string
	for _, workflow := range pr.Workflows {
		if classify.WorkflowStanding(pr, workflow).Red() {
			runs = append(runs, fmt.Sprintf("%s#%d.%d", workflow.Path, workflow.Run, workflow.Attempt))
		}
	}
	return strings.Join(runs, ", ")
}

// pendingAt names each required check and workflow the head's own CI left without a verdict
// (classify.HeadChecks), since none is a failure to open: one with no result at all - a check the
// settlement does not report, a workflow the head has no run of - and a check the settlement names
// as cancelled. A required workflow pending because it still runs, or is not read at the head yet,
// is not named: the daemon's next read decides it.
func pendingAt(pr record.PullRequest) []string {
	checks, _ := classify.HeadChecks(pr)
	var pending []string
	for _, check := range checks {
		switch {
		case check.Result == classify.Missing:
			pending = append(pending, check.Name+" (no result)")
		case check.Result == classify.Pending && slices.Contains(pr.Cancelled, check.Name):
			pending = append(pending, check.Name+" (cancelled)")
		}
	}
	return pending
}

// settleRound acts on what issue's review round comes to (reviewRound, with row its reviewer's and
// pr its pull request as the fact left them), and says whether it moved the issue. An ended round
// moves on, its request for changes back to implementing; a red code head sends the work back to
// implementing; a stuck round is told to the architect, in a notice whose summary is by, the fact
// that wrote it - unless the round was already stuck the same way (stuckAs) before that fact,
// which is before. The reviewer's completion and answer pass an empty before, so each that leaves
// the round stuck is told. The issue stays in reviewing and the architect asks the reviewer, at the
// notice's topic, for the decision. Linger holds a member of a closed tree where it stood
// (record.TreeLingers).
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
	case roundSentBack:
		return true, e.transition(ctx, tx, issue, TriggerChecksRed, "", record.PhaseRow{}, pr, redAt(*pr))
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
