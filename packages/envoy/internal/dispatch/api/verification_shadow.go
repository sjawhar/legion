package api

import (
 "context"
 "fmt"
 "net/http"
 "reflect"
 "slices"

 "github.com/jackc/pgx/v5"
 "github.com/sjawhar/envoy/internal/dispatch/model"
)

// Kept solely as the independent, pre-cutover relation source for the shadow census.
const legacyServingBlockCountsSQL = `
 select t.block_id, s.type, count(*)
 from block_refs r
 join document_blocks t on t.node_id = r.to_id and t.artifact_id = $1::uuid and t.type = 'requirement'
 join document_blocks s on s.artifact_id = r.artifact_id and s.block_id = r.block_id
 join artifacts a on a.id = r.artifact_id
 where r.kind = 'serves' and r.to_kind = 'block'
   and a.issue_key = any($2) and s.type in ('check', 'plan_task')
 group by t.block_id, s.type
`

// legacyCheckResults is the pre-cutover computation, not a second writer or a route fallback.
// Currency is deliberately absent on both sides of the census: delivery's currency checks
// can read GitHub and write their commit cache, whereas this census is strictly read-only.
func (s *server) legacyCheckResults(ctx context.Context, q queryer, key string) (checkResultsRead, error) {
 answer := checkResultsRead{Issue: key, Checks: []model.CheckBlock{}}
 artifact, held, err := s.verificationDocument(ctx, q, key, "")
 if err != nil || !held { return answer, err }
 answer.VerificationArtifactID = artifact.ID
 answer.Checks, err = s.checkBlocksOf(ctx, q, artifact)
 if err != nil { return answer, err }
 recorded, err := loadCheckResults(ctx, q, artifact.ID)
 if err != nil { return answer, err }
 for index := range answer.Checks {
  results := recorded[answer.Checks[index].BlockID]
  if results == nil { results = []model.CheckResult{} }
  answer.Checks[index].Results = results
 }
 return answer, nil
}

type verificationMismatch struct {
 Issue string `json:"issue"`
 Cause string `json:"cause"`
}

type verificationShadowReport struct {
 Issues int `json:"issues"`
 Mismatches int `json:"mismatches"`
 ByCause map[string]int `json:"by_cause"`
 Details []verificationMismatch `json:"details"`
}

func resultMismatchCauses(old, next checkResultsRead) []string {
 causes := []string{}
 oldIDs, nextIDs := make([]string, len(old.Checks)), make([]string, len(next.Checks))
 for i, check := range old.Checks { oldIDs[i] = check.BlockID }
 for i, check := range next.Checks { nextIDs[i] = check.BlockID }
 if old.VerificationArtifactID != next.VerificationArtifactID || !slices.Equal(oldIDs, nextIDs) { return []string{"check_membership"} }
 attributes, results := false, false
 for i, check := range old.Checks {
  other := next.Checks[i]
  if !reflect.DeepEqual(check.Results, other.Results) { results = true }
  check.Results, other.Results = nil, nil
  if !reflect.DeepEqual(check, other) { attributes = true }
 }
 if attributes { causes = append(causes, "check_attributes") }
 if results { causes = append(causes, "results") }
 return causes
}

func (s *server) getVerificationShadow(w http.ResponseWriter, r *http.Request) {
 if !s.requireAuthenticated(w, r) { return }
 pool, err := s.readyPool()
 if err != nil { s.writeHandlerError(w, err); return }
 tx, err := pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
 if err != nil { s.writeHandlerError(w, err); return }
 defer tx.Rollback(r.Context())
 report, err := s.verificationShadow(r.Context(), tx)
 if err != nil { s.writeHandlerError(w, err); return }
 WriteJSON(w, http.StatusOK, report)
}

func (s *server) verificationShadow(ctx context.Context, q queryer) (verificationShadowReport, error) {
 report := verificationShadowReport{ByCause: map[string]int{}, Details: []verificationMismatch{}}
 rows, err := q.Query(ctx, `select distinct a.issue_key from artifacts a where `+aliasedVerificationRole+` order by a.issue_key`)
 if err != nil { return report, err }
 keys := []string{}
 for rows.Next() {
  var key string
  if err := rows.Scan(&key); err != nil { rows.Close(); return report, err }
  keys = append(keys, key)
 }
 err = rows.Err()
 rows.Close()
 if err != nil { return report, err }
 for _, key := range keys {
  old, err := s.legacyCheckResults(ctx, q, key)
  if err != nil { return report, fmt.Errorf("shadow results of %s: %w", key, err) }
  next, err := s.verificationGraph(ctx, q, key, nil)
  if err != nil { return report, err }
  causes := resultMismatchCauses(old, next.checkResultsRead)
  oldCoverage, err := s.issueCoverageUsing(ctx, q, key, legacyServingBlockCountsSQL)
  if err != nil { return report, err }
  nextCoverage, err := s.issueCoverage(ctx, q, key)
  if err != nil { return report, err }
  if !reflect.DeepEqual(oldCoverage, nextCoverage) { causes = append(causes, "coverage") }
  report.Issues++
  for _, cause := range causes {
   report.Mismatches++
   report.ByCause[cause]++
   report.Details = append(report.Details, verificationMismatch{Issue: key, Cause: cause})
  }
 }
 return report, nil
}
