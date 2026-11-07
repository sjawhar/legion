// handlers_ui_machinelogin.go: POST /v1/machine-logins/lookup — resolves a pending machine login
// by its human-readable confirmation code, for the operator's own page (ruling 13: a direct link
// can never approve a machine login, only the typed code selects it) — and GET
// /v1/launcher-credentials and POST /v1/launcher-credentials/{id}/revoke-by-approver, the machine
// logins a person may revoke that can still reach a secret, and the route that ends one. Part of
// the routes a listed service calls with its own ServiceAccount token (accountAuth); a machine
// login is decided on the caller's word with its typed code (handlers_ui_records.go).
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/wire"
)

// lookupMachineLoginBody is wire.MachineLookupRequest under the name cmd/broker-refgen reads
// from the call site (bodies.go), as the package's eight response aliases are.
type lookupMachineLoginBody = wire.MachineLookupRequest

// lookupMachineLogin resolves a pending machine login by its human-readable confirmation code, the
// user code the machine's device authorization minted, and is the only route
// that selects a machine record. Deciding it takes the same code again (CODE_REQUIRED /
// CODE_MISMATCH), so a direct link to the record can never approve it (ruling 13).
func (s *server) lookupMachineLogin(w http.ResponseWriter, r *http.Request, _ string) {
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

// launcherCredentialsResponse is GET /v1/launcher-credentials's and GET /v1/operator/machines's
// answer: wire.LauncherCredentialsResponse itself, named here because the broker's generated
// HTTP reference documents a route's answer from a type this package declares, as
// introspectionResponse names oauthwire.Introspection (handlers_introspect.go).
type launcherCredentialsResponse = wire.LauncherCredentialsResponse

// listLauncherCredentials lists the machine logins the person ?approver= names may revoke
// (enroll.Service.LiveCredentials, through writeLauncherCredentials).
func (s *server) listLauncherCredentials(w http.ResponseWriter, r *http.Request, _ string) {
	approver := r.URL.Query().Get("approver")
	if !requireApprover(w, approver) {
		return
	}
	s.writeLauncherCredentials(w, r, s.deps.Enroll.LiveCredentials, approver)
}

// writeLauncherCredentials answers the machine logins list names for person, each one that can
// still reach a secret: not revoked, and either unexpired or expired with a session it enrolled
// still running. Dispatch's machine-logins page lists every service's login, whoever approved it,
// and the person's own machines' (enroll.Service.LiveCredentials); the person's own machine lists
// their machines' alone (enroll.Service.OwnLiveCredentials). Both answer through it, so each row
// the two share is the same bytes.
func (s *server) writeLauncherCredentials(w http.ResponseWriter, r *http.Request, list func(context.Context, string) ([]enroll.LiveCredential, error), person string) {
	rows, err := list(r.Context(), person)
	if err != nil {
		writeInternal(w, "list launcher credentials", err)
		return
	}
	out := make([]wire.LauncherCredentialResp, len(rows))
	for i, c := range rows {
		out[i] = wire.LauncherCredentialResp{CredentialID: c.ID.String(), Host: c.Host, Service: c.Service, Subject: c.Subject, ApprovedBy: c.ApprovedBy, IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt, Expired: c.Expired}
	}
	writeJSON(w, http.StatusOK, launcherCredentialsResponse{Credentials: out})
}

// revokeLauncherCredential ends a machine login as the person the caller names
// (enroll.Service.RevokeCredential, through revokeCredentialAs): a service's as anyone signed in, a
// person's machine's as the person who approved it. The caller that relayed it is logged beside
// that person, whose name the broker still takes on the caller's word: a revoke only takes power
// away.
func (s *server) revokeLauncherCredential(w http.ResponseWriter, r *http.Request, callerName string) {
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
	slog.Info("machine login revocation relayed", "caller", callerName, "approver", body.Approver, "credential", id)
	s.revokeCredentialAs(w, r, s.deps.Enroll.RevokeCredential, id, body.Approver, record.HumanActor(body.Approver))
}

// revokeCredentialAs ends machine login id, expired or not, with revoke on the word of person
// (enroll.Service.RevokeCredential: a service's for anyone, a person's machine's for the person who
// approved it; RevokeOwnCredential: a person's machine's alone), recording actor on every row it
// writes: no launcher proof signed with it authenticates again, and every session it enrolled ends
// — a service's login's pods among them — with their grants and pending requests. Revoking one
// already revoked answers the same.
func (s *server) revokeCredentialAs(w http.ResponseWriter, r *http.Request, revoke func(ctx context.Context, id, person, actor string) error, id, person, actor string) {
	err := revoke(r.Context(), id, person, actor)
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
