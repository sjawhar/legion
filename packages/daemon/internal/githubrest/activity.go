package githubrest

import (
	"context"
	"net/url"
	"time"
)

// Activity is one entry of GitHub's activity of a repository's ref (GET /activity): what moved it
// (activity_type: push, force_push, branch_creation, branch_deletion, pr_merge or
// merge_queue_merge), the commit it held before and the one it held after, when, and who moved it.
// An App's push names its bot account, <slug>[bot], as the push webhook's pusher.name does.
type Activity struct {
	Type      string    `json:"activity_type"`
	Before    string    `json:"before"`
	After     string    `json:"after"`
	Ref       string    `json:"ref"`
	Timestamp time.Time `json:"timestamp"`
	Actor     struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"actor"`
}

// BranchActivity is the newest hundred entries of the activity of branch of the client's
// repository, newest first, as one GET /activity?ref=refs/heads/<branch>&per_page=100 answers them
// (REST API, List repository activities,
// https://docs.github.com/en/rest/repos/repos#list-repository-activities). It reads that one page
// alone: a caller following the branch back from its head finds what it needs there or not at all.
func (c Client) BranchActivity(ctx context.Context, branch string) ([]Activity, error) {
	var activity []Activity
	query := url.Values{"ref": {"refs/heads/" + branch}, "per_page": {"100"}}
	if err := c.Get(ctx, "/activity?"+query.Encode(), &activity); err != nil {
		return nil, err
	}
	return activity, nil
}
