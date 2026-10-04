package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/requiredchecks"
)

// requiredChecksInterval is how often watchRequiredChecks reads every open pull request's required
// checks after its first pass. GitHub tells an App of a ruleset or branch-protection change only by
// webhooks (repository_ruleset, branch_protection_rule) the Envoy listener does not carry to the
// daemon, so reading again is how the daemon notices one: a change reaches the verdict within one
// interval. A pass reads each open pull request once and each base branch at least twice (its
// rulesets, a page per hundred rules, and its protection), on the implement App's installation,
// whose hourly limit every pane's gh shares: about ninety reads an hour for a pull request on a base
// of its own, thirty for each more on the same base. Two minutes also gives a newly opened pull
// request its set well before its first CI settles.
const requiredChecksInterval = 2 * time.Minute

// requiredChecksRead bounds one pull request's reads in a pass: its base branch, then that branch's
// rulesets and protection, each a single GitHub answer that comes in well under a second.
const requiredChecksRead = 30 * time.Second

// watchRequiredChecks reads the checks each open pull request's base branch requires
// (readRequiredChecks) as the runtime starts and every requiredChecksInterval after, until ctx
// ends. The first pass is the boot's resync: a pull request whose set was never read - every one
// recorded before the daemon read sets at all - has no checks verdict until it runs, and the round
// a red the base branch never required left stuck is decided by it, with no new push.
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

// readRequiredChecks is one pass: for each open pull request of the project it reads the base
// branch's required checks with the implement App's installation token, the credential the daemon
// reads GitHub with for its own work (requiredchecks.Required, the reader READY uses), and applies
// an intake.RequiredChecks fact when the set differs from the one recorded, which decides the
// head's verdict again (workflow's requiredChecks). A read that fails is logged with the
// repository, the pull request and GitHub's HTTP status, and changes nothing: the pull request
// keeps the set last read, and one never read keeps no checks verdict, neither red nor green, until
// a later pass reads it. A rate-limit answer ends the pass there, since every read after it on the
// same installation meets the same limit, and the panes share it; the next pass starts over.
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
	read := map[string][]string{}
	for _, pr := range open {
		names, err := w.requiredFor(ctx, pr, read)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			attributes := []any{"repo", pr.Repo, "pullRequest", pr.Number, "error", err}
			var answer *githubrest.Answer
			if errors.As(err, &answer) {
				attributes = append(attributes, "status", answer.Status)
			}
			if answer != nil && answer.RateLimited {
				w.log.Error("read the checks a pull request's base branch requires: GitHub's rate limit; this pass reads no more pull requests, and every unread one's checks verdict stays as the set last read decides it", attributes...)
				return
			}
			w.log.Error("read the checks a pull request's base branch requires; its checks verdict stays as the set last read decides it, and undecided when none was", attributes...)
			continue
		}
		if pr.RequiresExactly(names) {
			continue
		}
		eventID := fmt.Sprintf("required-checks:%s:%s:%s#%d:%d", w.dispatchProject, w.bootID, pr.Repo, pr.Number, time.Now().UnixNano())
		if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, intake.RequiredChecks{Repo: pr.Repo, Number: pr.Number, Names: names}, w.handlers...); err != nil {
			w.log.Warn("apply the checks a pull request's base branch requires", "repo", pr.Repo, "pullRequest", pr.Number, "error", err)
		}
	}
}

// requiredFor reads the checks pr's base branch requires: the pull request, for its base, then the
// base's rulesets and protection, which read keeps for the rest of the pass by repository and
// base, so pull requests on one base read them once.
func (w *workflowRuntime) requiredFor(ctx context.Context, pr record.PullRequest, read map[string][]string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, requiredChecksRead)
	defer cancel()
	repository, err := ghrepo.Parse("the pull request's repository", pr.Repo)
	if err != nil {
		return nil, err
	}
	lease, err := w.tokens.Token(ctx, appauth.Implement, repository.Owner())
	if err != nil {
		return nil, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
	}
	github := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(w.githubAPI, repository)}
	var pull struct {
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", pr.Number), &pull); err != nil {
		return nil, err
	}
	key := pr.Repo + "@" + pull.Base.Ref
	if names, ok := read[key]; ok {
		return names, nil
	}
	names, err := requiredchecks.Required(ctx, github, pull.Base.Ref)
	if err != nil {
		return nil, fmt.Errorf("read the checks %s requires: %w", pull.Base.Ref, err)
	}
	read[key] = names
	return names, nil
}
