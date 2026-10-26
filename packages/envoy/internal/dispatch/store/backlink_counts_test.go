package store

import (
	"context"
	"reflect"
	"slices"
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

		insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
			values ('CORE', 0, 'acme/core', 'main', 1, '{"kind":"user","id":"alice"}');
		insert into architecture_snapshots (project_key, commit, files)
			values ('CORE', 'abc123', '{}'::jsonb);
		insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
			select 'CORE', c.id, c.title, '', c.parent, s.id, 0
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

		-- A source of a node kind this build does not read, pointing at one node of every kind:
		-- the view lists it and the count must leave it out.
		insert into refs (from_kind, from_id, to_kind, to_id, source_seq)
			select 'future', 'f1', to_kind, min(to_id), 46
			from graph_edges group by to_kind;
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

// viewCounts is the count BacklinkCounts answered before it read the arms: the grouped count over
// graph_edges itself, of sources of a node kind this build reads, which is the answer this change
// may not move.
func viewCounts(t *testing.T, ctx context.Context, store *Store, kind string, ids, excludeKinds []string) map[string]int {
	t.Helper()
	sql := `select to_id, count(*)::int from graph_edges where to_kind = $1 and to_id = any($2) and from_kind = any($3)`
	args := []any{kind, ids, refs.NodeKinds()}
	if len(excludeKinds) > 0 {
		args = append(args, excludeKinds)
		sql += ` and not (kind = any($4))`
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
// answer is the view's, for every node kind, with and without an excluded edge kind, and every
// scan the statement it runs makes is driven by an index condition, in the plan its values give
// and in the generic plan a prepared statement's cache could settle on.
func TestBacklinkCountsEqualTheViewAndScanNoTable(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedEveryInboundArm(t, ctx, store)

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
		for _, plan := range []string{
			explainWithoutSeqScanArgs(t, store, "explain (costs off) "+recorder.sql, recorder.args),
			explainGenericWithoutSeqScan(t, store, recorder.sql, recorder.args),
		} {
			if unkeyed := scansWithoutIndexCond(plan); len(unkeyed) > 0 {
				t.Errorf("%s: the count reads %v without an index condition:\n%s", name, unkeyed, plan)
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

// The fixture fills every arm of graph_edges, and the view has the fifteen arms the count
// declares (refs' mentions and refs.BacklinkCounts' fourteen structural arms), so an arm added
// to the view fails this test until the count and the fixture carry it too.
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

	var definition string
	if err := store.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges'::regclass)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}
	if arms := strings.Count(definition, "UNION ALL") + 1; arms != 15 {
		t.Fatalf("graph_edges has %d arms; refs.BacklinkCounts declares 15 (the mention arm and 14 structural arms): add the new arm to inboundArms and to seedEveryInboundArm", arms)
	}

	// BacklinkCounts filters only the mention arm by source kind, which is exact only while every
	// structural arm's source is a node kind this build reads.
	sources, err := store.Pool.Query(ctx,
		`select distinct from_kind from graph_edges where kind <> 'mentions' order by from_kind`)
	if err != nil {
		t.Fatalf("read structural source kinds: %v", err)
	}
	defer sources.Close()
	for sources.Next() {
		var kind string
		if err := sources.Scan(&kind); err != nil {
			t.Fatalf("scan structural source kind: %v", err)
		}
		if !slices.Contains(refs.NodeKinds(), kind) {
			t.Errorf("a structural arm of graph_edges has source kind %q, which nodeLoaders does not read: filter that arm by source kind in refs.BacklinkCounts", kind)
		}
	}
	if err := sources.Err(); err != nil {
		t.Fatalf("iterate structural source kinds: %v", err)
	}
}

// scansWithoutIndexCond names each scan in an EXPLAIN plan that reads a table without an index
// condition: a sequential scan, or an index scan that only filters the rows it walks, which a
// plan with sequential scans turned off picks when no index serves the predicate.
func scansWithoutIndexCond(plan string) []string {
	lines := strings.Split(plan, "\n")
	var unkeyed []string
	for index, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "->"))
		switch {
		case strings.HasPrefix(trimmed, "Seq Scan on "):
			unkeyed = append(unkeyed, trimmed)
		case strings.HasPrefix(trimmed, "Index Scan "),
			strings.HasPrefix(trimmed, "Index Only Scan "),
			strings.HasPrefix(trimmed, "Bitmap Index Scan "):
			keyed := false
			for _, detail := range lines[index+1:] {
				detail = strings.TrimSpace(detail)
				if strings.HasPrefix(detail, "->") {
					break
				}
				if strings.HasPrefix(detail, "Index Cond:") {
					keyed = true
					break
				}
			}
			if !keyed {
				unkeyed = append(unkeyed, trimmed)
			}
		}
	}
	return unkeyed
}

// indexOnlyPlanSettings leave the planner only plans that reach each table through an index:
// with sequential scans off it can still hash a whole partial index into a join, so hash and
// merge joins go too, and a scan with no index condition left in the plan is one no index serves.
var indexOnlyPlanSettings = []string{
	`set local enable_seqscan = off`,
	`set local enable_hashjoin = off`,
	`set local enable_mergejoin = off`,
}

// explainWithoutSeqScanArgs runs explain, an EXPLAIN statement around the count's SQL, binding
// args, under indexOnlyPlanSettings.
func explainWithoutSeqScanArgs(t *testing.T, store *Store, explain string, args []any) string {
	t.Helper()
	ctx := context.Background()
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range indexOnlyPlanSettings {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return planLines(t, tx, explain, args...)
}

// explainGenericWithoutSeqScan is the plan a prepared statement's cache settles on when it
// chooses a generic plan: the count is prepared with no values and executed under
// plan_cache_mode = force_generic_plan, under indexOnlyPlanSettings. A prepared statement outlives
// the transaction, so it is deallocated by name.
func explainGenericWithoutSeqScan(t *testing.T, store *Store, query string, args []any) string {
	t.Helper()
	ctx := context.Background()
	conn, err := store.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain: %v", err)
	}
	defer tx.Rollback(ctx)
	literals := make([]string, len(args))
	for index, arg := range args {
		literals[index] = sqlLiteral(t, arg)
	}
	for _, statement := range append(indexOnlyPlanSettings,
		`set local plan_cache_mode = force_generic_plan`,
		`prepare backlink_count_probe as `+query,
	) {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	defer func() {
		if _, err := conn.Exec(ctx, `deallocate backlink_count_probe`); err != nil {
			t.Errorf("deallocate the probe: %v", err)
		}
	}()
	return planLines(t, tx, `explain (costs off) execute backlink_count_probe(`+strings.Join(literals, ", ")+`)`)
}

// sqlLiteral writes one of the count's arguments, a string or a list of strings, as a quoted SQL
// literal for EXECUTE, which takes no bind parameters of its own here.
func sqlLiteral(t *testing.T, arg any) string {
	t.Helper()
	quote := func(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }
	switch value := arg.(type) {
	case string:
		return quote(value) + "::text"
	case []string:
		elements := make([]string, len(value))
		for index, element := range value {
			element = strings.ReplaceAll(element, `\`, `\\`)
			elements[index] = `"` + strings.ReplaceAll(element, `"`, `\"`) + `"`
		}
		return quote("{"+strings.Join(elements, ",")+"}") + "::text[]"
	default:
		t.Fatalf("count argument %T has no literal form", arg)
		return ""
	}
}

func planLines(t *testing.T, tx pgx.Tx, explain string, args ...any) string {
	t.Helper()
	rows, err := tx.Query(context.Background(), explain, args...)
	if err != nil {
		t.Fatalf("%s: %v", explain, err)
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
