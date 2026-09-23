package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

type HandoffCompleteRequest struct {
	GrantID string `json:"grantId"`
	Summary string `json:"summary"`
	Verdict string `json:"verdict"`
	Ready   bool   `json:"ready"`
	Commit  string `json:"commit"`
}

// HandoffCompleteResponse confirms that the completion fact committed.
type HandoffCompleteResponse struct{}

func (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {
	var req HandoffCompleteRequest
	if !readBody(w, r, &req) || !requireFields(w, field{"grantId", req.GrantID}, field{"summary", req.Summary}, field{"commit", req.Commit}) {
		return
	}
	grant, ok := s.redeem(w, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller {
		writeFailure(w, http.StatusForbidden, "GRANT_INVALID", "grant is unavailable")
		return
	}
	if grant.Role == claim.RoleTester && req.Verdict != "pass" && req.Verdict != "fail" {
		writeFailure(w, http.StatusBadRequest, "TESTER_VERDICT_REQUIRED", "tester verdict must be pass or fail")
		return
	}
	if grant.Role != claim.RoleTester && req.Verdict != "" {
		writeFailure(w, http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN", "only a tester may report a verdict")
		return
	}
	if req.Ready && grant.Role != claim.RoleMerger {
		writeFailure(w, http.StatusBadRequest, "READY_ROLE_FORBIDDEN", "only a merger may report ready")
		return
	}
	if s.pool == nil || s.records == nil {
		writeFailure(w, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	issue, round, err := s.handoffPosition(r.Context(), grant.Issue)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read the issue record")
		return
	}
	if issue == nil {
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return
	}
	// One phase's completion is identified by where the issue stands — its generation, phase, and
	// review round — with the role and the commit reported. A retried call for the same phase is
	// the same fact; the next phase's completion at the same commit (the implementer's retro, then
	// its production check) is a different one. The position is read before the fact's own
	// transaction: should the issue move in between, the engine re-reads it there and ignores a
	// completion whose role no longer owns the phase.
	eventID := fmt.Sprintf("handoff:%s:%d:%s:%s:%d:%s", grant.Issue, issue.Generation, grant.Role, issue.Phase, round, req.Commit)
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, intake.HandoffComplete{Issue: grant.Issue, Role: grant.Role, Claim: grant.Claim, Summary: req.Summary, Verdict: req.Verdict, Ready: req.Ready, Commit: req.Commit}, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact")
		return
	}
	if result.Duplicate {
		writeFailure(w, http.StatusConflict, "HANDOFF_ALREADY_RECORDED", fmt.Sprintf(
			"the %s completion of phase %s (round %d) at commit %s was already recorded; this call changed nothing",
			grant.Role, issue.Phase, round, req.Commit))
		return
	}
	if result.Refusal != nil {
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	writeJSON(w, http.StatusOK, HandoffCompleteResponse{})
}

// handoffPosition reads the issue record and its review round: the implementer's count of returns
// to implementing, which tells each implementing, testing, and reviewing pass from the last.
func (s *server) handoffPosition(ctx context.Context, key string) (*record.Issue, int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	issue, err := s.records.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, 0, err
	}
	rows, err := s.records.Phases(ctx, tx, key)
	if err != nil {
		return nil, 0, err
	}
	round := 0
	for _, row := range rows {
		if row.Role == claim.RoleImplementer {
			round = row.Rounds
		}
	}
	return issue, round, tx.Commit(ctx)
}
