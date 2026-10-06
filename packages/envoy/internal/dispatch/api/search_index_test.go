package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan proves each meaning candidate CTE's own
// shape - `order by embedding <=> $qvec limit $k` against embeddings alone, with no shared window
// spanning every kind - is what lets pgvector's planner pick an Index Scan on embeddings_cosine
// (0072_embeddings_core.up.sql's HNSW index) rather than a sequential scan sorted in memory.
// enable_seqscan and enable_sort are forced off for this EXPLAIN only: a handful of test rows is
// too small for the planner to ever prefer the index on cost alone (reading a few rows via its
// primary key, kind = 'message', and sorting them in memory is cheaper than any index no matter
// what exists - confirmed empirically: without enable_sort off, the planner chose exactly that
// plan, an Index Scan on embeddings_pkey feeding a Sort node, not embeddings_cosine at all), so
// this proves the query's shape permits an index scan, which is the claim under test, not what a
// real corpus's planner would choose - search_bench_test.go's latency bound is what proves that
// at scale.
func TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan(t *testing.T) {
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"A title to embed for the index test": angledVector(0.9, 2),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createInteractionIssue(t, handler, "IDX1", "A title to embed for the index test", "Body text.")
	processAllPending(t, database, embedder)

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("set local enable_seqscan off: %v", err)
	}
	if _, err := tx.Exec(ctx, "set local enable_sort = off"); err != nil {
		t.Fatalf("set local enable_sort off: %v", err)
	}

	for _, leg := range []struct {
		kind, sql string
	}{
		{"issue", `select id from embeddings e join issues i on i.key = e.id
		            where e.kind = 'issue' and e.embedding is not null
		            order by e.embedding <=> $1 limit 20`},
		{"document", `select a.id from embeddings e join artifacts a on a.id::text = e.id
		               where e.kind = 'document' and e.embedding is not null
		               order by e.embedding <=> $1 limit 20`},
		{"comment", `select c.id from embeddings e join comments c on c.id::text = e.id
		              where e.kind = 'comment' and e.embedding is not null
		              order by e.embedding <=> $1 limit 20`},
		{"ask", `select k.id from embeddings e join asks k on k.id::text = e.id
		          where e.kind = 'ask' and e.embedding is not null
		          order by e.embedding <=> $1 limit 20`},
		{"message", `select m.id from embeddings e join messages m on m.id::text = e.id
		              where e.kind = 'message' and e.embedding is not null
		              order by e.embedding <=> $1 limit 20`},
	} {
		t.Run(leg.kind, func(t *testing.T) {
			var planJSON []byte
			if err := tx.QueryRow(ctx, "explain (format json) "+leg.sql, embed.Literal(queryVector())).Scan(&planJSON); err != nil {
				t.Fatalf("explain: %v", err)
			}
			plan := planString(t, planJSON)
			if strings.Contains(plan, `"Node Type": "Seq Scan"`) {
				t.Errorf("%s meaning leg's plan uses a Seq Scan, want an Index Scan on embeddings_cosine:\n%s", leg.kind, plan)
			}
			if !strings.Contains(plan, "embeddings_cosine") {
				t.Errorf("%s meaning leg's plan never names embeddings_cosine, want it to use that index:\n%s", leg.kind, plan)
			}
		})
	}
}

func planString(t *testing.T, planJSON []byte) string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(planJSON, &decoded); err != nil {
		t.Fatalf("decode explain output: %v", err)
	}
	pretty, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		t.Fatalf("re-encode explain output: %v", err)
	}
	return string(pretty)
}
