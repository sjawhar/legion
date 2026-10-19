package ghautomerge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// checkPage is one page of a merge queue commit's checks as removals and moreChecks read it: whether
// GitHub lists more after it, and each check run, with its workflow run, and commit status.
const checkPage = `pageInfo { hasNextPage endCursor }
                  nodes {
                    __typename
                    ... on CheckRun { name status conclusion detailsUrl checkSuite { workflowRun { url } } }
                    ... on StatusContext { context state targetUrl }
                  }`

// removals is the read of what took a pull request's arm or merge-queue entry away: the GraphQL
// viewer, the account the token acts as, and the pull request's last arms and removals, oldest
// first, each queue removal with the commit the queue built for it and the first page of that
// commit's checks (moreChecks reads the rest).
const removals = `query PullRequestRemovals($owner: String!, $name: String!, $number: Int!) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      timelineItems(last: 20, itemTypes: [ADDED_TO_MERGE_QUEUE_EVENT, AUTO_MERGE_ENABLED_EVENT, REMOVED_FROM_MERGE_QUEUE_EVENT, AUTO_MERGE_DISABLED_EVENT]) {
        nodes {
          __typename
          ... on AddedToMergeQueueEvent { createdAt }
          ... on AutoMergeEnabledEvent { createdAt }
          ... on AutoMergeDisabledEvent { id createdAt reason reasonCode actor { login } }
          ... on RemovedFromMergeQueueEvent {
            id createdAt reason actor { login }
            beforeCommit {
              oid
              statusCheckRollup {
                contexts(first: 100) {
                  ` + checkPage + `
                }
              }
            }
          }
        }
      }
    }
  }
}`

// moreChecks is the read of a merge queue commit's checks after the page that ended at $after: a
// merge group can carry more than the 100 a page holds.
const moreChecks = `query MergeGroupContexts($owner: String!, $name: String!, $oid: GitObjectID!, $after: String!) {
  repository(owner: $owner, name: $name) {
    object(oid: $oid) {
      ... on Commit {
        oid
        statusCheckRollup {
          contexts(first: 100, after: $after) {
            ` + checkPage + `
          }
        }
      }
    }
  }
}`

// removal is what took pull request number of repo's arm or merge-queue entry away at READY's
// head, after Submit saw one there, as GitHub's timeline records it: the removals since the last
// arm or entry. The merge queue's removal for the merge itself is a wait for GitHub to show the
// merge (Merging), whatever else the timeline shows, and reads no check. A queue removal whose
// reason leaves it to the checks on the commit the queue built (timelineItem.judgedByChecks) is
// judged on every page of them (mergeGroupCommit.checks): one for failed_checks, or for an
// unexplained reason with a run that failed there (failingRuns), is MergeFailed. Two removals
// restart the submission, and removal answers nil when they are all there is, for Submit to submit
// again (timelineItem.restarts): the daemon's own disarm landing late, and auto-merge a close turned
// off, which the pull request's reopen at READY's head undid. Any other is Removed, the latest of
// them: GitHub's own (Removed.ByGitHub) when it names the account the token acts as, whose own
// disarm leaves no other reason. A timeline that shows no removal since the last arm is behind the
// read of the pull request (Disappeared).
func removal(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, head string) error {
	var answer struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Repository struct {
			PullRequest *struct {
				TimelineItems struct {
					Nodes []timelineItem `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := github.GraphQL(ctx, removals, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "number": number}, &answer); err != nil {
		return fmt.Errorf("read what took pull request #%d of %s out of the merge queue or off auto-merge: %w", number, repo, err)
	}
	if answer.Repository.PullRequest == nil {
		return fmt.Errorf("GitHub answered no timeline for pull request #%d of %s", number, repo)
	}
	items := answer.Repository.PullRequest.TimelineItems.Nodes
	since := 0
	for i, item := range items {
		if item.Type == "AddedToMergeQueueEvent" || item.Type == "AutoMergeEnabledEvent" {
			since = i + 1
		}
	}
	removed := items[since:]
	for _, item := range removed {
		if item.Type == "RemovedFromMergeQueueEvent" && item.Reason == "merged" {
			return &Merging{Number: number, Repo: repo, Head: head}
		}
	}
	self := strings.TrimSuffix(answer.Viewer.Login, "[bot]")
	var failed, other *timelineItem
	var runs []string
	for i := range removed {
		item := &removed[i]
		if item.judgedByChecks() {
			checks, err := item.BeforeCommit.checks(ctx, github, repo)
			if err != nil {
				return err
			}
			if failing := failingRuns(checks); item.Reason == "failed_checks" || len(failing) > 0 {
				failed, runs = item, failing
				continue
			}
		}
		if !item.restarts(self) {
			other = item
		}
	}
	switch {
	case failed != nil:
		commit := ""
		if failed.BeforeCommit != nil {
			commit = failed.BeforeCommit.Oid
		}
		return &MergeFailed{Number: number, Head: head, Reason: failed.Reason, Commit: commit,
			Runs: runs, Event: failed.ID, At: failed.CreatedAt}
	case other != nil:
		return &Removed{Number: number, Head: head, Actor: other.actor(), Reason: other.removalReason(), Detail: other.detail(),
			Queue: other.Type == "RemovedFromMergeQueueEvent", ByGitHub: self != "" && other.actor() == self, At: other.CreatedAt}
	case since == len(items):
		return &Disappeared{Number: number, Repo: repo, Head: head}
	default:
		return nil
	}
}

// judgedByChecks says whether a removal is a queue removal whose reason leaves it to the checks on
// the commit the queue built: failed_checks, whose failing runs the issue goes back to implementing
// on, and a reason GitHub gives no other meaning, a failed merge only with a run that failed there.
// The merge itself (merged), a person's dequeue (manual) and a conflict (merge_conflict) mean what
// they say, so none of their checks is read: a person's dequeue cancels the merge group's runs in
// flight, which is no failed merge.
func (t *timelineItem) judgedByChecks() bool {
	if t.Type != "RemovedFromMergeQueueEvent" {
		return false
	}
	switch t.Reason {
	case "merged", "manual", "merge_conflict":
		return false
	default:
		return true
	}
}

// restarts says whether a removal restarts the submission rather than ending it: auto-merge a close
// turned off (reason code closed), which the reopen at READY's head undid, and the daemon's own
// disarm, made by the account the token acts as (self, its login without [bot], as the timeline
// names a bot) with the reason a disarm leaves: auto-merge manually_disabled, or a manual dequeue.
// GitHub names the App that enabled auto-merge as the actor of its own disables (a merge method
// the repository blocks, a base switch), so the actor alone does not make a disable the daemon's.
func (t *timelineItem) restarts(self string) bool {
	if t.Type == "AutoMergeDisabledEvent" && t.ReasonCode == "closed" {
		return true
	}
	if self == "" || t.actor() != self {
		return false
	}
	return t.Type == "AutoMergeDisabledEvent" && t.ReasonCode == "manually_disabled" ||
		t.Type == "RemovedFromMergeQueueEvent" && t.Reason == "manual"
}

// timelineItem is one arm or removal on a pull request's timeline as removals reads it.
type timelineItem struct {
	Type       string    `json:"__typename"`
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"createdAt"`
	Reason     string    `json:"reason"`
	ReasonCode string    `json:"reasonCode"`
	Actor      *struct {
		Login string `json:"login"`
	} `json:"actor"`
	BeforeCommit *mergeGroupCommit `json:"beforeCommit"`
}

// actor is the login of who made the item, "" when GitHub names no one.
func (t *timelineItem) actor() string {
	if t.Actor == nil {
		return ""
	}
	return t.Actor.Login
}

// removalReason is GitHub's reason for a removal: a queue removal's reason, or the reason code of
// an auto-merge turned off.
func (t *timelineItem) removalReason() string {
	if t.Type == "AutoMergeDisabledEvent" {
		return t.ReasonCode
	}
	return t.Reason
}

// detail is GitHub's sentence for an auto-merge turned off, and none for a queue removal, whose
// reason is all GitHub says of it.
func (t *timelineItem) detail() string {
	if t.Type == "AutoMergeDisabledEvent" {
		return t.Reason
	}
	return ""
}

// failingRuns is each of checks, every check on the commit the merge queue built for a queue
// removal (mergeGroupCommit.checks), that failed there, named with how it ended and its run: a check
// run that ended in a result Legion's rule for a check run counts as failed (classify.RunResult:
// failure, timed out, failed to start, action required, stale), and a commit status in failure or
// error. A cancelled run is no failure here: a person's dequeue cancels every run in flight.
func failingRuns(checks []checkContext) []string {
	var runs []string
	for _, check := range checks {
		switch check.Type {
		case "CheckRun":
			result := classify.RunResult(strings.ToLower(check.Status), strings.ToLower(check.Conclusion))
			if result == classify.Pending || result == classify.Success || result == classify.Cancelled {
				continue
			}
			url := check.DetailsURL
			if check.CheckSuite != nil && check.CheckSuite.WorkflowRun != nil && check.CheckSuite.WorkflowRun.URL != "" {
				url = check.CheckSuite.WorkflowRun.URL
			}
			runs = append(runs, fmt.Sprintf("%s (%s, %s)", check.Name, result, url))
		case "StatusContext":
			if check.State == "FAILURE" || check.State == "ERROR" {
				runs = append(runs, fmt.Sprintf("%s (%s, %s)", check.Context, strings.ToLower(check.State), check.TargetURL))
			}
		}
	}
	return runs
}

// mergeGroupCommit is the commit the merge queue built for a pull request it took out, and how its
// checks ended.
type mergeGroupCommit struct {
	Oid               string `json:"oid"`
	StatusCheckRollup *struct {
		Contexts checkContexts `json:"contexts"`
	} `json:"statusCheckRollup"`
}

// checkContexts is the checks GitHub listed on a merge queue commit, and whether it lists more.
type checkContexts struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []checkContext `json:"nodes"`
}

// checks is every check GitHub lists on the commit: the first page, as the timeline's read carried
// it, then each later page, read one by one (moreChecks), so a judgment of them misses no failure
// past the first. A nil commit, or one with no checks, has none.
func (c *mergeGroupCommit) checks(ctx context.Context, github githubrest.Client, repo ghrepo.Repository) ([]checkContext, error) {
	if c == nil || c.StatusCheckRollup == nil {
		return nil, nil
	}
	checks, page := c.StatusCheckRollup.Contexts.Nodes, c.StatusCheckRollup.Contexts.PageInfo
	for page.HasNextPage {
		var answer struct {
			Repository struct {
				Object *mergeGroupCommit `json:"object"`
			} `json:"repository"`
		}
		if err := github.GraphQL(ctx, moreChecks, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "oid": c.Oid, "after": page.EndCursor}, &answer); err != nil {
			return nil, fmt.Errorf("read the checks of merge queue commit %s after %s: %w", c.Oid, page.EndCursor, err)
		}
		next := answer.Repository.Object
		if next == nil || next.StatusCheckRollup == nil {
			return nil, fmt.Errorf("GitHub answered no further checks of merge queue commit %s after %s", c.Oid, page.EndCursor)
		}
		checks = append(checks, next.StatusCheckRollup.Contexts.Nodes...)
		page = next.StatusCheckRollup.Contexts.PageInfo
	}
	return checks, nil
}

// checkContext is one check run or commit status on a merge queue's commit.
type checkContext struct {
	Type       string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	CheckSuite *struct {
		WorkflowRun *struct {
			URL string `json:"url"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"targetUrl"`
}
