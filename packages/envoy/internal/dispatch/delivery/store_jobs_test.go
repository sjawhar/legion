package delivery

import (
	"net/http"
	"testing"
	"time"
)

// TestJobsReadBeforeARunEndsLeaveItForTheBackfill: jobs read while the run was still running leave
// the run on the backfill's list, so the rest of its jobs are read; jobs read after it ended take
// it off.
func TestJobsReadBeforeARunEndsLeaveItForTheBackfill(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(800, DeliveryRunKindDeploy, started, started)); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
	build := DeliveryRunJob{Repo: "acme/widgets", RunID: 800, Name: "build", StartedAt: &started}
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 800, []DeliveryRunJob{build}, false); err != nil {
		t.Fatalf("UpsertRunJobs while running: %v", err)
	}
	skip, err := ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{800})
	if err != nil {
		t.Fatalf("ListRunIDsSkippingJobs: %v", err)
	}
	if skip[800] {
		t.Fatal("a run whose jobs were read while it ran is skipped, want it listed")
	}
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 800, []DeliveryRunJob{build}, true); err != nil {
		t.Fatalf("UpsertRunJobs once ended: %v", err)
	}
	skip, err = ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{800})
	if err != nil {
		t.Fatalf("ListRunIDsSkippingJobs: %v", err)
	}
	if !skip[800] {
		t.Fatal("a run whose jobs were read after it ended is listed, want it skipped")
	}
}

// TestALateReadNeverUndoesAFinishedJob: a read taken while the run ran, landing after one taken
// once it ended, keeps every finished job and the run's completeness, and logs no change.
func TestALateReadNeverUndoesAFinishedJob(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(801, DeliveryRunKindDeploy, started, started)); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
	finished := started.Add(20 * time.Minute)
	success := DeliveryJobConclusionSuccess
	production := "widgets-release / widgets-release"
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 801, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 801, Name: "build", StartedAt: &started, CompletedAt: &finished, Conclusion: &success},
		{Repo: "acme/widgets", RunID: 801, Name: production, StartedAt: &started, CompletedAt: &finished, Conclusion: &success},
	}, true); err != nil {
		t.Fatalf("UpsertRunJobs finished: %v", err)
	}
	before := latestChange(t, ctx, pool)

	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 801, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 801, Name: production, StartedAt: &started},
	}, false); err != nil {
		t.Fatalf("UpsertRunJobs late: %v", err)
	}
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 801)
	if err != nil {
		t.Fatalf("ListRunJobs: %v", err)
	}
	for _, job := range jobs {
		if job.CompletedAt == nil || !job.CompletedAt.Equal(finished) || job.Conclusion == nil || *job.Conclusion != success {
			t.Fatalf("job %q after the late read = %+v, want it still finished with success at %v", job.Name, job, finished)
		}
	}
	var complete bool
	if err := pool.QueryRow(ctx, `select jobs_complete from delivery_runs where repo = 'acme/widgets' and run_id = 801`).Scan(&complete); err != nil {
		t.Fatalf("read jobs_complete: %v", err)
	}
	if !complete {
		t.Fatal("jobs_complete went back to false after a late read")
	}
	if changes := changesAfter(t, ctx, pool, before); len(changes) != 0 {
		t.Fatalf("a late read that changed nothing logged %+v", changes)
	}
}

// TestReconcilePrunesChangesOlderThanADay: a successful pass deletes the changes recorded more than
// ChangeRetention ago and keeps the rest.
func TestReconcilePrunesChangesOlderThanADay(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	if _, err := pool.Exec(ctx, `delete from delivery_changes`); err != nil {
		t.Fatalf("clear the change log: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into delivery_changes (at, kind) values (now() - interval '25 hours', 'settings'), (now() - interval '1 hour', 'settings')
	`); err != nil {
		t.Fatalf("seed changes: %v", err)
	}

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	for _, workflow := range []string{"deploy.yml", "pr-checks.yml"} {
		fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2F"+workflow+"/runs", func(w http.ResponseWriter, r *http.Request) {
			mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
		})
	}
	NewReconcile(pool, fake.newTestClient()).runOnce(ctx)

	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastError != nil {
		t.Fatalf("the pass failed: %v, last_error %v", err, settings.LastError)
	}
	var old, recent int
	if err := pool.QueryRow(ctx, `
		select count(*) filter (where at < now() - interval '24 hours'), count(*) filter (where at >= now() - interval '24 hours')
		from delivery_changes where kind = 'settings'
	`).Scan(&old, &recent); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if old != 0 || recent != 1 {
		t.Fatalf("after a pass: %d changes older than a day and %d newer, want 0 and 1", old, recent)
	}
}
