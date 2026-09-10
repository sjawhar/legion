package api

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestSearchLatencyOnCorpus measures GET /api/v1/search against a real corpus copy
// (scripts/restore-dispatch-dump.sh) and enforces the p95 < 100 ms bound from the spec.
// It is deliberately env-gated so CI never runs it.
func TestSearchLatencyOnCorpus(t *testing.T) {
	databaseURL := os.Getenv("DISPATCH_BENCH_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DISPATCH_BENCH_DATABASE_URL must point at a restored corpus copy")
	}

	handler := newCorpusSearchHandler(t, databaseURL)
	terms := []string{
		"dispatch",
		"search",
		"anchor",
		"astrolabe",
		`"merge queue" -daemon`,
		"legion",
		"proof",
		"document",
		"smoke",
		"token",
	}

	for _, term := range terms {
		assertCorpusSearchOK(t, handler, term)
	}

	const rounds = 10
	latencies := make([]time.Duration, 0, len(terms)*rounds)
	for range rounds {
		for _, term := range terms {
			started := time.Now()
			assertCorpusSearchOK(t, handler, term)
			latencies = append(latencies, time.Since(started))
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[(len(latencies)-1)/2]
	p95 := latencies[(len(latencies)*95+99)/100-1]
	maximum := latencies[len(latencies)-1]
	t.Logf("search latency over %d requests: p50=%v p95=%v max=%v", len(latencies), p50, p95, maximum)
	if p95 >= 100*time.Millisecond {
		t.Fatalf("p95 %v is not below 100ms", p95)
	}
}

func newCorpusSearchHandler(t *testing.T, databaseURL string) http.Handler {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open corpus database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate corpus database: %v", err)
	}

	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: time.Hour,
	})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}},
		},
		AgentToken:      "agent-token",
		RepoProjectsRaw: "owner/repo=TEST",
		ServerURL:       "https://dispatch.example",
		Docs:            documentService,
		Events:          broker,
	})
	if err != nil {
		t.Fatalf("create corpus API dependencies: %v", err)
	}

	mux := http.NewServeMux()
	Register(mux, deps)
	return mux
}

func assertCorpusSearchOK(t *testing.T, handler http.Handler, term string) {
	t.Helper()
	response := searchRequest(t, handler, url.Values{"q": {term}}.Encode())
	if response.Code != http.StatusOK {
		t.Fatalf("search %q: status=%d body=%s", term, response.Code, response.Body.String())
	}
}
