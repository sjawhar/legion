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

// TestIntakeStampsItsFlowControlOnADurableAnEarlierReleaseLeft holds what bind does to the
// durable production already has. An earlier release left it with the filter-subject set the
// server does not apply -- so the consumer was handed nothing at all -- and with the server's
// own flow control, 1,000 messages outstanding against a 30-second ack wait, which redelivers
// faster than a handler doing its GitHub calls inline can acknowledge. Neither can be set once
// and forgotten, since the durable already exists: bind has to correct both without an operator
// noticing a log line, which is what this holds.
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
	if created.Config.MaxAckPending == intakeMaxAckPending || created.Config.AckWait == intakeAckWait() {
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
		if info.Config.MaxAckPending == intakeMaxAckPending && info.Config.AckWait == intakeAckWait() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable ack_wait/max_ack_pending = %s/%d after %s, want %s/%d (bind must correct an earlier release's flow control in place)",
				info.Config.AckWait, info.Config.MaxAckPending, 20*time.Second, intakeAckWait(), intakeMaxAckPending)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if info.Config.FilterSubject != githubIntakeSubject || len(info.Config.FilterSubjects) > 0 {
		t.Fatalf("durable filter = %q / %v, want the one subject the server applies (%q)",
			info.Config.FilterSubject, info.Config.FilterSubjects, githubIntakeSubject)
	}
	// The bound this release holds: what NATS may have outstanding has to be acknowledgeable
	// inside the ack wait, handled one at a time under intakeMessageTimeout.
	if worst := time.Duration(intakeMaxAckPending) * intakeMessageTimeout; worst >= intakeAckWait() {
		t.Fatalf("intakeMaxAckPending*intakeMessageTimeout = %s, want less than intakeAckWait (%s)", worst, intakeAckWait())
	}
}

// TestWhichFilterShapeMatchesAWorkflowSubject pins why the durable an earlier release left was
// handed nothing: it probes the same stored messages with each filter shape in turn and reports
// what each one matches.
func TestWhichFilterShapeMatchesAWorkflowSubject(t *testing.T) {
	natsClient := intakeTestClient(t)
	const stored = 12
	for i := range stored {
		publishWorkflowEnvelope(t, natsClient, "acme", "widgets", ".github/workflows/deploy.yml", int64(6000+i))
	}
	subject := contracts.GithubWorkflowSubject("acme", "widgets", "deploy.yml", "in_progress")
	oldSet := []string{
		"notifications.github.*.*.pr.*",
		contracts.GithubWorkflowSubject("acme", "widgets", "deploy.yml", ">"),
		contracts.GithubWorkflowSubject("acme", "widgets", "pr-checks.yml", ">"),
	}
	t.Logf("stored %d messages on %q", stored, subject)

	probe := func(name string, config natsgo.ConsumerConfig) uint64 {
		t.Helper()
		config.AckPolicy = natsgo.AckExplicitPolicy
		info, err := natsClient.JS().AddConsumer(bus.Stream, &config)
		if err != nil {
			t.Logf("%s: AddConsumer refused: %v", name, err)
			return 0
		}
		defer func() {
			if err := natsClient.JS().DeleteConsumer(bus.Stream, info.Name); err != nil {
				t.Logf("delete probe consumer %s: %v", info.Name, err)
			}
		}()
		t.Logf("%s: pending=%d", name, info.NumPending)
		return info.NumPending
	}

	singleWorkflow := probe("FilterSubject = the workflow subject with >", natsgo.ConsumerConfig{FilterSubject: oldSet[1]})
	probe("FilterSubject = the PR subject", natsgo.ConsumerConfig{FilterSubject: oldSet[0]})
	probe("FilterSubject = the PR-checks workflow subject with >", natsgo.ConsumerConfig{FilterSubject: oldSet[2]})
	pluralOne := probe("pull, FilterSubjects = [the workflow subject with >]", natsgo.ConsumerConfig{FilterSubjects: []string{oldSet[1]}})
	pluralAll := probe("pull, FilterSubjects = the whole old set", natsgo.ConsumerConfig{FilterSubjects: oldSet})

	// The same filters on a push consumer -- one carrying a DeliverSubject -- which is what this
	// package's durable is. This is the axis the probes above do not cover.
	pushSingle := probe("push, FilterSubject = the workflow subject with >",
		natsgo.ConsumerConfig{DeliverSubject: natsgo.NewInbox(), FilterSubject: oldSet[1]})
	pushPluralOne := probe("push, FilterSubjects = [the workflow subject with >]",
		natsgo.ConsumerConfig{DeliverSubject: natsgo.NewInbox(), FilterSubjects: []string{oldSet[1]}})
	pushPluralAll := probe("push, FilterSubjects = the whole old set",
		natsgo.ConsumerConfig{DeliverSubject: natsgo.NewInbox(), FilterSubjects: oldSet})
	pushWide := probe("push, FilterSubject = "+githubIntakeSubject,
		natsgo.ConsumerConfig{DeliverSubject: natsgo.NewInbox(), FilterSubject: githubIntakeSubject})

	// The whole config the stalled release built, field for field, under a name of its own: a
	// durable push consumer with the three filter subjects and this package's flow control. Kept
	// alive rather than probed, because the subscription bound to it below is the subject here.
	old, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        "probe-old-config",
		DeliverSubject: natsgo.NewInbox(),
		FilterSubjects: oldSet,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
	})
	if err != nil {
		t.Fatalf("create the old-config durable: %v", err)
	}
	t.Logf("whole old config matched %d of %d stored messages", old.NumPending, stored)

	// Each consumer matches every stored message, so what differs is the subscription bound to
	// it. A plural-filter consumer leaves FilterSubject empty, and an empty subject is the only
	// one nats.go accepts against it; a single-filter consumer must be subscribed on its own
	// filter subject. Bind each the way its release does and count what actually arrives.
	t.Logf("bound to the old config with an empty subject: %d of %d arrived", drain(t, natsClient, "probe-old-config", ""), stored)
	single, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        "probe-new-config",
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
	})
	if err != nil {
		t.Fatalf("create the single-filter durable: %v", err)
	}
	t.Logf("bound to the new config on its own filter subject: %d of %d arrived", drain(t, natsClient, single.Name, githubIntakeSubject), stored)

	if pushWide != stored {
		t.Fatalf("the push consumer this release creates matched %d of %d stored messages", pushWide, stored)
	}
	if singleWorkflow != stored {
		t.Fatalf("the workflow subject matched %d of %d stored messages as a single pull FilterSubject", singleWorkflow, stored)
	}
	t.Logf("verdict: pull single=%d plural-one=%d plural-all=%d | push single=%d plural-one=%d plural-all=%d wide=%d (of %d stored)",
		singleWorkflow, pluralOne, pluralAll, pushSingle, pushPluralOne, pushPluralAll, pushWide, stored)
}

// drain binds a push subscription to consumer on subject, the way this package's bind does, and
// reports how many messages reach its handler within a short window.
func drain(t *testing.T, natsClient *bus.Client, consumer, subject string) int {
	t.Helper()
	var mu sync.Mutex
	arrived := 0
	sub, err := natsClient.JS().Subscribe(subject, func(msg *natsgo.Msg) {
		mu.Lock()
		arrived++
		mu.Unlock()
		if err := msg.Ack(); err != nil {
			t.Logf("ack on %s: %v", consumer, err)
		}
	}, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
	if err != nil {
		t.Logf("subscribe to %s on subject %q: %v", consumer, subject, err)
		return 0
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Logf("unsubscribe from %s: %v", consumer, err)
		}
	}()
	time.Sleep(3 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	return arrived
}

// TestIntakeDiscardsAnEnvelopeItDoesNotWantWithoutCallingGitHub holds the cost of the one filter
// subject this release carries: the durable is handed every GitHub notification on the bus, and
// the ones this slice does not want -- another repository's workflow, a workflow file neither
// setting names -- must cost a decode and an acknowledgement, with no GitHub call and no row
// written. Otherwise a wide filter would put the handler's serial GitHub work behind traffic it
// has no interest in.
func TestIntakeDiscardsAnEnvelopeItDoesNotWantWithoutCallingGitHub(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/{owner}/{repo}/actions/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub was called for an envelope the intake does not want: %s", r.URL.Path)
	})

	natsClient := intakeTestClient(t)
	intake := NewIntake(natsClient, pool, fake.newTestClient())
	runCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go intake.Run(runCtx)
	awaitBoundDurable(t, natsClient)

	// A workflow file neither setting names, and another repository's run of the deploy file.
	const unwanted = 8
	for i := range unwanted {
		workflowPath, owner, repo := ".github/workflows/unrelated.yml", "acme", "widgets"
		if i%2 == 1 {
			workflowPath, owner, repo = ".github/workflows/deploy.yml", "other-org", "other-repo"
		}
		publishWorkflowEnvelope(t, natsClient, owner, repo, workflowPath, int64(7000+i))
	}

	// Every one acknowledged: the ack floor reaches the last message the stream holds.
	deadline := time.Now().Add(30 * time.Second)
	for {
		stream, err := natsClient.JS().StreamInfo(bus.Stream)
		if err != nil {
			t.Fatalf("read stream info: %v", err)
		}
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		if err != nil {
			t.Fatalf("read consumer info: %v", err)
		}
		if info.AckFloor.Stream >= stream.State.LastSeq && info.NumPending == 0 {
			t.Logf("discarded %d envelopes: ack_floor=%d last_seq=%d redelivered=%d",
				unwanted, info.AckFloor.Stream, stream.State.LastSeq, info.NumRedelivered)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the intake did not acknowledge every envelope it discarded\n%s", intakeStateReport(t, natsClient))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if stored := countRuns(t, ctx, pool); stored != 0 {
		t.Fatalf("delivery_runs rows = %d, want 0 (a discarded envelope writes nothing)", stored)
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

// shrinkIntakeFlowControl shrinks the intake's own timing so a test exercises its real flow
// control in test time. intakeMaxAckPending is deliberately left alone: it is the bound under
// test.
func shrinkIntakeFlowControl(t *testing.T) {
	t.Helper()
	messageTimeout, pollInterval := intakeMessageTimeout, settingsPollInterval
	intakeMessageTimeout, settingsPollInterval = 5*time.Second, time.Second
	t.Cleanup(func() {
		intakeMessageTimeout, settingsPollInterval = messageTimeout, pollInterval
	})
}

// publishWorkflowEnvelope publishes one workflow-run envelope built as Envoy's own GitHub
// normalizer builds it -- source "github", the delivery GUID as SourceEventID, and the dedupe
// key that pairs with it ("github.<guid>"), which is what earns the publish a JetStream MsgId
// (contracts.DedupeKeyNamesTheUpstreamEvent). Publishing through production's own path rather
// than an ad-hoc envelope is what makes a burst test say anything about production: an envelope
// carrying another dedupe key is stored under different rules. Reports JetStream's own duplicate
// verdict, so a caller can tell a message the stream already held from a new one.
func publishWorkflowEnvelope(t *testing.T, natsClient *bus.Client, owner, repo, workflowPath string, runID int64) bool {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "workflow", "action": "in_progress", "repo": owner + "/" + repo,
		"path": workflowPath, "run_id": strconv.FormatInt(runID, 10),
	})
	if err != nil {
		t.Fatalf("encode workflow payload: %v", err)
	}
	deliveryID := fmt.Sprintf("%s-%d", t.Name(), runID)
	envelope := contracts.Envelope{
		EventID:       deliveryID,
		Source:        "github",
		SourceEventID: deliveryID,
		DedupeKey:     "github." + deliveryID,
		Topic:         contracts.GithubWorkflowSubject(owner, repo, pathBaseForTest(workflowPath), "in_progress"),
		Payload:       string(payload),
		TraceID:       deliveryID,
		IssuedAt:      time.Now().Unix(),
	}
	if !contracts.DedupeKeyNamesTheUpstreamEvent(envelope) {
		t.Fatalf("the test envelope's dedupe key does not name its event, so it is not published the way production publishes")
	}
	duplicate, err := natsClient.PublishReportingDuplicate(envelope)
	if err != nil {
		t.Fatalf("publish workflow envelope: %v", err)
	}
	return duplicate
}

// intakeTestClient connects one NATS client for an intake test and closes it with the test.
func intakeTestClient(t *testing.T) *bus.Client {
	t.Helper()
	natsClient, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(natsClient.Close)
	return natsClient
}

// awaitBoundDurable waits for the intake to have created its durable and subscribed to it, so a
// test that means to publish into a bound consumer does not race the bind.
func awaitBoundDurable(t *testing.T, natsClient *bus.Client) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		if err == nil && info.PushBound && info.Config.MaxAckPending == intakeMaxAckPending {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("intake never bound its durable: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// awaitRuns waits for the intake to have stored want delivery_runs rows, and fails with the
// stream's message count and the consumer's whole state when it does not -- the numbers that say
// whether a shortfall is a consumer that stopped being given messages or messages that never
// reached the stream.
func awaitRuns(t *testing.T, ctx context.Context, pool *store.Pool, natsClient *bus.Client, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		stored := countRuns(t, ctx, pool)
		if stored >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery_runs rows = %d, want %d after %s\n%s", stored, want, within, intakeStateReport(t, natsClient))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// intakeStateReport is the stream's message count per subject and the consumer's delivery state,
// one line each.
func intakeStateReport(t *testing.T, natsClient *bus.Client) string {
	t.Helper()
	report := ""
	if stream, err := natsClient.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: ">"}); err != nil {
		report += fmt.Sprintf("stream: %v\n", err)
	} else {
		report += fmt.Sprintf("stream: %d messages, by subject %v\n", stream.State.Msgs, stream.State.Subjects)
	}
	if info, err := natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName); err != nil {
		report += fmt.Sprintf("consumer: %v", err)
	} else {
		report += fmt.Sprintf("consumer: delivered=%d ack_floor=%d ack_pending=%d pending=%d redelivered=%d filter_subjects=%q",
			info.Delivered.Consumer, info.AckFloor.Consumer, info.NumAckPending, info.NumPending, info.NumRedelivered, info.Config.FilterSubjects)
	}
	report += "\n" + singleFilterProbe(t, natsClient)
	return report
}

// singleFilterProbe reports what a consumer carrying one filter subject sees of the same stream.
// Read beside the intake consumer's own numbers it separates the two explanations of a shortfall:
// a stream that does not hold the messages, or a multi-subject filter the server does not apply
// the way one filter subject is applied.
func singleFilterProbe(t *testing.T, natsClient *bus.Client) string {
	t.Helper()
	subject := contracts.GithubWorkflowSubject("acme", "widgets", "deploy.yml", "in_progress")
	probe, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		FilterSubject: subject,
		AckPolicy:     natsgo.AckExplicitPolicy,
	})
	if err != nil {
		return fmt.Sprintf("single-filter probe on %q: %v", subject, err)
	}
	defer func() {
		if err := natsClient.JS().DeleteConsumer(bus.Stream, probe.Name); err != nil {
			t.Logf("delete the single-filter probe consumer: %v", err)
		}
	}()
	return fmt.Sprintf("single-filter probe on %q: pending=%d", subject, probe.NumPending)
}

// TestIntakeKeepsUpWithABurstOfDeliveryEvents publishes more events at once than the consumer may
// have outstanding and holds the whole path end to end: every one is stored, each costs exactly
// one GitHub fetch, nothing is redelivered and nothing is left outstanding. A burst larger than
// intakeMaxAckPending is the case that bound exists for, so a flow-control change that stalls the
// consumer -- an acknowledgement that never reaches the server, a credit never given back --
// fails here.
func TestIntakeKeepsUpWithABurstOfDeliveryEvents(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	const burst = 30
	var mu sync.Mutex
	fetches := map[int64]int{}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("run_id"), 10, 64)
		if err != nil {
			t.Errorf("parse run id %q: %v", r.PathValue("run_id"), err)
		}
		mu.Lock()
		fetches[id]++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mustEncode(t, w, smallRun(id, time.Now().UTC()))
	})

	natsClient := intakeTestClient(t)
	intake := NewIntake(natsClient, pool, fake.newTestClient())
	runCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go intake.Run(runCtx)
	awaitBoundDurable(t, natsClient)

	duplicates := 0
	for i := range burst {
		if publishWorkflowEnvelope(t, natsClient, "acme", "widgets", settings.DeployWorkflowPath, int64(4000+i)) {
			duplicates++
		}
	}
	if duplicates > 0 {
		t.Fatalf("%d of %d publishes were duplicates the stream already held, so the burst never reached the consumer", duplicates, burst)
	}
	awaitRuns(t, ctx, pool, natsClient, burst, 60*time.Second)
	t.Log(intakeStateReport(t, natsClient))

	// A redelivery, which flow control that did not fit the handler would cause, arrives within
	// the ack wait; give one time to land before reading the consumer's state.
	time.Sleep(2 * time.Second)
	info, err := natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
	if err != nil {
		t.Fatalf("read consumer info: %v", err)
	}
	if info.NumRedelivered != 0 || info.NumAckPending != 0 {
		t.Fatalf("consumer redelivered=%d ack_pending=%d, want 0/0 (every message acknowledged once, none redelivered)", info.NumRedelivered, info.NumAckPending)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fetches) != burst {
		t.Fatalf("distinct runs fetched = %d, want %d", len(fetches), burst)
	}
	for id, count := range fetches {
		if count != 1 {
			t.Fatalf("run %d fetched %d times, want exactly 1 (a redelivery repeats its GitHub calls)", id, count)
		}
	}
}

// TestIntakeDrainsABacklogPublishedBeforeItBinds pins what the server does with events that
// arrive while no subscriber is attached: the window between the durable being created and the
// subscription attaching to it, and the whole time the process is down. The backlog is held
// undelivered rather than pushed at an inbox nobody is reading, so one larger than
// intakeMaxAckPending drains in full once the intake comes up, with nothing waiting on the ack
// wait to be redelivered.
func TestIntakeDrainsABacklogPublishedBeforeItBinds(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	backlog := 3 * intakeMaxAckPending
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("run_id"), 10, 64)
		if err != nil {
			t.Errorf("parse run id %q: %v", r.PathValue("run_id"), err)
		}
		mustEncode(t, w, smallRun(id, time.Now().UTC()))
	})

	natsClient := intakeTestClient(t)
	for i := range backlog {
		publishWorkflowEnvelope(t, natsClient, "acme", "widgets", settings.DeployWorkflowPath, int64(5000+i))
	}

	intake := NewIntake(natsClient, pool, fake.newTestClient())
	runCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go intake.Run(runCtx)

	awaitRuns(t, ctx, pool, natsClient, backlog, 60*time.Second)
	t.Log(intakeStateReport(t, natsClient))
}

// TestReconcilePrunesProgressOfAnInstallationThatNoLongerExists holds the one thing that ever
// deletes a merged-PR progress row: the step's own installation listing. An App installation
// that is removed, or whose repositories move to another, leaves a row keyed by an id no listing
// answers for again, and nothing else would ever collect it.
func TestReconcilePrunesProgressOfAnInstallationThatNoLongerExists(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	scope := searchProgressScope(settings)

	live, removed := int64(1), int64(999)
	for _, installationID := range []int64{live, removed} {
		if err := RecordReconcileProgress(ctx, pool, mergedPullRequestsStep(installationID), scope, time.Now().UTC().Add(-time.Hour)); err != nil {
			t.Fatalf("seed installation %d progress: %v", installationID, err)
		}
	}

	fake := newFakeGitHub(t)
	fake.installations = []fakeInstallation{{id: live, repos: []string{"acme/widgets"}}}
	NewReconcile(pool, fake.newTestClient()).runOnce(ctx)

	rows, err := pool.Query(ctx, `select step from delivery_reconcile_progress where step like 'merged_pull_requests/installation/%' order by step`)
	if err != nil {
		t.Fatalf("read progress steps: %v", err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var step string
		if err := rows.Scan(&step); err != nil {
			t.Fatalf("scan progress step: %v", err)
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read progress steps: %v", err)
	}
	if slices.Contains(steps, mergedPullRequestsStep(removed)) {
		t.Fatalf("progress steps = %v, want the removed installation's row gone", steps)
	}
	if !slices.Contains(steps, mergedPullRequestsStep(live)) {
		t.Fatalf("progress steps = %v, want the live installation's row kept", steps)
	}
}
