package refs

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// BacklinkCounts is how many edges point at each of ids, in one grouped query: the number a
// list carries beside a row, counted the same way the rows behind it are listed. It unions only
// the arms of graph_edges that can point at a node of toKind - the five mention arms every kind
// has and the structural arms declared for that kind (backlinkArms) - and counts them in one
// statement, each arm read through its own index, rather than leaving the planner the whole
// 26-arm view on every call. The counts are the view's, arm for arm. A kind with no entry in the
// arm table is refused rather than counted from the mention arms alone, so a count is never
// silently low.
//
// It counts only edges whose source is a node kind this build reads, so the count is the rows its
// list shows, and counts a document's, comment's, message's or ask's mention once for each of its
// blocks' links to the node, in place of the item's own edge (heldByABlock): a comment citing a
// node from two paragraphs counts 2, and a document's block pointing at the node counts once
// whatever its link's kind. An edge at a current block of a counted item is counted as that
// item's, since the item's own read lists it (readEdgeRows): a comment citing one part of an ask
// counts once for that ask. A node with no inbound edge is absent from the result; the caller
// decides what zero means.
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
	arms, ok := backlinkTargetArms(backlinkTarget{kind: toKind}, excludeKinds)
	if !ok {
		return "", false
	}
	// An ask, a comment and a message are each read with the edges at their current blocks, so
	// the count adds the arms that point at a block of the counted item, each counted under the
	// item that holds it. A block kind whose arms are not declared would count those edges low,
	// so it is refused for the same reason the counted kind's own are.
	if item := ItemKind(toKind); slices.Contains(itemKinds, item) {
		tables, _ := item.tables()
		target := backlinkTarget{kind: item.BlockKind(), blocks: tables.blocks, item: tables.item}
		atBlocks, ok := backlinkTargetArms(target, excludeKinds)
		if !ok {
			return "", false
		}
		arms = append(arms, atBlocks...)
	}
	return `select to_id, count(*)::int from (` + strings.Join(arms, " union all ") + `) edge (to_id) group by to_id`, true
}

// backlinkTargetArms is every arm of graph_edges that can point at target: the mention arms
// every node kind has and the structural arms declared for that kind, with the arms a caller's
// excludeKinds rule out left out. ok is false for a kind backlinkArms does not declare.
func backlinkTargetArms(target backlinkTarget, excludeKinds []string) ([]string, bool) {
	structural, ok := backlinkArms[target.kind]
	if !ok {
		return nil, false
	}
	arms := mentionBacklinkArms(target, excludeKinds)
	for _, arm := range structural {
		// A structural arm emits one edge kind, written as a literal, so excluding that kind
		// excludes every row the arm can produce: the arm is dropped rather than filtered.
		if slices.Contains(excludeKinds, arm.kind) {
			continue
		}
		arms = append(arms, arm.sql(target))
	}
	return arms, true
}

// backlinkTarget is what the arms built for it count: kind is the node kind their edges point
// at, written into each mention arm as a literal. The address counted is the edge's own to_id,
// unless blocks names an item's blocks table and item the column of it that names the item, when
// each arm counts the item holding the current block its edge points at instead, because that
// item's own read lists the edge (readEdgeRows).
type backlinkTarget struct {
	kind   string
	blocks string
	item   string
}

// read is how an arm whose rows point at the address toID is written for the target: the address
// the arm counts, the join that reaches it, the predicate that keys the arm on the addresses
// bound as $1, and the expression the arm's own test of the address is written over. An arm over
// an item's blocks is keyed on the item's id as text and reaches the block by its node id, the
// two columns the blocks table is indexed on, so the pair is one lookup each way whichever end
// the planner starts from; its test of the address is written over the blocks table's own node
// id, the same value, which leaves the arm's other relation keyed on the join alone.
func (t backlinkTarget) read(toID string) (counted, join, keyed, address string) {
	if t.blocks == "" {
		return toID, "", toID + ` = any($1)`, toID
	}
	counted = `part.` + t.item + `::text`
	return counted,
		` join ` + t.blocks + ` part on part.node_id = ` + toID,
		counted + ` = any($1) and part.position is not null`,
		`part.node_id`
}

// backlinkArm is one arm of graph_edges whose edge kind is a SQL literal rather than a column:
// kind is that literal, from the relations the arm reads, where the arm's own predicates and
// toID the address the view selects as its to_id. Because the kind is fixed, a caller excluding
// it excludes the whole arm. An arm is held in parts rather than as finished SQL because the
// count reads each of them two ways, keyed on the address the arm points at and keyed on the
// item whose current block that address is.
type backlinkArm struct {
	kind string
	from string
	// addressIs is the test the view's arm makes of the address itself, written as the tail of
	// a predicate over it: a reply names an ask's part and a comment's block in one column, and
	// the node id's prefix is what tells the two kinds apart. It is written over whichever
	// expression carries the address in the arm the count builds, so the same test reaches the
	// relation the arm is keyed on.
	addressIs string
	where     string
	toID      string
}

// sql is the arm as the count reads it for target, keyed on the addresses bound as $1.
func (a backlinkArm) sql(target backlinkTarget) string {
	counted, join, keyed, address := target.read(a.toID)
	where := a.where
	if a.addressIs != "" {
		if where != "" {
			where += ` and `
		}
		where += address + a.addressIs
	}
	if where != "" {
		where += ` and `
	}
	return `select ` + counted + ` from ` + a.from + join + ` where ` + where + keyed
}

// backlinkArms is, for each node kind nodeLoaders resolves, every arm of graph_edges whose edge
// kind is a literal and whose target is a node of that kind. BacklinkCounts refuses a kind that
// is no key of this table, so a node kind a newer build adds to the graph is counted only once
// its arms are declared here; "block" is present and empty because no literal-kind arm of the
// view targets a document's block, and an entry that is present but empty is counted from the
// mention arms alone rather than refused.
//
// Each arm reads only the relations it needs to reach the address it points at and leaves out
// the joins the view makes only to date an edge. Dropping them preserves every row:
// components.snapshot_id (0037 line 38), component_depends' (project_key, from_id) and
// (project_key, to_id) foreign keys (0037 lines 47-48) and issue_component_members.issue_key
// (0038 line 21) are each a not-null foreign key to the primary key the view joins, so each of
// those joins finds exactly one row.
var backlinkArms = map[string][]backlinkArm{
	"issue": {
		// issues_parent_key, partial on parent_key is not null.
		{kind: "child_of", from: `issues i`, where: `i.parent_key is not null`, toID: `i.parent_key`},
		// issue_links_target (target_key, kind).
		{kind: "blocked_by", from: `issue_links l`, where: `l.kind = 'blocked_by'`, toID: `l.target_key`},
		// artifacts_issue_key, partial on issue_key is not null.
		{kind: "attached_to", from: `artifacts a`, where: `a.issue_key is not null`, toID: `a.issue_key`},
	},
	// The view addresses an artifact by artifacts.ref_key, so each arm starts at artifacts
	// through the unique artifacts_ref_key_key and joins the table that points at it.
	"artifact": {
		// asks_anchor_artifact, on the same coalesce the join makes.
		{kind: "anchored_to", from: `artifacts a join asks k on coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = a.id`, toID: `a.ref_key`},
		// comments_anchor_artifact, on the same cast the join makes.
		{kind: "anchored_to", from: `artifacts a join comments c on (c.anchor->>'artifact_id')::uuid = a.id`, toID: `a.ref_key`},
		// asks_artifact_id.
		{kind: "owned_by", from: `artifacts a join asks k on k.artifact_id = a.id`, toID: `a.ref_key`},
		// comments_artifact_id.
		{kind: "owned_by", from: `artifacts a join comments c on c.artifact_id = a.id`, toID: `a.ref_key`},
		// The document_blocks primary key (artifact_id, block_id).
		{kind: "part_of", from: `artifacts a join document_blocks d on d.artifact_id = a.id`, toID: `a.ref_key`},
	},
	"ask": {
		// comments_ask_id_text.
		{kind: "replies_to", from: `comments c`, where: `c.ask_id is not null`, toID: `c.ask_id::text`},
		// ask_blocks_ask_id_text. The view's arm counts a current part only, so this one carries
		// its position filter.
		{kind: "part_of", from: `ask_blocks b`, where: `b.position is not null`, toID: `b.ask_id::text`},
	},
	"comment": {
		// comments_reply_to_text.
		{kind: "replies_to", from: `comments c`, where: `c.reply_to is not null`, toID: `c.reply_to::text`},
		// comment_blocks_comment_id_text. The view's arm counts a current block only, so this
		// one carries its position filter.
		{kind: "part_of", from: `comment_blocks b`, where: `b.position is not null`, toID: `b.comment_id::text`},
	},
	"message": {
		// messages_in_reply_to_text.
		{kind: "replies_to", from: `messages m`, where: `m.in_reply_to is not null`, toID: `m.in_reply_to::text`},
		// message_blocks_message_id_text, with the view's position filter.
		{kind: "part_of", from: `message_blocks b`, where: `b.position is not null`, toID: `b.message_id::text`},
	},
	"component": {
		// components_parent_node, on the same expression the arm selects.
		{kind: "part_of", from: `components c`, where: `c.parent is not null`, toID: `c.project_key || '/' || c.parent`},
		// component_depends_to_node.
		{kind: "depends_on", from: `component_depends d`, toID: `d.project_key || '/' || d.to_id`},
		// issue_component_members_node.
		{kind: "affects", from: `issue_component_members m`, toID: `m.project_key || '/' || m.component_id`},
	},
	"session": {
		// ask_followers_session_id (migration 20261101182419), which exists for this arm.
		{kind: "followed_by", from: `ask_followers f`, toID: `f.session_id`},
	},
	// No arm of the view whose edge kind is a literal targets a document's block: such a block
	// is pointed at by mentions alone, a reply naming the block of an ask, a comment or a
	// message instead. The entry is present and empty so a block is counted from its mention
	// arms rather than refused.
	"block": {},
	// A reply names the block it answers by node id, and the prefix of that id is what tells an
	// ask's part from a comment's block, so each of these two arms carries the test the view's
	// own arm carries. Both read comments_replies_to_block, partial on replies_to_block is not
	// null, and neither carries a position filter, as the view's arms do not.
	"comment_block": {
		{kind: "replies_to", from: `comments c`, where: `c.replies_to_block is not null`, addressIs: ` not like 'ask/%'`, toID: `c.replies_to_block`},
	},
	"ask_block": {
		{kind: "replies_to", from: `comments c`, where: `c.replies_to_block is not null`, addressIs: ` like 'ask/%'`, toID: `c.replies_to_block`},
	},
	"message_block": {
		// messages_replies_to_block, partial on replies_to_block is not null.
		{kind: "replies_to", from: `messages m`, where: `m.replies_to_block is not null`, toID: `m.replies_to_block`},
	},
}

// mentionBacklinkArms is the five arms of graph_edges whose edge kind and target kind are
// columns rather than literals: refs and the four tables of block references. Every node kind
// gets all five, since a mention can name any node. The target kind is written as a literal (see
// BacklinkCounts), and excludeKinds the caller's excluded edge kinds, bound as $3 - on these
// arms the edge kind is a column, so an exclusion is a predicate and never drops an arm, and an
// empty list adds nothing.
func mentionBacklinkArms(target backlinkTarget, excludeKinds []string) []string {
	exclude := ""
	if len(excludeKinds) > 0 {
		exclude = ` and not (r.kind = any($3))`
	}
	counted, join, keyed, _ := target.read(`r.to_id`)
	arm := func(relation, rest string) string {
		return `select ` + counted + ` from ` + relation + join + `
			where r.to_kind = '` + target.kind + `' and ` + keyed + rest + exclude
	}
	return []string{
		// refs_to (to_kind, to_id). heldByABlock belongs on this arm alone: it is gated on
		// kind = 'mentions' and from_kind in artifact, comment, message, ask, and refs is the
		// only arm of the view that can emit that pair, every other arm fixing from_kind as a
		// literal that is none of the four. Its exists probes carry no position filter, as the
		// view's readers do not. The source-kind filter is this arm's alone for the same reason:
		// every other arm's literal from_kind is a kind nodeLoaders resolves, so the filter would
		// drop nothing there.
		arm(`refs r`, ` and r.from_kind = any($2) and not `+heldByABlock("r")),
		// block_refs_to (to_kind, to_id). The view's own arm lists a block[] pointer's row only
		// while block_ref_listed holds, the types test a typed block's pointer carries, and a row
		// with no limit is listed without the call; this arm repeats both, so the count is the
		// rows the list shows. A pointer whose target is retyped out of its allowed types leaves
		// the count the moment it leaves the list, with no rewrite of either.
		arm(`block_refs r`, ` and (r.to_types is null or block_ref_listed(r.to_types, r.to_id))`),
		// comment_block_refs_to (to_kind, to_id, (comment_id::text)), then the comment_blocks
		// primary key for the join. The join keeps the view's position filter: the count copies
		// the view's answer, it does not correct it.
		arm(`comment_block_refs r join comment_blocks b on b.comment_id = r.comment_id and b.block_id = r.block_id`,
			` and b.position is not null`),
		// message_block_refs_to (to_kind, to_id, (message_id::text)), then the message_blocks
		// primary key for the join, with the view's position filter.
		arm(`message_block_refs r join message_blocks b on b.message_id = r.message_id and b.block_id = r.block_id`,
			` and b.position is not null`),
		// ask_block_refs_to (to_kind, to_id, (ask_id::text)), then the ask_blocks primary key for
		// the join, with the view's position filter: a link a part of an ask holds is listed
		// while that part is one of the ask's current parts.
		arm(`ask_block_refs r join ask_blocks b on b.ask_id = r.ask_id and b.block_id = r.block_id`,
			` and b.position is not null`),
	}
}
