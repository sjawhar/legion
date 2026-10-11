package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// machineLoginBody is POST /v1/launcher-credentials's exact shape: a machine request object with
// one launcher_credential detail, which names the service a service's login is for, or, for a
// person's machine, a login_hint naming the approving operator.
type machineLoginBody struct {
	// The machine's signed login request: a compact ES256 JWS carrying the machine's new public
	// key, signed with that key. A service's login names its service, and anyone signed in to
	// Dispatch approves it; a person's machine login names that person in its login_hint, and only
	// they approve it.
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
	// Once the login is approved, the moment the minted credential expires; absent before then. A
	// service's login moves it later with POST /v1/launcher-credentials/{id}/renew; a person's
	// machine logs in again, with a new code its person approves.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// "pending", "issued" (approved), "denied" or "expired".
	State string `json:"state"`
}

// machineLogin is POST /v1/launcher-credentials: public, rate-limited per source address and per
// login (limits.go): every service's login spends one shared bucket, a person's machine login the
// bucket of the person its login_hint names, in key spaces of their own (launcherLoginKey). The request object is
// verified once here — to resolve the key the rate limiter buckets on before any state is written —
// and again, redundantly but harmlessly, by MachineLogin.Login itself; an invalid object is refused
// at the first check and never reaches the limiter or the store, and one Login refuses (a person's
// login naming no one, a replayed jti) is refused 400 too.
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
	if s.launcherLimiter.refuse(w, r, launcherLoginKey(obj)) {
		return
	}
	pendingID, code, err := s.deps.MachineLogin.Login(r.Context(), body.Request)
	switch {
	case errors.Is(err, record.ErrRequestInvalid):
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", err.Error())
		return
	case err != nil:
		writeInternal(w, "machine login", err)
		return
	}
	writeJSON(w, http.StatusAccepted, machineLoginResponse{PendingID: pendingID, Code: code})
}

// launcherLoginKey is the per-login bucket a machine login spends: one shared bucket, "service",
// for every service's login, whatever service or login_hint it names, and person:<login> for a
// person's machine login. A service's name is the machine's own unauthenticated claim (anything
// record.ValidService admits), so a bucket per name would hand a fresh bucket, and a fresh row in
// every signed-in person's pending list, to every name a flood invents. A broker that registers no
// service (BROKER_SERVICES unset) still serves a service's login, so the bucket cannot key on the
// registered set instead. The two key spaces never meet: no person's key is "service".
func launcherLoginKey(obj record.RequestObject) string {
	if obj.Service() != "" {
		return "service"
	}
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

// launcherRenewalBody is POST /v1/launcher-credentials/{id}/renew's exact shape.
type launcherRenewalBody struct {
	// A machine request object signed by the key the credential moves to: a compact ES256 JWS
	// carrying that key, with one launcher_credential detail naming the credential's own service,
	// exactly as a service's machine login posts one.
	Request string `json:"request"`
}

// launcherRenewalResponse is POST /v1/launcher-credentials/{id}/renew's answer.
type launcherRenewalResponse struct {
	// The moment the renewed credential now expires: BROKER_LAUNCHER_CREDENTIAL_SECONDS from the
	// renewal.
	ExpiresAt time.Time `json:"expires_at"`
}

// launcherSelfResponse is GET /v1/launcher-credentials/self's answer: the calling machine login as
// the broker knows it.
type launcherSelfResponse struct {
	// The calling launcher credential's id.
	CredentialID string `json:"credential_id"`
	// The service a service's login is for (legion-daemon); absent for a person's machine.
	Service *string `json:"service,omitempty"`
	// The moment the credential expires.
	ExpiresAt time.Time `json:"expires_at"`
}

// readLauncherSelf is GET /v1/launcher-credentials/self: the calling machine login's credential, as
// its own launcher proof authenticates it, and changing nothing. A launcher that is not sure which
// of two keys the broker holds for its credential (a renewal whose answer it never read) asks with
// each: the one the broker holds is answered 200, the other 401 LAUNCHER_INVALID.
func (s *server) readLauncherSelf(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	expires, err := s.deps.MachineLogin.CredentialExpiry(r.Context(), cred.ID.String())
	if err != nil {
		writeInternal(w, "read launcher credential", err)
		return
	}
	writeJSON(w, http.StatusOK, launcherSelfResponse{CredentialID: cred.ID.String(), Service: cred.Service, ExpiresAt: expires.UTC()})
}

// renewLauncherCredential is POST /v1/launcher-credentials/{id}/renew: a service's machine login
// (the Legion daemon's) moves its own credential to a new key and a full lifetime from now
// (machine.Service.Renew), signed with the current key's launcher proof and carrying a request
// object the new key signed. The login a person approved once then lasts for as long as the service
// keeps renewing it, until someone revokes it, and a copy of an old key stops working at the next
// renewal. The proof-verified credential is authoritative: a URL id naming another credential is
// refused 403 NOT_YOURS, as an enrollment's renewal refuses one. A person's own machine login is
// 403 PERSON_CREDENTIAL, and a service the broker does not register (BROKER_SERVICES) 403
// SERVICE_NOT_REGISTERED, since a service's name is the machine's own claim. A request object that
// does not verify, names another service, repeats a jti or carries the current key is 400
// REQUEST_INVALID, and a new key that already holds another
// live credential 409 KEY_HOLDS_LIVE_CREDENTIAL. A credential revoked or past its expiry by the time
// the renewal reaches the database is 401 LAUNCHER_INVALID, the answer every launcher route gives a
// credential that is not live.
func (s *server) renewLauncherCredential(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	id, ok := pathUUID(w, r, "id", "CREDENTIAL_ID_INPUT", "launcher credential")
	if !ok {
		return
	}
	if id != cred.ID.String() {
		writeError(w, http.StatusForbidden, "NOT_YOURS", "a machine login may only renew its own credential")
		return
	}
	var body launcherRenewalBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	expires, err := s.deps.MachineLogin.Renew(r.Context(), id, body.Request)
	switch {
	case errors.Is(err, machine.ErrPersonCredential):
		writeError(w, http.StatusForbidden, "PERSON_CREDENTIAL", "a person's machine login does not renew; it logs in again with a code its person approves")
		return
	case errors.Is(err, machine.ErrServiceNotRegistered):
		writeError(w, http.StatusForbidden, "SERVICE_NOT_REGISTERED", err.Error())
		return
	case errors.Is(err, machine.ErrCredentialNotLive):
		writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
		return
	case errors.Is(err, record.ErrRequestInvalid):
		writeError(w, http.StatusBadRequest, "REQUEST_INVALID", err.Error())
		return
	case errors.Is(err, machine.ErrKeyHoldsLiveCredential):
		writeError(w, http.StatusConflict, "KEY_HOLDS_LIVE_CREDENTIAL", err.Error())
		return
	case err != nil:
		writeInternal(w, "renew launcher credential", err)
		return
	}
	writeJSON(w, http.StatusOK, launcherRenewalResponse{ExpiresAt: expires})
}
