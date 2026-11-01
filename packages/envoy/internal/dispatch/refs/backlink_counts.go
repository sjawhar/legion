package refs

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// BacklinkCounts is how many edges point at each of ids, in one grouped query: the number a
// list carries beside a row, counted the same way the rows behind it are listed. It unions only
// the arms of graph_edges that can point at a node of toKind - the four mention arms every kind
// has and the structural arms declared for that kind (backlinkArms) - and counts them in one
// statement, each arm read through its own index, rather than leaving the planner the whole
// 23-arm view on every call. The counts are the view's, arm for arm. A kind with no entry in the
// arm table is refused rather than counted from the mention arms alone, so a count is never
// silently low.
//
// It counts only edges whose source is a node kind this build reads, so the count is the rows its
// list shows, and counts a document's, comment's or message's mention once for each of its blocks'
// links to the node, in place of the item's own edge (heldByABlock): a comment citing a node from
// two paragraphs counts 2, and a document's block pointing at the node counts once whatever its
// link's kind. A node with no inbound edge is absent from the result; the caller decides what
// zero means.
func BacklinkCounts(
	ctx context.Context,
	q Queryer,
	toKind string,
	ids []string,
	excludeKinds []string,
) (map[string]int, error) {
	counts := make(map[string]int, len(ids))
	if len(ids) == 0 {
		return counts, nil
	}
	// A kind that is no key of the arm table is refused before any SQL is built: the count has
	// no arms to read for it, and would otherwise be silently low.
	statement, ok := backlinkCountSQL(toKind, excludeKinds)
	if !ok {
		return nil, fmt.Errorf("count %s backlinks: no arms declared for that node kind", toKind)
	}
	args := []any{ids, nodeKinds}
	if len(excludeKinds) > 0 {
		args = append(args, excludeKinds)
	}
	rows, err := q.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("count %s backlinks: %w", toKind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("scan %s backlink count: %w", toKind, err)
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// backlinkCountSQL is the statement BacklinkCounts runs: the union of the arms of graph_edges
// that can point at a node of toKind, grouped by target, with the arms a caller's excludeKinds
// rule out left out. It binds the addresses as $1, the node kinds this build reads as $2 and,
// when excludeKinds is not empty, those kinds as $3. ok is false for a kind backlinkArms does not
// declare, the one kind BacklinkCounts refuses; it is a builder of its own so a test can read the
// statement BacklinkCounts runs and hold its plan to an index on every arm.
//
// The kind is written into each arm as a literal rather than a parameter because the statement
// text already differs per kind, and a literal lets the planner know how selective each arm's
// to_kind predicate is; only a kind backlinkArms declares reaches that, so nothing a caller names
// is ever written into a statement.
func backlinkCountSQL(toKind string, excludeKinds []string) (string, bool) {
	structural, ok := backlinkArms[toKind]
	if !ok {
		return "", false
	}
	exclude := func(string) string { return "" }
	if len(excludeKinds) > 0 {
		exclude = func(alias string) string { return " and not (" + alias + ".kind = any($3))" }
	}
	arms := mentionBacklinkArms(toKind, exclude)
	for _, arm := range structural {
		// A structural arm emits one edge kind, written as a literal, so excluding that kind
		// excludes every row the arm can produce: the arm is dropped rather than filtered.
		if slices.Contains(excludeKinds, arm.kind) {
			continue
		}
		arms = append(arms, arm.sql)
	}
	return `select to_id, count(*)::int from (` + strings.Join(arms, " union all ") + `) edge group by to_id`, true
}

// backlinkArm is one arm of graph_edges whose edge kind is a SQL literal rather than a column:
// kind is that literal, and sql selects the arm's to_id alone, keyed on the addresses bound as
// $1. Because the kind is fixed, a caller excluding it excludes the whole arm.
type backlinkArm struct {
	kind string
	sql  string
}

// backlinkArms is, for each node kind nodeLoaders resolves, every arm of graph_edges whose edge
// kind is a literal and whose target is a node of that kind. BacklinkCounts refuses a kind that
// is no key of this table, so a node kind a newer build adds to the graph is counted only once
// its arms are declared here; "block" is present and empty because no literal-kind arm of the
// view targets a block, and an entry that is present but empty is counted from the mention arms
// alone rather than refused.
//
// Each arm selects only the address it is keyed on and leaves out the joins the view makes only
// to date an edge. Dropping them preserves every row: components.snapshot_id (0037 line 38),
// component_depends' (project_key, from_id) and (project_key, to_id) foreign keys (0037 lines
// 47-48) and issue_component_members.issue_key (0038 line 21) are each a not-null foreign key to
// the primary key the view joins, so each of those joins finds exactly one row.
var backlinkArms = map[string][]backlinkArm{
	"issue": {
		// issues_parent_key, partial on parent_key is not null.
		{kind: "child_of", sql: `select i.parent_key as to_id from issues i where i.parent_key is not null and i.parent_key = any($1)`},
		// issue_links_target (target_key, kind).
		{kind: "blocked_by", sql: `select l.target_key as to_id from issue_links l where l.kind = 'blocked_by' and l.target_key = any($1)`},
		// artifacts_issue_key, partial on issue_key is not null.
		{kind: "attached_to", sql: `select a.issue_key as to_id from artifacts a where a.issue_key is not null and a.issue_key = any($1)`},
	},
	// The view addresses an artifact by artifacts.ref_key, so each arm starts at artifacts
	// through the unique artifacts_ref_key_key and joins the table that points at it.
	"artifact": {
		// asks_anchor_artifact, on the same coalesce the join makes.
		{kind: "anchored_to", sql: `select a.ref_key as to_id from artifacts a join asks k on coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = a.id where a.ref_key = any($1)`},
		// comments_anchor_artifact, on the same cast the join makes.
		{kind: "anchored_to", sql: `select a.ref_key as to_id from artifacts a join comments c on (c.anchor->>'artifact_id')::uuid = a.id where a.ref_key = any($1)`},
		// asks_artifact_id.
		{kind: "owned_by", sql: `select a.ref_key as to_id from artifacts a join asks k on k.artifact_id = a.id where a.ref_key = any($1)`},
		// comments_artifact_id.
		{kind: "owned_by", sql: `select a.ref_key as to_id from artifacts a join comments c on c.artifact_id = a.id where a.ref_key = any($1)`},
		// The document_blocks primary key (artifact_id, block_id).
		{kind: "part_of", sql: `select a.ref_key as to_id from artifacts a join document_blocks d on d.artifact_id = a.id where a.ref_key = any($1)`},
	},
	"ask": {
		// comments_ask_id_text.
		{kind: "replies_to", sql: `select c.ask_id::text as to_id from comments c where c.ask_id is not null and c.ask_id::text = any($1)`},
	},
	"comment": {
		// comments_reply_to_text.
		{kind: "replies_to", sql: `select c.reply_to::text as to_id from comments c where c.reply_to is not null and c.reply_to::text = any($1)`},
		// comment_blocks_comment_id_text. The view's arm (0142 line 206) counts a current block
		// only, so this one carries its position filter.
		{kind: "part_of", sql: `select b.comment_id::text as to_id from comment_blocks b where b.position is not null and b.comment_id::text = any($1)`},
	},
	"message": {
		// messages_in_reply_to_text.
		{kind: "replies_to", sql: `select m.in_reply_to::text as to_id from messages m where m.in_reply_to is not null and m.in_reply_to::text = any($1)`},
		// message_blocks_message_id_text. The view's arm (0142 line 209) counts a current block
		// only, so this one carries its position filter.
		{kind: "part_of", sql: `select b.message_id::text as to_id from message_blocks b where b.position is not null and b.message_id::text = any($1)`},
	},
	"component": {
		// components_parent_node, on the same expression the arm selects.
		{kind: "part_of", sql: `select c.project_key || '/' || c.parent as to_id from components c where c.parent is not null and c.project_key || '/' || c.parent = any($1)`},
		// component_depends_to_node.
		{kind: "depends_on", sql: `select d.project_key || '/' || d.to_id as to_id from component_depends d where d.project_key || '/' || d.to_id = any($1)`},
		// issue_component_members_node.
		{kind: "affects", sql: `select m.project_key || '/' || m.component_id as to_id from issue_component_members m where m.project_key || '/' || m.component_id = any($1)`},
	},
	"session": {
		// ask_followers_session_id (migration 20261101182419), which exists for this arm.
		{kind: "followed_by", sql: `select f.session_id as to_id from ask_followers f where f.session_id = any($1)`},
	},
	// No arm of the view whose edge kind is a literal targets a block: a block is pointed at by
	// mentions and by replies, and a reply names a comment_block or a message_block. The entry is
	// present and empty so a block is counted from its mention arms rather than refused.
	"block": {},
	"comment_block": {
		// comments_replies_to_block, partial on replies_to_block is not null. The view's arm
		// (0142 line 222) carries no position filter, and neither does this one.
		{kind: "replies_to", sql: `select c.replies_to_block as to_id from comments c where c.replies_to_block is not null and c.replies_to_block = any($1)`},
	},
	"message_block": {
		// messages_replies_to_block, partial on replies_to_block is not null. The view's arm
		// (0142 line 225) carries no position filter either.
		{kind: "replies_to", sql: `select m.replies_to_block as to_id from messages m where m.replies_to_block is not null and m.replies_to_block = any($1)`},
	},
}

// mentionBacklinkArms is the four arms of graph_edges whose edge kind and target kind are columns
// rather than literals: refs and the three tables of block references. Every node kind gets all
// four, since a mention can name any node. The target kind is written as a literal (see
// BacklinkCounts); exclude is the edge-kind exclusion for an alias, empty when the caller passed
// none - on these arms the edge kind is a column, so an exclusion is a predicate and never drops
// an arm.
func mentionBacklinkArms(toKind string, exclude func(alias string) string) []string {
	kind := `'` + toKind + `'`
	return []string{
		// refs_to (to_kind, to_id). heldByABlock belongs on this arm alone: it is gated on
		// kind = 'mentions' and from_kind in artifact, comment, message, and refs is the only arm
		// of the view that can emit that pair, every other arm fixing from_kind as a literal that
		// is none of the three. Its three exists probes carry no position filter, as the view's
		// readers do not. The source-kind filter is this arm's alone for the same reason: every
		// other arm's literal from_kind is a kind nodeLoaders resolves, so the filter would drop
		// nothing there.
		`select r.to_id from refs r
			where r.to_kind = ` + kind + ` and r.to_id = any($1) and r.from_kind = any($2)
			and not ` + heldByABlock("r") + exclude("r"),
		// block_refs_to (to_kind, to_id). The view's own arm lists a block[] pointer's row only
		// while block_ref_listed holds, the types test a typed block's pointer carries, and a row
		// with no limit is listed without the call; this arm repeats both, so the count is the
		// rows the list shows. A pointer whose target is retyped out of its allowed types leaves
		// the count the moment it leaves the list, with no rewrite of either.
		`select b.to_id from block_refs b
			where b.to_kind = ` + kind + ` and b.to_id = any($1)
			and (b.to_types is null or block_ref_listed(b.to_types, b.to_id))` + exclude("b"),
		// comment_block_refs_to (to_kind, to_id, (comment_id::text)), then the comment_blocks
		// primary key for the join. The join keeps the view's position filter (0142 lines
		// 212-215): the count copies the view's answer, it does not correct it.
		`select r.to_id from comment_block_refs r
			join comment_blocks b on b.comment_id = r.comment_id and b.block_id = r.block_id
			where r.to_kind = ` + kind + ` and r.to_id = any($1) and b.position is not null` + exclude("r"),
		// message_block_refs_to (to_kind, to_id, (message_id::text)), then the message_blocks
		// primary key for the join, with the view's position filter (0142 lines 217-220).
		`select r.to_id from message_block_refs r
			join message_blocks b on b.message_id = r.message_id and b.block_id = r.block_id
			where r.to_kind = ` + kind + ` and r.to_id = any($1) and b.position is not null` + exclude("r"),
	}
}
