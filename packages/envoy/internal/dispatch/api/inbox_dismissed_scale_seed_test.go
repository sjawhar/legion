package api

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// How a14's three shapes are built: the seeding statements, the counts every shape is held to
// before anything is timed, and the rows the measurement names - the 120 a dismissal takes, the
// unread ones a Mark all read marks, and the cursor a page 50,000 deep resumes from. The counts,
// the ages and the bodies these write are api/inbox_dismissed_scale_test.go's, with the
// measurement itself; the plans are api/inbox_dismissed_scale_plan_test.go's.
//
// It is a file of its own because none of it is the measurement: a reader checking what a14
// measures and what it holds each number to reads the one file, and a reader asking what was in
// the database when it did reads this one. Both run only under LEGION_SCALE_TEST, since nothing
// here is called unless TestDismissedScaleHoldsTheInboxsReads runs.

// seedDismissedScale builds one shape: a server on a database of its own, the pile it was asked
// for, the 600 undismissed threads with 300 unread among them, and the 1,000 unread threads the
// mark-all works over. It asserts every count before the caller times anything, because a
// measurement over a seed that did not land is worse than none.
func seedDismissedScale(t *testing.T, name string, replies, agents int) *dismissedScaleShape {
	t.Helper()
	started := time.Now()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "SCALE", "A pile of dismissed threads", "A spec")
	shape := &dismissedScaleShape{
		name: name, handler: handler, database: database, issue: issue.Key,
		dismissedReplies: replies, dismissedAgents: agents,
	}
	ctx := context.Background()

	// The pile first, so the statement that writes the dismissal marks can take every row of the
	// caller's: at this point they are the pile and nothing else.
	if replies > 0 {
		dismissedScaleCommentPairs(t, shape, dismissedScalePileLabel, replies, dismissedScalePileAge)
		markDismissedScaleThreads(t, shape, replies)
	}
	if agents > 0 {
		dismissedScaleMessagePairs(t, shape, dismissedScaleAgentPileLabel, agents, dismissedScaleAgentAge)
		markDismissedScaleAgentThreads(t, shape, agents)
	}

	// The Inbox: 540 Reply threads, 270 of them read, and 60 Agent threads, 30 of them read.
	dismissedScaleCommentPairs(t, shape, dismissedScaleInboxLabel, dismissedScaleInboxReplies, dismissedScaleInboxAge)
	readDismissedScaleThreads(t, shape, dismissedScaleInboxLabel, dismissedScaleReadReplies)
	dismissedScaleMessagePairs(t, shape, dismissedScaleAgentInboxLabel, dismissedScaleInboxAgents, dismissedScaleInboxAge)
	readDismissedScaleAgentThreads(t, shape, dismissedScaleAgentInboxLabel, dismissedScaleReadAgents)
	// The threads Mark all read works over.
	dismissedScaleCommentPairs(t, shape, dismissedScaleMarkLabel, dismissedScaleMarkAll, dismissedScaleMarkAge)

	// Statistics, as a production table has them: without them the planner reads this seed as the
	// 24-row fixture every other test in this package leaves behind.
	if _, err := database.Pool.Exec(ctx, "vacuum analyze"); err != nil {
		t.Fatalf("analyze %s: %v", name, err)
	}

	assertDismissedScaleSeed(t, shape)
	shape.dismissRows = dismissedScaleDismissTargets(t, shape)
	shape.unreadThreads = dismissedScaleUnreadThreads(t, shape)
	shape.unreadReplies = dismissedScaleUnreadAgentReplies(t, shape)
	if replies+agents > dismissedScaleCursorDepth {
		shape.deepCursor = dismissedScaleDeepCursor(t, shape)
	}
	t.Logf("seeded %s in %s: %d dismissed Reply threads, %d dismissed Agent threads, %d in the Inbox (%d unread)",
		name, time.Since(started).Round(time.Millisecond), replies, agents,
		dismissedScaleInboxRows(), dismissedScaleUnreadRows())
	return shape
}

// dismissedScaleCommentPairs writes count comment threads: a root by the caller and a reply by the
// session under it, so the triggers of migrations 0095 to 0097 make the user_threads row and its
// generated unread column, which is how e2e/inbox-threads.ts seeds the same rows in bulk. Thread g
// is olderThan + 2g minutes old. It is written in batches so one statement never holds the whole
// pile.
func dismissedScaleCommentPairs(t *testing.T, shape *dismissedScaleShape, label string, count, olderThan int) {
	t.Helper()
	ctx := context.Background()
	const batch = 10_000
	for first := 1; first <= count; first += batch {
		last := min(first+batch-1, count)
		tag, err := shape.database.Pool.Exec(ctx, `
			with roots as (
				insert into comments (issue_key, author, body, created_at)
				select $1, jsonb_build_object('kind', 'user', 'id', $2::text),
				       $3::text || ' root ' || g,
				       now() - (($6::int + 2 * g + 1) || ' minutes')::interval
				from generate_series($4::int, $5::int) g
				returning id, body, created_at
			)
			insert into comments (issue_key, author, body, reply_to, created_at)
			select $1, jsonb_build_object('kind', 'session', 'id', $7::text),
			       replace(roots.body, ' root ', ' reply '), roots.id,
			       roots.created_at + interval '1 minute'
			from roots
		`, shape.issue, dismissedScaleLogin, label, first, last, olderThan, dismissedScaleSession)
		if err != nil {
			t.Fatalf("seed %s threads %d-%d: %v", label, first, last, err)
		}
		if tag.RowsAffected() != int64(last-first+1) {
			t.Fatalf("seeded %d %s replies, want %d", tag.RowsAffected(), label, last-first+1)
		}
	}
}

// dismissedScaleMessagePairs writes count Agent threads: a one-off message to the session and the
// session's reply under it, which is every row an Agent row is read from (no delivery row, since
// the Inbox leaves out only a root an attempt was accepted on).
func dismissedScaleMessagePairs(t *testing.T, shape *dismissedScaleShape, label string, count, olderThan int) {
	t.Helper()
	ctx := context.Background()
	tag, err := shape.database.Pool.Exec(ctx, `
		with roots as (
			insert into messages (issue_key, author, body, target, created_at)
			select null, jsonb_build_object('kind', 'user', 'id', $1::text),
			       $2::text || ' ask ' || g, 'session:' || $3::text,
			       now() - (($5::int + 2 * g + 1) || ' minutes')::interval
			from generate_series(1, $4::int) g
			returning id, body, created_at
		)
		insert into messages (issue_key, author, body, in_reply_to, target, created_at)
		select null, jsonb_build_object('kind', 'session', 'id', $3::text),
		       replace(roots.body, ' ask ', ' answer '), roots.id, 'session:' || $3::text,
		       roots.created_at + interval '1 minute'
		from roots
	`, dismissedScaleLogin, label, dismissedScaleSession, count, olderThan)
	if err != nil {
		t.Fatalf("seed %s threads: %v", label, err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("seeded %d %s replies, want %d", tag.RowsAffected(), label, count)
	}
}

// markDismissedScaleThreads writes the dismissal the route writes on every Reply row the caller
// has: the mark, and the read mark moved to it, which is what dismissInboxReplyThreads does, so
// none of the pile counts as unread. The marks are one a minute, newest reply dismissed most
// recently, so the Dismissed view's order is the index's own and a page 50,000 deep is a real
// cursor rather than one timestamp's worth of ties.
func markDismissedScaleThreads(t *testing.T, shape *dismissedScaleShape, count int) {
	t.Helper()
	tag, err := shape.database.Pool.Exec(context.Background(), `
		with ordered as (
			select thread_id, row_number() over (order by replied_at desc, thread_id desc) as n
			from user_threads
			where login = $1 and thread_id in (
				select c.id from comments c where c.issue_key = $2 and c.body like $3
			)
		)
		update user_threads u
		set dismissed_at = now() - (ordered.n || ' minutes')::interval,
		    read_through = greatest(u.read_through, now() - (ordered.n || ' minutes')::interval)
		from ordered
		where u.login = $1 and u.thread_id = ordered.thread_id
	`, dismissedScaleLogin, shape.issue, dismissedScalePileLabel+" root %")
	if err != nil {
		t.Fatalf("dismiss the pile: %v", err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("dismissed %d threads, want the pile's %d", tag.RowsAffected(), count)
	}
}

// markDismissedScaleAgentThreads writes the Agent half's dismissal: one user_agent_thread_dismissed
// row per root, at or after the newest reply the session wrote there, and the replies marked read
// by id, which is what a dismissal writes so a dismissed thread counts as unread nowhere. The marks
// are one every ten minutes, so they interleave with the Reply half's and a Dismissed page holds
// both kinds.
func markDismissedScaleAgentThreads(t *testing.T, shape *dismissedScaleShape, count int) {
	t.Helper()
	ctx := context.Background()
	tag, err := shape.database.Pool.Exec(ctx, `
		insert into user_agent_thread_dismissed (login, message_id, dismissed_at)
		select $1, ordered.id,
		       now() - (($3::int * ordered.n) || ' minutes')::interval
		from (
			select root.id, row_number() over (order by root.created_at desc, root.id desc) as n
			from messages root
			where root.in_reply_to is null and root.target = 'session:' || $2
			  and root.body like $4
		) ordered
	`, dismissedScaleLogin, dismissedScaleSession, dismissedScaleAgentStep, dismissedScaleAgentPileLabel+" ask %")
	if err != nil {
		t.Fatalf("dismiss the agent pile: %v", err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("dismissed %d agent threads, want %d", tag.RowsAffected(), count)
	}
	readDismissedScaleAgentThreads(t, shape, dismissedScaleAgentPileLabel, count)
}

// readDismissedScaleThreads marks the oldest count threads of one label read, as a person who has
// read a thread and left it in their Inbox has them: read_through at the newest reply, so the row
// is no longer unread and still in the Inbox.
func readDismissedScaleThreads(t *testing.T, shape *dismissedScaleShape, label string, count int) {
	t.Helper()
	tag, err := shape.database.Pool.Exec(context.Background(), `
		update user_threads set read_through = replied_at
		where login = $1 and thread_id in (
			select c.id from comments c
			where c.issue_key = $2 and c.body like $3
			order by c.created_at
			limit $4::int
		)
	`, dismissedScaleLogin, shape.issue, label+" root %", count)
	if err != nil {
		t.Fatalf("read %d %s threads: %v", count, label, err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("read %d %s threads, want %d", tag.RowsAffected(), label, count)
	}
}

// readDismissedScaleAgentThreads marks the oldest count Agent threads of one label read the way
// the Inbox marks one: the session's replies read by id, which is what leaves the root out of the
// unread set without moving any session's own read mark.
func readDismissedScaleAgentThreads(t *testing.T, shape *dismissedScaleShape, label string, count int) {
	t.Helper()
	tag, err := shape.database.Pool.Exec(context.Background(), `
		insert into user_agent_reply_read (login, session_id, reply_id)
		select $1, $2, reply.id
		from messages reply
		join messages root on root.id = reply.in_reply_to
		where root.body like $3
		order by root.created_at
		limit $4::int
		on conflict do nothing
	`, dismissedScaleLogin, dismissedScaleSession, label+" ask %", count)
	if err != nil {
		t.Fatalf("read %d %s agent threads: %v", count, label, err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("read %d %s agent threads, want %d", tag.RowsAffected(), label, count)
	}
}

// assertDismissedScaleSeed holds the seed to its counts, in the tables the triggers keep and
// through the routes themselves, before anything is timed.
func assertDismissedScaleSeed(t *testing.T, shape *dismissedScaleShape) {
	t.Helper()
	for _, counted := range []struct {
		what      string
		statement string
		want      int64
	}{
		{"dismissed Reply threads", `select count(*) from user_threads where login = $1 and dismissed_at is not null`,
			int64(shape.dismissedReplies)},
		{"dismissed Agent threads", `select count(*) from user_agent_thread_dismissed where login = $1`,
			int64(shape.dismissedAgents)},
		{"undismissed Reply threads", `select count(*) from user_threads where login = $1 and replied_at is not null and dismissed_at is null`,
			int64(dismissedScaleInboxReplies + dismissedScaleMarkAll)},
		{"unread Reply threads", `select count(*) from user_threads where login = $1 and unread`,
			int64(dismissedScaleInboxReplies - dismissedScaleReadReplies + dismissedScaleMarkAll)},
	} {
		var got int64
		if err := shape.database.Pool.QueryRow(context.Background(), counted.statement, dismissedScaleLogin).Scan(&got); err != nil {
			t.Fatalf("count %s on %s: %v", counted.what, shape.name, err)
		}
		if got != counted.want {
			t.Fatalf("%s on %s: %d rows, want %d", counted.what, shape.name, got, counted.want)
		}
	}
	// The routes' own reading of the same seed, both halves together: what each state answers is
	// what the measurement then times.
	for _, state := range []struct {
		query string
		total int
	}{
		{"?kind=reply,agent&state=inbox", dismissedScaleInboxRows()},
		{"?kind=reply,agent&state=unread&limit=1", dismissedScaleUnreadRows()},
		// The Dismissed view answers no count of the pile: its total is the page it holds, and the
		// pile itself is asserted above, in the tables the marks live in.
		{"?kind=reply,agent&state=dismissed",
			min(shape.dismissedReplies+shape.dismissedAgents, myRepliesDismissedPage)},
	} {
		page := getMyReplies(t, shape.handler, dismissedScaleLogin, state.query)
		if page.Total != state.total {
			t.Fatalf("%s on %s answered total=%d, want %d", state.query, shape.name, page.Total, state.total)
		}
	}
}

// dismissedScaleDismissTargets is the 120 rows one dismissal names: threads the seed marked read
// and left in the Inbox, so dismissing and restoring them 20 times moves no read mark and leaves
// every other operation's answer the same.
func dismissedScaleDismissTargets(t *testing.T, shape *dismissedScaleShape) []string {
	t.Helper()
	threads := dismissedScaleStrings(t, shape, `
		select thread_id::text from user_threads
		where login = $1 and dismissed_at is null and not unread
		order by thread_id
		limit `+fmt.Sprint(dismissedScaleDismissRows), dismissedScaleLogin)
	if len(threads) != dismissedScaleDismissRows {
		t.Fatalf("found %d read threads to dismiss on %s, want %d", len(threads), shape.name, dismissedScaleDismissRows)
	}
	return threads
}

// dismissedScaleUnreadThreads and dismissedScaleUnreadAgentReplies are what one Mark all read
// marks, which the run after it puts back.
func dismissedScaleUnreadThreads(t *testing.T, shape *dismissedScaleShape) []string {
	t.Helper()
	threads := dismissedScaleStrings(t, shape, `
		select thread_id::text from user_threads where login = $1 and unread order by thread_id
	`, dismissedScaleLogin)
	want := dismissedScaleInboxReplies - dismissedScaleReadReplies + dismissedScaleMarkAll
	if len(threads) != want {
		t.Fatalf("found %d unread threads on %s, want %d", len(threads), shape.name, want)
	}
	return threads
}

func dismissedScaleUnreadAgentReplies(t *testing.T, shape *dismissedScaleShape) []string {
	t.Helper()
	replies := dismissedScaleStrings(t, shape, `
		select reply.id::text
		from messages reply
		join messages root on root.id = reply.in_reply_to
		where root.body like $2
		  and not exists (
			select 1 from user_agent_reply_read r where r.login = $1 and r.reply_id = reply.id
		  )
		order by reply.id
	`, dismissedScaleLogin, dismissedScaleAgentInboxLabel+" ask %")
	want := dismissedScaleInboxAgents - dismissedScaleReadAgents
	if len(replies) != want {
		t.Fatalf("found %d unread agent replies on %s, want %d", len(replies), shape.name, want)
	}
	return replies
}

// dismissedScaleDeepCursor is the cursor a page 50,000 rows deep resumes from: the row at that
// depth of the merged order both halves answer in, read the way the route merges them - newest
// dismissal first, ties by id descending.
func dismissedScaleDeepCursor(t *testing.T, shape *dismissedScaleShape) string {
	t.Helper()
	var at time.Time
	var id string
	if err := shape.database.Pool.QueryRow(context.Background(), `
		select at, id from (
			select dismissed_at as at, thread_id::text as id from user_threads
			where login = $1 and dismissed_at is not null
			union all
			select dismissed_at, message_id::text from user_agent_thread_dismissed where login = $1
		) dismissed
		order by at desc, id desc
		offset $2::int limit 1
	`, dismissedScaleLogin, dismissedScaleCursorDepth-1).Scan(&at, &id); err != nil {
		t.Fatalf("read the cursor %d deep: %v", dismissedScaleCursorDepth, err)
	}
	return at.UTC().Format(time.RFC3339Nano) + "," + id
}

func dismissedScaleStrings(t *testing.T, shape *dismissedScaleShape, statement string, args ...any) []string {
	t.Helper()
	rows, err := shape.database.Pool.Query(context.Background(), statement, args...)
	if err != nil {
		t.Fatalf("read %s: %v", statement, err)
	}
	defer rows.Close()
	read := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan a row of %s: %v", statement, err)
		}
		read = append(read, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", statement, err)
	}
	return read
}
