package delivery

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// backfillRun is one deploy run the backfill fake serves, created at created.
func backfillRun(id int64, created time.Time) map[string]any {
	stamp := created.UTC().Format(time.RFC3339)
	return map[string]any{
		"id": id, "head_sha": fmt.Sprintf("sha%d", id), "status": "completed", "conclusion": "success",
		"html_url":   fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", id),
		"created_at": stamp, "run_started_at": stamp, "updated_at": created.Add(5 * time.Minute).UTC().Format(time.RFC3339),
		"head_branch": "main", "event": "push",
		"head_commit":   map[string]any{"timestamp": stamp},
		"pull_requests": []any{},
	}
}

// backfillFake is GitHub's run listing and jobs endpoint for the deploy workflow, as GitHub serves
// them: a created=A..B window's runs newest first, perPage at a time keyed on the request's page
// (GitHub's page size, not the client's: workflowRunsPageSize is a constant a test cannot shrink),
// with the window's whole count as total_count. It records every listing request's window and page
// and every jobs request, and refuses a listing request with a rate limit while limit says so.
type backfillFake struct {
	t       *testing.T
	runs    []map[string]any
	perPage int

	mu       sync.Mutex
	listings []string
	jobs     []int64
	limit    func(created string, page int) bool
}

func newBackfillFake(t *testing.T, runs []map[string]any, perPage int) (*backfillFake, *fakeGitHub) {
	t.Helper()
	fake := &backfillFake{t: t, runs: runs, perPage: perPage}
	github := newFakeGitHub(t)
	github.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", fake.list)
	github.handle("GET /repos/acme/widgets/actions/runs/{id}/jobs", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			t.Errorf("jobs request for run %q: %v", r.PathValue("id"), err)
		}
		fake.mu.Lock()
		fake.jobs = append(fake.jobs, id)
		fake.mu.Unlock()
		mustEncode(t, w, map[string]any{"total_count": 1, "jobs": []map[string]any{{
			"name": "widgets-release / widgets-release", "conclusion": "success",
			"started_at": "2026-09-01T00:00:00Z", "completed_at": "2026-09-01T00:05:00Z",
		}}})
	})
	return fake, github
}

func (f *backfillFake) list(w http.ResponseWriter, r *http.Request) {
	created := r.URL.Query().Get("created")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	f.mu.Lock()
	f.listings = append(f.listings, fmt.Sprintf("%s page %d", created, page))
	limited := f.limit != nil && f.limit(created, page)
	f.mu.Unlock()
	if limited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		return
	}
	since, until := parseCreatedWindow(f.t, created)
	var inWindow []map[string]any
	for _, run := range f.runs {
		at, _ := time.Parse(time.RFC3339, run["created_at"].(string))
		if !at.Before(since) && !at.After(until) {
			inWindow = append(inWindow, run)
		}
	}
	// Newest first, as GitHub lists a window.
	slices.SortFunc(inWindow, func(a, b map[string]any) int {
		return -compareStrings(a["created_at"].(string), b["created_at"].(string))
	})
	start := min((page-1)*f.perPage, len(inWindow))
	end := min(start+f.perPage, len(inWindow))
	mustEncode(f.t, w, map[string]any{"total_count": len(inWindow), "workflow_runs": inWindow[start:end]})
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (f *backfillFake) listingCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.listings)
}

func (f *backfillFake) jobCalls() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.jobs)
}

// hourWindow is the created=A..B qualifier backfillRuns sends for the hour starting at start.
func hourWindow(start time.Time) string {
	return start.UTC().Format(time.RFC3339) + ".." + start.Add(time.Hour-time.Second).UTC().Format(time.RFC3339)
}

const backfillWorkflow = ".github/workflows/deploy.yml"

func deployBackfill(t *testing.T, reconcile *Reconcile) (through, beganAt time.Time, found bool) {
	t.Helper()
	row, err := readBackfillProgress(t.Context(), reconcile.pool, backfillStep(DeliveryRunKindDeploy), runsProgressScope("acme/widgets", backfillWorkflow))
	if err != nil {
		t.Fatalf("read backfill progress: %v", err)
	}
	return row.through, row.beganAt, row.found
}

func storedRunIDs(t *testing.T, reconcile *Reconcile) []int64 {
	t.Helper()
	rows, err := reconcile.pool.Query(t.Context(), `select run_id from delivery_runs where repo = 'acme/widgets' order by run_id`)
	if err != nil {
		t.Fatalf("list stored runs: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan run id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// startBackfill records the backfill step as begun over [through, beganAt], as backfillRuns does
// on its first pass, for a test that needs a shorter walk than the 28-day window.
func startBackfill(t *testing.T, reconcile *Reconcile, through, beganAt time.Time) {
	t.Helper()
	if err := StartBackfillProgress(t.Context(), reconcile.pool, backfillStep(DeliveryRunKindDeploy), runsProgressScope("acme/widgets", backfillWorkflow), through, beganAt); err != nil {
		t.Fatalf("start backfill: %v", err)
	}
}

func (r *Reconcile) backfillDeploys(t *testing.T, now time.Time) {
	t.Helper()
	r.backfillRuns(t.Context(), "acme", "widgets", "acme/widgets", backfillWorkflow, DeliveryRunKindDeploy, now)
}

// TestBackfillRunsAdvancesOneFinishedHourAtATime: GitHub lists a window newest first, so the
// backfill moves its progress only once a whole hour is stored. Hour B's second page answers a
// rate limit on the first pass: hour A is stored and recorded, none of hour B is, and the next
// pass lists hour B again in full, both pages, before hour C. A third pass, the step finished,
// lists nothing. The backfill's failure stays out of the reconcile's health state.
func TestBackfillRunsAdvancesOneFinishedHourAtATime(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	hourA := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	hourB, hourC := hourA.Add(time.Hour), hourA.Add(2*time.Hour)
	var runs []map[string]any
	for i, at := range []time.Time{hourA.Add(5 * time.Minute), hourA.Add(20 * time.Minute), hourA.Add(40 * time.Minute)} {
		runs = append(runs, backfillRun(int64(100+i), at))
	}
	for i, at := range []time.Time{hourB.Add(5 * time.Minute), hourB.Add(15 * time.Minute), hourB.Add(30 * time.Minute), hourB.Add(50 * time.Minute)} {
		runs = append(runs, backfillRun(int64(200+i), at))
	}
	for i, at := range []time.Time{hourC.Add(10 * time.Minute), hourC.Add(45 * time.Minute)} {
		runs = append(runs, backfillRun(int64(300+i), at))
	}
	fake, github := newBackfillFake(t, runs, 2)
	fake.limit = func(created string, page int) bool { return created == hourWindow(hourB) && page == 2 }
	reconcile := NewReconcile(pool, github.newTestClient())
	now := hourA.Add(3 * time.Hour)
	startBackfill(t, reconcile, hourA, now)

	reconcile.backfillDeploys(t, now)
	through, beganAt, _ := deployBackfill(t, reconcile)
	if !through.Equal(hourB) || !beganAt.Equal(now) {
		t.Fatalf("after the rate-limited pass: through, began_at = %v, %v, want %v (the end of hour A), %v", through, beganAt, hourB, now)
	}
	if got, want := storedRunIDs(t, reconcile), []int64{100, 101, 102}; !slices.Equal(got, want) {
		t.Fatalf("stored runs after the rate-limited pass = %v, want hour A's %v and none of hour B's", got, want)
	}
	for _, id := range []int64{100, 101, 102} {
		run, err := ScanRun(pool.QueryRow(ctx, `select `+RunColumns+` from delivery_runs where repo = 'acme/widgets' and run_id = $1`, id))
		if err != nil || run.HeadBranch == nil || *run.HeadBranch != "main" || run.Event == nil || *run.Event != "push" {
			t.Fatalf("run %d = %+v, %v, want it stored with head_branch main and event push", id, run, err)
		}
	}

	fake.limit = nil
	before := len(fake.listingCalls())
	reconcile.backfillDeploys(t, now)
	second := fake.listingCalls()[before:]
	wantSecond := []string{
		hourWindow(hourB) + " page 1", hourWindow(hourB) + " page 2",
		hourWindow(hourC) + " page 1",
	}
	if !slices.Equal(second, wantSecond) {
		t.Fatalf("second pass listings = %v, want hour B in full then hour C: %v", second, wantSecond)
	}
	if got, want := storedRunIDs(t, reconcile), []int64{100, 101, 102, 200, 201, 202, 203, 300, 301}; !slices.Equal(got, want) {
		t.Fatalf("stored runs after the second pass = %v, want every run %v", got, want)
	}
	through, beganAt, _ = deployBackfill(t, reconcile)
	if through.Before(beganAt) {
		t.Fatalf("after the second pass: through %v is before began_at %v, want the step finished", through, beganAt)
	}

	before = len(fake.listingCalls())
	reconcile.backfillDeploys(t, now)
	if third := fake.listingCalls()[before:]; len(third) != 0 {
		t.Fatalf("a third pass listed %v, want nothing once the step is finished", third)
	}
}

// TestBackfillRunsKeepsItsFailuresOutOfTheHealthState: a pass whose backfill hits a rate limit
// while every regular step succeeds records the reconcile as healthy.
func TestBackfillRunsKeepsItsFailuresOutOfTheHealthState(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	github := newFakeGitHub(t)
	github.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	// The regular step lists from 28 days ago in one window the fake answers empty; every one of
	// the backfill's one-hour windows is refused.
	github.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		since, until := parseCreatedWindow(t, r.URL.Query().Get("created"))
		if until.Sub(since) < 2*time.Hour {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	github.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	reconcile := NewReconcile(pool, github.newTestClient())

	before := time.Now()
	reconcile.runOnce(ctx)
	after := time.Now()

	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.LastError != nil {
		t.Fatalf("last_error = %q, want nil: a backfill failure stays out of the health state", *settings.LastError)
	}
	if settings.LastReconcileAt == nil || settings.LastReconcileAt.Before(before.Add(-time.Second)) || settings.LastReconcileAt.After(after.Add(time.Second)) {
		t.Fatalf("last_reconcile_at = %v, want this pass's time, between %v and %v", settings.LastReconcileAt, before, after)
	}
	through, beganAt, found := deployBackfill(t, reconcile)
	if !found || through.Before(before.Add(-BackfillWindow-time.Second)) || beganAt.Before(before.Add(-time.Second)) || beganAt.After(after.Add(time.Second)) {
		t.Fatalf("backfill row = {%v, %v, %t}, want began at this pass and through 28 days before it", through, beganAt, found)
	}
}

// TestBackfillRunsStartsFromTheBackfillWindow: with no progress row the step begins at
// now - BackfillWindow, records began_at = now, and walks backfillWindowsPerPass hours.
func TestBackfillRunsStartsFromTheBackfillWindow(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake, github := newBackfillFake(t, nil, 10)
	reconcile := NewReconcile(pool, github.newTestClient())
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	reconcile.backfillDeploys(t, now)
	through, beganAt, found := deployBackfill(t, reconcile)
	start := now.Add(-BackfillWindow)
	if !found || !beganAt.Equal(now) || !through.Equal(start.Add(backfillWindowsPerPass*time.Hour)) {
		t.Fatalf("backfill row = {through %v, began_at %v, found %t}, want {%v, %v, true}", through, beganAt, found, start.Add(backfillWindowsPerPass*time.Hour), now)
	}
	if calls := fake.listingCalls(); len(calls) != backfillWindowsPerPass || calls[0] != hourWindow(start)+" page 1" {
		t.Fatalf("listings = %d, first %q, want %d starting at %q", len(calls), calls[0], backfillWindowsPerPass, hourWindow(start)+" page 1")
	}
}

// TestBackfillRunsWalksAtMostTwentyFourWindowsAPass: thirty hours of runs take two passes.
func TestBackfillRunsWalksAtMostTwentyFourWindowsAPass(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var runs []map[string]any
	for hour := range 30 {
		runs = append(runs, backfillRun(int64(1000+hour), start.Add(time.Duration(hour)*time.Hour+10*time.Minute)))
	}
	fake, github := newBackfillFake(t, runs, 10)
	reconcile := NewReconcile(pool, github.newTestClient())
	now := start.Add(30 * time.Hour)
	startBackfill(t, reconcile, start, now)

	reconcile.backfillDeploys(t, now)
	through, _, _ := deployBackfill(t, reconcile)
	if !through.Equal(start.Add(24*time.Hour)) || len(fake.listingCalls()) != 24 {
		t.Fatalf("after one pass: through %v, %d listings, want %v and 24", through, len(fake.listingCalls()), start.Add(24*time.Hour))
	}
	reconcile.backfillDeploys(t, now)
	through, beganAt, _ := deployBackfill(t, reconcile)
	if through.Before(beganAt) || len(storedRunIDs(t, reconcile)) != 30 {
		t.Fatalf("after two passes: through %v, began_at %v, %d runs stored, want finished with 30", through, beganAt, len(storedRunIDs(t, reconcile)))
	}
}

// TestBackfillProgressNeverMovesBackwards: a second task overlapping in a rolling deploy cannot
// move the backfill's progress back.
func TestBackfillProgressNeverMovesBackwards(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	step, scope := backfillStep(DeliveryRunKindDeploy), runsProgressScope("acme/widgets", backfillWorkflow)
	at := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	if err := StartBackfillProgress(ctx, pool, step, scope, at.Add(-time.Hour), at.Add(time.Hour)); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := AdvanceBackfillProgress(ctx, pool, step, scope, at); err != nil {
		t.Fatalf("advance to T: %v", err)
	}
	if err := AdvanceBackfillProgress(ctx, pool, step, scope, at.Add(-time.Hour)); err != nil {
		t.Fatalf("advance to T-1h: %v", err)
	}
	row, err := readBackfillProgress(ctx, pool, step, scope)
	if err != nil || !row.through.Equal(at) {
		t.Fatalf("through = %v, %v, want %v", row.through, err, at)
	}
}

// TestBackfillProgressIsNotRecreatedAfterASettingsChange: a settings change deletes every progress
// row mid-pass, so the pass's next advance updates nothing and inserts nothing, and the next pass
// starts the backfill afresh with a new began_at.
func TestBackfillProgressIsNotRecreatedAfterASettingsChange(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	step, scope := backfillStep(DeliveryRunKindDeploy), runsProgressScope("acme/widgets", backfillWorkflow)
	first := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	if err := StartBackfillProgress(ctx, pool, step, scope, first.Add(-BackfillWindow), first); err != nil {
		t.Fatalf("start: %v", err)
	}
	seedDeliverySettings(t, ctx, pool) // PutSettings deletes every progress row.
	if err := AdvanceBackfillProgress(ctx, pool, step, scope, first.Add(-BackfillWindow+time.Hour)); err != nil {
		t.Fatalf("advance after the settings change: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from delivery_reconcile_progress where step = $1`, step).Scan(&rows); err != nil {
		t.Fatalf("count progress rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("progress rows after an advance over a deleted row = %d, want 0 (an advance never inserts)", rows)
	}

	_, github := newBackfillFake(t, nil, 10)
	reconcile := NewReconcile(pool, github.newTestClient())
	later := first.Add(2 * time.Hour)
	reconcile.backfillDeploys(t, later)
	_, beganAt, found := deployBackfill(t, reconcile)
	if !found || !beganAt.Equal(later) {
		t.Fatalf("began_at after the next pass = %v (found %t), want %v: a fresh backfill", beganAt, found, later)
	}
}

// TestBackfillRunsListsJobsOnlyForRunsWithoutStoredJobs: of hour A's three runs two already have
// job rows, so the backfill lists jobs for the third alone, and the two keep their rows.
func TestBackfillRunsListsJobsOnlyForRunsWithoutStoredJobs(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	hourA := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	runs := []map[string]any{
		backfillRun(100, hourA.Add(5*time.Minute)), backfillRun(101, hourA.Add(20*time.Minute)), backfillRun(102, hourA.Add(40*time.Minute)),
	}
	completed := hourA.Add(10 * time.Minute)
	for _, id := range []int64{100, 101} {
		if err := UpsertRun(ctx, pool, DeliveryRun{
			Repo: "acme/widgets", RunID: id, Kind: DeliveryRunKindDeploy, HeadSHA: "old",
			HeadCommitAt: hourA, StartedAt: hourA, CompletedAt: &completed,
			Conclusion: new(DeliveryRunConclusionSuccess), URL: "https://github.com/acme/widgets/actions/runs/100",
		}); err != nil {
			t.Fatalf("seed run %d: %v", id, err)
		}
		if err := UpsertRunJobs(ctx, pool, "acme/widgets", id, []DeliveryRunJob{{
			Repo: "acme/widgets", RunID: id, Name: "build", CompletedAt: &completed, Conclusion: new(DeliveryJobConclusionSuccess),
		}}); err != nil {
			t.Fatalf("seed run %d's jobs: %v", id, err)
		}
	}
	fake, github := newBackfillFake(t, runs, 10)
	reconcile := NewReconcile(pool, github.newTestClient())
	startBackfill(t, reconcile, hourA, hourA.Add(time.Hour))

	reconcile.backfillDeploys(t, hourA.Add(time.Hour))
	if got := fake.jobCalls(); !slices.Equal(got, []int64{102}) {
		t.Fatalf("jobs listed for runs %v, want only 102", got)
	}
	for _, id := range []int64{100, 101} {
		jobs, err := ListRunJobs(ctx, pool, "acme/widgets", id)
		if err != nil || len(jobs) != 1 || jobs[0].Name != "build" {
			t.Fatalf("run %d jobs = %+v, %v, want its stored build job kept", id, jobs, err)
		}
	}
}

// TestBackfillRunsLeavesTheRegularStepAlone: with the backfill finished, the regular runs/deploy
// step lists from its own progress minus the overlap and lists every concluded run's jobs.
func TestBackfillRunsLeavesTheRegularStepAlone(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	scope := runsProgressScope("acme/widgets", backfillWorkflow)
	if err := StartBackfillProgress(ctx, pool, backfillStep(DeliveryRunKindDeploy), scope, now, now); err != nil {
		t.Fatalf("finish the backfill: %v", err)
	}
	regularThrough := now.Add(-2 * time.Hour)
	if err := RecordReconcileProgress(ctx, pool, runsStep(DeliveryRunKindDeploy), scope, regularThrough); err != nil {
		t.Fatalf("record the regular step's progress: %v", err)
	}
	runs := []map[string]any{backfillRun(400, now.Add(-90*time.Minute)), backfillRun(401, now.Add(-30*time.Minute))}
	for _, id := range []int64{400, 401} {
		completed := now.Add(-time.Hour)
		if err := UpsertRun(ctx, pool, DeliveryRun{
			Repo: "acme/widgets", RunID: id, Kind: DeliveryRunKindDeploy, HeadSHA: "old",
			HeadCommitAt: now, StartedAt: now, CompletedAt: &completed, URL: "https://github.com/acme/widgets/actions/runs/400",
		}); err != nil {
			t.Fatalf("seed run %d: %v", id, err)
		}
		if err := UpsertRunJobs(ctx, pool, "acme/widgets", id, []DeliveryRunJob{{Repo: "acme/widgets", RunID: id, Name: "build"}}); err != nil {
			t.Fatalf("seed run %d's jobs: %v", id, err)
		}
	}
	fake, github := newBackfillFake(t, runs, 10)
	reconcile := NewReconcile(pool, github.newTestClient())

	reconcile.backfillDeploys(t, now)
	if calls := fake.listingCalls(); len(calls) != 0 {
		t.Fatalf("the finished backfill listed %v, want nothing", calls)
	}
	if err := reconcile.reconcileWorkflow(ctx, "acme", "widgets", "acme/widgets", backfillWorkflow, DeliveryRunKindDeploy, now); err != nil {
		t.Fatalf("reconcileWorkflow: %v", err)
	}
	calls := fake.listingCalls()
	if len(calls) == 0 {
		t.Fatal("the regular step listed nothing")
	}
	since, _ := parseCreatedWindow(t, calls[0][:len(calls[0])-len(" page 1")])
	if !since.Equal(regularThrough.Add(-reconcileOverlap)) {
		t.Fatalf("the regular step listed from %v, want its own progress minus the overlap, %v", since, regularThrough.Add(-reconcileOverlap))
	}
	if got := fake.jobCalls(); !slices.Equal(slices.Sorted(slices.Values(got)), []int64{400, 401}) {
		t.Fatalf("the regular step listed jobs for %v, want every concluded run, 400 and 401", got)
	}
}
