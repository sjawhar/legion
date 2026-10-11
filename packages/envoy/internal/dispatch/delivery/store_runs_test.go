package delivery

import (
	"slices"
	"testing"
	"time"
)

// TestListRunsStartedInIsHalfOpenAndOrderedByStart seeds four deploy runs started one second
// before from, at from, one second before to and at to, every head commit before from: the
// measures count the middle two, oldest start first. ListRuns, which filters on head_commit_at,
// would return none of them.
func TestListRunsStartedInIsHalfOpenAndOrderedByStart(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	headCommit := from.Add(-time.Hour)
	// Seeded newest start first, so the order the query returns is its own.
	for i, started := range []time.Time{to, to.Add(-time.Second), from, from.Add(-time.Second)} {
		if err := UpsertRun(ctx, pool, DeliveryRun{
			Repo: "acme/widgets", RunID: int64(900 + i), Kind: DeliveryRunKindDeploy, HeadSHA: "abc",
			HeadCommitAt: headCommit, StartedAt: started, URL: "https://github.com/acme/widgets/actions/runs/900",
		}); err != nil {
			t.Fatalf("seed run %d: %v", 900+i, err)
		}
	}
	// A PR-checks run inside the window and a deploy run of another repository are not this
	// query's.
	prNumber := 7
	if err := UpsertRun(ctx, pool, DeliveryRun{
		Repo: "acme/widgets", RunID: 950, Kind: DeliveryRunKindPRChecks, PRNumber: &prNumber, HeadSHA: "def",
		HeadCommitAt: headCommit, StartedAt: from.Add(time.Hour), URL: "https://github.com/acme/widgets/actions/runs/950",
	}); err != nil {
		t.Fatalf("seed PR-checks run: %v", err)
	}
	if err := UpsertRun(ctx, pool, DeliveryRun{
		Repo: "acme/gadgets", RunID: 951, Kind: DeliveryRunKindDeploy, HeadSHA: "fed",
		HeadCommitAt: headCommit, StartedAt: from.Add(time.Hour), URL: "https://github.com/acme/gadgets/actions/runs/951",
	}); err != nil {
		t.Fatalf("seed other repository's run: %v", err)
	}

	runs, err := ListRunsStartedIn(ctx, pool, "acme/widgets", DeliveryRunKindDeploy, "", "", from, to)
	if err != nil {
		t.Fatalf("ListRunsStartedIn: %v", err)
	}
	got := make([]time.Time, len(runs))
	for i, run := range runs {
		got[i] = run.StartedAt.UTC()
	}
	want := []time.Time{from, to.Add(-time.Second)}
	if len(got) != len(want) || !got[0].Equal(want[0]) || !got[1].Equal(want[1]) {
		t.Fatalf("started_at of the runs ListRunsStartedIn returned = %v, want %v", got, want)
	}
	if runs[0].RunID != 902 || runs[1].RunID != 901 {
		t.Fatalf("run ids = %d, %d, want 902, 901", runs[0].RunID, runs[1].RunID)
	}
}

// TestUpsertRunRoundTripsBranchAndEvent writes runs carrying GitHub's head_branch and event and
// reads them back through ListRunsStartedIn's two filters: a non-empty filter matches only that
// value, an empty one matches anything, and a run stored with neither (one the backfill has not
// reached) matches only when both filters are empty.
func TestUpsertRunRoundTripsBranchAndEvent(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	main, push, dispatch := "main", "push", "workflow_dispatch"
	for _, run := range []DeliveryRun{
		{RunID: 960, HeadBranch: &main, Event: &push},
		{RunID: 961, HeadBranch: &main, Event: &dispatch},
		{RunID: 962},
	} {
		run.Repo, run.Kind, run.HeadSHA, run.URL = "acme/widgets", DeliveryRunKindDeploy, "abc", "https://github.com/acme/widgets/actions/runs/960"
		run.HeadCommitAt, run.StartedAt = from, from.Add(time.Duration(run.RunID-959)*time.Minute)
		if err := UpsertRun(ctx, pool, run); err != nil {
			t.Fatalf("seed run %d: %v", run.RunID, err)
		}
	}

	ids := func(branch, event string) []int64 {
		t.Helper()
		runs, err := ListRunsStartedIn(ctx, pool, "acme/widgets", DeliveryRunKindDeploy, branch, event, from, to)
		if err != nil {
			t.Fatalf("ListRunsStartedIn(%q, %q): %v", branch, event, err)
		}
		got := make([]int64, len(runs))
		for i, run := range runs {
			got[i] = run.RunID
		}
		return got
	}
	for _, c := range []struct {
		branch, event string
		want          []int64
	}{
		{"main", "push", []int64{960}},
		{"main", "", []int64{960, 961}},
		{"", "", []int64{960, 961, 962}},
	} {
		if got := ids(c.branch, c.event); !slices.Equal(got, c.want) {
			t.Errorf("ListRunsStartedIn(%q, %q) = %v, want %v", c.branch, c.event, got, c.want)
		}
	}

	runs, err := ListRunsStartedIn(ctx, pool, "acme/widgets", DeliveryRunKindDeploy, "main", "push", from, to)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRunsStartedIn(main, push) = %+v, %v", runs, err)
	}
	if runs[0].HeadBranch == nil || *runs[0].HeadBranch != "main" || runs[0].Event == nil || *runs[0].Event != "push" {
		t.Fatalf("run 960 read back HeadBranch, Event = %v, %v, want main, push", runs[0].HeadBranch, runs[0].Event)
	}
}

// TestListRunsNarrowsContainmentToOneBranch: the head-commit listing containment reads passes
// "main", so a successful deploy on another branch never ships a pull request.
func TestListRunsNarrowsContainmentToOneBranch(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	since := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	main, feature, push := "main", "feature/x", "push"
	for _, run := range []DeliveryRun{
		{RunID: 970, HeadBranch: &main, Event: &push},
		{RunID: 971, HeadBranch: &feature, Event: &push},
		{RunID: 972},
	} {
		run.Repo, run.Kind, run.HeadSHA, run.URL = "acme/widgets", DeliveryRunKindDeploy, "abc", "https://github.com/acme/widgets/actions/runs/970"
		run.HeadCommitAt, run.StartedAt = since.Add(time.Duration(run.RunID-969)*time.Minute), since
		if err := UpsertRun(ctx, pool, run); err != nil {
			t.Fatalf("seed run %d: %v", run.RunID, err)
		}
	}
	for _, c := range []struct {
		branch string
		want   []int64
	}{{"main", []int64{970}}, {"", []int64{970, 971, 972}}} {
		runs, err := ListRuns(ctx, pool, "acme/widgets", DeliveryRunKindDeploy, c.branch, since)
		if err != nil {
			t.Fatalf("ListRuns(%q): %v", c.branch, err)
		}
		var got []int64
		for _, run := range runs {
			if run.RunID >= 970 && run.RunID <= 972 {
				got = append(got, run.RunID)
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("ListRuns(%q) = %v, want %v", c.branch, got, c.want)
		}
	}
}
