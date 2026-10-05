package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// FetchedRun is one workflow run's GitHub facts.
//
// Conclusion carries GitHub's raw conclusion string verbatim -- "success", "failure",
// "cancelled", and values this package has no opinion on, such as "skipped", "neutral", or
// "stale". A workflow run and a job draw from different GitHub conclusion vocabularies, and the
// delivery_runs schema's check constraint accepts only "success", "failure", and "cancelled" for
// a run (delivery_run_jobs accepts a wider set for a job). This package filters nothing: it is
// the caller's job to map a run's Conclusion to model.DeliveryRunConclusion and drop or ignore a
// value the schema's constraint would refuse, so an unrecognized or future GitHub conclusion
// value is handled by that documented policy rather than causing this package to panic or guess.
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

// minimumRunWindow mirrors minimumSearchWindow for ListWorkflowRuns' halving recursion.
const minimumRunWindow = time.Second

// ListWorkflowRuns lists every run of the workflow at workflowPath in owner/repo created in
// [since, until), via GET
// /repos/{owner}/{repo}/actions/workflows/{workflow_path}/runs?created=ISO..ISO (GitHub accepts
// the workflow file's path, URL-encoded, in place of its numeric id). Paginated fully (per_page
// 100) and, exactly like SearchMergedPullRequests, recursively halved at the window's midpoint
// when a query's total_count exceeds 1,000 -- the Actions API shares the same per-query cap, and
// the two halves are a half-open partition of the original window.
func ListWorkflowRuns(ctx context.Context, client *githubapp.Client, owner, repo, workflowPath string, since, until time.Time) ([]FetchedRun, error) {
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("mint installation token for %s/%s workflow runs: %w", owner, repo, err)
	}
	return listWorkflowRuns(ctx, client, token, owner, repo, workflowPath, since, until)
}

func listWorkflowRuns(ctx context.Context, client *githubapp.Client, token, owner, repo, workflowPath string, since, until time.Time) ([]FetchedRun, error) {
	first, totalCount, err := fetchWorkflowRunsPage(ctx, client, token, owner, repo, workflowPath, since, until, 1)
	if err != nil {
		return nil, fmt.Errorf("list %s workflow runs for %s/%s in [%s, %s): %w", workflowPath, owner, repo, since, until, err)
	}

	if totalCount > 1000 {
		if until.Sub(since) <= minimumRunWindow {
			return nil, fmt.Errorf("list %s workflow runs for %s/%s: %d results in the window [%s, %s), which cannot be narrowed further", workflowPath, owner, repo, totalCount, since, until)
		}
		mid := since.Add(until.Sub(since) / 2)
		before, err := listWorkflowRuns(ctx, client, token, owner, repo, workflowPath, since, mid)
		if err != nil {
			return nil, err
		}
		after, err := listWorkflowRuns(ctx, client, token, owner, repo, workflowPath, mid, until)
		if err != nil {
			return nil, err
		}
		return append(before, after...), nil
	}

	results := first
	for page := 2; len(results) < totalCount; page++ {
		items, _, err := fetchWorkflowRunsPage(ctx, client, token, owner, repo, workflowPath, since, until, page)
		if err != nil {
			return nil, fmt.Errorf("list %s workflow runs for %s/%s in [%s, %s): %w", workflowPath, owner, repo, since, until, err)
		}
		if len(items) == 0 {
			break
		}
		results = append(results, items...)
	}
	return results, nil
}

func fetchWorkflowRunsPage(ctx context.Context, client *githubapp.Client, token, owner, repo, workflowPath string, since, until time.Time, page int) ([]FetchedRun, int, error) {
	created := since.UTC().Format(time.RFC3339) + ".." + until.UTC().Format(time.RFC3339)
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?created=%s&per_page=100&page=%d",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(workflowPath), url.QueryEscape(created), page)
	body, status, _, err := client.Read(ctx, token, path)
	if err != nil {
		return nil, 0, fmt.Errorf("page %d: %w", page, err)
	}
	if status != http.StatusOK {
		return nil, 0, fmt.Errorf("page %d: status %d: %s", page, status, body)
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
		path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=100&page=%d",
			url.PathEscape(owner), url.PathEscape(repo), runID, page)
		body, status, _, err := client.Read(ctx, token, path)
		if err != nil {
			return nil, fmt.Errorf("list jobs of %s/%s run %d (page %d): %w", owner, repo, runID, page, err)
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("list jobs of %s/%s run %d (page %d): status %d: %s", owner, repo, runID, page, status, body)
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
