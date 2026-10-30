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

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// followHead is the per-issue read's check of pr's head against pull, GitHub's own read of it.
// The daemon otherwise moves a recorded head only on the pull_request synchronize webhook
// (and a READY the merger reports), and a webhook lost, or waiting behind hours of the GitHub
// consumer's backlog, leaves reviews, READY and the CI verdict judged against an old head. When
// GitHub shows the pull request open at another head than the record holds now (the read looked
// its record up before it asked GitHub, and a webhook may have moved it since), it applies the
// intake.PullRequestSynchronized that webhook would have carried, with GitHub's updated_at as its
// clock, so the workflow decides it as it decides the webhook's (workflow's
// pullRequestSynchronized), behind the same fence: a read older than the record's clock moves
// nothing, and a webhook arriving later that is older than this read moves nothing either. The
// fence orders by GitHub's whole seconds, so a clock equal to the record's is not late. A record a
// webhook moved since the read looked it up is kept when this read is no newer than its clock: the
// read may predate that push within the same second, and following it would move the head back. A
// webhook of the same second arriving later runs as a redelivery does, and one applied between this
// function's read of the record and its apply is still undone by it - the record moves back to the
// head read here, judging it again, until the next read of the issue moves it forward.
//
// Before the synchronize it applies the push that reached the new head when GitHub shows it
// changed only the issue's own .legion/<issue>/ (handoffPush): the push webhook waits in the same
// backlog, and without it a reviewer's approval followed by its handoff push would read as not
// covering the head (classify.ApprovalStands), a round stuck for nothing. A push that may change
// code is left to its webhook, as when a synchronize beats its push: a code head with no known push
// is judged as such, and the push's own handler settles what only the push says, replaying the
// head run the head opened (classify.ApplyPush).
//
// It returns the record as the read goes on with it: as it stands when the head did not move, and
// otherwise as the synchronize left it, so the mergeability and review reads after it judge the
// head GitHub shows. On an error, or a record no longer open, it returns the record as it last
// read it, and the reads after it judge the pull request at that head; the next read of the issue
// follows it.
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
	read, cancel := context.WithTimeout(ctx, issueRead)
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

// applyMergeability applies what GitHub's read of a pull request (pull, one GET /pulls/{number})
// says about merging its head into its base, for the per-issue read (readIssuePullRequest). It
// applies an intake.PullRequestMergeability fact
// whenever GitHub's lazily computed mergeability is CONFLICTING, or when either it or GitHub's
// merge state differs from the one recorded, or when the base GitHub names is not the recorded
// one, or when a push onto that base is recorded with no read of it yet
// (record.PullRequest.BaseMoved): workflow's mergeability decides a CONFLICTING read every time,
// even one that only confirms the stored value, so a conflict recorded while the issue stood in a
// phase the trigger has no row for is still acted on once it reaches one that has; a merge state
// that moved while the mergeability stood still is recorded as the read found it; and this read
// is the one that records the base, both for a pull request whose record has none and as the
// answer to a push onto its base, which GitHub recomputed the mergeability for and sent no event
// about. A read that says what is recorded applies nothing, so a second read of an unchanged pull
// request changes nothing.
// An apply that fails is logged, and the next read of the pull request applies it again.
func (w *workflowRuntime) applyMergeability(ctx context.Context, pr record.PullRequest, pull githubPullRequest) {
	base, mergeable, state := pull.Base.Ref, pull.mergeability(), pull.mergeState()
	if pr.Mergeability == mergeable && pr.MergeState == state && pr.Base == base && pr.BaseMoved == "" && mergeable != record.MergeabilityConflicting {
		return
	}
	eventID := fmt.Sprintf("mergeability:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
	fact := intake.PullRequestMergeability{Repo: pr.Repo, Number: pr.Number, Base: base, Mergeable: mergeable, MergeState: state}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply whether a pull request's head can be merged into its base", "repo", pr.Repo, "pullRequest", pr.Number, "error", err)
	}
}

// handoffPush is the intake.Push that took pr's branch from from to to, as the push webhook would
// have carried it, when GitHub's compare of the two (githubrest.Client.Compare, the compare READY
// reads) shows to is from with commits on top that change only the issue's own handoffs,
// .legion/<issue>/ (classify.HandoffPath): followHead's recorded head and GitHub's, or the
// approved head and the recorded one the approval carry compares (readApprovalCarry, the outbox's
// approvalCarry row). Every other comparison says why it is not
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
