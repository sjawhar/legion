// handlers_ui_grants.go: GET /v1/grants, POST /v1/grants/{id}/revoke-by-approver — a person's own
// live-grant list and human revocation route. Part of the UI routes (uiAuth) Dispatch's server
// relays to on behalf of the browser.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/requests"
)

// approverGrantResp is one grant in GET /v1/grants?approver=<login>: a grant on a session the login
// operates, automatic or approved by anyone, or a grant the login approved on anyone's session.
type approverGrantResp struct {
	// The grant's id, which the revoke route takes.
	GrantID string `json:"grant_id"`
	// How the session got it: "automatic" (the policy gave it without asking) or "approval".
	Granted string `json:"granted"`
	// The credential-request record its approval rests on; null for an automatic grant.
	RecordID *string `json:"record_id"`
	// The session holding it.
	Enrollment recordEnrollmentResp `json:"enrollment"`
	// The secrets it covers.
	Names []string `json:"names"`
	// The person who approved it; null for an automatic grant.
	Approver *string `json:"approver"`
	// When it expires.
	ExpiresAt time.Time `json:"expires_at"`
	// When it was granted.
	CreatedAt time.Time `json:"created_at"`
}

// approverGrantsResponse is GET /v1/grants's answer.
type approverGrantsResponse struct {
	// The live grants of the named person's sessions, and those the person approved, newest first.
	Grants []approverGrantResp `json:"grants"`
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
			GrantID: g.GrantID, Granted: g.Granted, RecordID: g.RecordID, Enrollment: enrollmentResp(g.Enrollment),
			Names: names, Approver: strPtr(g.Approver), ExpiresAt: g.ExpiresAt, CreatedAt: g.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, approverGrantsResponse{Grants: out})
}

// revokeByApproverBody is {"approver"}: the revoking human's Dispatch login, which Dispatch's
// server sets from its own session.
type revokeByApproverBody struct {
	// The Dispatch login of the person revoking: the grant's approver or its session's operator.
	Approver string `json:"approver"`
}

// revokeByApprover ends a grant on a human's Dispatch login: the login must be the grant's
// approver or its enrollment's operator (requests.Machine.RevokeByApprover's own mayRevoke
// check). When the operator revokes a grant the session got without asking, its secrets are
// withheld from that session, even when the grant had already ended: its other grants that got
// them without asking end too, and its later requests for them ask their owner.
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
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}
