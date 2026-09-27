package api

import (
	"errors"
	"net/http"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/requests"
)

type createRequestBody struct {
	Secrets   []string `json:"secrets"`
	Reason    string   `json:"reason"`
	Issue     *string  `json:"issue"`
	SessionID *string  `json:"session_id"`
}

// createRequestResponse is POST /v1/requests's exact wire shape: request_id, state, secrets,
// grant_id, ask — nothing else. requests.Request itself carries additional fields (decided_at,
// decided_by, detail, coalesced) that this route's contract does not define, so the handler below
// builds this dedicated response rather than marshaling the Request it gets back from
// Machine.Create directly.
type createRequestResponse struct {
	RequestID string                    `json:"request_id"`
	State     string                    `json:"state"`
	Secrets   []requests.SecretDecision `json:"secrets"`
	GrantID   *string                   `json:"grant_id"`
	AskRef    *string                   `json:"ask"`
}

// createRequest requests secrets for the proof-verified caller, never for an enrollment named in
// the body: a session may only ever request secrets for itself.
func (s *server) createRequest(w http.ResponseWriter, r *http.Request, enrollmentID string) {
	var body createRequestBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") || !validSecretNames(w, body.Secrets) {
		return
	}
	issue := derefOr(body.Issue, "")
	if issue != "" && !issueKey.MatchString(issue) {
		writeError(w, http.StatusBadRequest, "ISSUE_INPUT", "issue must be a Dispatch issue key like PROJ-12")
		return
	}
	req, err := s.deps.Machine.Create(r.Context(), enrollmentID, body.Secrets, body.Reason, issue, derefOr(body.SessionID, ""))
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
		writeDispatchFailure(w, "create request", err)
		return
	}
	writeJSON(w, http.StatusOK, createRequestResponse{
		RequestID: req.ID,
		State:     req.State,
		Secrets:   req.Secrets,
		GrantID:   req.GrantID,
		AskRef:    req.AskRef,
	})
}

// validSecretNames refuses, with a 400 naming the problem, a request that names no secret, a name
// that is empty or carries control characters, or the same name twice.
func validSecretNames(w http.ResponseWriter, names []string) bool {
	if len(names) == 0 {
		writeError(w, http.StatusBadRequest, "SECRETS_REQUIRED", "secrets must name at least one secret")
		return false
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		for _, c := range name {
			if unicode.IsControl(c) {
				writeError(w, http.StatusBadRequest, "SECRET_NAME_INPUT", "secret names may not contain control characters")
				return false
			}
		}
		switch {
		case name == "":
			writeError(w, http.StatusBadRequest, "SECRET_NAME_INPUT", "secret names may not be empty")
			return false
		case seen[name]:
			writeError(w, http.StatusBadRequest, "DUPLICATE_SECRET", "secret "+name+" is named more than once")
			return false
		}
		seen[name] = true
	}
	return true
}

// requestDecision is GET /v1/requests/{id}'s nested "decision" object: who decided the request
// and when. requestStatusResponse reshapes requests.Request, whose own JSON tags carry
// decided_by/detail at the top level for POST /v1/requests's response — a different shape than
// this route's contract, which nests who and when here.
type requestDecision struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

type requestStatusResponse struct {
	State     string           `json:"state"`
	GrantID   *string          `json:"grant_id"`
	DecidedAt *time.Time       `json:"decided_at"`
	Decision  *requestDecision `json:"decision"`
	// Detail says why a decided request ended as it did (a denial's reason, a cancellation's).
	Detail *string `json:"detail"`
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
	resp := requestStatusResponse{State: req.State, GrantID: req.GrantID, DecidedAt: req.DecidedAt, Detail: req.Detail}
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

// revokeGrant ends a grant for either its own session (by proof) or a Dispatch-authenticated
// human: a session may end only its own grants; a human only a grant they approved or one whose
// enrollment they operate.
func (s *server) revokeGrant(w http.ResponseWriter, r *http.Request, by requests.Revoker) {
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	err := s.deps.Machine.RevokeGrant(r.Context(), id, by)
	switch {
	case errors.Is(err, requests.ErrNotYours):
		writeError(w, http.StatusForbidden, "NOT_YOURS", err.Error())
		return
	case errors.Is(err, requests.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
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
