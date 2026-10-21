package delivery

// The issue.deployed event (deployed.go): when the timeline writes it, what it carries, that it is
// written once, and that both the intake and the reconcile write it.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// deployedIssue is the issue the deployed tests attribute their pull requests to, and
// deployedOtherIssue the one a pull request outside the deploy repository names.
const (
	deployedIssue      = "DEPL-1"
	deployedOtherIssue = "DEPL-2"
)

// seedDeployedIssues writes project DEPL and its two issues, and clears their events, so a run
// against a database an earlier run left holds only what this test writes. The pull requests and
// runs are cleared by deliveryTestPool.
func seedDeployedIssues(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `insert into projects (key, name) values ('DEPL', 'Deployed') on conflict do nothing`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for _, key := range []string{deployedIssue, deployedOtherIssue} {
		number, err := strconv.Atoi(strings.TrimPrefix(key, "DEPL-"))
		if err != nil {
			t.Fatalf("issue key %q is not DEPL-<n>", key)
		}
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, created_by, rank)
			values ($1, 'DEPL', $2, $1, 'done', '{"kind":"system","id":"delivery-test"}', $1)
			on conflict do nothing`, key, number); err != nil {
			t.Fatalf("seed issue %s: %v", key, err)
		}
		if _, err := pool.Exec(ctx, `delete from events where issue_key = $1`, key); err != nil {
			t.Fatalf("clear the events of %s: %v", key, err)
		}
	}
}

// seedMergedPullRequest stores a complete, attributed pull request of repo merged at merged, its
// attribution inputs read and naming issue, so the reconcile's own re-attribution resolves the
// same issue and its inputs backfill leaves the row alone.
func seedMergedPullRequest(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int, merged time.Time, issue string) {
	t.Helper()
	created := merged.Add(-time.Hour)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: repo, Number: number, Title: fmt.Sprintf("%s: a widget", issue), URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		Author: "octocat", CreatedAt: &created, MergedAt: &merged, IssueKey: &issue,
		Attribution: &AttributionInputs{TitleKeys: []string{issue}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}},
	}); err != nil {
		t.Fatalf("seed %s#%d: %v", repo, number, err)
	}
}

// seedDeployRun stores a deploy run of acme/widgets on main, created at created, whose one
// production job (seedDeliverySettings' name) concluded with conclusion at done.
func seedDeployRun(t *testing.T, ctx context.Context, pool *store.Pool, runID int64, created, done time.Time, conclusion DeliveryJobConclusion) {
	t.Helper()
	const repo = "acme/widgets"
	if err := UpsertRun(ctx, pool, DeliveryRun{
		Repo: repo, RunID: runID, Kind: DeliveryRunKindDeploy, HeadSHA: fmt.Sprintf("%040d", runID),
		HeadCommitAt: created, StartedAt: created, CreatedAt: &created, CompletedAt: &done,
		URL: fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", runID), HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("seed run %d: %v", runID, err)
	}
	if err := UpsertRunJobs(ctx, pool, repo, runID, []DeliveryRunJob{{
		Repo: repo, RunID: runID, Name: "widgets-release / widgets-release", StartedAt: &created, CompletedAt: &done, Conclusion: &conclusion,
	}}, true); err != nil {
		t.Fatalf("seed run %d's jobs: %v", runID, err)
	}
}

// deployedEvent is one stored issue.deployed event, as the daemon's decoder and the API read it.
type deployedEvent struct {
	Seq     int
	Actor   model.Actor
	Notify  bool
	Payload map[string]json.RawMessage
}

// deployedEvents reads issue's stored issue.deployed events, oldest first.
func deployedEvents(t *testing.T, ctx context.Context, pool *store.Pool, issue string) []deployedEvent {
	t.Helper()
	rows, err := pool.Query(ctx, `select seq, actor, notify, payload from events where issue_key = $1 and type = $2 order by id`, issue, DeployedEventType)
	if err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	defer rows.Close()
	var found []deployedEvent
	for rows.Next() {
		var e deployedEvent
		var actor, payload []byte
		if err := rows.Scan(&e.Seq, &actor, &e.Notify, &payload); err != nil {
			t.Fatalf("scan an event of %s: %v", issue, err)
		}
		if err := json.Unmarshal(actor, &e.Actor); err != nil {
			t.Fatalf("decode an event actor: %v", err)
		}
		if err := json.Unmarshal(payload, &e.Payload); err != nil {
			t.Fatalf("decode an event payload: %v", err)
		}
		found = append(found, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the events of %s: %v", issue, err)
	}
	return found
}

// deployedEventAt reads the marker of repo#number.
func deployedEventAt(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(ctx, `select deployed_event_at from delivery_pull_requests where repo = $1 and number = $2`, repo, number).Scan(&at); err != nil {
		t.Fatalf("read the marker of %s#%d: %v", repo, number, err)
	}
	return at
}

// requireDeployedEvent holds that issue carries exactly one issue.deployed, by the system actor,
// quiet, whose payload is exactly {repo, number, run_id, completed_at} with the given values.
func requireDeployedEvent(t *testing.T, ctx context.Context, pool *store.Pool, issue, repo string, number int, runID int64, completedAt string) {
	t.Helper()
	found := deployedEvents(t, ctx, pool, issue)
	if len(found) != 1 {
		t.Fatalf("%s carries %d issue.deployed events, want 1", issue, len(found))
	}
	event := found[0]
	if event.Actor != DeployedEventActor || event.Notify || event.Seq <= 0 {
		t.Fatalf("issue.deployed actor %+v, notify %v, seq %d; want the delivery-timeline system actor, quiet, a positive sequence", event.Actor, event.Notify, event.Seq)
	}
	want := map[string]string{
		"repo": strconv.Quote(repo), "number": strconv.Itoa(number), "run_id": strconv.FormatInt(runID, 10), "completed_at": strconv.Quote(completedAt),
	}
	if len(event.Payload) != len(want) {
		t.Fatalf("issue.deployed payload has keys %v, want exactly %v", slices.Sorted(maps.Keys(event.Payload)), want)
	}
	for key, value := range want {
		if got := string(event.Payload[key]); got != value {
			t.Errorf("issue.deployed payload %s = %s, want %s", key, got, value)
		}
	}
}

// TestDeployedEventWaitsForAShippingRun: a merged pull request attributed to an issue writes no
// event while no deploy has shipped it: with no deploy run at all, with one created before its
// merge, and with a failed one created after it.
func TestDeployedEventWaitsForAShippingRun(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	seedDeployedIssues(t, ctx, pool)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	seedMergedPullRequest(t, ctx, pool, "acme/widgets", 501, merged, deployedIssue)
	broker := events.NewBroker()

	pass := func(what string) {
		t.Helper()
		written, err := RecordDeployedEvents(ctx, pool, settings, broker, time.Now())
		if err != nil {
			t.Fatalf("%s: RecordDeployedEvents: %v", what, err)
		}
		if written != 0 {
			t.Fatalf("%s: wrote %d issue.deployed events, want none", what, written)
		}
		if found := deployedEvents(t, ctx, pool, deployedIssue); len(found) != 0 {
			t.Fatalf("%s: %s carries %d issue.deployed events, want none", what, deployedIssue, len(found))
		}
		if at := deployedEventAt(t, ctx, pool, "acme/widgets", 501); at != nil {
			t.Fatalf("%s: the marker is set at %v, want unset", what, at)
		}
	}
	pass("no deploy run")
	seedDeployRun(t, ctx, pool, 899, merged.Add(-time.Hour), merged.Add(-30*time.Minute), DeliveryJobConclusionSuccess)
	pass("a deploy created before the merge")
	seedDeployRun(t, ctx, pool, 900, merged.Add(time.Hour), merged.Add(70*time.Minute), DeliveryJobConclusionFailure)
	pass("a failed deploy created after the merge")
}

// TestDeployedEventIsWrittenOncePerPullRequest: a deploy run created after the merge whose
// production job succeeded writes one issue.deployed on the pull request's issue, with the payload
// the daemon decodes, and sets the marker to the pass's time; a second pass writes none; a pull
// request outside the deploy repository, attributed and merged before that run, writes none.
func TestDeployedEventIsWrittenOncePerPullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	seedDeployedIssues(t, ctx, pool)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	seedMergedPullRequest(t, ctx, pool, "acme/widgets", 501, merged, deployedIssue)
	seedMergedPullRequest(t, ctx, pool, "acme/other", 77, merged, deployedOtherIssue)
	seedDeployRun(t, ctx, pool, 900, merged.Add(time.Hour), merged.Add(70*time.Minute), DeliveryJobConclusionSuccess)
	broker := events.NewBroker()
	now := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC)

	written, err := RecordDeployedEvents(ctx, pool, settings, broker, now)
	if err != nil {
		t.Fatalf("RecordDeployedEvents: %v", err)
	}
	if written != 1 {
		t.Fatalf("wrote %d issue.deployed events, want 1", written)
	}
	requireDeployedEvent(t, ctx, pool, deployedIssue, "acme/widgets", 501, 900, "2024-01-01T02:10:00Z")
	if at := deployedEventAt(t, ctx, pool, "acme/widgets", 501); at == nil || !at.Equal(now) {
		t.Fatalf("the marker is %v, want the pass's time %v", at, now)
	}
	if found := deployedEvents(t, ctx, pool, deployedOtherIssue); len(found) != 0 {
		t.Fatalf("%s, whose pull request is outside the deploy repository, carries %d issue.deployed events, want none", deployedOtherIssue, len(found))
	}
	if at := deployedEventAt(t, ctx, pool, "acme/other", 77); at != nil {
		t.Fatalf("the marker of acme/other#77 is set at %v, want unset", at)
	}

	written, err = RecordDeployedEvents(ctx, pool, settings, broker, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second RecordDeployedEvents: %v", err)
	}
	if written != 0 {
		t.Fatalf("the second pass wrote %d issue.deployed events, want none", written)
	}
	requireDeployedEvent(t, ctx, pool, deployedIssue, "acme/widgets", 501, 900, "2024-01-01T02:10:00Z")
	if at := deployedEventAt(t, ctx, pool, "acme/widgets", 501); at == nil || !at.Equal(now) {
		t.Fatalf("the second pass moved the marker to %v, want the first pass's %v", at, now)
	}
}

// TestReconcileWritesTheDeployedEvent: a deploy run the reconcile catches (the deploy workflow's
// run listing, as TestReconcileCatchesAMissedDeployRun) ships a pull request stored earlier, and
// the same pass writes the issue's event.
func TestReconcileWritesTheDeployedEvent(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedDeployedIssues(t, ctx, pool)
	seedMergedPullRequest(t, ctx, pool, "acme/widgets", 501, time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC), deployedIssue)

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 1, "workflow_runs": []map[string]any{shippedRun(900)}})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/runs/900/jobs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, shippedRunJobs())
	})

	NewReconcile(pool, fake.newTestClient(), events.NewBroker()).runOnce(ctx)

	requireDeployedEvent(t, ctx, pool, deployedIssue, "acme/widgets", 501, 900, "2024-01-01T02:10:00Z")
	if at := deployedEventAt(t, ctx, pool, "acme/widgets", 501); at == nil {
		t.Fatal("the reconcile left the marker unset")
	}
	settings, err := GetSettings(ctx, pool)
	if err != nil || settings.LastError != nil || settings.LastReconcileAt == nil {
		t.Fatalf("settings after the pass: %+v, %v; want a healthy pass", settings, err)
	}
}

// TestIntakeWritesTheDeployedEvent: a deploy run's envelope the intake handles (route, as the
// durable hands it over) stores the run and its jobs, and the same envelope ends with the issue's
// event. The envelope names only the run; its facts come from GitHub, as intake.go requires.
func TestIntakeWritesTheDeployedEvent(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	seedDeployedIssues(t, ctx, pool)
	seedMergedPullRequest(t, ctx, pool, "acme/widgets", 501, time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC), deployedIssue)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/900", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, shippedRun(900))
	})
	fake.handle("GET /repos/acme/widgets/actions/runs/900/jobs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, shippedRunJobs())
	})
	intake := newIntake(nil, pool, deliveryTestLeases(t, pool), fake.newTestClient(), events.NewBroker())

	envelope, err := json.Marshal(map[string]string{
		"kind": "workflow", "action": "completed", "repo": "acme/widgets",
		"path": settings.DeployWorkflowPath, "run_id": "900",
	})
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	handled, err := intake.route(ctx, settings, string(envelope))
	if err != nil || !handled {
		t.Fatalf("route the deploy run's envelope = (%v, %v), want handled without error", handled, err)
	}

	requireDeployedEvent(t, ctx, pool, deployedIssue, "acme/widgets", 501, 900, "2024-01-01T02:10:00Z")
	if at := deployedEventAt(t, ctx, pool, "acme/widgets", 501); at == nil {
		t.Fatal("the intake left the marker unset")
	}
}

// shippedRun is GitHub's answer for deploy run id of acme/widgets: on main, created at 02:00 on
// 2024-01-01, completed with success at 02:10.
func shippedRun(id int64) map[string]any {
	return map[string]any{
		"id": id, "head_sha": fmt.Sprintf("%040d", id), "html_url": fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", id),
		"status": "completed", "conclusion": "success",
		"created_at": "2024-01-01T02:00:00Z", "run_started_at": "2024-01-01T02:00:00Z", "updated_at": "2024-01-01T02:10:00Z",
		"head_branch": "main", "event": "push",
		"head_commit":   map[string]any{"timestamp": "2024-01-01T01:55:00Z"},
		"pull_requests": []any{},
	}
}

// shippedRunJobs is GitHub's jobs listing for shippedRun: the one production job, succeeded at
// 02:10, the completion the event names.
func shippedRunJobs() map[string]any {
	return map[string]any{
		"jobs": []map[string]any{
			{"name": "widgets-release / widgets-release", "started_at": "2024-01-01T02:00:00Z", "completed_at": "2024-01-01T02:10:00Z", "conclusion": "success", "steps": []any{}},
		},
	}
}
