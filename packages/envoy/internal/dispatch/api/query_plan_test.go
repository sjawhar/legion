package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// Ask reads resolve their opening event and edit history by the payload id. Both
// paths must use the partial events payload index rather than scale with global
// event history.
func TestAskEventQueriesAvoidSequentialEventsScan(t *testing.T) {
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
		{
			name: "attach opened event ids",
			sql: `select payload->>'id', min(id)
				from events
				where type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited')
				  and payload->>'id' = any($1)
				group by payload->>'id'`,
			args: []any{[]string{"ask-id"}},
		},
		{
			name: "load ask edits",
			sql: `select payload->'previous', payload->'edited_by', created_at
				from events
				where type = 'ask.edited' and payload->>'id' = $1
				order by id asc`,
			args: []any{"ask-id"},
		},
	} {
		t.Run(query.name, func(t *testing.T) {
			assertPlanAvoidsSequentialRelation(t, ctx, tx, query.sql, "events", query.args...)
		})
	}
}

// Owner comment lists and recursive reply traversal are hot interactive paths.
// They must seek their owner or parent instead of scanning unrelated comments.
func TestCommentQueriesAvoidSequentialCommentsScan(t *testing.T) {
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
		{
			name: "load issue owner comments",
			sql: `select ` + commentColumns + `
				from comments
				where issue_key = $1 and ($2 = '' or anchor->>'artifact_id' = $2)
				order by created_at, id`,
			args: []any{"TEST-1", ""},
		},
		{
			name: "load comment reply chain",
			sql:  fmt.Sprintf(replyChainQuery, "reply_to"),
			args: []any{"10000000-0000-0000-0000-000000000001"},
		},
		{
			name: "load inbox ask reply chains",
			sql:  inboxAskReplyChainsQuery,
			args: []any{[]string{"20000000-0000-0000-0000-000000000002"}},
		},
	} {
		t.Run(query.name, func(t *testing.T) {
			assertPlanAvoidsSequentialRelation(t, ctx, tx, query.sql, "comments", query.args...)
		})
	}
}

func assertPlanAvoidsSequentialRelation(t *testing.T, ctx context.Context, tx queryer, query, relation string, args ...any) {
	t.Helper()
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (format json) "+query, args...).Scan(&planJSON); err != nil {
		t.Fatalf("explain query: %v", err)
	}
	var plans []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &plans); err != nil {
		t.Fatalf("decode explain output: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("explain plans = %#v, want one plan", plans)
	}
	if planNodeSeqScansRelation(t, plans[0].Plan, relation) {
		t.Fatalf("query plan sequentially scans %s; want an index scan:\n%s", relation, planJSON)
	}
}
