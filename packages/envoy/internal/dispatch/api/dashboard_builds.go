package api

import (
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/processes"
)

// dashboardBuilds is GET /api/v1/dashboard/builds: the entry chunk of the dashboard build this
// process serves ("" when it serves none), and the distinct entry chunks of the builds every live
// server process serves, sorted. A tab offers "A new version is available" only when live names
// exactly one build and it is not the tab's own: while a rolling update or a rollback runs two
// builds, each tab keeps the page it has, since a reload could land on either.
type dashboardBuilds struct {
	Serving string   `json:"serving"`
	Live    []string `json:"live"`
}

// getDashboardBuilds answers which dashboard builds are being served. Live is the registry of live
// server processes (processes.Live, heartbeats on the database's clock) joined to the builds those
// processes published at boot (dashboard_builds, migration 0122) on the build id: a process whose
// build has no row, or a row whose page names no entry chunk, adds nothing.
func (s *server) getDashboardBuilds(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	live, err := processes.Live(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	builds := make([]string, 0, len(live))
	for _, process := range live {
		if process.DashboardBuild != "" && !slices.Contains(builds, process.DashboardBuild) {
			builds = append(builds, process.DashboardBuild)
		}
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select entry from dashboard_builds
		where build = any($1::text[]) and entry <> ''
		group by entry
		order by entry collate "C"`, builds)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	response := dashboardBuilds{Serving: s.deps.DashboardEntry, Live: []string{}}
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		response.Live = append(response.Live, entry)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, response)
}
