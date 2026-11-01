package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// The Replies page and its count run on every page view and every badge refresh, so both must read
// only the caller's unread rows through the partial index user_threads_unread, never a scan of
// user_threads, which holds a row for every thread anyone ever wrote in.
func TestMyRepliesQueriesReadTheUnreadIndex(t *testing.T) {
	_, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	for _, query := range []struct {
		name string
		sql  string
		args []any
	}{
		{"the count", myRepliesCountQuery, []any{"alice", "", nil}},
		{"a page", myRepliesPageQuery, []any{"alice", "", nil, 50, 0}},
		{"one issue's unread set", myRepliesPageQuery, []any{"alice", "TEST-1", nil, 200, 0}},
	} {
		t.Run(query.name, func(t *testing.T) {
			var planJSON []byte
			if err := tx.QueryRow(ctx, "explain (format json) "+query.sql, query.args...).Scan(&planJSON); err != nil {
				t.Fatalf("explain: %v", err)
			}
			var plans []struct {
				Plan json.RawMessage `json:"Plan"`
			}
			if err := json.Unmarshal(planJSON, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("decode explain output (%v): %s", err, planJSON)
			}
			if explaintest.SeqScansRelation(t, plans[0].Plan, "user_threads") {
				t.Fatalf("plan scans user_threads:\n%s", planJSON)
			}
			if !planReadsIndex(t, plans[0].Plan, "user_threads_unread") {
				t.Fatalf("plan does not read user_threads_unread:\n%s", planJSON)
			}
		})
	}
}
