package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Kinds is every edge type graph_edges emits.
var Kinds = []string{"mentions", "child_of", "blocked_by", "attached_to", "anchored_to", "owned_by", "replies_to", "followed_by", "part_of", "depends_on", "affects"}

// KnownKind reports whether kind is an edge type graph_edges emits.
func KnownKind(kind string) bool {
	return slices.Contains(Kinds, kind)
}

// excerptRunes bounds the head of an item's text shown beside an edge.
const excerptRunes = 240

// Query selects one node's edges. Direction "in" reads edges pointing at the node (Kind, ID
// are its to_kind, to_id: ref_key for artifacts); "out" reads edges it writes (from_kind,
// from_id: the uuid for artifacts). Kinds narrows edge types; Since keeps mentions whose
// source_seq is greater, which excludes every structural edge.
//
// A document's read also lists the edges at its current blocks: on "in", every edge pointing at
// one of them, and on "out", every link one of them holds; each such edge names that block as
// GraphEdge.Block. A comment's or a message's read does the same with the item's blocks. A
// document's, comment's or message's own mention of a target one of its blocks also cites is
// listed from that block alone (heldByABlock), so each link shows once, where its block holds it.
// A reply that names a current block of the item read is listed once, under that block: its
// replies_to edge to the item itself (a comment's thread root, the message a message answers) is
// not listed.
type Query struct {
	Direction string
	Kind      string
	ID        string
	Kinds     []string
	// ExcludeKinds drops edge kinds the caller never means by "referenced by": an ask's
	// inbound `replies_to` edges are its own clarification thread, which its card renders.
	ExcludeKinds []string
	Since        *int64
	// WholeDocuments, on a document's "in" read, lists the items and documents that cite it or
	// one of its current blocks, and no block: a citing document is listed by its own edges.
	WholeDocuments bool
}

// AskBacklinkExclusions is what "referenced by" never means for an ask: its own clarification
// thread, which every ask surface renders inline, and its own parts, each part_of it, which are
// the ask's own text. The count on a card and the rows its panel opens read the same exclusion.
var AskBacklinkExclusions = []string{"replies_to", "part_of"}

// BacklinkCounts is how many edges point at each of ids, in one grouped query: the number a
// list carries beside a row, counted the same way the rows behind it are listed. It counts only
// edges whose source is a node kind this build reads, so the count is the rows its list shows,
// and counts a document's, comment's, message's or ask's mention once for each of its blocks
// citing the node, in place of the item's own edge (heldByABlock): a comment citing a node from
// two paragraphs counts 2. An edge at a current block of a counted item is counted as that item's,
// since the item's own read lists it (readEdgeRows): a comment citing one part of an ask counts
// once for that ask. A node with no inbound edge is absent from the result; the caller decides
// what zero means.
//
// An ask is counted with its own clarification thread and its own parts excluded
// (AskBacklinkExclusions), which is also what keeps a reply that answers one of its parts from
// being counted beside the reply's edge to the ask itself.
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
	args := []any{toKind, ids, nodeKinds}
	narrow := ` and g.from_kind = any($3) and not ` + heldByABlock("g")
	if len(excludeKinds) > 0 {
		args = append(args, excludeKinds)
		narrow += " and not (g.kind = any($4))"
	}
	arms := `select g.to_id from graph_edges g where g.to_kind = $1 and g.to_id = any($2)` + narrow
	if item := ItemKind(toKind); slices.Contains(itemKinds, item) {
		tables, _ := item.tables()
		arms += ` union all
			select b.` + tables.item + `::text from graph_edges g
			join ` + tables.blocks + ` b on b.node_id = g.to_id and b.position is not null
			where g.to_kind = '` + item.BlockKind() + `' and b.` + tables.item + `::text = any($2)` + narrow
	}
	rows, err := q.Query(ctx, `select id, count(*)::int from (`+arms+`) e (id) group by id`, args...)
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

// resolvedEdge is a graph edge plus the ref_key of an artifact node, which the legacy
// referenced-by shape still reports.
type resolvedEdge struct {
	model.GraphEdge
	refKey string
}

type edgeRow struct {
	kind      string
	otherKind string
	otherID   string
	createdAt time.Time
	sourceSeq *int64
	// atBlock is the node id of the queried document's, comment's or message's block at the
	// queried end, on such a read of an edge at one of its blocks.
	atBlock *string
}

// itemKinds is every item kind whose blocks the graph reads, in the order a predicate over all
// three writes them.
var itemKinds = []ItemKind{ItemComment, ItemMessage, ItemAsk}

// heldByABlock is true of a document's, comment's, message's or ask's mention edge (graph_edges
// row alias) when a block of that item holds a link of the same kind to the same target: that
// block's own edge is listed and counted in its place, so each link shows once. An item the block
// index has not reached holds no block mention, so its own edge stays; an ask's question part
// holds the links its row holds, so the row's edge is the one hidden.
func heldByABlock(alias string) string {
	holders := []string{blockHolds(alias, "artifact", "block_refs", "artifact_id")}
	for _, item := range itemKinds {
		tables, _ := item.tables()
		holders = append(holders, blockHolds(alias, string(item), tables.refs, tables.item))
	}
	return `(` + alias + `.kind = 'mentions' and (` + strings.Join(holders, " or ") + `))`
}

// blockHolds is true of a graph_edges row alias written by an item of fromKind when one of the
// item's blocks holds a link of the row's kind to its target in table, whose column keys the item.
func blockHolds(alias, fromKind, table, column string) string {
	return `(` + alias + `.from_kind = '` + fromKind + `' and exists (
		select 1 from ` + table + ` b where b.to_kind = ` + alias + `.to_kind and b.to_id = ` + alias + `.to_id
		and b.kind = ` + alias + `.kind and b.` + column + `::text = ` + alias + `.from_id))`
}

// documentBlockIDs is the node id of every current block of the document whose ref_key is the
// numbered parameter param, the set an "in" arm reads a document's blocks' edges by. A deleted
// block has no row, so the edges at it drop out of every list until a version holds its id again.
func documentBlockIDs(param string) string {
	return `(select d.node_id from document_blocks d join artifacts a on a.id = d.artifact_id where a.ref_key = ` + param + `)`
}

// itemBlockIDs is the node id of every current block of the item of kind whose uuid is the
// numbered parameter param, the set an "in" arm reads an item's blocks' edges by. A removed block
// keeps its row with no position, and the edges at it drop out of every list until a revision
// holds its id again.
func itemBlockIDs(kind ItemKind, param string) string {
	tables, _ := kind.tables()
	return `(select b.node_id from ` + tables.blocks + ` b where b.` + tables.item + ` = ` + param + `::uuid and b.position is not null)`
}

// repliesToABlockOf is true of a graph_edges row alias that is the replies_to edge of a reply of
// kind replies naming a current block of the item of kind owner whose uuid is the numbered
// parameter param: the reply is listed under that block, by its replies_to edge to it. The two
// kinds are the same for a comment's blocks and a message's; an ask's thread is comments, so a
// reply to one of its parts is a comment.
func repliesToABlockOf(replies, owner ItemKind, alias, param string) string {
	tables, _ := replies.tables()
	return `(` + alias + `.kind = 'replies_to' and ` + alias + `.from_kind = '` + string(replies) + `' and ` + alias + `.from_id in (
		select r.id::text from ` + tables.items + ` r where r.replies_to_block in ` + itemBlockIDs(owner, param) + `))`
}

// repliesFrom is the kind of item that replies to a block of an item of kind: a comment answers a
// comment's block and an ask's part, both in comments.replies_to_block, and a message answers a
// message's block.
func repliesFrom(kind ItemKind) ItemKind {
	if kind == ItemMessage {
		return ItemMessage
	}
	return ItemComment
}

// itemRead is the item whose blocks' edges a query's read lists with its own: an ask, a comment
// or a message, named by its uuid as the item's own edges name it (canonical, lowercase), as
// every item that holds blocks is. An ask, comment or message reference whose id is no such uuid
// names no item with blocks.
func itemRead(query Query) (ItemKind, bool) {
	kind := ItemKind(query.Kind)
	if !slices.Contains(itemKinds, kind) {
		return "", false
	}
	if id, err := uuid.Parse(query.ID); err != nil || id.String() != query.ID {
		return "", false
	}
	return kind, true
}

// blockKindAt is the storage kind of the blocks a read of a node of kind lists edges at: a
// document's blocks, or an ask's, a comment's or a message's.
func blockKindAt(kind string) string {
	if item := ItemKind(kind); slices.Contains(itemKinds, item) {
		return item.BlockKind()
	}
	return "block"
}

// Edges reads a node's edges from graph_edges, newest first, with the other end of each
// resolved to a node and an excerpt of its text. An edge whose other end no longer exists is
// omitted, and so is one whose other end is a node kind this build does not know; on a
// document's read, so is one at a block that no longer exists. An edge whose other end is a
// block carries that block's stored excerpt. Document mentions carry the source's name as
// excerpt; the API replaces it with the containing block.
func Edges(ctx context.Context, q Queryer, query Query) ([]model.GraphEdge, error) {
	resolved, err := resolveEdges(ctx, q, query)
	if err != nil {
		return nil, err
	}
	edges := make([]model.GraphEdge, len(resolved))
	for index, edge := range resolved {
		edges[index] = edge.GraphEdge
	}
	return edges, nil
}

func resolveEdges(ctx context.Context, q Queryer, query Query) ([]resolvedEdge, error) {
	rows, err := readEdgeRows(ctx, q, query)
	if err != nil {
		return nil, err
	}
	lookups := rows
	atKind := blockKindAt(query.Kind)
	for _, row := range rows {
		if row.atBlock != nil {
			lookups = append(lookups, edgeRow{otherKind: atKind, otherID: *row.atBlock})
		}
	}
	nodes, err := resolveNodes(ctx, q, query.Direction == "out", lookups)
	if err != nil {
		return nil, err
	}
	edges := make([]resolvedEdge, 0, len(rows))
	for _, row := range rows {
		node, found := nodes[[2]string{row.otherKind, row.otherID}]
		if !found {
			continue
		}
		edge := resolvedEdge{
			GraphEdge: model.GraphEdge{
				Kind:      row.kind,
				Direction: query.Direction,
				Node:      node.GraphNode,
				CreatedAt: row.createdAt,
				SourceSeq: row.sourceSeq,
			},
			refKey: node.refKey,
		}
		if row.atBlock != nil {
			block, found := nodes[[2]string{atKind, *row.atBlock}]
			if !found {
				continue
			}
			edge.Block = &block.GraphNode
		}
		switch {
		case node.BlockID != "":
			// A block's stored excerpt is already cut to BlockExcerptRunes, the excerpt a
			// document's mention shows.
			edge.Excerpt = &model.GraphExcerpt{BlockID: node.BlockID, Text: node.text}
		case node.text != "":
			edge.Excerpt = &model.GraphExcerpt{Text: text.HeadRunes(node.text, excerptRunes)}
		}
		edges = append(edges, edge)
	}
	return edges, nil
}

// readEdgeRows reads the rows a query lists, as arms over graph_edges that its filters then
// narrow. A document's, comment's or message's read adds the edges at its blocks (see Query),
// and every inbound read lists an item's mention from the block holding it (heldByABlock).
func readEdgeRows(ctx context.Context, q Queryer, query Query) ([]edgeRow, error) {
	args := []any{query.Kind, query.ID}
	document := query.Kind == "artifact"
	item, itemBlocks := itemRead(query)
	if itemBlocks {
		// The item's id again, bound as the uuid its blocks are keyed by.
		args = append(args, query.ID)
	}
	var arms string
	switch {
	case query.Direction == "in":
		source := `not ` + heldByABlock("g")
		if document && query.WholeDocuments {
			// The document's citers, each by its own edges: a document's, an ask's, a comment's
			// or a message's blocks are listed by their item.
			source = `g.from_kind not in ('block', 'comment_block', 'message_block', 'ask_block')`
		}
		own := source
		if itemBlocks {
			own += ` and not ` + repliesToABlockOf(repliesFrom(item), item, "g", "$3")
		}
		arms = `select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and ` + own
		switch {
		case document:
			arms += `
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = 'block' and g.to_id in ` + documentBlockIDs("$2") + ` and ` + source
		case itemBlocks:
			arms += `
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = '` + item.BlockKind() + `' and g.to_id in ` + itemBlockIDs(item, "$3") + ` and ` + source
		}
	case query.Direction == "out" && document:
		// The document's id again, bound as the uuid block_refs keys its rows by.
		args = append(args, query.ID)
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = $1 and g.from_id = $2 and not ` + heldByABlock("g") + `
			union all
			select b.kind, b.to_kind, b.to_id, b.created_at, b.source_seq, b.from_node
			from block_refs b where b.artifact_id = $3::uuid`
	case query.Direction == "out" && itemBlocks:
		tables, _ := item.tables()
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = $1 and g.from_id = $2 and not ` + heldByABlock("g") + `
			union all
			select r.kind, r.to_kind, r.to_id, r.created_at, r.source_seq, r.from_node
			from ` + tables.refs + ` r join ` + tables.blocks + ` b on b.` + tables.item + ` = r.` + tables.item + ` and b.block_id = r.block_id
			where r.` + tables.item + ` = $3::uuid and b.position is not null`
	case query.Direction == "out":
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = $1 and g.from_id = $2`
	default:
		return nil, fmt.Errorf("reference direction %q must be in or out", query.Direction)
	}
	var sql strings.Builder
	sql.WriteString(`select kind, other_kind, other_id, created_at, source_seq, at_block from (` + arms + `)
		as e (kind, other_kind, other_id, created_at, source_seq, at_block) where true`)
	if len(query.Kinds) > 0 {
		args = append(args, query.Kinds)
		sql.WriteString(" and kind = any($" + strconv.Itoa(len(args)) + ")")
	}
	if len(query.ExcludeKinds) > 0 {
		args = append(args, query.ExcludeKinds)
		sql.WriteString(" and not (kind = any($" + strconv.Itoa(len(args)) + "))")
	}
	if query.Since != nil {
		args = append(args, *query.Since)
		sql.WriteString(" and source_seq > $" + strconv.Itoa(len(args)))
	}
	sql.WriteString(" order by created_at desc, kind, other_kind, other_id, at_block")
	rows, err := q.Query(ctx, sql.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("read graph edges: %w", err)
	}
	defer rows.Close()
	edges := []edgeRow{}
	for rows.Next() {
		var row edgeRow
		if err := rows.Scan(&row.kind, &row.otherKind, &row.otherID, &row.createdAt, &row.sourceSeq, &row.atBlock); err != nil {
			return nil, fmt.Errorf("scan graph edge: %w", err)
		}
		edges = append(edges, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate graph edges: %w", err)
	}
	return edges, nil
}

// Node resolves the node a query addresses. Artifacts are addressed by ref_key on the to side
// of an edge and by uuid on the from side; found is false when no such node exists.
func Node(ctx context.Context, q Queryer, kind, id string, byRefKey bool) (model.GraphNode, bool, error) {
	nodes, err := resolveNodes(ctx, q, byRefKey, []edgeRow{{otherKind: kind, otherID: id}})
	if err != nil {
		return model.GraphNode{}, false, err
	}
	node, found := nodes[[2]string{kind, id}]
	return node.GraphNode, found, nil
}

// resolvedNode is a graph node with the text an excerpt is cut from: a block's is its stored
// excerpt, already cut.
type resolvedNode struct {
	model.GraphNode
	text   string
	refKey string
}

// nodeLoader loads the nodes of one kind whose ids are ids into nodes. Artifact ids are uuids on
// the from side of an edge and ref_keys on the to side; byRefKey says which the ids are.
type nodeLoader func(ctx context.Context, q Queryer, kind string, byRefKey bool, ids []string, nodes map[[2]string]resolvedNode) error

// nodeLoaders resolves each node kind this build knows. These are node kinds, the ends of an
// edge, not edge kinds: a node kind a newer build adds to the graph is addressable, listed and
// counted only once it has an entry here. The keys are the kinds graph_edges stores; an ask's,
// a comment's or a message's block is stored as ask_block, comment_block or message_block and
// resolves to a node of kind block, as a document's block does.
var nodeLoaders = map[string]nodeLoader{
	"issue":         loadIssueNodes,
	"artifact":      loadArtifactNodes,
	"ask":           loadAskNodes,
	"comment":       loadCommentNodes,
	"message":       loadMessageNodes,
	"component":     loadComponentNodes,
	"session":       loadSessionNodes,
	"block":         loadBlockNodes,
	"ask_block":     loadAskBlockNodes,
	"comment_block": loadCommentBlockNodes,
	"message_block": loadMessageBlockNodes,
}

// nodeKinds is every node kind nodeLoaders resolves, sorted, as the reads that filter
// graph_edges in SQL bind it.
var nodeKinds = slices.Sorted(maps.Keys(nodeLoaders))

// resolveNodes loads every distinct other end by kind.
func resolveNodes(ctx context.Context, q Queryer, byRefKey bool, rows []edgeRow) (map[[2]string]resolvedNode, error) {
	byKind := make(map[string][]string)
	seen := make(map[[2]string]struct{})
	for _, row := range rows {
		key := [2]string{row.otherKind, row.otherID}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		byKind[row.otherKind] = append(byKind[row.otherKind], row.otherID)
	}
	nodes := make(map[[2]string]resolvedNode, len(seen))
	for kind, ids := range byKind {
		load, known := nodeLoaders[kind]
		if !known {
			// A kind this build cannot address is a node a newer build added, read while both
			// builds serve one graph_edges: its edges are omitted, as an edge whose other end no
			// longer exists is.
			continue
		}
		if err := load(ctx, q, kind, byRefKey, ids, nodes); err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

// loadSessionNodes addresses a session node by its id alone. Dispatch records no fact of a
// session: it is made by nobody recorded, never changed and belongs to no node.
func loadSessionNodes(_ context.Context, _ Queryer, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	for _, id := range ids {
		nodes[[2]string{kind, id}] = resolvedNode{GraphNode: model.GraphNode{Kind: kind, ID: id, MadeBy: []model.Actor{}, ChangedBy: []model.Actor{}}}
	}
	return nil
}

// loadIssueNodes resolves issue nodes. An issue is made by its creator; its last change is the
// latest event that wrote its own fields (issue.created, issue.updated, issue.closed: a claim, a
// deploy notice or a comment is no change of it), by that event's actor. It belongs to its parent
// issue, which has no versions.
func loadIssueNodes(ctx context.Context, q Queryer, _ string, _ bool, keys []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select i.key, i.project_key, i.title, i.created_by, i.created_at, i.parent_key, change.actor, change.created_at
		from issues i
		left join lateral (
			select e.actor, e.created_at from events e
			where e.issue_key = i.key and e.type = any('{issue.created,issue.updated,issue.closed}')
			order by e.seq desc limit 1
		) change on true
		where i.key = any($1)`, keys)
	if err != nil {
		return fmt.Errorf("load issue nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, project, title string
		var createdBy, changedBy []byte
		var createdAt time.Time
		var changedAt *time.Time
		var parentKey *string
		if err := rows.Scan(&key, &project, &title, &createdBy, &createdAt, &parentKey, &changedBy, &changedAt); err != nil {
			return fmt.Errorf("scan issue node: %w", err)
		}
		node := model.GraphNode{Kind: "issue", ID: key, IssueKey: new(key), Project: project, Ref: "dispatch://" + key, Parent: issueParent(parentKey)}
		if err := setMadeAndChanged(&node, createdBy, createdAt, changedBy, changedAt, storedActor); err != nil {
			return err
		}
		nodes[[2]string{"issue", key}] = resolvedNode{GraphNode: node, text: title}
	}
	return rows.Err()
}

// loadArtifactNodes resolves artifact nodes. An artifact is made by its creator; its last change
// is its latest version, by that version's authors. It belongs to its issue, else to the session
// whose conversation owns it, else (a project document) to no node.
func loadArtifactNodes(ctx context.Context, q Queryer, _ string, byRefKey bool, ids []string, nodes map[[2]string]resolvedNode) error {
	predicate := `a.id::text = any($1)`
	if byRefKey {
		predicate = `a.ref_key = any($1)`
	}
	rows, err := q.Query(ctx, `
		select a.id::text, a.ref_key, a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.name, a.kind, a.is_primary,
		       a.created_by, a.created_at, latest.authors, latest.created_at
		from artifacts a
		left join lateral (
			select v.authors, v.created_at from artifact_versions v
			where v.artifact_id = a.id order by v.number desc limit 1
		) latest on true
		where `+predicate, ids)
	if err != nil {
		return fmt.Errorf("load artifact nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, refKey, project, slug, name, kind string
		var issueKey, session *string
		var primary bool
		var createdBy, authors []byte
		var createdAt time.Time
		var versionAt *time.Time
		if err := rows.Scan(&id, &refKey, &issueKey, &project, &session, &slug, &name, &kind, &primary, &createdBy, &createdAt, &authors, &versionAt); err != nil {
			return fmt.Errorf("scan artifact node: %w", err)
		}
		lookup := id
		if byRefKey {
			lookup = refKey
		}
		node := model.GraphNode{Kind: "artifact", ID: id, IssueKey: issueKey, Project: project, Ref: ArtifactRef(issueKey, project, session, slug, kind, primary)}
		if err := setMadeAndChanged(&node, createdBy, createdAt, authors, versionAt, storedActors); err != nil {
			return err
		}
		switch {
		case issueKey != nil:
			node.Parent = issueParent(issueKey)
		case session != nil:
			node.Parent = &model.NodeParent{Kind: "session", ID: *session}
		}
		nodes[[2]string{"artifact", lookup}] = resolvedNode{GraphNode: node, text: name, refKey: refKey}
	}
	return rows.Err()
}

// loadBlockNodes resolves block nodes, addressed '<document ref_key>#<block id>', from the
// current blocks of their documents' latest indexed versions. A block no current version holds
// has no row and resolves to nothing, so every edge at it is omitted until a version holds its
// id again. A block is made by the authors of the version that first held it, when Dispatch knows
// that version (blockFirstVersion); its last change is its changed version, by that version's
// authors. It belongs to its document at its changed version.
func loadBlockNodes(ctx context.Context, q Queryer, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select d.node_id, d.block_id, d.excerpt, d.changed_version, d.changed_at, changed.authors,
		       `+documentColumns("a")+`, `+blockFirstColumns("first")+`
		from document_blocks d join artifacts a on a.id = d.artifact_id
		left join artifact_versions changed on changed.artifact_id = d.artifact_id and changed.number = d.changed_version
		`+blockFirstVersion("first", "d.artifact_id", "d.block_id")+`
		where d.node_id = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load block nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, blockID, excerpt string
		var changedVersion int
		var changedAt time.Time
		var changedBy []byte
		var document documentParent
		var first blockFirst
		targets := append([]any{&nodeID, &blockID, &excerpt, &changedVersion, &changedAt, &changedBy}, document.targets()...)
		if err := rows.Scan(append(targets, first.targets()...)...); err != nil {
			return fmt.Errorf("scan block node: %w", err)
		}
		changers, err := storedActors(changedBy)
		if err != nil {
			return fmt.Errorf("decode the last change of block %s: %w", nodeID, err)
		}
		parent := document.parent()
		node := model.GraphNode{
			Kind:           "block",
			ID:             nodeID,
			BlockID:        blockID,
			IssueKey:       document.issueKey,
			Project:        document.project,
			Ref:            BlockRef(parent.Ref, blockID),
			Document:       parent.Ref,
			ChangedVersion: changedVersion,
			ChangedBy:      changers,
			ChangedAt:      &changedAt,
			Parent:         parent,
			ParentVersion:  &changedVersion,
		}
		if node.MadeBy, node.MadeAt, err = first.maker(); err != nil {
			return fmt.Errorf("decode the maker of block %s: %w", nodeID, err)
		}
		nodes[[2]string{"block", nodeID}] = resolvedNode{GraphNode: node, text: excerpt}
	}
	return rows.Err()
}

// BlockRef is the reference to block blockID of the document whose reference is document:
// `<document>#b-<text.EncodeBlockID(blockID)>`. A document with no address has none.
func BlockRef(document, blockID string) string {
	if document == "" {
		return ""
	}
	return document + "#b-" + text.EncodeBlockID(blockID)
}

// anchorVersion is the version an item's anchor (alias.anchor) names, null when it names none a
// version number can be: the anchor is written by the server, and the read never fails on it.
func anchorVersion(alias string) string {
	return `case when ` + alias + `.anchor->>'version' ~ '^[1-9][0-9]{0,8}$' then (` + alias + `.anchor->>'version')::int end`
}

// loadAskNodes resolves ask nodes. An ask is made by its author. Its last change is its own edit
// (edited_at: its wording, a block ask's settlement, an approval request's version move), by the
// editor the latest ask.edited event names; an answer or a resolution is no change of it. It
// belongs to the document it is written on: the one its anchor names, at the anchored version; a
// block ask's document, at the version that first held its block when Dispatch knows it
// (blockFirstVersion); an approval request's document, at the version it named when it opened
// (written_version); the document that owns it, at the version current when it was written
// (written_version). An ask written on no document belongs to its issue.
//
// The edit lookup repeats events_ask_payload_id's type list (migration 0030), so the planner can
// prove the partial index serves it.
func loadAskNodes(ctx context.Context, q Queryer, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select k.id::text, k.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), k.question,
		       k.author, k.created_at, k.edited_at, edit.edited_by, `+documentColumns("doc")+`,
		       case
		         when k.anchor->>'artifact_id' is not null then `+anchorVersion("k")+`
		         when k.block_artifact_id is not null then case when first.known then first.version end
		         else k.written_version
		       end
		from asks k
		left join issues i on i.key = k.issue_key
		left join artifacts a on a.id = k.artifact_id
		left join artifacts doc on doc.id = case
		  when k.anchor->>'artifact_id' is not null then (k.anchor->>'artifact_id')::uuid
		  when k.block_artifact_id is not null then k.block_artifact_id
		  when k.kind = 'approval' then (k.approval->>'artifact_id')::uuid
		  else k.artifact_id
		end
		left join lateral (
			select e.payload->'edited_by' as edited_by from events e
			where e.type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited') and e.type = 'ask.edited'
			  and e.payload->>'id' = k.id::text
			order by e.id desc limit 1
		) edit on k.edited_at is not null
		`+blockFirstVersion("first", "k.block_artifact_id", "k.block_id")+`
		where k.id::text = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load ask nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, slug, question string
		var issueKey *string
		var author, editedBy []byte
		var createdAt time.Time
		var editedAt *time.Time
		var document documentParent
		var version *int
		targets := append([]any{&id, &issueKey, &project, &slug, &question, &author, &createdAt, &editedAt, &editedBy}, document.targets()...)
		if err := rows.Scan(append(targets, &version)...); err != nil {
			return fmt.Errorf("scan ask node: %w", err)
		}
		node := model.GraphNode{Kind: "ask", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("ask", issueKey, project, slug, id)}
		if err := setMadeAndChanged(&node, author, createdAt, editedBy, editedAt, storedActor); err != nil {
			return err
		}
		node.Parent = issueParent(issueKey)
		if parent := document.parent(); parent != nil {
			node.Parent, node.ParentVersion = parent, version
		}
		nodes[[2]string{"ask", id}] = resolvedNode{GraphNode: node, text: question}
	}
	return rows.Err()
}

// loadCommentNodes resolves comment nodes. A comment is made by its author, and only its author
// edits it, so its last change is its author at edited_at; a resolution is no change of it. A
// reply that names a block it answers belongs to that block, another reply to its thread's head
// comment, a comment in an ask's thread to the ask, and another comment to the document it is
// written on: the one its anchor names, at the anchored version, else the document that owns it,
// at the version current when it was written (written_version). A comment written on no document
// belongs to its issue. A reply shares its head's owner, and so the owner its reference, and the
// reference of the block it names, are written under.
func loadCommentNodes(ctx context.Context, q Queryer, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select c.id::text, c.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), c.body,
		       c.author, c.created_at, c.edited_at, c.reply_to::text, c.replies_to_block, c.ask_id::text, `+documentColumns("doc")+`,
		       case when c.anchor->>'artifact_id' is not null then `+anchorVersion("c")+` else c.written_version end
		from comments c
		left join issues i on i.key = c.issue_key
		left join artifacts a on a.id = c.artifact_id
		left join artifacts doc on doc.id = coalesce((c.anchor->>'artifact_id')::uuid, c.artifact_id)
		where c.id::text = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load comment nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, slug, body string
		var issueKey, replyTo, replyBlock, askID *string
		var author []byte
		var createdAt time.Time
		var editedAt *time.Time
		var document documentParent
		var version *int
		targets := append([]any{&id, &issueKey, &project, &slug, &body, &author, &createdAt, &editedAt, &replyTo, &replyBlock, &askID}, document.targets()...)
		if err := rows.Scan(append(targets, &version)...); err != nil {
			return fmt.Errorf("scan comment node: %w", err)
		}
		node := model.GraphNode{Kind: "comment", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("comment", issueKey, project, slug, id)}
		if err := setMadeAndChanged(&node, author, createdAt, author, editedAt, storedActor); err != nil {
			return err
		}
		switch parent := document.parent(); {
		case replyBlock != nil:
			node.Parent = itemBlockParent(issueKey, project, slug, *replyBlock)
		case replyTo != nil:
			node.Parent = itemParent("comment", issueKey, project, slug, *replyTo)
		case askID != nil:
			node.Parent = itemParent("ask", issueKey, project, slug, *askID)
		case parent != nil:
			node.Parent, node.ParentVersion = parent, version
		default:
			node.Parent = issueParent(issueKey)
		}
		nodes[[2]string{"comment", id}] = resolvedNode{GraphNode: node, text: body}
	}
	return rows.Err()
}

// loadMessageNodes resolves message nodes. A message is made by its author and no route edits
// it. A reply that names a block it answers belongs to that block, another reply to the message
// it answers, which shares its issue, and another message to its issue; a direct message belongs
// to no node.
func loadMessageNodes(ctx context.Context, q Queryer, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select m.id::text, m.issue_key, coalesce(i.project_key, ''), m.body, m.author, m.created_at, m.in_reply_to::text, m.replies_to_block
		from messages m
		left join issues i on i.key = m.issue_key
		where m.id::text = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load message nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, body string
		var issueKey, inReplyTo, replyBlock *string
		var author []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &issueKey, &project, &body, &author, &createdAt, &inReplyTo, &replyBlock); err != nil {
			return fmt.Errorf("scan message node: %w", err)
		}
		node := model.GraphNode{Kind: "message", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("message", issueKey, project, "", id)}
		if err := setMadeAndChanged(&node, author, createdAt, nil, nil, storedActor); err != nil {
			return err
		}
		switch {
		case replyBlock != nil:
			node.Parent = itemBlockParent(issueKey, project, "", *replyBlock)
		case inReplyTo != nil:
			node.Parent = itemParent("message", issueKey, project, "", *inReplyTo)
		default:
			node.Parent = issueParent(issueKey)
		}
		nodes[[2]string{"message", id}] = resolvedNode{GraphNode: node, text: body}
	}
	return rows.Err()
}

// loadComponentNodes resolves component nodes addressed '<project>/<id>' from the current
// model. A component a re-import retired resolves to nothing, so edges at it are omitted like
// any edge whose other end no longer exists. Dispatch does not import who made or changed a
// component; component_facts records when, and a component with no row there, or one whose
// times were never recorded, has none. A component belongs to its parent component while the
// model holds it.
func loadComponentNodes(ctx context.Context, q Queryer, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select c.project_key, c.id, c.title, f.made_at, f.changed_at, p.id
		from components c
		left join component_facts f on f.project_key = c.project_key and f.id = c.id
		left join components p on p.project_key = c.project_key and p.id = c.parent
		where c.project_key || '/' || c.id = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load component nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project, id, title string
		var madeAt, changedAt *time.Time
		var parent *string
		if err := rows.Scan(&project, &id, &title, &madeAt, &changedAt, &parent); err != nil {
			return fmt.Errorf("scan component node: %w", err)
		}
		address := project + "/" + id
		node := model.GraphNode{
			Kind: "component", ID: address, Project: project, Ref: componentRef(project, id),
			MadeBy: []model.Actor{}, MadeAt: madeAt, ChangedBy: []model.Actor{}, ChangedAt: changedAt,
		}
		if parent != nil {
			node.Parent = &model.NodeParent{Kind: "component", ID: project + "/" + *parent, Ref: componentRef(project, *parent)}
		}
		nodes[[2]string{"component", address}] = resolvedNode{GraphNode: node, text: title}
	}
	return rows.Err()
}

// componentRef is the reference to component id of project.
func componentRef(project, id string) string {
	return "dispatch://" + project + "/component/" + id
}

// issueParent is issue key as a node's parent, or nil for no issue.
func issueParent(key *string) *model.NodeParent {
	if key == nil {
		return nil
	}
	return &model.NodeParent{Kind: "issue", ID: *key, Ref: "dispatch://" + *key}
}

// itemParent is the ask, comment, or message id, addressed as itemRef addresses it, as a node's parent.
func itemParent(kind string, issueKey *string, project, slug, id string) *model.NodeParent {
	return &model.NodeParent{Kind: kind, ID: id, Ref: itemRef(kind, issueKey, project, slug, id)}
}

// documentColumns selects, from the artifacts row aliased alias, what a document parent's node
// id and reference are made of, in documentParent.targets' order.
func documentColumns(alias string) string {
	return alias + `.id::text, ` + alias + `.issue_key, coalesce(` + alias + `.project_key, ''), ` +
		alias + `.session_id, ` + alias + `.slug, ` + alias + `.kind, ` + alias + `.is_primary`
}

// documentParent is the document a node belongs to, as documentColumns selects it; a left join
// that found no document leaves id nil.
type documentParent struct {
	id, issueKey, session, slug, kind *string
	project                           string
	primary                           *bool
}

func (d *documentParent) targets() []any {
	return []any{&d.id, &d.issueKey, &d.project, &d.session, &d.slug, &d.kind, &d.primary}
}

// parent is the document as a node's parent: its uuid and its reference, absent when it has
// none; nil when there is no document.
func (d documentParent) parent() *model.NodeParent {
	if d.id == nil {
		return nil
	}
	return &model.NodeParent{Kind: "artifact", ID: *d.id, Ref: ArtifactRef(d.issueKey, d.project, d.session, *d.slug, *d.kind, *d.primary)}
}

// storedActor decodes a single-actor column (a creator, an author, an event's actor, an edit's
// editor) as a node's actors, a list of that one actor. SQL NULL (a left join that found no row,
// an edit that names no editor) and JSON null are nobody recorded, the empty list; any other value
// that does not decode as a model.Actor fails the read.
func storedActor(raw []byte) ([]model.Actor, error) {
	var actor *model.Actor
	if raw != nil {
		if err := json.Unmarshal(raw, &actor); err != nil {
			return nil, fmt.Errorf("decode actor: %w", err)
		}
	}
	if actor == nil {
		return []model.Actor{}, nil
	}
	return []model.Actor{*actor}, nil
}

// storedActors decodes a list column (a version's authors) as a node's actors. SQL NULL (a left
// join that found no version) and JSON null are nobody recorded, the empty list, as is an empty
// list; any other value that does not decode as a list of model.Actor fails the read.
func storedActors(raw []byte) ([]model.Actor, error) {
	var actors []model.Actor
	if raw != nil {
		if err := json.Unmarshal(raw, &actors); err != nil {
			return nil, fmt.Errorf("decode actors: %w", err)
		}
	}
	if actors == nil {
		return []model.Actor{}, nil
	}
	return actors, nil
}

// setMadeAndChanged sets node's maker, the single-actor column madeBy (storedActor), and its last
// change, which is the maker until changedAt is set and then changedBy as decodeChanged reads it
// (storedActor or storedActors). A column that does not decode fails, naming node's kind and id.
func setMadeAndChanged(node *model.GraphNode, madeBy []byte, madeAt time.Time, changedBy []byte, changedAt *time.Time, decodeChanged func([]byte) ([]model.Actor, error)) error {
	makers, err := storedActor(madeBy)
	if err != nil {
		return fmt.Errorf("decode the maker of %s %s: %w", node.Kind, node.ID, err)
	}
	node.MadeBy, node.MadeAt = makers, &madeAt
	node.ChangedBy, node.ChangedAt = node.MadeBy, node.MadeAt
	if changedAt != nil {
		changers, err := decodeChanged(changedBy)
		if err != nil {
			return fmt.Errorf("decode the last change of %s %s: %w", node.Kind, node.ID, err)
		}
		node.ChangedBy, node.ChangedAt = changers, changedAt
	}
	return nil
}

// ArtifactRef is the address the dashboard copies for a document: an issue's primary document
// is its spec, another issue artifact sits under artifact/<slug>, a project document under
// its project, and an agent's conversation's file under agent/<session id>.
func ArtifactRef(issueKey *string, project string, session *string, slug, kind string, primary bool) string {
	if session != nil {
		return "dispatch://agent/" + *session + "/artifact/" + slug
	}
	if issueKey == nil {
		if project == "" {
			// The row's owner is one a later schema adds (LEGION-541); main names no address for it.
			return ""
		}
		return "dispatch://" + project + "/artifact/" + slug
	}
	if primary && kind == "doc" {
		return "dispatch://" + *issueKey + "/spec"
	}
	return "dispatch://" + *issueKey + "/artifact/" + slug
}

// itemRef addresses an ask, comment, or message: under its issue, or under the project document
// that owns it. An issue-less message has no address, nor has an item on an artifact with no
// project, whose owner is one a later schema adds (LEGION-541).
func itemRef(kind string, issueKey *string, project, slug, id string) string {
	if issueKey != nil {
		return "dispatch://" + *issueKey + "/" + kind + "/" + id
	}
	if slug == "" || project == "" {
		return ""
	}
	return "dispatch://" + project + "/artifact/" + slug + "/" + kind + "/" + id
}
