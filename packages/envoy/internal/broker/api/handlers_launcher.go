// packages/envoy/internal/broker/api/handlers_launcher.go
//
// POST /v1/launcher-credentials and GET /v1/launcher-credentials/{pending} are authNone per the
// routes table (a launcher has no credential yet when it asks for one), but the service that
// actually issues them through a Dispatch standing-issue ask, launcher.Service, is Task 12's
// deliverable and does not exist yet — api.Deps.Launcher is always nil in this task's wiring.
// Registering the routes now (rather than omitting them from routes()) keeps routes_table.go
// exactly as given and gives Task 12 a real handler body to fill in instead of a routing gap to
// notice and fix; until then both answer a clear 501 rather than panicking on a nil Launcher.
package api

import "net/http"

func (s *server) requestLauncherCredential(w http.ResponseWriter, r *http.Request) {
	if s.deps.Launcher == nil {
		writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "launcher credential issuance is not yet available")
		return
	}
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "launcher credential issuance is not yet available")
}

func (s *server) readLauncherCredential(w http.ResponseWriter, r *http.Request) {
	if s.deps.Launcher == nil {
		writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "launcher credential issuance is not yet available")
		return
	}
	writeError(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "launcher credential issuance is not yet available")
}
