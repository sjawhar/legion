package delivery

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// statementRecorder records the SQL of every statement run on the pool it traces.
type statementRecorder struct {
	mu  sync.Mutex
	sql []string
}

func (r *statementRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.sql = append(r.sql, data.SQL)
	r.mu.Unlock()
	return ctx
}

func (r *statementRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *statementRecorder) statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sql...)
}

// recordedDeliveryPool is a pool of its own on pool's database whose every statement is recorded.
func recordedDeliveryPool(t *testing.T, pool *store.Pool) (*store.Pool, *statementRecorder) {
	t.Helper()
	config := pool.Config().Copy()
	recorder := &statementRecorder{}
	config.ConnConfig.Tracer = recorder
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open recorded pool: %v", err)
	}
	recorded := store.NewPool(opened)
	t.Cleanup(recorded.Close)
	return recorded, recorder
}

// timelineWrite is a statement that writes one of the timeline's tables directly. The bounded
// attribution walk (AttributePullRequests) is not one: it is a `with` statement that re-resolves up
// to AttributionResolveBatch stored rows every pass, and is held apart by the row comparison below.
var timelineWrite = regexp.MustCompile(`(?is)^\s*(insert\s+into|update|delete\s+from)\s+delivery_(pull_requests|runs|run_jobs)\b`)

// timelineRows is every stored pull request, run and job as one comparable text each, leaving out
// only attribution_checked_at, the stamp the bounded attribution walk moves on the rows it reads.
func timelineRows(t *testing.T, ctx context.Context, pool *store.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		select 'pr ' || (to_jsonb(pr) - 'attribution_checked_at')::text from delivery_pull_requests pr
		union all select 'run ' || to_jsonb(r)::text from delivery_runs r
		union all select 'job ' || to_jsonb(j)::text from delivery_run_jobs j
		order by 1`)
	if err != nil {
		t.Fatalf("read the timeline rows: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the timeline rows: %v", err)
	}
	return got
}

// quietReconcilePass stores prs merged pull requests a month old, with their attribution inputs
// read, beside one finished deploy run and its job, against a GitHub that has nothing new: the
// merged-PR search and both run listings answer nothing, and every runs backfill has finished. It
// runs one reconcile pass to settle the progress rows, then a second on a recorded pool, and
// answers that second pass's statements once it has proved the pass succeeded and changed no
// stored row, logged no change and made no direct write to the timeline's tables.
func quietReconcilePass(t *testing.T, prs int) []string {
	t.Helper()
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	finished := time.Now().UTC()
	for _, backfill := range []struct{ step, path string }{
		{backfillStep(DeliveryRunKindDeploy), ".github/workflows/deploy.yml"},
		{backfillStep(DeliveryRunKindPRChecks), ".github/workflows/pr-checks.yml"},
		{createdAtBackfillStep, ".github/workflows/deploy.yml"},
	} {
		if err := StartBackfillProgress(ctx, pool, backfill.step, runsProgressScope("acme/widgets", backfill.path), finished, finished); err != nil {
			t.Fatalf("finish the %s backfill: %v", backfill.step, err)
		}
	}
	merged := finished.Add(-30 * 24 * time.Hour).Truncate(time.Second)
	for number := 1; number <= prs; number++ {
		pr := changeTestPR(number, merged.Add(time.Duration(number)*time.Minute), "feat: settled")
		pr.Attribution = &AttributionInputs{}
		if err := UpsertPullRequest(ctx, pool, pr); err != nil {
			t.Fatalf("seed #%d: %v", number, err)
		}
	}
	run := changeTestRun(700, DeliveryRunKindDeploy, merged, merged.Add(time.Minute))
	completed := merged.Add(time.Hour)
	success := DeliveryRunConclusionSuccess
	run.CompletedAt, run.Conclusion = &completed, &success
	if err := UpsertRun(ctx, pool, run); err != nil {
		t.Fatalf("seed the run: %v", err)
	}
	jobSuccess := DeliveryJobConclusionSuccess
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 700, []DeliveryRunJob{{
		Repo: "acme/widgets", RunID: 700, Name: "widgets-release / widgets-release", StartedAt: &merged, CompletedAt: &completed, Conclusion: &jobSuccess,
	}}, true); err != nil {
		t.Fatalf("seed the job: %v", err)
	}

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	noRuns(t, fake)
	github := fake.newTestClient()
	NewReconcile(pool, github, events.NewBroker()).runOnce(ctx)

	before := timelineRows(t, ctx, pool)
	changesBefore := latestChange(t, ctx, pool)
	recorded, recorder := recordedDeliveryPool(t, pool)
	NewReconcile(recorded, github, events.NewBroker()).runOnce(ctx)

	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if settings.LastError != nil || settings.LastReconcileAt == nil {
		t.Fatalf("the pass recorded last_error %v and last_reconcile_at %v; want a successful pass", settings.LastError, settings.LastReconcileAt)
	}
	if after := timelineRows(t, ctx, pool); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("a pass with nothing new changed the stored timeline:\nbefore %v\nafter  %v", before, after)
	}
	if changes := changesAfter(t, ctx, pool, changesBefore); len(changes) != 0 {
		t.Fatalf("a pass with nothing new logged %+v, want nothing", changes)
	}
	statements := recorder.statements()
	for _, sql := range statements {
		if timelineWrite.MatchString(sql) {
			t.Errorf("a pass with nothing new wrote the timeline directly: %s", strings.TrimSpace(sql))
		}
	}
	return statements
}

// A reconcile pass that GitHub has nothing new for costs a constant number of statements, however
// many pull requests are stored: the timer stays (GitHub has no feed of the webhooks it would
// miss), so what it reads every five minutes must not grow with the table. 50 stored rows sit
// well inside one bounded attribution batch, so they cost the pass no statement more than one.
func TestAReconcilePassWithNothingNewCostsTheSameStatementsWhateverIsStored(t *testing.T) {
	one := quietReconcilePass(t, 1)
	fifty := quietReconcilePass(t, 50)
	if len(fifty) != len(one) {
		t.Fatalf("a pass with nothing new ran %d statements over 1 stored pull request and %d over 50; want the same\nwith 1:\n%s\nwith 50:\n%s",
			len(one), len(fifty), strings.Join(one, "\n---\n"), strings.Join(fifty, "\n---\n"))
	}
	t.Logf("a pass with nothing new ran %d statements over 1 stored pull request and over 50", len(one))
}
