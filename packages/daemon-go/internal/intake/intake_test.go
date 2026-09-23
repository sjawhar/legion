package intake

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

type handlerFunc func(context.Context, pgx.Tx, Fact) (Result, error)

func (f handlerFunc) Apply(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
	return f(ctx, tx, fact)
}

func TestApplyFactCommitsRefusalAndLaterHandlers(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx := context.Background()
	refusal := &Refusal{Status: 409, Code: "DESIGN_GATE_CLOSED", Message: "approve version 3"}

	result, err := ApplyFact(ctx, pool, "dispatch", "event-refusal", DispatchIssue{Key: "LEGION-208"},
		writeHandler("first", refusal),
		writeHandler("later", nil),
	)
	if err != nil {
		t.Fatalf("ApplyFact: %v", err)
	}
	if !reflect.DeepEqual(result.Refusal, refusal) {
		t.Fatalf("refusal = %#v, want %#v", result.Refusal, refusal)
	}
	if got := writeCount(t, pool); got != 2 {
		t.Fatalf("committed writes = %d, want 2", got)
	}
	if got, want := writeHandlers(t, pool), []string{"first", "later"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("handler order = %#v, want %#v", got, want)
	}
}

func TestApplyFactDeduplicatesBeforeHandlers(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx := context.Background()

	if _, err := ApplyFact(ctx, pool, "dispatch", "event-duplicate", DispatchIssue{Key: "LEGION-208"}, writeHandler("first", nil)); err != nil {
		t.Fatalf("first ApplyFact: %v", err)
	}
	result, err := ApplyFact(ctx, pool, "dispatch", "event-duplicate", DispatchIssue{Key: "LEGION-208"}, handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
		return Result{}, errors.New("duplicate reached a handler")
	}))
	if err != nil {
		t.Fatalf("duplicate ApplyFact: %v", err)
	}
	if result != (Result{}) {
		t.Fatalf("duplicate result = %#v, want zero result", result)
	}
	if got := writeCount(t, pool); got != 1 {
		t.Fatalf("writes after duplicate = %d, want 1", got)
	}
}

func TestApplyFactRollsBackHandlerAndDeduplicationOnError(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	ctx := context.Background()

	_, err := ApplyFact(ctx, pool, "dispatch", "event-rollback", DispatchIssue{Key: "LEGION-208"},
		writeHandler("rolled-back", nil),
		handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) { return Result{}, errors.New("handler failed") }),
	)
	if err == nil || !strings.Contains(err.Error(), "handler failed") {
		t.Fatalf("ApplyFact error = %v, want handler error", err)
	}
	if got := writeCount(t, pool); got != 0 {
		t.Fatalf("writes after rollback = %d, want 0", got)
	}

	if _, err := ApplyFact(ctx, pool, "dispatch", "event-rollback", DispatchIssue{Key: "LEGION-208"}, writeHandler("retry", nil)); err != nil {
		t.Fatalf("retry ApplyFact: %v", err)
	}
	if got := writeCount(t, pool); got != 1 {
		t.Fatalf("writes after retry = %d, want 1", got)
	}
}

func TestDecodeCapturedProducerEnvelopes(t *testing.T) {
	updatedAt := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	checksSettledAt := time.UnixMilli(1790124840596).UTC()
	cases := []struct {
		name    string
		subject string
		file    string
		want    Fact
	}{
		{
			name:    "Dispatch issue created",
			subject: "notifications.dispatch.issue.CAPTURE-4.issue.created",
			file:    "dispatch/issue-created.json",
			want:    DispatchIssue{Key: "CAPTURE-4", Seq: 1, Type: "issue.created", Status: "triage", Title: "Captured approval issue", Rank: "UUUU"},
		},
		{
			name:    "Dispatch issue updated",
			subject: "notifications.dispatch.issue.CAPTURE-3.issue.updated",
			file:    "dispatch/issue-updated.json",
			want:    DispatchIssue{Key: "CAPTURE-3", Seq: 2, Type: "issue.updated", Status: "todo", Title: "Captured workflow issue", Rank: "UUU"},
		},
		{
			name:    "Dispatch issue closed",
			subject: "notifications.dispatch.issue.CAPTURE-4.issue.closed",
			file:    "dispatch/issue-closed.json",
			want:    DispatchIssue{Key: "CAPTURE-4", Seq: 6, Type: "issue.closed", Status: "done", Title: "Captured approval issue", Rank: "UUUU"},
		},
		{
			name:    "Dispatch artifact version",
			subject: "notifications.dispatch.issue.CAPTURE-4.artifact.version",
			file:    "dispatch/artifact-version.json",
			want:    DispatchArtifact{Key: "CAPTURE-4", ArtifactID: "0544d460-0931-4374-b20b-790408519edd", Kind: DispatchArtifactVersion, Version: 2},
		},
		{
			name:    "Dispatch artifact approved",
			subject: "notifications.dispatch.issue.CAPTURE-4.artifact.approved",
			file:    "dispatch/artifact-approved.json",
			want:    DispatchArtifact{Key: "CAPTURE-4", ArtifactID: "0544d460-0931-4374-b20b-790408519edd", Kind: DispatchArtifactApproved, Version: 2},
		},
		{
			name:    "Dispatch artifact changes requested",
			subject: "notifications.dispatch.issue.CAPTURE-3.artifact.changes_requested",
			file:    "dispatch/artifact-changes-requested.json",
			want:    DispatchArtifact{Key: "CAPTURE-3", ArtifactID: "e7860036-ca1a-4ec6-8bd0-51d5f1b6fbd8", Kind: DispatchArtifactChangesRequested, Version: 2, Reason: "Captured reviewer reason"},
		},
		{
			name:    "pull request opened",
			subject: "notifications.github.sjawhar.legion.pr.42",
			file:    "github/pr-opened.json",
			want:    PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-captured", Body: "Dispatch: LEGION-208", URL: "https://github.com/sjawhar/legion/pull/42", UpdatedAt: updatedAt},
		},
		{
			name:    "pull request synchronized",
			subject: "notifications.github.sjawhar.legion.pr.42",
			file:    "github/pr-synchronized.json",
			want:    PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-captured", Body: "Dispatch: LEGION-208", UpdatedAt: updatedAt},
		},
		{
			name:    "pull request closed",
			subject: "notifications.github.sjawhar.legion.pr.42",
			file:    "github/pr-closed.json",
			want:    PullRequestClosed{Repo: "sjawhar/legion", Number: 42},
		},
		{
			name:    "pull request merged",
			subject: "notifications.github.sjawhar.legion.pr.42",
			file:    "github/pr-merged.json",
			want:    PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge-captured"},
		},
		{
			name:    "pull request review",
			subject: "notifications.github.sjawhar.legion.pr.42.review",
			file:    "github/review.json",
			want:    PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head-captured", HeadSHA: "head-captured", Author: "reviewer", Body: "Captured review"},
		},
		{
			name:    "checks settlement",
			subject: "notifications.github.sjawhar.legion.pr.42.checks",
			file:    "github/checks.json",
			want:    PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "abcdef1234567890abcdef1234567890abcdef12", CheckRuns: []CheckRun{{Name: "unit", ID: 73}}, Snapshot: "ed3e3bafc46f498bca65fe879fcd1765a90fecbb1fcd62579e46a94707c0bacd", Verdict: "red", Failing: []string{"unit"}, SettledAt: checksSettledAt},
		},
		{
			name:    "branch push",
			subject: "notifications.github.sjawhar.legion.push.branch.legion/LEGION-208",
			file:    "github/push.json",
			want:    Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", After: "head-captured", ChangedPaths: new(".legion/plan.json\nsource.go"), Truncated: new("false")},
		},
		{
			name:    "comment",
			subject: "notifications.github.sjawhar.legion.pr.42.comment",
			file:    "github/comment.json",
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.file)
			if err != nil {
				t.Fatalf("read captured envelope: %v", err)
			}
			got, err := decodeMessage(tc.subject, data)
			if err != nil {
				t.Fatalf("decode captured envelope: %v", err)
			}
			if !reflect.DeepEqual(got.Fact, tc.want) {
				t.Fatalf("fact = %#v, want %#v", got.Fact, tc.want)
			}
		})
	}
}

func TestCapturedIssueUpdatedEnvelopeDecodes(t *testing.T) {
	data := capturedIssueUpdatedEnvelope(t)
	if _, err := decodeMessage("notifications.dispatch.issue.CAPTURE-3.issue.updated", data); err != nil {
		t.Fatalf("decode captured issue.updated envelope: %v", err)
	}
}

func TestConsumeTermsPoisonMessagesOnce(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	stop := startConsume(t, js, spec, pool, writeHandler("applied", nil))
	defer stop()

	publish(t, js, "notifications.dispatch.issue.LEGION-208.issue.updated", []byte(`{`))
	publish(t, js, "notifications.dispatch.issue.LEGION-208.issue.updated", envelopeJSON(t, "dispatch-poison", "dispatch", `{"id":1,"issue_key":"LEGION-208","seq":1,"notify":true,"type":"issue.updated","payload":{"key":"LEGION-999","status":"todo","title":"wrong key"}}`))
	eventually(t, "two poison logs", func() bool { return strings.Count(logs.String(), "poison JetStream message") == 2 })
	time.Sleep(3 * spec.AckWait)
	if got := strings.Count(logs.String(), "poison JetStream message"); got != 2 {
		t.Fatalf("poison logs after ack wait = %d, want 2", got)
	}
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
}

func TestConsumeDeduplicatesOneEventAcrossDeliveries(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	stop := startConsume(t, js, spec, pool, writeHandler("applied", nil))
	defer stop()

	message := capturedIssueUpdatedEnvelope(t)
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", message)
	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", message)
	eventually(t, "one deduplicated write", func() bool { return writeCount(t, pool) == 1 })
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
}

func TestConsumeNaksRollbackAndAppliesRedeliveryOnce(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	var calls atomic.Int32
	handler := handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
		if _, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ('retry')`); err != nil {
			return Result{}, err
		}
		if calls.Add(1) == 1 {
			return Result{}, errors.New("transient handler failure")
		}
		return Result{}, nil
	})
	stop := startConsume(t, js, spec, pool, handler)
	defer stop()

	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	eventually(t, "redelivery committed once", func() bool { return calls.Load() >= 2 && writeCount(t, pool) == 1 })
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
}

func TestConsumeRestartResumesAfterAcknowledgedMessage(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, _ := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	stop := startConsume(t, js, spec, pool, writeHandler("restart", nil))

	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	eventually(t, "first committed message", func() bool { return writeCount(t, pool) == 1 })
	stop()

	publish(t, js, "notifications.dispatch.issue.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
	stop = startConsume(t, js, spec, pool, writeHandler("restart", nil))
	defer stop()
	eventually(t, "durable consumer resumes after ack", func() bool { return writeCount(t, pool) == 2 })
}

func TestConsumeCommitsRefusalAndAcknowledges(t *testing.T) {
	pool := migratedPool(t)
	createWrites(t, pool)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	stop := startConsume(t, js, spec, pool, writeHandler("refusal", &Refusal{Status: 409, Code: "DESIGN_GATE_CLOSED", Message: "approve version 3"}))
	defer stop()

	publish(t, js, "notifications.dispatch.issue.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
	eventually(t, "refusal committed", func() bool { return writeCount(t, pool) == 1 && strings.Count(logs.String(), "committed refusal") == 1 })
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))
}

func consumerSpec(logs *lockedBuffer) ConsumerSpec {
	return ConsumerSpec{
		Project:      "LEGION",
		Repositories: []string{"sjawhar/legion"},
		AckWait:      200 * time.Millisecond,
		NakDelay:     25 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	}
}

func startConsume(t *testing.T, js jetstream.JetStream, spec ConsumerSpec, pool *pgxpool.Pool, handlers ...Handler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Consume(ctx, js, spec, pool, handlers...) }()
	eventually(t, "durable intake consumers", func() bool {
		stream, err := js.Stream(context.Background(), "ENVOY_NOTIFICATIONS")
		if err != nil {
			return false
		}
		_, dispatchErr := stream.Consumer(context.Background(), dispatchConsumerName(spec.Project))
		_, githubErr := stream.Consumer(context.Background(), githubConsumerName(spec.Project))
		return dispatchErr == nil && githubErr == nil
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Consume: %v", err)
			}
		})
	}
}

func testJetStream(t *testing.T) (jetstream.JetStream, jetstream.Stream) {
	t.Helper()
	ctx := context.Background()
	js := testnats.JetStream(t)
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	if err != nil {
		t.Fatalf("create notification stream: %v", err)
	}
	return js, stream
}

func publish(t *testing.T, js jetstream.JetStream, subject string, data []byte) {
	t.Helper()
	if _, err := js.Publish(context.Background(), subject, data); err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
}

func assertNoAckPending(t *testing.T, stream jetstream.Stream, consumer string) {
	t.Helper()
	eventually(t, consumer+" has no acknowledgement pending", func() bool {
		item, err := stream.Consumer(context.Background(), consumer)
		if err != nil {
			return false
		}
		info, err := item.Info(context.Background())
		return err == nil && info.NumAckPending == 0
	})
}

func capturedIssueUpdatedEnvelope(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/dispatch/issue-updated.json")
	if err != nil {
		t.Fatalf("read captured issue.updated envelope: %v", err)
	}
	return data
}

func capturedIssueCreatedEnvelope(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/dispatch/issue-created.json")
	if err != nil {
		t.Fatalf("read captured issue.created envelope: %v", err)
	}
	return data
}

func envelopeJSON(t *testing.T, eventID, source, payload string) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"event_id":        eventID,
		"source":          source,
		"source_event_id": eventID + "-source",
		"topic":           "test",
		"dedupe_key":      eventID,
		"issued_at":       1,
		"payload_summary": "test event",
		"payload":         payload,
		"trace_id":        eventID + "-trace",
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return out
}

func writeHandler(name string, refusal *Refusal) Handler {
	return handlerFunc(func(ctx context.Context, tx pgx.Tx, _ Fact) (Result, error) {
		if _, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, name); err != nil {
			return Result{}, err
		}
		return Result{Refusal: refusal}, nil
	})
}

func createWrites(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `create table intake_test_writes (id bigserial primary key, handler text not null)`); err != nil {
		t.Fatalf("create write table: %v", err)
	}
}

func writeCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `select count(*) from intake_test_writes`).Scan(&count); err != nil {
		t.Fatalf("count writes: %v", err)
	}
	return count
}

func writeHandlers(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select handler from intake_test_writes order by id`)
	if err != nil {
		t.Fatalf("list handler writes: %v", err)
	}
	defer rows.Close()
	var handlers []string
	for rows.Next() {
		var handler string
		if err := rows.Scan(&handler); err != nil {
			t.Fatalf("scan handler write: %v", err)
		}
		handlers = append(handlers, handler)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate handler writes: %v", err)
	}
	return handlers
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test intake transactions")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	t.Cleanup(admin.Close)
	name := "legion_intake_test_" + randomSuffix(t)
	if _, err := admin.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	testURL := *base
	testURL.Path = "/" + name
	st, err := legionstore.Open(context.Background(), testURL.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate store: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(context.Background(), testURL.String())
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
