package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
)

// TestSearchMeaningLegsUseTheHNSWIndexNotASequentialScan proves each meaning candidate CTE's own
// shape - `order by embedding <=> $qvec limit $k` against embeddings alone, with no shared window
// spanning every kind, filtered to the query embedder's identity - is what lets pgvector's planner
// pick an Index Scan on embeddings_cosine (0072_embeddings_core.up.sql's HNSW index) rather than a
// sequential scan sorted in memory. It explains searchQueryMeaning itself, every parameter bound
// as runMeaningSearch binds it ($11 the fake embedder's identity), and reads each
// meaning_<kind>_candidates subtree of the plan, so the statements under test are the ones search
// runs, not a copy of them. enable_seqscan and enable_sort are forced off for this EXPLAIN only: a
// handful of test rows is too small for the planner to ever prefer the index on cost alone
// (reading a few rows via its primary key and sorting them in memory is cheaper than any index no
// matter what exists - confirmed empirically: without enable_sort off, the planner chose exactly
// that plan, an Index Scan on embeddings_pkey feeding a Sort node, not embeddings_cosine at all),
// so this proves the query's shape permits an index scan, which is the claim under test, not what
// a real corpus's planner would choose - search_bench_test.go's latency bound is what proves that
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

	const q = "index test"
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+searchQueryMeaning, q, "", searchDefaultLimit, firstTerm(q), headlineOptions,
		0, contracts.SearchKindDepth, searchFusionK, embed.Literal(queryVector()), searchMeaningFloor, embedder.Identity()).Scan(&planJSON); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("decode explain output (%v): %s", err, planJSON)
	}

	for _, kind := range []string{"issue", "document", "comment", "ask", "message"} {
		t.Run(kind, func(t *testing.T) {
			alias := "meaning_" + kind + "_candidates"
			leg := plan[0].Plan.find(func(node planNode) bool { return node.Alias == alias })
			if leg == nil {
				t.Fatalf("the plan has no node for %s:\n%s", alias, planString(t, planJSON))
			}
			legPlan, err := json.MarshalIndent(leg, "", "  ")
			if err != nil {
				t.Fatalf("re-encode %s: %v", alias, err)
			}
			if leg.find(func(node planNode) bool { return node.NodeType == "Seq Scan" }) != nil {
				t.Errorf("%s meaning leg's plan uses a Seq Scan, want an Index Scan on embeddings_cosine:\n%s", kind, legPlan)
			}
			cosine := leg.find(func(node planNode) bool { return node.IndexName == "embeddings_cosine" })
			if cosine == nil {
				t.Fatalf("%s meaning leg's plan never names embeddings_cosine, want it to use that index:\n%s", kind, legPlan)
			}
			if !strings.Contains(cosine.Filter+cosine.IndexCond, "model") {
				t.Errorf("%s meaning leg's index scan does not filter on embeddings.model:\n%s", kind, legPlan)
			}
		})
	}
}

// planNode is the part of one EXPLAIN (FORMAT JSON) plan node these tests read.
type planNode struct {
	NodeType  string     `json:"Node Type"`
	Alias     string     `json:"Alias,omitempty"`
	IndexName string     `json:"Index Name,omitempty"`
	IndexCond string     `json:"Index Cond,omitempty"`
	Filter    string     `json:"Filter,omitempty"`
	Plans     []planNode `json:"Plans,omitempty"`
}

// find answers the first node of the subtree rooted at node, depth first, that match accepts.
func (node *planNode) find(match func(planNode) bool) *planNode {
	if match(*node) {
		return node
	}
	for i := range node.Plans {
		if found := node.Plans[i].find(match); found != nil {
			return found
		}
	}
	return nil
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
