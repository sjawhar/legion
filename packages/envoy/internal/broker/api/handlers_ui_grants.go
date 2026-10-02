// handlers_ui_grants.go: GET /v1/grants, POST /v1/grants/{id}/revoke-by-approver — the approver's
// own live-grant list and human revocation route. Part of the UI routes (uiAuth) Dispatch's
// server relays to on behalf of the browser.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/requests"
)

// approverGrantResp is one grant in GET /v1/grants?approver=<login>. Approver is the login that
// approved it: the list also holds grants on enrollments the login operates that another login
// approved.
type approverGrantResp struct {
	GrantID    string               `json:"grant_id"`
	RecordID   *string              `json:"record_id"`
	Enrollment recordEnrollmentResp `json:"enrollment"`
	Names      []string             `json:"names"`
	Approver   string               `json:"approver"`
	ExpiresAt  time.Time            `json:"expires_at"`
	CreatedAt  time.Time            `json:"created_at"`
}

func (s *server) listGrantsForApprover(w http.ResponseWriter, r *http.Request) {
	approver := r.URL.Query().Get("approver")
	if !requireApprover(w, approver) {
		return
	}
	rows, err := s.deps.Machine.GrantsForApprover(r.Context(), approver)
	if err != nil {
		writeInternal(w, "list grants for approver", err)
		return
	}
	out := make([]approverGrantResp, len(rows))
	for i, g := range rows {
		names := g.Names
		if names == nil {
			names = []string{}
		}
		out[i] = approverGrantResp{
			GrantID: g.GrantID, RecordID: g.RecordID, Enrollment: enrollmentResp(g.Enrollment),
			Names: names, Approver: g.Approver, ExpiresAt: g.ExpiresAt, CreatedAt: g.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

// revokeByApproverBody is {"approver"}: the revoking human's Dispatch login, which Dispatch's
// server sets from its own session.
type revokeByApproverBody struct {
	Approver string `json:"approver"`
}

// revokeByApprover ends a grant on a human's Dispatch login: the login must be the grant's
// approver or its enrollment's operator (requests.Machine.RevokeByApprover's own mayRevoke
// check).
func (s *server) revokeByApprover(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	var body revokeByApproverBody
	if !readJSON(w, r, &body, "INVALID_REVOKE") {
		return
	}
	if !requireApprover(w, body.Approver) {
		return
	}
	err := s.deps.Machine.RevokeByApprover(r.Context(), id, body.Approver)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such grant")
		return
	case errors.Is(err, requests.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		return
	case err != nil:
		writeInternal(w, "revoke grant", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "revoked"})
}
