// packages/envoy/internal/broker/api/handlers_requests.go
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/requests"
)

type createRequestBody struct {
	Secrets   []string `json:"secrets"`
	Reason    string   `json:"reason"`
	Issue     *string  `json:"issue"`
	SessionID *string  `json:"session_id"`
}

// createRequest reads the enrollment id from ctx (the proof-verified caller), never from the
// request body: a session may only ever request secrets for itself.
func (s *server) createRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	enrollmentID := ctx.Value(ctxEnrollment).(string)
	var body createRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "body must be valid JSON")
		return
	}
	req, err := s.deps.Machine.Create(ctx, enrollmentID, body.Secrets, body.Reason, derefOr(body.Issue, ""), derefOr(body.SessionID, ""))
	switch {
	case errors.Is(err, requests.ErrReasonTooLong):
		writeError(w, http.StatusBadRequest, "REASON_TOO_LONG", err.Error())
		return
	case errors.Is(err, requests.ErrIssueMismatch):
		writeError(w, http.StatusBadRequest, "ISSUE_MISMATCH", err.Error())
		return
	case errors.Is(err, requests.ErrMixedApprovers):
		writeError(w, http.StatusBadRequest, "MIXED_APPROVERS", err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		// The proof was verified moments ago against a live enrollment, but it was revoked (or
		// its lease lapsed) before this Create's own read of it — the same race enroll.Renew
		// guards against. The caller's proof is no longer good for anything.
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "enrollment is not live")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "create request failed")
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// requestDecision is GET /v1/requests/{id}'s nested "decision" object: who decided the request
// and when. requestStatusResponse reshapes requests.Request, whose own JSON tags carry
// decided_by/detail at the top level for POST /v1/requests's response — a different shape than
// this route's contract, which nests them here and omits detail entirely.
type requestDecision struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

type requestStatusResponse struct {
	State     string           `json:"state"`
	GrantID   *string          `json:"grant_id"`
	DecidedAt *time.Time       `json:"decided_at"`
	Decision  *requestDecision `json:"decision"`
}

func (s *server) readRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	enrollmentID := ctx.Value(ctxEnrollment).(string)
	id := r.PathValue("id")

	owner, err := s.deps.Machine.OwnerOf(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such request")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "read request failed")
		return
	}
	if owner != enrollmentID {
		writeError(w, http.StatusForbidden, "NOT_YOURS", "this request belongs to another session")
		return
	}

	req, err := s.deps.Machine.Get(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "read request failed")
		return
	}
	resp := requestStatusResponse{State: req.State, GrantID: req.GrantID, DecidedAt: req.DecidedAt}
	if req.DecidedBy != nil && req.DecidedAt != nil {
		resp.Decision = &requestDecision{By: *req.DecidedBy, At: *req.DecidedAt}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) cancelRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	enrollmentID := ctx.Value(ctxEnrollment).(string)
	id := r.PathValue("id")

	err := s.deps.Machine.Cancel(ctx, id, enrollmentID)
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, requests.ErrTerminal):
		writeError(w, http.StatusConflict, "REQUEST_TERMINAL", err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such request")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "cancel request failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "cancelled"})
}

func (s *server) grantValues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	enrollmentID := ctx.Value(ctxEnrollment).(string)
	id := r.PathValue("id")

	values, proxyOnly, expires, err := s.deps.Machine.Values(ctx, id, enrollmentID)
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, requests.ErrGrantNotLive):
		writeError(w, http.StatusForbidden, "GRANT_NOT_LIVE", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "read grant values failed")
		return
	}
	if values == nil {
		values = map[string]string{}
	}
	if proxyOnly == nil {
		proxyOnly = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"values":     values,
		"expires_at": expires,
		"proxy_only": proxyOnly,
	})
}

// revokeGrant is authHumanOrProof: server.authenticate has already put exactly one of ctxEnrollment
// (a session revoking its own grant through its proof) or ctxHuman (a Dispatch-authenticated human,
// bearer or cookie-equivalent) on ctx. Machine.RevokeGrant's enrollmentID *string tells the two
// apart: non-nil (and equal to by) means a session revoking its own grant, nil means an
// already-authenticated human revoking any grant.
func (s *server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	var by string
	var enrollmentIDPtr *string
	if enrollmentID, ok := ctx.Value(ctxEnrollment).(string); ok {
		by = enrollmentID
		enrollmentIDPtr = &enrollmentID
	} else if human, ok := ctx.Value(ctxHuman).(string); ok {
		by = human
	} else {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "no authenticated actor")
		return
	}

	err := s.deps.Machine.RevokeGrant(ctx, id, by, enrollmentIDPtr)
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such grant")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "revoke grant failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "revoked"})
}
