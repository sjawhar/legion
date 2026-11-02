package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// coverage.go answers what each requirement of an issue's spec lacks. The requirements are the
// `requirement` blocks of the issue's spec, in document order. A requirement has a check when a
// `check` block of any document of that issue or of any descendant issue, at any status, names
// it in `serves`, and a plan task when a `plan_task` block of that set does; the descendants are
// read with refs.IssueDescendants, the server's one recursive `child_of` subtree walk. The
// missing-plan-task gap is named only once the issue has a plan document, since the plan comes
// after the spec's approval. Nothing is ever refused for a gap: three readers show it — the
// route below for the Spec tab, `dispatch read --issue`, which prints each gap after
// `Coverage: `, and an approval request's answer, which names the missing checks alone.

// noRequirementsGap is the one gap line a spec that marks no requirement answers, in place of a
// line per requirement.
const noRequirementsGap = "the spec marks no requirements"

// GET /api/v1/issues/{key}/coverage  (any authenticated caller)
//
// What each requirement of the issue's spec lacks. An issue with no spec, or a spec that marks
// no requirements, answers no requirements and the single gap line.
func (s *server) getIssueCoverage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	issueKey := r.PathValue("key")
	if err := s.requireIssueExists(r.Context(), issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	coverage, err := s.issueCoverage(r.Context(), s.deps.Store.Pool, issueKey)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, coverage)
}

// issueCoverage reads the issue's requirements and what serves each. The documents' blocks are
// read from the block index (document_blocks and block_refs), which every accepted write leaves
// in step with the document it wrote; the requirements' order and words come from the live spec.
func (s *server) issueCoverage(ctx context.Context, q queryer, issueKey string) (model.IssueCoverage, error) {
	coverage := model.IssueCoverage{Issue: issueKey, Requirements: []model.RequirementCoverage{}, Gaps: []string{}}
	err := q.QueryRow(ctx, `
		select coalesce((select a.id::text from artifacts a where a.issue_key = $1 and a.is_primary), ''),
		       exists (select 1 from artifacts a where a.issue_key = $1 and a.role = 'plan')
	`, issueKey).Scan(&coverage.SpecArtifactID, &coverage.HasPlan)
	if err != nil {
		return model.IssueCoverage{}, fmt.Errorf("read the documents of %s: %w", issueKey, err)
	}
	if coverage.SpecArtifactID == "" {
		coverage.Gaps = []string{noRequirementsGap}
		return coverage, nil
	}
	requirements, err := s.requirementBlocksIn(ctx, coverage.SpecArtifactID)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	if len(requirements) == 0 {
		coverage.Gaps = []string{noRequirementsGap}
		return coverage, nil
	}
	descendants, err := refs.IssueDescendants(ctx, q, issueKey)
	if err != nil {
		return model.IssueCoverage{}, err
	}
	served, err := servingBlockCounts(ctx, q, coverage.SpecArtifactID, append([]string{issueKey}, descendants...))
	if err != nil {
		return model.IssueCoverage{}, err
	}
	coverage.Requirements = make([]model.RequirementCoverage, len(requirements))
	for index, requirement := range requirements {
		requirement.Ref = "dispatch://" + issueKey + "/spec#b-" + text.EncodeBlockID(requirement.BlockID)
		counts := served[requirement.BlockID]
		requirement.Checks, requirement.PlanTasks = counts.checks, counts.planTasks
		coverage.Requirements[index] = requirement
		if requirement.Checks == 0 {
			coverage.Gaps = append(coverage.Gaps, requirement.BlockID+" has no check")
		}
		if coverage.HasPlan && requirement.PlanTasks == 0 {
			coverage.Gaps = append(coverage.Gaps, requirement.BlockID+" has no plan task")
		}
	}
	return coverage, nil
}

// missingCheckGaps is the gap lines an approval request's answer carries: one per requirement
// with no check, and never a missing plan task, since the plan comes after the approval. A spec
// that marks no requirements carries the one line that says so, as the route does, so the
// approver is told that too; a spec every requirement of which has a check carries none.
func missingCheckGaps(coverage model.IssueCoverage) []string {
	if len(coverage.Requirements) == 0 {
		return []string{noRequirementsGap}
	}
	gaps := []string{}
	for _, requirement := range coverage.Requirements {
		if requirement.Checks == 0 {
			gaps = append(gaps, requirement.BlockID+" has no check")
		}
	}
	return gaps
}

// requirementBlocksIn is every `requirement` block of one document, in document order, with its
// words. The document's markdown is read live and parsed, as the checks of a Verification
// document are (checkBlocksIn); nothing is written.
func (s *server) requirementBlocksIn(ctx context.Context, artifactID string) ([]model.RequirementCoverage, error) {
	markdown, err := s.deps.Docs.Text(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return nil, fmt.Errorf("read the requirements of document %s: %w", artifactID, err)
	}
	var requirements []model.RequirementCoverage
	seen := map[string]struct{}{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "requirement" {
			return true
		}
		id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
		if id == "" {
			return true
		}
		// A rendering can repeat an id; the block index keys one row per id, and so does this.
		if _, dup := seen[id]; dup {
			return true
		}
		seen[id] = struct{}{}
		requirements = append(requirements, model.RequirementCoverage{BlockID: id, Text: blockWords(node)})
		return true
	})
	return requirements, nil
}

// servingCounts is how many `check` and `plan_task` blocks serve one requirement.
type servingCounts struct {
	checks    int
	planTasks int
}

// servingBlockCounts counts, for each block of the spec, the `check` and `plan_task` blocks of
// the documents of issues that name it in `serves`. Both ends are current blocks of their
// documents: deleting the check deletes its row, so its requirement is uncovered again.
func servingBlockCounts(ctx context.Context, q queryer, specArtifactID string, issues []string) (map[string]servingCounts, error) {
	rows, err := q.Query(ctx, `
		select t.block_id, s.type, count(*)
		from block_refs r
		join document_blocks t on t.node_id = r.to_id and t.artifact_id = $1::uuid and t.type = 'requirement'
		join document_blocks s on s.artifact_id = r.artifact_id and s.block_id = r.block_id
		join artifacts a on a.id = r.artifact_id
		where r.kind = 'serves' and r.to_kind = 'block'
		  and a.issue_key = any($2) and s.type in ('check', 'plan_task')
		group by t.block_id, s.type
	`, specArtifactID, issues)
	if err != nil {
		return nil, fmt.Errorf("read what serves the requirements of %s: %w", specArtifactID, err)
	}
	defer rows.Close()
	served := map[string]servingCounts{}
	for rows.Next() {
		var blockID, blockType string
		var count int
		if err := rows.Scan(&blockID, &blockType, &count); err != nil {
			return nil, fmt.Errorf("read what serves the requirements of %s: %w", specArtifactID, err)
		}
		counts := served[blockID]
		if blockType == "check" {
			counts.checks += count
		} else {
			counts.planTasks += count
		}
		served[blockID] = counts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read what serves the requirements of %s: %w", specArtifactID, err)
	}
	return served, nil
}

// coverageOfSpec is the coverage an approval request's answer carries: the issue's, when the
// document being approved is that issue's spec, and none for any other document. A read that
// fails is the request's error, since the answer must not say a covered spec has gaps.
func (s *server) coverageOfSpec(ctx context.Context, q queryer, artifact model.Artifact) ([]string, error) {
	if !artifact.Primary || artifact.IssueKey == nil {
		return []string{}, nil
	}
	coverage, err := s.issueCoverage(ctx, q, *artifact.IssueKey)
	if err != nil {
		return nil, err
	}
	return missingCheckGaps(coverage), nil
}
