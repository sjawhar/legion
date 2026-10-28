package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// followHead is the required-checks pass's check of pr's head against pull, GitHub's own read of
// it. The daemon otherwise moves a recorded head only on the pull_request synchronize webhook
// (and a READY the merger reports), and a webhook lost, or waiting behind hours of the GitHub
// consumer's backlog, leaves reviews, READY and the CI verdict judged against an old head. When
// GitHub shows the pull request open at another head than the record holds now (the pass listed
// its records when it started, and a webhook may have moved this one since), it applies the
// intake.PullRequestSynchronized that webhook would have carried, with GitHub's updated_at as its
// clock, so the workflow decides it as it decides the webhook's (workflow's
// pullRequestSynchronized), behind the same fence: a read older than the record's clock moves
// nothing, and a webhook arriving later that is older than this read moves nothing either. The
// fence orders by GitHub's whole seconds, so a clock equal to the record's is not late. A record a
// webhook moved after the pass listed it is kept when this read is no newer than its clock: the
// read may predate that push within the same second, and following it would move the head back. A
// webhook of the same second arriving later runs as a redelivery does, and one applied between this
// function's read of the record and its apply is still undone by it - the record moves back to the
// head read here, judging it again, until the next pass moves it forward.
//
// Before the synchronize it applies the push that reached the new head when GitHub shows it
// changed only the issue's own .legion/<issue>/ (handoffPush): the push webhook waits in the same
// backlog, and without it a reviewer's approval followed by its handoff push would read as not
// covering the head (classify.ApprovalStands), a round stuck for nothing. A push that may change
// code is left to its webhook, as when a synchronize beats its push: a code head with no known push
// is judged as such, and the push's own handler settles what only the push says, replaying the
// head run the head opened (classify.ApplyPush).
//
// It returns the record as the pass goes on with it: as it stands when the head did not move, and
// otherwise as the synchronize left it, so the mergeability, review and required-check reads after
// it judge the head GitHub shows. On an error, or a record no longer open, it returns the record as
// it last read it, and the pass reads the pull request at that head; the next pass follows it.
func (w *workflowRuntime) followHead(ctx context.Context, github githubrest.Client, pr record.PullRequest, pull githubPullRequest) (record.PullRequest, error) {
	if pull.State != "open" || pull.Head.SHA == "" || pull.Head.SHA == pr.HeadSHA {
		return pr, nil
	}
	current, err := w.openRecord(ctx, pr)
	if err != nil || current.HeadSHA == pull.Head.SHA {
		return current, err
	}
	if current.HeadSHA != pr.HeadSHA && !pull.UpdatedAt.After(current.HeadUpdatedAt) {
		return current, nil
	}
	pr = current
	read, cancel := context.WithTimeout(ctx, requiredChecksRead)
	push, notHandoff, err := handoffPush(read, github, pr, pr.HeadSHA, pull.Head.SHA)
	handoff := notHandoff == ""
	cancel()
	if err != nil {
		return pr, err
	}
	w.log.Warn("workflow: GitHub shows a pull request at a head the daemon has not recorded; applying the synchronize its webhook would",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "updated_at", pull.UpdatedAt, "handoffOnly", handoff)
	if handoff {
		eventID := fmt.Sprintf("push-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
		if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, push, w.handlers...); err != nil {
			return pr, fmt.Errorf("apply the handoff push to %s: %w", pull.Head.SHA, err)
		}
	}
	eventID := fmt.Sprintf("head-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
	fact := intake.PullRequestSynchronized{Repo: pr.Repo, Number: pr.Number, Branch: pr.Branch, HeadSHA: pull.Head.SHA, Body: pull.Body, UpdatedAt: pull.UpdatedAt}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		return pr, fmt.Errorf("apply the synchronize to %s: %w", pull.Head.SHA, err)
	}
	return w.openRecord(ctx, pr)
}

// openRecord is pr's issue's pull request record as it stands now, which must still be pr's and
// open.
func (w *workflowRuntime) openRecord(ctx context.Context, pr record.PullRequest) (record.PullRequest, error) {
	var current *record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		current, err = w.records.PullRequest(ctx, tx, pr.Issue)
		return err
	}); err != nil {
		return pr, fmt.Errorf("read the pull request record of %s: %w", pr.Issue, err)
	}
	if current == nil || current.State != record.PullRequestOpen || current.Number != pr.Number {
		return pr, fmt.Errorf("%s records pull request #%d open no longer", pr.Issue, pr.Number)
	}
	return *current, nil
}

// handoffPush is the intake.Push that took pr's branch from from to to, as the push webhook would
// have carried it, when GitHub's compare of the two (githubrest.Client.Compare, the compare READY
// reads) shows to is from with commits on top that change only the issue's own handoffs,
// .legion/<issue>/ (classify.HandoffPath): followHead's recorded head and GitHub's, or
// readApprovalCarry's approved head and the recorded one. Every other comparison says why it is not
// one, and the push is left to its webhook: a path outside the issue's handoffs (code, or another
// issue's handoffs, which READY refuses as code too), a rewritten history or a head behind from, no
// file list or a list GitHub may have cut (githubrest.CompareFilesLimit), and a from GitHub does not
// know. Any other failure of the read is an error. The push names no pusher: the pusher decides
// only a code push's planned mark and whether a push spares a fix attempt, which a handoff-only
// push spares whoever pushed it, on a chain from the head the counted move started from
// (classify.sparedMove).
func handoffPush(ctx context.Context, github githubrest.Client, pr record.PullRequest, from, to string) (intake.Push, string, error) {
	compare, err := github.Compare(ctx, from, to)
	if err != nil {
		var answer *githubrest.Answer
		if errors.As(err, &answer) && answer.Status == http.StatusNotFound {
			return intake.Push{}, fmt.Sprintf("GitHub does not know %s, or cannot compare it with %s", from, to), nil
		}
		return intake.Push{}, "", fmt.Errorf("compare %s with %s: %w", from, to, err)
	}
	switch {
	case compare.Status != "ahead":
		return intake.Push{}, fmt.Sprintf("GitHub's compare of %s with %s is %q, so %s is not %s with commits on top", from, to, compare.Status, to, from), nil
	case compare.Files == nil:
		return intake.Push{}, fmt.Sprintf("GitHub's compare of %s with %s answered no file list", from, to), nil
	case len(*compare.Files) == 0:
		return intake.Push{}, fmt.Sprintf("GitHub's compare of %s with %s lists no changed file", from, to), nil
	case len(*compare.Files) >= githubrest.CompareFilesLimit:
		return intake.Push{}, fmt.Sprintf("GitHub's compare of %s with %s lists %d changed files, the most it lists, so a path past them is unread", from, to, len(*compare.Files)), nil
	}
	paths := githubrest.ChangedPaths(*compare.Files)
	for _, path := range paths {
		if !classify.HandoffPath(pr.Issue, path) {
			return intake.Push{}, fmt.Sprintf("%s changed between %s and %s, outside .legion/%s/", path, from, to, pr.Issue), nil
		}
	}
	// Not forced: GitHub's compare says ahead, so to is from with commits on top, and the paths
	// above are what those commits did to from (classify.sparesAttempt, carriesApproval).
	changed, whole, unforced := strings.Join(paths, "\n"), "false", "false"
	return intake.Push{Repo: pr.Repo, Branch: pr.Branch, Before: from, After: to, ChangedPaths: &changed, Truncated: &whole, Forced: &unforced}, "", nil
}

// readUnrecordedPullRequests is the required-checks pass's look for a pull request the daemon has
// no record of. An implementer's completion moves its issue to testing only once the issue's pull
// request is recorded (workflow's implementationReady), and only the opened or reopened webhook
// records it, so one lost, or waiting behind the GitHub consumer's backlog, holds the issue in
// implementing with its pull request open on GitHub. For each issue in implementing whose
// implementer recorded its handoff and that has no open or merged pull request recorded
// (record.Store.HandedOffWithoutPullRequest) it lists the open pull requests on the issue's branch
// (claim.IssueBranch) and applies the intake.PullRequestOpened that webhook would have carried,
// naming the repository as the webhook's fact would (heardSpelling) - a reopen when the record
// holds the same pull request closed - which the workflow decides as it decides the webhook's
// (workflow's pullRequestOpened): it records the pull request and moves the issue on. An issue
// whose implementer has not completed costs no read. ctx bounds the pass; a failed read is logged
// and the next pass asks again, and a rate-limit answer ends the look there.
//
// The workflow can decline the opened (one older than a closed record's clock), and the issue is
// then found again every pass. What it decides by is the pull request as GitHub shows it, the
// issue's generation and its record, so while none of them changes the pass applies it, and warns,
// once (unrecordedOpened), rather than writing a processed_events row and a warning every two
// minutes; a change to any of them applies it again.
func (w *workflowRuntime) readUnrecordedPullRequests(ctx context.Context) {
	var waiting []record.Issue
	recorded := map[string]*record.PullRequest{}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		if waiting, err = w.records.HandedOffWithoutPullRequest(ctx, tx, w.dispatchProject); err != nil {
			return err
		}
		for _, issue := range waiting {
			if recorded[issue.Key], err = w.records.PullRequest(ctx, tx, issue.Key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("list the issues whose implementer completed with no pull request recorded", "error", err)
		}
		return
	}
	// The set keeps, by issue, the opened this pass found declined again, so it holds no pull
	// request that has gone. An issue this pass did not read - its list failed, or GitHub's rate
	// limit or the end of ctx cut the pass short before it - keeps what the last pass learned.
	declined, complete := map[string]string{}, false
	keep := func(issue string) {
		if seen, ok := w.unrecordedOpened[issue]; ok {
			declined[issue] = seen
		}
	}
	defer func() {
		if !complete {
			for issue := range w.unrecordedOpened {
				if _, read := declined[issue]; !read {
					keep(issue)
				}
			}
		}
		w.unrecordedOpened = declined
	}()
	for _, issue := range waiting {
		branch := claim.IssueBranch(issue.Key)
		read, cancel := context.WithTimeout(ctx, requiredChecksRead)
		pull, found, err := w.openPullRequestOn(read, issue.GitHubRepo, branch)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attributes := []any{"repo", issue.GitHubRepo, "issue", issue.Key, "branch", branch, "error", err}
			var answer *githubrest.Answer
			if errors.As(err, &answer) {
				attributes = append(attributes, "status", answer.Status)
			}
			if answer != nil && answer.RateLimited {
				w.log.Error("list the open pull requests on an issue's branch: GitHub's rate limit; this pass looks up no more", attributes...)
				return
			}
			w.log.Error("list the open pull requests on an issue's branch; a later pass looks again", attributes...)
			keep(issue.Key)
			continue
		}
		if !found {
			continue
		}
		prior, repo := recorded[issue.Key], w.heardSpelling(issue.GitHubRepo)
		seen := fmt.Sprintf("%s#%d@%s:%s:%d:%s", repo, pull.Number, pull.Head.SHA, pull.UpdatedAt.Format(time.RFC3339Nano), issue.Generation, recordState(prior))
		if w.unrecordedOpened[issue.Key] == seen {
			declined[issue.Key] = seen
			continue
		}
		fact := intake.PullRequestOpened{Repo: repo, Number: pull.Number, Branch: branch, HeadSHA: pull.Head.SHA, Body: pull.Body,
			URL: pull.HTMLURL, UpdatedAt: pull.UpdatedAt, Reopened: prior != nil && prior.Number == pull.Number}
		w.log.Warn("workflow: GitHub holds an open pull request for an issue whose implementer completed, and the daemon has none recorded; applying the opened its webhook would",
			"repo", repo, "pullRequest", pull.Number, "issue", issue.Key, "head", pull.Head.SHA, "reopened", fact.Reopened, "updated_at", pull.UpdatedAt)
		eventID := fmt.Sprintf("opened-read:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, repo, pull.Number, time.Now().UnixNano())
		if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
			w.log.Warn("apply an open pull request read from GitHub", "repo", repo, "pullRequest", pull.Number, "issue", issue.Key, "error", err)
			continue
		}
		var after *record.PullRequest
		if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
			var err error
			after, err = w.records.PullRequest(ctx, tx, issue.Key)
			return err
		}); err != nil {
			w.log.Warn("read whether the workflow recorded an open pull request read from GitHub", "repo", repo, "pullRequest", pull.Number, "issue", issue.Key, "error", err)
			continue
		}
		if after == nil || after.State != record.PullRequestOpen || after.Number != pull.Number {
			w.log.Info("workflow: the workflow did not record the open pull request; it is applied again once GitHub's pull request, the issue's generation or its record changes",
				"repo", repo, "pullRequest", pull.Number, "issue", issue.Key)
			declined[issue.Key] = seen
		}
	}
	complete = true
}

// recordState is what of an issue's pull request record the workflow decides an opened by: none,
// or its number, state and clock.
func recordState(pr *record.PullRequest) string {
	if pr == nil {
		return "none"
	}
	return fmt.Sprintf("%d/%s/%s", pr.Number, pr.State, pr.HeadUpdatedAt.Format(time.RFC3339Nano))
}

// heardSpelling is repo as GitHub intake hears it, the spelling a webhook's fact names it by and a
// pull request is recorded and looked up under: a served repository as the mapping names it, and
// any other as its record keeps GitHub's spelling (heardRepositories).
func (w *workflowRuntime) heardSpelling(repo string) string {
	if w.repositories != nil {
		for _, served := range w.repositories.Accepted() {
			if strings.EqualFold(served.String(), repo) {
				return served.String()
			}
		}
	}
	return repo
}

// openPullRequestOn is the open pull request of repo on branch (GET
// /pulls?head=<owner>:<branch>&state=open), and whether there is one. The list's filter is not
// taken on trust: a listed pull request counts only when its head.ref is branch, the same identity
// the webhook path reads (intake's decodePullRequest takes head_ref alone), whichever repository
// the head lives in. The workflow keeps one pull request per branch and assumes one is open at a
// time, so of two it takes the first GitHub lists.
func (w *workflowRuntime) openPullRequestOn(ctx context.Context, repo, branch string) (githubPullRequest, bool, error) {
	repository, err := ghrepo.Parse("the issue's repository", repo)
	if err != nil {
		return githubPullRequest{}, false, err
	}
	github, err := w.implementClient(ctx, repository)
	if err != nil {
		return githubPullRequest{}, false, err
	}
	var pulls []githubPullRequest
	query := url.Values{"head": {repository.Owner() + ":" + branch}, "state": {"open"}}
	if err := github.Get(ctx, "/pulls?"+query.Encode(), &pulls); err != nil {
		return githubPullRequest{}, false, err
	}
	for _, pull := range pulls {
		if pull.Head.Ref == branch {
			return pull, true, nil
		}
	}
	return githubPullRequest{}, false, nil
}
