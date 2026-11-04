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
//
// The rows below are what makes that a claim about the queries. A plan is a judgement about data,
// and on an empty user_threads the partial index and user_threads_thread cost the same, so which
// one Postgres takes turns on what the table's statistics happen to be: a clone of the template,
// whose user_threads has never been analysed, takes the partial index, and a database the harness
// emptied and analysed (pgclone's reset) takes user_threads_thread with the unread rows filtered
// out afterwards, which is how the count failed in CI run 41821928631. Seeded with a table of
// threads where the caller has a handful unread, the partial index is the cheaper plan by what it
// skips, and both queries take it with nothing turned off.
func TestMyRepliesQueriesReadTheUnreadIndex(t *testing.T) {
	t.Parallel()
	_, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	// Threads of 200 other people, a twentieth of them unread, and alice's own twenty, of which
	// the five on TEST-1 are unread: the shape the page reads, where the caller's unread rows are
	// a small part of the table. The columns are the triggers' own (migration 0095); user_threads
	// references no other table, so these rows need no issue, ask or project of their own.
	if _, err := database.Pool.Exec(ctx, `
		insert into user_threads (login, thread_id, kind, project_key, issue_key, read_through, replied_at, last_reply_id)
		select 'reader' || (n % 200), gen_random_uuid(), 'comment', 'TEST', 'TEST-' || (n % 50),
		       now() - interval '1 day',
		       case when n % 20 = 0 then now() end,
		       case when n % 20 = 0 then gen_random_uuid() end
		from generate_series(1, 2000) as n;
	`); err != nil {
		t.Fatalf("seed the other readers' threads: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into user_threads (login, thread_id, kind, project_key, issue_key, read_through, replied_at, last_reply_id)
		select 'alice', gen_random_uuid(), 'comment', 'TEST', 'TEST-' || (n % 4),
		       now() - interval '1 day',
		       case when n % 4 = 1 then now() end,
		       case when n % 4 = 1 then gen_random_uuid() end
		from generate_series(1, 20) as n;
	`); err != nil {
		t.Fatalf("seed alice's threads: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `analyze user_threads`); err != nil {
		t.Fatalf("analyze user_threads: %v", err)
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
			if err := database.Pool.QueryRow(ctx, "explain (format json) "+query.sql, query.args...).Scan(&planJSON); err != nil {
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
