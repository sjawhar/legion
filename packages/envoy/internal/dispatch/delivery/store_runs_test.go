package delivery

import (
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
