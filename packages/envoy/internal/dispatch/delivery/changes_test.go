package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// latestChange is the change log's newest seq, which a test reads after its setup so its
// assertions see only what it did.
func latestChange(t *testing.T, ctx context.Context, pool *store.Pool) int64 {
	t.Helper()
	seq, err := LatestChangeSeq(ctx, pool)
	if err != nil {
		t.Fatalf("LatestChangeSeq: %v", err)
	}
	return seq
}

func changesAfter(t *testing.T, ctx context.Context, pool *store.Pool, seq int64) []Change {
	t.Helper()
	changes, err := ChangesSince(ctx, pool, seq, 1000)
	if err != nil {
		t.Fatalf("ChangesSince(%d): %v", seq, err)
	}
	return changes
}

func changeTestPR(number int, merged time.Time, title string) DeliveryPullRequest {
	created := merged.Add(-time.Hour)
	return DeliveryPullRequest{
		Repo: "acme/widgets", Number: number, Title: title, Author: "octocat",
		URL:       fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		CreatedAt: &created, MergedAt: &merged, Sessions: []string{},
	}
}

func changeTestRun(id int64, kind DeliveryRunKind, headAt, startedAt time.Time) DeliveryRun {
	return DeliveryRun{
		Repo: "acme/widgets", RunID: id, Kind: kind, HeadSHA: fmt.Sprintf("sha%d", id),
		HeadCommitAt: headAt, StartedAt: startedAt, URL: fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", id),
	}
}

func sameTime(a *time.Time, b time.Time) bool { return a != nil && a.Equal(b) }

// listenConn opens a connection of its own, outside the pool, listening on channel.
func listenConn(t *testing.T, ctx context.Context, channel string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, os.Getenv("DISPATCH_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect a listener: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, "listen "+channel); err != nil {
		t.Fatalf("listen %s: %v", channel, err)
	}
	return conn
}

// nextNotification is the next notification conn receives within wait, or nil.
func nextNotification(t *testing.T, ctx context.Context, conn *pgx.Conn, wait time.Duration) *pgconn.Notification {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	notification, err := conn.WaitForNotification(waitCtx)
	if err != nil {
		if waitCtx.Err() != nil && ctx.Err() == nil {
			return nil
		}
		t.Fatalf("wait for a notification: %v", err)
	}
	return notification
}

// TestAPullRequestUpsertWithNewContentLogsOneChange: a new pull request logs one change carrying
// its merge time, and a later upsert that changes its title logs a second with that time on both
// sides.
func TestAPullRequestUpsertWithNewContentLogsOneChange(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	merged := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	before := latestChange(t, ctx, pool)

	if err := UpsertPullRequest(ctx, pool, changeTestPR(7, merged, "feat: first")); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	changes := changesAfter(t, ctx, pool, before)
	if len(changes) != 1 {
		t.Fatalf("changes after a new pull request = %+v, want one", changes)
	}
	got := changes[0]
	if got.Kind != "pull_request" || got.Repo != "acme/widgets" || got.Ref != "7" || got.RunKind != "" ||
		got.TOld != nil || !sameTime(got.TNew, merged) {
		t.Fatalf("the new pull request's change = %+v, want pull_request acme/widgets 7 with TNew %v", got, merged)
	}

	if err := UpsertPullRequest(ctx, pool, changeTestPR(7, merged, "feat: first, retitled")); err != nil {
		t.Fatalf("UpsertPullRequest again: %v", err)
	}
	changes = changesAfter(t, ctx, pool, got.Seq)
	if len(changes) != 1 || !sameTime(changes[0].TOld, merged) || !sameTime(changes[0].TNew, merged) {
		t.Fatalf("changes after a retitle = %+v, want one with TOld = TNew = %v", changes, merged)
	}
}

// TestAReconcileReupsertOfTheSameFactsLogsNothing: what a reconcile pass that finds nothing new
// writes -- the merged-PR search's partial re-upsert of a complete row and of a partial one, the
// attribution pass over rows whose issue did not change, and a run and its jobs written again with
// the same facts -- logs no change.
func TestAReconcileReupsertOfTheSameFactsLogsNothing(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	merged := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	complete := changeTestPR(8, merged, "feat: complete")
	complete.Attribution = &AttributionInputs{}
	if err := UpsertPullRequest(ctx, pool, complete); err != nil {
		t.Fatalf("seed the complete row: %v", err)
	}
	partial := changeTestPR(9, merged, "feat: partial")
	partial.Partial = true
	if err := UpsertPullRequest(ctx, pool, partial); err != nil {
		t.Fatalf("seed the partial row: %v", err)
	}
	run := changeTestRun(500, DeliveryRunKindDeploy, merged, merged.Add(time.Minute))
	completed := merged.Add(time.Hour)
	success := DeliveryRunConclusionSuccess
	run.CompletedAt, run.Conclusion = &completed, &success
	if err := UpsertRun(ctx, pool, run); err != nil {
		t.Fatalf("seed the run: %v", err)
	}
	jobSuccess := DeliveryJobConclusionSuccess
	jobs := []DeliveryRunJob{{Repo: "acme/widgets", RunID: 500, Name: "widgets-release / widgets-release", StartedAt: &merged, CompletedAt: &completed, Conclusion: &jobSuccess}}
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 500, jobs, true); err != nil {
		t.Fatalf("seed the jobs: %v", err)
	}
	after := latestChange(t, ctx, pool)

	// reconcile.go:335 writes what the search returned, Partial true, for both rows.
	for _, pr := range []DeliveryPullRequest{complete, partial} {
		searched := changeTestPR(pr.Number, merged, pr.Title)
		searched.Partial = true
		if err := UpsertPullRequest(ctx, pool, searched); err != nil {
			t.Fatalf("reconcile re-upsert of #%d: %v", pr.Number, err)
		}
	}
	for range 2 {
		if _, err := AttributePullRequests(ctx, pool, AttributionResolveBatch); err != nil {
			t.Fatalf("AttributePullRequests: %v", err)
		}
	}
	if err := UpsertRun(ctx, pool, run); err != nil {
		t.Fatalf("re-upsert the run: %v", err)
	}
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 500, jobs, true); err != nil {
		t.Fatalf("re-upsert the jobs: %v", err)
	}
	if changes := changesAfter(t, ctx, pool, after); len(changes) != 0 {
		t.Fatalf("a pass that found nothing new logged %+v, want nothing", changes)
	}
}

// TestARunChangeLogsItsKindAndItsLaterTime: a run logs its kind and the later of its head commit
// and its start, and a job of it logs the run's kind and the same time.
func TestARunChangeLogsItsKindAndItsLaterTime(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	before := latestChange(t, ctx, pool)

	if err := UpsertRun(ctx, pool, changeTestRun(600, DeliveryRunKindDeploy, started.Add(-time.Hour), started)); err != nil {
		t.Fatalf("UpsertRun deploy: %v", err)
	}
	if err := UpsertRun(ctx, pool, changeTestRun(601, DeliveryRunKindPRChecks, started, started.Add(-time.Minute))); err != nil {
		t.Fatalf("UpsertRun pr_checks: %v", err)
	}
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 600, []DeliveryRunJob{{Repo: "acme/widgets", RunID: 600, Name: "build", StartedAt: &started}}, false); err != nil {
		t.Fatalf("UpsertRunJobs: %v", err)
	}
	changes := changesAfter(t, ctx, pool, before)
	if len(changes) != 3 {
		t.Fatalf("changes = %+v, want three", changes)
	}
	deploy, checks, job := changes[0], changes[1], changes[2]
	if deploy.Kind != "run" || deploy.RunKind != "deploy" || deploy.Ref != "600" || deploy.TOld != nil || !sameTime(deploy.TNew, started) {
		t.Fatalf("the deploy run's change = %+v, want run deploy 600 with TNew %v (its start, after its head commit)", deploy, started)
	}
	if checks.Kind != "run" || checks.RunKind != "pr_checks" || !sameTime(checks.TNew, started) {
		t.Fatalf("the PR-checks run's change = %+v, want run pr_checks with TNew %v (its head commit, after its start)", checks, started)
	}
	if job.Kind != "run_job" || job.RunKind != "deploy" || job.Ref != "600" || !sameTime(job.TOld, started) || !sameTime(job.TNew, started) {
		t.Fatalf("the job's change = %+v, want run_job deploy 600 with TOld = TNew = %v", job, started)
	}
}

// freshnessPayload is what a delivery_freshness notification carries.
type freshnessPayload struct {
	LastEventAt     *time.Time `json:"last_event_at"`
	LastReconcileAt *time.Time `json:"last_reconcile_at"`
	LastError       *string    `json:"last_error"`
}

// TestFreshnessOnlySettingsWritesLogNoChangeButNotify: the freshness writes every GitHub event and
// every reconcile pass make log no change, and each committed one notifies delivery_freshness with
// the row's three freshness values.
func TestFreshnessOnlySettingsWritesLogNoChangeButNotify(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	conn := listenConn(t, ctx, "delivery_freshness")
	before := latestChange(t, ctx, pool)
	eventAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	reconciledAt := eventAt.Add(time.Minute)

	writes := []struct {
		name  string
		write func() error
		want  func(freshnessPayload) bool
	}{
		{"RecordEventAt", func() error { return RecordEventAt(ctx, pool, eventAt) }, func(p freshnessPayload) bool {
			return sameTime(p.LastEventAt, eventAt) && p.LastReconcileAt == nil && p.LastError == nil
		}},
		{"RecordReconcileSuccess", func() error { return RecordReconcileSuccess(ctx, pool, reconciledAt) }, func(p freshnessPayload) bool {
			return sameTime(p.LastEventAt, eventAt) && sameTime(p.LastReconcileAt, reconciledAt) && p.LastError == nil
		}},
		{"RecordReconcileError", func() error { return RecordReconcileError(ctx, pool, "rate limited") }, func(p freshnessPayload) bool {
			return sameTime(p.LastReconcileAt, reconciledAt) && p.LastError != nil && *p.LastError == "rate limited"
		}},
	}
	for _, write := range writes {
		if err := write.write(); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		notification := nextNotification(t, ctx, conn, 5*time.Second)
		if notification == nil {
			t.Fatalf("%s notified nothing", write.name)
		}
		var payload freshnessPayload
		if err := json.Unmarshal([]byte(notification.Payload), &payload); err != nil {
			t.Fatalf("%s's payload %q: %v", write.name, notification.Payload, err)
		}
		if !write.want(payload) {
			t.Fatalf("%s notified %s", write.name, notification.Payload)
		}
		if extra := nextNotification(t, ctx, conn, 200*time.Millisecond); extra != nil {
			t.Fatalf("%s notified twice: %s", write.name, extra.Payload)
		}
	}
	if changes := changesAfter(t, ctx, pool, before); len(changes) != 0 {
		t.Fatalf("freshness writes logged %+v, want nothing", changes)
	}
}

// TestTheFreshnessPayloadStaysUnderTheNotifyLimit: an error JSON writes at six bytes a character,
// and one of four-byte characters, each commit and notify a payload under Postgres's 8,000-byte
// limit, rather than failing the settings write.
func TestTheFreshnessPayloadStaysUnderTheNotifyLimit(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	conn := listenConn(t, ctx, "delivery_freshness")
	for name, message := range map[string]string{
		"control characters":   strings.Repeat("\x01", 5000),
		"four-byte characters": strings.Repeat("\U0001F600", 5000),
	} {
		if err := RecordReconcileError(ctx, pool, message); err != nil {
			t.Fatalf("RecordReconcileError with 5,000 %s: %v", name, err)
		}
		notification := nextNotification(t, ctx, conn, 5*time.Second)
		if notification == nil {
			t.Fatalf("5,000 %s notified nothing", name)
		}
		if size := len(notification.Payload); size >= 8000 {
			t.Fatalf("5,000 %s notified %d bytes, want under 8,000", name, size)
		}
	}
}

// TestEveryLoggedChangeNotifiesAtCommit: a change notifies delivery_changes once its transaction
// commits, and not before.
func TestEveryLoggedChangeNotifiesAtCommit(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	conn := listenConn(t, ctx, "delivery_changes")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		insert into delivery_pull_requests (repo, number, title, url, author, merged_at, partial)
		values ('acme/widgets', 10, 'feat: in a transaction', 'https://github.com/acme/widgets/pull/10', 'octocat', now(), true)
	`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if early := nextNotification(t, ctx, conn, 300*time.Millisecond); early != nil {
		t.Fatalf("notified before commit: %+v", early)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if nextNotification(t, ctx, conn, 5*time.Second) == nil {
		t.Fatal("the commit notified nothing")
	}
	if extra := nextNotification(t, ctx, conn, 300*time.Millisecond); extra != nil {
		t.Fatalf("one change notified twice: %+v", extra)
	}
}

// TestChangeSeqsFollowCommitOrder: a second writer's change waits on the trigger's advisory lock
// until the first writer commits, so it cannot take a seq a reader could pass before the first
// writer's lands.
func TestChangeSeqsFollowCommitOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	before := latestChange(t, ctx, pool)
	insert := `
		insert into delivery_pull_requests (repo, number, title, url, author, merged_at, partial)
		values ('acme/widgets', $1::int, 'feat: ordered', 'https://github.com/acme/widgets/pull/' || $1::text, 'octocat', now(), true)`

	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = a.Rollback(context.Background()) }()
	if _, err := a.Exec(ctx, insert, 11); err != nil {
		t.Fatalf("A's insert: %v", err)
	}

	var wg sync.WaitGroup
	bErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		b, err := pool.Begin(ctx)
		if err != nil {
			bErr <- err
			return
		}
		defer func() { _ = b.Rollback(context.Background()) }()
		if _, err := b.Exec(ctx, insert, 12); err != nil {
			bErr <- err
			return
		}
		bErr <- b.Commit(ctx)
	}()

	waitFor(t, 10*time.Second, func() bool {
		var waiting int
		err := pool.QueryRow(ctx, `
			select count(*) from pg_locks
			where locktype = 'advisory' and not granted and classid = 6900 and objid = 0 and objsubid = 2
		`).Scan(&waiting)
		return err == nil && waiting == 1
	}, func() string { return "B never waited on the change log's advisory lock" })

	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	wg.Wait()
	if err := <-bErr; err != nil {
		t.Fatalf("B: %v", err)
	}
	changes := changesAfter(t, ctx, pool, before)
	if len(changes) != 2 || changes[0].Ref != "11" || changes[1].Ref != "12" || changes[0].Seq >= changes[1].Seq {
		t.Fatalf("changes = %+v, want A's (11) before B's (12)", changes)
	}
}

// TestRunJobAndGroupAndSettingsChangesAreLogged: a job's new content, a session group's and its
// members' writes, and a settings change each log their change; settings written again unchanged
// log none.
func TestRunJobAndGroupAndSettingsChangesAreLogged(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(700, DeliveryRunKindPRChecks, started, started)); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 700, []DeliveryRunJob{{Repo: "acme/widgets", RunID: 700, Name: "lint", StartedAt: &started}}, false); err != nil {
		t.Fatalf("UpsertRunJobs: %v", err)
	}

	before := latestChange(t, ctx, pool)
	finished := started.Add(time.Minute)
	failure := DeliveryJobConclusionFailure
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 700, []DeliveryRunJob{{Repo: "acme/widgets", RunID: 700, Name: "lint", StartedAt: &started, CompletedAt: &finished, Conclusion: &failure}}, false); err != nil {
		t.Fatalf("UpsertRunJobs finished: %v", err)
	}
	changes := changesAfter(t, ctx, pool, before)
	if len(changes) != 1 || changes[0].Kind != "run_job" || changes[0].RunKind != "pr_checks" {
		t.Fatalf("a finished job logged %+v, want one run_job of a pr_checks run", changes)
	}

	actor := model.Actor{Kind: "system", ID: "delivery-test"}
	before = latestChange(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	group, err := CreateSessionGroup(ctx, tx, "Planners", []string{"s1", "s2"}, actor)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("CreateSessionGroup: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the group: %v", err)
	}
	assertKinds := func(what string, after int64, want string) {
		t.Helper()
		changes := changesAfter(t, ctx, pool, after)
		if len(changes) == 0 {
			t.Fatalf("%s logged nothing", what)
		}
		for _, change := range changes {
			if change.Kind != want {
				t.Fatalf("%s logged %+v, want only %s", what, changes, want)
			}
		}
	}
	assertKinds("a new session group", before, "session_group")

	before = latestChange(t, ctx, pool)
	if _, err := pool.Exec(ctx, `delete from delivery_session_group_members where session_id = 's2'`); err != nil {
		t.Fatalf("remove a member: %v", err)
	}
	assertKinds("a removed member", before, "session_group")

	before = latestChange(t, ctx, pool)
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := DeleteSessionGroup(ctx, tx, group.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("DeleteSessionGroup: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the delete: %v", err)
	}
	assertKinds("a deleted session group", before, "session_group")

	before = latestChange(t, ctx, pool)
	if _, err := PutSettings(ctx, pool, settings, actor); err != nil {
		t.Fatalf("PutSettings unchanged: %v", err)
	}
	if changes := changesAfter(t, ctx, pool, before); len(changes) != 0 {
		t.Fatalf("settings written again unchanged logged %+v, want nothing", changes)
	}
	settings.ProductionJobName = "widgets-release / production"
	if _, err := PutSettings(ctx, pool, settings, actor); err != nil {
		t.Fatalf("PutSettings changed: %v", err)
	}
	assertKinds("a settings change", before, "settings")
}

// TestDeletingAPullRequestLogsItsOldMergeTime: a deleted pull request logs its merge time as the
// time before and none after.
func TestDeletingAPullRequestLogsItsOldMergeTime(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	merged := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, changeTestPR(13, merged, "chore: task")); err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	before := latestChange(t, ctx, pool)
	if err := DeletePullRequest(ctx, pool, "acme/widgets", 13); err != nil {
		t.Fatalf("DeletePullRequest: %v", err)
	}
	changes := changesAfter(t, ctx, pool, before)
	if len(changes) != 1 || changes[0].Kind != "pull_request" || changes[0].Ref != "13" ||
		!sameTime(changes[0].TOld, merged) || changes[0].TNew != nil {
		t.Fatalf("a deleted pull request logged %+v, want TOld %v and no TNew", changes, merged)
	}
}
