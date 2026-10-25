package embedqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// azureFake stands in for an Azure AI Foundry deployment's embeddings route: it answers 429 while
// throttling is set, and vectors otherwise, recording each request's inputs.
type azureFake struct {
	mu         sync.Mutex
	throttling bool
	requests   [][]string
}

func (f *azureFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Input []string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, request.Input)
	throttling := f.throttling
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if throttling {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"RateLimitReached","message":"Rate limit is exceeded."}}`))
		return
	}
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]item, len(request.Input))
	for i := range data {
		data[i] = item{Index: i, Embedding: oneVector()}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (f *azureFake) take() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := f.requests
	f.requests = nil
	return requests
}

// An Azure 429, through the real AzureClient, is a throttle: the whole batch is retried as one
// request - never bisected, never confirmed with a canary - however many times it recurs, and
// nothing is dead-lettered; once Azure answers, the batch is embedded under the Azure identity.
func TestAnAzureThrottleRetriesTheWholeBatchAndNeverDeadLetters(t *testing.T) {
	database := storetest.Open(t)
	ids := []string{"AZTH-1", "AZTH-2", "AZTH-3"}
	for _, id := range ids {
		seedIssue(t, database, "AZTH", id, "Title of "+id)
	}
	fake := &azureFake{throttling: true}
	server := httptest.NewServer(fake)
	defer server.Close()
	client, err := embed.NewAzure(embed.AzureConfig{
		Endpoint:        server.URL + "/models",
		Deployment:      "embed-v-4-0",
		TokensPerMinute: 300_000,
		APIKey:          "test-key",
		HTTPClient:      server.Client(),
	})
	if err != nil {
		t.Fatalf("NewAzure: %v", err)
	}
	deps := Deps{Store: database, Embedder: client}

	for cycle := range retry.DeadLetterAttempts + 2 {
		for _, id := range ids {
			forceEligible(t, database, "issue", id)
		}
		succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil || blocked || !throttled || succeeded != 0 || failed != len(ids) {
			t.Fatalf("cycle %d: ProcessBatch = %d, %d, %v, %v, %v; want 0, %d, not blocked, throttled", cycle, succeeded, failed, blocked, throttled, err, len(ids))
		}
		if requests := fake.take(); len(requests) != 1 || len(requests[0]) != len(ids) {
			t.Fatalf("cycle %d: Azure saw %v, want one request carrying the whole batch", cycle, requests)
		}
	}
	var dead, confirmed int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) filter (where dead), coalesce(sum(confirmed_failures), 0) from embeddings where kind = 'issue'
	`).Scan(&dead, &confirmed); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	if dead != 0 || confirmed != 0 {
		t.Fatalf("%d dead rows, %d confirmed failures after a sustained throttle, want 0 and 0", dead, confirmed)
	}

	fake.mu.Lock()
	fake.throttling = false
	fake.mu.Unlock()
	for _, id := range ids {
		forceEligible(t, database, "issue", id)
	}
	succeeded, failed, _, throttled, err := ProcessBatch(context.Background(), deps)
	if err != nil || throttled || succeeded != len(ids) || failed != 0 {
		t.Fatalf("ProcessBatch once Azure answers = %d, %d, %v, %v; want %d, 0", succeeded, failed, throttled, err, len(ids))
	}
	var models []string
	if err := database.Pool.QueryRow(context.Background(), `select array_agg(distinct model) from embeddings where kind = 'issue'`).Scan(&models); err != nil {
		t.Fatalf("read models: %v", err)
	}
	if len(models) != 1 || models[0] != client.Identity() {
		t.Errorf("models %v, want only the Azure identity %q", models, client.Identity())
	}
}

// A batch is at most the embedder's own MaxBatchTexts, and its token ceiling is the embedder's
// own BackgroundTokensPerMinute: two rows whose estimates together pass it go in separate calls,
// and the shared bucket is capped at it.
func TestBatchSizeAndTokenCeilingFollowTheEmbeddersLimits(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	// 30,400 characters is 7,600 estimated tokens: one fits under a 15,000 ceiling, two do not.
	long := strings.Repeat("x", 30_400)
	for i := range 2 {
		seedIssue(t, database, "LIMT", fmt.Sprintf("LIMT-%d", i+1), fmt.Sprintf("%d%s", i, long[1:]))
	}
	for i := range 3 {
		seedIssue(t, database, "LIMT", fmt.Sprintf("LIMT-%d", i+3), fmt.Sprintf("Short %d", i))
	}
	embedder := newPlaceEmbedder(configuredIdentity)
	embedder.limits = embed.Limits{MaxBatchTexts: 2, BackgroundTokensPerMinute: 15_000}
	deps := Deps{Store: database, Embedder: embedder}

	succeeded, failed, _, _, err := ProcessBatch(ctx, deps)
	if err != nil || succeeded != 2 || failed != 0 {
		t.Fatalf("first ProcessBatch = %d, %d, %v; want the two long rows", succeeded, failed, err)
	}
	if len(embedder.calls) != 2 || len(embedder.calls[0]) != 1 || len(embedder.calls[1]) != 1 {
		t.Fatalf("calls %d of sizes %v, want two calls of one long row each: together they pass the ceiling", len(embedder.calls), callSizes(embedder.calls))
	}
	var available float64
	if err := database.Pool.QueryRow(ctx, `select tokens_available from embeddings_rate_limit`).Scan(&available); err != nil {
		t.Fatalf("read the bucket: %v", err)
	}
	if available > 15_000 {
		t.Errorf("tokens_available = %v, want at most the embedder's 15,000 ceiling", available)
	}

	scan(ctx, deps)
	if sizes := callSizes(embedder.calls[2:]); len(sizes) != 2 || sizes[0] != 2 || sizes[1] != 1 {
		t.Errorf("the short rows went in calls of sizes %v, want [2 1]: MaxBatchTexts is 2", sizes)
	}
	if got := pendingCount(t, database); got != 0 {
		t.Errorf("pending after the scan = %d, want 0", got)
	}
}

func callSizes(calls [][]string) []int {
	sizes := make([]int, len(calls))
	for i, call := range calls {
		sizes[i] = len(call)
	}
	return sizes
}
