// handlers_operator.go: GET /v1/operator/machines, POST /v1/operator/machines/{id}/revoke,
// POST /v1/operator/machines/{id}/retire, GET /v1/operator/grants and
// POST /v1/operator/grants/{id}/revoke — a person's own machine logins and live grants, listed and
// ended from their machine under its machine login (launcherAuth). The grant routes answer as
// Dispatch's Live grants page does through the UI routes; the machine routes cover the person's
// own machines' logins alone, a subset of Dispatch's machine-logins page, which also lists every
// service's login, so ending the Legion daemon's login stays a Dispatch sign-in's. Each acts for
// the calling credential's operator, and each revoke records that machine login as its actor,
// since its launcher proof, not a Dispatch sign-in, is what authenticated it. A retire is the
// calling login ending itself after its machine logged in again, leaving its sessions running.
package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// operatorOf answers the person a launcher credential acts for, canonicalized. A service's login
// (the Legion daemon's) has none: machine and grant self-service is a person's.
func operatorOf(w http.ResponseWriter, cred enroll.Credential) (string, bool) {
	if cred.Operator == nil {
		writeError(w, http.StatusForbidden, "SERVICE_CREDENTIAL", "a service's machine login has no operator; machine and grant commands are a person's")
		return "", false
	}
	return record.CanonicalLogin(*cred.Operator), true
}

// listOperatorMachines lists the calling credential's operator's own machines' logins, as
// GET /v1/launcher-credentials lists them for that person but without any service's login: not
// revoked, and unexpired or expired with a session still running.
func (s *server) listOperatorMachines(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	operator, ok := operatorOf(w, cred)
	if !ok {
		return
	}
	s.writeLauncherCredentials(w, r, s.deps.Enroll.OwnLiveCredentials, operator)
}

// revokeOperatorMachine ends one of the calling credential's operator's own machines' logins, as
// POST /v1/launcher-credentials/{id}/revoke-by-approver ends it for that person, recording the
// calling machine login as the actor. A service's login is not found here (404 NOT_FOUND), as an
// unknown id is. Revoking the calling credential itself ends this machine's own access.
func (s *server) revokeOperatorMachine(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	operator, ok := operatorOf(w, cred)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "id", "CREDENTIAL_ID_INPUT", "launcher credential")
	if !ok {
		return
	}
	s.revokeCredentialAs(w, r, s.deps.Enroll.RevokeOwnCredential, id, operator, record.LauncherActor(cred.ID.String()))
}

// retireOperatorMachine retires the calling credential, the login a newer login of its machine
// replaced (enroll.Service.RetireCredential): it enrolls nothing more and cannot refresh, while the
// sessions it enrolled keep running. {id} must name the calling credential (403
// NOT_CALLING_CREDENTIAL otherwise), so a machine retires only its own login. 204 on success,
// retiring one already ended included.
func (s *server) retireOperatorMachine(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	id, ok := pathUUID(w, r, "id", "CREDENTIAL_ID_INPUT", "launcher credential")
	if !ok {
		return
	}
	if id != cred.ID.String() {
		writeError(w, http.StatusForbidden, "NOT_CALLING_CREDENTIAL", "a machine login retires only itself: sign with the credential the path names")
		return
	}
	if err := s.deps.Enroll.RetireCredential(r.Context(), id, record.LauncherActor(id)); err != nil {
		writeInternal(w, "retire launcher credential", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listOperatorGrants lists the live grants of the calling credential's operator's sessions and
// those the operator approved, as GET /v1/grants lists them for that person.
func (s *server) listOperatorGrants(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	operator, ok := operatorOf(w, cred)
	if !ok {
		return
	}
	s.writeApproverGrants(w, r, operator)
}

// revokeOperatorGrant ends a grant as the calling credential's operator, as
// POST /v1/grants/{id}/revoke-by-approver ends it for that person — the operator's revoke, which
// withholds the secrets the grant's request got automatically from its session — recording the
// calling machine login as the actor.
func (s *server) revokeOperatorGrant(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	operator, ok := operatorOf(w, cred)
	if !ok {
		return
	}
	id, ok := pathUUID(w, r, "id", "GRANT_ID_INPUT", "grant")
	if !ok {
		return
	}
	s.revokeGrantAs(w, r, id, operator, record.LauncherActor(cred.ID.String()))
}
