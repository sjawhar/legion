// handlers_ui_machinelogin.go: POST /v1/machine-logins/lookup — resolves a pending machine login
// by its human-readable confirmation code, for the operator's own UI (ruling 13: a direct link
// can never approve a machine login, only the typed code selects it).
package api

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/machine"
)

type lookupMachineLoginBody struct {
	Code string `json:"code"`
}

// lookupMachineLogin resolves a pending machine login by its human-readable confirmation code —
// the operator's dashboard never needs the machine's opaque pending id — and is the only route
// that hands out a machine record's approve/deny challenges (ruling 13).
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
	resp := buildRecordResponse(detail)
	ch := challengesResp{
		Approve: base64.RawURLEncoding.EncodeToString(view.ApproveChallenge),
		Deny:    base64.RawURLEncoding.EncodeToString(view.DenyChallenge),
	}
	resp.Challenges = &ch
	writeJSON(w, http.StatusOK, resp)
}
