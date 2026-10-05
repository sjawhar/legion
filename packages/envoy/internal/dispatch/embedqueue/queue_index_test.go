package embedqueue

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// explainPlan runs an EXPLAIN (FORMAT JSON) and returns the pretty-printed plan, for a test to
// search for the node types and index names it asserts on.
func explainPlan(t *testing.T, tx pgx.Tx, ctx context.Context, sql string) string {
	t.Helper()
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+sql).Scan(&planJSON); err != nil {
		t.Fatalf("explain: %v", err)
	}
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

// TestPendingStatusMinQueryUsesTheIndexOnlyMinMaxRewrite proves pendingStatus's min(next_attempt_at)
// query (run on its own, not paired with count(*) in the same statement - see pendingStatus's own
// doc comment for why the pairing matters) gets Postgres's min/max-via-index-scan rewrite
// (preprocess_minmax_aggregates): an Index Only Scan on embeddings_pending bounded by a Limit,
// rather than a scan of every row the predicate matches followed by an aggregate. The negative
// control proves the rewrite genuinely depends on the split: the old combined
// count(*) + min(...) shape this replaced loses it, which is the whole reason pendingStatus runs
// two queries now instead of one.
func TestPendingStatusMinQueryUsesTheIndexOnlyMinMaxRewrite(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "IDXP", "IDXP-1", "A title")

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	plan := explainPlan(t, tx, ctx, `
		select min(next_attempt_at) from embeddings
		where embedded_hash is distinct from content_hash and not dead
	`)
	if !strings.Contains(plan, `"Index Only Scan"`) {
		t.Errorf("pendingStatus's min query plan has no Index Only Scan, want the min/max rewrite to apply:\n%s", plan)
	}
	if !strings.Contains(plan, "embeddings_pending") {
		t.Errorf("pendingStatus's min query plan never names embeddings_pending, want it to use that index:\n%s", plan)
	}
}

func TestPendingStatusOldCombinedQueryLosesTheIndexOnlyRewrite(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "IDXC", "IDXC-1", "A title")

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	plan := explainPlan(t, tx, ctx, `
		select count(*), min(next_attempt_at) from embeddings
		where embedded_hash is distinct from content_hash and not dead
	`)
	if strings.Contains(plan, `"Index Only Scan"`) {
		t.Errorf("the combined count(*)+min(...) query got an Index Only Scan after all - this negative control no longer demonstrates why pendingStatus splits the query, re-check whether the split is still needed:\n%s", plan)
	}
}
