package delivery

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 800)
	if err != nil {
		t.Fatalf("ListRunJobs while running: %v", err)
	}
	if len(jobs) != 1 || jobs[0].CompletedAt != nil {
		t.Fatalf("jobs while running = %+v, want build without completed_at", jobs)
	}
	skip, err := ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{800})
	if err != nil {
		t.Fatalf("ListRunIDsSkippingJobs: %v", err)
	}
	if skip[800] {
		t.Fatal("a run whose jobs were read while it ran is skipped, want it listed")
	}
	finished := started.Add(4 * time.Minute)
	success := DeliveryJobConclusionSuccess
	build.CompletedAt, build.Conclusion = &finished, &success
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 800, []DeliveryRunJob{build}, true); err != nil {
		t.Fatalf("UpsertRunJobs once ended: %v", err)
	}
	jobs, err = ListRunJobs(ctx, pool, "acme/widgets", 800)
	if err != nil {
		t.Fatalf("ListRunJobs once ended: %v", err)
	}
	if len(jobs) != 1 || jobs[0].CompletedAt == nil || !jobs[0].CompletedAt.Equal(finished) {
		t.Fatalf("jobs once ended = %+v, want build completed at %v", jobs, finished)
	}
	skip, err = ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{800})
	if err != nil {
		t.Fatalf("ListRunIDsSkippingJobs: %v", err)
	}
	if !skip[800] {
		t.Fatal("a run whose jobs were read after it ended is listed, want it skipped")
	}
}

// TestACompleteJobsWriteRefusesARunningJob: complete is the monotonic run-level flag, so a caller
// cannot set it from a list that still holds a job without its completion.
func TestACompleteJobsWriteRefusesARunningJob(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(802, DeliveryRunKindDeploy, started, started)); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
	err := UpsertRunJobs(ctx, pool, "acme/widgets", 802, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 802, Name: "widgets-release / widgets-release", StartedAt: &started},
	}, true)
	if err == nil || !strings.Contains(err.Error(), "widgets-release / widgets-release") {
		t.Fatalf("UpsertRunJobs complete with a running job = %v, want an error naming the job", err)
	}
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 802)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("jobs after refused complete write = %+v, %v; want none", jobs, err)
	}
	skip, err := ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{802})
	if err != nil || skip[802] {
		t.Fatalf("backfill skips run after refused complete write (%v), want it listed", err)
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

// TestAListingThatRepeatsAJobNameStoresItAsWritingEachInTurn: two jobs of one run may share a name,
// and the one statement that writes a run's jobs folds them by the rule a write follows, so a
// finished one wins over an unfinished one whichever GitHub listed first.
func TestAListingThatRepeatsAJobNameStoresItAsWritingEachInTurn(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(804, DeliveryRunKindDeploy, started, started)); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
	finished := started.Add(5 * time.Minute)
	success := DeliveryJobConclusionSuccess
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 804, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 804, Name: "test", StartedAt: &started, CompletedAt: &finished, Conclusion: &success},
		{Repo: "acme/widgets", RunID: 804, Name: "test", StartedAt: &started},
	}, false); err != nil {
		t.Fatalf("UpsertRunJobs with a repeated name: %v", err)
	}
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 804)
	if err != nil {
		t.Fatalf("ListRunJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].CompletedAt == nil || !jobs[0].CompletedAt.Equal(finished) {
		t.Fatalf("jobs after a repeated name = %+v, want one test job completed at %v", jobs, finished)
	}
}

// TestAJobsWriteAndARunWriteOfOneRunNeverDeadlock: a run's jobs and the run's own completion are
// written by separate consumers (JobIntake and Intake) and race for the same run. A single-statement
// run write takes the run row's lock and then, in delivery_runs' change trigger, the change log's
// advisory lock (0100); the jobs write must take the two in that order too. Here the change log's
// lock is held while the jobs write starts and waits for it, then the run write starts and waits
// behind it, and then the lock is let go: in the opposite order the jobs write would hold the
// change log's lock while it waits for the row the run write holds, and Postgres refuses one of
// them as a deadlock (40P01).
func TestAJobsWriteAndARunWriteOfOneRunNeverDeadlock(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	run := changeTestRun(803, DeliveryRunKindDeploy, started, started)
	if err := UpsertRun(ctx, pool, run); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
	finished := started.Add(10 * time.Minute)
	success := DeliveryJobConclusionSuccess
	jobs := []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 803, Name: "build", StartedAt: &started, CompletedAt: &finished, Conclusion: &success},
		{Repo: "acme/widgets", RunID: 803, Name: "widgets-release / widgets-release", StartedAt: &started, CompletedAt: &finished, Conclusion: &success},
	}
	runSuccess := DeliveryRunConclusionSuccess
	run.CompletedAt, run.Conclusion = &finished, &runSuccess

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the gate: %v", err)
	}
	defer func() { _ = gate.Rollback(ctx) }()
	if _, err := gate.Exec(ctx, `select pg_advisory_xact_lock(6900, 0)`); err != nil {
		t.Fatalf("take the change log's lock: %v", err)
	}
	var gatePID int
	if err := gate.QueryRow(ctx, `select pg_backend_pid()`).Scan(&gatePID); err != nil {
		t.Fatalf("read the gate's pid: %v", err)
	}
	jobsDone := make(chan error, 1)
	go func() { jobsDone <- UpsertRunJobs(ctx, pool, "acme/widgets", 803, jobs, true) }()
	jobsPID := waitForBlocked(t, ctx, pool, []int{gatePID}, []int{})
	runDone := make(chan error, 1)
	go func() { runDone <- UpsertRun(ctx, pool, run) }()
	waitForBlocked(t, ctx, pool, []int{gatePID, jobsPID}, []int{jobsPID})
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("release the change log's lock: %v", err)
	}
	if err := <-jobsDone; err != nil {
		t.Errorf("UpsertRunJobs: %v", err)
	}
	if err := <-runDone; err != nil {
		t.Errorf("UpsertRun: %v", err)
	}
	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 803)
	if err != nil || len(stored) != 2 {
		t.Fatalf("jobs after both writes = %+v, %v; want both", stored, err)
	}
	var completed *time.Time
	var complete bool
	if err := pool.QueryRow(ctx, `select completed_at, jobs_complete from delivery_runs where repo = 'acme/widgets' and run_id = 803`).Scan(&completed, &complete); err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if completed == nil || !completed.Equal(finished) || !complete {
		t.Fatalf("run after both writes: completed_at %v, jobs_complete %v; want %v and true", completed, complete, finished)
	}
}

// waitForBlocked returns the pid of a backend, not one of holders or except, that waits for a lock
// one of holders holds.
func waitForBlocked(t *testing.T, ctx context.Context, pool *store.Pool, holders, except []int) int {
	t.Helper()
	for {
		var pid int
		err := pool.QueryRow(ctx, `
			select pid from pg_stat_activity
			where datname = current_database() and pid <> all($1::int[]) and pid <> all($2::int[])
				and pg_blocking_pids(pid) && $1::int[]
			limit 1
		`, holders, except).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read lock waits: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no backend waited for a lock held by %v: %v", holders, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
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
