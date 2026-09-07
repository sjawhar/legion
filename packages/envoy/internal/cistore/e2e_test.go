package cistore_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/webhook"
)

// TestEndToEndCheckRunToChecks drives the listener's real webhook, CI store,
// and NATS loop. CI webhooks publish nothing raw; one settled checks envelope
// is emitted for the PR after the quiet period.
func TestEndToEndCheckRunToChecks(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("start nats: %v", err)
	}
	defer ctr.Terminate(ctx)
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("nats uri: %v", err)
	}

	client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Conn.Close()
	store, err := cistore.Open(client.Conn, cistore.WithReplicas(1), cistore.WithTTL(time.Hour))
	if err != nil {
		t.Fatalf("open cistore: %v", err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := store.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("cache ready: %v", err)
	}

	const (
		secret = "s"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	ci := webhook.CIRecorderFuncs{RecordFunc: store.Record, RecordSuiteFunc: store.RecordSuite, RecordHeadFunc: store.RecordHead}
	handler := webhook.GitHubHandler(secret, "@legion", "", client, ci)
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, 100*time.Millisecond, 20*time.Millisecond, logging.New("e2e"))

	sub, err := client.Conn.SubscribeSync("notifications.github.example-org.example-repo.pr.42.checks")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	postEvent := func(event, body, delivery string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("handler status = %d, body=%s", rr.Code, rr.Body.String())
		}
	}
	postEvent("pull_request", fmt.Sprintf(`{
		"action": "opened", "number": 42,
		"pull_request": {"head": {"sha": %q}},
		"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
	}`, sha), "d0")

	post := func(name, status, conclusion, delivery string) {
		t.Helper()
		body := fmt.Sprintf(`{
			"action": "completed",
			"check_run": {"id": 1, "name": %q, "status": %q, "conclusion": %q, "head_sha": %q,
				"pull_requests": [{"number": 42}]},
			"sender": {"login": "ci", "type": "Bot"},
			"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
		}`, name, status, conclusion, sha)
		postEvent("check_run", body, delivery)
	}

	post("build", "completed", "success", "d1")
	post("lint", "completed", "failure", "d2")
	msg, err := sub.NextMsg(3 * time.Second)
	if err != nil {
		t.Fatalf("expected settled checks envelope, got: %v", err)
	}
	var env contracts.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		t.Fatalf("checks envelope not JSON: %v", err)
	}
	if env.Topic != "notifications.github.example-org.example-repo.pr.42.checks" {
		t.Fatalf("topic = %q", env.Topic)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env.Payload), &sum); err != nil {
		t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
	}
	if sum.Kind != "checks" || sum.Passed.Count != 1 || sum.Failed.Count != 1 {
		t.Fatalf("unexpected checks summary: %+v", sum)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
