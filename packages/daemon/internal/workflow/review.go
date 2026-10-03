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
	return intake.Result{}, e.decideChecks(ctx, tx, pr, prior, byChecks)
}

// requiredChecks records the checks the pull request's base branch requires, as the daemon read
// them (intake.RequiredChecks), and decides what the head's verdict now comes to (decideChecks):
// only a required check makes a head red (classify.HeadVerdict), so a new set can end a round that
// waited on a red the base branch never required, send work back for a newly required check, and
// give a head its first verdict when its set was never read. A read that names the set already
// recorded changes nothing.
func (e *Engine) requiredChecks(ctx context.Context, tx pgx.Tx, fact intake.RequiredChecks) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil || (pr.Required != nil && slices.Equal(pr.Required, fact.Names)) {
		return intake.Result{}, err
	}
	prior := *pr
	pr.Required = append([]string{}, fact.Names...)
	return intake.Result{}, e.decideChecks(ctx, tx, pr, prior, byRequired)
}

// decideChecks records pr, its checks verdict changed from prior's by a CI settlement or a new
// required set (the fact by names), and decides, with the issue's phase in hand, what it moves. An
// exhausted fix-attempt count is posted and told to the architect. In reviewing, the round decides
// what the verdict comes to (reviewRound, settleRound): it can be what an approval waits for, a
// red at a code head sends the work back, and a round the verdict leaves stuck another way than
// before is told. In testing, a red at a code head (classify.RedSendsBack) sends the work back.
// The implementer's task and the architect's checks-red notice name the red required checks, and
// the implementer's next push is a counted fix attempt, as any new head on a red verdict is
// (classify.AdvancePullRequestHead).
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
	if issue.Phase == phase.Reviewing {
		reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
		if err != nil {
			return err
		}
		_, err = e.settleRound(ctx, tx, *issue, reviewer, pr, reviewRound(*issue, reviewer, &prior), by)
		return err
	}
	if !classify.RedSendsBack(*pr) {
		return nil
	}
	return e.transition(ctx, tx, *issue, TriggerChecksRed, "", record.PhaseRow{}, pr, redAt(*pr))
}

// redAt is what the red verdict standing for the pull request's head says: the head, and each check
// the base branch requires that is red there (classify.HeadChecks), one that reported no result
// at all marked so, since it has no run to open.
func redAt(pr record.PullRequest) string {
	checks, _ := classify.HeadChecks(pr)
	var red []string
	for _, check := range checks {
		switch {
		case check.Result == classify.Missing:
			red = append(red, check.Name+" (no result)")
		case check.Red():
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

// reviewersAnswer says whether fact is the reviewer's answer to a round it completed undecided: a
// review the review App submitted, with a body - GitHub records a reply on a review thread as a
// review with no body - more than answerSkew after the completion was applied. A review submitted
// before the completion but delivered after it is not, since the completion comes through the API
// and the review by webhook; nor is one with no submission time, or anyone else's.
func (e *Engine) reviewersAnswer(fact intake.PullRequestReview, reviewer record.PhaseRow) bool {
	return e.byReviewApp(fact.Author) && fact.Body != "" && fact.SubmittedAt.After(reviewer.CompletedAt.Add(answerSkew))
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
	// The reviewer's answer tells whenever it leaves the round stuck; any other review, only when it
	// changes how the round is stuck.
	before, by := reviewRound(*issue, reviewer, pr), byOtherReview
	if e.reviewersAnswer(fact, reviewer) {
		before, by = round{}, byAnswer
	}
	// Only changes_requested and approved decide anything; a comment orders nothing either, so a
	// comment written after a decision but delivered before it cannot make the decision look old.
	state := strings.ToLower(fact.State)
	if state != "changes_requested" && state != "approved" {
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
	// handoff head is the round's to decide.
	roundSentBack
	// roundStuck is a round its reviewer completed that nothing on its way will end (stuckCause).
	roundStuck
)

// stuckCause is why a completed round is stuck.
type stuckCause int

const (
	// stuckUndecided: no review decided it. A COMMENT decides nothing, and neither does a review in
	// any state but approved or changes_requested.
	stuckUndecided stuckCause = iota + 1
	// stuckApprovedRed: its approval stands on CI red at the reviewer's own head.
	stuckApprovedRed
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

// stuckAs says whether r is stuck in the same way as other: the same cause at the same head. A
// fact that is not the reviewer's own tells only when the round is stuck in a new way, so a second
// red settlement at the same head, failing other checks, tells nothing.
func (r round) stuckAs(other round) bool {
	return r.outcome == roundStuck && other.outcome == roundStuck && r.cause == other.cause && r.head == other.head
}

// reviewRound is what issue's review round comes to, row being its reviewer's and pr its pull
// request as a fact left them. A round ends once both of its halves are in: the reviewer's
// completion, which is its handoff landing - part of its phase's contract, as every role's is - and
// the decision it posted on GitHub, recorded on the round. Either may arrive second, and an
// approval also waits for the head's checks to settle green. A red at a code head sends the work
// back first. A round whose reviewer never completes is not ended by the decision alone: the
// reviewer's pane gets one follow-up turn when a turn ends with its phase open (pi-envoy's
// phase-stall check), and past that the issue stays in reviewing, as a tester's that never completes
// stays in testing. A completed round that nothing on its way would end is stuck, its reason naming
// the head, since the decision it needs is of the head.
func reviewRound(issue record.Issue, row record.PhaseRow, pr *record.PullRequest) round {
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
	switch {
	case completed && decided == "approved" && verdict == "green" && classify.ApprovalStands(*pr, row.Decision.Head):
		return round{outcome: roundApproved}
	case classify.RedSendsBack(*pr):
		return round{outcome: roundSentBack}
	case !completed:
		return round{}
	case decided == "":
		return round{outcome: roundStuck, cause: stuckUndecided, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer completed its round on pull request #%d with no review that decides it: only an APPROVE of head %s or a REQUEST_CHANGES ends the round, and a COMMENT decides nothing",
				pr.Number, pr.HeadSHA)}
	case verdict == "red":
		return round{outcome: roundStuck, cause: stuckApprovedRed, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, but %s; a red at the reviewer's own head is its round's to decide, with a REQUEST_CHANGES naming the failing checks",
				row.Decision.Head, pr.Number, redAt(*pr))}
	case verdict == "green" && !classify.CodeOnItsWay(*pr):
		return round{outcome: roundStuck, cause: stuckApprovedOtherCode, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, which does not approve head %s: a push since may have changed code, so only an APPROVE of %s or a REQUEST_CHANGES ends the round",
				row.Decision.Head, pr.Number, pr.HeadSHA, pr.HeadSHA)}
	}
	return round{}
}

// settleRound acts on what issue's review round comes to (reviewRound, with row its reviewer's and
// pr its pull request as the fact left them), and says whether it moved the issue. An ended round
// moves on, its request for changes counting a round; a red code head sends the work back to
// implementing; a stuck round is told to the architect, in a notice whose summary is by, the fact
// that wrote it - unless the round was already stuck the same way (stuckAs) before that fact,
// which is before. The reviewer's completion and answer pass an empty before, so each that leaves
// the round stuck is told. The issue stays in reviewing and the architect asks the reviewer, at the
// notice's topic, for the decision. Linger holds a member of a closed tree where it stood
// (record.TreeLingers).
func (e *Engine) settleRound(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest, before round, by string) (bool, error) {
	r := reviewRound(issue, row, pr)
	switch r.outcome {
	case roundRejected:
		if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
			return false, err
		}
		if err := e.recordRound(ctx, tx, issue.Key); err != nil {
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
