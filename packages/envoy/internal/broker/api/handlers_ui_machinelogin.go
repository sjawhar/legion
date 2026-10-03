// handlers_ui_machinelogin.go: POST /v1/machine-logins/lookup — resolves a pending machine login
// by its human-readable confirmation code, for the operator's own UI (ruling 13: a direct link
// can never approve a machine login, only the typed code selects it) — and GET
// /v1/launcher-credentials and POST /v1/launcher-credentials/{id}/revoke-by-operator, a person's
// live machine logins and the route that ends one before it expires.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
)

type lookupMachineLoginBody struct {
	// The XXXX-XXXX confirmation code the machine shows, as the approver typed it.
	Code string `json:"code"`
}

// lookupMachineLogin resolves a pending machine login by its human-readable confirmation code —
// the operator's dashboard never needs the machine's opaque pending id — and is the only route
// that selects a machine record. Deciding it takes the same code again (CODE_REQUIRED /
// CODE_MISMATCH), so a direct link to the record can never approve it (ruling 13).
func (s *server) lookupMachineLogin(w http.ResponseWriter, r *http.Request) {
	var body lookupMachineLoginBody
	if !readJSON(w, r, &body, "INVALID_LOOKUP") {
		return
	}
	if body.Code == "" {
		writeError(w, http.StatusBadRequest, "CODE_REQUIRED", "code is required")
		return
	}
	view, err := s.deps.MachineLogin.LookupByCode(r.Context(), body.Code)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		writeError(w, http.StatusNotFound, "NO_SUCH_CODE", "no pending machine login has this code")
		return
	case err != nil:
		writeInternal(w, "lookup machine login", err)
		return
	}
	detail, err := s.deps.Machine.ReadRecord(r.Context(), view.RecordID)
	if err != nil {
		writeInternal(w, "read credential request", err)
		return
	}
	writeJSON(w, http.StatusOK, buildRecordResponse(detail))
}

// launcherCredentialResp is one machine login in GET /v1/launcher-credentials?operator=<email>.
type launcherCredentialResp struct {
	// The launcher credential's id, which the revoke route takes.
	CredentialID string `json:"credential_id"`
	// The machine's host name, as its login named it.
	Host string `json:"host"`
	// When the person approved the login.
	IssuedAt time.Time `json:"issued_at"`
	// When the credential expires; the broker has no renewal.
	ExpiresAt time.Time `json:"expires_at"`
}

// launcherCredentialsResponse is GET /v1/launcher-credentials's answer.
type launcherCredentialsResponse struct {
	// The named person's live machine logins, newest first.
	Credentials []launcherCredentialResp `json:"credentials"`
}

// listLauncherCredentials lists the live machine logins whose operator is the person ?operator=
// names: every launcher credential of theirs that is neither revoked nor expired.
func (s *server) listLauncherCredentials(w http.ResponseWriter, r *http.Request) {
	operator := r.URL.Query().Get("operator")
	if !requireOperator(w, operator) {
		return
	}
	rows, err := s.deps.Enroll.LiveCredentials(r.Context(), operator)
	if err != nil {
		writeInternal(w, "list launcher credentials", err)
		return
	}
	out := make([]launcherCredentialResp, len(rows))
	for i, c := range rows {
		out[i] = launcherCredentialResp{CredentialID: c.ID.String(), Host: c.Host, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt}
	}
	writeJSON(w, http.StatusOK, launcherCredentialsResponse{Credentials: out})
}

// revokeByOperatorBody is {"operator"}: the revoking person's Dispatch email, which Dispatch's
// server sets from its own session.
type revokeByOperatorBody struct {
	// The email of the person revoking, who must be the machine login's operator.
	Operator string `json:"operator"`
}

// revokeLauncherCredential ends a machine login before it expires, on its operator's word
// (enroll.Service.RevokeCredential): no launcher proof signed with it authenticates again, and
// every session it enrolled ends, with their grants and pending requests. Revoking one already
// revoked answers the same.
func (s *server) revokeLauncherCredential(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id", "CREDENTIAL_ID_INPUT", "launcher credential")
	if !ok {
		return
	}
	var body revokeByOperatorBody
	if !readJSON(w, r, &body, "INVALID_REVOKE") {
		return
	}
	if !requireOperator(w, body.Operator) {
		return
	}
	err := s.deps.Enroll.RevokeCredential(r.Context(), id, body.Operator)
	switch {
	case errors.Is(err, enroll.ErrNoCredential):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	case errors.Is(err, enroll.ErrNotOperator):
		writeError(w, http.StatusForbidden, "NOT_OPERATOR", err.Error())
		return
	case err != nil:
		writeInternal(w, "revoke launcher credential", err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}

// requireOperator refuses an operator email that canonicalizes to nothing with 400
// OPERATOR_REQUIRED: the machine-login routes name the person they act for, as ?operator= or the
// body's operator.
func requireOperator(w http.ResponseWriter, operator string) bool {
	if record.CanonicalLogin(operator) == "" {
		writeError(w, http.StatusBadRequest, "OPERATOR_REQUIRED", "operator is required")
		return false
	}
	return true
}
