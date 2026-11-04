package api

import (
	"net/http"
	"sort"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// getIssueDecisionShadow compares the graph projection with the actual typed document nodes.
// It is a read-only, issue-scoped census, not a second decision source or a general graph API.
func (s *server) getIssueDecisionShadow(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ctx := r.Context()
	key := r.PathValue("key")
	indexed, err := s.issueDecisionNodes(ctx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `select id::text,slug,is_primary,project_key from artifacts where issue_key=$1 and kind='doc' order by id`, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var documents []model.Artifact
	for rows.Next() {
		document := model.Artifact{IssueKey: &key, Kind: "doc"}
		if err := rows.Scan(&document.ID, &document.Slug, &document.Primary, &document.Project); err != nil {
			rows.Close()
			s.writeHandlerError(w, err)
			return
		}
		documents = append(documents, document)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	source := map[string]bool{}
	for _, document := range documents {
		_, blocks, err := s.deps.Docs.TextWithBlocks(ctx, document.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		for _, block := range blocks {
			if block.Type == "decision" || block.Type == "ask" && blockAttribute(block, "copied_from") == "" {
				source[refs.BlockRef(artifactReference(document), block.ID)] = true
			}
		}
	}
	graph := map[string]bool{}
	for _, decision := range indexed.Decisions {
		graph[decision.Ref] = true
	}
	for _, question := range indexed.OpenQuestions {
		if question.BlockArtifact == nil || question.BlockID == nil {
			continue
		}
		document := question.BlockArtifact
		reference := refs.ArtifactRef(&key, "", nil, document.Slug, "doc", document.Primary)
		graph[refs.BlockRef(reference, *question.BlockID)] = true
	}
	missing, extra := []string{}, []string{}
	for ref := range source {
		if !graph[ref] {
			missing = append(missing, ref)
		}
	}
	for ref := range graph {
		if !source[ref] {
			extra = append(extra, ref)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	WriteJSON(w, http.StatusOK, map[string]any{"source_count": len(source), "node_count": len(graph), "differences": len(missing) + len(extra), "missing": missing, "extra": extra})
}
