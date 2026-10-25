package store

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// recordingQueryer is the pool, keeping the last statement BacklinkCounts ran so the test can
// EXPLAIN exactly what production executes without refs exporting its query builder.
type recordingQueryer struct {
	*Pool
	sql  string
	args []any
}

func (r *recordingQueryer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sql, r.args = sql, args
	return r.Pool.Query(ctx, sql, args...)
}

// seedEveryInboundArm fills one row into every arm of graph_edges, and a second row into each
// arm whose to side can hold more than one edge, so a count that drops or doubles an arm
// differs from the view's.
func seedEveryInboundArm(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'Parent', '{"kind":"user","id":"alice"}', 'A');
		insert into issues (key, project_key, number, title, created_by, rank, parent_key) values
			('CORE-2', 'CORE', 2, 'Child one', '{"kind":"user","id":"alice"}', 'B', 'CORE-1'),
			('CORE-3', 'CORE', 3, 'Child two', '{"kind":"user","id":"alice"}', 'C', 'CORE-1');
		insert into issue_links (issue_key, kind, target_key) values
			('CORE-2', 'blocked_by', 'CORE-1'),
			('CORE-3', 'blocked_by', 'CORE-1');
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) values
			('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}'),
			('CORE-1', 'CORE', 'notes', 'notes.md', 'doc', false, '{"kind":"user","id":"alice"}');
		insert into artifacts (project_key, slug, name, kind, created_by) values
			('CORE', 'handbook', 'Handbook', 'doc', '{"kind":"user","id":"alice"}');

		insert into asks (issue_key, author, question, block_id, block_artifact_id)
			select 'CORE-1', '{"kind":"user","id":"alice"}', 'block?', 'b1', id
			from artifacts where ref_key = 'CORE-1/spec';
		insert into asks (issue_key, author, question, anchor)
			select 'CORE-1', '{"kind":"user","id":"alice"}', 'anchored?',
			       jsonb_build_object('artifact_id', id::text, 'mark_id', 'm1')
			from artifacts where ref_key = 'CORE-1/spec';
		insert into asks (artifact_id, author, question)
			select id, '{"kind":"user","id":"alice"}', 'handbook one?'
			from artifacts where ref_key = 'CORE/handbook';
		insert into asks (artifact_id, author, question)
			select id, '{"kind":"user","id":"alice"}', 'handbook two?'
			from artifacts where ref_key = 'CORE/handbook';
		insert into asks (issue_key, author, question) values
			('CORE-1', '{"kind":"user","id":"alice"}', 'plain one?'),
			('CORE-1', '{"kind":"user","id":"alice"}', 'plain two?');

		insert into comments (issue_key, author, body, anchor)
			select 'CORE-1', '{"kind":"user","id":"alice"}', 'anchored',
			       jsonb_build_object('artifact_id', id::text, 'mark_id', 'm2')
			from artifacts where ref_key = 'CORE-1/spec';
		insert into comments (artifact_id, author, body)
			select id, '{"kind":"user","id":"alice"}', 'on the handbook'
			from artifacts where ref_key = 'CORE/handbook';
		insert into comments (issue_key, author, body, reply_to)
			select 'CORE-1', '{"kind":"user","id":"alice"}', 'reply ' || n, c.id
			from comments c, generate_series(1, 2) as n
			where c.anchor->>'mark_id' = 'm2';
		insert into comments (issue_key, author, body, ask_id)
			select 'CORE-1', '{"kind":"user","id":"alice"}', 'ask reply ' || n, k.id
			from asks k, generate_series(1, 2) as n
			where k.question = 'plain one?';

		insert into messages (issue_key, author, body) values
			('CORE-1', '{"kind":"session","id":"s1"}', 'hello');
		insert into messages (issue_key, author, body, in_reply_to)
			select 'CORE-1', '{"kind":"user","id":"alice"}', 'hi ' || n, m.id
			from messages m, generate_series(1, 2) as n
			where m.body = 'hello';

		insert into ask_followers (ask_id, session_id)
			select id, 's1' from asks where question in ('plain one?', 'plain two?');
		insert into ask_followers (ask_id, session_id)
			select id, 's2' from asks where question = 'plain one?';

		insert into architecture_snapshots (project_key, commit, files)
			values ('CORE', 'abc123', '{}'::jsonb);
		insert into components (project_key, id, title, prose, parent, snapshot_id)
			select 'CORE', c.id, c.title, '', c.parent, s.id
			from architecture_snapshots s,
			     (values ('web', 'Web', null), ('store', 'Store', null),
			             ('api', 'API', 'web'), ('ui', 'UI', 'web')) as c(id, title, parent)
			where s.commit = 'abc123';
		insert into component_depends (project_key, from_id, to_id) values
			('CORE', 'api', 'store'),
			('CORE', 'ui', 'store');
		insert into issue_components (issue_key, mode) values
			('CORE-2', 'explicit'),
			('CORE-3', 'explicit');
		insert into issue_component_members (issue_key, project_key, component_id) values
			('CORE-2', 'CORE', 'store'),
			('CORE-3', 'CORE', 'store');

		insert into refs (from_kind, from_id, to_kind, to_id, source_seq)
			select 'message', m.id::text, t.to_kind, t.to_id, 42
			from messages m,
			     (values ('issue', 'CORE-1'), ('issue', 'CORE-2'), ('artifact', 'CORE-1/spec'),
			             ('component', 'CORE/store'), ('session', 's1')) as t(to_kind, to_id)
			where m.body = 'hello';
		insert into refs (from_kind, from_id, to_kind, to_id, source_seq)
			select 'message', m.id::text, 'ask', k.id::text, 43
			from messages m, asks k
			where m.body = 'hello' and k.question = 'plain one?';
		insert into refs (from_kind, from_id, to_kind, to_id, source_seq)
			select 'message', m.id::text, 'comment', c.id::text, 44
			from messages m, comments c
			where m.body = 'hello' and c.anchor->>'mark_id' = 'm2';
		insert into refs (from_kind, from_id, to_kind, to_id, source_seq)
			select 'issue', 'CORE-2', 'message', m.id::text, 45
			from messages m where m.body = 'hello';
	`); err != nil {
		t.Fatalf("seed every inbound arm: %v", err)
	}
}

// inboundTargets is every node of a kind the view holds an inbound edge for, so the comparison
// below covers each arm the fixture filled without the test naming an id.
func inboundTargets(t *testing.T, ctx context.Context, store *Store, kind string) []string {
	t.Helper()
	rows, err := store.Pool.Query(ctx,
		`select distinct to_id from graph_edges where to_kind = $1 order by to_id`, kind)
	if err != nil {
		t.Fatalf("read %s targets: %v", kind, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %s target: %v", kind, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s targets: %v", kind, err)
	}
	return ids
}

// viewCounts is what BacklinkCounts counted before it read the arms: the grouped count over
// graph_edges itself, which is the answer this change may not move.
func viewCounts(t *testing.T, ctx context.Context, store *Store, kind string, ids, excludeKinds []string) map[string]int {
	t.Helper()
	sql := `select to_id, count(*)::int from graph_edges where to_kind = $1 and to_id = any($2)`
	args := []any{kind, ids}
	if len(excludeKinds) > 0 {
		args = append(args, excludeKinds)
		sql += ` and not (kind = any($3))`
	}
	rows, err := store.Pool.Query(ctx, sql+" group by to_id", args...)
	if err != nil {
		t.Fatalf("count %s backlinks through the view: %v", kind, err)
	}
	defer rows.Close()
	counts := make(map[string]int, len(ids))
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			t.Fatalf("scan %s view count: %v", kind, err)
		}
		counts[id] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s view counts: %v", kind, err)
	}
	return counts
}

// refs.BacklinkCounts reads the arms of graph_edges one by one so each can use an index. Its
// answer is the view's, for every node kind, with and without an excluded edge kind, and the
// statement it runs reads no table sequentially.
func TestBacklinkCountsEqualTheViewAndScanNoTable(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedEveryInboundArm(t, ctx, store)

	// Every table an arm reads. A plan with sequential scans off still falls back to one when
	// no index serves a predicate, so a plan free of them proves each arm has its index.
	armTables := []string{
		"refs", "issues", "issue_links", "artifacts", "asks", "comments", "messages",
		"ask_followers", "components", "component_depends", "issue_component_members",
	}
	// Each node kind with the exclusions its callers pass: none, the ask card's own thread,
	// and the mention arm, which is the one every kind shares.
	for _, probe := range []struct {
		kind    string
		exclude []string
	}{
		{"issue", nil},
		{"issue", []string{"child_of", "attached_to"}},
		{"artifact", nil},
		{"artifact", []string{"owned_by"}},
		{"ask", nil},
		{"ask", refs.AskBacklinkExclusions},
		{"comment", nil},
		{"comment", []string{"replies_to"}},
		{"message", nil},
		{"session", nil},
		{"session", []string{"followed_by"}},
		{"component", nil},
		{"component", []string{"part_of", "depends_on", "affects"}},
		{"component", []string{"mentions"}},
	} {
		name := probe.kind + " excluding " + strings.Join(probe.exclude, ",")
		ids := inboundTargets(t, ctx, store, probe.kind)
		if len(ids) == 0 {
			t.Fatalf("%s: the fixture holds no inbound edge for that kind", name)
		}
		// An id with no inbound edge must be absent from both answers.
		ids = append(ids, "nothing-points-at-this")
		recorder := &recordingQueryer{Pool: store.Pool}
		got, err := refs.BacklinkCounts(ctx, recorder, probe.kind, ids, probe.exclude)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := viewCounts(t, ctx, store, probe.kind, ids, probe.exclude)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: counts = %v, want the view's %v", name, got, want)
		}
		plan := explainWithoutSeqScanArgs(t, store, recorder.sql, recorder.args)
		for _, table := range armTables {
			if strings.Contains(plan, "Seq Scan on "+table) {
				t.Errorf("%s: the count scans %s sequentially:\n%s", name, table, plan)
			}
		}
	}

	// The fixture must make a count the view can disagree with: at least one node with more
	// than one inbound edge, through more than one arm.
	counts := viewCounts(t, ctx, store, "issue", []string{"CORE-1"}, nil)
	if counts["CORE-1"] < 5 {
		t.Errorf("CORE-1 has %d inbound edges, too few to tell a dropped arm from a kept one", counts["CORE-1"])
	}

	// A node kind with no declared arms is refused rather than counted low, so an arm added to
	// the view for a new kind cannot silently return zero.
	if _, err := refs.BacklinkCounts(ctx, store.Pool, "pull_request", []string{"x"}, nil); err == nil {
		t.Error("counting an unknown node kind returned no error")
	}
}

// refs.Kinds is every edge kind graph_edges emits, and each one is either a mention or an arm
// the count knows, so an edge kind added to the view without an arm here fails this test.
func TestEveryEdgeKindHasAnInboundArm(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedEveryInboundArm(t, ctx, store)

	rows, err := store.Pool.Query(ctx, `select distinct kind, to_kind from graph_edges order by kind, to_kind`)
	if err != nil {
		t.Fatalf("read edge kinds: %v", err)
	}
	defer rows.Close()
	var seeded []string
	for rows.Next() {
		var kind, toKind string
		if err := rows.Scan(&kind, &toKind); err != nil {
			t.Fatalf("scan edge kind: %v", err)
		}
		seeded = append(seeded, kind+" -> "+toKind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate edge kinds: %v", err)
	}
	want := []string{
		"affects -> component",
		"anchored_to -> artifact",
		"attached_to -> issue",
		"blocked_by -> issue",
		"child_of -> issue",
		"depends_on -> component",
		"followed_by -> session",
		"mentions -> artifact",
		"mentions -> ask",
		"mentions -> comment",
		"mentions -> component",
		"mentions -> issue",
		"mentions -> message",
		"mentions -> session",
		"owned_by -> artifact",
		"part_of -> component",
		"replies_to -> ask",
		"replies_to -> comment",
		"replies_to -> message",
	}
	sort.Strings(seeded)
	if !reflect.DeepEqual(seeded, want) {
		t.Fatalf("the fixture's edges are %v,\nwant every arm of the view: %v", seeded, want)
	}
}

func explainWithoutSeqScanArgs(t *testing.T, store *Store, query string, args []any) string {
	t.Helper()
	ctx := context.Background()
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off`); err != nil {
		t.Fatalf("disable seq scans: %v", err)
	}
	rows, err := tx.Query(ctx, "explain (costs off) "+query, args...)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	return strings.Join(lines, "\n")
}
