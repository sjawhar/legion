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
	"strconv"
	"strings"
	"sync"
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
// completing fetch that failed earlier. Each windowed step keeps its own progress
// (delivery_reconcile_progress), so a step that fails part way through resumes where it stopped
// rather than restarting its whole window: a 28-day backfill of a busy deploy repository is tens
// of thousands of GitHub requests, well past one installation's hourly rate limit, so no single
// pass can finish one and a pass that forgets what it did could never converge. last_reconcile_at
// advances ONLY when every step that ran succeeded; any failure (a missing permission, a rate
// limit, any other GitHub or store error) is recorded by name in last_error instead, and the
// previous last_reconcile_at is left untouched -- so the freshness row can never report a healthy
// timestamp for a pass that silently did nothing.
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

	searchScope := searchProgressScope(settings)

	var failures []string
	record := func(step string, err error) {
		if err == nil {
			return
		}
		slog.Error("dispatch delivery: "+step, "error", err)
		failures = append(failures, fmt.Sprintf("%s: %s", step, err))
	}

	record("reconcile merged pull requests", r.reconcileMergedPullRequests(ctx, settings, searchScope, now))
	record("reconcile partial pull requests", r.reconcilePartialPullRequests(ctx))
	record("reconcile deploy workflow runs", r.reconcileWorkflow(ctx, owner, repo, settings.DeployRepo, settings.DeployWorkflowPath, DeliveryRunKindDeploy, now))
	if settings.PRChecksWorkflowPath != settings.DeployWorkflowPath {
		record("reconcile PR-checks workflow runs", r.reconcileWorkflow(ctx, owner, repo, settings.DeployRepo, settings.PRChecksWorkflowPath, DeliveryRunKindPRChecks, now))
	}
	// The runs backfill logs and resumes its own failures and never reaches record(): a rate limit
	// it meets must not hold last_reconcile_at or set last_error while every regular step
	// succeeded. It walks at most backfillWindowsPerPass hours a pass.
	r.backfillRuns(ctx, owner, repo, settings.DeployRepo, settings.DeployWorkflowPath, DeliveryRunKindDeploy, now)
	if settings.PRChecksWorkflowPath != settings.DeployWorkflowPath {
		r.backfillRuns(ctx, owner, repo, settings.DeployRepo, settings.PRChecksWorkflowPath, DeliveryRunKindPRChecks, now)
	}
	// The attribution backfill runs last, within its own call budget: it is the one step whose
	// work can wait a pass, so the timeline's own facts take the rate limit first.
	record("read attribution inputs of stored pull requests", r.reconcileAttributionInputs(ctx))
	if changed, err := AttributePullRequests(ctx, r.pool, AttributionResolveBatch); err != nil {
		record("attribute stored pull requests", err)
	} else if changed > 0 {
		slog.Info("dispatch delivery: attributed stored pull requests", "changed", changed)
	}

	if len(failures) > 0 {
		r.fail(ctx, errors.New(strings.Join(failures, "; ")))
		return
	}
	if err := RecordReconcileSuccess(ctx, r.pool, now); err != nil {
		slog.Error("dispatch delivery: record reconcile success", "error", err)
	}
}

// maxLastErrorLength bounds what fail() writes to delivery_settings.last_error: a reconcile pass
// with many failing items (every rate-limited run in a reconcileWorkflow pass before Rev's
// probe-driven stop-at-first-rate-limit fix, 197 of 200 requests, each contributing its own
// error text) could otherwise grow this column without bound. Generous enough to still name
// several distinct failures, not just one.
const maxLastErrorLength = 4000

// fail records a reconcile pass's failure reason in delivery_settings.last_error without
// advancing last_reconcile_at (RecordReconcileError's own contract).
func (r *Reconcile) fail(ctx context.Context, cause error) {
	message := cause.Error()
	if len(message) > maxLastErrorLength {
		message = message[:maxLastErrorLength] + fmt.Sprintf(" ... (truncated from %d bytes)", len(message))
	}
	if err := RecordReconcileError(ctx, r.pool, message); err != nil {
		slog.Error("dispatch delivery: record reconcile error", "error", err)
	}
}

// searchProgressScope names what the merged-PR search's recorded progress was measured against:
// a change to the population, the exclusions or the deploy repository (which decides how a
// result is classified) makes a window imported under the old settings say nothing about the new
// ones, so the step backfills afresh rather than resuming it.
func searchProgressScope(settings DeliverySettings) string {
	return fmt.Sprintf("authors=%s excluded=%s deploy=%s",
		strings.Join(settings.PopulationAuthors, ","), strings.Join(settings.ExcludedRepos, ","), settings.DeployRepo)
}

// runsProgressScope names what a run listing's recorded progress was measured against.
func runsProgressScope(repoFull, workflowPath string) string {
	return repoFull + " " + workflowPath
}

// stepSince is where one step's window starts: the 28-day backfill when the step has no usable
// progress, else where it last imported through, minus reconcileOverlap so a merge or run
// GitHub's index was still catching up on at that boundary is re-read rather than permanently
// missed. Never earlier than the backfill window: a step whose progress is older than that (a
// server down for a month) catches up to the window the timeline actually shows rather than
// re-reading history nothing displays.
func (r *Reconcile) stepSince(ctx context.Context, step, scope string, now time.Time) (time.Time, error) {
	floor := now.Add(-BackfillWindow)
	through, err := ReconcileProgressThrough(ctx, r.pool, step, scope)
	if err != nil {
		return time.Time{}, fmt.Errorf("read %s progress: %w", step, err)
	}
	if through.IsZero() {
		return floor, nil
	}
	if since := through.Add(-reconcileOverlap); since.After(floor) {
		return since, nil
	}
	return floor, nil
}

// mergedPullRequestsStep names one installation's merged-PR search progress. Per installation
// rather than one step for the whole search: the installations are searched concurrently and
// fail independently, so one that cannot be searched must not make every other one redo what it
// already imported.
func mergedPullRequestsStep(installationID int64) string {
	return mergedPullRequestsStepPrefix + strconv.FormatInt(installationID, 10)
}

// mergedPullRequestsStepPrefix is what every installation's merged-PR step name starts with, and
// so the one place that names the family: PruneMergedPullRequestProgress selects the family with
// starts_with rather than LIKE, since the prefix's own underscores are LIKE wildcards.
const mergedPullRequestsStepPrefix = "merged_pull_requests/installation/"

// reconcileMergedPullRequests searches every population pull request merged since each
// installation's own recorded progress across every installation the App has (LEGION-294's
// population spans any repository the three authors merge into, not just the deploy
// repository), classifies each with the labels the search result already carries, and upserts
// the ones that belong -- complete, except for MergeCommitSHA, Additions, Deletions and
// FirstCommitAt, which the search response never carries.
//
// Each completed search window records that installation's progress, but only while every
// window before it in this pass succeeded: a pull request this pass could not classify (a
// deploy-repo PR GitHub's own classifier workflow has not labelled yet, IsTaskPR's "neither
// label") must be reachable again on a later pass, so progress stops at the window holding it
// while every later window is still imported. The search's own error and every per-PR
// classify/upsert failure are errors.Joined into one returned error -- not stringified into a
// plain errors.New, so a *githubapp.RateLimitError among them still answers errors.As for a
// caller that needs to tell it apart from an ordinary failure, the same reason
// reconcileWorkflow's own per-run aggregation keeps its typed error with %w.
func (r *Reconcile) reconcileMergedPullRequests(ctx context.Context, settings DeliverySettings, scope string, until time.Time) error {
	var mu sync.Mutex
	var failures []error
	blocked := map[int64]bool{}

	since := func(installationID int64) (time.Time, error) {
		return r.stepSince(ctx, mergedPullRequestsStep(installationID), scope, until)
	}

	// The listing this search already makes is the only answer to which installations still
	// exist, so the rows of the ones that no longer do are dropped here rather than by a sweep
	// of its own.
	listed := func(installations []githubapp.Installation) {
		keep := make([]int64, 0, len(installations))
		for _, installation := range installations {
			keep = append(keep, installation.ID)
		}
		if err := PruneMergedPullRequestProgress(ctx, r.pool, keep); err != nil {
			mu.Lock()
			failures = append(failures, fmt.Errorf("prune installation progress: %w", err))
			mu.Unlock()
		}
	}

	visit := func(installation githubapp.Installation, windowUntil time.Time, prs []FetchedPullRequest) error {
		var windowFailures []error
		for _, pr := range prs {
			if pr.MergedAt == nil {
				continue
			}
			repoFull, err := r.searchResultRepo(pr)
			if err != nil {
				slog.Warn("dispatch delivery: merged-PR search result", "number", pr.Number, "error", err)
				windowFailures = append(windowFailures, fmt.Errorf("pull request #%d: %w", pr.Number, err))
				continue
			}
			if err := r.reconcilePullRequest(ctx, settings, repoFull, pr); err != nil {
				slog.Warn("dispatch delivery: reconcile pull request", "repo", repoFull, "number", pr.Number, "error", err)
				windowFailures = append(windowFailures, fmt.Errorf("%s#%d: %w", repoFull, pr.Number, err))
			}
		}
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, windowFailures...)
		if len(windowFailures) > 0 {
			blocked[installation.ID] = true
		}
		if blocked[installation.ID] {
			return nil
		}
		if err := RecordReconcileProgress(ctx, r.pool, mergedPullRequestsStep(installation.ID), scope, windowUntil); err != nil {
			failures = append(failures, fmt.Errorf("record installation %d progress: %w", installation.ID, err))
			blocked[installation.ID] = true
		}
		return nil
	}

	if err := SearchMergedPullRequestsAcrossInstallation(ctx, r.github, settings.PopulationAuthors, since, until, listed, visit); err != nil {
		mu.Lock()
		failures = append(failures, fmt.Errorf("search merged pull requests: %w", err))
		mu.Unlock()
	}
	return errors.Join(failures...)
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

// boundedFanOut runs fn(ctx, item) for every item in items, up to concurrency goroutines at
// once, via errgroup.WithContext(ctx) + SetLimit: the first goroutine to return a non-nil error
// cancels the shared context, and the loop checks it before starting each further item, so
// nothing new launches once that happens. An item already in flight when that happens is not
// necessarily spared either: it shares the cancelled context, so its own in-flight HTTP request
// (there is no cheap way to cancel one already sent otherwise) can itself be aborted before
// GitHub ever answers it -- it does not reliably run to completion the way an item started
// before the cancellation and already past its own HTTP call does. fn decides for itself which
// errors are the batch's own stop signal (returned) versus one item's own problem (logged and
// swallowed inside fn, nil returned) -- this helper only runs the fan-out, never interprets fn's
// result. reconcilePartialPullRequests and SearchMergedPullRequestsAcrossInstallation's own
// searchOneInstallation loop each hand-rolled this identical
// errgroup.WithContext+SetLimit+groupCtx.Err()-before-each-Go shape separately before this
// helper existed.
func boundedFanOut[T any](ctx context.Context, concurrency int, items []T, fn func(ctx context.Context, item T) error) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(concurrency)
	for _, item := range items {
		if groupCtx.Err() != nil {
			break
		}
		item := item
		group.Go(func() error {
			return fn(groupCtx, item)
		})
	}
	return group.Wait()
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
// completing fields) with a single-pull-request fetch, via boundedFanOut
// (SetLimit(reconcilePartialConcurrency)) -- see boundedFanOut's own doc comment for the shared
// fan-out/cancellation mechanics. Each partial row is a distinct (repo, number) key, so
// concurrent upserts never race each other. completePartialPullRequest below returns non-nil
// only for a *githubapp.RateLimitError, which stops the batch and is boundedFanOut's own
// returned error here, so the pass is reported failed and last_reconcile_at does not advance
// past rows this pass never got to -- the next pass's overlap re-reads them rather than losing
// them for good. Any other per-row error (a 404, a deleted repository, a malformed answer) is
// still only logged and skipped inside completePartialPullRequest, never returned: it is that
// one row's own problem, not a reason to stop the rest of the batch.
func (r *Reconcile) reconcilePartialPullRequests(ctx context.Context) error {
	partials, err := ListPartialPullRequests(ctx, r.pool)
	if err != nil {
		return fmt.Errorf("list partial pull requests: %w", err)
	}
	if err := boundedFanOut(ctx, reconcilePartialConcurrency, partials, r.completePartialPullRequest); err != nil {
		return fmt.Errorf("rate-limited completing partial pull requests: %w", err)
	}
	return nil
}

// attributionBackfillCalls bounds the GitHub calls reconcileAttributionInputs makes in one pass. A
// row costs one call for the pull request and one per page of 100 commits
// (fetchAttributionFacts): two for nearly every row, at most maxAttributionCalls. At twelve passes
// an hour that is at most 1,440 calls an hour, under a third of an installation's 5,000, and the
// rest of each pass (the merged-PR search, partial rows, the workflow runs and their jobs) keeps
// the remainder. A 28-day population's ~4,000 rows then backfill over about 70 passes, six hours.
const attributionBackfillCalls = 120

// maxAttributionCalls is the most one row's attribution read can cost: the pull request and every
// commits page fetchCommitMessagesWithToken reads.
const maxAttributionCalls = 1 + maxCommitPages

// attributionBackfillRows is how many unread rows a pass lists for the backfill: as many as the
// call budget can read at two calls a row.
const attributionBackfillRows = attributionBackfillCalls / 2

// callBudget is one pass's allowance of GitHub calls, shared by concurrent reads. A read reserves
// the most it can cost before it starts and gives back what it did not use, waiting while other
// reads hold reservations, so the calls made never exceed the allowance however the reads
// interleave, and a read gives up only once the allowance is spent.
type callBudget struct {
	mu       sync.Mutex
	changed  *sync.Cond
	left     int
	reserved int
	spent    int
}

func newCallBudget(calls int) *callBudget {
	budget := &callBudget{left: calls}
	budget.changed = sync.NewCond(&budget.mu)
	return budget
}

// reserve takes n calls from the allowance, waiting for reads in flight to give theirs back, or
// answers false when the allowance cannot cover n.
func (b *callBudget) reserve(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.left < n && b.reserved > 0 {
		b.changed.Wait()
	}
	if b.left < n {
		return false
	}
	b.left -= n
	b.reserved += n
	return true
}

// settle records that a read which reserved `reserved` calls made `used`, and gives the rest back.
func (b *callBudget) settle(reserved, used int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.left += reserved - used
	b.reserved -= reserved
	b.spent += used
	b.changed.Broadcast()
}

// reconcileAttributionInputs reads, once each, the attribution inputs of complete rows stored
// before those inputs were (ListUnreadAttributionPullRequests, newest first), within
// attributionBackfillCalls, and stores them with the row's issue resolved from them. A rate limit
// stops the batch and fails the pass, as for partial rows; a row the budget does not reach waits
// for a later pass.
func (r *Reconcile) reconcileAttributionInputs(ctx context.Context) error {
	unread, err := ListUnreadAttributionPullRequests(ctx, r.pool, attributionBackfillRows)
	if err != nil {
		return fmt.Errorf("list pull requests with unread attribution inputs: %w", err)
	}
	budget := newCallBudget(attributionBackfillCalls)
	err = boundedFanOut(ctx, reconcilePartialConcurrency, unread, func(ctx context.Context, pr DeliveryPullRequest) error {
		return r.readAttributionInputs(ctx, pr, budget)
	})
	if len(unread) > 0 {
		slog.Info("dispatch delivery: read attribution inputs", "listed", len(unread), "calls", budget.spent)
	}
	if err != nil {
		return fmt.Errorf("rate-limited reading attribution inputs: %w", err)
	}
	return nil
}

// readAttributionInputs is reconcileAttributionInputs' per-row body, run as its own boundedFanOut
// goroutine: it reads only what the attribution needs (fetchAttributionFacts), since the row
// already holds every other fact, and only when budget still covers the most that read can cost.
// Like completePartialPullRequest, it returns non-nil only for a *githubapp.RateLimitError, marks
// a row GitHub no longer has unfetchable, and logs and skips any other failure, leaving the row
// unread for a later pass.
func (r *Reconcile) readAttributionInputs(ctx context.Context, pr DeliveryPullRequest, budget *callBudget) error {
	owner, repo, err := splitRepo(pr.Repo)
	if err != nil {
		slog.Warn("dispatch delivery: read attribution inputs", "repo", pr.Repo, "number", pr.Number, "error", err)
		return nil
	}
	if !budget.reserve(maxAttributionCalls) {
		return nil
	}
	facts, calls, err := fetchAttributionFacts(ctx, r.github, owner, repo, pr.Repo, pr.Number)
	budget.settle(maxAttributionCalls, calls)
	if err != nil {
		if limited, ok := githubapp.AsRateLimit(err); ok {
			slog.Warn("dispatch delivery: stopped reading attribution inputs: rate limit", "repo", pr.Repo, "number", pr.Number, "error", err)
			return limited
		}
		if errors.Is(err, ErrPullRequestNotFound) || errors.Is(err, githubapp.ErrNoInstallation) {
			if markErr := MarkPullRequestUnfetchable(ctx, r.pool, pr.Repo, pr.Number, err.Error()); markErr != nil {
				slog.Warn("dispatch delivery: mark pull request unfetchable", "repo", pr.Repo, "number", pr.Number, "error", markErr)
			}
			return nil
		}
		slog.Warn("dispatch delivery: read attribution inputs", "repo", pr.Repo, "number", pr.Number, "error", err)
		return nil
	}
	inputs := attributionInputsFrom(facts)
	issueKey, _, err := resolveStoredIssueKey(ctx, r.pool, pr.URL, inputs)
	if err != nil {
		slog.Warn("dispatch delivery: resolve the issue of a backfilled pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
		return nil
	}
	if err := StoreAttributionInputs(ctx, r.pool, pr.Repo, pr.Number, inputs, sessionTrailers(facts.CommitMessages), issueKey); err != nil {
		slog.Warn("dispatch delivery: store attribution inputs", "repo", pr.Repo, "number", pr.Number, "error", err)
	}
	return nil
}

// completePartialPullRequest is reconcilePartialPullRequests' per-row body, run as its own
// boundedFanOut goroutine. Returns non-nil only for a *githubapp.RateLimitError (the signal that
// stops the batch, per reconcilePartialPullRequests' own doc comment, from either FetchPullRequest
// or completePullRequest's own session-trailer fetch); every other error is logged and swallowed
// here, since one row's own failure must not stop any other row's completion already in flight.
// A permanent 404/410 (ErrPullRequestNotFound) or a 404 resolving which installation covers the
// repository (githubapp.ErrNoInstallation -- the repository itself was deleted or renamed, which
// its own doc comment already names as exactly this case) both mark the row unfetchable instead
// of logging a transient-looking warning every pass forever.
func (r *Reconcile) completePartialPullRequest(ctx context.Context, pr DeliveryPullRequest) error {
	owner, repo, err := splitRepo(pr.Repo)
	if err != nil {
		slog.Warn("dispatch delivery: partial pull request", "repo", pr.Repo, "number", pr.Number, "error", err)
		return nil
	}
	fetched, err := FetchPullRequest(ctx, r.github, owner, repo, pr.Number)
	if err != nil {
		if limited, ok := githubapp.AsRateLimit(err); ok {
			slog.Warn("dispatch delivery: stopped completing partial pull request: rate limit", "repo", pr.Repo, "number", pr.Number, "error", err)
			return limited
		}
		if errors.Is(err, ErrPullRequestNotFound) || errors.Is(err, githubapp.ErrNoInstallation) {
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
		if limited, ok := githubapp.AsRateLimit(err); ok {
			slog.Warn("dispatch delivery: stopped completing partial pull requests: rate limit", "repo", pr.Repo, "number", pr.Number, "error", err)
			return limited
		}
		slog.Warn("dispatch delivery: complete partial pull request; it stays partial for the next pass", "repo", pr.Repo, "number", pr.Number, "error", err)
	}
	return nil
}

// runsWindow bounds how many runs one window of a run listing hands to reconcileWorkflow's
// visitor, and so how much work a failed pass redoes. Unlike a search window, every completed
// run in it costs its own jobs request, so 100 runs is about ten listing pages plus a hundred
// jobs requests -- a couple of minutes -- where GitHub's own 1,000-result cap would be most of
// an hour and a rate limit part way through would throw all of it away.
const runsWindow = 100

// runsStep names one workflow kind's run-listing progress.
func runsStep(kind DeliveryRunKind) string {
	return "runs/" + string(kind)
}

// completedRunIDs is the ids of runs that have concluded: the only runs whose jobs a pass lists,
// so the only ones its skip-set queries need to ask about.
func completedRunIDs(runs []FetchedRun) []int64 {
	ids := make([]int64, 0, len(runs))
	for _, run := range runs {
		if run.CompletedAt != nil {
			ids = append(ids, run.RunID)
		}
	}
	return ids
}

// reconcileWorkflow walks kind's workflow runs created since this step's own recorded progress,
// in windows, and upserts each run plus (for a concluded run) its jobs. Each completed window
// records the step's progress, but only while every window before it in this pass succeeded: a
// run whose jobs this pass could not fetch must be reachable again on a later pass, so progress
// stops at the window holding it while every later window is still imported.
//
// Returns a combined error naming every run reconcileRun failed on, rather than only logging
// it: a rate-limited or otherwise-failed per-run fetch (most often the jobs listing, since the
// runs listing above it already succeeded) must not let this pass report itself healthy.
// Reads which of a window's completed runs already carry a jobs-unfetchable mark in one bulk
// query (ListUnfetchableRunIDs) per window, instead of reconcileRun running its own
// RunJobsUnfetchable point query once per completed run on every pass -- the overwhelming
// majority of runs were never marked, and this package already bulk-fetches this way one
// function away (ListPartialPullRequests feeding reconcilePartialPullRequests).
func (r *Reconcile) reconcileWorkflow(ctx context.Context, owner, repo, repoFull, workflowPath string, kind DeliveryRunKind, until time.Time) error {
	step, scope := runsStep(kind), runsProgressScope(repoFull, workflowPath)
	since, err := r.stepSince(ctx, step, scope, until)
	if err != nil {
		return err
	}

	var failures []string
	attempted := 0
	blocked := false

	visit := func(windowUntil time.Time, runs []FetchedRun) error {
		jobsUnfetchable, err := ListUnfetchableRunIDs(ctx, r.pool, repoFull, completedRunIDs(runs))
		if err != nil {
			blocked = true
			return fmt.Errorf("list unfetchable %s run jobs: %w", kind, err)
		}
		windowFailed := false
		for _, run := range runs {
			attempted++
			err := r.reconcileRun(ctx, owner, repo, repoFull, kind, run, jobsUnfetchable)
			if err == nil {
				continue
			}
			slog.Warn("dispatch delivery: reconcile run", "repo", repoFull, "run_id", run.RunID, "error", err)
			failures = append(failures, fmt.Sprintf("run %d: %s", run.RunID, err))
			windowFailed = true
			// A rate limit is a global condition on this installation token's budget, not one
			// run's own problem: Rev's probe showed the un-fixed loop sending 197 of 200
			// requests after the first rate-limited one, each adding its own failure text to
			// last_error (57 KB by the end). Stop the whole walk here instead, preserving the
			// typed error so a caller checking errors.As for it still can, unlike every other
			// per-run failure joined into one string.
			if limited, ok := githubapp.AsRateLimit(err); ok {
				blocked = true
				return fmt.Errorf("%d of %d %s runs attempted before a rate limit stopped the rest: %s: %w",
					len(failures), attempted, kind, strings.Join(failures, "; "), limited)
			}
		}
		if windowFailed {
			blocked = true
		}
		if blocked {
			return nil
		}
		if err := RecordReconcileProgress(ctx, r.pool, step, scope, windowUntil); err != nil {
			blocked = true
			failures = append(failures, fmt.Sprintf("record %s progress: %s", kind, err))
		}
		return nil
	}

	if listErr := ListWorkflowRuns(ctx, r.github, owner, repo, workflowPath, since, until, runsWindow, visit); listErr != nil {
		return fmt.Errorf("list %s workflow runs: %w", kind, listErr)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d %s runs failed: %s", len(failures), attempted, kind, strings.Join(failures, "; "))
	}
	return nil
}

// reconcileRun upserts one run -- GitHub's workflow-runs listing already carries the head
// commit's timestamp (head_commit.timestamp, read into FetchedRun.HeadCommitAt by
// github_runs.go's fetchedRunFromItem), unlike the live webhook envelope intake.go handles, which
// re-verifies the whole run against GitHub rather than trusting the envelope -- and, once it has
// concluded, its jobs. A run whose jobs listing already carries a jobs-unfetchable mark
// (jobsUnfetchable, reconcileWorkflow's own bulk ListUnfetchableRunIDs read, from an earlier
// pass's permanent 404) is skipped rather than re-fetched: the same treatment
// completePartialPullRequest gives an already-unfetchable pull request.
func (r *Reconcile) reconcileRun(ctx context.Context, owner, repo, repoFull string, kind DeliveryRunKind, run FetchedRun, jobsUnfetchable map[int64]bool) error {
	if err := UpsertRun(ctx, r.pool, DeliveryRun{
		Repo: repoFull, RunID: run.RunID, Kind: kind, PRNumber: run.PRNumber, HeadSHA: run.HeadSHA,
		HeadCommitAt: run.HeadCommitAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		Conclusion: mapRunConclusionPtr(run.Conclusion), URL: run.URL,
		HeadBranch: run.HeadBranch, Event: run.Event,
	}); err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	if run.CompletedAt == nil {
		return nil
	}
	if jobsUnfetchable[run.RunID] {
		return nil
	}
	fetchedJobs, err := ListWorkflowRunJobs(ctx, r.github, owner, repo, run.RunID)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) || errors.Is(err, githubapp.ErrNoInstallation) {
			if markErr := MarkRunJobsUnfetchable(ctx, r.pool, repoFull, run.RunID, err.Error()); markErr != nil {
				slog.Warn("dispatch delivery: mark run jobs unfetchable", "repo", repoFull, "run_id", run.RunID, "error", markErr)
			}
			return nil
		}
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
