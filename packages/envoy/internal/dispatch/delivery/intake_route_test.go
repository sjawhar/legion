package delivery

// What the intake does with each envelope it is handed: which kinds it records, writes and
// fetches for, which it acknowledges and nothing else, and which it logs as malformed.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testnats"
)

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
	startIntake(t, intake)

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

	// deliver records the event's time only once the handler that stored the row has returned
	// (intake.go deliver), so the row can be read before the freshness is: wait for it on a
	// deadline of its own rather than reading it once.
	got, err := GetSettings(ctx, pool)
	freshnessDeadline := time.Now().Add(10 * time.Second)
	for err == nil && got.LastEventAt == nil && time.Now().Before(freshnessDeadline) {
		time.Sleep(50 * time.Millisecond)
		got, err = GetSettings(ctx, pool)
	}
	if err != nil || got.LastEventAt == nil {
		t.Errorf("settings.LastEventAt not recorded after intake processed an event: %+v, %v", got, err)
	}
	_ = settings
}

// TestIntakeAcksAKindItDoesNotRecordWithoutLoggingOrWriting: the durable is handed every GitHub
// notification on the bus (githubIntakeSubject), CI settlements and comments among them. A CI
// settlement's payload carries arrays (cistore.Summary), which no handler here reads. It and a
// comment are acknowledged with no ERROR line, no GitHub call and no database write, the freshness
// row's last_event_at included.
func TestIntakeAcksAKindItDoesNotRecordWithoutLoggingOrWriting(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	logs := captureLogs(t)
	fake := newFakeGitHub(t)
	natsClient := intakeTestClient(t)
	startIntake(t, NewIntake(natsClient, pool, fake.newTestClient()))
	awaitBoundDurable(t, natsClient)

	checks, err := json.Marshal(cistore.Summary{
		Kind: "checks", Repo: "acme/widgets", Number: "7", SHA: "abc123",
		CheckRuns:  []cistore.CheckRunRef{{Name: "test", ID: 11}},
		Generation: 3, Snapshot: "snapshot",
		Passed: cistore.StatusGroup{Count: 1, Checks: []string{"test"}},
		Failed: cistore.StatusGroup{Checks: []string{}}, Running: cistore.StatusGroup{Checks: []string{}},
		Queued: cistore.StatusGroup{Checks: []string{}}, Cancelled: cistore.StatusGroup{Checks: []string{}},
		Skipped: cistore.StatusGroup{Checks: []string{}}, FailingChecks: []cistore.FailingCheck{},
	})
	if err != nil {
		t.Fatalf("encode the checks settlement: %v", err)
	}
	publishGitHubEnvelope(t, natsClient, contracts.GithubSubject("acme", "widgets", "pr.7.checks"), string(checks))
	publishGitHubEnvelope(t, natsClient, contracts.GithubSubject("acme", "widgets", "pr.7.comment"),
		`{"kind":"comment","action":"created","repo":"acme/widgets","number":"7","author":"octocat"}`)
	awaitAcknowledged(t, natsClient)

	for _, record := range logs() {
		if record["level"] == "ERROR" && strings.HasPrefix(fmt.Sprint(record["msg"]), "dispatch delivery:") {
			t.Errorf("intake logged an ERROR for a kind it does not record: %v", record)
		}
	}
	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if settings.LastEventAt != nil {
		t.Errorf("last_event_at = %v, want null: a kind intake does not record writes nothing", *settings.LastEventAt)
	}
	var prs int
	if err := pool.QueryRow(ctx, "select count(*) from delivery_pull_requests").Scan(&prs); err != nil {
		t.Fatalf("count pull requests: %v", err)
	}
	if runs := countRuns(t, ctx, pool); prs != 0 || runs != 0 {
		t.Errorf("stored %d pull requests and %d runs, want none", prs, runs)
	}
	if mints := fake.tokenMints.Load(); mints != 0 {
		t.Errorf("minted %d installation tokens, want none: a kind intake does not record calls no GitHub", mints)
	}
}

// TestIntakeAcksAnEnvelopeWithNoPayloadSilently: Envoy relays a GitHub event it builds no payload
// for (normalize.go's githubPayload answers "" for an event it has no case for, an installation
// event among them), on notifications.github.unknown.unknown.comment when the event names no
// repository. It carries no kind, so it is no kind intake records: acknowledged with no ERROR
// line, no GitHub call and no write.
func TestIntakeAcksAnEnvelopeWithNoPayloadSilently(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	logs := captureLogs(t)
	fake := newFakeGitHub(t)
	natsClient := intakeTestClient(t)
	startIntake(t, NewIntake(natsClient, pool, fake.newTestClient()))
	awaitBoundDurable(t, natsClient)

	publishGitHubEnvelope(t, natsClient, contracts.GithubSubject("unknown", "unknown", "comment"), "")
	awaitAcknowledged(t, natsClient)

	for _, record := range logs() {
		if record["level"] == "ERROR" && strings.HasPrefix(fmt.Sprint(record["msg"]), "dispatch delivery:") {
			t.Errorf("intake logged an ERROR for an envelope with no payload: %v", record)
		}
	}
	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if settings.LastEventAt != nil || fake.tokenMints.Load() != 0 {
		t.Errorf("an envelope with no payload wrote or fetched: last_event_at %v, %d token mints", settings.LastEventAt, fake.tokenMints.Load())
	}
}

// TestIntakeLogsAMalformedEnvelopeOfAKindItRecords: a pull request's or a workflow run's payload
// is the flat string map its handler reads. One carrying anything else is malformed: logged at
// ERROR, as every malformed envelope is, acknowledged, and nothing is written or fetched.
func TestIntakeLogsAMalformedEnvelopeOfAKindItRecords(t *testing.T) {
	for _, envelope := range []struct{ name, topic, payload string }{
		{"pr", contracts.GithubResourceSubject("acme", "widgets", "pr", "7"),
			`{"kind":"pr","action":"closed","repo":"acme/widgets","number":"7","merged":"true","labels":["non-task"]}`},
		{"workflow", contracts.GithubWorkflowSubject("acme", "widgets", "deploy_yml", "completed"),
			`{"kind":"workflow","action":"completed","repo":"acme/widgets","path":".github/workflows/deploy.yml","run_id":["1"]}`},
	} {
		t.Run(envelope.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			seedDeliverySettings(t, ctx, pool)
			logs := captureLogs(t)
			fake := newFakeGitHub(t)
			natsClient := intakeTestClient(t)
			startIntake(t, NewIntake(natsClient, pool, fake.newTestClient()))
			awaitBoundDurable(t, natsClient)

			publishGitHubEnvelope(t, natsClient, envelope.topic, envelope.payload)
			awaitAcknowledged(t, natsClient)

			logged := false
			for _, record := range logs() {
				if record["level"] == "ERROR" && record["msg"] == "dispatch delivery: decode envelope payload" && record["subject"] == envelope.topic {
					logged = true
				}
			}
			if !logged {
				t.Errorf("no ERROR \"dispatch delivery: decode envelope payload\" for %s; logged %v", envelope.topic, logs())
			}
			settings, err := GetSettings(ctx, pool)
			if err != nil {
				t.Fatalf("read settings: %v", err)
			}
			if settings.LastEventAt != nil || countRuns(t, ctx, pool) != 0 || fake.tokenMints.Load() != 0 {
				t.Errorf("a malformed envelope wrote or fetched: last_event_at %v, %d runs, %d token mints",
					settings.LastEventAt, countRuns(t, ctx, pool), fake.tokenMints.Load())
			}
		})
	}
}

// TestIntakeDiscardsAnEnvelopeItDoesNotWantWithoutCallingGitHub holds the cost of the one filter
// subject this release carries: the durable is handed every GitHub notification on the bus, and
// the ones this slice does not want -- another repository's workflow, a workflow file neither
// setting names -- must cost a decode and an acknowledgement, with no GitHub call and no run
// stored. Otherwise a wide filter would put the handler's serial GitHub work behind traffic it
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
	startIntake(t, intake)
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
	var info *natsgo.ConsumerInfo
	var lastSeq uint64
	waitFor(t, 30*time.Second, func() bool {
		stream, err := natsClient.JS().StreamInfo(bus.Stream)
		if err != nil {
			t.Fatalf("read stream info: %v", err)
		}
		info, err = natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		if err != nil {
			t.Fatalf("read consumer info: %v", err)
		}
		lastSeq = stream.State.LastSeq
		return info.AckFloor.Stream >= lastSeq && info.NumPending == 0
	}, func() string {
		return fmt.Sprintf("the intake did not acknowledge every envelope it discarded\n%s", intakeStateReport(t, natsClient))
	})
	t.Logf("discarded %d envelopes: ack_floor=%d last_seq=%d redelivered=%d",
		unwanted, info.AckFloor.Stream, lastSeq, info.NumRedelivered)
	if stored := countRuns(t, ctx, pool); stored != 0 {
		t.Fatalf("delivery_runs rows = %d, want 0 (a discarded envelope stores no run)", stored)
	}
}

// TestIntakeRoutesByTheSettingsInForceWhenAnEnvelopeArrives holds that a settings change reaches
// the running intake with no rebind: the durable's filter subject never changes, so the only way
// a new deploy workflow path takes effect is the poll publishing the new settings to the handler.
// After the change, an envelope for the new path is handled and one for the old path is discarded
// with no GitHub call.
func TestIntakeRoutesByTheSettingsInForceWhenAnEnvelopeArrives(t *testing.T) {
	shrinkIntakeFlowControl(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	oldPath, newPath := settings.DeployWorkflowPath, ".github/workflows/release.yml"

	var mu sync.Mutex
	fetched := map[int64]bool{}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("run_id"), 10, 64)
		if err != nil {
			t.Errorf("parse run id %q: %v", r.PathValue("run_id"), err)
		}
		mu.Lock()
		fetched[id] = true
		mu.Unlock()
		mustEncode(t, w, smallRun(id, time.Now().UTC()))
	})

	natsClient := intakeTestClient(t)
	intake := NewIntake(natsClient, pool, fake.newTestClient())
	startIntake(t, intake)
	awaitBoundDurable(t, natsClient)

	settings.DeployWorkflowPath = newPath
	if _, err := PutSettings(ctx, pool, settings, model.Actor{Kind: "system", ID: "delivery-test"}); err != nil {
		t.Fatalf("change the deploy workflow path: %v", err)
	}
	// Two poll intervals: the change is read on the first tick after the write, whichever side of
	// a tick the write lands.
	time.Sleep(2 * settingsPollInterval)

	const onNewPath, onOldPath = int64(8001), int64(8002)
	publishWorkflowEnvelope(t, natsClient, "acme", "widgets", newPath, onNewPath)
	awaitRuns(t, ctx, pool, natsClient, 1, 30*time.Second)
	publishWorkflowEnvelope(t, natsClient, "acme", "widgets", oldPath, onOldPath)

	// The old-path envelope is acknowledged without being fetched.
	waitFor(t, 30*time.Second, func() bool {
		info, err := natsClient.JS().ConsumerInfo(bus.Stream, deliveryConsumerName)
		return err == nil && info.NumPending == 0 && info.NumAckPending == 0
	}, func() string {
		return fmt.Sprintf("the intake did not acknowledge the old path's envelope\n%s", intakeStateReport(t, natsClient))
	})
	mu.Lock()
	defer mu.Unlock()
	if !fetched[onNewPath] {
		t.Fatalf("the run on the new deploy workflow path %s was never fetched", newPath)
	}
	if fetched[onOldPath] {
		t.Fatalf("the run on the old deploy workflow path %s was fetched after the settings moved off it", oldPath)
	}
	if stored := countRuns(t, ctx, pool); stored != 1 {
		t.Fatalf("delivery_runs rows = %d, want 1 (only the new path's run)", stored)
	}
}
