// handlers_ui_pending.go: GET /v1/pending — lists every still-pending credential-request record
// naming an approver, newest first. Part of the UI routes (uiAuth) Dispatch's server relays to on
// behalf of the browser; see handlers_ui_records.go for the shared trust-model note.
package api

import (
	"net/http"
	"time"
)

type pendingEntry struct {
	RecordID    string    `json:"record_id"`
	Kind        string    `json:"kind"`
	Identifiers []string  `json:"identifiers"`
	RequestedAt time.Time `json:"requested_at"`
}

func (s *server) listPending(w http.ResponseWriter, r *http.Request) {
	approver := r.URL.Query().Get("approver")
	if approver == "" {
		writeError(w, http.StatusBadRequest, "APPROVER_REQUIRED", "approver is required")
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
	writeJSON(w, http.StatusOK, map[string]any{"pending": entries})
}
