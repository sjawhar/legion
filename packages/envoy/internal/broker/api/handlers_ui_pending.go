// handlers_ui_pending.go: GET /v1/pending — lists every still-pending credential-request record
// naming an approver, newest first. Part of the routes a listed service calls with its own
// ServiceAccount token (accountAuth), relaying a person's own page: each secret request names the
// broker's own page where that person decides it.
package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/wire"
)

// pendingResponse is GET /v1/pending's answer: wire.PendingResponse itself, named here because
// the broker's generated HTTP reference documents a route's answer from a type this package
// declares, as introspectionResponse names oauthwire.Introspection (handlers_introspect.go).
type pendingResponse = wire.PendingResponse

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
	entries := make([]wire.PendingEntry, len(rows))
	for i, row := range rows {
		entries[i] = wire.PendingEntry{
			RecordID: row.RecordID, Kind: row.Kind, Identifiers: row.Identifiers, RequestedAt: row.RequestedAt,
			Session: sessionResp(row.Session),
		}
		if row.Kind == record.KindAgentSecret {
			entries[i].ApproveURL = s.approveURL(row.RecordID)
		}
	}
	writeJSON(w, http.StatusOK, pendingResponse{Pending: entries})
}
