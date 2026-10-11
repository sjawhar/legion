package api

import (
	"errors"
	"net/http"
	"time"

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
	// Once the login is approved, the moment the minted credential expires; absent before then.
	// Renewing it takes a new machine login a human approves.
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
