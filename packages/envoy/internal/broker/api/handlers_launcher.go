package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// launcherLoginKey is the per-login bucket a person's machine login spends: person:<login>, the
// person its login_hint names. A request naming a service never reaches the limiter
// (spendDeviceLoginBucket refuses it first).
func launcherLoginKey(obj record.RequestObject) string {
	return "person:" + record.CanonicalLogin(obj.LoginHint)
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
// A login whose token verified and that the broker then could not complete (5xx, such as one the
// database was away for) gives back what it spent of both: the Legion daemon retries such a login
// every few seconds through a database outage, and the broker decided nothing about its request,
// so there is nothing to slow, and spent, those retries would leave the daemon's first login after
// the outage refused 429. A refused credential (any 4xx) keeps what it spent, since the limits
// exist to slow guessing, and a token that does not verify always keeps its address's request,
// whatever it is answered, since that bucket is what guards the verifier's fetches of the issuer's
// keys.
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
	giveBackAddress, refused := s.launcherLimiter.refuseAddress(w, r)
	if refused {
		return
	}
	login := s.deps.MachineLogin
	account, err := login.VerifyServiceToken(r.Context(), body.Token)
	if err != nil {
		writeTokenLoginRefusal(w, err)
		return
	}
	giveBackAccount, refused := s.launcherLimiter.refuseAccount(w, account.Subject)
	if refused {
		return
	}
	cred, err := login.TokenLogin(r.Context(), account, body.Request)
	if err != nil {
		if writeTokenLoginRefusal(w, err) {
			giveBackAccount()
			giveBackAddress()
		}
		return
	}
	writeJSON(w, http.StatusOK, tokenLoginResponse{CredentialID: cred.ID, ExpiresAt: cred.ExpiresAt})
}

// writeTokenLoginRefusal answers a token login's refusal with the code tokenLogin documents, and
// reports couldNotComplete when it answered 500: a refusal that is none of the documented ones,
// such as the database being away, which decided nothing about the token. tokenLogin gives back
// what the login spent of its buckets on that report only once the token has verified.
func writeTokenLoginRefusal(w http.ResponseWriter, err error) (couldNotComplete bool) {
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
		return true
	}
	return false
}
