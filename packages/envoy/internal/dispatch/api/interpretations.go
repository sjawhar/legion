package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// Content and interprets live only in the document. The event records the authenticated writer,
// which cannot be supplied as a document attribute or forged by a browser's CRDT update.
func (s *server) createInterpretation(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	if human {
		writeError(w, "INTERPRETATION_FORBIDDEN", http.StatusForbidden, "an interpretation is written by an agent session under a bearer token")
		return
	}
	var input struct {
		Decision string       `json:"decision"`
		Body     string       `json:"body"`
		Actor    *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, err = bearerSessionActor(r, actor, input.Actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Decision == "" || strings.TrimSpace(input.Body) == "" {
		writeError(w, "INTERPRETATION_INPUT", http.StatusBadRequest, "decision block id and body are required")
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" || artifact.IssueKey == nil {
		writeError(w, "INTERPRETATION_INPUT", http.StatusBadRequest, "interpretations belong to an issue document")
		return
	}
	body, err := pmdoc.Parse(input.Body)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for _, child := range body.Children {
		if child.Type != "paragraph" {
			writeError(w, "INTERPRETATION_INPUT", http.StatusBadRequest, "an interpretation body contains paragraphs, not decision or ask blocks")
			return
		}
	}
	id := uuid.NewString()
	reference := artifactReference(artifact)
	node := &pmdoc.Node{Type: "interpretation", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: id, "interprets": []string{refs.BlockRef(reference, input.Decision)}}, Children: body.Children}
	markdown, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{node}})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	owner := ownerForArtifact(artifact)
	if _, err := s.requireOpenOwnerStatus(r.Context(), tx, owner); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	// Block pointer validation runs inside ApplyOps against the current tree and refuses a
	// deleted, cross-document or non-decision parent before any write commits.
	if _, err := s.deps.Docs.ApplyOps(ctx, artifact.ID, []model.EditOp{{Op: "insert", After: "block:" + input.Decision, Markdown: markdown}}, actor, nil); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	written, err := s.deps.Docs.SnapshotVersion(ctx, artifact.ID, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	versionEvent, err := s.commitArtifactVersionEvent(r.Context(), tx, owner, actor, artifact.ID, artifact.Name, written, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, owner.event("interpretation.added", actor, map[string]any{"artifact_id": artifact.ID, "block_id": id, "version": written.Version.Number}))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	superseded := s.supersededApprovalAdvice(r.Context(), tx, "POST /api/v1/artifacts/{id}/interpretations", artifactIssueKey(artifact), artifact.ID, written.Version.Number)
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.deps.Docs.ScheduleSettlement(artifact.ID)
	s.publish(versionEvent, event)
	response := struct {
		model.Interpretation
		LostOps []int `json:"lost_ops"`
	}{
		Interpretation: model.Interpretation{BlockID: id, DecisionRef: refs.BlockRef(reference, input.Decision), Ref: refs.BlockRef(reference, id), Text: strings.TrimSpace(input.Body), Author: &actor},
		LostOps:        lostOps(ledger, artifact.ID),
	}
	WriteJSON(w, http.StatusCreated, withRawAdvice(response, documentWriteAdvice{SupersededApproval: superseded}))
}

// interpretationEdgesQuery follows only same-document typed pointers in this issue. Unknown
// graph kinds are never traversed, and no direct message has an artifact on this route.
const interpretationEdgesQuery = `
 select a.id::text,b.block_id,target.block_id,a.slug,a.is_primary
 from artifacts a
 join document_blocks b on b.artifact_id=a.id and b.type='interpretation'
 join graph_edges g on g.from_kind='block' and g.from_id=b.node_id and g.kind='interprets' and g.to_kind='block'
 join document_blocks target on target.artifact_id=a.id and target.node_id=g.to_id and target.type in ('decision','ask')
 where a.issue_key=$1 and a.kind='doc'
 order by a.is_primary desc,a.created_at,a.id,target.block_id`

// A deleted id reused by hand is not the original agent's writing. History's tombstone breaks
// the attribution, while edits and moves of the original node retain its recorded author.
const interpretationAuthorsQuery = `
 select e.payload->>'artifact_id',e.payload->>'block_id',e.actor
 from events e
 where e.issue_key=$1 and e.type='interpretation.added'
 and not exists (
   select 1 from document_block_texts t
   where t.artifact_id=(e.payload->>'artifact_id')::uuid
     and t.block_id=e.payload->>'block_id' and t.text is null
     and t.version > (e.payload->>'version')::int
 )
 order by e.seq`

func (s *server) getIssueInterpretations(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	items, err := s.issueInterpretations(r.Context(), r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, items)
}

func (s *server) issueInterpretations(ctx context.Context, key string) ([]model.Interpretation, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, interpretationEdgesQuery, key)
	if err != nil {
		return nil, err
	}
	type edge struct {
		document, block, parent, slug string
		primary                       bool
	}
	var edges []edge
	for rows.Next() {
		var e edge
		if err := rows.Scan(&e.document, &e.block, &e.parent, &e.slug, &e.primary); err != nil {
			rows.Close()
			return nil, err
		}
		edges = append(edges, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items := []model.Interpretation{}
	// One event read per issue, using its existing issue event index. No event scan occurs for an
	// issue with no interpretations, and author-less handwritten blocks remain visible.
	if len(edges) == 0 {
		return items, nil
	}
	rows, err = s.deps.Store.Pool.Query(ctx, interpretationAuthorsQuery, key)
	if err != nil {
		return nil, err
	}
	authors := map[[2]string]model.Actor{}
	for rows.Next() {
		var document, block string
		var raw []byte
		if err := rows.Scan(&document, &block, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var actor model.Actor
		if err := json.Unmarshal(raw, &actor); err != nil {
			rows.Close()
			return nil, err
		}
		pair := [2]string{document, block}
		if _, exists := authors[pair]; !exists {
			authors[pair] = actor
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	type documentRead struct {
		reference, markdown string
		blocks              map[string]model.ArtifactBlock
	}
	documents := map[string]documentRead{}
	for _, e := range edges {
		read, found := documents[e.document]
		if !found {
			artifact := model.Artifact{ID: e.document, IssueKey: &key, Slug: e.slug, Primary: e.primary, Kind: "doc"}
			markdown, blocks, err := s.deps.Docs.TextWithBlocks(ctx, e.document)
			if err != nil {
				return nil, err
			}
			read = documentRead{reference: artifactReference(artifact), markdown: markdown, blocks: map[string]model.ArtifactBlock{}}
			for _, block := range blocks {
				read.blocks[block.ID] = block
			}
			documents[e.document] = read
		}
	}
	for first := 0; first < len(edges); {
		last := first + 1
		for last < len(edges) && edges[last].document == edges[first].document {
			last++
		}
		group := edges[first:last]
		blocks := documents[edges[first].document].blocks
		sort.SliceStable(group, func(i, j int) bool { return blocks[group[i].block].From < blocks[group[j].block].From })
		first = last
	}
	for _, e := range edges {
		read := documents[e.document]
		block, found := read.blocks[e.block]
		if !found || block.Type != "interpretation" {
			continue
		}
		tree, err := pmdoc.ParseRendering(sliceMarkdown(read.markdown, block.From, block.To))
		if err != nil {
			return nil, err
		}
		if len(tree.Children) != 1 || tree.Children[0].Type != "interpretation" {
			continue
		}
		body, err := pmdoc.Render(&pmdoc.Node{Type: "doc", Children: tree.Children[0].Children})
		if err != nil {
			return nil, err
		}
		item := model.Interpretation{BlockID: e.block, DecisionRef: refs.BlockRef(read.reference, e.parent), Ref: refs.BlockRef(read.reference, e.block), Text: strings.TrimSpace(body)}
		if actor, found := authors[[2]string{e.document, e.block}]; found {
			item.Author = &actor
		}
		items = append(items, item)
	}
	return items, nil
}
