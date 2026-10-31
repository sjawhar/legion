package workflow

import (
	"context"
	"fmt"
	"slices"
	"strconv"
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

// checks applies a CI settlement to the pull request and tells the roles that work the issue what
// it says (tellChecks). It moves nothing: a failed CI check is information the worker holding the
// phase decides on - rerun, wait, or fix - and not the daemon's to read the semantics of
// (dispatch://INFRA-714/spec R1).
func (e *Engine) checks(ctx context.Context, tx pgx.Tx, fact intake.PullRequestChecks) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	// A handoff push can start no CI of its own (GitHub's skip-checks trailer), so the settlement
	// that stands for the head can be of the code head it replaced, arriving after it. A
	// settlement is for the commit it names, never the head by default. While a head run is open
	// the settlement is recorded in it whether it stands or not (classify.ApplyCheckSettlement).
	candidate := classify.SettlementCandidate{Head: fact.HeadSHA, CheckRuns: fact.CheckRuns, Generation: fact.Generation, Snapshot: fact.Snapshot, Failing: fact.Failing, Cancelled: fact.Cancelled}
	told := settledChecks(*pr)
	settled, applied := classify.ApplyCheckSettlement(*pr, candidate)
	if !applied {
		if pr.Run != nil {
			*pr = settled
			if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
				return intake.Result{}, err
			}
		}
		// A settlement that does not stand for the head tells nobody, and GitHub's merge state is
		// read for the head all the same: a run GitHub counts for the head can settle on a commit
		// the record does not stand at, and the state is the only answer that says so.
		return intake.Result{}, e.askMergeState(ctx, tx, pr)
	}
	*pr = settled
	if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
		return intake.Result{}, err
	}
	if err := e.tellChecks(ctx, tx, pr, told); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.askMergeState(ctx, tx, pr)
}

// carryApproval queues GitHub's compare of the head the reviewer approved with the pull request's
// head (record.ApprovalCarry, the outbox's approvalCarry) when the push just recorded leaves that
// approval set aside below the head (classify.ApprovalSetAside): the reviewer approved another
// head than the current one, no recorded push carries the approval up to it, and no push that may
// change code is on its way. A push carries an approval across only when
// its own file list shows it changed nothing outside the issue's handoffs, so an approval is set
// aside by a push whose list was truncated or forced, or that came from a listener that says
// nothing about paths; GitHub's compare of the two heads is what settles it, and R1 reads "the
// file list comes from GitHub's compare, read once per push event"
// (dispatch://INFRA-714/spec).
//
// The compare is a durable row rather than a call made here: the compare is a GitHub read, which
// the fact's transaction must not wait on, and one rate-limited answer must not silently drop an
// approval - the row retries on the outbox's backoff and under its rate-limit budget until GitHub
// answers. The row names the head it was queued at and does nothing once the record has moved past
// it, so each push's row answers for its own head.
func (e *Engine) carryApproval(ctx context.Context, tx pgx.Tx, issue record.Issue, pr *record.PullRequest) error {
	if pr == nil || pr.State != record.PullRequestOpen {
		return nil
	}
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return err
	}
	if reviewer.Decision == nil || reviewer.Decision.State != "approved" || reviewer.Decision.Head == "" {
		return nil
	}
	if reviewer.Decision.Head == pr.HeadSHA || !classify.ApprovalSetAside(*pr, reviewer.Decision.Head) {
		return nil
	}
	return e.enqueue(ctx, tx, issue.Key, record.ApprovalCarry{Repo: pr.Repo, Number: pr.Number,
		Approved: reviewer.Decision.Head, Head: pr.HeadSHA})
}

// mergeability records what GitHub's last read found for whether the pull request's head can be
// merged into its base (intake.PullRequestMergeability, readPullRequest's own GitHub answer), and
// acts on the two things that read can say.
//
// A head that conflicts with its base sends the issue back to implementing from whatever later
// phase it stands in (TriggerConflict):
// GitHub computes no merge ref for a conflicting head, so no check runs on it and no CI result
// will ever arrive for it, and only the implementer merging the base forward clears it. It
// decides on every read, not only a changed one, since the recorded conflict is what a read made
// in an earlier phase left. Implementing, where the work already is, has no row for the trigger,
// so a read there records the conflict and moves nothing (transition).
//
// GitHub's own merge state for the head rides along on the same fact: it is what says whether
// GitHub would merge this head now, which the tri-state mergeability cannot, and it is what an
// approval standing on the head waits for (reviewRound). So in reviewing the round is asked again
// once the state is recorded - this read is the one that moves an approved issue to accepting, as
// often as not. The base the read names is recorded with
// them, since that read is the only one that names the base of a pull request opened before the
// record kept one: it is what a push onto that branch finds the pull request by (basePush), and a
// read that answers for a base push already recorded clears the push's mark, the two having been
// read together.
//
// A read that answers for another head than the record stands at is dropped whole, and logged: a
// push landed between GitHub's answer and this apply, so the verdict is about a tree the record
// has moved past, and an approval must never advance on it. Nothing is lost by dropping it - that
// push is itself a fact that asks for a read of the head it left (askMergeState).
func (e *Engine) mergeability(ctx context.Context, tx pgx.Tx, fact intake.PullRequestMergeability) (intake.Result, error) {
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return intake.Result{}, err
	}
	if fact.HeadSHA != pr.HeadSHA {
		e.logOnCommit(ctx, "workflow: GitHub's merge state answered for a head the pull request has moved past; the read is dropped",
			"issue", pr.Issue, "pullRequest", pr.Number, "answered", fact.HeadSHA, "head", pr.HeadSHA,
			"mergeable", string(fact.Mergeable), "mergeState", string(fact.MergeState))
		return intake.Result{}, nil
	}
	base := pr.Base
	if fact.Base != "" {
		base = fact.Base
	}
	prior := *pr
	if pr.Mergeability != fact.Mergeable || pr.MergeState != fact.MergeState || pr.Base != base || pr.BaseMoved != "" {
		pr.Mergeability, pr.MergeState, pr.Base, pr.BaseMoved = fact.Mergeable, fact.MergeState, base, ""
		if err := e.store.PutPullRequest(ctx, tx, *pr); err != nil {
			return intake.Result{}, err
		}
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	if classify.HeadConflicts(*pr) {
		reason := fmt.Sprintf("the head conflicts with %s: GitHub runs no checks on it; merge %s forward", base, base)
		return intake.Result{}, e.transition(ctx, tx, *issue, TriggerConflict, "", record.PhaseRow{}, pr, reason)
	}
	if issue.Phase != phase.Reviewing {
		return intake.Result{}, nil
	}
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return intake.Result{}, err
	}
	_, err = e.settleRound(ctx, tx, *issue, reviewer, pr, e.reviewRound(*issue, reviewer, &prior), byMergeState)
	return intake.Result{}, err
}

// settledChecks is what the CI settlement standing for the pull request's head says: the head it
// settled on, and each check it reports with the conclusion it reports for it, in name order; ""
// while no settlement stands. A check the settlement names as failed failed, one it names as
// cancelled was cancelled, and one it reports as a run and names neither way succeeded - the
// listener settles a commit once every check it has seen is terminal. Nothing here judges those
// conclusions: a job GitHub never started reads like any other, and what they mean for the work is
// the reading worker's (dispatch://INFRA-714/spec R1). The line is the whole of what the daemon
// knows of that head's CI, so two settlements that leave it the same say nothing new (tellChecks).
//
// Each name's conclusion is the newest run of it across the code head and every handoff-only head
// above it, which is what the record holds (classify.SettlementFor): a handoff push changes
// nothing GitHub checks, so those heads hold one tree and a run on any of them is a run on the
// same code. That is the whole of it for a check that runs per pull request - a title check, or
// anything on a pull_request edited trigger - which runs on whatever head is current and so never
// on the code head again: its run on the head above is the newer one, and carrying the code
// head's older failure past it would report a check that has since passed as red. A check still
// running on every one of those heads is in no settlement of any of them - the listener settles a
// commit only once every check it has seen there is terminal - so this line names it not at all:
// a pending check is no verdict, and never a failure.
//
// A check run's name is whoever wrote the workflow's text, and this line is read into an agent's
// prompt, so each name is quoted as a Go string (strconv.Quote): the quotes bound it, and a quote
// or a newline inside it is escaped rather than closing the one around it. A name of ordinary
// characters reads as itself inside the quotes.
func settledChecks(pr record.PullRequest) string {
	if pr.CheckedHead == "" {
		return ""
	}
	conclusions := make(map[string]string, len(pr.CheckRuns)+len(pr.Failing)+len(pr.Cancelled))
	for _, run := range pr.CheckRuns {
		conclusions[run.Name] = "succeeded"
	}
	for _, name := range pr.Cancelled {
		conclusions[name] = "cancelled"
	}
	for _, name := range pr.Failing {
		conclusions[name] = "failed"
	}
	if len(conclusions) == 0 {
		return "CI settled at " + pr.CheckedHead + " reporting no check"
	}
	names := make([]string, 0, len(conclusions))
	for name := range conclusions {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		names[i] = strconv.Quote(name) + " " + conclusions[name]
	}
	return "CI settled at " + pr.CheckedHead + ": " + strings.Join(names, ", ")
}

// tellChecks tells the roles that work the issue what the settlement now standing for the pull
// request's head says (settledChecks), and moves nothing: the daemon passes the CI result on, and
// the worker that holds the phase decides whether to re-run, wait or fix
// (dispatch://INFRA-714/spec R1). One message goes to three roles - the worker holding the issue's
// phase, the implementer, whose code the checks ran on, and the tree's architect - each on its own
// role topic (record.RoleMessage), the way the daemon reaches a role holder with a message. A role
// named twice, the implementer holding the phase, is told once, and a phase no worker holds names
// none (addressRoles). told is what the settlement standing for the head said before this one, so
// a second settlement of the same head that leaves the same conclusions tells nobody again, and so
// does a settlement that stands for no head. Linger holds a member of a closed tree where it stood
// (record.TreeLingers): every role of the tree is suspended there, so the settlement is recorded
// and nobody is told.
//
// A holder this daemon cannot name is an error here, and with it the settlement's whole
// transaction: the settlement moves no issue, so there is nothing kept by telling the rest, and
// the next settlement of the head asks again.
func (e *Engine) tellChecks(ctx context.Context, tx pgx.Tx, pr *record.PullRequest, told string) error {
	settled := settledChecks(*pr)
	if settled == "" || settled == told {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return err
	}
	if lingers, err := record.TreeLingers(ctx, e.store, tx, issue.Tree); err != nil || lingers {
		return err
	}
	// The tree's architect owns the tree, so its claim is the tree root's; a phase the architect
	// itself holds (accepting) is that same claim, and is told once with it.
	recipients := []roleRecipient{{issue: issue.Tree, role: claim.RoleArchitect, row: issue.Key}}
	for _, role := range []claim.Role{RoleFor(issue.Phase), claim.RoleImplementer} {
		if role == claim.RoleArchitect {
			continue
		}
		recipients = append(recipients, roleRecipient{issue: issue.Key, role: role, row: issue.Key})
	}
	addressed, err := e.addressRoles(issue.Project, recipients, nil)
	if err != nil {
		return err
	}
	body := fmt.Sprintf("Pull request #%d: %s. Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code.", pr.Number, settled)
	for _, to := range addressed {
		if err := e.enqueue(ctx, tx, to.issue, record.RoleMessage{Role: string(to.token), Body: body}); err != nil {
			return err
		}
	}
	return nil
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
		if _, err := e.settleRound(ctx, tx, *issue, reviewer, pr, before, by); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.askMergeState(ctx, tx, pr)
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
	if _, err := e.settleRound(ctx, tx, *issue, reviewer, pr, before, by); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.askMergeState(ctx, tx, pr)
}

// The fact that wrote a review-stuck notice, its summary. The architect prompt
// (prompts/go/architect-common.md) reads a notice the reviewer's completion or answer wrote after
// the architect asked as the reviewer answering, and every other as news about the round.
const (
	byCompletion  = "the reviewer's completion"
	byAnswer      = "the reviewer's review"
	byOtherReview = "a review that is not the reviewer's answer"
	byPush        = "a push"
	byMergeState  = "GitHub's merge state for the head"
)

// roundOutcome is what a review round comes to after a fact (reviewRound).
type roundOutcome int

const (
	// roundOpen is a round still owed its reviewer's completion or its decision, or waiting on a
	// head a push that may change code is bringing; or an issue not in reviewing, since one held
	// from reviewing ends only once the retry restores it.
	roundOpen roundOutcome = iota
	// roundApproved ends the round: its reviewer completed, and its approval approves the head's code
	// (classify.ApprovalStands), which GitHub answers for with a merge state the issue may advance
	// on. No CI verdict is read: the reviewer reads the head's checks on GitHub before it approves,
	// and a settlement moves no issue (dispatch://INFRA-714/spec R1).
	roundApproved
	// roundRejected ends the round: its reviewer completed, and it requested changes.
	roundRejected
	// roundStuck is a round that nothing on its way will end (stuckCause): one its reviewer
	// completed without a decision of the head, or one whose approval GitHub will not merge from.
	roundStuck
)

// stuckCause is why a round that nothing on its way will end is stuck.
type stuckCause int

const (
	// stuckUndecided: no review decided it. A COMMENT decides nothing, and neither does a review in
	// any state but approved or changes_requested, nor one DecidesRound does not count.
	stuckUndecided stuckCause = iota + 1
	// stuckApprovedOtherCode: its approval is of a head whose code the current head may not carry.
	stuckApprovedOtherCode
	// stuckMergeState: its approval approves the head's code, and GitHub answers for that head
	// with a merge state it would not merge from - blocked on a check or a review the repository
	// requires, behind its base, a draft. Nothing the daemon does moves it: GitHub's answer is
	// about the head on the branch, and only a push that earns the answer the base wants changes
	// it. This cause alone does not wait for the reviewer's completion: the approval has already
	// decided the round, and the completion is no part of what holds it.
	stuckMergeState
)

// round is reviewRound's account of a review round. A stuck round names its cause, the head it is
// stuck at, the merge state GitHub gave for that head, and what the architect, the reviewer and a
// restarted reviewer are told of it. waitsOnMergeState is the one thing the round is owed from
// outside the record: an approval that stands on the head with GitHub's answer for it not one the
// issue may advance on, which the daemon reads GitHub for (settleRound, daemon's readMergeState).
// It is set for the round that waits for a first answer and for the stuck one alike, since the
// state GitHub gives a head changes under it - a base push clears `behind`, a required check that
// runs clears `blocked` - and a read is the only thing that would ever say so.
type round struct {
	outcome           roundOutcome
	cause             stuckCause
	head              string
	state             record.MergeState
	reason            string
	waitsOnMergeState bool
}

// stuckAs says whether r is stuck in the same way as other: the same cause at the same head, and
// for a round GitHub will not merge from, the same merge state too, since that state is what the
// notice reports and GitHub can answer the same head two ways in turn. A fact that is not the
// reviewer's own tells only when the round is stuck in a new way, so a second push classified late
// at the same head tells nothing.
func (r round) stuckAs(other round) bool {
	return r.outcome == roundStuck && other.outcome == roundStuck &&
		r.cause == other.cause && r.head == other.head && r.state == other.state
}

// stuckMergeStateReason is what the architect and the reviewer are told of an approval standing
// on a head GitHub answers for with a merge state it would not merge from: the head, the pull
// request, the state in GitHub's own spelling, and every check GitHub reported on that head with
// the conclusion it gave (settledChecks), which is the whole of what the daemon knows of the
// head's CI.
//
// The daemon reads no repository ruleset, so it does not know which checks the base requires
// (dispatch://INFRA-714/spec R1): a required check that has never run is in no list GitHub gives
// for the head, so it is simply absent from the checks named here. The reason says so, because a
// reader who does not know that would take a short list for the daemon leaving something out
// rather than for the finding it is - the check that is missing is the one to look for.
func stuckMergeStateReason(pr record.PullRequest) string {
	reported := settledChecks(pr)
	if reported == "" {
		return fmt.Sprintf("the reviewer approved head %s on pull request #%d, and GitHub reports that head %s rather than one it would merge. No CI settlement stands for the head, so the daemon knows of no check GitHub reported on it. The daemon reads no ruleset and so names no required check: a check the base requires that has never run is absent from what GitHub reports for the head, and so from this notice, which makes the missing check the thing to look for. The issue waits in reviewing until GitHub reports the head clean, unstable or has_hooks, which only a push to the branch that earns what the base requires can do",
			pr.HeadSHA, pr.Number, pr.MergeState)
	}
	return fmt.Sprintf("the reviewer approved head %s on pull request #%d, and GitHub reports that head %s rather than one it would merge. %s. That is every check GitHub reported on the head, and the daemon reads no ruleset and so names no required check: one the base requires that has never run is absent from the list above rather than named as missing, which makes the missing check the thing to look for. The issue waits in reviewing until GitHub reports the head clean, unstable or has_hooks, which only a push to the branch that earns what the base requires can do",
		pr.HeadSHA, pr.Number, pr.MergeState, reported)
}

// reviewRound is what issue's review round comes to, row being its reviewer's and pr its pull
// request as a fact left them. The review event decides it, and the reviewer's handoff records its
// findings (dispatch://INFRA-714/spec R1): a request for changes ends the round whether or not the
// reviewer has completed, and the completion that follows is recorded on the reviewer's own row
// (engine's leftItsPhase). An approval ends it once it approves the head's code
// (classify.ApprovalStands) and GitHub reports that head one the issue may advance on
// (record.MergeState.Mergeable). An approval GitHub has given no answer for yet - the state
// unread, or `unknown` while GitHub computes it - waits where it stands, and the read that lands
// moves it (mergeability, the ladder behind daemon's readMergeState). An approval GitHub answered
// for with any other state is stuck (stuckMergeState): the architect and the reviewer are told,
// once for each head and state, since nothing else would ever come to move the issue.
// The round reads no CI verdict: a CI result moves nothing and
// decides no round, and the reviewer reads the head's checks on GitHub itself. A completed round
// that nothing on its way would end is stuck, its reason naming the
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
	if decided == "changes_requested" {
		return round{outcome: roundRejected}
	}
	if pr == nil {
		return round{}
	}
	switch {
	case decided == "approved" && classify.ApprovalStands(*pr, row.Decision.Head):
		if pr.MergeState.Mergeable() {
			return round{outcome: roundApproved}
		}
		// Everything below here waits on GitHub's answer for this head: the unread state and
		// `unknown` because GitHub has given none yet, and every other because the one it gave
		// can change under the head. The daemon reads for each (settleRound).
		if pr.MergeState == record.MergeStateUnread || pr.MergeState == record.MergeStateUnknown {
			return round{waitsOnMergeState: true}
		}
		return round{outcome: roundStuck, cause: stuckMergeState, head: pr.HeadSHA, state: pr.MergeState,
			waitsOnMergeState: true, reason: stuckMergeStateReason(*pr)}
	case !completed:
		return round{}
	case decided == "":
		return round{outcome: roundStuck, cause: stuckUndecided, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer completed its round on pull request #%d with no review that decides it: only an APPROVE of head %s or a REQUEST_CHANGES ends the round, from an account with write access to the repository or from the reviewer's own session as the review App (its Legion footer naming the session recorded on the reviewer's claim: a review-App review with no footer or another session's is set aside and decides nothing), and a COMMENT decides nothing",
				pr.Number, pr.HeadSHA)}
	case classify.ApprovalSetAside(*pr, row.Decision.Head):
		return round{outcome: roundStuck, cause: stuckApprovedOtherCode, head: pr.HeadSHA,
			reason: fmt.Sprintf("the reviewer approved %s on pull request #%d, which does not approve head %s: a push since may have changed code, so only an APPROVE of %s or a REQUEST_CHANGES ends the round",
				row.Decision.Head, pr.Number, pr.HeadSHA, pr.HeadSHA)}
	}
	return round{}
}

// implementationReady moves issue from implementing to testing on the implementer's completion of
// the round that row records, once the pull request pr is recorded open or merged
// (TriggerImplementationReady: a record closed without merging, or none, moves nothing). It
// reads no CI result: a settlement that arrived while the implementer worked moved nothing and
// moves nothing now, since the roles were told what it said when it arrived (checks,
// dispatch://INFRA-714/spec R1).
func (e *Engine) implementationReady(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, pr *record.PullRequest) error {
	return e.transition(ctx, tx, issue, TriggerImplementationReady, "", row, pr, "")
}

// settleRound acts on what issue's review round comes to (reviewRound, with row its reviewer's and
// pr its pull request as the fact left them), and says whether it moved the issue. An ended round
// moves on, its request for changes back to implementing; a stuck round is told to the architect,
// in a notice whose summary is by, the fact that wrote it - unless the round was already stuck the
// same way (stuckAs) before that fact, which is before. Each caller passes the round as its own
// fact found it, so a round stuck at a head the architect was told of is told once and no fact
// about the same head tells it again. The issue stays in reviewing. A round waiting on the
// reviewer's decision names the reviewer's role topic, which the architect asks for that
// decision; a round GitHub will not merge from (stuckMergeState) names none, since no decision of
// the reviewer's would move it. Linger holds a member of a closed tree where it stood
// (record.TreeLingers).
//
// A round GitHub will not merge from reaches the reviewer as well, in a role message of its own
// (record.RoleMessage, the way the daemon reaches a role holder with something to read): the
// reviewer approved that head and would otherwise watch a round it decided go nowhere, and it is
// the role reading the head's checks on GitHub, which is where what the notice names is answered.
// It goes with the architect's notice, so the two are told once for each head and state.
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
		topic := notify.RoleTopicPrefix + string(reviewer)
		if r.cause == stuckMergeState {
			topic = ""
			if err := e.enqueue(ctx, tx, issue.Key, record.RoleMessage{Role: string(reviewer),
				Body: "The round you approved is stuck: " + r.reason + "."}); err != nil {
				return false, err
			}
		}
		return false, e.notice(ctx, tx, issue.Key, record.Notice{Kind: "review-stuck", Role: claim.RoleReviewer, Phase: phase.Reviewing,
			Summary: by, Reason: r.reason, Topic: topic})
	}
	return false, nil
}

// askMergeState asks the daemon to read GitHub's merge state for pr's issue when the round the
// fact just applied leaves waiting on GitHub's answer for the head (round.waitsOnMergeState,
// Config.ReadMergeState). It is the engine's own account of what the round is owed rather than a
// second reading of the record beside it, and it runs on the commit, so the fact's transaction
// waits for no GitHub call.
//
// Every fact that can change GitHub's answer asks: the pull request's own events, a push to it, a
// review, a CI settlement, and a push to the base it sits on. GitHub's own answer does not ask -
// the fact a read applies is mergeability, and a read that asked for another would read the same
// head for as long as GitHub kept answering the same way. What a read answers is the ladder's to
// decide (daemon's mergeStateLadder), and the next fact of the issue asks again.
func (e *Engine) askMergeState(ctx context.Context, tx pgx.Tx, pr *record.PullRequest) error {
	if pr == nil || e.cfg.ReadMergeState == nil || pr.State != record.PullRequestOpen {
		return nil
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return err
	}
	reviewer, err := e.phaseRow(ctx, tx, issue.Key, claim.RoleReviewer)
	if err != nil {
		return err
	}
	if !e.reviewRound(*issue, reviewer, pr).waitsOnMergeState {
		return nil
	}
	key := issue.Key
	intake.OnCommit(ctx, func() { e.cfg.ReadMergeState(key) })
	return nil
}

// recordPass counts the implementer one more pass through the implementing, testing and reviewing
// stretch, which every move back into an earlier phase opens. Nothing is bounded by it: it is the
// counter the handoff completion key carries (api/handoff.go), and its whole job is to make a
// completion at a commit an earlier pass reported a fact of its own.
func (e *Engine) recordPass(ctx context.Context, tx pgx.Tx, issue string) error {
	row, err := e.phaseRow(ctx, tx, issue, claim.RoleImplementer)
	if err != nil {
		return err
	}
	row.Pass++
	return e.store.PutPhase(ctx, tx, row)
}

// recordRound counts the implementer one more review round, which only a review that requested
// changes ends, and tells the architect once the rounds reach review_round_cap.
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
