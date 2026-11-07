// handlers_ui_personsignin.go: GET /v1/person-signins and POST /v1/person-signins/{id}/revoke —
// the sign-ins a person made on their machines with `agent-secrets login`, by machine, and the
// route that ends one. Part of the routes a listed service calls with its own ServiceAccount
// token (accountAuth); the person is the one the caller names, as it is for every list and every
// revoke Dispatch's server relays (handlers_ui_machinelogin.go).
package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/wire"
)

// personSignInsResponse is GET /v1/person-signins's answer: wire.PersonSignInsResponse itself,
// named here because the broker's generated HTTP reference documents a route's answer from a
// type this package declares, as introspectionResponse names oauthwire.Introspection
// (handlers_introspect.go).
type personSignInsResponse = wire.PersonSignInsResponse

// personSignInRevokedResponse is POST /v1/person-signins/{id}/revoke's answer:
// wire.PersonSignInRevokedResponse itself, named here for the same reason.
type personSignInRevokedResponse = wire.PersonSignInRevokedResponse

// listPersonSignIns lists the live sign-ins the person ?person= names made on their machines
// (store.Store.PersonSignIns). The person is the one the caller names, which Dispatch filled
// from its own session, exactly as the machine-login and grant lists take theirs.
//
// The login is canonicalized before it is read on, as the revoke beside it does: the table holds
// lowercase logins and the query lowercases what it is given, so an address a caller spells with
// surrounding space would otherwise match nothing and answer an empty list to a person who has
// sign-ins.
func (s *server) listPersonSignIns(w http.ResponseWriter, r *http.Request, _ string) {
	person := record.CanonicalLogin(r.URL.Query().Get("person"))
	if !requirePerson(w, person) {
		return
	}
	rows, err := s.deps.Enroll.Store.PersonSignIns(r.Context(), person)
	if err != nil {
		writeInternal(w, "list person sign-ins", err)
		return
	}
	out := make([]wire.PersonSignInResp, len(rows))
	for i, in := range rows {
		out[i] = wire.PersonSignInResp{
			SignInID: in.ID, Machine: in.MachineAccount, Thumbprint: in.Thumbprint,
			CreatedAt: in.CreatedAt, LastUsedAt: in.LastUsedAt,
		}
	}
	writeJSON(w, http.StatusOK, personSignInsResponse{SignIns: out})
}

// revokePersonSignIn ends one sign-in of the person the body names
// (enroll.Service.RevokePersonSignIn): that box key trades nothing more, every access token
// already minted under it ends, and the launcher credential the sign-in held ends with every
// session it enrolled — all in the revoke's own transaction, so nothing it gave outlives it.
// The person's sign-ins on their other boxes are untouched, and the box signs in again with
// `agent-secrets login`, which makes a key of its own.
//
// A sign-in of any other person is answered as an unknown id, so nobody learns another person's
// sign-ins by trying ids. The revoking person is the one the request names, which the broker
// takes on the caller's word as every relayed revoke does: a revoke only takes power away.
func (s *server) revokePersonSignIn(w http.ResponseWriter, r *http.Request, callerName string) {
	id, ok := pathUUID(w, r, "id", "SIGNIN_ID_INPUT", "sign-in")
	if !ok {
		return
	}
	var body revokePersonSignInBody
	if !readJSON(w, r, &body, "INVALID_REVOKE") {
		return
	}
	if !requirePerson(w, body.Person) {
		return
	}
	person := record.CanonicalLogin(body.Person)
	ended, err := s.deps.Enroll.RevokePersonSignIn(r.Context(), id, person, record.HumanActor(person))
	switch {
	case errors.Is(err, store.ErrNoSignIn):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no sign-in of that person has that id")
		return
	case err != nil:
		writeInternal(w, "revoke person sign-in", err)
		return
	}
	slog.Info("person sign-in revocation relayed", "caller", callerName, "person", person, "signin", id, "tokens_ended", ended)
	writeJSON(w, http.StatusOK, personSignInRevokedResponse{State: "revoked", TokensEnded: ended})
}

// revokePersonSignInBody is POST /v1/person-signins/{id}/revoke's body.
type revokePersonSignInBody struct {
	// The login of the person whose sign-in it is. A sign-in of anyone else is answered as an
	// unknown id.
	Person string `json:"person"`
}

// requirePerson refuses a request naming no person, as requireApprover does for the routes whose
// subject is a decision's approver. It is a word of its own because these two routes name no
// approver at all: a sign-in is the person's own, and what they do to it is revoke it.
func requirePerson(w http.ResponseWriter, login string) bool {
	if record.CanonicalLogin(login) == "" {
		writeError(w, http.StatusBadRequest, "PERSON_REQUIRED", "person is required")
		return false
	}
	return true
}
