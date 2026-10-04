// handlers_ui_machinelogin.go: POST /v1/machine-logins/lookup — resolves a pending machine login
// by its human-readable confirmation code, for the operator's own UI (a direct link can never
// approve a machine login, only the typed code selects it).
package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/machine"
)

type lookupMachineLoginBody struct {
	// The XXXX-XXXX confirmation code the machine shows, as the approver typed it.
	Code string `json:"code"`
}

// lookupMachineLogin resolves a pending machine login by its human-readable confirmation code —
// the operator's dashboard never needs the machine's opaque pending id — and is the only route
// that selects a machine record. Deciding it takes the same code again (CODE_REQUIRED /
// CODE_MISMATCH), so a direct link to the record can never approve it.
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
	writeJSON(w, http.StatusOK, buildRecordResponse(detail))
}
