package docs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// statementCounter counts every statement started on the pool it traces.
type statementCounter struct{ count atomic.Int64 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.count.Add(1)
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countedStore is database through a pool of its own whose every statement the returned counter
// counts, the lease pool built from it included (store.Pool.Leases). Only the code under test is
// given it, so everything it counts is that code's.
func countedStore(t *testing.T, database *store.Store) (*store.Store, *statementCounter) {
	t.Helper()
	config := database.Pool.Config().Copy()
	counter := &statementCounter{}
	config.ConnConfig.Tracer = counter
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a counted pool: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}, counter
}

// awaitQuiet waits until counter has counted no statement for quiet, failing once within passes.
func awaitQuiet(t *testing.T, counter *statementCounter, quiet, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	last, since := counter.count.Load(), time.Now()
	for time.Since(since) < quiet {
		if time.Now().After(deadline) {
			t.Fatalf("statements did not stop for %v within %v", quiet, within)
		}
		time.Sleep(20 * time.Millisecond)
		if n := counter.count.Load(); n != last {
			last, since = n, time.Now()
		}
	}
}

// requireNoStatements fails if counter counts any statement over window.
func requireNoStatements(t *testing.T, counter *statementCounter, window time.Duration, what string) {
	t.Helper()
	before := counter.count.Load()
	time.Sleep(window)
	if made := counter.count.Load() - before; made != 0 {
		t.Errorf("%s: %d statements over %v, want none", what, made, window)
	}
}

// captureLogs sends the default logger's lines to the returned log until the test ends.
func captureLogs(t *testing.T) *storetest.LockedLog {
	t.Helper()
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// seedUncountedIssue commits issue TP-<number> whose primary document's version 1 is markdown,
// with no task count stored, as the migration that added the count left every row.
func seedUncountedIssue(t *testing.T, database *store.Store, number int, markdown string) string {
	t.Helper()
	ctx := context.Background()
	key := fmt.Sprintf("TP-%d", number)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into projects (key, name) values ('TP', 'Task progress') on conflict (key) do nothing`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'TP', $2, 'Issue', '{"kind":"user","id":"alice"}', 'U')
	`, key, number); err != nil {
		t.Fatalf("create issue %s: %v", key, err)
	}
	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, 'TP', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text
	`, key).Scan(&artifactID); err != nil {
		t.Fatalf("create the primary document of %s: %v", key, err)
	}
	if _, err := tx.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, $2, '[]')`, artifactID, markdown); err != nil {
		t.Fatalf("write version 1 of %s: %v", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return key
}

// taskCount reads an issue's stored task count; version is -1 while none is stored.
func taskCount(t *testing.T, database *store.Store, key string) (done, total, version int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(tasks_done, -1), coalesce(tasks_total, -1), coalesce(tasks_version, -1) from issues where key = $1
	`, key).Scan(&done, &total, &version); err != nil {
		t.Fatalf("read the task count of %s: %v", key, err)
	}
	return done, total, version
}

// runTaskProgress runs RunTaskProgressReconciliation until it returns or the test ends, and
// returns a channel closed when it returns.
func runTaskProgress(t *testing.T, service *Service) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunTaskProgressReconciliation(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return done
}

// The reconciliation counts every uncounted issue once at start and then returns: every version
// write counts its own, so it keeps no timer and makes no statement after the boot pass.
func TestTaskProgressIsCountedAtStartAndThenNothingRuns(t *testing.T) {
	database := storetest.Open(t)
	keys := []string{
		seedUncountedIssue(t, database, 1, "- [x] a\n- [ ] b\n"),
		seedUncountedIssue(t, database, 2, "- [x] a\n- [x] b\n- [ ] c\n"),
	}
	counted, counter := countedStore(t, database)
	service := New(Deps{Store: counted, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	done := runTaskProgress(t, service)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconciliation did not return after its boot pass found every row counted")
	}
	for index, want := range [][3]int{{1, 2, 1}, {2, 3, 1}} {
		if tasksDone, total, version := taskCount(t, database, keys[index]); [3]int{tasksDone, total, version} != want {
			t.Fatalf("%s counted %d/%d at version %d; want %d/%d at version %d", keys[index], tasksDone, total, version, want[0], want[1], want[2])
		}
	}
	requireNoStatements(t, counter, time.Second, "task progress after its boot pass")
}

// A row another writer holds through the whole boot pass - its own retries included - is left
// to the retries after it, and the first of those after the writer lets go counts it; the
// reconciliation then returns.
func TestTaskProgressLockedThroughTheBootPassIsCountedByARetry(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	free := seedUncountedIssue(t, database, 1, "- [x] a\n- [ ] b\n")
	held := seedUncountedIssue(t, database, 2, "- [x] a\n- [x] b\n- [ ] c\n")
	logs := captureLogs(t)
	writer, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := writer.Exec(ctx, `select 1 from issues where key = $1 for update`, held); err != nil {
		t.Fatalf("hold %s: %v", held, err)
	}
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	done := runTaskProgress(t, service)
	waitFor(t, 20*time.Second, "the boot pass to give up on the held row", func() bool {
		return strings.Contains(logs.String(), "issue task progress reconciliation pass left drift")
	})
	if tasksDone, total, version := taskCount(t, database, free); tasksDone != 1 || total != 2 || version != 1 {
		t.Fatalf("%s counted %d/%d at version %d by the boot pass; want 1/2 at version 1", free, tasksDone, total, version)
	}
	if _, _, version := taskCount(t, database, held); version != -1 {
		t.Fatalf("%s counted at version %d while held; want no count", held, version)
	}
	select {
	case <-done:
		t.Fatal("the reconciliation returned with a held row still uncounted")
	default:
	}

	if err := writer.Rollback(ctx); err != nil {
		t.Fatalf("release %s: %v", held, err)
	}
	waitFor(t, 10*time.Second, "a retry to count the released row", func() bool {
		_, _, version := taskCount(t, database, held)
		return version == 1
	})
	if tasksDone, total, _ := taskCount(t, database, held); tasksDone != 2 || total != 3 {
		t.Fatalf("%s counted %d/%d; want 2/3", held, tasksDone, total)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconciliation did not return once no drift was left")
	}
}
