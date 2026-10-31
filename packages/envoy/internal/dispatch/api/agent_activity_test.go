package api

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// mainAgentLastActivityQuery is the statement main runs for the agents list's last activity,
// held here letter for letter as the oracle of the comparison below. It is a copy of the text
// this change replaced, not text built from the package's own constant: a statement built from
// that would move with it, and the two sides would agree however the read changed.
const mainAgentLastActivityQuery = `
		select actor->>'id', max(created_at)
		from events
		where actor->>'kind' = 'session'
			and actor->>'id' = any($1::text[])
		group by actor->>'id'
	`

// agentActivitySessions are the sessions the fixture below is read for: one with several
// events, one with a single event, one whose only events are not its own, and one the events
// table has never heard of. The last two are what the comparison rests on, because `cross join
// lateral` answers a session with no event no row, which is what the grouped read answered and
// what the caller's map needs.
var agentActivitySessions = []string{"planner", "reviewer", "quiet", "unknown"}

// newAgentActivityFixture writes events of a session actor, of a user actor and of a session
// nobody asked about, so that the statement has rows to leave out as well as rows to answer.
func newAgentActivityFixture(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank)
			values ('TEST-1', 'TEST', 1, 'Test issue', 'todo', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into events (issue_key, seq, type, actor, payload, notify, created_at) values
			('TEST-1', 1, 'message.created', '{"kind":"session","id":"planner"}',  '{}', false, '2026-09-13T12:00:00Z'),
			('TEST-1', 2, 'message.created', '{"kind":"session","id":"planner"}',  '{}', false, '2026-09-13T14:00:00Z'),
			('TEST-1', 3, 'message.created', '{"kind":"session","id":"planner"}',  '{}', false, '2026-09-13T13:00:00Z'),
			('TEST-1', 4, 'message.created', '{"kind":"session","id":"reviewer"}', '{}', false, '2026-09-13T11:00:00Z'),
			('TEST-1', 5, 'message.created', '{"kind":"user","id":"quiet"}',       '{}', false, '2026-09-13T15:00:00Z'),
			('TEST-1', 6, 'message.created', '{"kind":"session","id":"other"}',    '{}', false, '2026-09-13T16:00:00Z')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("write the agent activity fixture: %v", err)
		}
	}
	return database
}

// agentActivityRows is one string per row of statement, sorted. Neither statement orders its
// rows and the caller reads the answer as a map, so the comparison is of the set of rows.
func agentActivityRows(t *testing.T, database *store.Store, statement string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), statement, agentActivitySessions)
	if err != nil {
		t.Fatalf("run the agent activity statement: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read an agent activity row: %v", err)
		}
		got = append(got, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the agent activity rows: %v", err)
	}
	slices.Sort(got)
	return got
}

// The lateral read answers exactly what main's grouped read answered: the newest event of each
// session, no row for a session whose only events carry another actor kind, and no row for a
// session the events table has never heard of.
func TestTheAgentsLastActivityAnswersWhatMainsStatementDid(t *testing.T) {
	database := newAgentActivityFixture(t)
	want := agentActivityRows(t, database, mainAgentLastActivityQuery)
	if len(want) != 2 {
		t.Fatalf("main's last activity answered %v; want the two sessions with events of their own", want)
	}
	if got := agentActivityRows(t, database, agentLastActivityQuery); !slices.Equal(got, want) {
		t.Fatalf("the last activity answered\n%v\nwant main's rows\n%v", got, want)
	}
}

// The last activity reaches events only through events_session_actor_created_at_idx, the
// partial index on ((actor->>'id'), created_at desc) for a session actor, and reads it once
// for each session, stopping at that session's first row. Naming the index alone decides
// nothing here: main's grouped read uses the same index, with the same condition, and reads
// every event of every listed session through it. The lateral is what holds the read to one
// row a session, and the plan says so in the loops and the rows of the one node that reads
// events.
func TestTheAgentsLastActivityReadsEventsThroughTheSessionActorIndex(t *testing.T) {
	database := newAgentActivityFixture(t)
	plan := planWithoutSeqScans(t, database, agentLastActivityQuery, agentActivitySessions)
	if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
		t.Errorf("the last activity reads %v, which no index serves:\n%s", unkeyed, plan)
	}
	const index = "events_session_actor_created_at_idx"
	firstKey := explaintest.NewIndexKeys(database.Pool).FirstKey(t, index)
	searched := false
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Index != index {
			continue
		}
		searched = true
		if !explaintest.CondNamesFirstKey(scan.Cond, firstKey) {
			t.Errorf("the last activity reads %s on `%s`, which names nothing of that index's first key column `%s`:\n%s",
				scan, scan.Cond, firstKey, plan)
		}
	}
	if !searched {
		t.Errorf("the last activity reads no event through %s:\n%s", index, plan)
	}

	// `Actual Rows` is the average over the loops, so a read that stops at one row answers at
	// most one however many of the sessions have an event; main's one grouped read answers
	// every event it read.
	root, raw := analyzePlan(t, database, agentLastActivityQuery, agentActivitySessions)
	read := 0
	for _, node := range root.everyNode() {
		if node.IndexName != index {
			continue
		}
		read++
		if node.Loops != float64(len(agentActivitySessions)) || node.Rows > 1 {
			t.Errorf("the last activity reads events %v times for %d sessions, %v rows each; want one read a session of one row:\n%s",
				node.Loops, len(agentActivitySessions), node.Rows, raw)
		}
	}
	if read != 1 {
		t.Errorf("the last activity reads events through %s %d times; want the lateral's one read:\n%s", index, read, raw)
	}
}
