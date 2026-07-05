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

// TestEndToEndCheckRunToSummary drives the fully-wired path exactly as the
// listener wires it: a signed check_run webhook hits the real GitHubHandler,
// which records into a real cistore (via CIRecorderFunc) instead of publishing;
// the real StartSummaryLoop then debounces and publishes one rendered summary to
// pr.<n>.ci, which a live NATS subscriber receives. A second wave produces a
// second, grown summary; a quiet period produces none.
func TestEndToEndCheckRunToSummary(t *testing.T) {
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

	const secret = "s"
	ci := webhook.CIRecorderFunc(store.Record)
	handler := webhook.GitHubHandler(secret, "@legion", client, ci)

	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, 100*time.Millisecond, 20*time.Millisecond, logging.New("e2e"))

	// Subscribe before any publish. JetStream publishes are normal subject
	// messages, so a core subscriber on the topic receives them live.
	sub, err := client.Conn.SubscribeSync("notifications.github.o.r.pr.42.ci")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	post := func(name, status, conclusion, delivery string) {
		t.Helper()
		body := fmt.Sprintf(`{
			"action": %q,
			"check_run": {"name": %q, "status": %q, "conclusion": %q, "head_sha": "sha1",
				"pull_requests": [{"number": 42}]},
			"sender": {"login": "ci", "type": "Bot"},
			"repository": {"name": "r", "owner": {"login": "o"}, "full_name": "o/r"}
		}`, "completed", name, status, conclusion)
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", "check_run")
		req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("handler status = %d, body=%s", rr.Code, rr.Body.String())
		}
	}

	// Wave 1: two checks arrive close together.
	post("build", "in_progress", "", "d1")
	post("test", "in_progress", "", "d2")

	msg, err := sub.NextMsg(3 * time.Second)
	if err != nil {
		t.Fatalf("expected a debounced summary, got: %v", err)
	}
	got := string(msg.Data)
	if !strings.Contains(got, "build") || !strings.Contains(got, "test") {
		t.Fatalf("wave-1 summary should mention both checks: %s", got)
	}

	// No re-emit for an unchanged set.
	if m, err := sub.NextMsg(400 * time.Millisecond); err == nil {
		t.Fatalf("unexpected extra summary for unchanged set: %s", string(m.Data))
	}

	// Wave 2: a check completes → the set changed → exactly one more summary.
	post("build", "completed", "success", "d3")
	msg2, err := sub.NextMsg(3 * time.Second)
	if err != nil {
		t.Fatalf("expected a second summary after change, got: %v", err)
	}
	var env2 contracts.Envelope
	if err := json.Unmarshal(msg2.Data, &env2); err != nil {
		t.Fatalf("wave-2 envelope not JSON: %v", err)
	}
	var sum cistore.Summary
	if err := json.Unmarshal([]byte(env2.PayloadSummary), &sum); err != nil {
		t.Fatalf("wave-2 payload_summary not JSON: %v\n%s", err, env2.PayloadSummary)
	}
	inPassed := false
	for _, c := range sum.Passed.Checks {
		if c == "build" {
			inPassed = true
		}
	}
	if !inPassed || sum.Passed.Count != 1 {
		t.Fatalf("wave-2: build should be in passed{count:1}, got passed=%+v running=%+v", sum.Passed, sum.Running)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
