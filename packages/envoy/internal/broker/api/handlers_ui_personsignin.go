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
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/store"
)

// personSignInResp is one live sign-in in GET /v1/person-signins?person=<email>.
type personSignInResp struct {
	// The sign-in's id, which the revoke route takes.
	SignInID string `json:"signin_id"`
	// The Google account of the machine the person signed in on, as BROKER_VM_SERVICES lists it:
	// the fleet's account, which is what names the box on the person's own list.
	Machine string `json:"machine"`
	// The RFC 7638 thumbprint of the box key the sign-in is bound to, which is what tells this
	// sign-in from the person's others on the same fleet. It is public: it is what the box's
	// DPoP proofs already carry.
	Thumbprint string `json:"thumbprint"`
	// When the person first signed in on that box.
	CreatedAt time.Time `json:"created_at"`
	// When that box last traded a token under this sign-in.
	LastUsedAt time.Time `json:"last_used_at"`
}

// personSignInsResponse is GET /v1/person-signins's answer.
type personSignInsResponse struct {
	// The named person's live sign-ins, newest first: one per box key they signed in with and
	// have not revoked. A revoked sign-in is not listed, and neither is anyone else's.
	SignIns []personSignInResp `json:"signins"`
}

// personSignInRevokedResponse is POST /v1/person-signins/{id}/revoke's answer.
type personSignInRevokedResponse struct {
	// Always "revoked": the sign-in no longer trades, whether this call ended it or an earlier
	// one did.
	State string `json:"state"`
	// How many of that person's live access tokens this call ended with it, the ones minted on
	// that box under that sign-in. It is 0 for a sign-in revoked already, and for one whose
	// tokens had all expired.
	TokensEnded int `json:"tokens_ended"`
}

// listPersonSignIns lists the live sign-ins the person ?person= names made on their machines
// (store.Store.PersonSignIns). The person is the one the caller names, which Dispatch filled
// from its own session, exactly as the machine-login and grant lists take theirs.
func (s *server) listPersonSignIns(w http.ResponseWriter, r *http.Request, _ string) {
	person := r.URL.Query().Get("person")
	if !requirePerson(w, person) {
		return
	}
	rows, err := s.deps.Enroll.Store.PersonSignIns(r.Context(), person)
	if err != nil {
		writeInternal(w, "list person sign-ins", err)
		return
	}
	out := make([]personSignInResp, len(rows))
	for i, in := range rows {
		out[i] = personSignInResp{
			SignInID: in.ID, Machine: in.MachineAccount, Thumbprint: in.Thumbprint,
			CreatedAt: in.CreatedAt, LastUsedAt: in.LastUsedAt,
		}
	}
	writeJSON(w, http.StatusOK, personSignInsResponse{SignIns: out})
}

// revokePersonSignIn ends one sign-in of the person the body names
// (store.Store.RevokePersonSignIn): that box key trades nothing more, and every access token
// already minted under it ends in the revoke's own transaction, so nothing it minted outlives
// it. The person's sign-ins on their other boxes are untouched, and the box signs in again with
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
	ended, err := s.deps.Enroll.Store.RevokePersonSignIn(r.Context(), id, body.Person)
	switch {
	case errors.Is(err, store.ErrNoSignIn):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no sign-in of that person has that id")
		return
	case err != nil:
		writeInternal(w, "revoke person sign-in", err)
		return
	}
	slog.Info("person sign-in revocation relayed", "caller", callerName, "person", record.CanonicalLogin(body.Person), "signin", id, "tokens_ended", ended)
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
