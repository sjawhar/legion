// handlers_ui_machinelogin.go: POST /v1/machine-logins/lookup — resolves a pending machine login
// by its human-readable confirmation code, for the operator's own UI (ruling 13: a direct link
// can never approve a machine login, only the typed code selects it) — and GET
// /v1/launcher-credentials and POST /v1/launcher-credentials/{id}/revoke-by-approver, the machine
// logins a person approved that can still reach a secret, and the route that ends one.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
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

// launcherCredentialResp is one machine login in GET /v1/launcher-credentials?approver=<email>.
type launcherCredentialResp struct {
	// The launcher credential's id, which the revoke route takes.
	CredentialID string `json:"credential_id"`
	// The machine's host name, as its login named it.
	Host string `json:"host"`
	// The service a service's login is for (legion-daemon), whose sessions are its pods; null for
	// a person's own machine.
	Service *string `json:"service"`
	// When the person approved the login.
	IssuedAt time.Time `json:"issued_at"`
	// When the credential expires; the broker has no renewal.
	ExpiresAt time.Time `json:"expires_at"`
	// True once expires_at has passed: the login enrolls nothing more, but sessions it enrolled
	// still run (each renews with its own key), and revoking it ends them.
	Expired bool `json:"expired"`
}

// launcherCredentialsResponse is GET /v1/launcher-credentials's answer.
type launcherCredentialsResponse struct {
	// The machine logins the named person approved that are not revoked and are either unexpired
	// or expired with a session still running, newest first.
	Credentials []launcherCredentialResp `json:"credentials"`
}

// listLauncherCredentials lists the machine logins the person ?approver= names approved, their own
// machines' and any service's, that can still reach a secret: not revoked, and either unexpired or
// expired with a session it enrolled still running (enroll.Service.LiveCredentials).
func (s *server) listLauncherCredentials(w http.ResponseWriter, r *http.Request) {
	approver := r.URL.Query().Get("approver")
	if !requireApprover(w, approver) {
		return
	}
	rows, err := s.deps.Enroll.LiveCredentials(r.Context(), approver)
	if err != nil {
		writeInternal(w, "list launcher credentials", err)
		return
	}
	out := make([]launcherCredentialResp, len(rows))
	for i, c := range rows {
		out[i] = launcherCredentialResp{CredentialID: c.ID.String(), Host: c.Host, Service: c.Service, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt, Expired: c.Expired}
	}
	writeJSON(w, http.StatusOK, launcherCredentialsResponse{Credentials: out})
}

// revokeLauncherCredential ends a machine login, expired or not, on the word of the person who
// approved it (enroll.Service.RevokeCredential): no launcher proof signed with it authenticates
// again, and every session it enrolled ends — a service's login's pods among them — with their
// grants and pending requests. Revoking one already revoked answers the same.
func (s *server) revokeLauncherCredential(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id", "CREDENTIAL_ID_INPUT", "launcher credential")
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
	err := s.deps.Enroll.RevokeCredential(r.Context(), id, body.Approver)
	switch {
	case errors.Is(err, enroll.ErrNoCredential):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	case errors.Is(err, enroll.ErrNotApprover):
		writeError(w, http.StatusForbidden, "NOT_APPROVER", err.Error())
		return
	case err != nil:
		writeInternal(w, "revoke launcher credential", err)
		return
	}
	writeJSON(w, http.StatusOK, stateResponse{State: "revoked"})
}
