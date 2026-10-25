package refs

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

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
// GraphEdge.Block. A document's own mention of a target one of its blocks also cites is listed
// from that block alone (heldByABlock), so each link shows once, where its block holds it.
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
// thread, which every ask surface renders inline. The count on a card and the rows its panel
// opens read the same exclusion.
var AskBacklinkExclusions = []string{"replies_to"}

// BacklinkCounts is how many edges point at each of ids, in one grouped query: the number a
// list carries beside a row, counted the same way the rows behind it are listed. It counts only
// edges whose source is a node kind this build reads, so the count is the rows its list shows,
// and counts a document's mention once for each of its blocks citing the node, in place of the
// document's own edge (heldByABlock). A node with no inbound edge is absent from the result; the
// caller decides what zero means.
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
	sql := `select g.to_id, count(*)::int from graph_edges g
		where g.to_kind = $1 and g.to_id = any($2) and g.from_kind = any($3) and not ` + heldByABlock("g")
	args := []any{toKind, ids, nodeKinds}
	if len(excludeKinds) > 0 {
		args = append(args, excludeKinds)
		sql += " and not (g.kind = any($4))"
	}
	rows, err := q.Query(ctx, sql+" group by g.to_id", args...)
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
	// atBlock is the node id of the queried document's block at the queried end, on a
	// document's read of an edge at one of its blocks.
	atBlock *string
}

// heldByABlock is true of a document's mention edge (graph_edges row alias) when a block of that
// document holds a link of the same kind to the same target: that block's own edge is listed and
// counted in its place, so each link shows once. A document the block index has not reached
// holds no block_refs row, so its own edge stays.
func heldByABlock(alias string) string {
	return `(` + alias + `.from_kind = 'artifact' and ` + alias + `.kind = 'mentions' and exists (
		select 1 from block_refs b where b.to_kind = ` + alias + `.to_kind and b.to_id = ` + alias + `.to_id
		and b.kind = ` + alias + `.kind and b.artifact_id::text = ` + alias + `.from_id))`
}

// documentBlockIDs is the node id of every current block of the document whose ref_key is the
// numbered parameter param, the set an "in" arm reads a document's blocks' edges by. A deleted
// block has no row, so the edges at it drop out of every list until a version holds its id again.
func documentBlockIDs(param string) string {
	return `(select d.node_id from document_blocks d join artifacts a on a.id = d.artifact_id where a.ref_key = ` + param + `)`
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
	for _, row := range rows {
		if row.atBlock != nil {
			lookups = append(lookups, edgeRow{otherKind: "block", otherID: *row.atBlock})
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
			block, found := nodes[[2]string{"block", *row.atBlock}]
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
// narrow. A document's read adds the edges at its blocks (see Query), and every inbound read
// lists a document's mention from the block holding it (heldByABlock).
func readEdgeRows(ctx context.Context, q Queryer, query Query) ([]edgeRow, error) {
	args := []any{query.Kind, query.ID}
	document := query.Kind == "artifact"
	var arms string
	switch {
	case query.Direction == "in":
		source := `not ` + heldByABlock("g")
		if document && query.WholeDocuments {
			source = `g.from_kind <> 'block'`
		}
		arms = `select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.to_kind = $1 and g.to_id = $2 and ` + source
		if document {
			arms += `
			union all
			select g.kind, g.from_kind, g.from_id, g.created_at, g.source_seq, g.to_id
			from graph_edges g where g.to_kind = 'block' and g.to_id in ` + documentBlockIDs("$2") + ` and ` + source
		}
	case query.Direction == "out" && document:
		// The document's id again, bound as the uuid block_refs keys its rows by.
		args = append(args, query.ID)
		arms = `select g.kind, g.to_kind, g.to_id, g.created_at, g.source_seq, null::text
			from graph_edges g where g.from_kind = $1 and g.from_id = $2 and not ` + heldByABlock("g") + `
			union all
			select b.kind, b.to_kind, b.to_id, b.created_at, b.source_seq, b.from_node
			from block_refs b where b.artifact_id = $3::uuid`
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
// counted only once it has an entry here.
var nodeLoaders = map[string]nodeLoader{
	"issue":     loadIssueNodes,
	"artifact":  loadArtifactNodes,
	"ask":       loadItemNodes,
	"comment":   loadItemNodes,
	"message":   loadMessageNodes,
	"component": loadComponentNodes,
	"session":   loadSessionNodes,
	"block":     loadBlockNodes,
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

// loadSessionNodes addresses a session node by its id alone.
func loadSessionNodes(_ context.Context, _ Queryer, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	for _, id := range ids {
		nodes[[2]string{kind, id}] = resolvedNode{GraphNode: model.GraphNode{Kind: kind, ID: id}}
	}
	return nil
}

func loadIssueNodes(ctx context.Context, q Queryer, _ string, _ bool, keys []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `select key, project_key, title from issues where key = any($1)`, keys)
	if err != nil {
		return fmt.Errorf("load issue nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, project, title string
		if err := rows.Scan(&key, &project, &title); err != nil {
			return fmt.Errorf("scan issue node: %w", err)
		}
		nodes[[2]string{"issue", key}] = resolvedNode{
			GraphNode: model.GraphNode{Kind: "issue", ID: key, IssueKey: new(key), Project: project, Ref: "dispatch://" + key},
			text:      title,
		}
	}
	return rows.Err()
}

func loadArtifactNodes(ctx context.Context, q Queryer, _ string, byRefKey bool, ids []string, nodes map[[2]string]resolvedNode) error {
	predicate := `id::text = any($1)`
	if byRefKey {
		predicate = `ref_key = any($1)`
	}
	rows, err := q.Query(ctx, `
		select id::text, ref_key, issue_key, coalesce(project_key, ''), session_id, slug, name, kind, is_primary
		from artifacts where `+predicate, ids)
	if err != nil {
		return fmt.Errorf("load artifact nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, refKey, project, slug, name, kind string
		var issueKey, session *string
		var primary bool
		if err := rows.Scan(&id, &refKey, &issueKey, &project, &session, &slug, &name, &kind, &primary); err != nil {
			return fmt.Errorf("scan artifact node: %w", err)
		}
		lookup := id
		if byRefKey {
			lookup = refKey
		}
		nodes[[2]string{"artifact", lookup}] = resolvedNode{
			GraphNode: model.GraphNode{Kind: "artifact", ID: id, IssueKey: issueKey, Project: project, Ref: ArtifactRef(issueKey, project, session, slug, kind, primary)},
			text:      name,
			refKey:    refKey,
		}
	}
	return rows.Err()
}

// loadBlockNodes resolves block nodes, addressed '<document ref_key>#<block id>', from the
// current blocks of their documents' latest indexed versions. A block no current version holds
// has no row and resolves to nothing, so every edge at it is omitted until a version holds its
// id again.
func loadBlockNodes(ctx context.Context, q Queryer, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select d.node_id, d.block_id, d.excerpt, d.changed_version, d.changed_at,
		       a.issue_key, coalesce(a.project_key, ''), a.session_id, a.slug, a.kind, a.is_primary
		from document_blocks d join artifacts a on a.id = d.artifact_id
		where d.node_id = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load block nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, blockID, excerpt, project, slug, kind string
		var changedVersion int
		var changedAt time.Time
		var issueKey, session *string
		var primary bool
		if err := rows.Scan(&nodeID, &blockID, &excerpt, &changedVersion, &changedAt, &issueKey, &project, &session, &slug, &kind, &primary); err != nil {
			return fmt.Errorf("scan block node: %w", err)
		}
		document := ArtifactRef(issueKey, project, session, slug, kind, primary)
		nodes[[2]string{"block", nodeID}] = resolvedNode{
			GraphNode: model.GraphNode{
				Kind:           "block",
				ID:             nodeID,
				BlockID:        blockID,
				IssueKey:       issueKey,
				Project:        project,
				Ref:            BlockRef(document, blockID),
				Document:       document,
				ChangedVersion: changedVersion,
				ChangedAt:      &changedAt,
			},
			text: excerpt,
		}
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

func loadItemNodes(ctx context.Context, q Queryer, kind string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	table, column := "asks", "question"
	if kind == "comment" {
		table, column = "comments", "body"
	}
	rows, err := q.Query(ctx, `
		select k.id::text, k.issue_key, coalesce(i.project_key, a.project_key, ''), coalesce(a.slug, ''), k.`+column+`
		from `+table+` k
		left join issues i on i.key = k.issue_key
		left join artifacts a on a.id = k.artifact_id
		where k.id::text = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load %s nodes: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, slug, body string
		var issueKey *string
		if err := rows.Scan(&id, &issueKey, &project, &slug, &body); err != nil {
			return fmt.Errorf("scan %s node: %w", kind, err)
		}
		nodes[[2]string{kind, id}] = resolvedNode{
			GraphNode: model.GraphNode{Kind: kind, ID: id, IssueKey: issueKey, Project: project, Ref: itemRef(kind, issueKey, project, slug, id)},
			text:      body,
		}
	}
	return rows.Err()
}

func loadMessageNodes(ctx context.Context, q Queryer, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select m.id::text, m.issue_key, coalesce(i.project_key, ''), m.body
		from messages m
		left join issues i on i.key = m.issue_key
		where m.id::text = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load message nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project, body string
		var issueKey *string
		if err := rows.Scan(&id, &issueKey, &project, &body); err != nil {
			return fmt.Errorf("scan message node: %w", err)
		}
		nodes[[2]string{"message", id}] = resolvedNode{
			GraphNode: model.GraphNode{Kind: "message", ID: id, IssueKey: issueKey, Project: project, Ref: itemRef("message", issueKey, project, "", id)},
			text:      body,
		}
	}
	return rows.Err()
}

// loadComponentNodes resolves component nodes addressed '<project>/<id>' from the current
// model. A component a re-import retired resolves to nothing, so edges at it are omitted like
// any edge whose other end no longer exists.
func loadComponentNodes(ctx context.Context, q Queryer, _ string, _ bool, ids []string, nodes map[[2]string]resolvedNode) error {
	rows, err := q.Query(ctx, `
		select project_key, id, title from components
		where project_key || '/' || id = any($1)`, ids)
	if err != nil {
		return fmt.Errorf("load component nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project, id, title string
		if err := rows.Scan(&project, &id, &title); err != nil {
			return fmt.Errorf("scan component node: %w", err)
		}
		address := project + "/" + id
		nodes[[2]string{"component", address}] = resolvedNode{
			GraphNode: model.GraphNode{Kind: "component", ID: address, Project: project, Ref: "dispatch://" + project + "/component/" + id},
			text:      title,
		}
	}
	return rows.Err()
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
