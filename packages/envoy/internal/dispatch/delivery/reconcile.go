// Reconcile (LEGION-567): the five-minute GitHub-App pass that backfills 28 days on first run and
// catches whatever intake's NATS consumer missed, through the same upserts intake uses -- so
// "reconcile catches a missed event" is provable as "reconcile and intake write through the same
// store functions", never two implementations that can drift. Shares its jittered-ticker loop
// with architecture.Run via duty.RunJittered (idle without GitHub App credentials, same shape as
// architecture.Importer and Intake).
package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ReconcileInterval is how often reconcile re-imports GitHub facts. The Delivery page's
// freshness row promises a missed event appears "by the next five-minute reconcile".
const ReconcileInterval = 5 * time.Minute

// BackfillWindow is how far back the first reconcile pass imports, per the issue's "28-day
// backfill on first run".
const BackfillWindow = 28 * 24 * time.Hour

// reconcileOverlap extends every pass but the first back past its own last run, so a merge or run
// GitHub's search/listing index was still catching up on at the previous pass's boundary is not
// permanently missed. The shared upsert makes re-reading anything in the overlap a no-op, not a
// duplicate.
const reconcileOverlap = time.Hour

// Reconcile wires the five-minute ticker to the database and the GitHub App client. A nil GitHub
// client (no App credentials) leaves it idle, exactly like architecture.Importer and Intake.
type Reconcile struct {
	pool   *store.Pool
	github *githubapp.Client
}

// NewReconcile builds a Reconcile.
func NewReconcile(pool *store.Pool, github *githubapp.Client) *Reconcile {
	return &Reconcile{pool: pool, github: github}
}

// HasApp reports whether the GitHub App credentials needed to do any work are configured.
func (r *Reconcile) HasApp() bool {
	return r != nil && r.github != nil
}

// Run re-imports delivery facts on ReconcileInterval until ctx is cancelled. The first tick is
// jittered so a restart does not stampede GitHub, and a server without App credentials logs once
// and stops: no import can be signed.
func (r *Reconcile) Run(ctx context.Context) {
	duty.RunJittered(ctx, ReconcileInterval, func() bool {
		if r.HasApp() {
			return false
		}
		slog.Info("dispatch delivery: no GitHub App key — the reconcile is idle")
		return true
	}, r.runOnce)
}

// runOnce performs one reconcile pass: a permission check (so a missing Actions or Pull-requests
// grant is named, never confused with a transient failure), the population pull-request search,
// the deploy and PR-checks workflows' runs and jobs, and any partial row left over from a
// completing fetch that failed earlier. last_reconcile_at advances ONLY when every step that ran
// succeeded; any failure (a missing permission, a rate limit, any other GitHub or store error) is
// recorded by name in last_error instead, and the previous last_reconcile_at is left untouched --
// so the freshness row can never report a healthy timestamp for a pass that silently did nothing.
func (r *Reconcile) runOnce(ctx context.Context) {
	now := time.Now()
	settings, err := GetSettings(ctx, r.pool)
	if err != nil {
		if errors.Is(err, ErrNoSettings) {
			slog.Info("dispatch delivery: delivery is not configured — the reconcile is idle")
			return
		}
		slog.Error("dispatch delivery: read settings for reconcile", "error", err)
		return
	}
	owner, repo, err := splitRepo(settings.DeployRepo)
	if err != nil {
		slog.Error("dispatch delivery: deploy_repo setting", "error", err)
		r.fail(ctx, err)
		return
	}

	// Checked first and named distinctly from every other failure below: a missing Actions or
	// Pull-requests permission would otherwise fail every GitHub call this pass makes in exactly
	// the same shape as a transient 5xx, which this slice's acceptance bar (LEGION-567) requires
	// surfacing loudly and by name, not folding into a generic fetch failure.
	if _, err := r.github.DeliveryPermissions(ctx, owner, repo); err != nil {
		slog.Error("dispatch delivery: GitHub App permission check", "error", err)
		r.fail(ctx, err)
		return
	}

	since := r.windowStart(settings, now)

	var failures []string
	record := func(step string, err error) {
		if err == nil {
			return
		}
		slog.Error("dispatch delivery: "+step, "error", err)
		failures = append(failures, fmt.Sprintf("%s: %s", step, err))
	}

	record("reconcile merged pull requests", r.reconcileMergedPullRequests(ctx, settings, owner, repo, since, now))
	record("reconcile partial pull requests", r.reconcilePartialPullRequests(ctx))
	record("reconcile deploy workflow runs", r.reconcileWorkflow(ctx, owner, repo, settings.DeployRepo, settings.DeployWorkflowPath, DeliveryRunKindDeploy, since, now))
	if settings.PRChecksWorkflowPath != settings.DeployWorkflowPath {
		record("reconcile PR-checks workflow runs", r.reconcileWorkflow(ctx, owner, repo, settings.DeployRepo, settings.PRChecksWorkflowPath, DeliveryRunKindPRChecks, since, now))
	}

	if len(failures) > 0 {
		r.fail(ctx, errors.New(strings.Join(failures, "; ")))
		return
	}
	if err := RecordReconcileSuccess(ctx, r.pool, now); err != nil {
		slog.Error("dispatch delivery: record reconcile success", "error", err)
	}
}

// fail records a reconcile pass's failure reason in delivery_settings.last_error without
// advancing last_reconcile_at (RecordReconcileError's own contract).
func (r *Reconcile) fail(ctx context.Context, cause error) {
	if err := RecordReconcileError(ctx, r.pool, cause.Error()); err != nil {
		slog.Error("dispatch delivery: record reconcile error", "error", err)
	}
}

// windowStart is the 28-day backfill on the first pass (no recorded last_reconcile_at), else the
// last pass's own time minus reconcileOverlap.
func (r *Reconcile) windowStart(settings DeliverySettings, now time.Time) time.Time {
	if settings.LastReconcileAt == nil {
		return now.Add(-BackfillWindow)
	}
	return settings.LastReconcileAt.Add(-reconcileOverlap)
}

// reconcileMergedPullRequests searches every population pull request merged in [since, until)
// across the whole installation (LEGION-294's population spans any repository the three authors
// merge into, not just the deploy repository), classifies each with the labels the search result
// already carries, and upserts the ones that belong -- complete, except for MergeCommitSHA,
// Additions, Deletions and FirstCommitAt, which the search response never carries (the
// per-repository tokenOwner/tokenRepo pair -- the configured deploy repository -- only resolves
// which installation to search as).
func (r *Reconcile) reconcileMergedPullRequests(ctx context.Context, settings DeliverySettings, tokenOwner, tokenRepo string, since, until time.Time) error {
	found, err := SearchMergedPullRequestsAcrossInstallation(ctx, r.github, tokenOwner, tokenRepo, settings.PopulationAuthors, since, until)
	if err != nil {
		return fmt.Errorf("search merged pull requests: %w", err)
	}
	for _, pr := range found {
		if pr.MergedAt == nil {
			continue
		}
		repoFull, err := r.searchResultRepo(pr)
		if err != nil {
			slog.Warn("dispatch delivery: merged-PR search result", "number", pr.Number, "error", err)
			continue
		}
		if err := r.reconcilePullRequest(ctx, settings, repoFull, pr); err != nil {
			slog.Warn("dispatch delivery: reconcile pull request", "repo", repoFull, "number", pr.Number, "error", err)
		}
	}
	return nil
}

// searchResultRepo recovers "owner/repo" from a search result's HTML URL
// (https://github.com/<owner>/<repo>/pull/<n>): GitHub's search-issues response does not carry a
// separate repository field, only the issue/PR's own URL.
func (r *Reconcile) searchResultRepo(pr FetchedPullRequest) (string, error) {
	const prefix = "https://github.com/"
	if len(pr.URL) <= len(prefix) || pr.URL[:len(prefix)] != prefix {
		return "", fmt.Errorf("pull request URL %q does not start with %s", pr.URL, prefix)
	}
	rest := pr.URL[len(prefix):]
	owner, repoTail, err := splitRepo(rest)
	if err != nil {
		return "", fmt.Errorf("pull request URL %q: %w", pr.URL, err)
	}
	// repoTail is "repo/pull/N"; keep only the repo segment.
	repoName, _, found := strings.Cut(repoTail, "/")
	if !found {
		return "", fmt.Errorf("pull request URL %q: no /pull/ segment", pr.URL)
	}
	return owner + "/" + repoName, nil
}

// reconcilePullRequest classifies and upserts one merged pull request the search found, the same
// decision handlePullRequestEnvelope makes: population membership from author (already true,
// since the search itself filtered on it)/repository/task-label, then the row. A deploy-repo PR
// the label rule cannot classify yet (IsTaskPR's "neither label" error) is logged and left for a
// later pass, exactly as intake does.
func (r *Reconcile) reconcilePullRequest(ctx context.Context, settings DeliverySettings, repoFull string, pr FetchedPullRequest) error {
	raw := RawPullRequest{Repo: repoFull, Number: pr.Number, Title: pr.Title, Author: pr.Author, Labels: pr.Labels, MergedAt: *pr.MergedAt}
	isPopulation, err := IsPopulationPR(raw, settings, TimeWindow{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)})
	if err != nil {
		return fmt.Errorf("classify: %w", err)
	}
	if !isPopulation {
		return DeletePullRequest(ctx, r.pool, repoFull, pr.Number)
	}
	return UpsertPullRequest(ctx, r.pool, DeliveryPullRequest{
		Repo: repoFull, Number: pr.Number, Title: pr.Title, URL: pr.URL, Author: pr.Author,
		CreatedAt: &pr.CreatedAt, MergedAt: pr.MergedAt, Rework: IsRework(pr.Title),
		Partial: true, // the search response never carries MergeCommitSHA/Additions/Deletions/FirstCommitAt
	})
}

// reconcilePartialConcurrency bounds how many partial pull requests reconcilePartialPullRequests
// completes at once. Each one makes two sequential GitHub calls (FetchPullRequest, then
// fetchSessionTrailers once merged); run one at a time, a 3,500-PR backfill (CONTRACT.md's own
// measured population size) serializes thousands of round trips end to end. GitHub documents only
// a 100-concurrent-request secondary-rate-limit ceiling and advises against running many requests
// concurrently at all, not a specific safe number below it -- 8 is this package's own
// conservative, arbitrary choice, picked to meaningfully parallelize a backfill without treating
// a single reconcile pass as free to hammer the API as hard as GitHub's documented ceiling
// allows; the rate-limit-aware stop in reconcilePartialPullRequests is what actually protects
// against running too hot, not this number.
const reconcilePartialConcurrency = 8

// reconcilePartialPullRequests completes every stored partial row (written by intake when its
// completing fetch failed, or by this reconcile's own merged-PR search, which never carries the
// completing fields) with a single-pull-request fetch, up to reconcilePartialConcurrency at once.
// Each partial row is a distinct (repo, number) key, so concurrent upserts never race each other.
// errgroup.WithContext(ctx)'s own semantics give the redeliver.Sweeper rate-limit pattern
// directly: SetLimit bounds concurrency, and the first goroutine to return a non-nil error (here,
// specifically a *githubapp.RateLimitError) cancels groupCtx -- the loop checks it before
// starting each further completion, so nothing new launches once that happens, while every
// completion already in flight (there is no cheap way to cancel a request already sent, and
// GitHub's own rate-limit window does not care whether it finishes) runs to completion.
// Wait returns that first error, so the pass is reported failed and last_reconcile_at does not
// advance past rows this pass never got to -- the next pass's overlap re-reads them rather than
// losing them for good. Any other per-row error (a 404, a deleted repository, a malformed answer)
// is still only logged and skipped inside completePartialPullRequest, never returned: it is that
// one row's own problem, not a reason to stop the rest of the batch.
func (r *Reconcile) reconcilePartialPullRequests(ctx context.Context) error {
	partials, err := ListPartialPullRequests(ctx, r.pool)
	if err != nil {
		return fmt.Errorf("list partial pull requests: %w", err)
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(reconcilePartialConcurrency)
	for _, pr := range partials {
		if groupCtx.Err() != nil {
			break
		}
		group.Go(func() error {
			return r.completePartialPullRequest(groupCtx, pr)
		})
	}
	if err := group.Wait(); err != nil {
		return fmt.Errorf("rate-limited completing partial pull requests: %w", err)
	}
	return nil
}

// completePartialPullRequest is reconcilePartialPullRequests' per-row body, run as its own
// errgroup goroutine. Returns non-nil only for a *githubapp.RateLimitError from FetchPullRequest
// (the signal that stops the batch, per reconcilePartialPullRequests' own doc comment); every
// other error is logged and swallowed here, since one row's own failure must not stop any other
// row's completion already in flight. A permanent 404/410 (ErrPullRequestNotFound) marks the row
// unfetchable instead of logging a transient-looking warning every pass forever.
func (r *Reconcile) completePartialPullRequest(ctx context.Context, pr DeliveryPullRequest) error {
	owner, repo, err := splitRepo(pr.Repo)
	if err != nil {
		slog.Warn("dispatch delivery: partial pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
		return nil
	}
	fetched, err := FetchPullRequest(ctx, r.github, owner, repo, pr.Number)
	if err != nil {
		var limited *githubapp.RateLimitError
		if errors.As(err, &limited) {
			slog.Warn("dispatch delivery: rate-limited completing partial pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
			return limited
		}
		if errors.Is(err, ErrPullRequestNotFound) {
			if markErr := MarkPullRequestUnfetchable(ctx, r.pool, pr.Repo, pr.Number, err.Error()); markErr != nil {
				slog.Warn("dispatch delivery: mark pull request unfetchable", "repo", pr.Repo, "number", pr.Number, "error", markErr)
			}
			return nil
		}
		slog.Warn("dispatch delivery: complete partial pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
		return nil
	}
	if fetched.MergedAt == nil {
		return nil
	}
	if err := completePullRequest(ctx, r.pool, r.github, owner, repo, pr.Repo, pr.Number, fetched); err != nil {
		slog.Warn("dispatch delivery: upsert completed pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
	}
	return nil
}

// reconcileWorkflow lists kind's workflow runs created in [since, until) and upserts each one
// plus (for a concluded run) its jobs. Returns a combined error naming every run reconcileRun
// failed on, rather than only logging it: a rate-limited or otherwise-failed per-run fetch (most
// often the jobs listing, since the runs listing above it already succeeded) must not let this
// pass report itself healthy -- runOnce's record() must see the failure so last_reconcile_at
// does not advance past a window this pass left incompletely fetched, and the next pass's
// smaller overlap re-reads it instead of the gap becoming permanent.
func (r *Reconcile) reconcileWorkflow(ctx context.Context, owner, repo, repoFull, workflowPath string, kind DeliveryRunKind, since, until time.Time) error {
	runs, err := ListWorkflowRuns(ctx, r.github, owner, repo, workflowPath, since, until)
	if err != nil {
		return fmt.Errorf("list %s workflow runs: %w", kind, err)
	}
	var failures []string
	for _, run := range runs {
		if err := r.reconcileRun(ctx, owner, repo, repoFull, kind, run); err != nil {
			slog.Warn("dispatch delivery: reconcile run", "repo", repoFull, "run_id", run.RunID, "error", err)
			failures = append(failures, fmt.Sprintf("run %d: %s", run.RunID, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d %s runs failed: %s", len(failures), len(runs), kind, strings.Join(failures, "; "))
	}
	return nil
}

// reconcileRun upserts one run -- GitHub's workflow-runs listing already carries the head
// commit's timestamp (head_commit.timestamp, read into FetchedRun.HeadCommitAt by
// github_runs.go's fetchedRunFromItem), unlike the live webhook envelope intake.go handles, which
// re-verifies the whole run against GitHub rather than trusting the envelope -- and, once it has
// concluded, its jobs.
func (r *Reconcile) reconcileRun(ctx context.Context, owner, repo, repoFull string, kind DeliveryRunKind, run FetchedRun) error {
	if err := UpsertRun(ctx, r.pool, DeliveryRun{
		Repo: repoFull, RunID: run.RunID, Kind: kind, PRNumber: run.PRNumber, HeadSHA: run.HeadSHA,
		HeadCommitAt: run.HeadCommitAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		Conclusion: mapRunConclusionPtr(run.Conclusion), URL: run.URL,
	}); err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	if run.CompletedAt == nil {
		return nil
	}
	fetchedJobs, err := ListWorkflowRunJobs(ctx, r.github, owner, repo, run.RunID)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	jobs := make([]DeliveryRunJob, len(fetchedJobs))
	for i, job := range fetchedJobs {
		jobs[i] = DeliveryRunJob{
			Repo: repoFull, RunID: run.RunID, Name: job.Name, StartedAt: job.StartedAt,
			CompletedAt: job.CompletedAt, Conclusion: mapJobConclusion(job.Conclusion),
		}
	}
	return UpsertRunJobs(ctx, r.pool, repoFull, run.RunID, jobs)
}

// mapRunConclusionPtr adapts mapRunConclusion (which intake.go calls with the envelope's raw
// string, always present) to FetchedRun.Conclusion's pointer (nil while a run is still in
// progress).
func mapRunConclusionPtr(raw *string) *DeliveryRunConclusion {
	if raw == nil {
		return nil
	}
	return mapRunConclusion(*raw)
}
