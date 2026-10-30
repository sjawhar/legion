package githubrest

import (
	"context"
	"net/url"
	"time"
)

// CheckRun is one check run on a commit as GitHub's check-runs list answers it: its id, its name,
// its status (queued, in_progress, completed, or waiting, requested, pending), its conclusion once
// completed (empty before), when it completed (nil before), and the GitHub App that created it.
type CheckRun struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	CompletedAt *time.Time `json:"completed_at"`
	App         struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

// CheckRuns is every check run on commit sha of the client's repository, each run of a check
// apart, as GET /commits/{sha}/check-runs?filter=all lists them (REST API, List check runs for a
// Git reference, https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference):
// a request per hundred runs (GetListPages). filter=all keeps the runs GitHub's default of latest
// leaves out, so a caller sees every run's id and picks the latest of each check itself.
func (c Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	return GetListPages[CheckRun](ctx, c, "/commits/"+url.PathEscape(sha)+"/check-runs?filter=all", "check_runs")
}
