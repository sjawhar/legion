package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
)

type HandoffCompleteRequest struct {
	GrantID string `json:"grantId"`
	Summary string `json:"summary"`
	Verdict string `json:"verdict"`
	Ready bool `json:"ready"`
	Commit string `json:"commit"`
}

// HandoffCompleteResponse confirms that the completion fact committed.
type HandoffCompleteResponse struct{}

func (s *server) handoffComplete(w http.ResponseWriter, r *http.Request) {
	var req HandoffCompleteRequest
	if !readBody(w, r, &req) || !requireFields(w, field{"grantId", req.GrantID}, field{"summary", req.Summary}, field{"commit", req.Commit}) { return }
	grant, ok := s.actionGrant(w, req.GrantID)
	if !ok {
		return
	}
	if grant.Controller {
		writeFailure(w, http.StatusForbidden, "GRANT_INVALID", "grant is unavailable")
		return
	}
	if grant.Role == claim.RoleTester && req.Verdict != "pass" && req.Verdict != "fail" { writeFailure(w, http.StatusBadRequest, "TESTER_VERDICT_REQUIRED", "tester verdict must be pass or fail"); return }
	if grant.Role != claim.RoleTester && req.Verdict != "" { writeFailure(w, http.StatusBadRequest, "VERDICT_ROLE_FORBIDDEN", "only a tester may report a verdict"); return }
	if req.Ready && grant.Role != claim.RoleMerger { writeFailure(w, http.StatusBadRequest, "READY_ROLE_FORBIDDEN", "only a merger may report ready"); return }
	if s.pool == nil { writeFailure(w, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable"); return }
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", fmt.Sprintf("handoff:%s:%s:%s", grant.Issue, grant.Role, req.Commit), intake.HandoffComplete{Issue: grant.Issue, Role: grant.Role, Claim: grant.Claim, Summary: req.Summary, Verdict: req.Verdict, Ready: req.Ready, Commit: req.Commit}, s.handlers...)
	if err != nil { writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply handoff fact"); return }
	if result.Refusal != nil { writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message); return }
	writeJSON(w, http.StatusOK, HandoffCompleteResponse{})
}
