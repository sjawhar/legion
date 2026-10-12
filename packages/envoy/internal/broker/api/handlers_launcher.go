package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// machineLoginBody is POST /v1/launcher-credentials's exact shape: a machine request object with
// one launcher_credential detail and a login_hint naming the approving operator.
type machineLoginBody struct {
	// The machine's signed login request: a compact ES256 JWS carrying the machine's new public
	// key, signed with that key. A person's machine login names that person in its login_hint, and
	// only they approve it.
	Request string `json:"request"`
}

// machineLoginResponse is POST /v1/launcher-credentials's answer.
type machineLoginResponse struct {
	// The opaque id the machine polls the login by; only the machine knows it.
	PendingID string `json:"pending_id"`
	// The XXXX-XXXX confirmation code the machine shows and the approver types into Dispatch.
	Code string `json:"code"`
}

// machineLoginStateResponse is GET /v1/launcher-credentials/{pending}'s answer.
type machineLoginStateResponse struct {
	// Once the login is approved, the minted machine credential's id; absent before then.
	CredentialID string `json:"credential_id,omitempty"`
	// Once the login is approved, the moment the minted credential expires; absent before then.
	// Renewing it takes a new machine login a human approves.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// "pending", "issued" (approved), "denied" or "expired".
	State string `json:"state"`
}

// machineLogin is POST /v1/launcher-credentials: a person's machine login, public, rate-limited
// per source address and per person its login_hint names (limits.go, launcherLoginKey). The
// request object is verified once here — to resolve the key the rate limiter buckets on before any
// state is written — and again, redundantly but harmlessly, by MachineLogin.Login itself; an
// invalid object, and one naming a service (a service logs in with its token, tokenLogin), are
// refused 400 at the first check and never reach the limiter or the store, and one Login refuses
// (a login naming no one, a replayed jti) is refused 400 too.
func (s *server) machineLogin(w http.ResponseWriter, r *http.Request) {
	var body machineLoginBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	obj, err := record.VerifyRequestObject(body.Request, s.deps.MachineLogin.Audience, s.deps.MachineLogin.Skew, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", err.Error())
		return
	}
	if obj.Service() != "" {
		writeError(w, http.StatusBadRequest, "SERVICE_LOGS_IN_WITH_TOKEN", machine.ErrServiceLogsInWithToken.Error())
		return
	}
	if s.launcherLimiter.refuse(w, r, launcherLoginKey(obj)) {
		return
	}
	pendingID, code, err := s.deps.MachineLogin.Login(r.Context(), body.Request)
	switch {
	case errors.Is(err, machine.ErrServiceLogsInWithToken):
		writeError(w, http.StatusBadRequest, "SERVICE_LOGS_IN_WITH_TOKEN", err.Error())
		return
	case errors.Is(err, record.ErrRequestInvalid):
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "machine login", err)
		return
	}
	writeJSON(w, http.StatusAccepted, machineLoginResponse{PendingID: pendingID, Code: code})
}

// launcherLoginKey is the per-login bucket a person's machine login spends: person:<login>, the
// person its login_hint names. A request naming a service never reaches the limiter (machineLogin
// refuses it first).
func launcherLoginKey(obj record.RequestObject) string {
	return "person:" + record.CanonicalLogin(obj.LoginHint)
}

// readMachineLogin is GET /v1/launcher-credentials/{pending}: public, no rate limit (an opaque
// capability the machine itself minted, not a caller-chosen key). No token is ever returned; the
// minted credential is usable only with proofs signed by the key the request object embedded.
func (s *server) readMachineLogin(w http.ResponseWriter, r *http.Request) {
	pendingID := r.PathValue("pending")
	state, credentialID, expiresAt, err := s.deps.MachineLogin.Read(r.Context(), pendingID)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such pending machine login")
		return
	case err != nil:
		writeInternal(w, "read machine login", err)
		return
	}
	resp := machineLoginStateResponse{CredentialID: credentialID, State: state}
	if state == "issued" {
		resp.ExpiresAt = new(expiresAt.UTC())
	}
	writeJSON(w, http.StatusOK, resp)
}

// tokenLoginBody is POST /v1/launcher-credentials/token's exact shape.
type tokenLoginBody struct {
	// The service's projected Kubernetes ServiceAccount token, whose audience is the broker's
	// (BROKER_K8S_OIDC_AUDIENCE) and whose account BROKER_SERVICE_LOGINS names.
	Token string `json:"token"`
	// A machine request object signed by the key the credential is bound to: a compact ES256 JWS
	// carrying that public key, with one launcher_credential detail naming the account's service.
	Request string `json:"request"`
}

// tokenLoginResponse is POST /v1/launcher-credentials/token's answer.
type tokenLoginResponse struct {
	// The launcher credential's id, which every launcher proof the key signs names.
	CredentialID string `json:"credential_id"`
	// When the credential expires: the token's own expiry, at most an hour after the login. The
	// service logs in again before then with its current token, and no person approves it.
	ExpiresAt time.Time `json:"expires_at"`
}

// tokenLogin is POST /v1/launcher-credentials/token: public. A service (the Legion daemon) logs in
// with its own projected ServiceAccount token, and no person approves it (machine.Service's
// VerifyServiceToken, then TokenLogin). The account's one launcher credential moves to the
// request's key and the token's expiry. Every token login spends its source address's bucket before
// the token is verified (refuseAddress), so no flood of tokens, forged or real, costs the broker a
// verification (and a fetch of the issuer's keys) past that bucket; a verified account then spends a
// bucket of its own too (refuseAccount). The pair is the device-code route's, address then login.
// A token that has expired is 401 TOKEN_EXPIRED, one that does not verify 401 TOKEN_INVALID, one
// that lives longer than an hour from its issue 401 TOKEN_TOO_LONG_LIVED, one whose account
// BROKER_SERVICE_LOGINS does not name
// for the requested service 403 ACCOUNT_NOT_LISTED, a request object that does not verify or
// repeats a jti 400 REQUEST_INVALID, a key another live credential holds 409
// KEY_HOLDS_LIVE_CREDENTIAL, and a broker that trusts no Kubernetes token issuer answers 404
// TOKEN_LOGIN_OFF.
func (s *server) tokenLogin(w http.ResponseWriter, r *http.Request) {
	var body tokenLoginBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	if s.launcherLimiter.refuseAddress(w, r) {
		return
	}
	login := s.deps.MachineLogin
	account, err := login.VerifyServiceToken(r.Context(), body.Token)
	if err != nil {
		writeTokenLoginRefusal(w, err)
		return
	}
	if s.launcherLimiter.refuseAccount(w, account.Subject) {
		return
	}
	cred, err := login.TokenLogin(r.Context(), account, body.Request)
	if err != nil {
		writeTokenLoginRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenLoginResponse{CredentialID: cred.ID, ExpiresAt: cred.ExpiresAt})
}

// writeTokenLoginRefusal answers a token login's refusal with the code tokenLogin documents.
func writeTokenLoginRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, machine.ErrTokenLoginsOff):
		writeError(w, http.StatusNotFound, "TOKEN_LOGIN_OFF", err.Error())
	case errors.Is(err, machine.ErrTokenExpired):
		writeError(w, http.StatusUnauthorized, "TOKEN_EXPIRED", err.Error())
	case errors.Is(err, machine.ErrTokenInvalid):
		writeError(w, http.StatusUnauthorized, "TOKEN_INVALID", err.Error())
	case errors.Is(err, machine.ErrTokenTooLongLived):
		writeError(w, http.StatusUnauthorized, "TOKEN_TOO_LONG_LIVED", err.Error())
	case errors.Is(err, machine.ErrAccountNotListed):
		writeError(w, http.StatusForbidden, "ACCOUNT_NOT_LISTED", err.Error())
	case errors.Is(err, record.ErrRequestInvalid):
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", err.Error())
	case errors.Is(err, machine.ErrKeyHoldsLiveCredential):
		writeError(w, http.StatusConflict, "KEY_HOLDS_LIVE_CREDENTIAL", err.Error())
	default:
		writeInternal(w, "token login", err)
	}
}
