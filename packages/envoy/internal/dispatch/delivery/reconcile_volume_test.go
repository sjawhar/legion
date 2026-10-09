package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// fastPageRetries shrinks the wait between page retries for a test that means to exercise one.
// Production's wait is a second, which would only make these tests slower.
func fastPageRetries(t *testing.T) {
	t.Helper()
	previous := githubPageRetryWait
	githubPageRetryWait = time.Millisecond
	t.Cleanup(func() { githubPageRetryWait = previous })
}

// maximalRun is one workflow run as big as GitHub serves them: its head commit's message is the
// 65,536 characters GitHub truncates at, and the rest of the object carries the repository,
// head_repository and actor objects a real run does. Measured live, that is 79,943 bytes, the
// largest of 300 runs of a busy deploy repository.
func maximalRun(id int64, created time.Time) map[string]any {
	return map[string]any{
		"id": id, "head_sha": fmt.Sprintf("%040x", id), "status": "in_progress",
		"html_url":   fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", id),
		"created_at": created.UTC().Format(time.RFC3339), "run_started_at": created.UTC().Format(time.RFC3339),
		"updated_at": created.UTC().Format(time.RFC3339),
		"head_commit": map[string]any{
			"id": fmt.Sprintf("%040x", id), "message": strings.Repeat("m", 65536),
			"timestamp": created.UTC().Format(time.RFC3339),
		},
		// The fixed weight of a run object beside its message: two whole repository objects of
		// URL templates plus two account objects, about 14 KB live.
		"repository":      map[string]any{"full_name": "acme/widgets", "description": strings.Repeat("r", 7000)},
		"head_repository": map[string]any{"full_name": "acme/widgets", "description": strings.Repeat("h", 7000)},
		"pull_requests":   []any{},
	}
}

// runsPage serves one page of a runs listing the way GitHub does: the runs created inside the
// request's own created=A..B window, newest first, cut to its per_page and page, with the
// window's whole count as total_count.
func runsPage(t *testing.T, w http.ResponseWriter, r *http.Request, runs []map[string]any) {
	t.Helper()
	since, until := parseCreatedWindow(t, r.URL.Query().Get("created"))
	var inWindow []map[string]any
	for _, run := range runs {
		created, err := time.Parse(time.RFC3339, run["created_at"].(string))
		if err != nil {
			t.Fatalf("parse fixture created_at: %v", err)
		}
		if !created.Before(since) && !created.After(until) {
			inWindow = append(inWindow, run)
		}
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if perPage <= 0 {
		perPage = 30
	}
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * perPage
	end := min(start+perPage, len(inWindow))
	if start > len(inWindow) {
		start = len(inWindow)
	}
	mustEncode(t, w, map[string]any{"total_count": len(inWindow), "workflow_runs": inWindow[start:end]})
}

// parseCreatedWindow reads back the created=ISO..ISO qualifier the run listing sends.
func parseCreatedWindow(t *testing.T, created string) (time.Time, time.Time) {
	t.Helper()
	bounds := strings.SplitN(created, "..", 2)
	if len(bounds) != 2 {
		t.Fatalf("created qualifier = %q, want ISO..ISO", created)
	}
	since, err := time.Parse(time.RFC3339, bounds[0])
	if err != nil {
		t.Fatalf("parse created since %q: %v", bounds[0], err)
	}
	until, err := time.Parse(time.RFC3339, bounds[1])
	if err != nil {
		t.Fatalf("parse created until %q: %v", bounds[1], err)
	}
	return since, until
}

// TestListWorkflowRunsPagesABusyDeployRepositoryUnderTheResponseLimit is the deploy-run failure
// the production freshness row carried: "GitHub response body exceeds the 1048576-byte limit".
// A page of 40 runs this size is 3.2 MB at the measured worst case and measured past the cap in
// a real week of the deploy repository's history, which failed the pass identically on every
// retry, forever.
func TestListWorkflowRunsPagesABusyDeployRepositoryUnderTheResponseLimit(t *testing.T) {
	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	runs := make([]map[string]any, 0, 25)
	for i := range 25 {
		runs = append(runs, maximalRun(int64(1000+i), until.Add(-time.Duration(i+1)*time.Minute)))
	}

	fake := newFakeGitHub(t)
	var largest int
	fake.handle("GET /repos/acme/widgets/actions/workflows/deploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		recorder := &countingWriter{ResponseWriter: w}
		runsPage(t, recorder, r, runs)
		if recorder.n > largest {
			largest = recorder.n
		}
	})

	got, err := collectWorkflowRuns(t.Context(), fake.newTestClient(), "acme", "widgets", "deploy.yml", since, until)
	if err != nil {
		t.Fatalf("ListWorkflowRuns over a busy deploy repository: %v", err)
	}
	if len(got) != 25 {
		t.Fatalf("len(runs) = %d, want 25 (every run of the window, paged under the response limit)", len(got))
	}
	if largest >= 1<<20 {
		t.Fatalf("largest page = %d bytes, want under the %d-byte response limit", largest, 1<<20)
	}
}

// countingWriter counts the bytes a handler wrote, so a test can hold a page against githubapp's
// own response limit rather than only against what the client accepted.
type countingWriter struct {
	http.ResponseWriter
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += n
	return n, err
}

// TestListWorkflowRunsRetriesAPageGitHubAnswersWithItsOwnTimeout is the PR-checks failure: a page
// that failed once failed the whole pass, which then restarted its 28-day window from the
// beginning and failed again. GitHub reports a query it gave up on as a 502 or a 504.
func TestListWorkflowRunsRetriesAPageGitHubAnswersWithItsOwnTimeout(t *testing.T) {
	fastPageRetries(t)
	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	runs := make([]map[string]any, 0, 25)
	for i := range 25 {
		runs = append(runs, smallRun(int64(2000+i), until.Add(-time.Duration(i+1)*time.Minute)))
	}

	fake := newFakeGitHub(t)
	var mu sync.Mutex
	timedOut := false
	attempts := 0
	fake.handle("GET /repos/acme/widgets/actions/workflows/deploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		first := !timedOut && r.URL.Query().Get("page") == "2"
		if first {
			timedOut = true
		}
		mu.Unlock()
		if first {
			http.Error(w, "upstream timed out", http.StatusGatewayTimeout)
			return
		}
		runsPage(t, w, r, runs)
	})

	got, err := collectWorkflowRuns(t.Context(), fake.newTestClient(), "acme", "widgets", "deploy.yml", since, until)
	if err != nil {
		t.Fatalf("ListWorkflowRuns across one page GitHub timed out on: %v", err)
	}
	if len(got) != 25 {
		t.Fatalf("len(runs) = %d, want 25 (the retried page's runs among them)", len(got))
	}
	if !timedOut {
		t.Fatal("the fake never answered a timeout, so the retry was never exercised")
	}
}

// smallRun is an ordinary workflow run: the shape, without the maximal commit message.
func smallRun(id int64, created time.Time) map[string]any {
	return map[string]any{
		"id": id, "head_sha": fmt.Sprintf("%040x", id), "status": "in_progress",
		"html_url":   fmt.Sprintf("https://github.com/acme/widgets/actions/runs/%d", id),
		"created_at": created.UTC().Format(time.RFC3339), "run_started_at": created.UTC().Format(time.RFC3339),
		"updated_at":  created.UTC().Format(time.RFC3339),
		"head_commit": map[string]any{"id": fmt.Sprintf("%040x", id), "message": "chore: a commit", "timestamp": created.UTC().Format(time.RFC3339)},
	}
}

// TestSearchMergedPullRequestsRetriesAPageGitHubAnswersWithItsOwnTimeout is the merged-PR search
// failure: one slow search page failed the whole installation's search, and with no progress kept
// the next pass restarted the same 28-day window.
func TestSearchMergedPullRequestsRetriesAPageGitHubAnswersWithItsOwnTimeout(t *testing.T) {
	fastPageRetries(t)
	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)

	fake := newFakeGitHub(t)
	var mu sync.Mutex
	timedOut := false
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode GraphQL request: %v", err)
		}
		after, _ := body.Variables["after"].(string)
		mu.Lock()
		first := after != "" && !timedOut
		if first {
			timedOut = true
		}
		mu.Unlock()
		if first {
			http.Error(w, "upstream timed out", http.StatusBadGateway)
			return
		}
		if after == "" {
			mustEncode(t, w, searchAnswer(2, "cursor-1", true, []any{searchNode(1, "2024-01-01T01:00:00Z")}))
			return
		}
		mustEncode(t, w, searchAnswer(2, "cursor-2", false, []any{searchNode(2, "2024-01-01T02:00:00Z")}))
	})

	found, err := SearchMergedPullRequests(t.Context(), fake.newTestClient(), "acme", "widgets", []string{"octocat"}, since, until)
	if err != nil {
		t.Fatalf("SearchMergedPullRequests across one page GitHub timed out on: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("len(found) = %d, want 2 (both pages, the retried one among them)", len(found))
	}
	if !timedOut {
		t.Fatal("the fake never answered a timeout, so the retry was never exercised")
	}
}

// searchAnswer builds one GraphQL merged-PR search page.
func searchAnswer(count int, cursor string, hasNext bool, nodes []any) map[string]any {
	return map[string]any{"data": map[string]any{"search": map[string]any{
		"issueCount": count,
		"pageInfo":   map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
		"nodes":      nodes,
	}}}
}

// searchNode builds one merged pull request as the search returns it.
func searchNode(number int, merged string) map[string]any {
	return map[string]any{
		"number": number, "title": fmt.Sprintf("feat: widget %d", number),
		"url":    fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		"author": map[string]any{"login": "octocat"}, "createdAt": merged, "mergedAt": merged,
		"labels": map[string]any{"nodes": []any{map[string]any{"name": "non-task"}}},
	}
}

// TestReconcileResumesWorkflowRunsWhereAFailedPassStopped is the structural half of the
// production failure: with the whole pass's progress recorded in one timestamp that only advanced
// when every step succeeded, a 28-day backfill of a busy repository -- tens of thousands of
// requests, past one installation's hourly rate limit -- could never finish one, since each pass
// restarted it from the beginning.
func TestReconcileResumesWorkflowRunsWhereAFailedPassStopped(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	start := time.Now().UTC().Add(-27 * 24 * time.Hour)
	runs := make([]map[string]any, 0, 300)
	for i := range 300 {
		runs = append(runs, smallRun(int64(3000+i), start.Add(time.Duration(i)*2*time.Hour)))
	}
	// The failure is the second half of the window: every window starting at or after it fails,
	// so the first pass completes the windows before it and stops there.
	failFrom := start.Add(13 * 24 * time.Hour)

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchAnswer(0, "", false, []any{}))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	var mu sync.Mutex
	failing := true
	var windowStarts []time.Time
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		since, _ := parseCreatedWindow(t, r.URL.Query().Get("created"))
		mu.Lock()
		windowStarts = append(windowStarts, since)
		refuse := failing && !since.Before(failFrom)
		mu.Unlock()
		if refuse {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		runsPage(t, w, r, runs)
	})

	reconcile := NewReconcile(pool, fake.newTestClient())
	reconcile.runOnce(ctx)

	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after the failed pass: %v", err)
	}
	if settings.LastError == nil || !strings.Contains(*settings.LastError, "deploy workflow runs") {
		t.Fatalf("settings.LastError = %v, want the failed deploy-run step named", settings.LastError)
	}
	if settings.LastReconcileAt != nil {
		t.Fatalf("settings.LastReconcileAt = %v, want nil after a failed pass", settings.LastReconcileAt)
	}
	stored := countRuns(t, ctx, pool)
	if stored == 0 {
		t.Fatal("delivery_runs is empty after the failed pass, want the windows before the failure imported")
	}

	mu.Lock()
	failing = false
	windowStarts = nil
	mu.Unlock()
	reconcile.runOnce(ctx)

	settings, err = GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after the resumed pass: %v", err)
	}
	if settings.LastError != nil {
		t.Fatalf("settings.LastError = %q, want nil once the pass succeeded", *settings.LastError)
	}
	if settings.LastReconcileAt == nil {
		t.Fatal("settings.LastReconcileAt = nil, want the successful pass recorded")
	}
	if got := countRuns(t, ctx, pool); got != 300 {
		t.Fatalf("delivery_runs rows = %d, want 300", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(windowStarts) == 0 {
		t.Fatal("the resumed pass listed no runs at all")
	}
	earliest := windowStarts[0]
	for _, at := range windowStarts {
		if at.Before(earliest) {
			earliest = at
		}
	}
	// The resumed pass must start where the failed one stopped, less the overlap it re-reads for
	// GitHub's index lag -- not at the 28-day backfill window, which is what a pass that forgets
	// what it did starts from.
	if resumeFloor := failFrom.Add(-reconcileOverlap - time.Minute); earliest.Before(resumeFloor) {
		t.Fatalf("the resumed pass's earliest window started %s, want no earlier than %s (where the failed pass stopped, less the overlap)",
			earliest.Format(time.RFC3339), resumeFloor.Format(time.RFC3339))
	}
}

// countRuns counts the stored delivery_runs rows.
func countRuns(t *testing.T, ctx context.Context, pool *store.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, "select count(*) from delivery_runs").Scan(&count); err != nil {
		t.Fatalf("count delivery_runs: %v", err)
	}
	return count
}

// TestIntakeStampsItsFlowControlOnADurableAnEarlierReleaseLeft is the consumer half of the
// production incident. The handler does its GitHub calls inline on the subscription's own
// delivery goroutine, one message at a time, so what NATS may have outstanding against it has to
// be what it can acknowledge inside the ack wait. An earlier release set neither, leaving the
// server's defaults -- 1,000 outstanding against a 30-second ack wait -- so every message past
// roughly the thirtieth was redelivered before the handler had reached it, each redelivery
// another copy in nats.go's per-subscription pending buffer, which filled its 64 MiB limit and
// started dropping messages as a slow consumer: 6,900 drops over four hours, and 543,070
// deliveries against an ack floor of 41,575 for about 10,000 distinct messages. Neither field can
// be set once and forgotten, since the durable already exists in production: bind has to correct
// it in place, which is what this holds.
func TestIntakeStampsItsFlowControlOnADurableAnEarlierReleaseLeft(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	natsURL := testnats.URL(t)
	natsClient, err := bus.ConnectOwningStream([]string{natsURL})
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(natsClient.Close)

	// The durable an earlier release left: its filter subjects, and the server's own flow control.
	legacy := natsgo.ConsumerConfig{
		Durable:        deliveryConsumerName,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubjects: []string{
			"notifications.github.*.*.pr.*",
			contracts.GithubWorkflowSubject("acme", "widgets", pathBaseForTest(settings.DeployWorkflowPath), ">"),
			contracts.GithubWorkflowSubject("acme", "widgets", pathBaseForTest(settings.PRChecksWorkflowPath), ">"),
		},
		AckPolicy: natsgo.AckExplicitPolicy,
	}
	created, err := natsClient.JS().AddConsumer(bus.Stream, &legacy)
	if err != nil {
		t.Fatalf("pre-create the durable an earlier release left: %v", err)
	}
	if created.Config.MaxAckPending == intakeMaxAckPending || created.Config.AckWait == intakeAckWait {
		t.Fatalf("the pre-created durable already carries this release's flow control (%s/%d), so the test proves nothing",
			created.Config.AckWait, created.Config.MaxAckPending)
	}

	intake := NewIntake(natsClient, pool, newFakeGitHub(t).newTestClient())
	runCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go intake.Run(runCtx)

	deadline := time.Now().Add(20 * time.Second)
	var info *natsgo.ConsumerInfo
	for {
		info, err = natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		if err != nil {
			t.Fatalf("read consumer info: %v", err)
		}
		if info.Config.MaxAckPending == intakeMaxAckPending && info.Config.AckWait == intakeAckWait {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable ack_wait/max_ack_pending = %s/%d after %s, want %s/%d (bind must correct an earlier release's flow control in place)",
				info.Config.AckWait, info.Config.MaxAckPending, 20*time.Second, intakeAckWait, intakeMaxAckPending)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !slices.Equal(info.Config.FilterSubjects, legacy.FilterSubjects) {
		t.Fatalf("durable filter subjects = %v, want them left as they were (%v)", info.Config.FilterSubjects, legacy.FilterSubjects)
	}
	// The bound this release holds: what NATS may have outstanding has to be acknowledgeable
	// inside the ack wait, handled one at a time under intakeMessageTimeout.
	if worst := intakeMaxAckPending * intakeMessageTimeout; worst >= intakeAckWait {
		t.Fatalf("intakeMaxAckPending*intakeMessageTimeout = %s, want less than intakeAckWait (%s)", worst, intakeAckWait)
	}
}

// pathBaseForTest is the workflow file name a workflow subject carries, as intake derives it from
// the configured workflow path.
func pathBaseForTest(workflowPath string) string {
	if i := strings.LastIndex(workflowPath, "/"); i >= 0 {
		return workflowPath[i+1:]
	}
	return workflowPath
}
