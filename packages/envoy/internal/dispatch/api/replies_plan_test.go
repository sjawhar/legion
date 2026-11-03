package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// The Inbox's thread read and its count run on every page view and every badge refresh, so each
// must read only the caller's own rows through the partial index its state has, never a scan of
// user_threads, which holds a row for every thread anyone ever wrote in: user_threads_unread
// (migration 0095) for state=unread, user_threads_login_replied (migration 20261103071942) for
// state=all.
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
	// An unpaged read: the limit is null, which is what an Inbox read sends.
	var everyRow *int
	oneIssuesPage := 200
	for _, query := range []struct {
		name  string
		sql   string
		args  []any
		index string
	}{
		{"the count", myRepliesCountQuery, []any{"alice", "", nil}, "user_threads_unread"},
		{"every unread row", myRepliesPageQuery, []any{"alice", "", nil, everyRow, 0}, "user_threads_unread"},
		{"one issue's unread set", myRepliesPageQuery, []any{"alice", "TEST-1", nil, &oneIssuesPage, 0}, "user_threads_unread"},
		{"every row under state=all", myRepliesAllPageQuery, []any{"alice", "", nil, everyRow, 0}, "user_threads_login_replied"},
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
			if !planReadsIndex(t, plans[0].Plan, query.index) {
				t.Fatalf("plan does not read %s:\n%s", query.index, planJSON)
			}
		})
	}
}
