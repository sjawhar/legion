package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
// Before the synchronize it applies, oldest first, the pushes that took the branch to the new head,
// as their push webhooks would have carried them (headPushes): those webhooks wait in the same
// backlog, or were lost, and the workflow judges a move by the chain of its pushes - whether it is a
// fix attempt, and whether the red it leaves is the review App's planned one
// (classify.AdvancePullRequestHead). When a recorded push already ends at the new head it applies
// none, and when no recorded push does they are the pushes GitHub's activity of the branch shows
// (lostPushes); a push that activity cannot account for is left to its webhook, as when a
// synchronize beats its push: a head with no known push is judged as such, and the push's own
// handler settles what only the push says, replaying the head run the head opened
// (classify.ApplyPush).
//
// It returns the record as the pass goes on with it: as it stands when the head did not move, and
// otherwise as the synchronize left it, so the mergeability, review and required-check reads after
// it judge the head GitHub shows. On an error, or a record no longer open, it returns the record as
// it last read it, and the pass reads the pull request at that head; the next pass follows it.
func (w *workflowRuntime) followHead(ctx context.Context, github githubrest.Client, pr record.PullRequest, pull githubrest.PullRequest) (record.PullRequest, error) {
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
	// A synchronize the pass applied before at this head, from this recorded one, is GitHub's same
	// observation, which the workflow declined (an updated_at older than the record's clock): it is
	// neither compared nor applied again.
	eventID := w.passEventID("head-read", pr.Repo, pr.Number, pr.HeadSHA+"..."+pull.Head.SHA, clock(pull.UpdatedAt))
	if !w.unapplied(ctx, eventID) {
		return pr, nil
	}
	pushes, err := w.headPushes(ctx, github, pr, pull.Head.SHA)
	if err != nil {
		return pr, err
	}
	w.log.Warn("workflow: GitHub shows a pull request at a head the daemon has not recorded; applying the synchronize its webhook would",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "recorded", pr.HeadSHA, "head", pull.Head.SHA, "updated_at", pull.UpdatedAt, "pushes", len(pushes))
	for _, push := range pushes {
		if !w.unapplied(ctx, push.eventID) {
			continue
		}
		if _, err := intake.ApplyFact(ctx, w.pool, "github", push.eventID, push.fact, w.handlers...); err != nil {
			return pr, fmt.Errorf("apply the push from %s to %s: %w", push.fact.Before, push.fact.After, err)
		}
	}
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

// passPush is a push the pass rebuilt from its reads of GitHub, with the event id it is applied
// under.
type passPush struct {
	eventID string
	fact    intake.Push
}

// headPushes is the pushes followHead applies, oldest first, before the synchronize that moves pr
// from its recorded head to head. When a recorded push already ends at head
// (classify.HeadPushKnown) there are none: the record knows the move. When none does, its webhook
// lost or waiting behind the GitHub consumer's backlog, they are the pushes GitHub's activity of
// the branch shows took it there (lostPushes); when that activity cannot be followed, which it
// logs, there are none either, and the pushes are left to their webhooks. A rate-limit answer, or
// ctx ending, is an error.
func (w *workflowRuntime) headPushes(ctx context.Context, github githubrest.Client, pr record.PullRequest, head string) ([]passPush, error) {
	if classify.HeadPushKnown(pr, head) {
		return nil, nil
	}
	pushes, unfollowed, err := w.lostPushes(ctx, github, pr, head)
	if err != nil || unfollowed == "" {
		return pushes, err
	}
	w.log.Warn("workflow: the pass cannot rebuild from GitHub's activity the pushes that moved a pull request's head; it applies none of them, and the synchronize",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "branch", pr.Branch, "recorded", pr.HeadSHA, "head", head, "reason", unfollowed)
	return nil, nil
}

// lostPushes is the pushes that took pr's branch from the recorded head to head, oldest first, as
// their push webhooks would have carried them, less those the record holds already (its pushes and
// the moves it made): GitHub's activity of the branch (githubrest.Client.BranchActivity, its newest
// hundred entries) followed back from head to the recorded head (followActivity). Each is GitHub's
// push, force push or branch creation, with Before and After its two commits and Pusher its
// actor's login - an App's <slug>[bot], the form of the push webhook's pusher.name, by which the
// workflow tells the review App's pushes. Repo and Branch are the record's, the names the webhook's
// push carries. The workflow judges a move by the chain of its pushes, so one push from the
// recorded head to head would judge it wrong. Each push's event id names the project, the pull
// request and the push's two commits (pushReadEventID), so a later pass skips it before any
// transaction, and the push webhook arriving later is the same push recorded again, which the
// record keeps as its move's push or a repeat of it and which changes no judgement
// (classify.ApplyPush).
//
// unfollowed says why the pushes cannot be rebuilt, and then there are none: the page does not
// lead back from head to the recorded head, an entry on the way is no push, or GitHub answered the
// read with an error. A rate-limit answer, or ctx ending, is an error: the pass ends there.
func (w *workflowRuntime) lostPushes(ctx context.Context, github githubrest.Client, pr record.PullRequest, head string) ([]passPush, string, error) {
	read, cancel := context.WithTimeout(ctx, requiredChecksRead)
	activity, err := github.BranchActivity(read, pr.Branch)
	cancel()
	if err != nil {
		if endsPass(ctx, err) {
			return nil, "", fmt.Errorf("read the activity of %s: %w", pr.Branch, err)
		}
		return nil, fmt.Sprintf("the read of the branch's activity failed: %v", err), nil
	}
	chain, unfollowed := followActivity(activity, pr.HeadSHA, head)
	if unfollowed != "" {
		return nil, unfollowed, nil
	}
	recorded := map[[2]string]bool{}
	for _, p := range pr.Pushes {
		recorded[[2]string{p.Before, p.SHA}] = true
	}
	for _, m := range pr.Moves {
		recorded[[2]string{m.Before, m.SHA}] = true
	}
	var pushes []passPush
	for _, entry := range chain {
		if recorded[[2]string{entry.Before, entry.After}] {
			continue
		}
		pushes = append(pushes, passPush{
			eventID: w.pushReadEventID(pr, entry.Before, entry.After),
			fact:    intake.Push{Repo: pr.Repo, Branch: pr.Branch, Before: entry.Before, After: entry.After, Pusher: entry.Actor.Login},
		})
	}
	return pushes, "", nil
}

// endsPass says whether err, a read of the pass that failed, ends the whole pass: GitHub's rate
// limit, which every later read on the installation meets, or ctx ending.
func endsPass(ctx context.Context, err error) bool {
	var answer *githubrest.Answer
	return ctx.Err() != nil || (errors.As(err, &answer) && answer.RateLimited)
}

// pushActivity is the kinds of GitHub's activity of a branch that are its pushes, whose push
// webhook carries the same two commits: a push, a force push, and the push that created the branch.
var pushActivity = map[string]bool{"push": true, "force_push": true, "branch_creation": true}

// followActivity is the entries of activity, GitHub's activity of a branch newest first, that took
// the branch from from to to, oldest first: from the newest entry that left the branch at to, each
// older entry in turn, each of which left the branch at the commit the newer one replaced, until one
// replaced from. Every entry on the way must be a push (pushActivity). unfollowed says why the page
// holds no such chain: no entry left the branch at to, the entries break off, one is no push, or the
// page ends before from.
func followActivity(activity []githubrest.Activity, from, to string) ([]githubrest.Activity, string) {
	start := slices.IndexFunc(activity, func(entry githubrest.Activity) bool { return entry.After == to })
	if start < 0 {
		return nil, fmt.Sprintf("none of the %d newest entries of the branch's activity left it at %s", len(activity), to)
	}
	var chain []githubrest.Activity
	at := to
	for _, entry := range activity[start:] {
		switch {
		case entry.After != at:
			return nil, fmt.Sprintf("the branch's activity before it reached %s left it at %s", at, entry.After)
		case !pushActivity[entry.Type]:
			return nil, fmt.Sprintf("the branch's activity reached %s by a %s, not a push", at, entry.Type)
		}
		chain = append(chain, entry)
		if entry.Before == from {
			slices.Reverse(chain)
			return chain, ""
		}
		at = entry.Before
	}
	return nil, fmt.Sprintf("the %d newest entries of the branch's activity do not lead back from %s to the recorded head %s", len(activity), to, from)
}

// readListings is the required-checks pass's listing of the open pull requests of every repository
// the daemon serves (githubrest.Client.OpenPullRequests), a request per hundred open pull requests
// of each, whatever their branches, for what no read of a recorded pull request sees. Only the
// opened and reopened webhooks record a pull request open, in whatever phase its issue is
// (workflow's pullRequestOpened), so one lost, or waiting behind the GitHub consumer's backlog,
// leaves the record without it: a pull request opened in planning or testing, reopened in
// reviewing, opened again on a done issue's branch, or the one an implementing issue's completion
// waits for. Each listed pull request on an issue's branch of the repository itself, not a fork's
// (githubrest.PullRequest.HeadIn), is applied as its webhook would have been (recordListed), the
// first GitHub lists of each branch, as the workflow keeps one pull request per branch. Then each
// issue of the repository in implementing whose implementer recorded its handoff, with no open or
// merged pull request recorded and none listed on its branch, is told to its architect
// (tellMissing). A listing that fails tells nothing of its repository, and the next pass lists it
// again; a rate-limit answer ends the pass there, since every later read on the installation meets
// the same limit, and the panes share it. ctx bounds the pass.
func (w *workflowRuntime) readListings(ctx context.Context) {
	var handedOff []record.HandedOff
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		handedOff, err = w.records.HandedOffWithoutOpenOrMergedPullRequest(ctx, tx, w.dispatchProject)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("list the issues whose implementer completed with no open or merged pull request recorded", "error", err)
		}
		return
	}
	for _, repository := range w.repositories.Accepted() {
		repo := repository.String()
		read, cancel := context.WithTimeout(ctx, requiredChecksRead)
		pulls, err := w.openPullRequests(read, repository)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if w.logReadFailure(err, "list a repository's open pull requests: GitHub's rate limit; this pass reads no more",
				"list a repository's open pull requests; an open pull request the daemon has not recorded, and an issue whose completion waits for one, stay as they are until a later pass lists it",
				"repo", repo) {
				return
			}
			continue
		}
		listed := map[string]bool{}
		for _, pull := range pulls {
			issue, ok := claim.BranchIssue(pull.Head.Ref)
			if !ok || listed[issue] || !pull.HeadIn(repo) {
				continue
			}
			listed[issue] = true
			w.recordListed(ctx, repo, issue, pull)
		}
		for _, waiting := range handedOff {
			if strings.EqualFold(waiting.Issue.GitHubRepo, repo) && !listed[waiting.Issue.Key] {
				w.tellMissing(ctx, repo, waiting)
			}
		}
	}
}

// openPullRequests is every open pull request of repository as the implement App lists them.
func (w *workflowRuntime) openPullRequests(ctx context.Context, repository ghrepo.Repository) ([]githubrest.PullRequest, error) {
	github, err := w.implementClient(ctx, repository)
	if err != nil {
		return nil, err
	}
	return github.OpenPullRequests(ctx)
}

// recordListed applies the intake.PullRequestOpened the webhook of pull, listed open on key's
// branch of repo, would have carried, naming repo as the mapping spells it, as a webhook's fact
// names it, when the project records key on repo with no open pull request: a reopen when the
// record holds the same pull request. The workflow decides it as it decides the webhook's
// (workflow's pullRequestOpened): it records the pull request in whatever phase the issue is, moves
// an implementing issue whose implementer completed on to testing, and declines a reopen of a
// merged pull request, one older than the record's clock, and one opened from the branch of an
// issue that waits to close on its merge. Its event id is what the workflow decides by
// (intake.OpenedRead), so a declined opened, found again every pass, is applied and warned
// of once while neither GitHub's pull request, the issue's generation nor its record changes, and
// the implementer's completion, reading the same pull request, applies the same fact.
func (w *workflowRuntime) recordListed(ctx context.Context, repo, key string, pull githubrest.PullRequest) {
	var issue *record.Issue
	var prior *record.PullRequest
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		if issue, err = w.records.Issue(ctx, tx, key); err != nil || issue == nil {
			return err
		}
		prior, err = w.records.PullRequest(ctx, tx, key)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			w.log.Warn("read the record of an issue GitHub lists an open pull request for", "repo", repo, "pullRequest", pull.Number, "issue", key, "error", err)
		}
		return
	}
	if issue == nil || issue.Project != w.dispatchProject || !strings.EqualFold(issue.GitHubRepo, repo) || (prior != nil && prior.State == record.PullRequestOpen) {
		return
	}
	fact, eventID := intake.OpenedRead(w.dispatchProject, repo, pull.Head.Ref, pull, issue.Generation, prior)
	if !w.unapplied(ctx, eventID) {
		return
	}
	w.log.Warn("workflow: GitHub lists an open pull request on an issue's branch that the daemon does not record open; applying the opened its webhook would",
		"repo", repo, "pullRequest", pull.Number, "issue", key, "phase", issue.Phase, "head", pull.Head.SHA, "reopened", fact.Reopened, "updated_at", pull.UpdatedAt)
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply an open pull request read from GitHub", "repo", repo, "pullRequest", pull.Number, "issue", key, "error", err)
	}
}

// tellMissing applies the intake.PullRequestMissing of waiting, an issue of repo in implementing
// whose implementer recorded its handoff, with no open or merged pull request recorded and none
// GitHub lists on its branch, and the workflow tells its architect (workflow's pullRequestMissing).
// Its event id names the issue's generation and the handoff commit, so the architect is told once
// per handoff commit, by this pass or any later one, and again only for a completion at another
// commit.
func (w *workflowRuntime) tellMissing(ctx context.Context, repo string, waiting record.HandedOff) {
	key, branch := waiting.Issue.Key, claim.IssueBranch(waiting.Issue.Key)
	eventID := fmt.Sprintf("pull-request-missing:%s:%s:%d:%s", w.dispatchProject, key, waiting.Issue.Generation, waiting.Commit)
	if !w.unapplied(ctx, eventID) {
		return
	}
	w.log.Warn("workflow: GitHub lists no open pull request on the branch of an issue whose implementer completed; telling its architect",
		"repo", repo, "issue", key, "branch", branch, "handoff", waiting.Commit)
	fact := intake.PullRequestMissing{Issue: key, Repo: repo, Branch: branch, HandoffCommit: waiting.Commit}
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply a missing pull request read from GitHub", "repo", repo, "issue", key, "error", err)
	}
}

// passEventID is the event id, under source "github", of a fact the required-checks pass builds
// from its own read of GitHub about pull request number of repo: kind, the project, the pull
// request, and observed, what the read saw - a head, GitHub's updated_at, a review's id, a merge
// commit - and, for a fact whose handler judges the read against the record as it stands, what it
// is judged against (decidedBy). It holds no read time and no boot, so one observation is one fact
// however many passes, boots or daemons read it, and unapplied finds it applied before any
// transaction.
func (w *workflowRuntime) passEventID(kind, repo string, number int, observed ...string) string {
	return fmt.Sprintf("%s:%s:%s#%d:%s", kind, w.dispatchProject, repo, number, strings.Join(observed, ":"))
}

// pushReadEventID is the event id of a push from before to after on pr's branch that the pass
// rebuilt from its reads of GitHub (passEventID): the same whichever read rebuilt it - the compare
// of the recorded head with the new one (headPushes) or the branch's activity (lostPushes) - so the
// two commits are one fact.
func (w *workflowRuntime) pushReadEventID(pr record.PullRequest, before, after string) string {
	return w.passEventID("push-read", pr.Repo, pr.Number, before+"..."+after)
}

// clock is GitHub's time t as an event id spells it.
func clock(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// unapplied says whether processed_events holds no eventID under source "github"
// (intake.Applied), read with no transaction and no fact lock: a fact this pass, an earlier one,
// another boot or the implementer's completion applied already costs that one read and no wait
// behind the GitHub consumer's facts. A failed read is logged and says false, leaving the fact to a
// later pass, as a failed apply would.
func (w *workflowRuntime) unapplied(ctx context.Context, eventID string) bool {
	applied, err := intake.Applied(ctx, w.pool, "github", []string{eventID})
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("read whether a fact the pass read from GitHub is applied; a later pass applies it", "event", eventID, "error", err)
		}
		return false
	}
	return !applied[eventID]
}

// decidedBy is a digest of read, a fact the pass built for pr, and of what the workflow decides it
// by beside it: pr's record as the pass holds it, and its issue's phase and generation as recorded
// now. It is for a fact the pass applies when its read differs from the record, or decides again
// on an unchanged read (a CONFLICTING mergeability, a required read whose hold lapsed): the same
// read against the same record and phase is one fact, applied once, and a record or phase that
// changed since - a webhook, an earlier fact of the pass, the issue reaching awaiting_merge with its
// conflict recorded, a red the lapse decided - makes the same read a new one.
func (w *workflowRuntime) decidedBy(ctx context.Context, pr record.PullRequest, read intake.Fact) (string, error) {
	var issue *record.Issue
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = w.records.Issue(ctx, tx, pr.Issue)
		return err
	}); err != nil {
		return "", fmt.Errorf("read the issue of %s#%d: %w", pr.Repo, pr.Number, err)
	}
	var at struct {
		Phase      phase.Phase
		Generation uint64
	}
	if issue != nil {
		at.Phase, at.Generation = issue.Phase, issue.Generation
	}
	hash := sha256.New()
	if err := json.NewEncoder(hash).Encode([]any{read, pr, at}); err != nil {
		return "", fmt.Errorf("digest what %s#%d is judged by: %w", pr.Repo, pr.Number, err)
	}
	return hex.EncodeToString(hash.Sum(nil)[:16]), nil
}
