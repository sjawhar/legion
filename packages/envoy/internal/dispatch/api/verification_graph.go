package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type checkResultsRead struct {
	Issue                  string             `json:"issue"`
	VerificationArtifactID string             `json:"verification_artifact_id"`
	Checks                 []model.CheckBlock `json:"checks"`
}

// servesTarget is one requirement a check names in `serves`. removed marks a target the block
// index no longer holds as a `requirement` block, because the block was deleted or retyped: the
// reader shows it rather than dropping it, so a dangling citation stays visible.
type servesTarget struct {
	Reference string `json:"reference"`
	Removed   bool   `json:"removed"`
}

type verificationRead struct {
	checkResultsRead
	Serves map[string][]servesTarget `json:"serves"`
}

// servesTargetsSQL reads the active serves edges and retains citations whose target was
// deleted or retyped. The graph deliberately hides pointers to disallowed target types, so
// those citations come from block_refs and are marked removed, not treated as active edges.
// A missing target passes the graph's type filter and is marked removed by the left join.
// OFFSET 0 keeps the edge lookup parameterized by each check of this document, rather than
// letting the planner read the whole edge view before filtering it.
const servesTargetsSQL = `
	select distinct
		source.block_id,
		e.to_id,
		e.removed or target.node_id is null
	from document_blocks source
	cross join lateral (
		select e.to_id, false as removed from graph_edges e
		where e.from_kind = 'block' and e.from_id = source.node_id
		  and e.kind = 'serves' and e.to_kind = 'block'
		union all
		select r.to_id, true as removed from block_refs r
		where r.artifact_id = source.artifact_id and r.block_id = source.block_id
		  and r.kind = 'serves' and r.to_kind = 'block'
		  and r.to_types is not null and not block_ref_listed(r.to_types, r.to_id)
		offset 0
	) e
	left join document_blocks target
		on target.node_id = e.to_id and target.type = 'requirement'
	where source.artifact_id = $1::uuid and source.type = 'check'
	order by 1, 2
`

// servesOfChecks reads each check's active typed edges and removed citations. A check the
// index does not hold yet has no indexed relations and answers an empty serves list.
func (s *server) servesOfChecks(ctx context.Context, q queryer, artifactID string) (map[string][]servesTarget, error) {
	rows, err := q.Query(ctx, servesTargetsSQL, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read what the checks of %s serve: %w", artifactID, err)
	}
	defer rows.Close()
	serves := map[string][]servesTarget{}
	for rows.Next() {
		var blockID, targetID string
		var target servesTarget
		if err := rows.Scan(&blockID, &targetID, &target.Removed); err != nil {
			return nil, fmt.Errorf("read what the checks of %s serve: %w", artifactID, err)
		}
		document, block, _ := strings.Cut(targetID, "#")
		target.Reference = "dispatch://" + document + "#b-" + text.EncodeBlockID(block)
		serves[blockID] = append(serves[blockID], target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read what the checks of %s serve: %w", artifactID, err)
	}
	for _, targets := range serves {
		slices.SortFunc(targets, func(a, b servesTarget) int {
			return strings.Compare(a.Reference, b.Reference)
		})
	}
	return serves, nil
}

// verificationTab is the Verification tab's one read: the checks and results GET
// /api/v1/issues/{key}/results answers, plus what each check serves. Membership, order, words
// and attributes are the live document's, so a check the block index does not hold yet is
// listed, with a zero version and digest and an empty serves list; only the relations are the
// graph's.
func (s *server) verificationTab(ctx context.Context, q queryer, issueKey string, currency *currencyReader) (verificationRead, error) {
	answer := verificationRead{Serves: map[string][]servesTarget{}}
	results, err := s.liveCheckResults(ctx, q, issueKey, currency)
	if err != nil {
		return verificationRead{Serves: map[string][]servesTarget{}}, err
	}
	answer.checkResultsRead = results
	if len(answer.Checks) == 0 {
		return answer, nil
	}
	serves, err := s.servesOfChecks(ctx, q, answer.VerificationArtifactID)
	if err != nil {
		return verificationRead{Serves: map[string][]servesTarget{}}, err
	}
	for _, check := range answer.Checks {
		held := serves[check.BlockID]
		if held == nil {
			held = []servesTarget{}
		}
		answer.Serves[check.BlockID] = held
	}
	return answer, nil
}

func (s *server) getIssueVerification(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if err := s.requireIssueExists(r.Context(), key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := s.verificationTab(r.Context(), s.deps.Store.Pool, key, newCurrencyReader(s))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}
