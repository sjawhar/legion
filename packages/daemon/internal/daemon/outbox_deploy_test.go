package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// deployMerged is when acme/widgets#42 merged, as deployGitHub answers its pull request; every
// step's completion in the deploy tests is a duration from it.
var deployMerged = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// deployAttempt is the path of attempt 2 of run 901, the deploy run attempt the deploy tests'
// rows name, under acme/widgets's REST base.
const deployAttempt = "/repos/acme/widgets/actions/runs/901/attempts/2"

// deployRow is a check_compare row of attempt 2 of acme/widgets's envoy.yaml run 901 on main, its
// step Apply, queued for look 1 of the check opened at named.
func deployRow(t *testing.T, issue string, named time.Time) record.OutboxRow {
	t.Helper()
	row := mustOutboxRow(t, issue, record.CheckCompare{Look: 1, Cause: "deploy run", OpenedAt: named,
		Deploy: &record.DeployRun{Run: 901, Attempt: 2, Workflow: ".github/workflows/envoy.yaml", Step: "Apply", Branch: "main"}}, time.Now())
	row.ID = 73
	return row
}

// deployGitHub is GitHub's REST API for acme/widgets as a check_compare row of a deploy run calls
// it: the pull request, naming its merge time (null when unmerged) and merge commit; attempt 2 of
// run 901 as run — envoy.yaml on main from acme/widgets, a push, started ten minutes before the
// merge and so before every step the tests list, until a test changes it — or the HTTP code when
// one is set; and the attempt's jobs, a page of jobs per entry of pages, every page but the last
// naming the next in its Link header as GitHub does. It records each read's path and query, in
// order.
type deployGitHub struct {
	url      string
	mu       sync.Mutex
	merge    string
	unmerged bool
	run      map[string]any
	code     int
	pages    [][]string
	reads    []string
}

func newDeployGitHub(t *testing.T, merge string) *deployGitHub {
	t.Helper()
	g := &deployGitHub{merge: merge, run: map[string]any{
		"id": 901, "run_attempt": 2, "name": "envoy", "path": ".github/workflows/envoy.yaml", "head_branch": "main", "event": "push",
		"status": "completed", "conclusion": "success", "html_url": "https://github.com/acme/widgets/actions/runs/901/attempts/2",
		"run_started_at": deployMerged.Add(-10 * time.Minute).Format(time.RFC3339), "head_repository": map[string]any{"full_name": "acme/widgets"},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.reads = append(g.reads, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/42":
			merged := strconv.Quote(deployMerged.Format(time.RFC3339))
			if g.unmerged {
				merged = "null"
			}
			fmt.Fprintf(w, `{"number":42,"merged":%t,"merged_at":%s,"merge_commit_sha":%q}`, !g.unmerged, merged, g.merge)
		case r.Method == http.MethodGet && r.URL.Path == deployAttempt:
			if g.code != 0 {
				w.WriteHeader(g.code)
				_, _ = io.WriteString(w, `{"message":"`+http.StatusText(g.code)+`"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(g.run)
		case r.Method == http.MethodGet && r.URL.Path == deployAttempt+"/jobs":
			page := 1
			if p := r.URL.Query().Get("page"); p != "" {
				page, _ = strconv.Atoi(p)
			}
			if page < 1 || page > len(g.pages) {
				t.Errorf("unexpected jobs page %d of %d", page, len(g.pages))
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if page < len(g.pages) {
				next := fmt.Sprintf("http://%s%s/jobs?per_page=100&page=%d", r.Host, deployAttempt, page+1)
				w.Header().Set("Link", "<"+next+`>; rel="next"`)
			}
			total := 0
			for _, jobs := range g.pages {
				total += len(jobs)
			}
			fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, total, strings.Join(g.pages[page-1], ","))
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	g.url = server.URL
	return g
}

func (g *deployGitHub) read() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reads...)
}

// deployJobJSON is job id of run 901, named name, as GitHub lists it among attempt 2's jobs: under
// run_attempt 2, as GitHub lists every job of the attempt, those it kept from attempt 1 included,
// and its steps (deployStepJSON).
func deployJobJSON(id int64, name string, steps ...string) string {
	return fmt.Sprintf(`{"id":%d,"run_id":901,"run_attempt":2,"name":%q,"status":"completed","conclusion":"success","html_url":"https://github.com/acme/widgets/actions/runs/901/job/%d","steps":[%s]}`,
		id, name, id, strings.Join(steps, ","))
}

// deployStepJSON is a step of a job as GitHub lists it: its name, its conclusion, and when it
// completed, after the pull request's merge (a negative duration before it), a minute after it
// started.
func deployStepJSON(name, conclusion string, after time.Duration) string {
	completed := deployMerged.Add(after)
	return fmt.Sprintf(`{"name":%q,"status":"completed","conclusion":%q,"number":4,"started_at":%q,"completed_at":%q}`,
		name, conclusion, completed.Add(-time.Minute).Format(time.RFC3339), completed.Format(time.RFC3339))
}

// deployWakeDetail is the detail of the wake that job's Apply step gives, completed after after the
// merge.
func deployWakeDetail(job string, after time.Duration) string {
	return "acme/widgets .github/workflows/envoy.yaml run 901 attempt 2, job " + job + ": Apply succeeded at " + deployMerged.Add(after).Format(time.RFC3339)
}

// outboxWakeEvents is every event the outbox applied a fact under, in the order it applied them.
func outboxWakeEvents(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select event_id from processed_events where source = 'outbox' order by processed_at, event_id`)
	if err != nil {
		t.Fatalf("read the outbox's processed events: %v", err)
	}
	events, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the outbox's processed events: %v", err)
	}
	return events
}

// A check_compare row of a deploy run reads, as the implement App, the pull request's merge time,
// the run attempt, and the attempt's jobs across every page, and wakes the check once for each job
// whose Apply step ran in the attempt and succeeded strictly after the merge, in order of the
// step's completion, each under an event of the check, the run, the attempt and the job, the fact
// naming the check and the job. A skipped Apply, one that failed, one finished before or at the
// merge, or one of a job the attempt kept from an earlier one — listed under a new id and this
// attempt, with steps that started before the attempt did — wakes nothing, and the log says why;
// neither does a run of another workflow, branch or head repository, one a pull request triggered,
// or one GitHub does not know. A re-run Apply wakes under its new job id. A failed run whose Apply
// succeeded wakes. Any other failure, an attempt naming no start time, and a pull request naming
// no merge time, is the row's to retry. A check opened without its merge commit has it recorded
// from the pull request; a check closed since, or one a later merge replaced, reads nothing.
func TestACheckCompareOfADeployRunWakesTheCheckForEachApplyAfterTheMerge(t *testing.T) {
	const merge = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0"
	pull, run, jobs := "/repos/acme/widgets/pulls/42?", deployAttempt+"?", deployAttempt+"/jobs?per_page=100"
	applied := func(after time.Duration) string { return deployStepJSON("Apply", "success", after) }
	preview := deployStepJSON("Preview", "success", 2*time.Minute)
	// A re-run attempt started six minutes after the merge: an Apply of a job it kept from attempt
	// 1 started and finished before that, and a re-run one after.
	rerun := map[string]any{"run_started_at": deployMerged.Add(6 * time.Minute).Format(time.RFC3339)}
	type wake struct {
		job   int64
		name  string
		after time.Duration
	}
	for _, tc := range []struct {
		name     string
		recorded string
		unmerged bool
		run      map[string]any
		code     int
		pages    [][]string
		closed   bool
		replaced bool
		wantErr  string
		reads    []string
		wakes    []wake
		why      string
	}{
		{name: "an apply after the merge wakes once", recorded: merge,
			pages: [][]string{{deployJobJSON(10, "build"), deployJobJSON(11, "deploy", preview, applied(5*time.Minute))}},
			reads: []string{pull, run, jobs}, wakes: []wake{{11, "deploy", 5 * time.Minute}}},
		{name: "a preview-only run wakes nothing", recorded: merge,
			pages: [][]string{{deployJobJSON(11, "deploy", preview, deployStepJSON("Apply", "skipped", 3*time.Minute))}},
			reads: []string{pull, run, jobs}, why: `step "Apply" succeeded in no job of the attempt`},
		{name: "an apply finished before the merge wakes nothing", recorded: merge,
			pages: [][]string{{deployJobJSON(11, "deploy", applied(-5*time.Minute))}},
			reads: []string{pull, run, jobs}, why: `step "Apply" succeeded only at or before the merge`},
		{name: "an apply finished at the merge wakes nothing", recorded: merge,
			pages: [][]string{{deployJobJSON(11, "deploy", applied(0))}},
			reads: []string{pull, run, jobs}, why: `step "Apply" succeeded only at or before the merge`},
		{name: "a run with no apply step wakes nothing", recorded: merge,
			pages: [][]string{{deployJobJSON(10, "build", preview)}},
			reads: []string{pull, run, jobs}, why: `no job of the attempt has a step named "Apply"`},
		{name: "a run of another workflow wakes nothing", recorded: merge, run: map[string]any{"path": ".github/workflows/ci.yaml"},
			reads: []string{pull, run}},
		{name: "a run of another branch wakes nothing", recorded: merge, run: map[string]any{"head_branch": "feature"},
			reads: []string{pull, run}},
		{name: "a run of another head repository wakes nothing", recorded: merge, run: map[string]any{"head_repository": map[string]any{"full_name": "mallory/widgets"}},
			reads: []string{pull, run}},
		{name: "a pull_request run wakes nothing", recorded: merge, run: map[string]any{"event": "pull_request"},
			reads: []string{pull, run}},
		{name: "a pull_request_target run wakes nothing", recorded: merge, run: map[string]any{"event": "pull_request_target"},
			reads: []string{pull, run}},
		{name: "the head repository is compared without case", recorded: merge, run: map[string]any{"head_repository": map[string]any{"full_name": "Acme/Widgets"}},
			pages: [][]string{{deployJobJSON(11, "deploy", applied(5*time.Minute))}},
			reads: []string{pull, run, jobs}, wakes: []wake{{11, "deploy", 5 * time.Minute}}},
		{name: "a failed run with one successful apply wakes", recorded: merge, run: map[string]any{"conclusion": "failure"},
			pages: [][]string{{deployJobJSON(11, "deploy-a", applied(5*time.Minute)), deployJobJSON(12, "deploy-b", deployStepJSON("Apply", "failure", 6*time.Minute))}},
			reads: []string{pull, run, jobs}, wakes: []wake{{11, "deploy-a", 5 * time.Minute}}},
		{name: "two applying jobs wake twice in order of their applies", recorded: merge,
			pages: [][]string{{deployJobJSON(21, "deploy-east", applied(9*time.Minute)), deployJobJSON(22, "deploy-west", applied(4*time.Minute))}},
			reads: []string{pull, run, jobs}, wakes: []wake{{22, "deploy-west", 4 * time.Minute}, {21, "deploy-east", 9 * time.Minute}}},
		{name: "an apply job the attempt kept from an earlier one wakes nothing", recorded: merge, run: rerun,
			pages: [][]string{{deployJobJSON(31, "deploy", applied(5*time.Minute))}},
			reads: []string{pull, run, jobs}, why: `step "Apply" succeeded only in jobs this attempt kept from an earlier one`},
		{name: "a re-run apply job wakes under its new job id", recorded: merge, run: rerun,
			pages: [][]string{{deployJobJSON(32, "deploy", applied(8*time.Minute))}},
			reads: []string{pull, run, jobs}, wakes: []wake{{32, "deploy", 8 * time.Minute}}},
		{name: "of a kept apply job and a re-run one only the re-run one wakes", recorded: merge, run: rerun,
			pages: [][]string{{deployJobJSON(33, "deploy-east", applied(5*time.Minute)), deployJobJSON(34, "deploy-west", applied(8*time.Minute))}},
			reads: []string{pull, run, jobs}, wakes: []wake{{34, "deploy-west", 8 * time.Minute}}},
		{name: "jobs over two pages are read", recorded: merge,
			pages: [][]string{{deployJobJSON(41, "build", preview)}, {deployJobJSON(42, "deploy", applied(8*time.Minute))}},
			reads: []string{pull, run, jobs, deployAttempt + "/jobs?per_page=100&page=2"}, wakes: []wake{{42, "deploy", 8 * time.Minute}}},
		{name: "a run GitHub does not know wakes nothing", recorded: merge, code: http.StatusNotFound, reads: []string{pull, run}},
		{name: "another failure retries", recorded: merge, code: http.StatusInternalServerError, reads: []string{pull, run}, wantErr: "500"},
		{name: "an attempt naming no start time retries", recorded: merge, run: map[string]any{"run_started_at": nil}, reads: []string{pull, run}, wantErr: "names no start time"},
		{name: "a pull request naming no merge time retries", recorded: merge, unmerged: true, reads: []string{pull}, wantErr: "names no merge time"},
		{name: "a missing merge commit is recorded", pages: [][]string{{deployJobJSON(11, "deploy", applied(5*time.Minute))}},
			reads: []string{pull, run, jobs}, wakes: []wake{{11, "deploy", 5 * time.Minute}}},
		{name: "a closed check reads nothing", recorded: merge, closed: true},
		{name: "a replaced check reads nothing", recorded: merge, replaced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := checkRoot()
			putOutboxIssue(t, pool, records, root)
			check := openCheck(root, tc.recorded)
			if tc.closed {
				closed := time.Now()
				check.LookOpen, check.ClosedAt, check.Outcome = false, &closed, record.CheckRecorded
			}
			putProductionCheck(t, pool, records, check)
			github := newDeployGitHub(t, merge)
			github.unmerged, github.code, github.pages = tc.unmerged, tc.code, tc.pages
			for key, value := range tc.run {
				github.run[key] = value
			}
			facts := &outboxFactHandler{}
			var logged bytes.Buffer
			runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, tokens: outboxTokens{}, githubAPI: github.url, project: "legion",
				handlers: []intake.Handler{facts}, log: slog.New(slog.NewJSONHandler(&logged, nil)), now: time.Now}
			named := check.OpenedAt
			if tc.replaced {
				named = named.Add(-24 * time.Hour)
			}

			err := runner.execute(context.Background(), deployRow(t, root.Key, named))
			if tc.wantErr == "" && err != nil {
				t.Fatalf("check compare = %v, want it finished", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("check compare = %v, want a failure naming %s", err, tc.wantErr)
			}
			if got := github.read(); strings.Join(got, " ") != strings.Join(tc.reads, " ") {
				t.Fatalf("GitHub reads = %v, want %v", got, tc.reads)
			}
			got, events := facts.facts(), outboxWakeEvents(t, pool)
			if len(got) != len(tc.wakes) || len(events) != len(tc.wakes) {
				t.Fatalf("facts applied = %+v under %v, want %+v", got, events, tc.wakes)
			}
			for i, w := range tc.wakes {
				want := intake.CheckWake{Issue: root.Key, Look: 1, Cause: "deploy run", Detail: deployWakeDetail(w.name, w.after)}
				if !sameCheckFact(got[i], want, check) {
					t.Fatalf("fact %d = %+v, want %+v", i, got[i], want)
				}
				if event := fmt.Sprintf("check-wake:%s:%s:deploy run:901:2:%d", root.Key, check.ID(), w.job); events[i] != event {
					t.Fatalf("fact %d applied under %q, want %q", i, events[i], event)
				}
			}
			if tc.why != "" {
				lines := loggedLines(t, logged.String(), "the run did not deploy after the merge")
				if len(lines) != 1 || lines[0]["why"] != tc.why {
					t.Fatalf("logged %v, want the run said not to have deployed after the merge because %s", lines, tc.why)
				}
			}
			if !tc.closed && !tc.replaced {
				if recorded := readProductionCheck(t, pool, records, root.Key); recorded.MergeSHA != merge {
					t.Fatalf("the check's merge commit = %q, want %s recorded", recorded.MergeSHA, merge)
				}
			}
		})
	}
}

// The same deploy run compared twice — a second delivery of its webhook, or a second engine row —
// adds nothing: each wake's event is a duplicate of the first row's, so the workflow is not asked
// again.
func TestACheckCompareOfTheSameDeployRunWakesOnce(t *testing.T) {
	const merge = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, merge)
	putProductionCheck(t, pool, records, check)
	github := newDeployGitHub(t, merge)
	github.pages = [][]string{{deployJobJSON(11, "deploy", deployStepJSON("Apply", "success", 5*time.Minute))}}
	facts := &outboxFactHandler{}
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, tokens: outboxTokens{}, githubAPI: github.url, project: "legion",
		handlers: []intake.Handler{facts}, log: quietLogger(), now: time.Now}
	for id := int64(73); id <= 74; id++ {
		row := deployRow(t, root.Key, check.OpenedAt)
		row.ID = id
		if err := runner.execute(context.Background(), row); err != nil {
			t.Fatalf("check compare row %d = %v", id, err)
		}
	}
	if got, events := facts.facts(), outboxWakeEvents(t, pool); len(got) != 1 || len(events) != 1 {
		t.Fatalf("facts applied = %+v under %v, want the one wake", got, events)
	}
}

// A deploy run GitHub names still running wakes nothing, whatever the envelope its row was queued
// from claimed, since anyone holding an Envoy bearer can publish a `completed` one: the row reads no
// jobs, records no event, logs that the run has not completed, and finishes rather than retrying.
// The same run, attempt and job answered `completed` later then wakes the check under the job's
// event, which the earlier row left unspent.
func TestACheckCompareOfADeployRunStillRunningWakesNothingUntilItCompletes(t *testing.T) {
	const merge = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0"
	pull, run, jobs := "/repos/acme/widgets/pulls/42?", deployAttempt+"?", deployAttempt+"/jobs?per_page=100"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, merge)
	putProductionCheck(t, pool, records, check)
	github := newDeployGitHub(t, merge)
	github.pages = [][]string{{deployJobJSON(11, "deploy", deployStepJSON("Apply", "success", 5*time.Minute))}}
	github.mu.Lock()
	github.run["status"] = "in_progress"
	github.mu.Unlock()
	facts := &outboxFactHandler{}
	var logged bytes.Buffer
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, tokens: outboxTokens{}, githubAPI: github.url, project: "legion",
		handlers: []intake.Handler{facts}, log: slog.New(slog.NewJSONHandler(&logged, nil)), now: time.Now}

	if err := runner.execute(context.Background(), deployRow(t, root.Key, check.OpenedAt)); err != nil {
		t.Fatalf("check compare of the running attempt = %v, want it finished", err)
	}
	if got := github.read(); strings.Join(got, " ") != strings.Join([]string{pull, run}, " ") {
		t.Fatalf("GitHub reads of the running attempt = %v, want the pull request and the attempt, no jobs", got)
	}
	if got, events := facts.facts(), outboxWakeEvents(t, pool); len(got) != 0 || len(events) != 0 {
		t.Fatalf("facts applied for the running attempt = %+v under %v, want none", got, events)
	}
	lines := loggedLines(t, logged.String(), "the run has not completed")
	if len(lines) != 1 || lines[0]["status"] != "in_progress" || lines[0]["url"] != "https://github.com/acme/widgets/actions/runs/901/attempts/2" {
		t.Fatalf("logged %v, want the run said not to have completed, naming its status and url", lines)
	}

	github.mu.Lock()
	github.run["status"] = "completed"
	github.mu.Unlock()
	row := deployRow(t, root.Key, check.OpenedAt)
	row.ID = 74
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("check compare of the completed attempt = %v, want it finished", err)
	}
	if got := github.read(); strings.Join(got, " ") != strings.Join([]string{pull, run, pull, run, jobs}, " ") {
		t.Fatalf("GitHub reads after the completed attempt = %v, want its jobs read", got)
	}
	got, events := facts.facts(), outboxWakeEvents(t, pool)
	want := intake.CheckWake{Issue: root.Key, Look: 1, Cause: "deploy run", Detail: deployWakeDetail("deploy", 5*time.Minute)}
	if len(got) != 1 || len(events) != 1 || !sameCheckFact(got[0], want, check) {
		t.Fatalf("facts applied after the completed attempt = %+v under %v, want the one wake %+v", got, events, want)
	}
	if event := fmt.Sprintf("check-wake:%s:%s:deploy run:901:2:11", root.Key, check.ID()); events[0] != event {
		t.Fatalf("the wake applied under %q, want %q", events[0], event)
	}
}

// Two applying jobs of one run are two deploys the workflow sees: with no look open, the first job's
// wake starts the next look, woken by the deploy run naming that job, and the second's is kept for
// the look after, as any wake arriving while a look is open is.
func TestTwoApplyingJobsOfADeployRunStartALookAndKeepTheNext(t *testing.T) {
	const merge = "a1b2c3d4e5f6a7b8c9d0a1b2c3d4e5f6a7b8c9d0"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := checkRoot()
	putOutboxIssue(t, pool, records, root)
	check := openCheck(root, merge)
	check.LookOpen = false
	putProductionCheck(t, pool, records, check)
	github := newDeployGitHub(t, merge)
	github.pages = [][]string{{
		deployJobJSON(21, "deploy-east", deployStepJSON("Apply", "success", 9*time.Minute)),
		deployJobJSON(22, "deploy-west", deployStepJSON("Apply", "success", 4*time.Minute)),
	}}
	engine := workflow.New(records, workflow.Config{Project: "legion", Linger: 24 * time.Hour}, quietLogger())
	runner := &outbox{pool: pool, dispatchProject: "LEGION", records: records, tokens: outboxTokens{}, githubAPI: github.url, project: "legion",
		handlers: []intake.Handler{engine}, log: quietLogger(), now: time.Now}

	if err := runner.execute(context.Background(), deployRow(t, root.Key, check.OpenedAt)); err != nil {
		t.Fatalf("check compare = %v, want it finished", err)
	}
	got := readProductionCheck(t, pool, records, root.Key)
	if got.Looks != 2 || !got.LookOpen || got.LastWakeKind != "deploy run" || got.LastWakeDetail != deployWakeDetail("deploy-west", 4*time.Minute) {
		t.Fatalf("the check after the run = look %d open %t woken by %q %q; want look 2 open, woken by the deploy run's first apply", got.Looks, got.LookOpen, got.LastWakeKind, got.LastWakeDetail)
	}
	if got.PendingCauseKind != "deploy run" || got.PendingCauseDetail != deployWakeDetail("deploy-east", 9*time.Minute) {
		t.Fatalf("the check's kept cause = %q %q; want the deploy run's second apply kept for the next look", got.PendingCauseKind, got.PendingCauseDetail)
	}
}
