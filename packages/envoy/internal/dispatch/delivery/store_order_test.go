package delivery

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The live Delivery hub hashes each run and pull request it sends, so a read that answers rows
// tied on its order in another order makes it send them again unchanged. Production's reads flip
// between a sequential scan, which answers a table's rows as they lie (an update writes a row's
// new version after the rest), and an index scan, which answers the index's order. Each test here
// writes tied rows out of the order the read promises, reads under both plans, writes one row
// again as it was, and reads again: every read must answer the one order.

// plannedPools is a pool on pool's database for each storetest plan, in storetest.Plans' order.
func plannedPools(t *testing.T, pool *store.Pool) []*store.Pool {
	t.Helper()
	pools := make([]*store.Pool, len(storetest.Plans))
	for i, plan := range storetest.Plans {
		pools[i] = storetest.Planned(t, pool, plan)
	}
	return pools
}

// TestARunsJobsComeBackInOneOrder: a run's jobs come back by when each started, a job that never
// started last, then by name, from both job reads.
func TestARunsJobsComeBackInOneOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	earlier := started.Add(-time.Minute)
	done := started.Add(10 * time.Minute)
	failure, skipped := DeliveryJobConclusionFailure, DeliveryJobConclusionSkipped
	runs := []int64{810, 811}
	listed := map[string]DeliveryRunJob{}
	for _, job := range []DeliveryRunJob{
		{Name: "test / unit", StartedAt: &started, CompletedAt: &done, Conclusion: &failure},
		{Name: "notify-slack", Conclusion: &skipped},
		{Name: "deploy", StartedAt: &started, CompletedAt: &done, Conclusion: &failure},
		{Name: "setup", StartedAt: &earlier, CompletedAt: &done, Conclusion: &failure},
		{Name: "build", StartedAt: &started, CompletedAt: &done, Conclusion: &failure},
	} {
		listed[job.Name] = job
	}
	// GitHub's listing order, which is neither the start order nor the name order.
	listing := []string{"test / unit", "notify-slack", "deploy", "setup", "build"}
	write := func(runID int64, names []string) {
		t.Helper()
		jobs := make([]DeliveryRunJob, len(names))
		for i, name := range names {
			jobs[i] = listed[name]
			jobs[i].Repo, jobs[i].RunID = "acme/widgets", runID
		}
		if err := UpsertRunJobs(ctx, pool, "acme/widgets", runID, jobs, true); err != nil {
			t.Fatalf("write run %d's jobs %v: %v", runID, names, err)
		}
	}
	for _, id := range runs {
		if err := UpsertRun(ctx, pool, changeTestRun(id, DeliveryRunKindDeploy, earlier, earlier)); err != nil {
			t.Fatalf("UpsertRun %d: %v", id, err)
		}
		write(id, listing)
	}
	want := []string{"setup", "build", "deploy", "test / unit", "notify-slack"}
	names := func(jobs []DeliveryRunJob) []string {
		out := make([]string, len(jobs))
		for i, job := range jobs {
			out[i] = job.Name
		}
		return out
	}
	pools := plannedPools(t, pool)
	check := func(when string) {
		t.Helper()
		for i, planned := range pools {
			plan := storetest.Plans[i]
			for _, id := range runs {
				jobs, err := ListRunJobs(ctx, planned, "acme/widgets", id)
				if err != nil {
					t.Fatalf("ListRunJobs %d: %v", id, err)
				}
				if got := names(jobs); !slices.Equal(got, want) {
					t.Errorf("%s, ListRunJobs(%d) under a %s = %q, want %q", when, id, plan, got, want)
				}
			}
			byRun, err := ListRunJobsForRuns(ctx, planned, "acme/widgets", runs)
			if err != nil {
				t.Fatalf("ListRunJobsForRuns: %v", err)
			}
			for _, id := range runs {
				if got := names(byRun[id]); !slices.Equal(got, want) {
					t.Errorf("%s, ListRunJobsForRuns' run %d under a %s = %q, want %q", when, id, plan, got, want)
				}
			}
		}
	}
	check("as written")
	// A later listing writes one job again as it was.
	write(810, []string{"test / unit"})
	write(811, []string{"deploy"})
	check("after a job was written again unchanged")
}

// TestPullRequestsMergedTogetherComeBackInOneOrder: both pull request reads answer by merge time,
// then by id ("owner/repo#N" in byte order, the order applyDelta.ts's ORDERS.prs keeps), which
// decides each run's prs and the waiting list.
func TestPullRequestsMergedTogetherComeBackInOneOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	merged := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	earlier := merged.Add(-time.Hour)
	at := merged.Add(time.Hour)
	other := changeTestPR(5, merged, "feat: gadgets")
	other.Repo, other.URL = "acme/gadgets", "https://github.com/acme/gadgets/pull/5"
	// Written neither in merge order nor in id order.
	prs := []DeliveryPullRequest{
		changeTestPR(7, merged, "feat: seven"), changeTestPR(12, merged, "feat: twelve"), other,
		changeTestPR(3, merged, "feat: three"), changeTestPR(20, earlier, "feat: twenty"),
	}
	for _, pr := range prs {
		if err := UpsertPullRequest(ctx, pool, pr); err != nil {
			t.Fatalf("UpsertPullRequest %s#%d: %v", pr.Repo, pr.Number, err)
		}
	}
	wantWindow := []string{"acme/widgets#20", "acme/gadgets#5", "acme/widgets#12", "acme/widgets#3", "acme/widgets#7"}
	wantWaiting := []string{"acme/widgets#20", "acme/widgets#12", "acme/widgets#3", "acme/widgets#7"}
	id := func(pr DeliveryPullRequest) string { return pr.Repo + "#" + strconv.Itoa(pr.Number) }
	pools := plannedPools(t, pool)
	check := func(when string) {
		t.Helper()
		for i, planned := range pools {
			plan := storetest.Plans[i]
			window, err := ListPullRequestsInWindow(ctx, planned, earlier, at)
			if err != nil {
				t.Fatalf("ListPullRequestsInWindow: %v", err)
			}
			got := make([]string, len(window))
			for j, pr := range window {
				got[j] = id(pr)
			}
			if !slices.Equal(got, wantWindow) {
				t.Errorf("%s, ListPullRequestsInWindow under a %s = %q, want %q", when, plan, got, wantWindow)
			}
			waiting, err := ListPullRequestsWaitingAt(ctx, planned, "acme/widgets", Production{JobNames: []string{"widgets-release / widgets-release"}}, at)
			if err != nil {
				t.Fatalf("ListPullRequestsWaitingAt: %v", err)
			}
			got = make([]string, len(waiting))
			for j, w := range waiting {
				got[j] = id(w.DeliveryPullRequest)
			}
			if !slices.Equal(got, wantWaiting) {
				t.Errorf("%s, ListPullRequestsWaitingAt under a %s = %q, want %q", when, plan, got, wantWaiting)
			}
		}
	}
	check("as written")
	// A reconcile pass writes a pull request again as it was.
	if err := UpsertPullRequest(ctx, pool, prs[0]); err != nil {
		t.Fatalf("UpsertPullRequest again: %v", err)
	}
	check("after a pull request was written again unchanged")
}

// TestRunsStartedTogetherComeBackInOneOrder: ListRunsStartedIn answers runs that started in one
// instant by run id, which decides the Pipeline's last failing run of a job.
func TestRunsStartedTogetherComeBackInOneOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	from, to := started.Add(-time.Hour), started.Add(time.Hour)
	ids := []int64{903, 901, 902}
	for _, id := range ids {
		if err := UpsertRun(ctx, pool, changeTestRun(id, DeliveryRunKindDeploy, from, started)); err != nil {
			t.Fatalf("UpsertRun %d: %v", id, err)
		}
	}
	want := []int64{901, 902, 903}
	pools := plannedPools(t, pool)
	check := func(when string) {
		t.Helper()
		for i, planned := range pools {
			runs, err := ListRunsStartedIn(ctx, planned, "acme/widgets", DeliveryRunKindDeploy, "", "", from, to)
			if err != nil {
				t.Fatalf("ListRunsStartedIn: %v", err)
			}
			got := make([]int64, len(runs))
			for j, run := range runs {
				got[j] = run.RunID
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s, ListRunsStartedIn under a %s = %v, want %v", when, storetest.Plans[i], got, want)
			}
		}
	}
	check("as written")
	// A reconcile pass writes a run again as it was.
	if err := UpsertRun(ctx, pool, changeTestRun(901, DeliveryRunKindDeploy, from, started)); err != nil {
		t.Fatalf("UpsertRun 901 again: %v", err)
	}
	check("after a run was written again unchanged")
}
