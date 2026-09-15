package refs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Kinds is every edge type graph_edges emits.
var Kinds = []string{"mentions", "child_of", "attached_to", "anchored_to", "owned_by", "replies_to", "followed_by"}

// KnownKind reports whether kind is an edge type graph_edges emits.
func KnownKind(kind string) bool {
	for _, known := range Kinds {
		if known == kind {
			return true
		}
	}
	return false
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
	Since     *int64
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
// omitted. Document mentions carry the source's name as excerpt; the API replaces it with the
// containing block.
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
			edge.Excerpt = &model.GraphExcerpt{Text: headRunes(node.text, excerptRunes)}
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

// resolveNodes loads every distinct other end by kind. Artifact ids are uuids on the from side
// of an edge and ref_keys on the to side; byRefKey says which the ids are.
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
		var err error
		switch kind {
		case "issue":
			err = loadIssueNodes(ctx, q, ids, nodes)
		case "artifact":
			err = loadArtifactNodes(ctx, q, byRefKey, ids, nodes)
		case "ask", "comment":
			err = loadItemNodes(ctx, q, kind, ids, nodes)
		case "message":
			err = loadMessageNodes(ctx, q, ids, nodes)
		case "session":
			for _, id := range ids {
				nodes[[2]string{kind, id}] = resolvedNode{GraphNode: model.GraphNode{Kind: kind, ID: id}}
			}
		default:
			return nil, fmt.Errorf("graph node kind %q is not addressable", kind)
		}
		if err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

func loadIssueNodes(ctx context.Context, q Queryer, keys []string, nodes map[[2]string]resolvedNode) error {
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
		issueKey := key
		nodes[[2]string{"issue", key}] = resolvedNode{
			GraphNode: model.GraphNode{Kind: "issue", ID: key, IssueKey: &issueKey, Project: project, Ref: "dispatch://" + key},
			text:      title,
		}
	}
	return rows.Err()
}

func loadArtifactNodes(ctx context.Context, q Queryer, byRefKey bool, ids []string, nodes map[[2]string]resolvedNode) error {
	predicate := `id::text = any($1)`
	if byRefKey {
		predicate = `ref_key = any($1)`
	}
	rows, err := q.Query(ctx, `
		select id::text, ref_key, issue_key, project_key, slug, name, kind, is_primary
		from artifacts where `+predicate, ids)
	if err != nil {
		return fmt.Errorf("load artifact nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, refKey, project, slug, name, kind string
		var issueKey *string
		var primary bool
		if err := rows.Scan(&id, &refKey, &issueKey, &project, &slug, &name, &kind, &primary); err != nil {
			return fmt.Errorf("scan artifact node: %w", err)
		}
		lookup := id
		if byRefKey {
			lookup = refKey
		}
		nodes[[2]string{"artifact", lookup}] = resolvedNode{
			GraphNode: model.GraphNode{Kind: "artifact", ID: id, IssueKey: issueKey, Project: project, Ref: artifactRef(issueKey, project, slug, kind, primary)},
			text:      name,
			refKey:    refKey,
		}
	}
	return rows.Err()
}

func loadItemNodes(ctx context.Context, q Queryer, kind string, ids []string, nodes map[[2]string]resolvedNode) error {
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

func loadMessageNodes(ctx context.Context, q Queryer, ids []string, nodes map[[2]string]resolvedNode) error {
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

// artifactRef is the address the dashboard copies for a document: an issue's primary document
// is its spec, another issue artifact sits under artifact/<slug>, and a project document under
// its project.
func artifactRef(issueKey *string, project, slug, kind string, primary bool) string {
	if issueKey == nil {
		return "dispatch://" + project + "/artifact/" + slug
	}
	if primary && kind == "doc" {
		return "dispatch://" + *issueKey + "/spec"
	}
	return "dispatch://" + *issueKey + "/artifact/" + slug
}

// itemRef addresses an ask, comment, or message: under its issue, or under the project document
// that owns it. An issue-less message has no address.
func itemRef(kind string, issueKey *string, project, slug, id string) string {
	if issueKey != nil {
		return "dispatch://" + *issueKey + "/" + kind + "/" + id
	}
	if slug == "" {
		return ""
	}
	return "dispatch://" + project + "/artifact/" + slug + "/" + kind + "/" + id
}

func headRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
