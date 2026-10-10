package delivery

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTheWaitingSQLAgreesWithTheShippingRule: ListPullRequestsWaitingAt states in SQL when a
// deploy ships a pull request, and FirstShippingApply states it in Go; nothing else keeps the two
// in step. Over one fixture holding every case - a deploy on main before a merge and after it, at
// a merge's own instant, a deploy on a feature branch, a failed and a cancelled deploy, a deploy
// that finishes only after the instant asked about, and pull requests merged at that instant or in
// another repository - the SQL's waiting set, each with the run that ships it, must equal the set
// the Go rule derives from the runs, their jobs and the pull requests the handler reads.
func TestTheWaitingSQLAgreesWithTheShippingRule(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const repo, job = "acme/widgets", "widgets-release / widgets-release"
	at := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
	day := func(d, h int) time.Time { return time.Date(2024, 3, d, h, 0, 0, 0, time.UTC) }

	// Nothing on this branch records a run's branch, so the feature-branch deploy (302) counts as
	// a deploy for both rules; it is here so a branch rule added to one is held to the other.
	runs := []struct {
		id         int64
		head, done time.Time
		result     DeliveryJobConclusion
	}{
		{300, day(1, 0), day(1, 1), DeliveryJobConclusionSuccess},     // main, before every later merge
		{301, day(3, 0), day(3, 1), DeliveryJobConclusionSuccess},     // main, after #403's merge, done before at
		{302, day(5, 0), day(5, 1), DeliveryJobConclusionSuccess},     // a feature branch
		{303, day(6, 0), day(11, 0), DeliveryJobConclusionSuccess},    // main, done only after at
		{304, day(8, 0), day(8, 1), DeliveryJobConclusionFailure},     // failed
		{305, day(8, 12), day(8, 13), DeliveryJobConclusionCancelled}, // cancelled
	}
	for _, run := range runs {
		started, done := run.head.Add(time.Minute), run.done
		if err := UpsertRun(ctx, pool, DeliveryRun{
			Repo: repo, RunID: run.id, Kind: DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", run.id),
			HeadCommitAt: run.head, StartedAt: started, CompletedAt: &done,
			URL: fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", run.id),
		}); err != nil {
			t.Fatalf("seed run %d: %v", run.id, err)
		}
		if err := UpsertRunJobs(ctx, pool, repo, run.id, []DeliveryRunJob{
			{Repo: repo, RunID: run.id, Name: job, StartedAt: &started, CompletedAt: &done, Conclusion: new(run.result)},
		}); err != nil {
			t.Fatalf("seed run %d's job: %v", run.id, err)
		}
	}
	prs := []struct {
		repo   string
		number int
		merged time.Time
	}{
		{repo, 401, time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)}, // shipped by 300
		{repo, 402, day(1, 0)},         // merged at 300's head commit: shipped by it, the boundary inclusive
		{repo, 403, day(2, 0)},         // shipped by 301
		{repo, 404, day(5, 0)},         // merged at 302's head, the newest head shipped by at: shipped by it (no branch is recorded)
		{repo, 405, day(6, 0)},         // merged at 303's head: waiting at at, and 303 ships it after, the boundary inclusive
		{repo, 406, day(7, 0)},         // waiting with nothing to ship it: 303 came before it, 304 failed, 305 was cancelled
		{repo, 407, at},                // merged at at itself: not before it
		{"acme/other", 408, day(7, 0)}, // another repository: nothing tracks its deploys
	}
	for _, pr := range prs {
		merged := pr.merged
		created := merged.Add(-time.Hour)
		if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
			Repo: pr.repo, Number: pr.number, Title: "feat: a widget", URL: fmt.Sprintf("https://github.com/%s/pull/%d", pr.repo, pr.number),
			Author: "octocat", CreatedAt: &created, MergedAt: &merged,
		}); err != nil {
			t.Fatalf("seed %s#%d: %v", pr.repo, pr.number, err)
		}
	}

	// The SQL's answer.
	waiting, err := ListPullRequestsWaitingAt(ctx, pool, repo, job, at)
	if err != nil {
		t.Fatalf("ListPullRequestsWaitingAt: %v", err)
	}
	sqlSet := map[int]string{}
	for _, w := range waiting {
		sqlSet[w.Number] = shipment(w.DeployRun, w.DeployedAt)
	}

	// The Go rule's, from the same reads the timeline handler makes, over all of time.
	epoch := time.Unix(0, 0)
	stored, err := ListPullRequestsInWindow(ctx, pool, epoch, at)
	if err != nil {
		t.Fatalf("ListPullRequestsInWindow: %v", err)
	}
	deployRuns, err := ListRuns(ctx, pool, repo, DeliveryRunKindDeploy, epoch)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	ids := make([]int64, len(deployRuns))
	for i, run := range deployRuns {
		ids[i] = run.RunID
	}
	jobs, err := ListRunJobsForRuns(ctx, pool, repo, ids)
	if err != nil {
		t.Fatalf("ListRunJobsForRuns: %v", err)
	}
	applies := ProductionApplies(deployRuns, jobs, job)
	goSet := map[int]string{}
	for _, pr := range stored {
		if ComputeDeployedStatus(pr, repo, nil) == DeployedStatusNotTracked {
			continue
		}
		apply := ContainingRun(pr, repo, applies)
		if apply != nil && apply.CompletedAt.Before(at) {
			continue // shipped by at
		}
		if apply == nil {
			goSet[pr.Number] = shipment(nil, nil)
		} else {
			goSet[pr.Number] = shipment(&apply.RunID, &apply.CompletedAt)
		}
	}

	if !maps.Equal(sqlSet, goSet) {
		t.Fatalf("waiting at %s:\n  SQL (ListPullRequestsWaitingAt): %s\n  Go (FirstShippingApply):        %s",
			at.Format(time.RFC3339), describeWaiting(sqlSet), describeWaiting(goSet))
	}
	// The fixture is only a check while it holds both kinds of waiter and rules the rest out.
	if goSet[405] != shipment(new(int64(303)), new(day(11, 0))) || goSet[406] != shipment(nil, nil) {
		t.Fatalf("waiting at %s = %s; want #405 shipping with run 303 after it and #406 with nothing to ship it",
			at.Format(time.RFC3339), describeWaiting(goSet))
	}
	for _, number := range []int{401, 402, 403, 404, 407, 408} {
		if _, ok := goSet[number]; ok {
			t.Fatalf("#%d is waiting at %s; want it shipped, merged at that instant, or untracked", number, at.Format(time.RFC3339))
		}
	}
}

func shipment(run *int64, at *time.Time) string {
	if run == nil {
		return "no deploy yet"
	}
	return fmt.Sprintf("run %d at %s", *run, at.UTC().Format(time.RFC3339))
}

func describeWaiting(set map[int]string) string {
	numbers := slices.Sorted(maps.Keys(set))
	parts := make([]string, len(numbers))
	for i, number := range numbers {
		parts[i] = fmt.Sprintf("#%d (%s)", number, set[number])
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
