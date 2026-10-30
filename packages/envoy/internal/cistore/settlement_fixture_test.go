package cistore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/webhook"
)

// fixtureName is a check name as packages/contracts/fixtures/checks-settlement writes it: a string,
// or {repeat, times, then}, repeat written times over and then then.
type fixtureName string

func (n *fixtureName) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*n = fixtureName(name)
		return nil
	}
	var built struct {
		Repeat string `json:"repeat"`
		Times  int    `json:"times"`
		Then   string `json:"then"`
	}
	if err := json.Unmarshal(data, &built); err != nil {
		return err
	}
	*n = fixtureName(strings.Repeat(built.Repeat, built.Times) + built.Then)
	return nil
}

// The listener settles a head's check runs as the shared fixture
// packages/contracts/fixtures/checks-settlement/listener-rules.json says: each run is delivered as
// its check_run webhook through the real handler, configured with the fixture's reviewer App, into
// the real CI store, and the summary loop's settlement names each check's highest-id run, its name
// cut as the store keys it, the failed and cancelled checks, and a tester or architect run only from
// the reviewer App. The Legion daemon's pass builds a settlement for a head the listener never
// settled from the same rules, and its own test (packages/daemon/internal/daemon,
// TestThePassSettlesCheckRunsAsEnvoysListenerDoes) holds it to the same fixture, so a rule changed on
// one side and not the other fails one of the two.
func TestTheListenerSettlesCheckRunsAsTheSharedFixtureSays(t *testing.T) {
	encoded, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "checks-settlement", "listener-rules.json"))
	if err != nil {
		t.Fatalf("read the shared fixture: %v", err)
	}
	var fixture struct {
		ReviewerAppID string                       `json:"reviewerAppId"`
		CheckRuns     []map[string]json.RawMessage `json:"checkRuns"`
		Settlement    struct {
			CheckRuns []struct {
				Name fixtureName `json:"name"`
				ID   uint64      `json:"id"`
			} `json:"check_runs"`
			Failed    []fixtureName `json:"failed"`
			Cancelled []fixtureName `json:"cancelled"`
		} `json:"settlement"`
	}
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatalf("decode the shared fixture: %v", err)
	}
	var wantRuns []cistore.CheckRunRef
	for _, run := range fixture.Settlement.CheckRuns {
		wantRuns = append(wantRuns, cistore.CheckRunRef{Name: string(run.Name), ID: run.ID})
	}
	names := func(list []fixtureName) []string {
		out := []string{}
		for _, name := range list {
			out = append(out, string(name))
		}
		return out
	}
	wantFailed, wantCancelled := names(fixture.Settlement.Failed), names(fixture.Settlement.Cancelled)

	ctx := context.Background()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("bus connect: %v", err)
	}
	defer client.Close()
	store, err := cistore.Open(client.Conn, logging.New("test"), cistore.WithReplicas(1), cistore.WithTTL(time.Hour))
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
		head   = "abcdef1234567890abcdef1234567890abcdef12"
	)
	handler := webhook.GitHubHandler(secret, "@legion", fixture.ReviewerAppID, client, store)
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	cistore.StartSummaryLoop(loopCtx, store, client, 300*time.Millisecond, 20*time.Millisecond, metrics.New().NewGauge("held", ""), logging.New("fixture"))
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
		"pull_request": {"head": {"sha": %q}, "updated_at": "2026-10-28T08:00:00Z"},
		"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
	}`, head), "d0")
	for i, run := range fixture.CheckRuns {
		var name fixtureName
		if err := json.Unmarshal(run["name"], &name); err != nil {
			t.Fatalf("decode a run's name: %v", err)
		}
		run["name"], _ = json.Marshal(string(name))
		run["head_sha"], _ = json.Marshal(head)
		run["pull_requests"] = json.RawMessage(`[{"number": 42}]`)
		checkRun, _ := json.Marshal(run)
		postEvent("check_run", `{"action": "completed", "check_run": `+string(checkRun)+`,
			"sender": {"login": "ci", "type": "Bot"},
			"repository": {"name": "example-repo", "owner": {"login": "example-org"}}}`, fmt.Sprintf("d%d", i+1))
	}

	var last cistore.Summary
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		msg, err := sub.NextMsg(time.Until(deadline))
		if err != nil {
			break
		}
		var env contracts.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			t.Fatalf("checks envelope not JSON: %v", err)
		}
		if err := json.Unmarshal([]byte(env.Payload), &last); err != nil {
			t.Fatalf("checks payload not JSON: %v\n%s", err, env.Payload)
		}
		if len(last.CheckRuns) == len(wantRuns) {
			break
		}
	}
	if !slices.Equal(last.CheckRuns, wantRuns) || !slices.Equal(last.Failed.Checks, wantFailed) || !slices.Equal(last.Cancelled.Checks, wantCancelled) {
		t.Fatalf("the listener's settlement\nruns %v\nfailed %v\ncancelled %v\nwant the fixture's\nruns %v\nfailed %v\ncancelled %v",
			last.CheckRuns, last.Failed.Checks, last.Cancelled.Checks, wantRuns, wantFailed, wantCancelled)
	}
}
