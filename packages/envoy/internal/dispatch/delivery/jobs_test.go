package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeJobClock is a jobClock the test moves: Advance fires, in due order and on the caller's
// goroutine, every timer due by the new time, so a read a timer starts has finished when Advance
// returns.
type fakeJobClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeJobTimer
}

type fakeJobTimer struct {
	clock   *fakeJobClock
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func newFakeJobClock() *fakeJobClock {
	return &fakeJobClock{now: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeJobClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeJobClock) AfterFunc(d time.Duration, f func()) jobTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeJobTimer{clock: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, timer)
	return timer
}

func (t *fakeJobTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// armed counts the timers neither fired nor stopped.
func (c *fakeJobClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired {
			n++
		}
	}
	return n
}

func (c *fakeJobClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeJobTimer
	for _, timer := range c.timers {
		if !timer.stopped && !timer.fired && !timer.at.After(c.now) {
			timer.fired = true
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	slices.SortStableFunc(due, func(a, b *fakeJobTimer) int { return a.at.Compare(b.at) })
	for _, timer := range due {
		timer.f()
	}
}

// jobsFake is a fake GitHub answering run 500's jobs: a build job and the production job, each
// finished as the test sets them, counting the reads.
type jobsFake struct {
	*fakeGitHub
	calls      atomic.Int32
	mu         sync.Mutex
	production map[string]any
	answer     func(w http.ResponseWriter, r *http.Request) bool
}

func newJobsFake(t *testing.T) *jobsFake {
	t.Helper()
	fake := &jobsFake{fakeGitHub: newFakeGitHub(t)}
	fake.production = map[string]any{
		"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z",
		"completed_at": "2026-09-03T12:09:00Z", "conclusion": "success",
	}
	fake.handle("GET /repos/acme/widgets/actions/runs/500/jobs", func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		fake.mu.Lock()
		answer, production := fake.answer, fake.production
		fake.mu.Unlock()
		if answer != nil && answer(w, r) {
			return
		}
		mustEncode(t, w, map[string]any{"total_count": 2, "jobs": []map[string]any{
			{"name": "build", "started_at": "2026-09-03T12:00:00Z", "completed_at": "2026-09-03T12:04:00Z", "conclusion": "success"},
			production,
		}})
	})
	return fake
}

// seedRunningDeployRun stores run 500 of the deploy repository, started and not ended.
func seedRunningDeployRun(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(500, DeliveryRunKindDeploy, started.Add(-time.Minute), started)); err != nil {
		t.Fatalf("seed run 500: %v", err)
	}
}

func jobPayload(repo string, runID int64) string {
	payload, _ := json.Marshal(map[string]string{
		"kind": "job", "repo": repo, "run_id": fmt.Sprint(runID), "job_id": "9001",
		"name": "widgets-release / widgets-release", "status": "completed", "conclusion": "success",
	})
	return string(payload)
}

func readJobsComplete(t *testing.T, ctx context.Context, pool *store.Pool, runID int64) bool {
	t.Helper()
	var complete bool
	if err := pool.QueryRow(ctx, `select jobs_complete from delivery_runs where repo = 'acme/widgets' and run_id = $1`, runID).Scan(&complete); err != nil {
		t.Fatalf("read jobs_complete of run %d: %v", runID, err)
	}
	return complete
}

// TestAJobOfATrackedDeployRunRecordsItsJobsWhileItRuns drives the consumer end to end: a job event
// for run 500, which Dispatch tracks and which has not ended, makes one read of its jobs once the
// burst goes quiet. The production job is stored finished, another job in that same listing is
// still running, so the run's jobs stay incomplete, and the change log holds a run_job change.
func TestAJobOfATrackedDeployRunRecordsItsJobsWhileItRuns(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	before := latestChange(t, ctx, pool)
	fake := newJobsFake(t)
	fake.mu.Lock()
	fake.answer = func(w http.ResponseWriter, _ *http.Request) bool {
		mustEncode(t, w, map[string]any{"total_count": 3, "jobs": []map[string]any{
			{"name": "build", "started_at": "2026-09-03T12:00:00Z", "completed_at": "2026-09-03T12:04:00Z", "conclusion": "success"},
			{"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z", "completed_at": "2026-09-03T12:09:00Z", "conclusion": "success"},
			{"name": "post-release", "started_at": "2026-09-03T12:10:00Z", "completed_at": nil, "conclusion": nil},
		}})
		return true
	}
	fake.mu.Unlock()
	clock := newFakeJobClock()
	natsClient := intakeTestClient(t)
	jobs := newJobIntake(natsClient, pool, fake.newTestClient(), clock)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); jobs.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 20*time.Second, func() bool {
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, jobConsumerName)
		return err == nil && info.PushBound
	}, func() string { return "the job intake never bound its durable" })

	publishGitHubEnvelope(t, natsClient, "notifications.github.acme.widgets.job.500.completed", jobPayload("acme/widgets", 500))
	waitFor(t, 20*time.Second, func() bool { return clock.armed() == 1 }, func() string { return "the job event scheduled no read" })
	if calls := fake.calls.Load(); calls != 0 {
		t.Fatalf("read the jobs %d times before the burst went quiet", calls)
	}
	clock.Advance(jobQuiet)

	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("jobs reads = %d, want 1", calls)
	}
	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500)
	if err != nil {
		t.Fatalf("ListRunJobs: %v", err)
	}
	var production *DeliveryRunJob
	for i := range stored {
		if stored[i].Name == "widgets-release / widgets-release" {
			production = &stored[i]
		}
	}
	if production == nil || production.CompletedAt == nil || production.Conclusion == nil || *production.Conclusion != DeliveryJobConclusionSuccess {
		t.Fatalf("the production job = %+v, want it stored finished with success", production)
	}
	if readJobsComplete(t, ctx, pool, 500) {
		t.Fatal("jobs_complete = true for a run that has not ended")
	}
	changes := changesAfter(t, ctx, pool, before)
	if !slices.ContainsFunc(changes, func(c Change) bool { return c.Kind == "run_job" && c.Ref == "500" }) {
		t.Fatalf("changes = %+v, want a run_job change of run 500", changes)
	}
}

// TestAJobOfAnUntrackedRunCostsNoGitHubCall: a job of a run Dispatch does not store schedules
// nothing and asks GitHub for nothing, not even a token.
func TestAJobOfAnUntrackedRunCostsNoGitHubCall(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	fake := newJobsFake(t)
	clock := newFakeJobClock()
	jobs := newJobIntake(nil, pool, fake.newTestClient(), clock)
	jobs.receive(ctx, jobPayload("acme/widgets", 999))
	clock.Advance(jobMaxWait)
	if armed, calls, mints := clock.armed(), fake.calls.Load(), fake.tokenMints.Load(); armed != 0 || calls != 0 || mints != 0 {
		t.Fatalf("an untracked run's job: %d timers, %d jobs reads, %d token mints, want none", armed, calls, mints)
	}
}

// TestAJobOfAnotherRepositoryIsIgnored: a job of a repository other than the deploy repository
// schedules nothing, even when a run of that number is stored.
func TestAJobOfAnotherRepositoryIsIgnored(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	clock := newFakeJobClock()
	jobs := newJobIntake(nil, pool, fake.newTestClient(), clock)
	jobs.receive(ctx, jobPayload("acme/other", 500))
	clock.Advance(jobMaxWait)
	if armed, calls, mints := clock.armed(), fake.calls.Load(), fake.tokenMints.Load(); armed != 0 || calls != 0 || mints != 0 {
		t.Fatalf("another repository's job: %d timers, %d jobs reads, %d token mints, want none", armed, calls, mints)
	}
}

// TestABurstOfJobsOfOneRunReadsItsJobsOnce: thirty job events of one run within a second are one
// read.
func TestABurstOfJobsOfOneRunReadsItsJobsOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	clock := newFakeJobClock()
	jobs := newJobIntake(nil, pool, fake.newTestClient(), clock)
	for range 30 {
		jobs.receive(ctx, jobPayload("acme/widgets", 500))
		clock.Advance(time.Second / 30)
	}
	clock.Advance(jobQuiet)
	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("thirty events within a second made %d reads, want 1", calls)
	}
}

// TestABurstThatKeepsComingStillReadsWithinTenSeconds: an event every second for fifteen seconds
// never goes quiet, yet the run is read at ten seconds, and again once the rest of the burst ends.
func TestABurstThatKeepsComingStillReadsWithinTenSeconds(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	clock := newFakeJobClock()
	jobs := newJobIntake(nil, pool, fake.newTestClient(), clock)
	for second := range 15 {
		jobs.receive(ctx, jobPayload("acme/widgets", 500))
		clock.Advance(time.Second)
		if second == 9 {
			if calls := fake.calls.Load(); calls != 1 {
				t.Fatalf("reads at ten seconds = %d, want 1", calls)
			}
		}
	}
	clock.Advance(jobQuiet)
	if calls := fake.calls.Load(); calls != 2 {
		t.Fatalf("reads once the burst ended = %d, want 2", calls)
	}
}

// TestAStaleJobsReadStaysForTheBackfill: the job listing can see the production job running while
// the run's completed workflow event reaches Dispatch between that listing and the jobs write. The
// stale listing is the first job-row write, so it must leave the run available for the completion
// listing rather than permanently mark it complete.
func TestAStaleJobsReadStaysForTheBackfill(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	ended := started.Add(15 * time.Minute)
	success := DeliveryRunConclusionSuccess
	run := changeTestRun(500, DeliveryRunKindDeploy, started.Add(-time.Minute), started)
	run.CompletedAt, run.Conclusion = &ended, &success
	completed := make(chan error, 1)
	fake.mu.Lock()
	fake.production = map[string]any{
		"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z",
		"completed_at": nil, "conclusion": nil,
	}
	fake.answer = func(_ http.ResponseWriter, _ *http.Request) bool {
		completed <- UpsertRun(ctx, pool, run)
		return false
	}
	fake.mu.Unlock()

	jobs := newJobIntake(nil, pool, fake.newTestClient(), newFakeJobClock())
	jobs.read(ctx, runKey{repo: "acme/widgets", runID: 500})
	if err := <-completed; err != nil {
		t.Fatalf("store the run completion: %v", err)
	}
	skip, err := ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{500})
	if err != nil {
		t.Fatalf("ListRunIDsSkippingJobs after stale read: %v", err)
	}
	if skip[500] {
		t.Fatal("a stale jobs read marked the run complete, so the backfill cannot finish its production job")
	}

	finished := started.Add(9 * time.Minute)
	jobSuccess := DeliveryJobConclusionSuccess
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 500, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 500, Name: "build", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobSuccess},
		{Repo: "acme/widgets", RunID: 500, Name: "widgets-release / widgets-release", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobSuccess},
	}, true); err != nil {
		t.Fatalf("store the completion listing: %v", err)
	}
	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500)
	if err != nil {
		t.Fatalf("ListRunJobs after completion listing: %v", err)
	}
	for _, job := range stored {
		if job.CompletedAt == nil || !job.CompletedAt.Equal(finished) {
			t.Fatalf("job %q after the completion listing = %+v, want completed at %v", job.Name, job, finished)
		}
	}
}

// TestAReadThatLandsAfterTheRunEndedUndoesNothing: a read in flight while the run ends -- its end
// writes every job finished -- answers with the production job still running, and lands after:
// the stored jobs stay finished and the run's jobs stay complete.
func TestAReadThatLandsAfterTheRunEndedUndoesNothing(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	fake := newJobsFake(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fake.mu.Lock()
	fake.production = map[string]any{"name": "widgets-release / widgets-release", "started_at": "2026-09-03T12:05:00Z", "completed_at": nil, "conclusion": nil}
	fake.answer = func(w http.ResponseWriter, r *http.Request) bool {
		close(entered)
		<-release
		return false
	}
	fake.mu.Unlock()
	clock := newFakeJobClock()
	jobs := newJobIntake(nil, pool, fake.newTestClient(), clock)
	jobs.receive(ctx, jobPayload("acme/widgets", 500))
	read := make(chan struct{})
	go func() { defer close(read); clock.Advance(jobQuiet) }()
	<-entered

	// The run's end, as Intake writes it: the run concluded and every job finished.
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	ended, finished := started.Add(15*time.Minute), started.Add(9*time.Minute)
	success := DeliveryRunConclusionSuccess
	run := changeTestRun(500, DeliveryRunKindDeploy, started.Add(-time.Minute), started)
	run.CompletedAt, run.Conclusion = &ended, &success
	if err := UpsertRun(ctx, pool, run); err != nil {
		t.Fatalf("end the run: %v", err)
	}
	jobSuccess := DeliveryJobConclusionSuccess
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 500, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 500, Name: "build", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobSuccess},
		{Repo: "acme/widgets", RunID: 500, Name: "widgets-release / widgets-release", StartedAt: &started, CompletedAt: &finished, Conclusion: &jobSuccess},
	}, true); err != nil {
		t.Fatalf("store the ended run's jobs: %v", err)
	}
	close(release)
	<-read

	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500)
	if err != nil {
		t.Fatalf("ListRunJobs: %v", err)
	}
	for _, job := range stored {
		if job.CompletedAt == nil || job.Conclusion == nil || *job.Conclusion != jobSuccess {
			t.Fatalf("job %q after the late read = %+v, want it still finished", job.Name, job)
		}
	}
	if !readJobsComplete(t, ctx, pool, 500) {
		t.Fatal("jobs_complete went back to false after a late read")
	}
}

// TestAJobsReadThatFailsIsLoggedAndTheRunIsReadAtItsEnd: GitHub answering 500 to the jobs read is
// a WARN and changes no row; the run stays on the backfill's list, and its end reads its jobs.
func TestAJobsReadThatFailsIsLoggedAndTheRunIsReadAtItsEnd(t *testing.T) {
	logs := captureLogs(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	before := latestChange(t, ctx, pool)
	fake := newJobsFake(t)
	fake.mu.Lock()
	fake.answer = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
		return true
	}
	fake.mu.Unlock()
	clock := newFakeJobClock()
	jobs := newJobIntake(nil, pool, fake.newTestClient(), clock)
	jobs.receive(ctx, jobPayload("acme/widgets", 500))
	clock.Advance(jobQuiet)

	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("jobs reads = %d, want 1", calls)
	}
	warned := slices.ContainsFunc(logs(), func(record map[string]any) bool {
		return record["level"] == "WARN" && record["run_id"] == float64(500)
	})
	if !warned {
		t.Fatalf("no WARN naming run 500 among %v", logs())
	}
	if changes := changesAfter(t, ctx, pool, before); len(changes) != 0 {
		t.Fatalf("a failed read logged %+v, want nothing", changes)
	}
	if stored, err := ListRunJobs(ctx, pool, "acme/widgets", 500); err != nil || len(stored) != 0 {
		t.Fatalf("jobs after a failed read = %+v, %v, want none", stored, err)
	}
	skip, err := ListRunIDsSkippingJobs(ctx, pool, "acme/widgets", []int64{500})
	if err != nil || skip[500] {
		t.Fatalf("the backfill skips run 500 after a failed read (%v), want it listed", err)
	}
}

// concurrentJobClock is a jobClock whose timers all fire at once, each on a goroutine of its own,
// when the test calls fireAll: the way a backlog that drains faster than jobQuiet ends every run's
// burst together.
type concurrentJobClock struct {
	mu     sync.Mutex
	timers []func()
}

// unstoppableTimer is the timer concurrentJobClock hands back; each run is scheduled once here,
// so nothing stops one.
type unstoppableTimer struct{}

func (unstoppableTimer) Stop() bool { return false }

func (c *concurrentJobClock) Now() time.Time { return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC) }

func (c *concurrentJobClock) AfterFunc(_ time.Duration, f func()) jobTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = append(c.timers, f)
	return unstoppableTimer{}
}

// fireAll starts every timer's function on a goroutine of its own and returns the group they run
// in.
func (c *concurrentJobClock) fireAll() *sync.WaitGroup {
	c.mu.Lock()
	timers := c.timers
	c.timers = nil
	c.mu.Unlock()
	var fired sync.WaitGroup
	for _, f := range timers {
		fired.Go(f)
	}
	return &fired
}

// TestTheJobIntakeReadsAtMostABoundedNumberOfRunsAtOnce: when many tracked runs' bursts end
// together, as on the first start after an outage drains its backlog, the job intake asks GitHub for
// at most reconcilePartialConcurrency runs' jobs at a time, and reads every run once. Unbounded, it
// opened one listing per run at once, which past GitHub's 100 concurrent requests rate-limits the
// App.
func TestTheJobIntakeReadsAtMostABoundedNumberOfRunsAtOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	const runs = 3 * reconcilePartialConcurrency
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	for id := int64(600); id < 600+runs; id++ {
		if err := UpsertRun(ctx, pool, changeTestRun(id, DeliveryRunKindDeploy, started.Add(-time.Minute), started)); err != nil {
			t.Fatalf("seed run %d: %v", id, err)
		}
	}
	var inFlight, most, reads atomic.Int32
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}/jobs", func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		for seen := most.Load(); now > seen && !most.CompareAndSwap(seen, now); seen = most.Load() {
		}
		<-release
		inFlight.Add(-1)
		reads.Add(1)
		mustEncode(t, w, map[string]any{"total_count": 1, "jobs": []map[string]any{
			{"name": "build", "started_at": "2026-09-03T12:00:00Z", "completed_at": "2026-09-03T12:04:00Z", "conclusion": "success"},
		}})
	})
	github := fake.newTestClient()
	// Registered after the fake's server, so it runs first: the server's Close waits for the
	// handlers it holds.
	t.Cleanup(releaseOnce)

	clock := &concurrentJobClock{}
	jobs := newJobIntake(nil, pool, github, clock)
	for id := int64(600); id < 600+runs; id++ {
		jobs.schedule(runKey{repo: "acme/widgets", runID: id})
	}
	fired := clock.fireAll()
	// Runs before the pool's and the fake's cleanups, so no read outlives the test into them.
	t.Cleanup(func() {
		releaseOnce()
		fired.Wait()
	})
	waitFor(t, 20*time.Second, func() bool { return inFlight.Load() >= reconcilePartialConcurrency }, func() string {
		return fmt.Sprintf("%d jobs reads in flight, want %d", inFlight.Load(), reconcilePartialConcurrency)
	})
	// Every run's burst has ended; give the rest a moment to start reads they should not.
	time.Sleep(500 * time.Millisecond)
	if got := most.Load(); got != reconcilePartialConcurrency {
		t.Fatalf("%d of %d runs' jobs read at once, want at most %d", got, runs, reconcilePartialConcurrency)
	}

	releaseOnce()
	fired.Wait()
	if got := reads.Load(); got != runs {
		t.Fatalf("jobs reads = %d, want one for each of the %d runs", got, runs)
	}
	if got := most.Load(); got != reconcilePartialConcurrency {
		t.Fatalf("%d runs' jobs read at once by the end, want at most %d", got, reconcilePartialConcurrency)
	}
}
