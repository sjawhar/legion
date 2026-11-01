package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

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

// LiveDashboardBuilds is the distinct dashboard builds (routes.DashboardBuild's ids) the live
// server processes serve (processes.Live), in the order Live first names each, and never nil, so a
// query given it binds an empty array rather than SQL null. A process that serves no dashboard
// adds nothing. It lives here rather than in package dashboard, which imports this package through
// routes, so this handler and the boot's prune in cmd/dispatch share it.
func LiveDashboardBuilds(ctx context.Context, db processes.Querier) ([]string, error) {
	live, err := processes.Live(ctx, db)
	if err != nil {
		return nil, err
	}
	builds := make([]string, 0, len(live))
	for _, process := range live {
		if process.DashboardBuild != "" && !slices.Contains(builds, process.DashboardBuild) {
			builds = append(builds, process.DashboardBuild)
		}
	}
	return builds, nil
}

// getDashboardBuilds answers which dashboard builds are being served. Live is the registry of live
// server processes (processes.Live, heartbeats on the database's clock) joined to the builds those
// processes published at boot (dashboard_builds, migration 0136) on the build id: a process whose
// build has no row, or a row whose page names no entry chunk, adds nothing.
func (s *server) getDashboardBuilds(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	builds, err := LiveDashboardBuilds(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select entry from dashboard_builds
		where build = any($1::text[]) and entry <> ''
		group by entry
		order by entry collate "C"`, builds)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	// CollectRows answers a non-nil slice when there are no rows, which keeps "live":[] in the JSON.
	entries, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, dashboardBuilds{Serving: s.deps.DashboardEntry, Live: entries})
}
