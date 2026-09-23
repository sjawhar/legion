package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

type IssueStatusRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
	Status  string `json:"status"`
}

// IssueStatusResponse confirms Dispatch accepted the controller's synchronous status update.
type IssueStatusResponse struct{}

func (s *server) issueStatus(w http.ResponseWriter, r *http.Request) {
	var req IssueStatusRequest
	if !readBody(w, r, &req) || !requireFields(w, field{"grantId", req.GrantID}, field{"issue", req.Issue}, field{"status", req.Status}) {
		return
	}
	if req.Status != "todo" && req.Status != "backlog" && req.Status != "icebox" {
		writeFailure(w, http.StatusBadRequest, "INVALID_STATUS", "status must be todo, backlog, or icebox")
		return
	}
	grant, ok := s.actionGrant(w, req.GrantID)
	if !ok {
		return
	}
	if !grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_REQUIRED", "issue status requires a controller grant")
		return
	}
	if s.dispatch == nil {
		writeFailure(w, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return
	}
	if err := s.dispatch.SetStatus(r.Context(), req.Issue, req.Status); err != nil {
		var dispatchError *dispatch.Error
		if errors.As(err, &dispatchError) {
			writeFailure(w, http.StatusBadGateway, dispatchError.Code, dispatchError.Message)
			return
		}
		writeFailure(w, http.StatusBadGateway, "DISPATCH_FAILED", "Dispatch status update failed")
		return
	}
	writeJSON(w, http.StatusOK, IssueStatusResponse{})
}
