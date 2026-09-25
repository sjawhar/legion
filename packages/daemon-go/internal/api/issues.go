package api

import "net/http"

type IssueStatusRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Status  string `json:"status"`
}

// IssueStatusResponse confirms Dispatch accepted the controller's synchronous status update.
type IssueStatusResponse struct{}

func (s *server) issueStatus(w http.ResponseWriter, r *http.Request) {
	var req IssueStatusRequest
	if !readBody(w, r, &req) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"status", req.Status}) {
		return
	}
	if req.Status != "todo" && req.Status != "backlog" && req.Status != "icebox" {
		writeFailure(w, http.StatusBadRequest, "INVALID_STATUS", "status must be todo, backlog, or icebox")
		return
	}
	grant, ok := s.redeem(w, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", "issue status requires a controller grant")
		return
	}
	if s.setDispatchStatus(w, r, req.Issue, req.Status) {
		writeJSON(w, http.StatusOK, IssueStatusResponse{})
	}
}
