package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// The candidate read the census measures: membership from the graph's `part_of` edges, the
// version, the digest and the results from the block index, decoded through JSON exactly as a
// cutover would decode them. No route serves it. Lateral aggregates avoid multiplying results by
// edges and keep every recorded result, including runs recorded by children.
const graphCheckResultsSQL = `
 select a.slug, coalesce((
  select jsonb_agg(jsonb_build_object(
   'block_id', d.block_id, 'version', d.changed_version, 'sha256', d.text_sha256,
   'results', coalesce((select jsonb_agg(to_jsonb(r) order by r.created_at desc, r.id desc)
     from check_results r where r.check_artifact_id = a.id and r.check_block_id = d.block_id), '[]'::jsonb)
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

type graphCheck struct {
	BlockID string              `json:"block_id"`
	Version int                 `json:"version"`
	SHA256  string              `json:"sha256"`
	Results []model.CheckResult `json:"results"`
}

// graphCheckResults is the pre-cutover candidate for GET /api/v1/issues/{key}/results, read for
// the census alone. Currency is deliberately absent on both sides of the census: delivery's
// currency checks can read GitHub and write their commit cache, whereas this census is strictly
// read-only.
func (s *server) graphCheckResults(ctx context.Context, q queryer, issueKey string) (checkResultsRead, error) {
	answer := checkResultsRead{Issue: issueKey, Checks: []model.CheckBlock{}}
	artifact, held, err := s.verificationDocument(ctx, q, issueKey, "")
	if err != nil || !held {
		return answer, err
	}
	answer.VerificationArtifactID = artifact.ID
	var slug string
	var encoded []byte
	err = q.QueryRow(ctx, graphCheckResultsSQL, artifact.ID).Scan(&slug, &encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return answer, nil
	}
	if err != nil {
		return answer, fmt.Errorf("read the graph checks of %s: %w", issueKey, err)
	}
	var indexed []graphCheck
	if err := json.Unmarshal(encoded, &indexed); err != nil {
		return answer, fmt.Errorf("decode the graph checks of %s: %w", issueKey, err)
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
		}
		answer.Checks = append(answer.Checks, check)
	}
	return answer, nil
}

type verificationMismatch struct {
	Issue string `json:"issue"`
	Cause string `json:"cause"`
}

type verificationShadowReport struct {
	Issues     int                    `json:"issues"`
	Mismatches int                    `json:"mismatches"`
	ByCause    map[string]int         `json:"by_cause"`
	Details    []verificationMismatch `json:"details"`
	// NextIssue is the issue the next page starts at, empty once the census reached the end.
	NextIssue string `json:"next_issue"`
}

func resultMismatchCauses(old, next checkResultsRead) []string {
	causes := []string{}
	oldIDs, nextIDs := make([]string, len(old.Checks)), make([]string, len(next.Checks))
	for i, check := range old.Checks {
		oldIDs[i] = check.BlockID
	}
	for i, check := range next.Checks {
		nextIDs[i] = check.BlockID
	}
	if old.VerificationArtifactID != next.VerificationArtifactID || !slices.Equal(oldIDs, nextIDs) {
		return []string{"check_membership"}
	}
	attributes, results := false, false
	for i, check := range old.Checks {
		other := next.Checks[i]
		if !reflect.DeepEqual(check.Results, other.Results) {
			results = true
		}
		check.Results, other.Results = nil, nil
		if !reflect.DeepEqual(check, other) {
			attributes = true
		}
	}
	if attributes {
		causes = append(causes, "check_attributes")
	}
	if results {
		causes = append(causes, "results")
	}
	return causes
}

// shadowPage is one bounded run of the census: at most limit issues, starting at the first issue
// key that is not before `after`.
type shadowPage struct {
	after string
	limit int
}

const maxShadowIssues = 100

func parseShadowPage(r *http.Request) (shadowPage, error) {
	page := shadowPage{after: strings.TrimSpace(r.URL.Query().Get("issue")), limit: 20}
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return page, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maxShadowIssues {
		return shadowPage{}, errorf(http.StatusBadRequest, "INVALID_QUERY",
			"limit must be between 1 and %d", maxShadowIssues)
	}
	page.limit = limit
	return page, nil
}

// GET /api/v1/verification-shadow  (any authenticated caller)
//
// One page of the census, never the whole fleet: it reads at most `limit` issues from `issue`
// onwards, and names the issue the next page starts at. Each page runs under one RepeatableRead
// read-only snapshot, so no page holds a long transaction open.
func (s *server) getVerificationShadow(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	page, err := parseShadowPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	tx, err := pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	report, err := s.verificationShadow(r.Context(), tx, page)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, report)
}

func (s *server) verificationShadow(ctx context.Context, q queryer, page shadowPage) (verificationShadowReport, error) {
	report := verificationShadowReport{ByCause: map[string]int{}, Details: []verificationMismatch{}}
	// One row past the page, so the answer can name where the next page starts without counting
	// the whole fleet.
	rows, err := q.Query(ctx, `select distinct a.issue_key from artifacts a
		where `+aliasedVerificationRole+` and a.issue_key >= $1 order by a.issue_key limit $2`,
		page.after, page.limit+1)
	if err != nil {
		return report, err
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return report, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return report, err
	}
	if len(keys) > page.limit {
		report.NextIssue = keys[page.limit]
		keys = keys[:page.limit]
	}
	for _, key := range keys {
		old, err := s.liveCheckResults(ctx, q, key, nil)
		if err != nil {
			return report, fmt.Errorf("shadow results of %s: %w", key, err)
		}
		next, err := s.graphCheckResults(ctx, q, key)
		if err != nil {
			return report, err
		}
		causes := resultMismatchCauses(old, next)
		oldCoverage, err := s.issueCoverageUsing(ctx, q, key, servingBlockCountsSQL)
		if err != nil {
			return report, err
		}
		nextCoverage, err := s.issueCoverageUsing(ctx, q, key, graphServingBlockCountsSQL)
		if err != nil {
			return report, err
		}
		if !reflect.DeepEqual(oldCoverage, nextCoverage) {
			causes = append(causes, "coverage")
		}
		report.Issues++
		for _, cause := range causes {
			report.Mismatches++
			report.ByCause[cause]++
			report.Details = append(report.Details, verificationMismatch{Issue: key, Cause: cause})
		}
	}
	return report, nil
}
