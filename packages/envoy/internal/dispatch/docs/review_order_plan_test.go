package docs

import (
	"context"
	"encoding/json"
	"testing"
)

// reviewOrderIndex is the index migration 0120 adds in docs.ReviewOrder's own order.
const reviewOrderIndex = "artifact_reviews_by_artifact_event_seq"

// The newest-review reads take one row in ReviewOrder: the approval an approval request names and
// a carry candidate's latest review, read at every request and every version that holds an
// answered block. Each must read that row from the index in ReviewOrder's order, the first entry
// of the document's range, rather than read every review of the document and sort them, which
// artifact_reviews_by_artifact (created_at desc, id desc) alone would make it do.
func TestNewestReviewReadsTakeTheReviewOrderIndex(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	// An empty table makes every plan cheap, so the planner is left the ordered reads alone: no
	// sequential scan and no bitmap scan, which reads its rows out of index order. What remains is
	// an index scan, and the test asks which index it reads and whether it sorts what it read.
	for _, setting := range []string{"set local enable_seqscan = off", "set local enable_bitmapscan = off"} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	document := "00000000-0000-4000-8000-000000000000"
	for _, query := range []struct {
		name string
		sql  string
		args []any
	}{
		{"the latest approval", latestApprovedReviewQuery, []any{document}},
		{"a person's latest review", latestReviewByQuery, []any{document, "alice@example.com"}},
	} {
		t.Run(query.name, func(t *testing.T) {
			var planJSON []byte
			if err := tx.QueryRow(ctx, "explain (format json) "+query.sql, query.args...).Scan(&planJSON); err != nil {
				t.Fatalf("explain query: %v", err)
			}
			var plans []struct {
				Plan json.RawMessage `json:"Plan"`
			}
			if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("decode explain output (%v): %s", err, planJSON)
			}
			if !planUsesIndex(t, plans[0].Plan, reviewOrderIndex) {
				t.Fatalf("query plan does not read %s:\n%s", reviewOrderIndex, planJSON)
			}
			if planSorts(t, plans[0].Plan) {
				t.Fatalf("query plan sorts the document's reviews; want the first entry of %s:\n%s", reviewOrderIndex, planJSON)
			}
		})
	}
}

// planSorts reports whether a plan node, at any depth, is a sort.
func planSorts(t *testing.T, planJSON json.RawMessage) bool {
	t.Helper()
	var node struct {
		NodeType string            `json:"Node Type"`
		Plans    []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.NodeType == "Sort" || node.NodeType == "Incremental Sort" {
		return true
	}
	for _, child := range node.Plans {
		if planSorts(t, child) {
			return true
		}
	}
	return false
}
