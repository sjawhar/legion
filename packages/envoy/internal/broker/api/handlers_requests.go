package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/rules"
)

// createRequestBody is POST /v1/requests's exact contract v9 shape: a signed request object plus
// an optional, unsigned session_id (wake-only). The v8 "secrets"/"reason"/"issue" top-level fields
// are gone — they now live inside the signed request object itself.
type createRequestBody struct {
	Request   string  `json:"request"`
	SessionID *string `json:"session_id"`
}

// createRequestResponse is POST /v1/requests's exact wire shape: request_id, state, secrets,
// grant_id, record_id, coalesced — nothing else. requests.Request itself carries additional
// fields (decided_at, decided_by, detail) this route's contract does not define, so the handler
// below builds this dedicated response rather than marshaling the Request it gets back from
// Machine.Create directly.
type createRequestResponse struct {
	RequestID string                    `json:"request_id"`
	State     string                    `json:"state"`
	Secrets   []requests.SecretDecision `json:"secrets"`
	GrantID   *string                   `json:"grant_id"`
	RecordID  *string                   `json:"record_id"`
	Coalesced bool                      `json:"coalesced,omitempty"`
}

// createRequest requests secrets for the proof-verified caller, never for an enrollment named in
// the body: a session may only ever request secrets for itself. The request object itself (not
// this handler) names the secrets, the reason, and — implicitly, via the rules — the approver.
func (s *server) createRequest(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	var body createRequestBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	req, err := s.deps.Machine.Create(r.Context(), enrollmentID, body.Request, derefOr(body.SessionID, ""))
	switch {
	case errors.Is(err, record.ErrRequestInvalid):
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", err.Error())
		return
	case errors.Is(err, requests.ErrMixedApprovers):
		writeError(w, http.StatusBadRequest, "MIXED_APPROVERS", err.Error())
		return
	case errors.Is(err, rules.ErrUnknownSecret):
		writeError(w, http.StatusBadRequest, "UNKNOWN_SECRET", err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		// The proof was verified moments ago against a live enrollment, but it was revoked (or
		// its lease lapsed) before this Create's own read of it — the same race enroll.Renew
		// guards against. The caller's proof is no longer good for anything.
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "enrollment is not live")
		return
	case err != nil:
		writeInternal(w, "create request", err)
		return
	}
	writeJSON(w, http.StatusOK, createRequestResponse{
		RequestID: req.ID,
		State:     req.State,
		Secrets:   req.Secrets,
		GrantID:   req.GrantID,
		RecordID:  req.RecordID,
		Coalesced: req.Coalesced,
	})
}

// requestDecision is GET /v1/requests/{id}'s nested "decision" object: who decided the request
// and when.
type requestDecision struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

type requestStatusResponse struct {
	State     string           `json:"state"`
	GrantID   *string          `json:"grant_id"`
	RecordID  *string          `json:"record_id"`
	DecidedAt *time.Time       `json:"decided_at"`
	Decision  *requestDecision `json:"decision"`
}

func (s *server) readRequest(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	ctx := r.Context()
	id, ok := pathUUID(w, r, "id", "REQUEST_ID_INPUT", "request")
	if !ok {
		return
	}
	owner, err := s.deps.Machine.OwnerOf(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such request")
		return
	case err != nil:
		writeInternal(w, "read request owner", err)
		return
	}
	if owner != enrollmentID {
		writeError(w, http.StatusForbidden, "NOT_YOURS", "this request belongs to another session")
		return
	}

	req, err := s.deps.Machine.Get(ctx, id)
	if err != nil {
		writeInternal(w, "read request", err)
		return
	}
	resp := requestStatusResponse{State: req.State, GrantID: req.GrantID, RecordID: req.RecordID, DecidedAt: req.DecidedAt}
	if req.DecidedBy != nil && req.DecidedAt != nil {
		resp.Decision = &requestDecision{By: *req.DecidedBy, At: *req.DecidedAt}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) cancelRequest(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	id, ok := pathUUID(w, r, "id", "REQUEST_ID_INPUT", "request")
	if !ok {
		return
	}
	err := s.deps.Machine.Cancel(r.Context(), id, enrollmentID)
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
		writeInternal(w, "cancel request", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "cancelled"})
}

func (s *server) grantValues(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	values, proxyOnly, expires, err := s.deps.Machine.Values(r.Context(), id, enrollmentID)
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, requests.ErrGrantNotLive):
		writeError(w, http.StatusForbidden, "GRANT_NOT_LIVE", err.Error())
		return
	case errors.Is(err, requests.ErrGrantChainInvalid):
		writeError(w, http.StatusForbidden, "GRANT_CHAIN_INVALID", err.Error())
		return
	case errors.Is(err, requests.ErrSecretNotInStore):
		writeError(w, http.StatusNotFound, "SECRET_NOT_IN_STORE", err.Error())
		return
	case err != nil:
		writeInternal(w, "read grant values", err)
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

// revokeGrant ends a grant for the session that holds it — session proof only, per contract v9
// (human revocation is the UI's revoke-by-approver route, which Dispatch's server calls with the
// human's login).
func (s *server) revokeGrant(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	err := s.deps.Machine.RevokeGrant(r.Context(), id, enrollmentID)
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such grant")
		return
	case err != nil:
		writeInternal(w, "revoke grant", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "revoked"})
}
