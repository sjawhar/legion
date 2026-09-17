package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockExcerptRunes bounds the containing block shown for a document mention.
const blockExcerptRunes = 480

// getReferences reads one node's edges in the reference graph: ?to= lists every edge pointing
// at the node (backlinks), ?from= every edge it writes; exactly one is required. ?kind= narrows
// edge types and ?since= keeps mentions introduced after an events.id, which excludes
// structural edges. Cross-project by design: no project filter.
func (s *server) getReferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query, ref, err := parseReferencesQuery(r, s.deps.ServerURL)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if query.Direction == "out" && ref.Kind == "artifact" {
		if err := s.deps.Store.Pool.QueryRow(r.Context(), `select id::text from artifacts where ref_key = $1`, query.ID).Scan(&query.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "no document at %s", query.ID))
				return
			}
			s.writeHandlerError(w, err)
			return
		}
	}
	node, found, err := refs.Node(r.Context(), s.deps.Store.Pool, query.Kind, query.ID, query.Direction == "in")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !found {
		s.writeHandlerError(w, errorf(http.StatusNotFound, "NOT_FOUND", "no %s at %s", query.Kind, query.ID))
		return
	}
	edges, err := refs.Edges(r.Context(), s.deps.Store.Pool, query)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if query.Direction == "in" {
		if err := s.documentMentionExcerpts(r.Context(), query, edges); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, model.GraphReferences{Node: node, Edges: edges})
}

// parseReferencesQuery turns ?to=|?from=, ?kind=, and ?since= into an edge query addressed the
// way graph_edges stores the node: ToID for the to side (ref_key for artifacts), the item id
// for the from side (the caller swaps an artifact's ref_key for its uuid).
func parseReferencesQuery(r *http.Request, serverURL string) (refs.Query, text.Ref, error) {
	values := r.URL.Query()
	to, from := strings.TrimSpace(values.Get("to")), strings.TrimSpace(values.Get("from"))
	if (to == "") == (from == "") {
		return refs.Query{}, text.Ref{}, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "exactly one of to or from is required")
	}
	raw, direction := to, "in"
	if from != "" {
		raw, direction = from, "out"
	}
	located := text.ExtractAt(raw, serverURL)
	if len(located) != 1 || located[0].Offset != 0 || located[0].Kind == "url" {
		return refs.Query{}, text.Ref{}, errorf(http.StatusBadRequest, "INVALID_REFERENCE", "%q is not a dispatch:// reference to an issue, document, ask, comment, message, or component", raw)
	}
	ref := located[0].Ref
	query := refs.Query{Direction: direction, Kind: ref.Kind, ID: refs.ToID(ref)}
	if kinds := strings.TrimSpace(values.Get("kind")); kinds != "" {
		for _, kind := range strings.Split(kinds, ",") {
			kind = strings.TrimSpace(kind)
			if !refs.KnownKind(kind) {
				return refs.Query{}, text.Ref{}, errorf(http.StatusBadRequest, "INVALID_KIND", "kind %q is not one of %s", kind, strings.Join(refs.Kinds, ", "))
			}
			query.Kinds = append(query.Kinds, kind)
		}
	}
	if since := strings.TrimSpace(values.Get("since")); since != "" {
		value, err := strconv.ParseInt(since, 10, 64)
		if err != nil || value < 0 {
			return refs.Query{}, text.Ref{}, errorf(http.StatusBadRequest, "INVALID_SINCE", "since must be an event id")
		}
		query.Since = &value
	}
	return query, ref, nil
}

// documentMentionExcerpts replaces the excerpt of every mention written by a document with the
// block containing the mention: the block id (a #b-<id> jump target) and its markdown. The live
// tree is rendered once per document; a mention in an edit not yet settled has no block until
// the closer runs and keeps the document's name.
func (s *server) documentMentionExcerpts(ctx context.Context, query refs.Query, edges []model.GraphEdge) error {
	type rendered struct {
		markdown string
		blocks   []model.ArtifactBlock
	}
	documents := make(map[string]rendered)
	for index := range edges {
		edge := &edges[index]
		if edge.Kind != "mentions" || edge.Node.Kind != "artifact" {
			continue
		}
		document, loaded := documents[edge.Node.ID]
		if !loaded {
			markdown, blocks, err := s.deps.Docs.TextWithBlocks(ctx, edge.Node.ID)
			if err != nil {
				return fmt.Errorf("render document %s for reference excerpt: %w", edge.Node.ID, err)
			}
			document = rendered{markdown: markdown, blocks: blocks}
			documents[edge.Node.ID] = document
		}
		block, found := mentionBlock(document.markdown, document.blocks, s.deps.ServerURL, query.Kind, query.ID)
		if !found {
			continue
		}
		edge.Excerpt = &model.GraphExcerpt{
			BlockID: block.ID,
			Text:    text.HeadRunes(document.markdown[block.From:block.To], blockExcerptRunes),
		}
	}
	return nil
}

// mentionBlock finds the block whose byte range contains the first mention of (kind, id) in
// markdown.
func mentionBlock(markdown string, blocks []model.ArtifactBlock, serverURL, kind, id string) (model.ArtifactBlock, bool) {
	for _, located := range text.ExtractAt(markdown, serverURL) {
		if located.Kind != kind || refs.ToID(located.Ref) != id {
			continue
		}
		for _, block := range blocks {
			if block.From <= located.Offset && located.Offset < block.To {
				return block, true
			}
		}
	}
	return model.ArtifactBlock{}, false
}

func (s *server) getIssueReferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	issue, err := s.loadIssue(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	closure, truncated, err := refs.Closure(r.Context(), s.deps.Store.Pool, issue.Key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	refKeys := make([]string, len(closure))
	for index, member := range closure {
		refKeys[index] = member.RefKey
	}
	artifacts, err := s.loadArtifactsByRefKey(r.Context(), s.deps.Store.Pool, refKeys)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	members := make([]model.ReferenceMember, 0, len(closure))
	for _, closureMember := range closure {
		artifact, ok := artifacts[closureMember.RefKey]
		if !ok {
			s.writeHandlerError(w, fmt.Errorf("reference target %q disappeared while loading closure", closureMember.RefKey))
			return
		}
		members = append(members, model.ReferenceMember{
			Artifact: artifact,
			Depth:    closureMember.Depth,
			Via:      closureMember.Via,
		})
	}
	etag := referencesETag(issue.LastSeq, members, truncated)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	WriteJSON(w, http.StatusOK, model.IssueReferences{Members: members, Truncated: truncated})
}

func (s *server) getArtifactReferences(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	outgoing, err := refs.Outgoing(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	referencedBy, err := refs.ReferencedBy(r.Context(), s.deps.Store.Pool, artifact.RefKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, model.ArtifactReferences{Outgoing: outgoing, ReferencedBy: referencedBy})
}

func referencesETag(lastSeq int, members []model.ReferenceMember, truncated bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d\n%t\n", lastSeq, truncated)
	for _, member := range members {
		_, _ = fmt.Fprintf(hash, "%s@%d\n", member.Artifact.ID, member.Artifact.Versions[len(member.Artifact.Versions)-1].Number)
	}
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
}

func (s *server) loadArtifactsByRefKey(ctx context.Context, q queryer, refKeys []string) (map[string]model.Artifact, error) {
	artifacts := make(map[string]model.Artifact, len(refKeys))
	if len(refKeys) == 0 {
		return artifacts, nil
	}
	rows, err := q.Query(ctx, `
		select a.id::text, a.issue_key, a.project_key, a.ref_key, a.slug, a.name, a.kind, a.is_primary,
		       a.created_by, a.created_at,
		       v.number, v.named, v.summary, v.authors, v.created_at, v.size, v.mime, v.sha256
		from artifacts a
		join artifact_versions v on v.artifact_id = a.id
		where a.ref_key = any($1)
		order by a.ref_key, v.number
	`, refKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var artifact model.Artifact
		var createdBy, authors []byte
		var version model.Version
		if err := rows.Scan(
			&artifact.ID,
			&artifact.IssueKey,
			&artifact.Project,
			&artifact.RefKey,
			&artifact.Slug,
			&artifact.Name,
			&artifact.Kind,
			&artifact.Primary,
			&createdBy,
			&artifact.CreatedAt,
			&version.Number,
			&version.Named,
			&version.Summary,
			&authors,
			&version.CreatedAt,
			&version.Size,
			&version.MIME,
			&version.SHA256,
		); err != nil {
			return nil, err
		}
		if err := decodeArtifactReference(&artifact, createdBy, &version, authors); err != nil {
			return nil, err
		}
		if current, found := artifacts[artifact.RefKey]; found {
			current.Versions = append(current.Versions, version)
			artifacts[artifact.RefKey] = current
			continue
		}
		artifact.Versions = []model.Version{version}
		artifacts[artifact.RefKey] = artifact
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func decodeArtifactReference(artifact *model.Artifact, createdBy []byte, version *model.Version, authors []byte) error {
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return fmt.Errorf("decode artifact author: %w", err)
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return fmt.Errorf("decode artifact version authors: %w", err)
	}
	return nil
}
