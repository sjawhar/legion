package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/requiredchecks"
)

// requiredChecksInterval is how often watchRequiredChecks reads every open pull request's required
// checks after its first pass. GitHub tells an App of a ruleset or branch-protection change only by
// webhooks (repository_ruleset, branch_protection_rule) the Envoy listener does not carry to the
// daemon, and the listener does not carry a pull request's workflow runs at all, so reading again
// is how the daemon notices either: a change reaches the verdict within one interval. A pass reads
// each open pull request twice (the pull request itself, and its reviews, a page per hundred
// reviews: readReviews), each base branch at least twice (its rulesets, a page per hundred rules,
// and its protection), and, when the base requires a workflow, each pull request's workflow runs
// once more, all on the implement App's installation, whose hourly limit every pane's gh shares:
// about a hundred and twenty reads an hour for a pull request on a base of its own, sixty for each
// more on the same base, and thirty more for each pull request whose base requires a workflow; it
// compares an approved head with the head once for each review round whose approval is set aside
// below the head (readApprovalCarry), until the approval stands or GitHub answers; and it lists the
// open pull requests on the branch of each issue whose implementer completed with no pull request
// recorded, thirty an hour while one waits. Two minutes also gives a newly opened pull request its
// set well before its first CI settles, and bounds how long a review decision, a head or a pull
// request whose webhook never arrived stays unread.
const requiredChecksInterval = 2 * time.Minute

// requiredChecksRead bounds one pull request's reads in a pass: its base branch, its reviews, then
// that branch's rulesets and protection, then its workflow runs, each a single GitHub answer that
// comes in well under a second.
const requiredChecksRead = 30 * time.Second

// watchRequiredChecks reads the checks each open pull request's base branch requires
// (readRequiredChecks) as the runtime starts and every requiredChecksInterval after, until ctx
// ends. The first pass is the boot's resync: a pull request whose set was never read - every one
// recorded before the daemon read sets at all - has no checks verdict until it runs, and the round
// a red the base branch never required left stuck is decided by it, with no new push, as is a
// round whose decision's webhook never arrived.
func (w *workflowRuntime) watchRequiredChecks(ctx context.Context) {
	w.readRequiredChecks(ctx)
	ticker := time.NewTicker(requiredChecksInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.readRequiredChecks(ctx)
		}
	}
}

// readRequiredChecks is one pass: for each open pull request of the project it reads the pull
// request itself (readPullRequest). When GitHub's head is not the recorded one it applies the
// synchronize the webhook would have carried and goes on with the record that leaves (followHead);
// when following fails short of GitHub's rate limit, it goes on with the record as it stands.
// It applies an intake.PullRequestMergeability fact whenever GitHub's lazily computed mergeability
// is CONFLICTING or differs from the one recorded - workflow's mergeability decides a CONFLICTING
// read every time, even one that only confirms the stored value, so a conflict the poll recorded
// before the issue reached awaiting_merge is still acted on once it gets there. It then reads the
// pull request's reviews and applies a deciding one the daemon has not recorded, whose webhook
// never arrived (readReviews), whatever phase the issue is in, and, for a review round whose
// approval is set aside below the head, GitHub's compare of the approved head with the head,
// applying the handoff-only push it shows (readApprovalCarry). It then reads what the base branch
// requires with the implement App's installation token, the credential the daemon reads GitHub
// with for its own work (requiredchecks.Required, which this pass is the only reader of), and, when
// that includes a workflow, each required workflow's latest run on the head whose settlement stands
// for the pull request's (classify.WorkflowHead, requiredchecks.Workflows). It applies an
// intake.RequiredChecks fact when that read differs from the one recorded, which decides the head's
// verdict again (workflow's requiredChecks). Last it looks up the open pull request of each issue
// whose implementer completed with none recorded (readUnrecordedPullRequests). A read that fails is
// logged with the repository, the pull request and GitHub's HTTP status, and changes nothing: the
// pull request keeps the set and runs last read, and one never read keeps no checks verdict,
// neither red nor green, until a later pass reads it. A rate-limit answer ends the pass there,
// since every read after it on the same installation meets the same limit, and the panes share it;
// the next pass starts over.
func (w *workflowRuntime) readRequiredChecks(ctx context.Context) {
	var open []record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		open, err = w.records.OpenPullRequests(ctx, tx, w.dispatchProject)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("list the open pull requests to read their required checks", "error", err)
		}
		return
	}
	read := map[string]requiredchecks.Set{}
	for _, pr := range open {
		readCtx, cancelRead := context.WithTimeout(ctx, requiredChecksRead)
		github, pull, err := w.readPullRequest(readCtx, pr)
		cancelRead()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if w.logPassFailure(pr, "a pull request and its mergeability", err) {
				return
			}
			continue
		}
		if pr, err = w.followHead(ctx, github, pr, pull); err != nil {
			if ctx.Err() != nil {
				return
			}
			var answer *githubrest.Answer
			if errors.As(err, &answer) && answer.RateLimited {
				w.logPassFailure(pr, "what a pull request's new head changed, to follow it", err)
				return
			}
			// The rest of the pull request's reads go on against the record as it stands, as they
			// did before the pass followed heads: a comparison GitHub keeps failing must not stop
			// them, and a later pass follows the head.
			w.log.Warn("follow the head GitHub shows for a pull request; this pass reads it at the recorded head, and a later pass follows it",
				"repo", pr.Repo, "pullRequest", pr.Number, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "error", err)
		}
		// The reads below start their bound only now: following a head waits on the fact lock,
		// which the GitHub consumer's backlog can hold for a while.
		prCtx, cancel := context.WithTimeout(ctx, requiredChecksRead)
		base, mergeable := pull.Base.Ref, pull.mergeability()
		if pr.Mergeability != mergeable || mergeable == record.MergeabilityConflicting {
			eventID := fmt.Sprintf("mergeability:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
			fact := intake.PullRequestMergeability{Repo: pr.Repo, Number: pr.Number, Base: base, Mergeable: mergeable}
			if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
				w.log.Warn("apply whether a pull request's head can be merged into its base", "repo", pr.Repo, "pullRequest", pr.Number, "error", err)
			}
		}
		if w.readReviews(ctx, prCtx, github, pr) || w.readApprovalCarry(ctx, prCtx, github, pr) {
			cancel()
			return
		}
		checks, err := w.requiredFor(prCtx, github, pr, base, read)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if w.logPassFailure(pr, "the checks a pull request's base branch requires", err) {
				return
			}
			continue
		}
		if pr.RequiredReadUnchanged(checks.Names, checks.Workflows, checks.WorkflowsHead) {
			continue
		}
		eventID := fmt.Sprintf("required-checks:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
		if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, checks, w.handlers...); err != nil {
			w.log.Warn("apply the checks a pull request's base branch requires", "repo", pr.Repo, "pullRequest", pr.Number, "error", err)
		}
	}
	w.readUnrecordedPullRequests(ctx)
}

// logPassFailure logs a failed GitHub read for pr during a required-checks pass, naming action -
// what the pass was trying to read when it failed - with the repository, the pull request and
// GitHub's HTTP status when the error carries one. It returns true when the answer was a rate
// limit: every further read on the same installation meets the same limit, and the panes share it,
// so the caller must stop the whole pass there rather than skip only this pull request.
func (w *workflowRuntime) logPassFailure(pr record.PullRequest, action string, err error) bool {
	attributes := []any{"repo", pr.Repo, "pullRequest", pr.Number, "error", err}
	var answer *githubrest.Answer
	if errors.As(err, &answer) {
		attributes = append(attributes, "status", answer.Status)
	}
	if answer != nil && answer.RateLimited {
		w.log.Error("read "+action+": GitHub's rate limit; this pass reads no more pull requests, and every unread one's checks verdict stays as the set last read decides it", attributes...)
		return true
	}
	w.log.Error("read "+action+"; its checks verdict stays as the set last read decides it, and undecided when none was", attributes...)
	return false
}

// githubPullRequest is one pull request as GitHub answers GET /pulls/{number} and lists it in
// GET /pulls: its number, state (open or closed), page, body, base branch, head branch and commit,
// when it last changed, and its nullable, lazily computed `mergeable`, which the list leaves out.
type githubPullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Base    struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	UpdatedAt time.Time `json:"updated_at"`
	Mergeable *bool     `json:"mergeable"`
}

// mergeability is the pull request's `mergeable` as the mergeability fact decides a READY by: null
// is record.MergeabilityUnknown, GitHub not having computed it yet; true is
// record.MergeabilityMergeable; false is record.MergeabilityConflicting, the state that, in
// awaiting_merge, withdraws a READY GitHub can run no checks on and merge (workflow's
// mergeability).
func (p githubPullRequest) mergeability() record.Mergeability {
	switch {
	case p.Mergeable == nil:
		return record.MergeabilityUnknown
	case *p.Mergeable:
		return record.MergeabilityMergeable
	default:
		return record.MergeabilityConflicting
	}
}

// readPullRequest reads pr from one GET /pulls/{number}: the base requiredFor needs to read what it
// requires, the mergeability, and the head and its clock followHead compares with the record, in
// the single read they share. It also returns the githubrest.Client its own token mint built, so
// the pass's further reads of the same pull request reuse it rather than minting a second token.
func (w *workflowRuntime) readPullRequest(ctx context.Context, pr record.PullRequest) (githubrest.Client, githubPullRequest, error) {
	repository, err := ghrepo.Parse("the pull request's repository", pr.Repo)
	if err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	github, err := w.implementClient(ctx, repository)
	if err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	var pull githubPullRequest
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", pr.Number), &pull); err != nil {
		return githubrest.Client{}, githubPullRequest{}, err
	}
	return github, pull, nil
}

// implementClient is a client for repository's REST API on the implement App's installation token.
func (w *workflowRuntime) implementClient(ctx context.Context, repository ghrepo.Repository) (githubrest.Client, error) {
	lease, err := w.tokens.Token(ctx, appauth.Implement, repository.Owner())
	if err != nil {
		return githubrest.Client{}, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
	}
	return githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(w.githubAPI, repository)}, nil
}

// requiredFor reads what base requires for pr: its rulesets and protection, which read keeps for
// the rest of the pass by repository and base, so pull requests on one base read them once, and
// then, when the base requires a workflow, pr's own workflow runs on the head whose settlement
// stands for its head. github and base are readPullRequest's own read for this pr, so a
// failure here - a flaky rulesets read, a repository with no workflow runs yet - never discards the
// mergeability it already decoded: that fact was applied, if it needed to be, before this is ever
// called.
func (w *workflowRuntime) requiredFor(ctx context.Context, github githubrest.Client, pr record.PullRequest, base string, read map[string]requiredchecks.Set) (intake.RequiredChecks, error) {
	key := pr.Repo + "@" + base
	set, ok := read[key]
	if !ok {
		var err error
		if set, err = requiredchecks.Required(ctx, github, base); err != nil {
			return intake.RequiredChecks{}, fmt.Errorf("read the checks %s requires: %w", base, err)
		}
		read[key] = set
	}
	fact := intake.RequiredChecks{Repo: pr.Repo, Number: pr.Number, Names: set.Checks, Workflows: []record.RequiredWorkflow{}}
	if len(set.Workflows) == 0 {
		return fact, nil
	}
	fact.WorkflowsHead = classify.WorkflowHead(pr)
	runs, err := requiredchecks.Workflows(ctx, github, fact.WorkflowsHead, set.Workflows)
	if err != nil {
		return intake.RequiredChecks{}, fmt.Errorf("read the workflow runs on %s: %w", fact.WorkflowsHead, err)
	}
	for _, run := range runs {
		fact.Workflows = append(fact.Workflows, record.RequiredWorkflow{Path: run.Path, Result: run.Result, Run: run.Run, Attempt: run.Attempt})
	}
	return fact, nil
}
