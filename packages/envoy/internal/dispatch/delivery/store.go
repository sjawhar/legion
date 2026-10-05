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
const SettingsColumns = `deploy_repo, deploy_workflow_path, production_job_name, pr_checks_workflow_path, population_authors, excluded_repos, last_event_at, last_reconcile_at, updated_by, updated_at`

// ScanSettings decodes one SettingsColumns row into a model.DeliverySettings.
func ScanSettings(row pgx.Row) (model.DeliverySettings, error) {
	var settings model.DeliverySettings
	var updatedBy []byte
	if err := row.Scan(
		&settings.DeployRepo, &settings.DeployWorkflowPath, &settings.ProductionJobName,
		&settings.PRChecksWorkflowPath, &settings.PopulationAuthors, &settings.ExcludedRepos,
		&settings.LastEventAt, &settings.LastReconcileAt, &updatedBy, &settings.UpdatedAt,
	); err != nil {
		return model.DeliverySettings{}, err
	}
	if err := json.Unmarshal(updatedBy, &settings.UpdatedBy); err != nil {
		return model.DeliverySettings{}, fmt.Errorf("decode delivery settings author: %w", err)
	}
	return settings, nil
}

// ErrNoSettings is "delivery_settings has no row yet": intake and reconcile stay idle, and the
// API answers that delivery is not configured, rather than guessing a repository.
var ErrNoSettings = errors.New("delivery settings are not configured")

// GetSettings reads the one delivery_settings row, or ErrNoSettings when none has been written
// yet.
func GetSettings(ctx context.Context, pool *store.Pool) (model.DeliverySettings, error) {
	settings, err := ScanSettings(pool.QueryRow(ctx, `select `+SettingsColumns+` from delivery_settings where singleton`))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.DeliverySettings{}, ErrNoSettings
		}
		return model.DeliverySettings{}, err
	}
	return settings, nil
}

// PutSettings upserts the one delivery_settings row, clearing the freshness columns: a settings
// change (a different repository, a different population) makes the previous freshness
// meaningless, and the next reconcile sets it again from scratch.
func PutSettings(ctx context.Context, pool *store.Pool, settings model.DeliverySettings, actor model.Actor) (model.DeliverySettings, error) {
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return model.DeliverySettings{}, fmt.Errorf("encode delivery settings author: %w", err)
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

// RecordReconcileAt sets delivery_settings.last_reconcile_at to t: called once a reconcile pass
// completes, successful or not (a failed pass still proves the attempt, which is what the
// freshness row promises).
func RecordReconcileAt(ctx context.Context, pool *store.Pool, t time.Time) error {
	_, err := pool.Exec(ctx, `update delivery_settings set last_reconcile_at = $1 where singleton`, t)
	return err
}

// PullRequestColumns is the delivery_pull_requests select list ScanPullRequest reads, in scan
// order.
const PullRequestColumns = `repo, number, title, url, author, created_at, merged_at, first_commit_at, merge_commit_sha, additions, deletions, rework, issue_key, sessions, partial, updated_at`

// ScanPullRequest decodes one PullRequestColumns row into a model.DeliveryPullRequest.
func ScanPullRequest(row pgx.Row) (model.DeliveryPullRequest, error) {
	var pr model.DeliveryPullRequest
	if err := row.Scan(
		&pr.Repo, &pr.Number, &pr.Title, &pr.URL, &pr.Author, &pr.CreatedAt, &pr.MergedAt,
		&pr.FirstCommitAt, &pr.MergeCommitSHA, &pr.Additions, &pr.Deletions, &pr.Rework,
		&pr.IssueKey, &pr.Sessions, &pr.Partial, &pr.UpdatedAt,
	); err != nil {
		return model.DeliveryPullRequest{}, err
	}
	return pr, nil
}

// UpsertPullRequest writes pr, keyed on (repo, number). Every field pr carries overwrites the
// stored row's: a live intake write that knows only the envelope's fields passes nil for the
// rest and Partial true, and a later completing fetch or reconcile pass overwrites that same row
// with the complete fields and Partial false — the one shared upsert both paths call, so "the
// reconcile catches a missed event" and "the completing fetch completes a partial row" are the
// same code path, never two implementations that can drift.
func UpsertPullRequest(ctx context.Context, pool *store.Pool, pr model.DeliveryPullRequest) error {
	sessions := pr.Sessions
	if sessions == nil {
		// pgx sends a nil slice as SQL NULL, not the column default: an explicit insert value
		// always wins over the default, so a nil Sessions here would violate the not-null
		// constraint instead of quietly becoming '{}'.
		sessions = []string{}
	}
	_, err := pool.Exec(ctx, `
		insert into delivery_pull_requests (
			repo, number, title, url, author, created_at, merged_at, first_commit_at,
			merge_commit_sha, additions, deletions, rework, issue_key, sessions, partial
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
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
			updated_at = now()
	`,
		pr.Repo, pr.Number, pr.Title, pr.URL, pr.Author, pr.CreatedAt, pr.MergedAt, pr.FirstCommitAt,
		pr.MergeCommitSHA, pr.Additions, pr.Deletions, pr.Rework, pr.IssueKey, sessions, pr.Partial,
	)
	return err
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

// ListPartialPullRequests lists every partial=true row, oldest merge first: the reconcile's cheap
// way to find rows a completing fetch never finished, using delivery_pull_requests_partial
// instead of a full table scan. Not scoped to one repository: a population pull request (and so a
// partial row) can be in any repository the population authors merge into, not just the
// configured deploy repository.
func ListPartialPullRequests(ctx context.Context, pool *store.Pool) ([]model.DeliveryPullRequest, error) {
	rows, err := pool.Query(ctx, `
		select `+PullRequestColumns+`
		from delivery_pull_requests
		where partial
		order by repo, number
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prs := []model.DeliveryPullRequest{}
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

// ScanRun decodes one RunColumns row into a model.DeliveryRun.
func ScanRun(row pgx.Row) (model.DeliveryRun, error) {
	var run model.DeliveryRun
	var kind string
	var conclusion *string
	if err := row.Scan(
		&run.Repo, &run.RunID, &kind, &run.PRNumber, &run.HeadSHA, &run.HeadCommitAt,
		&run.StartedAt, &run.CompletedAt, &conclusion, &run.URL,
	); err != nil {
		return model.DeliveryRun{}, err
	}
	run.Kind = model.DeliveryRunKind(kind)
	if conclusion != nil {
		c := model.DeliveryRunConclusion(*conclusion)
		run.Conclusion = &c
	}
	return run, nil
}

// UpsertRun writes run, keyed on (repo, run_id). Like UpsertPullRequest, intake and reconcile
// share this one upsert: a run intake saw only in progress (nil CompletedAt/Conclusion) and a
// reconcile pass that reads it already concluded both write through here, and the later,
// more-complete write simply wins — there is no ordering to get wrong because every write states
// the run's whole current fields, not a delta.
func UpsertRun(ctx context.Context, pool *store.Pool, run model.DeliveryRun) error {
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

// ScanJob decodes one JobColumns row into a model.DeliveryRunJob.
func ScanJob(row pgx.Row) (model.DeliveryRunJob, error) {
	var job model.DeliveryRunJob
	var conclusion *string
	if err := row.Scan(&job.Repo, &job.RunID, &job.Name, &job.StartedAt, &job.CompletedAt, &conclusion); err != nil {
		return model.DeliveryRunJob{}, err
	}
	if conclusion != nil {
		c := model.DeliveryRunConclusion(*conclusion)
		job.Conclusion = &c
	}
	return job, nil
}

// UpsertRunJobs writes every job of one run, keyed on (repo, run_id, name): a job's own identity
// is its name, since GitHub does not number jobs and a run never repeats one within the attempt
// intake and reconcile keep. The run row itself must already exist (delivery_runs' foreign key).
func UpsertRunJobs(ctx context.Context, pool *store.Pool, repo string, runID int64, jobs []model.DeliveryRunJob) error {
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
	return nil
}

// ListRunJobs lists every job of one run, in no particular order (callers that need the jobs in
// a specific order, e.g. a workflow's declared sequence, sort client-side — GitHub's own job
// listing order is not guaranteed either).
func ListRunJobs(ctx context.Context, pool *store.Pool, repo string, runID int64) ([]model.DeliveryRunJob, error) {
	rows, err := pool.Query(ctx, `select `+JobColumns+` from delivery_run_jobs where repo = $1 and run_id = $2`, repo, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []model.DeliveryRunJob{}
	for rows.Next() {
		job, err := ScanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListRuns lists every run of kind on repo with head_commit_at or later, oldest first: the
// containment algorithm's input (population.go/containment.go), and the window the API reads for
// the timeline.
func ListRuns(ctx context.Context, pool *store.Pool, repo string, kind model.DeliveryRunKind, since time.Time) ([]model.DeliveryRun, error) {
	rows, err := pool.Query(ctx, `
		select `+RunColumns+`
		from delivery_runs
		where repo = $1 and kind = $2 and head_commit_at >= $3
		order by head_commit_at
	`, repo, string(kind), since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []model.DeliveryRun{}
	for rows.Next() {
		run, err := ScanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// ListPullRequestsInWindow lists every population pull request merged in [from, to), oldest
// merge first: the API's timeline window and the facet computations that run over it. A partial
// row (merged_at still null) is never in range and so never appears here — exactly right, since
// its lead time and shipped status are not yet knowable.
func ListPullRequestsInWindow(ctx context.Context, pool *store.Pool, from, to time.Time) ([]model.DeliveryPullRequest, error) {
	rows, err := pool.Query(ctx, `
		select `+PullRequestColumns+`
		from delivery_pull_requests
		where merged_at >= $1 and merged_at < $2
		order by merged_at
	`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prs := []model.DeliveryPullRequest{}
	for rows.Next() {
		pr, err := ScanPullRequest(rows)
		if err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}
