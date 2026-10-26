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
type Query struct {
	Direction string
	Kind      string
	ID        string
	Kinds     []string
	// ExcludeKinds drops edge kinds the caller never means by "referenced by": an ask's
	// inbound `replies_to` edges are its own clarification thread, which its card renders.
	ExcludeKinds []string
	Since        *int64
}

// AskBacklinkExclusions is what "referenced by" never means for an ask: its own clarification
// thread, which every ask surface renders inline. The count on a card and the rows its panel
// opens read the same exclusion.
var AskBacklinkExclusions = []string{"replies_to"}

// inboundArm is one arm of graph_edges whose to side can be a given node kind: the edge kind
// the arm emits, and the SQL listing the arm's to_id for each id of $1. Each arm reads the
// index named beside it. BacklinkCounts reads these arms rather than the view because the view
// has fifteen arms and its to_kind is a parameter: every call plans all fifteen, about 1 ms of
// planning each time a prepared statement takes a custom plan (the plan cache's default for this
// statement), where the arms of one kind plan in about a fifth of that.
type inboundArm struct {
	kind string
	sql  string
}

// mentionsArm is the arm every node kind has: the edges derived from text, which refs stores
// addressed exactly as the view reads them, less a source of a node kind this build does not
// read ($3, nodeKinds). Index refs_to (to_kind, to_id). It is the one arm that filter can
// drop a row from: every structural arm's source is a fixed kind nodeLoaders resolves.
const mentionsArm = `select to_id from refs where to_kind = $2 and to_id = any($1) and from_kind = any($3)`

// inboundArms is, per node kind, every structural arm of graph_edges whose to side is that
// kind, read off the view's own definition (migration 0094, which re-states 0038's arms). A
// node kind absent from the map is refused rather than counted low. store's equivalence test
// compares every kind's count with the view's, and fails when the view gains an arm, so an arm
// added to the view is added here in the same change. A build older than that change does not
// read the new arm, so while it still serves beside the new schema (a rolling deploy or a
// rollback) its count is low by the new arm's edges, which its list, reading the view, shows.
//
// Each arm selects only the address it is keyed on, so the joins the view makes for an edge's
// created_at are left out: components.snapshot_id, component_depends' two component keys and
// issue_component_members.issue_key are each a not-null foreign key to a primary key, which
// neither drops a row nor doubles one.
var inboundArms = map[string][]inboundArm{
	"issue": {
		// Index issues_parent_key, partial on parent_key is not null.
		{"child_of", `select parent_key as to_id from issues where parent_key is not null and parent_key = any($1)`},
		// Index issue_links_target (target_key, kind).
		{"blocked_by", `select target_key as to_id from issue_links where kind = 'blocked_by' and target_key = any($1)`},
		// Index artifacts_issue_key, partial on issue_key is not null.
		{"attached_to", `select issue_key as to_id from artifacts where issue_key is not null and issue_key = any($1)`},
	},
	"artifact": {
		// An ask's block or anchor artifact. Indexes artifacts_ref_key_key, asks_anchor_artifact.
		{"anchored_to", `select a.ref_key as to_id from artifacts a
			join asks k on coalesce(k.block_artifact_id, (k.anchor->>'artifact_id')::uuid) = a.id
			where a.ref_key = any($1)`},
		// A comment's anchor artifact. Indexes artifacts_ref_key_key, comments_anchor_artifact.
		{"anchored_to", `select a.ref_key as to_id from artifacts a
			join comments c on (c.anchor->>'artifact_id')::uuid = a.id
			where a.ref_key = any($1)`},
		// A project document's own asks. Indexes artifacts_ref_key_key, asks_artifact_id.
		{"owned_by", `select a.ref_key as to_id from artifacts a
			join asks k on k.artifact_id = a.id
			where a.ref_key = any($1)`},
		// A project document's own comments. Indexes artifacts_ref_key_key, comments_artifact_id.
		{"owned_by", `select a.ref_key as to_id from artifacts a
			join comments c on c.artifact_id = a.id
			where a.ref_key = any($1)`},
	},
	"ask": {
		// The ask's clarification thread. Index comments_ask_id_text.
		{"replies_to", `select ask_id::text as to_id from comments where ask_id is not null and ask_id::text = any($1)`},
	},
	"comment": {
		// A comment's replies. Index comments_reply_to_text.
		{"replies_to", `select reply_to::text as to_id from comments where reply_to is not null and reply_to::text = any($1)`},
	},
	"message": {
		// A message's replies. Index messages_in_reply_to_text.
		{"replies_to", `select in_reply_to::text as to_id from messages where in_reply_to is not null and in_reply_to::text = any($1)`},
	},
	"session": {
		// The asks a session follows. Index ask_followers_session_id (migration 0117).
		{"followed_by", `select session_id as to_id from ask_followers where session_id = any($1)`},
	},
	"component": {
		// A component's children. Index components_parent_node.
		{"part_of", `select project_key || '/' || parent as to_id from components
			where parent is not null and project_key || '/' || parent = any($1)`},
		// The components that depend on it. Index component_depends_to_node.
		{"depends_on", `select project_key || '/' || to_id as to_id from component_depends
			where project_key || '/' || to_id = any($1)`},
		// The issues that name it explicitly. Index issue_component_members_node.
		{"affects", `select project_key || '/' || component_id as to_id from issue_component_members
			where project_key || '/' || component_id = any($1)`},
	},
}

// BacklinkCounts is how many edges point at each of ids, in one grouped query: the number a
// list carries beside a row, counted the same way the rows behind it are listed. It counts only
// edges whose source is a node kind this build reads, so the count is the rows its list shows.
// A node with no inbound edge is absent from the result; the caller decides what zero means. It
// unions the arms of graph_edges that can point at toKind, each read through its own index,
// rather than planning the whole view; the counts are the view's, which store's equivalence test
// holds it to.
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
	arms, addressable := inboundArms[toKind]
	if !addressable {
		return nil, fmt.Errorf("count %s backlinks: graph node kind %q is not addressable", toKind, toKind)
	}
	args := []any{ids, toKind, nodeKinds}
	mentions := mentionsArm
	if len(excludeKinds) > 0 {
		args = append(args, excludeKinds)
		mentions += ` and not (kind = any($4))`
	}
	parts := []string{mentions}
	for _, arm := range arms {
		if slices.Contains(excludeKinds, arm.kind) {
			continue
		}
		parts = append(parts, arm.sql)
	}
	sql := `select to_id, count(*)::int from (` + strings.Join(parts, " union all ") + `) edge group by to_id`
	rows, err := q.Query(ctx, sql, args...)
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
}

// Edges reads a node's edges from graph_edges, newest first, with the other end of each
// resolved to a node and an excerpt of its text. An edge whose other end no longer exists is
// omitted, and so is one whose other end is a node kind this build does not know. Document
// mentions carry the source's name as excerpt; the API replaces it with the containing block.
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
	nodes, err := resolveNodes(ctx, q, query.Direction == "out", rows)
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
		if node.text != "" {
			edge.Excerpt = &model.GraphExcerpt{Text: text.HeadRunes(node.text, excerptRunes)}
		}
		edges = append(edges, edge)
	}
	return edges, nil
}

func readEdgeRows(ctx context.Context, q Queryer, query Query) ([]edgeRow, error) {
	var sql strings.Builder
	args := []any{query.Kind, query.ID}
	switch query.Direction {
	case "in":
		sql.WriteString(`select kind, from_kind, from_id, created_at, source_seq from graph_edges where to_kind = $1 and to_id = $2`)
	case "out":
		sql.WriteString(`select kind, to_kind, to_id, created_at, source_seq from graph_edges where from_kind = $1 and from_id = $2`)
	default:
		return nil, fmt.Errorf("reference direction %q must be in or out", query.Direction)
	}
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
	sql.WriteString(" order by created_at desc, kind, from_kind, from_id, to_kind, to_id")
	rows, err := q.Query(ctx, sql.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("read graph edges: %w", err)
	}
	defer rows.Close()
	edges := []edgeRow{}
	for rows.Next() {
		var row edgeRow
		if err := rows.Scan(&row.kind, &row.otherKind, &row.otherID, &row.createdAt, &row.sourceSeq); err != nil {
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

// resolvedNode is a graph node with the text an excerpt is cut from.
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
}

// nodeKinds is every node kind nodeLoaders resolves, sorted, as the reads that filter
// graph_edges in SQL bind it.
var nodeKinds = slices.Sorted(maps.Keys(nodeLoaders))

// NodeKinds is a copy of nodeKinds, for a test outside this package that holds a count to the
// view's answer over the same kinds.
func NodeKinds() []string { return slices.Clone(nodeKinds) }

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
