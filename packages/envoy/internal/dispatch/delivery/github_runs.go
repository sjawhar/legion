package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// ErrRunNotFound is a 404 from GitHub listing a specific run's jobs: the run or its repository no
// longer exists, or no longer reaches this token. A permanent condition, unlike every other
// ListWorkflowRunJobs failure (a transient 5xx, a rate limit) -- reconcile.reconcileRun marks the
// run's jobs unfetchable rather than retrying forever, the same treatment github_prs.go's
// ErrPullRequestNotFound gives a permanently-gone pull request.
var ErrRunNotFound = errors.New("workflow run not found or gone")

// FetchedRun is one workflow run's GitHub facts.
//
// Conclusion carries GitHub's raw conclusion string verbatim -- "success", "failure",
// "cancelled", and values this package has no opinion on, such as "skipped", "neutral", or
// "stale". A workflow run and a job draw from different GitHub conclusion vocabularies, and the
// delivery_runs schema's check constraint accepts only "success", "failure", and "cancelled" for
// a run (delivery_run_jobs accepts a wider set for a job). This package filters nothing: it is
// the caller's job to map a run's Conclusion to DeliveryRunConclusion and drop or ignore a value
// the schema's constraint would refuse, so an unrecognized or future GitHub conclusion value is
// handled by that documented policy rather than causing this package to panic or guess.
type FetchedRun struct {
	RunID        int64
	HeadSHA      string
	HeadCommitAt time.Time
	StartedAt    time.Time
	CompletedAt  *time.Time
	Conclusion   *string
	PRNumber     *int // non-nil only when GitHub associates the run with exactly one pull request
	URL          string
}

// FetchedJob is one job of a FetchedRun.
type FetchedJob struct {
	Name        string
	StartedAt   *time.Time
	CompletedAt *time.Time
	Conclusion  *string
}

// workflowRunsPageSize bounds one page of the Actions run listing, so that a page fits inside
// githubapp's 1 MiB response cap even when the largest runs land together. A run object carries
// its head commit's whole message, which GitHub truncates at 65,536 characters, so one run
// measures up to about 80 KB: measured against the live API over 300 runs of a busy deploy
// repository, a run is 13.7 KB at its smallest, 16 KB at the median, 77 KB at the 99th
// percentile and 79,943 bytes at its largest. Ten runs is 800 KB at that worst case and
// 148-208 KB in practice; 40 is past the cap whenever a handful of long squash-merge messages
// fall in one page.
const workflowRunsPageSize = 10

// runJobsPageSize bounds one page of a run's jobs listing, which is a different shape and sized
// separately: measured live, one job with its steps is at most 12,376 bytes and a run carries
// 27-33 jobs, so 40 jobs is 33-55 KB in practice and under 500 KB at that worst case -- one
// request per run, with the same margin under the 1 MiB cap.
const runJobsPageSize = 40

// workflowRunsPayload is GET /repos/{owner}/{repo}/actions/workflows/{workflow_path}/runs's
// answer, limited to the fields ListWorkflowRuns reads.
type workflowRunsPayload struct {
	TotalCount   int               `json:"total_count"`
	WorkflowRuns []workflowRunItem `json:"workflow_runs"`
}

type workflowRunItem struct {
	ID           int64      `json:"id"`
	HeadSHA      string     `json:"head_sha"`
	HTMLURL      string     `json:"html_url"`
	Status       string     `json:"status"`
	Conclusion   *string    `json:"conclusion"`
	RunStartedAt *time.Time `json:"run_started_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
	HeadCommit *struct {
		Timestamp time.Time `json:"timestamp"`
	} `json:"head_commit"`
}

func fetchedRunFromItem(item workflowRunItem) FetchedRun {
	run := FetchedRun{
		RunID:      item.ID,
		HeadSHA:    item.HeadSHA,
		StartedAt:  item.CreatedAt,
		Conclusion: item.Conclusion,
		URL:        item.HTMLURL,
	}
	if item.HeadCommit != nil {
		run.HeadCommitAt = item.HeadCommit.Timestamp
	} else {
		// GitHub's head_commit is nullable; fall back to the run's own creation time rather than
		// leave the zero time, which would sort before every real commit.
		run.HeadCommitAt = item.CreatedAt
	}
	if item.RunStartedAt != nil {
		run.StartedAt = *item.RunStartedAt
	}
	if item.Status == "completed" {
		completedAt := item.UpdatedAt
		run.CompletedAt = &completedAt
	}
	if len(item.PullRequests) == 1 {
		number := item.PullRequests[0].Number
		run.PRNumber = &number
	}
	return run
}

// ListWorkflowRuns walks every run of the workflow at workflowPath in owner/repo created in
// [since, until], oldest window first, via GET
// /repos/{owner}/{repo}/actions/workflows/{workflow_path}/runs?created=ISO..ISO (GitHub accepts
// the workflow file's path, URL-encoded, in place of its numeric id). Paginated fully and halved
// on overflow through the same walkWindowed (windowed.go) searchMergedPullRequests uses -- the
// Actions API shares the same 1,000-result-per-query cap.
//
// visit is called once per window with that window's own upper bound and its runs, so the caller
// can record how far it has imported after each one rather than only at the end: maxRuns bounds
// how much work one window is, and so how much a failed pass redoes. Every completed run costs
// its own jobs request, so a window is far more work than its listing pages alone.
func ListWorkflowRuns(ctx context.Context, client *githubapp.Client, owner, repo, workflowPath string, since, until time.Time, maxRuns int, visit func(windowUntil time.Time, runs []FetchedRun) error) error {
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("mint installation token for %s/%s workflow runs: %w", owner, repo, err)
	}
	scope := fmt.Sprintf("%s workflow runs for %s/%s", workflowPath, owner, repo)
	newFetcher := func(since, until time.Time) func() ([]FetchedRun, int, error) {
		page := 0
		return func() ([]FetchedRun, int, error) {
			page++
			return fetchWorkflowRunsPage(ctx, client, token, owner, repo, workflowPath, since, until, page)
		}
	}
	return walkWindowed(since, until, scope, maxRuns, newFetcher, visit)
}

func fetchWorkflowRunsPage(ctx context.Context, client *githubapp.Client, token, owner, repo, workflowPath string, since, until time.Time, page int) ([]FetchedRun, int, error) {
	created := since.UTC().Format(time.RFC3339) + ".." + until.UTC().Format(time.RFC3339)
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?created=%s&per_page=%d&page=%d",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(workflowPath), url.QueryEscape(created), workflowRunsPageSize, page)
	body, status, header, err := readGitHubPage(ctx, client, token, path)
	if err != nil {
		return nil, 0, fmt.Errorf("page %d (%s): %w", page, pageAttemptsNote(), err)
	}
	if err := githubapp.CheckResponse(status, header, body); err != nil {
		return nil, 0, fmt.Errorf("page %d (%s): %w", page, pageAttemptsNote(), err)
	}
	var payload workflowRunsPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, 0, fmt.Errorf("decode page %d: %w", page, err)
	}
	runs := make([]FetchedRun, len(payload.WorkflowRuns))
	for i, item := range payload.WorkflowRuns {
		runs[i] = fetchedRunFromItem(item)
	}
	return runs, payload.TotalCount, nil
}

// FetchWorkflowRun fetches one run by id via GET /repos/{owner}/{repo}/actions/runs/{run_id} --
// the same object shape as one element of ListWorkflowRuns' workflow_runs array, decoded with the
// same workflowRunItem/fetchedRunFromItem. Used by intake.go to re-verify a live NATS
// workflow-run envelope's run_id against GitHub's own answer before trusting anything about it:
// an Envoy /v1 bearer can publish an arbitrary envelope naming any repo/run_id/conclusion, so the
// envelope is only ever used to know which run to look up, never to supply the facts stored.
func FetchWorkflowRun(ctx context.Context, client *githubapp.Client, owner, repo string, runID int64) (FetchedRun, error) {
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return FetchedRun{}, fmt.Errorf("mint installation token for %s/%s run %d: %w", owner, repo, runID, err)
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d", url.PathEscape(owner), url.PathEscape(repo), runID)
	body, status, header, err := readGitHubPage(ctx, client, token, path)
	if err != nil {
		return FetchedRun{}, fmt.Errorf("fetch %s/%s run %d: %w", owner, repo, runID, err)
	}
	if err := githubapp.CheckResponse(status, header, body); err != nil {
		return FetchedRun{}, fmt.Errorf("fetch %s/%s run %d: %w", owner, repo, runID, err)
	}
	var item workflowRunItem
	if err := json.Unmarshal(body, &item); err != nil {
		return FetchedRun{}, fmt.Errorf("decode %s/%s run %d: %w", owner, repo, runID, err)
	}
	return fetchedRunFromItem(item), nil
}

// jobsPayload is GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs's answer, limited to the
// fields ListRunJobs reads.
type jobsPayload struct {
	TotalCount int       `json:"total_count"`
	Jobs       []jobItem `json:"jobs"`
}

type jobItem struct {
	Name        string     `json:"name"`
	Conclusion  *string    `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// ListWorkflowRunJobs lists every job of one run's latest attempt via GET
// /repos/{owner}/{repo}/actions/runs/{run_id}/jobs?filter=latest, paginated fully. Named
// distinctly from store.go's ListRunJobs (a Postgres read of already-stored jobs): this is the
// GitHub fetch that feeds it.
func ListWorkflowRunJobs(ctx context.Context, client *githubapp.Client, owner, repo string, runID int64) ([]FetchedJob, error) {
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("mint installation token for %s/%s run %d jobs: %w", owner, repo, runID, err)
	}

	var jobs []FetchedJob
	total := -1
	for page := 1; total < 0 || len(jobs) < total; page++ {
		path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=%d&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), runID, runJobsPageSize, page)
		body, status, header, err := readGitHubPage(ctx, client, token, path)
		if err != nil {
			return nil, fmt.Errorf("list jobs of %s/%s run %d (page %d, %s): %w", owner, repo, runID, page, pageAttemptsNote(), err)
		}
		if status == http.StatusNotFound {
			return nil, fmt.Errorf("list jobs of %s/%s run %d (page %d): %w (status %d)", owner, repo, runID, page, ErrRunNotFound, status)
		}
		if err := githubapp.CheckResponse(status, header, body); err != nil {
			return nil, fmt.Errorf("list jobs of %s/%s run %d (page %d): %w", owner, repo, runID, page, err)
		}
		var payload jobsPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode jobs of %s/%s run %d (page %d): %w", owner, repo, runID, page, err)
		}
		total = payload.TotalCount
		if len(payload.Jobs) == 0 {
			break
		}
		for _, j := range payload.Jobs {
			jobs = append(jobs, FetchedJob{
				Name:        j.Name,
				StartedAt:   j.StartedAt,
				CompletedAt: j.CompletedAt,
				Conclusion:  j.Conclusion,
			})
		}
	}
	return jobs, nil
}
