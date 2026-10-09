// Package delivery is LEGION-567 slice 1: the delivery timeline's stored facts (population pull
// requests, deploy-workflow and PR-checks workflow runs with their jobs, and the one
// delivery_settings record), their intake from the GitHub events Envoy relays over NATS, and the
// five-minute GitHub-App reconcile that backfills and catches a missed event. Population,
// containment and failure-attribution are pure functions in population.go, containment.go and
// failures.go, unit-tested without Postgres.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SettingsColumns is the delivery_settings select list ScanSettings reads, in scan order; every
// query that returns the settings row selects exactly this.
const SettingsColumns = `deploy_repo, deploy_workflow_path, production_job_name, pr_checks_workflow_path, population_authors, excluded_repos, last_event_at, last_reconcile_at, last_error, updated_by, updated_at`

// ScanSettings decodes one SettingsColumns row into a DeliverySettings.
func ScanSettings(row pgx.Row) (DeliverySettings, error) {
	var settings DeliverySettings
	var updatedBy []byte
	if err := row.Scan(
		&settings.DeployRepo, &settings.DeployWorkflowPath, &settings.ProductionJobName,
		&settings.PRChecksWorkflowPath, &settings.PopulationAuthors, &settings.ExcludedRepos,
		&settings.LastEventAt, &settings.LastReconcileAt, &settings.LastError, &updatedBy, &settings.UpdatedAt,
	); err != nil {
		return DeliverySettings{}, err
	}
	if err := json.Unmarshal(updatedBy, &settings.UpdatedBy); err != nil {
		return DeliverySettings{}, fmt.Errorf("decode delivery settings author: %w", err)
	}
	return settings, nil
}

// ErrNoSettings is "delivery_settings has no row yet": intake and reconcile stay idle, and the
// API answers that delivery is not configured, rather than guessing a repository.
var ErrNoSettings = errors.New("delivery settings are not configured")

// GetSettings reads the one delivery_settings row, or ErrNoSettings when none has been written
// yet.
func GetSettings(ctx context.Context, pool *store.Pool) (DeliverySettings, error) {
	settings, err := ScanSettings(pool.QueryRow(ctx, `select `+SettingsColumns+` from delivery_settings where singleton`))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeliverySettings{}, ErrNoSettings
		}
		return DeliverySettings{}, err
	}
	return settings, nil
}

// PutSettings upserts the one delivery_settings row, clearing the freshness columns and every
// delivery_reconcile_progress row: a settings change (a different repository, a different
// population) makes the previous freshness and every step's recorded progress meaningless, and
// the next reconcile sets both again from scratch. The delete rides the same statement as the
// upsert (a data-modifying CTE, which Postgres runs exactly once whether or not the outer query
// reads its output) so no pass can ever see the new settings beside the old settings' progress.
// Takes the pool directly, not a
// transaction: putDeliverySettings (api/settings_delivery.go) deliberately does not wrap this in
// a transaction+event+publish the way sibling settings routes do, since delivery_settings is a
// true singleton with no project/issue/artifact to own an event against (see that handler's own
// doc comment and events/broker.go's eventOwnerKey).
func PutSettings(ctx context.Context, pool *store.Pool, settings DeliverySettings, actor model.Actor) (DeliverySettings, error) {
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return DeliverySettings{}, fmt.Errorf("encode delivery settings author: %w", err)
	}
	// pgx sends a nil slice as SQL NULL, not the column default: an explicit insert value always
	// wins over the default, so a nil slice here would violate the not-null constraint instead
	// of quietly becoming '{}'.
	populationAuthors, excludedRepos := settings.PopulationAuthors, settings.ExcludedRepos
	if populationAuthors == nil {
		populationAuthors = []string{}
	}
	if excludedRepos == nil {
		excludedRepos = []string{}
	}
	return ScanSettings(pool.QueryRow(ctx, `
		with cleared as (delete from delivery_reconcile_progress)
		insert into delivery_settings (
			singleton, deploy_repo, deploy_workflow_path, production_job_name,
			pr_checks_workflow_path, population_authors, excluded_repos, updated_by
		)
		values (true, $1, $2, $3, $4, $5, $6, $7)
		on conflict (singleton) do update set
			deploy_repo = excluded.deploy_repo,
			deploy_workflow_path = excluded.deploy_workflow_path,
			production_job_name = excluded.production_job_name,
			pr_checks_workflow_path = excluded.pr_checks_workflow_path,
			population_authors = excluded.population_authors,
			excluded_repos = excluded.excluded_repos,
			last_event_at = null,
			last_reconcile_at = null,
			last_error = null,
			updated_by = excluded.updated_by,
			updated_at = now()
		returning `+SettingsColumns,
		settings.DeployRepo, settings.DeployWorkflowPath, settings.ProductionJobName,
		settings.PRChecksWorkflowPath, populationAuthors, excludedRepos, actorJSON,
	))
}

// RecordEventAt advances delivery_settings.last_event_at to t: called after intake processes a
// GitHub envelope. Never moves it backward, so an out-of-order redelivery cannot make the
// freshness row look staler than it is.
func RecordEventAt(ctx context.Context, pool *store.Pool, t time.Time) error {
	_, err := pool.Exec(ctx, `
		update delivery_settings set last_event_at = greatest(coalesce(last_event_at, $1), $1)
		where singleton
	`, t)
	return err
}

// RecordReconcileSuccess sets delivery_settings.last_reconcile_at to t and clears last_error:
// called only once a reconcile pass's every GitHub-touching step has actually succeeded.
// last_reconcile_at deliberately does NOT advance on a failed pass (see RecordReconcileError) --
// unlike architecture_sources' last_sync_at, which advances on every attempt and tracks failure
// separately, this freshness row's whole purpose is "when did this last actually refresh data",
// so a pass that touched nothing successfully must not look like one that did.
func RecordReconcileSuccess(ctx context.Context, pool *store.Pool, t time.Time) error {
	_, err := pool.Exec(ctx, `update delivery_settings set last_reconcile_at = $1, last_error = null where singleton`, t)
	return err
}

// RecordReconcileError records a failed reconcile pass's reason in delivery_settings.last_error,
// without advancing last_reconcile_at: a missing GitHub App permission, a rate limit, or any
// other failure is named here so the freshness row (and the API) can show it, rather than the
// pass silently reporting itself healthy by advancing the timestamp anyway.
func RecordReconcileError(ctx context.Context, pool *store.Pool, message string) error {
	_, err := pool.Exec(ctx, `update delivery_settings set last_error = $1 where singleton`, message)
	return err
}

// ReconcileProgressThrough reads one step's recorded progress under scope, or a zero time when
// the step has none (no row, or a row recorded against another scope).
func ReconcileProgressThrough(ctx context.Context, pool *store.Pool, step, scope string) (time.Time, error) {
	var through time.Time
	err := pool.QueryRow(ctx, `select through from delivery_reconcile_progress where step = $1 and scope = $2`, step, scope).Scan(&through)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return through, nil
}

// RecordReconcileProgress records that step has imported everything up to through, under scope.
// Within one scope progress only ever moves forward (greatest): a resumed pass re-reads its
// overlap, so its first window can end before the progress already recorded, and that must not
// move the step backward. A row recorded under another scope is replaced outright, since that
// scope's window says nothing about this one's.
func RecordReconcileProgress(ctx context.Context, pool *store.Pool, step, scope string, through time.Time) error {
	_, err := pool.Exec(ctx, `
		insert into delivery_reconcile_progress (step, scope, through)
		values ($1, $2, $3)
		on conflict (step) do update set
			scope = excluded.scope,
			through = case
				when delivery_reconcile_progress.scope = excluded.scope
					then greatest(delivery_reconcile_progress.through, excluded.through)
				else excluded.through
			end,
			updated_at = now()
	`, step, scope, through)
	return err
}

// PruneMergedPullRequestProgress deletes the merged-PR search progress of every installation
// outside keep -- an App installation that was removed, or one whose repositories moved to
// another. Its row would otherwise sit in delivery_reconcile_progress for good: nothing else
// deletes a step's row, and the step name carries an installation id no listing answers any
// more. keep empty deletes every installation's row, which is what an App with no installations
// means.
func PruneMergedPullRequestProgress(ctx context.Context, pool *store.Pool, keep []int64) error {
	steps := make([]string, 0, len(keep))
	for _, installationID := range keep {
		steps = append(steps, mergedPullRequestsStep(installationID))
	}
	_, err := pool.Exec(ctx, `
		delete from delivery_reconcile_progress
		where starts_with(step, $1) and not (step = any($2))
	`, mergedPullRequestsStepPrefix, steps)
	return err
}

// PullRequestColumns is the delivery_pull_requests select list ScanPullRequest reads, in scan
// order.
const PullRequestColumns = `repo, number, title, url, author, created_at, merged_at, first_commit_at, merge_commit_sha, additions, deletions, rework, issue_key, sessions, partial, unfetchable_at, unfetchable_reason, updated_at, attribution_title_keys, attribution_cited_issues, attribution_branch_keys, attribution_commit_keys`

// ScanPullRequest decodes one PullRequestColumns row into a DeliveryPullRequest.
func ScanPullRequest(row pgx.Row) (DeliveryPullRequest, error) {
	var pr DeliveryPullRequest
	var inputs AttributionInputs
	if err := row.Scan(
		&pr.Repo, &pr.Number, &pr.Title, &pr.URL, &pr.Author, &pr.CreatedAt, &pr.MergedAt,
		&pr.FirstCommitAt, &pr.MergeCommitSHA, &pr.Additions, &pr.Deletions, &pr.Rework,
		&pr.IssueKey, &pr.Sessions, &pr.Partial, &pr.UnfetchableAt, &pr.UnfetchableReason, &pr.UpdatedAt,
		&inputs.TitleKeys, &inputs.CitedIssues, &inputs.BranchKeys, &inputs.CommitKeys,
	); err != nil {
		return DeliveryPullRequest{}, err
	}
	if inputs.TitleKeys != nil {
		pr.Attribution = &inputs
	}
	return pr, nil
}

// UpsertPullRequest writes pr, keyed on (repo, number). Every field pr carries overwrites the
// stored row's EXCEPT when doing so would regress a complete row (Partial: false, real attribution
// already resolved) to a partial one: the WHERE clause on the UPDATE refuses that specific case,
// leaving the existing complete row untouched instead. This matters because reconcile's merged-PR
// search re-finds every PR merged in roughly the last hour on every five-minute pass (its own
// overlap window), including ones intake's live NATS path already completed moments earlier with
// real session/issue attribution the search result can't carry -- without this guard, reconcile
// would silently wipe that attribution back to empty within one pass of every merge, deterministic
// and immediate, not a race. A live intake write that knows only the envelope's fields passes nil
// for the rest and Partial true the first time a PR is seen; a later completing fetch or reconcile
// pass overwrites that same row with the complete fields and Partial false, which the WHERE clause
// always allows (excluded.partial = false never regresses anything). The one shared upsert both
// paths call, so "the reconcile catches a missed event" and "the completing fetch completes a
// partial row" are the same code path, never two implementations that can drift. Every call also
// clears unfetchable_at/unfetchable_reason to null: a successful write is itself the "it is
// fetchable after all" signal, whichever path (a webhook retry, a later reconcile pass) made it.
func UpsertPullRequest(ctx context.Context, pool *store.Pool, pr DeliveryPullRequest) error {
	sessions := pr.Sessions
	if sessions == nil {
		// pgx sends a nil slice as SQL NULL, not the column default: an explicit insert value
		// always wins over the default, so a nil Sessions here would violate the not-null
		// constraint instead of quietly becoming '{}'.
		sessions = []string{}
	}
	// A write that carries no attribution inputs (a search result, a partial envelope row) keeps
	// the ones stored: they are what GitHub said about the merged pull request, which does not
	// change. A write that carries them stores all four, never a nil slice (SQL NULL).
	var inputs [4][]string
	if a := pr.Attribution; a != nil {
		inputs = [4][]string{nonNil(a.TitleKeys), nonNil(a.CitedIssues), nonNil(a.BranchKeys), nonNil(a.CommitKeys)}
	}
	_, err := pool.Exec(ctx, `
		insert into delivery_pull_requests (
			repo, number, title, url, author, created_at, merged_at, first_commit_at,
			merge_commit_sha, additions, deletions, rework, issue_key, sessions, partial,
			attribution_title_keys, attribution_cited_issues, attribution_branch_keys, attribution_commit_keys
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		on conflict (repo, number) do update set
			title = excluded.title,
			url = excluded.url,
			author = excluded.author,
			created_at = excluded.created_at,
			merged_at = excluded.merged_at,
			first_commit_at = excluded.first_commit_at,
			merge_commit_sha = excluded.merge_commit_sha,
			additions = excluded.additions,
			deletions = excluded.deletions,
			rework = excluded.rework,
			issue_key = excluded.issue_key,
			sessions = excluded.sessions,
			partial = excluded.partial,
			attribution_title_keys = coalesce(excluded.attribution_title_keys, delivery_pull_requests.attribution_title_keys),
			attribution_cited_issues = coalesce(excluded.attribution_cited_issues, delivery_pull_requests.attribution_cited_issues),
			attribution_branch_keys = coalesce(excluded.attribution_branch_keys, delivery_pull_requests.attribution_branch_keys),
			attribution_commit_keys = coalesce(excluded.attribution_commit_keys, delivery_pull_requests.attribution_commit_keys),
			unfetchable_at = null,
			unfetchable_reason = null,
			updated_at = now()
		where delivery_pull_requests.partial or not excluded.partial
	`,
		pr.Repo, pr.Number, pr.Title, pr.URL, pr.Author, pr.CreatedAt, pr.MergedAt, pr.FirstCommitAt,
		pr.MergeCommitSHA, pr.Additions, pr.Deletions, pr.Rework, pr.IssueKey, sessions, pr.Partial,
		inputs[0], inputs[1], inputs[2], inputs[3],
	)
	return err
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// DeletePullRequest removes a stored row: a provisional row written from an envelope alone
// (population author and repository already passed, but the task-label check needs the
// completing fetch's labels) turns out, once completed, to be a task PR after all. Deleting
// rather than updating it is correct here because a task PR is never in the population at
// all -- storing it with some "excluded" flag would be a second representation of "not
// population" alongside "the row does not exist", which is the one this schema already has.
func DeletePullRequest(ctx context.Context, pool *store.Pool, repo string, number int) error {
	_, err := pool.Exec(ctx, `delete from delivery_pull_requests where repo = $1 and number = $2`, repo, number)
	return err
}

// MarkPullRequestUnfetchable records that repo#number's completing fetch answered a permanent
// 404 or 410 (FetchPullRequest's ErrPullRequestNotFound) or a 404 resolving which installation
// covers the repository (githubapp.ErrNoInstallation): the pull request or its repository no
// longer exists, or no longer reaches this token. ListPartialPullRequests and
// ListUnreadAttributionPullRequests stop returning the row until a later successful
// UpsertPullRequest clears it (a webhook retry, or a reconcile pass once the repository or PR
// becomes reachable again). Conditioned on the row still needing that fetch (partial, or its
// attribution inputs unread): a concurrent write (a live webhook, or another goroutine of this
// same reconcile pass racing a retried fetch) can complete the row between this caller's own
// failed fetch and this UPDATE running, and marking it unfetchable after that would silently
// throw the completion away.
func MarkPullRequestUnfetchable(ctx context.Context, pool *store.Pool, repo string, number int, reason string) error {
	_, err := pool.Exec(ctx, `
		update delivery_pull_requests
		set unfetchable_at = now(), unfetchable_reason = $3
		where repo = $1 and number = $2 and (partial or attribution_title_keys is null)
	`, repo, number, reason)
	return err
}

// CountUnfetchablePullRequests counts every row MarkPullRequestUnfetchable has marked, for the
// freshness row's "N pull requests can no longer be fetched from GitHub".
func CountUnfetchablePullRequests(ctx context.Context, pool *store.Pool) (int, error) {
	var count int
	err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests where unfetchable_at is not null`).Scan(&count)
	return count, err
}

// ListPartialPullRequests lists every partial=true row, oldest merge first: the reconcile's cheap
// way to find rows a completing fetch never finished, using delivery_pull_requests_partial
// instead of a full table scan. Not scoped to one repository: a population pull request (and so a
// partial row) can be in any repository the population authors merge into, not just the
// configured deploy repository.
func ListPartialPullRequests(ctx context.Context, pool *store.Pool) ([]DeliveryPullRequest, error) {
	return listPullRequests(ctx, pool, `
		select `+PullRequestColumns+`
		from delivery_pull_requests
		where partial and unfetchable_at is null
		order by repo, number
	`)
}

// ListUnreadAttributionPullRequests lists up to limit complete rows whose attribution inputs
// have never been read from GitHub (rows stored before they were), newest merge first: the
// reconcile's backfill of them, through delivery_pull_requests_attribution_unread.
func ListUnreadAttributionPullRequests(ctx context.Context, pool *store.Pool, limit int) ([]DeliveryPullRequest, error) {
	return listPullRequests(ctx, pool, `
		select `+PullRequestColumns+`
		from delivery_pull_requests
		where attribution_title_keys is null and not partial and unfetchable_at is null
		order by merged_at desc nulls last, repo, number
		limit $1
	`, limit)
}

func listPullRequests(ctx context.Context, pool *store.Pool, query string, args ...any) ([]DeliveryPullRequest, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prs := []DeliveryPullRequest{}
	for rows.Next() {
		pr, err := ScanPullRequest(rows)
		if err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

// RunColumns is the delivery_runs select list ScanRun reads, in scan order.
const RunColumns = `repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, completed_at, conclusion, url`

// ScanRun decodes one RunColumns row into a DeliveryRun.
func ScanRun(row pgx.Row) (DeliveryRun, error) {
	var run DeliveryRun
	var kind string
	var conclusion *string
	if err := row.Scan(
		&run.Repo, &run.RunID, &kind, &run.PRNumber, &run.HeadSHA, &run.HeadCommitAt,
		&run.StartedAt, &run.CompletedAt, &conclusion, &run.URL,
	); err != nil {
		return DeliveryRun{}, err
	}
	run.Kind = DeliveryRunKind(kind)
	if conclusion != nil {
		c := DeliveryRunConclusion(*conclusion)
		run.Conclusion = &c
	}
	return run, nil
}

// UpsertRun writes run, keyed on (repo, run_id). Like UpsertPullRequest, intake and reconcile
// share this one upsert: a run intake saw only in progress (nil CompletedAt/Conclusion) and a
// reconcile pass that reads it already concluded both write through here, and the later,
// more-complete write simply wins — there is no ordering to get wrong because every write states
// the run's whole current fields, not a delta.
func UpsertRun(ctx context.Context, pool *store.Pool, run DeliveryRun) error {
	var conclusion *string
	if run.Conclusion != nil {
		c := string(*run.Conclusion)
		conclusion = &c
	}
	_, err := pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, completed_at, conclusion, url)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		on conflict (repo, run_id) do update set
			kind = excluded.kind,
			pr_number = excluded.pr_number,
			head_sha = excluded.head_sha,
			head_commit_at = excluded.head_commit_at,
			started_at = excluded.started_at,
			completed_at = excluded.completed_at,
			conclusion = excluded.conclusion,
			url = excluded.url
	`, run.Repo, run.RunID, string(run.Kind), run.PRNumber, run.HeadSHA, run.HeadCommitAt, run.StartedAt, run.CompletedAt, conclusion, run.URL)
	return err
}

// JobColumns is the delivery_run_jobs select list ScanJob reads, in scan order.
const JobColumns = `repo, run_id, name, started_at, completed_at, conclusion`

// ScanJob decodes one JobColumns row into a DeliveryRunJob.
func ScanJob(row pgx.Row) (DeliveryRunJob, error) {
	var job DeliveryRunJob
	var conclusion *string
	if err := row.Scan(&job.Repo, &job.RunID, &job.Name, &job.StartedAt, &job.CompletedAt, &conclusion); err != nil {
		return DeliveryRunJob{}, err
	}
	if conclusion != nil {
		c := DeliveryJobConclusion(*conclusion)
		job.Conclusion = &c
	}
	return job, nil
}

// UpsertRunJobs writes every job of one run, keyed on (repo, run_id, name): a job's own identity
// is its name, since GitHub does not number jobs and a run never repeats one within the attempt
// intake and reconcile keep. The run row itself must already exist (delivery_runs' foreign key).
// Reaching here means ListWorkflowRunJobs just succeeded for this run, so this also clears
// jobs_unfetchable_at/jobs_unfetchable_reason to null: a successful listing is itself the "it is
// fetchable after all" signal, the same way a successful UpsertPullRequest clears
// unfetchable_at/unfetchable_reason.
func UpsertRunJobs(ctx context.Context, pool *store.Pool, repo string, runID int64, jobs []DeliveryRunJob) error {
	for _, job := range jobs {
		var conclusion *string
		if job.Conclusion != nil {
			c := string(*job.Conclusion)
			conclusion = &c
		}
		if _, err := pool.Exec(ctx, `
			insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
			values ($1, $2, $3, $4, $5, $6)
			on conflict (repo, run_id, name) do update set
				started_at = excluded.started_at,
				completed_at = excluded.completed_at,
				conclusion = excluded.conclusion
		`, repo, runID, job.Name, job.StartedAt, job.CompletedAt, conclusion); err != nil {
			return fmt.Errorf("upsert job %q of run %d: %w", job.Name, runID, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		update delivery_runs set jobs_unfetchable_at = null, jobs_unfetchable_reason = null
		where repo = $1 and run_id = $2
	`, repo, runID); err != nil {
		return fmt.Errorf("clear jobs-unfetchable mark for run %d: %w", runID, err)
	}
	return nil
}

// MarkRunJobsUnfetchable records that repo's run_id's job listing answered a permanent 404
// (github_runs.go's ErrRunNotFound) or a 404 resolving which installation covers the repository
// (githubapp.ErrNoInstallation): the run's repository no longer exists, or no longer reaches this
// token. reconcileRun skips re-listing this run's jobs on every further pass until a later
// successful UpsertRunJobs clears it. Conditioned on no job row already existing for this run: a
// concurrent write (a live webhook's own fetch finishing between this caller's own failed fetch
// and this UPDATE running) must not be overwritten back to unfetchable.
func MarkRunJobsUnfetchable(ctx context.Context, pool *store.Pool, repo string, runID int64, reason string) error {
	_, err := pool.Exec(ctx, `
		update delivery_runs
		set jobs_unfetchable_at = now(), jobs_unfetchable_reason = $3
		where repo = $1 and run_id = $2
			and not exists (select 1 from delivery_run_jobs where repo = $1 and run_id = $2)
	`, repo, runID, reason)
	return err
}

// RunJobsUnfetchable reports whether repo's run_id already carries a MarkRunJobsUnfetchable mark
// from an earlier pass, so reconcileRun can skip re-listing jobs GitHub has already told this
// installation are permanently gone, rather than repeating the same failing call on every further
// pass within the reconcile overlap window.
func RunJobsUnfetchable(ctx context.Context, pool *store.Pool, repo string, runID int64) (bool, error) {
	var unfetchableAt *time.Time
	err := pool.QueryRow(ctx, `
		select jobs_unfetchable_at from delivery_runs where repo = $1 and run_id = $2
	`, repo, runID).Scan(&unfetchableAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return unfetchableAt != nil, nil
}

// ListUnfetchableRunIDs reads, among runIDs, which ones already carry a MarkRunJobsUnfetchable
// mark, in one query -- reconcileWorkflow calls this once per pass for every completed run its
// window found, instead of reconcileRun's own RunJobsUnfetchable point query running once per
// completed run on every pass: the overwhelming majority of runs are never marked unfetchable,
// the same bulk-then-loop shape ListPartialPullRequests already gives reconcilePartialPullRequests
// and ListRunJobsForRuns gives the timeline handler.
func ListUnfetchableRunIDs(ctx context.Context, pool *store.Pool, repo string, runIDs []int64) (map[int64]bool, error) {
	unfetchable := make(map[int64]bool, len(runIDs))
	if len(runIDs) == 0 {
		return unfetchable, nil
	}
	rows, err := pool.Query(ctx, `
		select run_id from delivery_runs
		where repo = $1 and run_id = any($2) and jobs_unfetchable_at is not null
	`, repo, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		unfetchable[id] = true
	}
	return unfetchable, rows.Err()
}

// ListRunJobs lists every job of one run, in no particular order (callers that need the jobs in
// a specific order, e.g. a workflow's declared sequence, sort client-side — GitHub's own job
// listing order is not guaranteed either).
func ListRunJobs(ctx context.Context, pool *store.Pool, repo string, runID int64) ([]DeliveryRunJob, error) {
	rows, err := pool.Query(ctx, `select `+JobColumns+` from delivery_run_jobs where repo = $1 and run_id = $2`, repo, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []DeliveryRunJob{}
	for rows.Next() {
		job, err := ScanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListRunJobsForRuns lists every job of every run in runIDs on repo in one query (not one query
// per run): the timeline handler's own lookback window can hold hundreds of runs, and
// ResolveSessionTitles/resolveIssueFacetData already use this `= any($1)` shape for the same
// reason. Returns a map keyed by run id for a caller to index directly.
func ListRunJobsForRuns(ctx context.Context, pool *store.Pool, repo string, runIDs []int64) (map[int64][]DeliveryRunJob, error) {
	byRun := make(map[int64][]DeliveryRunJob, len(runIDs))
	if len(runIDs) == 0 {
		return byRun, nil
	}
	rows, err := pool.Query(ctx, `select `+JobColumns+` from delivery_run_jobs where repo = $1 and run_id = any($2)`, repo, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		job, err := ScanJob(rows)
		if err != nil {
			return nil, err
		}
		byRun[job.RunID] = append(byRun[job.RunID], job)
	}
	return byRun, rows.Err()
}

// ListRuns lists every run of kind on repo with head_commit_at or later, oldest first: the
// containment algorithm's input (population.go/containment.go), and the window the API reads for
// the timeline. delivery_runs_kind_started's index covers (repo, kind, head_commit_at) to match
// this exact filter/order -- a prior revision indexed started_at instead, which this query never
// filters or orders by. Bounded by maxRunsPerWindow for the same reason
// ListPullRequestsInWindow is: an unbounded caller should fail loudly rather than exhaust memory.
const maxRunsPerWindow = 50_000

func ListRuns(ctx context.Context, pool *store.Pool, repo string, kind DeliveryRunKind, since time.Time) ([]DeliveryRun, error) {
	rows, err := pool.Query(ctx, `
		select `+RunColumns+`
		from delivery_runs
		where repo = $1 and kind = $2 and head_commit_at >= $3
		order by head_commit_at
		limit $4
	`, repo, string(kind), since, maxRunsPerWindow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []DeliveryRun{}
	for rows.Next() {
		run, err := ScanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// ListRunsStartedIn lists every run of kind on repo with started_at in [from, to), oldest start
// first, narrowed to one head branch when branch is non-empty and to one GitHub event when event
// is non-empty (a null head_branch or event never matches a non-empty filter: it is a row the
// backfill has not rewritten yet). The measures' deploy population is ("main", ""), the
// Pipeline's deploy cards ("main", "push"), PR checks ("", "pull_request"): the prototype's
// `withinWindow(run.started_at)`, `branch=main` and `event=pull_request` listings. Bounded by
// maxRunsPerWindow, as ListRuns is.
//
// delivery_runs does not store head_branch or event yet, so branch and event narrow nothing until
// the migration that adds them (LEGION-567 slice 2, Task 2b) adds their two predicates here; the
// signature is final, so no caller changes when it does.
func ListRunsStartedIn(ctx context.Context, pool *store.Pool, repo string, kind DeliveryRunKind, branch, event string, from, to time.Time) ([]DeliveryRun, error) {
	rows, err := pool.Query(ctx, `
		select `+RunColumns+`
		from delivery_runs
		where repo = $1 and kind = $2 and started_at >= $3 and started_at < $4
		order by started_at
		limit $5
	`, repo, string(kind), from, to, maxRunsPerWindow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []DeliveryRun{}
	for rows.Next() {
		run, err := ScanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// maxPullRequestsPerWindow bounds ListPullRequestsInWindow: a window a caller (the API, a future
// script) opens too wide should fail loudly against this limit rather than return an unbounded
// result set that could exhaust memory -- 28 days of this population is on the order of a few
// thousand rows (CONTRACT.md: "28 days is roughly 3,500 PRs"), so this is generous headroom, not
// a tight production ceiling.
const maxPullRequestsPerWindow = 50_000

// ListPullRequestsInWindow lists every population pull request merged in [from, to), oldest
// merge first, up to maxPullRequestsPerWindow: the API's timeline window and the facet
// computations that run over it. A partial row (merged_at still null) is never in range and so
// never appears here — exactly right, since its lead time and shipped status are not yet knowable.
func ListPullRequestsInWindow(ctx context.Context, pool *store.Pool, from, to time.Time) ([]DeliveryPullRequest, error) {
	rows, err := pool.Query(ctx, `
		select `+PullRequestColumns+`
		from delivery_pull_requests
		where merged_at >= $1 and merged_at < $2
		order by merged_at
		limit $3
	`, from, to, maxPullRequestsPerWindow)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prs := []DeliveryPullRequest{}
	for rows.Next() {
		pr, err := ScanPullRequest(rows)
		if err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}
