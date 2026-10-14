package delivery

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// waitingRun is one deploy run a waiting-rule history holds. created is GitHub's created_at, nil
// on a row stored before that column (read as started); head is its head commit's time, which a
// merge queue writes before GitHub records the merge it carries.
type waitingRun struct {
	id                  int64
	branch              string
	head, started, done time.Time
	created             *time.Time
	result              DeliveryJobConclusion
}

// waitingPR is one merged pull request a waiting-rule history holds.
type waitingPR struct {
	repo   string
	number int
	merged time.Time
}

const waitingRepo, waitingJob = "acme/widgets", "widgets-release / widgets-release"

// TestTheWaitingSQLAgreesWithTheShippingRule: ListPullRequestsWaitingAt states in SQL when a
// deploy ships a pull request, and FirstShippingApply states it in Go; nothing else keeps the two
// in step. Over one fixture holding every case - a deploy on main created before a merge and after
// it, created in a merge's own instant, one whose head commit is timed before the merge it ships (a
// merge queue's), a re-run whose start moved past a merge its head does not hold, a row stored
// before created_at, a deploy on a feature branch, a failed and a cancelled deploy, deploys that
// finish only after the instant asked about, and pull requests merged at that instant or in
// another repository - the SQL's waiting set, each with the run that ships it, must equal the set
// the Go rule derives from the runs on main, their jobs and the pull requests the handler reads.
func TestTheWaitingSQLAgreesWithTheShippingRule(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	at := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
	day := func(d, h int) time.Time { return time.Date(2024, 3, d, h, 0, 0, 0, time.UTC) }

	runs := []waitingRun{
		// Stored before created_at: read by its start, day 1 at midnight.
		{id: 300, branch: "main", head: day(1, 0).Add(-20 * time.Minute), started: day(1, 0), done: day(1, 1), result: DeliveryJobConclusionSuccess},
		// A merge queue's: its head commit is timed at 11:00 on day 2, before #409's merge at noon,
		// and GitHub created it on day 3.
		{id: 301, branch: "main", head: day(2, 11), created: new(day(3, 0)), started: day(3, 0), done: day(3, 1), result: DeliveryJobConclusionSuccess},
		{id: 302, branch: "feature/x", head: day(5, 0), created: new(day(5, 0)), started: day(5, 0), done: day(5, 1), result: DeliveryJobConclusionSuccess},
		{id: 303, branch: "main", head: day(6, 0).Add(-30 * time.Minute), created: new(day(6, 0)), started: day(6, 0), done: day(11, 0), result: DeliveryJobConclusionSuccess},
		{id: 304, branch: "main", head: day(8, 0), created: new(day(8, 0)), started: day(8, 0), done: day(8, 1), result: DeliveryJobConclusionFailure},
		{id: 305, branch: "main", head: day(8, 12), created: new(day(8, 12)), started: day(8, 12), done: day(8, 13), result: DeliveryJobConclusionCancelled},
		// A re-run: created on day 9 at midnight, its re-run started at 02:00, done after at.
		{id: 306, branch: "main", head: day(9, 0).Add(-10 * time.Minute), created: new(day(9, 0)), started: day(9, 2), done: day(12, 0), result: DeliveryJobConclusionSuccess},
	}
	prs := []waitingPR{
		{waitingRepo, 401, time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)}, // shipped by 300
		{waitingRepo, 402, day(1, 0)},                                    // merged in 300's start, which stands in for its creation: the boundary is exclusive, so 301 ships it
		{waitingRepo, 403, day(2, 0)},                                    // shipped by 301
		{waitingRepo, 409, day(2, 12)},                                   // after 301's head commit, before its creation: shipped by 301
		{waitingRepo, 404, day(5, 0)},                                    // merged in 302's creation, on a feature branch: waiting, and 303 ships it after
		{waitingRepo, 405, day(6, 0)},                                    // merged in 303's creation: exclusive, so waiting until 306
		{waitingRepo, 406, day(7, 0)},                                    // waiting: 304 failed, 305 was cancelled, 306 ships it after
		{waitingRepo, 410, day(9, 1)},                                    // after 306's creation, before its re-run's start: nothing ships it
		{waitingRepo, 407, at},                                           // merged at at itself: not before it
		{"acme/other", 408, day(7, 0)},                                   // another repository: nothing tracks its deploys
	}
	seedWaitingHistory(t, ctx, pool, runs, prs)
	sqlSet, goSet := waitingSets(t, ctx, pool, at)
	if !maps.Equal(sqlSet, goSet) {
		t.Fatalf("waiting at %s:\n  SQL (ListPullRequestsWaitingAt): %s\n  Go (FirstShippingApply):        %s",
			at.Format(time.RFC3339), describeWaiting(sqlSet), describeWaiting(goSet))
	}
	// The fixture is only a check while it holds every kind of waiter and rules the rest out.
	want := map[int]string{
		404: shipment(new(int64(303)), new(day(11, 0))),
		405: shipment(new(int64(306)), new(day(12, 0))),
		406: shipment(new(int64(306)), new(day(12, 0))),
		410: shipment(nil, nil),
	}
	if !maps.Equal(goSet, want) {
		t.Fatalf("waiting at %s = %s; want %s", at.Format(time.RFC3339), describeWaiting(goSet), describeWaiting(want))
	}
}

// TestTheWaitingSQLAgreesWithTheShippingRuleOverMergeQueueHistories holds the same agreement over
// generated histories shaped as a merge queue makes them: each run's head commit is timed up to an
// hour before GitHub created it, a pull request lands in that gap, in a run's own creation second,
// or anywhere else, some runs are re-runs whose start moved 14 to 54 minutes past their creation,
// and some are rows stored before created_at. Each round's seed is in its failure.
func TestTheWaitingSQLAgreesWithTheShippingRuleOverMergeQueueHistories(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	base := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	for seed := range uint64(60) {
		random := rand.New(rand.NewPCG(seed, 730))
		minutes := func(lo, hi int) time.Duration { return time.Duration(lo+random.IntN(hi-lo+1)) * time.Minute }
		var runs []waitingRun
		var gaps [][2]time.Time
		for i := range 4 + random.IntN(8) {
			created := base.Add(minutes(0, 14*24*60))
			run := waitingRun{
				id: int64(500 + i), branch: "main", head: created.Add(-minutes(0, 60)),
				created: new(created), started: created.Add(time.Duration(random.IntN(30)) * time.Second),
				result: DeliveryJobConclusionSuccess,
			}
			switch random.IntN(10) {
			case 0:
				run.branch = "feature/x"
			case 1:
				run.result = DeliveryJobConclusionFailure
			case 2:
				run.result = DeliveryJobConclusionCancelled
			case 3:
				run.started = created.Add(minutes(14, 54)) // a re-run
			case 4:
				run.created, run.started = nil, created // stored before created_at
			}
			// Distinct per run: the two rules order applies by completion alone, and break a tie
			// each its own way.
			run.done = run.started.Add(minutes(5, 6*60) + time.Duration(i)*time.Millisecond)
			runs = append(runs, run)
			gaps = append(gaps, [2]time.Time{run.head, created})
		}
		var prs []waitingPR
		for i := range 6 + random.IntN(10) {
			var merged time.Time
			switch gap := gaps[random.IntN(len(gaps))]; random.IntN(3) {
			case 0: // between a run's head commit and its creation, as a merge queue leaves it
				merged = gap[0].Add(time.Duration(random.Int64N(int64(gap[1].Sub(gap[0])) + 1))).Truncate(time.Second)
			case 1: // in a run's own creation second
				merged = gap[1]
			default:
				merged = base.Add(minutes(0, 14*24*60))
			}
			prs = append(prs, waitingPR{waitingRepo, 600 + i, merged})
		}
		at := base.Add(minutes(24*60, 14*24*60))
		seedWaitingHistory(t, ctx, pool, runs, prs)
		sqlSet, goSet := waitingSets(t, ctx, pool, at)
		if !maps.Equal(sqlSet, goSet) {
			t.Fatalf("seed %d, waiting at %s:\n  SQL (ListPullRequestsWaitingAt): %s\n  Go (FirstShippingApply):        %s",
				seed, at.Format(time.RFC3339), describeWaiting(sqlSet), describeWaiting(goSet))
		}
	}
}

// seedWaitingHistory replaces the stored runs, their jobs and the pull requests with the history's.
func seedWaitingHistory(t *testing.T, ctx context.Context, pool *store.Pool, runs []waitingRun, prs []waitingPR) {
	t.Helper()
	for _, table := range []string{"delivery_run_jobs", "delivery_runs", "delivery_pull_requests"} {
		if _, err := pool.Exec(ctx, "delete from "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	for _, run := range runs {
		started, done := run.started, run.done
		if err := UpsertRun(ctx, pool, DeliveryRun{
			Repo: waitingRepo, RunID: run.id, Kind: DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", run.id),
			HeadCommitAt: run.head, StartedAt: started, CreatedAt: run.created, CompletedAt: &done,
			URL:        fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", run.id),
			HeadBranch: new(run.branch), Event: new("push"),
		}); err != nil {
			t.Fatalf("seed run %d: %v", run.id, err)
		}
		if err := UpsertRunJobs(ctx, pool, waitingRepo, run.id, []DeliveryRunJob{
			{Repo: waitingRepo, RunID: run.id, Name: waitingJob, StartedAt: &started, CompletedAt: &done, Conclusion: new(run.result)},
		}, true); err != nil {
			t.Fatalf("seed run %d's job: %v", run.id, err)
		}
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
}

// waitingSets is the stored history's waiting set at at as the SQL states it and as the Go rule
// derives it from the reads the timeline handler makes, over all of time, each pull request with
// the run that ships it after at.
func waitingSets(t *testing.T, ctx context.Context, pool *store.Pool, at time.Time) (sqlSet, goSet map[int]string) {
	t.Helper()
	waiting, err := ListPullRequestsWaitingAt(ctx, pool, waitingRepo, waitingJob, at)
	if err != nil {
		t.Fatalf("ListPullRequestsWaitingAt: %v", err)
	}
	sqlSet = map[int]string{}
	for _, w := range waiting {
		sqlSet[w.Number] = shipment(w.DeployRun, w.DeployedAt)
	}

	epoch := time.Unix(0, 0)
	stored, err := ListPullRequestsInWindow(ctx, pool, epoch, at)
	if err != nil {
		t.Fatalf("ListPullRequestsInWindow: %v", err)
	}
	deployRuns, err := ListRuns(ctx, pool, waitingRepo, DeliveryRunKindDeploy, "main", epoch)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	ids := make([]int64, len(deployRuns))
	for i, run := range deployRuns {
		ids[i] = run.RunID
	}
	jobs, err := ListRunJobsForRuns(ctx, pool, waitingRepo, ids)
	if err != nil {
		t.Fatalf("ListRunJobsForRuns: %v", err)
	}
	applies, _ := ProductionApplies(deployRuns, jobs, waitingJob)
	goSet = map[int]string{}
	for _, pr := range stored {
		if ComputeDeployedStatus(pr, waitingRepo, nil) == DeployedStatusNotTracked {
			continue
		}
		apply := ContainingRun(pr, waitingRepo, applies)
		if apply != nil && apply.CompletedAt.Before(at) {
			continue // shipped by at
		}
		if apply == nil {
			goSet[pr.Number] = shipment(nil, nil)
		} else {
			goSet[pr.Number] = shipment(&apply.RunID, &apply.CompletedAt)
		}
	}
	return sqlSet, goSet
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
