package store

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type graphEdgeRow struct {
	FromKind, FromID, Kind, ToKind, ToID string
	HasSourceSeq                         bool
}

// graph_edges is one typed relation over refs and every structural column: each seeded relation
// appears exactly once with its kind, anchors that are JSON null or absent produce nothing, and
// the guard in 0032 leaves well-formed uuid anchors alone.
func TestGraphEdgesUnionsMentionsWithEveryStructuralRelation(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var ids struct {
		Spec, ProjectDoc, BlockAsk, AnchoredAsk, ProjectAsk, PlainAsk, AnchoredComment, ProjectComment, Reply, AskReply, Message, MessageReply string
	}
	if err := store.Pool.QueryRow(ctx, `
		with p as (insert into projects (key, name) values ('CORE', 'Core') returning key),
		parent as (insert into issues (key, project_key, number, title, created_by, rank) values ('CORE-1', 'CORE', 1, 'Parent', '{"kind":"user","id":"alice"}', 'A') returning key),
		child as (insert into issues (key, project_key, number, title, created_by, rank, parent_key) select 'CORE-2', 'CORE', 2, 'Child', '{"kind":"user","id":"alice"}', 'B', key from parent returning key),
		spec as (insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}') returning id, ref_key),
		pdoc as (insert into artifacts (project_key, slug, name, kind, created_by) values ('CORE', 'notes', 'Notes', 'doc', '{"kind":"user","id":"alice"}') returning id, ref_key),
		block_ask as (insert into asks (issue_key, author, question, block_id, block_artifact_id) select 'CORE-1', '{"kind":"user","id":"alice"}', 'block?', 'b1', id from spec returning id),
		anchored_ask as (insert into asks (issue_key, author, question, anchor) select 'CORE-1', '{"kind":"user","id":"alice"}', 'anchored?', jsonb_build_object('artifact_id', id::text, 'mark_id', 'm1') from spec returning id),
		project_ask as (insert into asks (artifact_id, author, question, anchor) select id, '{"kind":"user","id":"alice"}', 'project?', 'null'::jsonb from pdoc returning id),
		plain_ask as (insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"user","id":"alice"}', 'plain?') returning id),
		anchored_comment as (insert into comments (issue_key, author, body, anchor) select 'CORE-1', '{"kind":"user","id":"alice"}', 'anchored', jsonb_build_object('artifact_id', id::text, 'mark_id', 'm2') from spec returning id),
		project_comment as (insert into comments (artifact_id, author, body) select id, '{"kind":"user","id":"alice"}', 'project' from pdoc returning id),
		reply as (insert into comments (issue_key, author, body, reply_to) select 'CORE-1', '{"kind":"user","id":"alice"}', 'reply', id from anchored_comment returning id),
		ask_reply as (insert into comments (issue_key, author, body, ask_id) select 'CORE-1', '{"kind":"user","id":"alice"}', 'ask reply', id from plain_ask returning id),
		message as (insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1"}', 'hello') returning id),
		message_reply as (insert into messages (issue_key, author, body, in_reply_to) select 'CORE-1', '{"kind":"user","id":"alice"}', 'hi', id from message returning id),
		follower as (insert into ask_followers (ask_id, session_id) select id, 's1' from plain_ask returning ask_id),
		mention as (insert into refs (from_kind, from_id, to_kind, to_id, source_seq) select 'message', id::text, 'issue', 'CORE-2', 42 from message returning from_id),
		attachment as (insert into issue_components (issue_key, mode) values ('CORE-2', 'explicit') returning issue_key),
		member as (insert into issue_component_members (issue_key, project_key, component_id) select issue_key, 'CORE', 'web' from attachment returning issue_key)
		select spec.id::text, pdoc.id::text, block_ask.id::text, anchored_ask.id::text, project_ask.id::text, plain_ask.id::text,
		       anchored_comment.id::text, project_comment.id::text, reply.id::text, ask_reply.id::text, message.id::text, message_reply.id::text
		from spec, pdoc, block_ask, anchored_ask, project_ask, plain_ask, anchored_comment, project_comment, reply, ask_reply, message, message_reply, follower, mention, member
	`).Scan(&ids.Spec, &ids.ProjectDoc, &ids.BlockAsk, &ids.AnchoredAsk, &ids.ProjectAsk, &ids.PlainAsk, &ids.AnchoredComment, &ids.ProjectComment, &ids.Reply, &ids.AskReply, &ids.Message, &ids.MessageReply); err != nil {
		t.Fatalf("seed graph: %v", err)
	}

	rows, err := store.Pool.Query(ctx, `select from_kind, from_id, kind, to_kind, to_id, source_seq is not null from graph_edges`)
	if err != nil {
		t.Fatalf("read graph_edges: %v", err)
	}
	defer rows.Close()
	var got []graphEdgeRow
	for rows.Next() {
		var row graphEdgeRow
		if err := rows.Scan(&row.FromKind, &row.FromID, &row.Kind, &row.ToKind, &row.ToID, &row.HasSourceSeq); err != nil {
			t.Fatalf("scan graph edge: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate graph_edges: %v", err)
	}
	want := []graphEdgeRow{
		{"message", ids.Message, "mentions", "issue", "CORE-2", true},
		{"issue", "CORE-2", "child_of", "issue", "CORE-1", false},
		{"artifact", ids.Spec, "attached_to", "issue", "CORE-1", false},
		{"ask", ids.BlockAsk, "anchored_to", "artifact", "CORE-1/spec", false},
		{"ask", ids.AnchoredAsk, "anchored_to", "artifact", "CORE-1/spec", false},
		{"comment", ids.AnchoredComment, "anchored_to", "artifact", "CORE-1/spec", false},
		{"ask", ids.ProjectAsk, "owned_by", "artifact", "CORE/notes", false},
		{"comment", ids.ProjectComment, "owned_by", "artifact", "CORE/notes", false},
		{"comment", ids.Reply, "replies_to", "comment", ids.AnchoredComment, false},
		{"comment", ids.AskReply, "replies_to", "ask", ids.PlainAsk, false},
		{"message", ids.MessageReply, "replies_to", "message", ids.Message, false},
		{"ask", ids.PlainAsk, "followed_by", "session", "s1", false},
		{"issue", "CORE-2", "affects", "component", "CORE/web", false},
	}
	sortGraphEdges(got)
	sortGraphEdges(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("graph_edges = %#v\nwant %#v", got, want)
	}

	// With sequential scans disabled the planner still falls back to one when no index can
	// serve an arm, so a plan free of them proves every arm of the view has the index 0032
	// gives it for the predicates the read API pushes down.
	for name, query := range map[string]string{
		"to artifact":  `select * from graph_edges where to_kind = 'artifact' and to_id = 'CORE-1/spec'`,
		"to issue":     `select * from graph_edges where to_kind = 'issue' and to_id = 'CORE-1'`,
		"to component": `select * from graph_edges where to_kind = 'component' and to_id = 'CORE/web'`,
		"from ask":     `select * from graph_edges where from_kind = 'ask' and from_id = '` + ids.PlainAsk + `'`,
		"from issue":   `select * from graph_edges where from_kind = 'issue' and from_id = 'CORE-2'`,
	} {
		plan := explainWithoutSeqScan(t, store, query)
		for _, table := range []string{"refs", "artifacts", "asks", "comments", "messages", "issues", "ask_followers", "issue_component_members", "issue_components"} {
			if strings.Contains(plan, "Seq Scan on "+table) {
				t.Errorf("%s: plan scans %s sequentially:\n%s", name, table, plan)
			}
		}
	}
}

func sortGraphEdges(edges []graphEdgeRow) {
	sort.Slice(edges, func(left, right int) bool {
		if edges[left].Kind != edges[right].Kind {
			return edges[left].Kind < edges[right].Kind
		}
		if edges[left].FromKind != edges[right].FromKind {
			return edges[left].FromKind < edges[right].FromKind
		}
		return edges[left].FromID < edges[right].FromID
	})
}

func explainWithoutSeqScan(t *testing.T, store *Store, query string) string {
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
	rows, err := tx.Query(ctx, "explain (costs off) "+query)
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

// 0032 refuses to build graph_edges over an anchor whose artifact_id is not a uuid, because the
// view casts it on every row and one bad anchor would break every to=artifact read.
func TestMigrate0032RefusesNonUUIDAnchorArtifactIDs(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 31)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank) values ('CORE-1', 'CORE', 1, 'Issue', '{"kind":"user","id":"alice"}', 'A');
		insert into asks (issue_key, author, question, anchor) values ('CORE-1', '{"kind":"user","id":"alice"}', 'q', '{"artifact_id":"spec","mark_id":"m1"}');
	`); err != nil {
		t.Fatalf("seed legacy anchor: %v", err)
	}
	err := store.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "0032_refs_provenance") || !strings.Contains(err.Error(), "is not a uuid") {
		t.Fatalf("migrate with a non-uuid anchor: err=%v; want the 0032 guard to refuse", err)
	}
	var version int
	if err := store.Pool.QueryRow(ctx, `select max(version) from schema_migrations`).Scan(&version); err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version != 31 {
		t.Fatalf("schema version after refused migration = %d; want 31", version)
	}
}
