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
		blocker as (insert into issue_links (issue_key, kind, target_key) values ('CORE-2', 'blocked_by', 'CORE-1') returning issue_key),
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
		from spec, pdoc, block_ask, anchored_ask, project_ask, plain_ask, anchored_comment, project_comment, reply, ask_reply, message, message_reply, follower, mention, blocker, member
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
		{"issue", "CORE-2", "blocked_by", "issue", "CORE-1", false},
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
		"to artifact":            `select * from graph_edges where to_kind = 'artifact' and to_id = 'CORE-1/spec'`,
		"to issue":               `select * from graph_edges where to_kind = 'issue' and to_id = 'CORE-1'`,
		"to component":           `select * from graph_edges where to_kind = 'component' and to_id = 'CORE/web'`,
		"from ask":               `select * from graph_edges where from_kind = 'ask' and from_id = '` + ids.PlainAsk + `'`,
		"from issue":             `select * from graph_edges where from_kind = 'issue' and from_id = 'CORE-2'`,
		"from block":             `select * from graph_edges where from_kind = 'block' and from_id = 'CORE-1/spec#b1'`,
		"to a document's blocks": `select * from graph_edges where to_kind = 'block' and to_id in (select node_id from document_blocks where artifact_id = '` + ids.Spec + `')`,
		"to comment":             `select * from graph_edges where to_kind = 'comment' and to_id = '` + ids.AnchoredComment + `'`,
		"to message":             `select * from graph_edges where to_kind = 'message' and to_id = '` + ids.Message + `'`,
		"to comment block":       `select * from graph_edges where to_kind = 'comment_block' and to_id = 'comment/` + ids.AnchoredComment + `#b1'`,
		"from comment block":     `select * from graph_edges where from_kind = 'comment_block' and from_id = 'comment/` + ids.AnchoredComment + `#b1'`,
		"to message block":       `select * from graph_edges where to_kind = 'message_block' and to_id = 'message/` + ids.Message + `#b1'`,
		"from message block":     `select * from graph_edges where from_kind = 'message_block' and from_id = 'message/` + ids.Message + `#b1'`,
		"to a comment's blocks":  `select * from graph_edges where to_kind = 'comment_block' and to_id in (select node_id from comment_blocks where comment_id = '` + ids.AnchoredComment + `')`,
		"to a message's blocks":  `select * from graph_edges where to_kind = 'message_block' and to_id in (select node_id from message_blocks where message_id = '` + ids.Message + `')`,
		"to ask block":           `select * from graph_edges where to_kind = 'ask_block' and to_id = 'ask/` + ids.PlainAsk + `#b1'`,
		"from ask block":         `select * from graph_edges where from_kind = 'ask_block' and from_id = 'ask/` + ids.PlainAsk + `#b1'`,
		"to an ask's blocks":     `select * from graph_edges where to_kind = 'ask_block' and to_id in (select node_id from ask_blocks where ask_id = '` + ids.PlainAsk + `')`,
	} {
		plan := explainWithoutSeqScan(t, store, query)
		for _, table := range []string{"refs", "artifacts", "asks", "comments", "messages", "issues", "issue_links", "ask_followers", "issue_component_members", "issue_components", "document_blocks", "block_refs", "document_block_versions", "comment_blocks", "message_blocks", "comment_block_refs", "message_block_refs", "ask_blocks", "ask_block_refs"} {
			if strings.Contains(plan, "Seq Scan on "+table) {
				t.Errorf("%s: plan scans %s sequentially:\n%s", name, table, plan)
			}
		}
	}
}

// A block of a document is a node of the graph: each link it holds is an edge out of it, two
// links from one block to one target under different names are two edges, and the block is
// part_of its document. A block's edges keep their source_seq; its part_of edge, being
// structural, carries none.
func TestGraphEdgesCarryBlockMentionsAndPartOf(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		with p as (insert into projects (key, name) values ('CORE', 'Core') returning key),
		issue as (insert into issues (key, project_key, number, title, created_by, rank) select 'CORE-1', key, 1, 'One', '{"kind":"user","id":"alice"}', 'A' from p returning key),
		target as (insert into issues (key, project_key, number, title, created_by, rank) select 'CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'B' from issue returning key),
		spec as (insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) select key, 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}' from issue returning id),
		block as (insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at) select id, 'b1', 'CORE-1/spec#b1', 'paragraph', 'x', 'x', 1, now() from spec returning artifact_id)
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version, source_seq)
		select artifact_id, 'b1', 'CORE-1/spec#b1', 'issue', 'CORE-2', link.kind, 1, 7
		from block, target, (values ('mentions'), ('depends_on')) as link (kind)
	`); err != nil {
		t.Fatalf("seed block: %v", err)
	}

	rows, err := store.Pool.Query(ctx, `
		select from_kind, from_id, kind, to_kind, to_id, source_seq is not null from graph_edges
		where from_kind = 'block'`)
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
		{"block", "CORE-1/spec#b1", "mentions", "issue", "CORE-2", true},
		{"block", "CORE-1/spec#b1", "depends_on", "issue", "CORE-2", true},
		{"block", "CORE-1/spec#b1", "part_of", "artifact", "CORE-1/spec", false},
	}
	sortGraphEdges(got)
	sortGraphEdges(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("block edges = %#v\nwant %#v", got, want)
	}
}

// A current block of a comment, a message or an ask is a node of the graph under its storage kind
// (comment_block, message_block, ask_block): it is part_of its item, each link it holds is an edge
// out of it, and a reply that names it is a replies_to edge to it. A removed block is neither
// part_of its item nor the source of the links it held. A reply whose request names an ask names
// a part of that ask in the same comments.replies_to_block column a reply to a comment names a
// comment block in, so the two kinds are told apart by the node id's prefix: the ask part's
// reply reaches ask_block and the comment block's reply comment_block.
func TestItemBlockArmsAreGraphEdges(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var comment, reply, message, messageReply, ask, askReply string
	if err := store.Pool.QueryRow(ctx, `
		with p as (insert into projects (key, name) values ('CORE', 'Core') returning key),
		issue as (insert into issues (key, project_key, number, title, created_by, rank) select 'CORE-1', key, 1, 'One', '{"kind":"user","id":"alice"}', 'A' from p returning key),
		target as (insert into issues (key, project_key, number, title, created_by, rank) select 'CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'B' from issue returning key),
		comment as (insert into comments (issue_key, author, body) select key, '{"kind":"user","id":"alice"}', 'Alpha CORE-2' from issue returning id),
		comment_block as (insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at)
			select id, 'b1', 'comment/' || id || '#b1', 0, 'paragraph', 'x', 'Alpha CORE-2', now(), 1, now() from comment returning comment_id),
		comment_removed as (insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at, removed_at)
			select id, 'b2', 'comment/' || id || '#b2', null, 'paragraph', 'y', 'Gone CORE-2', now(), 1, now(), now() from comment returning comment_id),
		comment_refs as (insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id, source_seq)
			select id, b.block_id, 'comment/' || id || '#' || b.block_id, 'issue', 'CORE-2', 7 from comment, target, (values ('b1'), ('b2')) as b (block_id) returning comment_id),
		reply as (insert into comments (issue_key, author, body, reply_to, replies_to_block)
			select 'CORE-1', '{"kind":"session","id":"s1"}', 'Agreed', id, 'comment/' || id || '#b1' from comment returning id),
		message as (insert into messages (issue_key, author, body) select key, '{"kind":"session","id":"s1"}', 'Beta CORE-2' from issue returning id),
		message_block as (insert into message_blocks (message_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_at)
			select id, 'b1', 'message/' || id || '#b1', 0, 'paragraph', 'z', 'Beta CORE-2', now(), now() from message returning message_id),
		message_refs as (insert into message_block_refs (message_id, block_id, from_node, to_kind, to_id, source_seq)
			select id, 'b1', 'message/' || id || '#b1', 'issue', 'CORE-2', 8 from message, target returning message_id),
		message_reply as (insert into messages (issue_key, author, body, in_reply_to, replies_to_block)
			select 'CORE-1', '{"kind":"user","id":"alice"}', 'Noted', id, 'message/' || id || '#b1' from message returning id),
		ask as (insert into asks (issue_key, author, question) select key, '{"kind":"user","id":"alice"}', 'Which CORE-2?' from issue returning id),
		ask_block as (insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at)
			select id, 'b1', 'ask/' || id || '#b1', 0, 'question', 'q', 'Which CORE-2?', now(), 1, now() from ask returning ask_id),
		ask_removed as (insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt, made_at, changed_revision, changed_at, removed_at)
			select id, 'b2', 'ask/' || id || '#b2', null, 'option', 'o', 'Dropped CORE-2', now(), 1, now(), now() from ask returning ask_id),
		ask_refs as (insert into ask_block_refs (ask_id, block_id, from_node, to_kind, to_id, source_seq)
			select id, b.block_id, 'ask/' || id || '#' || b.block_id, 'issue', 'CORE-2', 9 from ask, target, (values ('b1'), ('b2')) as b (block_id) returning ask_id),
		ask_reply as (insert into comments (issue_key, author, body, ask_id, replies_to_block)
			select 'CORE-1', '{"kind":"session","id":"s1"}', 'The first one', id, 'ask/' || id || '#b1' from ask returning id)
		select comment.id::text, reply.id::text, message.id::text, message_reply.id::text, ask.id::text, ask_reply.id::text
		from comment, reply, message, message_reply, ask, ask_reply, comment_block, comment_removed, message_block, ask_block, ask_removed
	`).Scan(&comment, &reply, &message, &messageReply, &ask, &askReply); err != nil {
		t.Fatalf("seed item blocks: %v", err)
	}

	rows, err := store.Pool.Query(ctx, `
		select from_kind, from_id, kind, to_kind, to_id, source_seq is not null from graph_edges
		where from_kind in ('comment_block', 'message_block', 'ask_block') or to_kind in ('comment_block', 'message_block', 'ask_block')`)
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
	commentBlock, messageBlock := "comment/"+comment+"#b1", "message/"+message+"#b1"
	askBlock := "ask/" + ask + "#b1"
	want := []graphEdgeRow{
		{"comment_block", commentBlock, "part_of", "comment", comment, false},
		{"comment_block", commentBlock, "mentions", "issue", "CORE-2", true},
		{"comment", reply, "replies_to", "comment_block", commentBlock, false},
		{"message_block", messageBlock, "part_of", "message", message, false},
		{"message_block", messageBlock, "mentions", "issue", "CORE-2", true},
		{"message", messageReply, "replies_to", "message_block", messageBlock, false},
		{"ask_block", askBlock, "part_of", "ask", ask, false},
		{"ask_block", askBlock, "mentions", "issue", "CORE-2", true},
		{"comment", askReply, "replies_to", "ask_block", askBlock, false},
	}
	sortGraphEdges(got)
	sortGraphEdges(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("item block edges = %#v\nwant %#v", got, want)
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
