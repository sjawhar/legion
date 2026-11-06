package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// The Inbox's thread read and its count run on every page view and every badge refresh, so each
// must read only the caller's own rows through the index its state has, never a scan of
// user_threads, which holds a row for every thread anyone ever wrote in, and never a sort of what
// it reads, since the index already answers in the state's own order: user_threads_unread
// (migration 0095) for state=unread, user_threads_inbox (20261105093502) for state=inbox and
// user_threads_dismissed (20261105093503) for state=dismissed, which is also the cursor the
// Dismissed view resumes from.
//
// The Agent half of each state is here too. Its rows come from the messages themselves, read one
// root at a time (api/replies_agent.go), and the dismissal it tests is a row of
// user_agent_thread_dismissed (20261105093505): the Inbox's own reads take the person's marks as
// one set, which the login index answers, and the Dismissed view reads those marks in the order it
// answers in, which is the same index's other half. Neither may scan that table,
// and neither may sort it: the sorts those plans do carry are the list's own - the order the
// Inbox's two states answer in, over the roots they found - so what each Agent case holds is that
// no sort stands in for the dismissal, named by its column.
//
// The last case is no read at all but the write that clears a mark, which is here because it is
// the one reader of that table's key: every session reply runs it, and its column order is the
// only thing keeping it off a scan.
//
// Bitmap scans are off with sequential scans: on a fixture this small a bitmap scan of either index
// is the cheapest plan, and a bitmap scan answers in heap order, which loses exactly the order
// these reads are pinned to.
func TestMyRepliesQueriesReadTheirStatesIndex(t *testing.T) {
	_, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range []string{"set local enable_seqscan = off", "set local enable_bitmapscan = off"} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	// An unpaged read: the limit is null, which is what an Inbox read sends. A Dismissed page is
	// 200 rows from a cursor.
	var everyRow *int
	oneIssuesPage := 200
	dismissedPage := myRepliesDismissedPage
	cursorAt := "2026-11-05T09:00:00Z"
	cursorThread := "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	replyArgs := func(extra ...any) []any {
		return append([]any{"alice", "", nil}, extra...)
	}
	agentArgs := func(extra ...any) []any {
		return append([]any{"alice", "alice", nil}, extra...)
	}
	for _, query := range []struct {
		name  string
		sql   string
		args  []any
		index string
		// relation is the table the read may not scan sequentially, and noSortOn the column no
		// Sort node's key may name. An empty noSortOn holds the plan to no Sort node at all.
		relation string
		noSortOn string
	}{
		{"the unread count", myRepliesCountQuery, replyArgs(), "user_threads_unread", "user_threads", ""},
		{"every unread row", myRepliesPageQuery, replyArgs(everyRow), "user_threads_unread", "user_threads", ""},
		{"one issue's unread set", myRepliesPageQuery, append([]any{"alice", "TEST-1", nil}, &oneIssuesPage), "user_threads_unread", "user_threads", ""},
		{"the inbox count", myRepliesInboxCountQuery, replyArgs(), "user_threads_inbox", "user_threads", ""},
		{"every row under state=inbox", myRepliesInboxPageQuery, replyArgs(everyRow), "user_threads_inbox", "user_threads", ""},
		// The Dismissed view has no count: its total is the page it answers (listMyReplies).
		{"the first page under state=dismissed", myRepliesDismissedPageQuery, replyArgs(&dismissedPage), "user_threads_dismissed", "user_threads", ""},
		{
			"a page under state=dismissed from its cursor", myRepliesDismissedFromQuery,
			replyArgs(&dismissedPage, cursorAt, cursorThread), "user_threads_dismissed", "user_threads", "",
		},
		// The Inbox's own states read the person's dismissal marks as one set rather than asking
		// the table about one root at a time, so what answers them is the login index, not the
		// primary key. The key is what a dismiss and a restore write by, and what the trigger of
		// 20261105093507 deletes through: its message_id leads for that delete's sake, which is
		// the case below.
		{
			"the agent rows under state=inbox", myAgentInboxRowsQuery, agentArgs(everyRow),
			"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at",
		},
		{
			"the agent rows and total under state=inbox", myAgentCountedInboxRowsQuery, agentArgs(everyRow),
			"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at",
		},
		{
			"the agent rows under state=dismissed", myAgentDismissedRowsQuery, agentArgs(&dismissedPage),
			"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at",
		},
		{
			"an agent page under state=dismissed from its cursor", myAgentDismissedFromQuery,
			agentArgs(&dismissedPage, cursorAt, cursorThread),
			"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at",
		},
		// The trigger of 20261105093507 runs in the transaction of every session reply and
		// deletes by the root alone - the reply does not name whoever dismissed the thread - so
		// the key's leading column is the whole reason it is (message_id, login): under the other
		// order this delete scans a table holding a row per person per thread they have ever
		// dismissed. The statement is the trigger's own (20261105093507's up-file), which names
		// the root and nothing else, with the casts this file's other reads bind their text
		// through.
		{
			"the clear trigger's delete",
			`delete from user_agent_thread_dismissed
				where message_id = $1::text::uuid`,
			[]any{cursorThread},
			"user_agent_thread_dismissed_pkey", "user_agent_thread_dismissed", "",
		},
	} {
		t.Run(query.name, func(t *testing.T) {
			var planJSON []byte
			if err := tx.QueryRow(ctx, "explain (format json) "+query.sql, query.args...).Scan(&planJSON); err != nil {
				t.Fatalf("explain: %v", err)
			}
			plan := explaintest.OnePlan(t, planJSON)
			if explaintest.SeqScansRelation(t, plan, query.relation) {
				t.Fatalf("plan scans %s:\n%s", query.relation, planJSON)
			}
			if !planReadsIndex(t, plan, query.index) {
				t.Fatalf("plan does not read %s:\n%s", query.index, planJSON)
			}
			for _, key := range planSortKeys(t, plan) {
				if query.noSortOn == "" {
					t.Fatalf("plan sorts on %s, which its index already answers in:\n%s", key, planJSON)
				}
				if strings.Contains(key, query.noSortOn) {
					t.Fatalf("plan sorts on %s, which %s already answers in:\n%s", key, query.index, planJSON)
				}
			}
		})
	}
}

// planSortKeys is the Sort Key of every Sort node of planJSON, at any depth, as the plan writes it.
// A read whose order its index already answers in carries none.
func planSortKeys(t *testing.T, planJSON json.RawMessage) []string {
	t.Helper()
	var node struct {
		NodeType string            `json:"Node Type"`
		SortKey  []string          `json:"Sort Key"`
		Plans    []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	keys := []string{}
	if node.NodeType == "Sort" {
		keys = append(keys, strings.Join(node.SortKey, ", "))
	}
	for _, child := range node.Plans {
		keys = append(keys, planSortKeys(t, child)...)
	}
	return keys
}
