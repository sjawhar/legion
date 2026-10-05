// Reconcile (LEGION-567): the five-minute GitHub-App pass that backfills 28 days on first run and
// catches whatever intake's NATS consumer missed, through the same upserts intake uses -- so
// "reconcile catches a missed event" is provable as "reconcile and intake write through the same
// store functions", never two implementations that can drift. Mirrors
// architecture.Run/Importer's shape (ticker, per-pass idempotent work, idle without GitHub App
// credentials).
package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	if !r.HasApp() {
		slog.Info("dispatch delivery: no GitHub App key — the reconcile is idle")
		return
	}
	// #nosec G404 — jitter, not a secret.
	start := time.Duration(rand.Int64N(int64(ReconcileInterval)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(start):
	}
	for {
		r.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(ReconcileInterval):
		}
	}
}

// runOnce performs one reconcile pass: settings, the population pull-request search, the deploy
// and PR-checks workflows' runs and jobs, and any partial row left over from a completing fetch
// that failed earlier -- always recording last_reconcile_at at the end, successful or not, since
// a failed pass still proves the attempt the freshness row promises.
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
		return
	}

	since := r.windowStart(settings, now)

	if err := r.reconcileMergedPullRequests(ctx, settings, owner, repo, since, now); err != nil {
		slog.Error("dispatch delivery: reconcile merged pull requests", "error", err)
	}
	if err := r.reconcilePartialPullRequests(ctx); err != nil {
		slog.Error("dispatch delivery: reconcile partial pull requests", "error", err)
	}
	if err := r.reconcileWorkflow(ctx, owner, repo, settings.DeployRepo, settings.DeployWorkflowPath, model.DeliveryRunKindDeploy, settings.ProductionJobName, since, now); err != nil {
		slog.Error("dispatch delivery: reconcile deploy workflow runs", "error", err)
	}
	if settings.PRChecksWorkflowPath != settings.DeployWorkflowPath {
		if err := r.reconcileWorkflow(ctx, owner, repo, settings.DeployRepo, settings.PRChecksWorkflowPath, model.DeliveryRunKindPRChecks, "", since, now); err != nil {
			slog.Error("dispatch delivery: reconcile PR-checks workflow runs", "error", err)
		}
	}

	if err := RecordReconcileAt(ctx, r.pool, now); err != nil {
		slog.Error("dispatch delivery: record reconcile freshness", "error", err)
	}
}

// windowStart is the 28-day backfill on the first pass (no recorded last_reconcile_at), else the
// last pass's own time minus reconcileOverlap.
func (r *Reconcile) windowStart(settings model.DeliverySettings, now time.Time) time.Time {
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
func (r *Reconcile) reconcileMergedPullRequests(ctx context.Context, settings model.DeliverySettings, tokenOwner, tokenRepo string, since, until time.Time) error {
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
	repoName, _, found := cutFirst(repoTail, "/")
	if !found {
		return "", fmt.Errorf("pull request URL %q: no /pull/ segment", pr.URL)
	}
	return owner + "/" + repoName, nil
}

// cutFirst is strings.Cut, spelled out so this file needs no extra import for one call site.
func cutFirst(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// reconcilePullRequest classifies and upserts one merged pull request the search found, the same
// decision handlePullRequestEnvelope makes: population membership from author (already true,
// since the search itself filtered on it)/repository/task-label, then the row. A deploy-repo PR
// the label rule cannot classify yet (IsTaskPR's "neither label" error) is logged and left for a
// later pass, exactly as intake does.
func (r *Reconcile) reconcilePullRequest(ctx context.Context, settings model.DeliverySettings, repoFull string, pr FetchedPullRequest) error {
	raw := RawPullRequest{Repo: repoFull, Number: pr.Number, Title: pr.Title, Author: pr.Author, Labels: pr.Labels, MergedAt: *pr.MergedAt}
	isPopulation, err := IsPopulationPR(raw, settings, TimeWindow{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)})
	if err != nil {
		return fmt.Errorf("classify: %w", err)
	}
	if !isPopulation {
		return DeletePullRequest(ctx, r.pool, repoFull, pr.Number)
	}
	return UpsertPullRequest(ctx, r.pool, model.DeliveryPullRequest{
		Repo: repoFull, Number: pr.Number, Title: pr.Title, URL: pr.URL, Author: pr.Author,
		CreatedAt: &pr.CreatedAt, MergedAt: pr.MergedAt, Rework: IsRework(pr.Title),
		Partial: true, // the search response never carries MergeCommitSHA/Additions/Deletions/FirstCommitAt
	})
}

// reconcilePartialPullRequests completes every stored partial row (written by intake when its
// completing fetch failed, or by this reconcile's own merged-PR search, which never carries the
// completing fields) with a single-pull-request fetch.
func (r *Reconcile) reconcilePartialPullRequests(ctx context.Context) error {
	partials, err := ListPartialPullRequests(ctx, r.pool)
	if err != nil {
		return fmt.Errorf("list partial pull requests: %w", err)
	}
	for _, pr := range partials {
		owner, repo, err := splitRepo(pr.Repo)
		if err != nil {
			slog.Warn("dispatch delivery: partial pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
			continue
		}
		fetched, err := FetchPullRequest(ctx, r.github, owner, repo, pr.Number)
		if err != nil {
			slog.Warn("dispatch delivery: complete partial pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
			continue
		}
		if fetched.MergedAt == nil {
			continue
		}
		issueKey := r.resolveIssueKey(ctx, fetched.Title, fetched.Body)
		if err := UpsertPullRequest(ctx, r.pool, model.DeliveryPullRequest{
			Repo: pr.Repo, Number: pr.Number, Title: fetched.Title, URL: fetched.URL, Author: fetched.Author,
			CreatedAt: &fetched.CreatedAt, MergedAt: fetched.MergedAt, FirstCommitAt: fetched.FirstCommitAt,
			MergeCommitSHA: fetched.MergeCommitSHA, Additions: fetched.Additions, Deletions: fetched.Deletions,
			Rework: IsRework(fetched.Title), IssueKey: issueKey, Sessions: pr.Sessions, Partial: false,
		}); err != nil {
			slog.Warn("dispatch delivery: upsert completed pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
		}
	}
	return nil
}

// resolveIssueKey mirrors Intake.resolveIssueKey (duplicated rather than shared through a method
// on a common embedded type, since the two owning structs are otherwise unrelated and the
// function is three lines of pure logic over the same package-level regex and pool).
func (r *Reconcile) resolveIssueKey(ctx context.Context, title, body string) *string {
	for _, candidate := range issueKeyCandidate.FindAllString(title+"\n"+body, -1) {
		var key string
		if err := r.pool.QueryRow(ctx, "select key from issues where key = $1", candidate).Scan(&key); err == nil {
			return &key
		}
	}
	return nil
}

// reconcileWorkflow lists kind's workflow runs created in [since, until) and upserts each one
// plus (for a concluded run) its jobs. productionJobName is read from delivery_settings only to
// log which job failed to match on a deploy run that never shipped anything; it does not
// otherwise affect what reconcile stores (containment.go, not reconcile, is what reads it to
// compute deploys at read time).
func (r *Reconcile) reconcileWorkflow(ctx context.Context, owner, repo, repoFull, workflowPath string, kind model.DeliveryRunKind, productionJobName string, since, until time.Time) error {
	runs, err := ListWorkflowRuns(ctx, r.github, owner, repo, workflowPath, since, until)
	if err != nil {
		return fmt.Errorf("list %s workflow runs: %w", kind, err)
	}
	for _, run := range runs {
		if err := r.reconcileRun(ctx, owner, repo, repoFull, kind, run); err != nil {
			slog.Warn("dispatch delivery: reconcile run", "repo", repoFull, "run_id", run.RunID, "error", err)
		}
	}
	return nil
}

// reconcileRun upserts one run -- GitHub's workflow-runs listing already carries the head
// commit's timestamp (head_commit.timestamp, read into FetchedRun.HeadCommitAt by
// github_runs.go's fetchedRunFromItem), unlike the live webhook envelope intake.go handles, which
// carries only the head SHA and needs a separate commit fetch -- and, once it has concluded, its
// jobs.
func (r *Reconcile) reconcileRun(ctx context.Context, owner, repo, repoFull string, kind model.DeliveryRunKind, run FetchedRun) error {
	if err := UpsertRun(ctx, r.pool, model.DeliveryRun{
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
	jobs := make([]model.DeliveryRunJob, len(fetchedJobs))
	for i, job := range fetchedJobs {
		jobs[i] = model.DeliveryRunJob{
			Repo: repoFull, RunID: run.RunID, Name: job.Name, StartedAt: job.StartedAt,
			CompletedAt: job.CompletedAt, Conclusion: mapJobConclusion(job.Conclusion),
		}
	}
	return UpsertRunJobs(ctx, r.pool, repoFull, run.RunID, jobs)
}

// mapRunConclusionPtr adapts mapRunConclusion (which intake.go calls with the envelope's raw
// string, always present) to FetchedRun.Conclusion's pointer (nil while a run is still in
// progress).
func mapRunConclusionPtr(raw *string) *model.DeliveryRunConclusion {
	if raw == nil {
		return nil
	}
	return mapRunConclusion(*raw)
}
