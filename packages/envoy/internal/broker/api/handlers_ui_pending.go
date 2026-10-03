// handlers_ui_pending.go: GET /v1/pending — lists every still-pending credential-request record
// naming an approver, newest first. Part of the UI routes (uiAuth) Dispatch's server relays to on
// behalf of the browser; see handlers_ui_records.go for the shared trust-model note.
package api

import (
	"net/http"
	"time"
)

type pendingEntry struct {
	// The credential-request record's id, which the record, approve and deny routes take.
	RecordID string `json:"record_id"`
	// "agent_secret" (a secret request) or "launcher_credential" (a machine login).
	Kind string `json:"kind"`
	// The secrets a secret request asks for, or the machine a machine login names.
	Identifiers []string `json:"identifiers"`
	// When it was asked.
	RequestedAt time.Time `json:"requested_at"`
}

// pendingResponse is GET /v1/pending's answer.
type pendingResponse struct {
	// The undecided records the named person decides, newest first.
	Pending []pendingEntry `json:"pending"`
}

func (s *server) listPending(w http.ResponseWriter, r *http.Request) {
	approver := r.URL.Query().Get("approver")
	if !requireApprover(w, approver) {
		return
	}
	rows, err := s.deps.Machine.PendingForApprover(r.Context(), approver)
	if err != nil {
		writeInternal(w, "list pending requests", err)
		return
	}
	entries := make([]pendingEntry, len(rows))
	for i, row := range rows {
		entries[i] = pendingEntry{RecordID: row.RecordID, Kind: row.Kind, Identifiers: row.Identifiers, RequestedAt: row.RequestedAt}
	}
	writeJSON(w, http.StatusOK, pendingResponse{Pending: entries})
}
