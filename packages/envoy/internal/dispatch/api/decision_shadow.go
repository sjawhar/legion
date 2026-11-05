package api

import (
	"net/http"
	"sort"
	"strconv"

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
 where issue_key=$1 and kind='doc' and (name='decisions.md' or slug='decisions-md') order by id`, key)
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
	shaped := map[string]map[string]bool{}
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
					addDecisionShapedTargets(shaped, decision.Ref, entry.Shaped)
				}
			} else {
				ref := refs.BlockRef(specRef, entry.ID)
				decision, exists := graph[ref]
				if exists && decision.Kind == "agent" && decision.Replaced == entry.Replaced && decision.TextSHA256 == decisionTextSum(entry.Text()) {
					section.Status = "moved"
					section.Nodes = append(section.Nodes, ref)
					matched[ref] = true
					addDecisionShapedTargets(shaped, ref, entry.Shaped)
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
	rows, err = s.deps.Store.Pool.Query(ctx, `select a.id::text from asks a
 where a.issue_key=$1 and a.state='answered' and a.answer is not null and a.block_id is not null
   and (a.block_artifact_id is not null or exists (
     select 1 from document_block_texts h join artifacts d on d.id=h.artifact_id
     where d.issue_key=$1 and h.block_id=a.block_id))
 order by a.id`, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for rows.Next() {
		var askID string
		if err := rows.Scan(&askID); err != nil {
			rows.Close()
			s.writeHandlerError(w, err)
			return
		}
		decision, exists := answered[askID]
		if exists && matched[decision.Ref] {
			continue
		}
		source := "dispatch://" + key + "/ask/" + askID
		section := decisionShadowSection{Source: source, Status: "flagged", Nodes: []string{}}
		if exists {
			matched[decision.Ref] = true
			section.Status = "moved"
			section.Nodes = append(section.Nodes, decision.Ref)
		} else {
			missing = append(missing, source)
		}
		sections = append(sections, section)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	divergent := map[string]bool{}
	for ref, expected := range shaped {
		actual := map[string]bool{}
		for _, link := range graph[ref].Links {
			target := link.To.Ref
			if link.To.Pin != nil && link.To.Pin.Version > 0 {
				target += "@v" + strconv.Itoa(link.To.Pin.Version)
			}
			actual[target] = true
		}
		for target := range expected {
			if !actual[target] {
				missing = append(missing, ref+"/shaped/"+target)
				divergent[ref] = true
			}
		}
		// An actual ask can already shape passages that its legacy restatement does not cite.
		if graph[ref].Kind != "person" {
			for target := range actual {
				if !expected[target] {
					extra = append(extra, ref+"/shaped/"+target)
					divergent[ref] = true
				}
			}
		}
	}
	for i, section := range sections {
		for _, ref := range section.Nodes {
			if divergent[ref] {
				sections[i].Status = "flagged"
			}
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

func addDecisionShapedTargets(expected map[string]map[string]bool, ref string, values []string) {
	targets := expected[ref]
	if targets == nil {
		targets = map[string]bool{}
		expected[ref] = targets
	}
	for _, value := range values {
		targets[value] = true
	}
}
