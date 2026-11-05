package api

import (
	"net/http"
	"sort"

	"github.com/sjawhar/envoy/internal/dispatch/decisionmigration"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

type decisionShadowSection struct {
	Source string   `json:"source"`
	Status string   `json:"status"`
	Nodes  []string `json:"nodes"`
}

// getIssueDecisionShadow compares the legacy sections with the node read, not two projections
// of the new documents. Converted sources retain their original blocks and Keep receipts as
// the independent baseline; reading this census never converts or repairs anything.
func (s *server) getIssueDecisionShadow(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ctx, key := r.Context(), r.PathValue("key")
	indexed, err := s.issueDecisionNodes(ctx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	spec, found, err := s.issueSpecDocument(ctx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	specRef := ""
	if found {
		specRef = artifactReference(spec)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `select id::text,slug,is_primary,project_key from artifacts
 where issue_key=$1 and kind='doc' and (name='decisions.md' or slug='decisions-md' or name='decision-history-'||id::text||'.md') order by id`, key)
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
	graph, answered := map[string]model.Decision{}, map[string]model.Decision{}
	for _, decision := range indexed.Decisions {
		graph[decision.Ref] = decision
		if decision.Kind == "person" && !decision.Replaced {
			for _, askID := range decision.Asks {
				answered[askID] = decision
			}
		}
	}
	matched := map[string]bool{}
	missing, extra := []string{}, []string{}
	sections := []decisionShadowSection{}
	for _, document := range documents {
		markdown, blocks, err := s.deps.Docs.TextWithBlocks(ctx, document.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		entries, err := decisionmigration.Plan(markdown, document.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		headings := decisionmigration.SectionHeadings(entries, markdown, blocks)
		keeps, err := decisionKeeps(ctx, s.deps.Store.Pool, document.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		for _, entry := range entries {
			heading := headings[entry.ID]
			source := refs.BlockRef(artifactReference(document), heading)
			if heading == "" {
				source = artifactReference(document) + "#section-" + entry.ID
			}
			section := decisionShadowSection{Source: source, Status: "flagged", Nodes: []string{}}
			keep, kept := keeps[heading]
			delete(keeps, heading)
			drop := entry.Drop && !kept && len(entry.Asks) > 0
			for _, askID := range entry.Asks {
				if _, exists := answered[askID]; !exists {
					drop = false
				}
			}
			if drop {
				section.Status = "dropped"
				for _, askID := range entry.Asks {
					decision := answered[askID]
					matched[decision.Ref] = true
					section.Nodes = append(section.Nodes, decision.Ref)
				}
			} else {
				ref := refs.BlockRef(specRef, entry.ID)
				decision, exists := graph[ref]
				if exists && decision.Kind == "agent" && decision.Replaced == entry.Replaced && decision.TextSHA256 == decisionTextSum(entry.Text()) {
					section.Status = "moved"
					section.Nodes = append(section.Nodes, ref)
					matched[ref] = true
				} else {
					missing = append(missing, source)
				}
				if kept && (!exists || decision.Kept == nil || decision.Kept.TextSHA256 != keep.TextSHA256 || decision.Kept.KeptBy != keep.KeptBy || !decision.Kept.KeptAt.Equal(keep.KeptAt)) {
					missing = append(missing, source+"/keep")
					section.Status = "flagged"
				}
			}
			sections = append(sections, section)
		}
		for heading := range keeps {
			missing = append(missing, refs.BlockRef(artifactReference(document), heading)+"/keep")
		}
	}
	for ref := range graph {
		if !matched[ref] {
			extra = append(extra, ref)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	WriteJSON(w, http.StatusOK, map[string]any{"source_count": len(sections), "node_count": len(graph), "differences": len(missing) + len(extra), "missing": missing, "extra": extra, "sections": sections})
}
