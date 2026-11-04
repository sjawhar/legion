package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type checkResultsRead struct {
	Issue                  string             `json:"issue"`
	VerificationArtifactID string             `json:"verification_artifact_id"`
	Checks                 []model.CheckBlock `json:"checks"`
}

type verificationRead struct {
	checkResultsRead
	Serves map[string][]string `json:"serves"`
}

type graphCheck struct {
	BlockID string              `json:"block_id"`
	Version int                 `json:"version"`
	SHA256  string              `json:"sha256"`
	Results []model.CheckResult `json:"results"`
	Serves  []string            `json:"serves"`
}

// The graph supplies membership and relations. Words and attributes remain the live
// document's, not a second stored copy. Lateral aggregates avoid multiplying results
// by serves edges and keep every recorded result, including runs recorded by children.
var verificationGraphSQL = `
 select a.id::text, a.slug, coalesce((
  select jsonb_agg(jsonb_build_object(
   'block_id', d.block_id, 'version', d.changed_version, 'sha256', d.text_sha256,
   'results', coalesce((select jsonb_agg(to_jsonb(r) order by r.created_at desc, r.id desc)
     from check_results r where r.check_artifact_id = a.id and r.check_block_id = d.block_id), '[]'::jsonb),
   'serves', coalesce((select jsonb_agg(target.reference order by target.reference) from (
     select distinct 'dispatch://' || split_part(t.node_id, '#', 1) || '#b-' || t.block_id as reference
     from graph_edges e join document_blocks t on t.node_id = e.to_id and t.type = 'requirement'
     where e.from_kind = 'block' and e.from_id = d.node_id
       and e.kind = 'serves' and e.to_kind = 'block'
   ) target), '[]'::jsonb)
  ) order by d.block_id)
  from document_blocks d
  where d.artifact_id = $1::uuid and d.type = 'check' and exists (
   select 1 from graph_edges membership
   where membership.from_kind = 'block' and membership.from_id = d.node_id
     and membership.kind = 'part_of' and membership.to_kind = 'artifact' and membership.to_id = a.ref_key
   offset 0
  )
 ), '[]'::jsonb)
 from artifacts a where a.id = $1::uuid`

func (s *server) verificationGraph(ctx context.Context, q queryer, issueKey string, currency *currencyReader) (verificationRead, error) {
	answer := verificationRead{checkResultsRead: checkResultsRead{Issue: issueKey, Checks: []model.CheckBlock{}}, Serves: map[string][]string{}}
	artifact, held, err := s.verificationDocument(ctx, q, issueKey, "")
	if err != nil || !held {
		return answer, err
	}
	var slug string
	var encoded []byte
	err = q.QueryRow(ctx, verificationGraphSQL, artifact.ID).Scan(&answer.VerificationArtifactID, &slug, &encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return answer, nil
	}
	if err != nil {
		return answer, fmt.Errorf("read verification graph of %s: %w", issueKey, err)
	}
	var indexed []graphCheck
	if err := json.Unmarshal(encoded, &indexed); err != nil {
		return answer, fmt.Errorf("decode verification graph of %s: %w", issueKey, err)
	}
	byID := make(map[string]graphCheck, len(indexed))
	for _, check := range indexed {
		byID[check.BlockID] = check
	}
	live, err := s.checkBlocksIn(ctx, answer.VerificationArtifactID)
	if err != nil {
		return answer, err
	}
	for _, check := range live {
		graph, held := byID[check.BlockID]
		if !held {
			continue
		}
		check.IssueKey = issueKey
		check.Ref, _ = text.FormatBlockRef(text.Ref{Kind: "block", IssueKey: issueKey, ID: slug, Block: check.BlockID})
		check.Version, check.SHA256, check.Results = graph.Version, graph.SHA256, graph.Results
		for index := range check.Results {
			// Match pgx's timestamptz decoding; JSON's +00:00 can decode as Local.
			check.Results[index].CreatedAt = check.Results[index].CreatedAt.UTC()
			if currency != nil {
				check.Results[index].Currency = currency.of(ctx, check, check.Results[index])
			}
		}
		answer.Checks = append(answer.Checks, check)
		answer.Serves[check.BlockID] = graph.Serves
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
	answer, err := s.verificationGraph(r.Context(), s.deps.Store.Pool, key, newCurrencyReader(s))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}
