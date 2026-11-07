package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/explaintest"
)

// What a14 holds the plans to: every statement its six operations run, the index each must read,
// the table each may not scan and the sort no plan may make, the EXPLAIN runs themselves, and the
// negative control - the shape this change replaced, run at the pile's own size and held to being
// far outside the bound. The measurement and its time bounds are
// api/inbox_dismissed_scale_test.go's, and the seed they run over
// api/inbox_dismissed_scale_seed_test.go's.
//
// It is a file of its own because a plan is held to different things than a time is: a number out
// of its bound is read here, where the statement, its index and its plan are, and the file that
// takes the readings stays short enough to read in one sitting.

// dismissedScaleExplained is one statement an operation runs, with what its plan is held to: the
// index it must read, the table it may not scan, and the column no Sort key may name. An empty
// noSortOn holds the plan to no Sort node at all, which is the rule replies_plan_test.go holds
// every Reply-half read to; an Agent-half read carries sorts of the list's own that are older
// than the dismissal - the roots' newest reply, the lateral that reads it, the ordered array_agg -
// so what it is held to is that no sort stands in for the dismissal, named by its column. The two
// write routes read their rows and then sort them by thread id to take their locks in one order
// (markInboxReadThreads), which is the route's own sort and not the index's, so each is held the
// same way, by the column its state is ordered by.
type dismissedScaleExplained struct {
	operation string
	half      string
	sql       string
	args      []any
	index     string
	// relation is the table the plan may not scan, empty for a read whose cheapest way to take a
	// whole set at this seed is a scan of it, and onceOver a table the plan must read once rather
	// than once per row: the Inbox's own three reads take the person's dismissal marks as one set,
	// and whether the planner takes that set off the index or off the table is its own business,
	// while taking it one root at a time is what made those reads grow with the pile.
	relation string
	noSortOn string
	onceOver string
}

// explainDismissedScaleReads explains every statement the six operations run, under
// EXPLAIN (ANALYZE, BUFFERS) with enable_seqscan and enable_bitmapscan left on, so the plans are
// what the planner really does at this size rather than what a pin makes it do. On the loaded
// shape each plan is held to its index and its sort rule; on the baselines they are printed and
// held to nothing, since a scan of an empty table is the right plan for an empty table.
func explainDismissedScaleReads(t *testing.T, shape *dismissedScaleShape, hold bool) {
	t.Helper()
	ctx := context.Background()
	connection, err := shape.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	for _, explained := range dismissedScaleStatements(shape) {
		name := explained.operation + " / " + explained.half + " @ " + shape.name
		plan, text := explainDismissedScale(t, connection.Conn(), name, explained.sql, explained.args)
		t.Logf("%s:\n%s", name, text)
		if !hold {
			continue
		}
		if explained.relation != "" && explaintest.SeqScansRelation(t, plan, explained.relation) {
			t.Errorf("%s scans %s:\n%s", name, explained.relation, text)
		}
		if !planReadsIndex(t, plan, explained.index) {
			t.Errorf("%s does not read %s:\n%s", name, explained.index, text)
		}
		if explained.onceOver != "" {
			if loops := planLoopsOver(t, plan, explained.onceOver); loops > 1 {
				t.Errorf("%s reads %s %.0f times over, want once: the Inbox's own reads take the person's marks as one set rather than one read a root, which is what stops them growing with the pile:\n%s",
					name, explained.onceOver, loops, text)
			}
		}
		for _, key := range planSortKeys(t, plan) {
			if explained.noSortOn == "" {
				t.Errorf("%s sorts on %s, which its index already answers in:\n%s", name, key, text)
				continue
			}
			if strings.Contains(key, explained.noSortOn) {
				t.Errorf("%s sorts on %s, which %s already answers in:\n%s", name, key, explained.index, text)
			}
		}
	}
}

// planLoopsOver is the greatest number of times any one node of an analysed plan read relation:
// one for a table the read visits once, and a row count for one it visits per row.
func planLoopsOver(t *testing.T, planJSON json.RawMessage, relation string) float64 {
	t.Helper()
	var node struct {
		Relation string            `json:"Relation Name"`
		Loops    float64           `json:"Actual Loops"`
		Plans    []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(planJSON, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	most := float64(0)
	if node.Relation == relation {
		most = node.Loops
	}
	for _, child := range node.Plans {
		if loops := planLoopsOver(t, child, relation); loops > most {
			most = loops
		}
	}
	return most
}

// explainDismissedScale runs one statement under EXPLAIN (ANALYZE, BUFFERS) twice: once as JSON,
// which the checks read, and once as text, which the report prints. Both run inside a transaction
// that is rolled back, so a write route's read takes no lock past the measurement.
func explainDismissedScale(
	t *testing.T, connection *pgx.Conn, name, statement string, args []any,
) (json.RawMessage, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the explain of %s: %v", name, err)
	}
	defer tx.Rollback(ctx)
	var planJSON []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+statement, args...).Scan(&planJSON); err != nil {
		t.Fatalf("explain %s: %v", name, err)
	}
	rows, err := tx.Query(ctx, "explain (analyze, buffers) "+statement, args...)
	if err != nil {
		t.Fatalf("explain %s as text: %v", name, err)
	}
	defer rows.Close()
	lines := []string{}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan a plan line of %s: %v", name, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain %s as text: %v", name, err)
	}
	return explaintest.OnePlan(t, planJSON), strings.Join(lines, "\n")
}

// dismissedScaleStatements is every statement the six operations run, with the index each is held
// to: the list route's two halves per state (api/replies.go and api/replies_agent.go), the
// dismissal's read of the rows it was given (api/inbox_dismiss.go) and the mark-all's two reads
// (api/inbox_read.go). The indexes are the ones replies_plan_test.go pins, which is where the
// states' own reads are held to them.
func dismissedScaleStatements(shape *dismissedScaleShape) []dismissedScaleExplained {
	var everyRow *int
	page := myRepliesDismissedPage
	one := 1
	replyArgs := func(extra ...any) []any {
		return append([]any{dismissedScaleLogin, "", nil}, extra...)
	}
	agentArgs := func(extra ...any) []any {
		return append([]any{dismissedScaleLogin, dismissedScaleLogin, nil}, extra...)
	}
	cursorAt, cursorThread, _ := strings.Cut(shape.deepCursor, ",")
	mark := time.Now().UTC().Format(time.RFC3339Nano)
	statements := []dismissedScaleExplained{
		{"state=inbox", "reply", myRepliesInboxPageQuery, replyArgs(everyRow), "user_threads_inbox", "user_threads", "", ""},
		// The Inbox's own three reads take the person's dismissal marks as one set, so each is
		// held to reading that table once rather than to an access path of its own.
		{"state=inbox", "agent", myAgentInboxRowsQuery, agentArgs(everyRow),
			"messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
		{"unread count", "reply count", myRepliesCountQuery, replyArgs(), "user_threads_unread", "user_threads", "", ""},
		{"unread count", "reply page", myRepliesPageQuery, replyArgs(&one), "user_threads_unread", "user_threads", "", ""},
		{"unread count", "agent page and total", myAgentCountedRowsQuery, agentArgs(&one),
			"messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
		{"dismissed page 1", "reply page", myRepliesDismissedPageQuery, replyArgs(&page),
			"user_threads_dismissed", "user_threads", "", ""},
		{"dismissed page 1", "agent page", myAgentDismissedRowsQuery, agentArgs(&page),
			"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at", ""},
		// The two write routes sort the rows they matched by thread id, which is the lock order
		// markInboxReadThreads writes down and not an order any index owes them, so what each is
		// held to is that nothing sorts by the state's own order. The dismissal's read of the
		// rows a request named binds the ids alone and answers each row's own version: what the
		// page drew in them is the request's, written over that answer in the server rather
		// than sent to this read (readInboxDismissThreads).
		{"120-thread dismiss", "reply rows", inboxDismissNamedThreads,
			[]any{dismissedScaleLogin, shape.dismissRows},
			"user_threads_pkey", "user_threads", "replied_at", ""},
		{"mark all read", "reply rows", inboxReadReplyThreads,
			[]any{dismissedScaleLogin, "", nil, mark, nil, true, nil}, "user_threads_unread", "user_threads", "replied_at", ""},
		{"mark all read", "agent rows", inboxReadAgentThreads,
			append(agentArgs(), mark), "messages_direct_roots", "", "dismissed_at", "user_agent_thread_dismissed"},
	}
	if shape.deepCursor != "" {
		statements = append(statements,
			dismissedScaleExplained{"dismissed page 50,000 deep", "reply page", myRepliesDismissedFromQuery,
				replyArgs(&page, cursorAt, cursorThread), "user_threads_dismissed", "user_threads", "", ""},
			dismissedScaleExplained{"dismissed page 50,000 deep", "agent page", myAgentDismissedFromQuery,
				agentArgs(&page, cursorAt, cursorThread),
				"user_agent_thread_dismissed_login_dismissed_at", "user_agent_thread_dismissed", "dismissed_at", ""},
		)
	}
	return statements
}

// dismissedScaleControlAgentCount is the shape this change replaced, kept here and nowhere else:
// the Dismissed view's Agent count read over one recursive CTE of every one-off message the person
// ever sent, with one lateral per dismissed root scanning that whole CTE again. It is a14's
// negative control - put the per-root Agent count back and the Dismissed pages go far above their
// bound - so it is spelled out rather than built from the live constants, which no longer hold it.
const dismissedScaleControlAgentCount = `
	with recursive` + unreadDirectRepliesCTE + `,
	one_off_replies as (
		select dt.root_id, dt.session_id, dt.id as reply_id, dt.created_at, dt.author
		from direct_thread dt
		where dt.author->>'kind' = 'session'
		  and not exists (
			select 1 from message_deliveries d
			where d.message_id = dt.root_id and d.accepted_as = 'user_turn'
		  )
	),
	dismissed_roots as (
		select d.message_id as root_id, d.dismissed_at
		from user_agent_thread_dismissed d
		where d.login = $1
		order by d.dismissed_at desc, d.message_id desc
	),
	roots as (
		select dr.root_id, newest.session_id, newest.replied_at, dr.dismissed_at
		from dismissed_roots dr
		join lateral (
			select o.session_id, max(o.created_at) as replied_at
			from one_off_replies o
			where o.root_id = dr.root_id
			group by o.session_id
		) newest on true
		where dr.dismissed_at >= newest.replied_at
	)
	select count(*) from roots`

// dismissedScaleNegativeControl runs that control at the pile's own size and holds it to being
// red: a read the Dismissed page would have to make before it could answer, far above the 250 ms
// every one of those pages is held to. A control that came in under the bound would mean this
// measurement cannot tell the two shapes apart, and so proves nothing about the one it passed.
func dismissedScaleNegativeControl(t *testing.T, shape *dismissedScaleShape) {
	t.Helper()
	ctx := context.Background()
	var planJSON []byte
	if err := shape.database.Pool.QueryRow(ctx,
		"explain (analyze, buffers, format json) "+dismissedScaleControlAgentCount,
		dismissedScaleLogin, dismissedScaleLogin, nil).Scan(&planJSON); err != nil {
		t.Fatalf("explain the negative control: %v", err)
	}
	var answered []struct {
		Execution float64         `json:"Execution Time"`
		Plan      json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(planJSON, &answered); err != nil || len(answered) != 1 {
		t.Fatalf("decode the control's plan: %v\n%s", err, planJSON)
	}
	took := time.Duration(answered[0].Execution * float64(time.Millisecond))
	t.Logf("negative control (the per-root Agent count over the recursive thread query) @ %s: %.1f ms\n%s",
		shape.name, msOf(took), planJSON)
	if took <= dismissedScaleTimeBound {
		t.Errorf("the control answered in %.1f ms, inside the %s bound: the measurement cannot tell the shape it replaced from the one it holds, so the bounds above prove nothing",
			msOf(took), dismissedScaleTimeBound)
	}
}
