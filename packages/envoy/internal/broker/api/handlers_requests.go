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

// createRequestBody is POST /v1/requests's exact shape in the shared broker contract
// (dispatch://AGENTC-393/artifact/plan-overview-md): a signed request object plus an optional,
// unsigned session_id (wake-only). The v8 "secrets"/"reason"/"issue" top-level fields are gone —
// they now live inside the signed request object itself.
type createRequestBody struct {
	// The session's signed request: a compact ES256 JWS, signed with the session's key and
	// addressed to the broker's public URL, naming the secrets (authorization_details) and the
	// reason.
	Request string `json:"request"`
	// Optional: the Envoy session the broker notifies if this request expires undecided.
	SessionID *string `json:"session_id"`
}

// createRequestResponse is POST /v1/requests's exact wire shape: request_id, state, secrets,
// grant_id, record_id, coalesced — nothing else. requests.Request itself carries additional
// fields (decided_at, decided_by, detail) this route's contract does not define, so the handler
// below builds this dedicated response rather than marshaling the Request it gets back from
// Machine.Create directly.
type createRequestResponse struct {
	// The request's id, which GET /v1/requests/{id} and its cancel route take.
	RequestID string `json:"request_id"`
	// "granted", "pending" (a person must decide it) or "denied".
	State string `json:"state"`
	// How the rules decided each name asked for.
	Secrets []requests.SecretDecision `json:"secrets"`
	// Once granted, the grant to read the values from; null otherwise.
	GrantID *string `json:"grant_id"`
	// The credential-request record the approver decides; null when nobody needs to.
	RecordID *string `json:"record_id"`
	// True when the request joined an identical one this session already had pending.
	Coalesced bool `json:"coalesced,omitempty"`
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
		// its lease lapsed) before Create read it or locked it to write — the same race
		// enroll.Renew guards against. The caller's proof is no longer good for anything.
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
	// Who decided it: the approver's login, "session:<enrollment id>" (the session cancelled it),
	// "launcher:<credential id>" (its launcher ended the session) or "broker" (the session's lease
	// lapsed).
	By string `json:"by"`
	// When it was decided.
	At time.Time `json:"at"`
}

// requestStatusResponse is GET /v1/requests/{id}'s answer.
type requestStatusResponse struct {
	// "pending", "granted", "denied", "expired" or "cancelled".
	State string `json:"state"`
	// Once granted, the grant to read the values from; null otherwise.
	GrantID *string `json:"grant_id"`
	// The credential-request record the approver decides; null when nobody needs to.
	RecordID *string `json:"record_id"`
	// When it left "pending"; null while pending.
	DecidedAt *time.Time `json:"decided_at"`
	// Who decided it and when; null while pending, when the rules decided it at once, and when it
	// expired.
	Decision *requestDecision `json:"decision"`
}

// grantValuesResponse is POST /v1/grants/{id}/values's answer.
type grantValuesResponse struct {
	// When the grant expires; the session can read the values again until then.
	ExpiresAt time.Time `json:"expires_at"`
	// The granted names the broker releases no value for (delivery: proxy).
	ProxyOnly []string `json:"proxy_only"`
	// Each granted name's value, read fresh from the secret store.
	Values map[string]string `json:"values"`
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
	writeJSON(w, http.StatusOK, stateResponse{State: "cancelled"})
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
	writeJSON(w, http.StatusOK, grantValuesResponse{ExpiresAt: expires, ProxyOnly: proxyOnly, Values: values})
}

// revokeGrant ends a grant for the session that holds it — session proof only, per the shared
// broker contract (human revocation is the UI's revoke-by-approver route, which Dispatch's server
// calls with the human's login).
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
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}
