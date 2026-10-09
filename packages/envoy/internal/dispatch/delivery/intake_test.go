package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain removes the NATS server the package's tests share (testnats.Main).
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

// deliveryTestPool opens a real Postgres pool from DISPATCH_TEST_DATABASE_URL, migrates it, and
// returns it; a test that needs it skips (not fails) when the variable is unset, matching every
// other Postgres-backed test in this module's convention.
func deliveryTestPool(t *testing.T) (*store.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres-backed delivery tests")
	}
	ctx := t.Context()
	pgxPool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pgxPool.Close)
	pool := store.NewPool(pgxPool)
	st := &store.Store{Pool: pool}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Every delivery table is truncated before the test, not after: a failed previous run's rows
	// (useful to inspect) are cleared by the next run that needs a clean slate, not hidden by one
	// that crashes before its own cleanup runs.
	for _, table := range []string{"delivery_run_jobs", "delivery_runs", "delivery_pull_requests", "delivery_reconcile_progress", "delivery_settings"} {
		if _, err := pool.Exec(ctx, "delete from "+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return pool, ctx
}

// seedDeliverySettings writes a delivery_settings row with acme/widgets-shaped placeholders.
func seedDeliverySettings(t *testing.T, ctx context.Context, pool *store.Pool) DeliverySettings {
	t.Helper()
	settings, err := PutSettings(ctx, pool, DeliverySettings{
		DeployRepo:           "acme/widgets",
		DeployWorkflowPath:   ".github/workflows/deploy.yml",
		ProductionJobName:    "widgets-release / widgets-release",
		PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors:    []string{"octocat"},
		ExcludedRepos:        []string{"acme/playground"},
	}, model.Actor{Kind: "system", ID: "delivery-test"})
	if err != nil {
		t.Fatalf("seed delivery_settings: %v", err)
	}
	return settings
}

// publishPullRequestEnvelope publishes one notifications.github.<owner>.<repo>.pr.<number>
// envelope (the shape intake.go's handlePullRequestEnvelope reads) and waits for the publish to
// be acknowledged by JetStream before returning.
func publishPullRequestEnvelope(t *testing.T, natsClient *bus.Client, owner, repo string, number int, fields map[string]string) {
	t.Helper()
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	envelope := contracts.Envelope{
		EventID:       fmt.Sprintf("test-%s-%s-%d-%d", owner, repo, number, time.Now().UnixNano()),
		Source:        "github",
		SourceEventID: fmt.Sprintf("%d", time.Now().UnixNano()),
		Topic:         contracts.GithubResourceSubject(owner, repo, "pr", fmt.Sprintf("%d", number)),
		Payload:       string(payload),
		TraceID:       "test-trace",
		IssuedAt:      time.Now().Unix(),
	}
	if err := natsClient.Publish(envelope); err != nil {
		t.Fatalf("publish envelope: %v", err)
	}
}

func TestIntakeDedupesAMergedPullRequestEnvelope(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 42, "title": "feat: a widget", "html_url": "https://github.com/acme/widgets/pull/42",
			"user": map[string]any{"login": "octocat"}, "labels": []any{map[string]any{"name": "non-task"}},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T02:00:00Z",
			"merge_commit_sha": "abc123", "additions": 10, "deletions": 2, "body": "",
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/42/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			mustEncode(t, w, []any{})
			return
		}
		mustEncode(t, w, []any{
			map[string]any{"commit": map[string]any{"message": "feat: a widget\n\nOmp-Session: 01a1-test-session", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}},
		})
	})
	client := fake.newTestClient()

	natsURL := testnats.URL(t)
	natsClient, err := bus.ConnectOwningStream([]string{natsURL})
	if err != nil {
		t.Fatalf("connect NATS: %v", err)
	}
	t.Cleanup(natsClient.Close)

	intake := NewIntake(natsClient, pool, client)
	runCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go intake.Run(runCtx)

	envelopeFields := map[string]string{
		"kind": "pr", "action": "closed", "repo": "acme/widgets", "number": "42",
		"title": "feat: a widget", "author": "octocat", "url": "https://github.com/acme/widgets/pull/42",
		"merged": "true", "merge_commit_sha": "abc123", "updated_at": "2024-01-01T02:00:00Z",
	}
	// Publish the same merged-PR envelope twice: the second publish simulates a NATS redelivery
	// or an overlapping reconcile pass re-observing the same merge. Both must land through the
	// same upsert, so the result is one row, not two or an error.
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 42, envelopeFields)
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", 42, envelopeFields)

	var count int
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := pool.QueryRow(ctx, "select count(*) from delivery_pull_requests where repo = $1 and number = $2", "acme/widgets", 42).Scan(&count); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if count > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if count != 1 {
		t.Fatalf("delivery_pull_requests row count for acme/widgets#42 = %d, want exactly 1 (two envelopes for the same merge must dedupe through the shared upsert)", count)
	}

	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 42))
	if err != nil {
		t.Fatalf("scan pull request: %v", err)
	}
	if pr.Partial {
		t.Error("pr.Partial = true, want false (the completing fetch succeeded)")
	}
	if pr.Additions == nil || *pr.Additions != 10 {
		t.Errorf("pr.Additions = %v, want 10", pr.Additions)
	}
	if len(pr.Sessions) != 1 || pr.Sessions[0] != "01a1-test-session" {
		t.Errorf("pr.Sessions = %v, want [01a1-test-session]", pr.Sessions)
	}

	got, err := GetSettings(ctx, pool)
	if err != nil || got.LastEventAt == nil {
		t.Errorf("settings.LastEventAt not recorded after intake processed an event: %+v, %v", got, err)
	}
	_ = settings
}
