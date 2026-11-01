// handlers_ui_pending.go: GET /v1/pending — lists every still-pending credential-request record
// naming an approver, newest first. Part of the routes a listed service calls with its own
// ServiceAccount token (accountAuth), relaying a person's own page: each secret request names the
// broker's own page where that person decides it.
package api

import (
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/record"
)

type pendingEntry struct {
	// The credential-request record's id, which the approval page and the machine login's approve
	// and deny routes take.
	RecordID string `json:"record_id"`
	// "agent_secret" (a secret request) or "launcher_credential" (a machine login).
	Kind string `json:"kind"`
	// The secrets a secret request asks for, or the machine a machine login names.
	Identifiers []string `json:"identifiers"`
	// When it was asked.
	RequestedAt time.Time `json:"requested_at"`
	// The session this request names, read independently of each other (LEGION-587); see
	// recordSessionResp (handlers_ui_records.go), this row's exact shape here too.
	Session recordSessionResp `json:"session"`
	// A secret request's page on the broker, <BROKER_PUBLIC_URL>/approve/<record>, where a person
	// signed in to the broker decides it; absent for a machine login, which is decided with its
	// typed code.
	ApproveURL string `json:"approve_url,omitempty"`
}

// pendingResponse is GET /v1/pending's answer.
type pendingResponse struct {
	// The undecided records the named person decides, newest first.
	Pending []pendingEntry `json:"pending"`
}

func (s *server) listPending(w http.ResponseWriter, r *http.Request, _ string) {
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
		entries[i] = pendingEntry{
			RecordID: row.RecordID, Kind: row.Kind, Identifiers: row.Identifiers, RequestedAt: row.RequestedAt,
			Session: sessionResp(row.Session),
		}
		if row.Kind == record.KindAgentSecret {
			entries[i].ApproveURL = s.approveURL(row.RecordID)
		}
	}
	writeJSON(w, http.StatusOK, pendingResponse{Pending: entries})
}
