package api

import "net/http"

type IssueStatusRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Status  string `json:"status"`
}

// IssueStatusResponse confirms Dispatch accepted the controller's synchronous status update and the
// record took it (applyOwnStatusWrite): an issue the controller parks stops before the answer, not
// when the move's event comes back through the Dispatch consumer.
type IssueStatusResponse struct{}

func (s *server) issueStatus(w http.ResponseWriter, r *http.Request) {
	var req IssueStatusRequest
	if !s.readBody(w, r, &req, s.redeemIdentityCheck) || !requireFailureFields(w, field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"status", req.Status}) {
		return
	}
	if req.Status != "todo" && req.Status != "backlog" && req.Status != "icebox" {
		writeFailure(w, http.StatusBadRequest, "INVALID_STATUS", "status must be todo, backlog, or icebox")
		return
	}
	grant, ok := s.redeem(w, r, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", "issue status requires a controller grant")
		return
	}
	if written, ok := s.setDispatchStatus(w, r, req.Issue, req.Status); ok && s.applyOwnStatusWrite(w, r, written) {
		writeJSON(w, http.StatusOK, IssueStatusResponse{})
	}
}
