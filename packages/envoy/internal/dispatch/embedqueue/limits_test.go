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
// throttling is set, 404 DeploymentNotFound while missing is set (the deployment name is not one
// the resource has), and vectors otherwise, recording each request's inputs.
type azureFake struct {
	mu         sync.Mutex
	throttling bool
	missing    bool
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
	throttling, missing := f.throttling, f.missing
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if throttling {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"RateLimitReached","message":"Rate limit is exceeded."}}`))
		return
	}
	if missing {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist."}}`))
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

// denseCJK is n runes of CJK ideographs (3 UTF-8 bytes each, 96,000 bytes at maxInputChars),
// varied by seed: text Azure counts at well over EstimateTokens' 4 characters a token, the kind
// that timed out a 40-text call.
func denseCJK(n, seed int) string {
	var b strings.Builder
	b.Grow(3 * n)
	for i := range n {
		b.WriteRune(rune(0x4E00 + (i*7919+seed*104729)%0x5000))
	}
	return b.String()
}

// azureLimits are the real AzureClient's limits at a 300,000 tokens-a-minute quota.
func azureLimits(t *testing.T) embed.Limits {
	t.Helper()
	client, err := embed.NewAzure(embed.AzureConfig{
		Endpoint:        "https://example.services.ai.azure.com/models",
		Deployment:      "embed-v-4-0",
		TokensPerMinute: 300_000,
		APIKey:          "k",
	})
	if err != nil {
		t.Fatalf("NewAzure: %v", err)
	}
	return client.Limits()
}

func batchBytes(texts []string) int {
	total := 0
	for _, text := range texts {
		total += embed.BoundedBytes(text)
	}
	return total
}

// With Azure's limits, a full batch of 96 maxInputChars CJK texts (96,000 bytes each) splits into
// groups of three, each at most Azure's 320,000-byte budget, every row in exactly one group and in
// order; the token ceiling alone (MaxBatchBytes 0) groups the same rows 25 at a time, as before.
func TestChunkForBudgetKeepsEachCallWithinTheByteBudget(t *testing.T) {
	limits := azureLimits(t)
	if limits.MaxBatchBytes != 320_000 || limits.MaxBatchTexts != 96 {
		t.Fatalf("Azure limits = %+v, want 96 texts and 320,000 bytes", limits)
	}
	rows := make([]pendingRow, limits.MaxBatchTexts)
	for i := range rows {
		rows[i] = pendingRow{kind: "comment", id: fmt.Sprint(i), text: denseCJK(32_000, i)}
	}
	check := func(limits embed.Limits, wantSizes []int) {
		t.Helper()
		groups := chunkForBudget(rows, limits)
		sizes := make([]int, len(groups))
		next := 0
		for g, group := range groups {
			sizes[g] = len(group.rows)
			texts := make([]string, len(group.rows))
			for i, row := range group.rows {
				if row.id != rows[next].id {
					t.Fatalf("group %d row %d is %s, want %s: every row once, in order", g, i, row.id, rows[next].id)
				}
				texts[i] = row.text
				next++
			}
			if limits.MaxBatchBytes > 0 && batchBytes(texts) > limits.MaxBatchBytes {
				t.Errorf("group %d carries %d bytes, over the %d budget", g, batchBytes(texts), limits.MaxBatchBytes)
			}
			if group.tokens != embed.EstimateTokens(texts) || group.tokens > limits.BackgroundTokensPerMinute {
				t.Errorf("group %d tokens = %d, want its texts' estimate %d, at most %d", g, group.tokens, embed.EstimateTokens(texts), limits.BackgroundTokensPerMinute)
			}
		}
		if next != len(rows) {
			t.Fatalf("groups hold %d rows, want all %d", next, len(rows))
		}
		if fmt.Sprint(sizes) != fmt.Sprint(wantSizes) {
			t.Errorf("group sizes = %v, want %v", sizes, wantSizes)
		}
	}
	threes := make([]int, 32)
	for i := range threes {
		threes[i] = 3
	}
	check(limits, threes)
	limits.MaxBatchBytes = 0
	check(limits, []int{25, 25, 25, 21})
}

// timeoutEmbedder answers like a place whose request timeout a call over its byte budget passes:
// such a call fails, not throttled, as Azure's client does when the 60 s run out; any other call
// embeds. It records every call.
type timeoutEmbedder struct {
	placeEmbedder
}

func (e *timeoutEmbedder) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	if size := batchBytes(texts); size > e.limits.MaxBatchBytes {
		e.calls = append(e.calls, append([]string(nil), texts...))
		return nil, fmt.Errorf("embed: azure call: %d bytes: %w", size, context.DeadlineExceeded)
	}
	return e.placeEmbedder.Embed(ctx, texts, inputType)
}

// A full batch of 96 pending maxInputChars CJK rows, through ProcessBatch, under an embedder whose
// call over its byte budget would time out: every call stays within the budget and succeeds the
// first time, so nothing is bisected or sent twice, and every row is current.
func TestAFullBatchOfDenseTextIsEmbeddedWithinTheByteBudgetWithoutBisection(t *testing.T) {
	database := storetest.Open(t)
	limits := azureLimits(t)
	// The token pace is not under test: a ceiling this high never waits.
	limits.BackgroundTokensPerMinute = 100_000_000
	for i := range limits.MaxBatchTexts {
		seedIssue(t, database, "DENS", fmt.Sprintf("DENS-%d", i+1), denseCJK(32_000, i))
	}
	embedder := &timeoutEmbedder{placeEmbedder{identity: configuredIdentity, limits: limits}}

	succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), Deps{Store: database, Embedder: embedder})
	if err != nil || succeeded != 96 || failed != 0 || blocked || throttled {
		t.Fatalf("ProcessBatch = %d, %d, %v, %v, %v; want all 96 committed", succeeded, failed, blocked, throttled, err)
	}
	seen := map[string]bool{}
	for i, call := range embedder.calls {
		if size := batchBytes(call); size > limits.MaxBatchBytes {
			t.Errorf("call %d carried %d bytes, over the %d budget", i, size, limits.MaxBatchBytes)
		}
		for _, text := range call {
			if seen[text] {
				t.Errorf("call %d sent a text a second time: a call failed and was bisected", i)
			}
			seen[text] = true
		}
	}
	if len(embedder.calls) != 32 || len(seen) != 96 {
		t.Errorf("%d calls carrying %d distinct texts, want 32 calls of three: the fewest the budget allows", len(embedder.calls), len(seen))
	}
	if got := pendingCount(t, database); got != 0 {
		t.Errorf("pending after the batch = %d, want 0", got)
	}
}

func callSizes(calls [][]string) []int {
	sizes := make([]int, len(calls))
	for i, call := range calls {
		sizes[i] = len(call)
	}
	return sizes
}
