package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/record"
)

// machineLoginBody is POST /v1/launcher-credentials's exact shape in the shared broker contract
// (dispatch://AGENTC-393/artifact/plan-overview-md): a machine request object naming login_hint
// (the approving operator) and one launcher_credential detail.
type machineLoginBody struct {
	Request string `json:"request"`
}

type machineLoginResponse struct {
	PendingID string `json:"pending_id"`
	Code      string `json:"code"`
}

// machineLogin is POST /v1/launcher-credentials: public, rate-limited per source address and per
// named operator (limits.go, unchanged from v8). The request object is verified once here — to
// resolve the operator the rate limiter buckets on before any state is written — and again,
// redundantly but harmlessly, by MachineLogin.Login itself; an invalid object is refused at the
// first check and never reaches the limiter or the store.
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
	if s.launcherLimiter.refuse(w, r, record.CanonicalLogin(obj.LoginHint)) {
		return
	}
	pendingID, code, err := s.deps.MachineLogin.Login(r.Context(), body.Request)
	if err != nil {
		writeInternal(w, "machine login", err)
		return
	}
	writeJSON(w, http.StatusAccepted, machineLoginResponse{PendingID: pendingID, Code: code})
}

// readMachineLogin is GET /v1/launcher-credentials/{pending}: public, no rate limit (an opaque
// capability the machine itself minted, not a caller-chosen key). No token is ever returned; the
// minted credential is usable only with proofs signed by the key the request object embedded.
func (s *server) readMachineLogin(w http.ResponseWriter, r *http.Request) {
	pendingID := r.PathValue("pending")
	state, credentialID, err := s.deps.MachineLogin.Read(r.Context(), pendingID)
	switch {
	case errors.Is(err, machine.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such pending machine login")
		return
	case err != nil:
		writeInternal(w, "read machine login", err)
		return
	}
	if state == "issued" {
		writeJSON(w, http.StatusOK, map[string]string{"state": state, "credential_id": credentialID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": state})
}
