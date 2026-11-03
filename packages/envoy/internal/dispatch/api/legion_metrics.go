// legion_metrics.go serves the Delivery page's Legion section: the four measures of
// dispatch://INFRA-1077, computed from Dispatch's own event log, the handoffs, and what the
// sampler recorded of the project's Legion daemon.
//
// The handler is the read-and-compute split api/delivery_pipeline.go uses: metrics.Read makes
// every database and GitHub read the answer needs and metrics.Compute turns that population into
// the body without touching either, so the live hub recomputes the same numbers from a population
// it already holds rather than carrying a second copy of the rules.
package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// getLegionMetrics answers GET /api/v1/legion/metrics. Its parameters are `project` (the project
// whose issues are counted), `from` and `to` (RFC3339; with neither, the last 7 days, as
// GET /api/v1/delivery/measures reads its window) and `scope` (all, roots or children).
//
// A call with no `project` reads the first project that names a Legion daemon, so a bare call
// answers 200 against any deployment; a deployment where no project names one answers an empty
// report rather than a refusal, since nothing there has been measured yet.
func (s *server) getLegionMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	from, to, err := parseDeliveryWindow(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	scope, err := metrics.ParseScope(strings.TrimSpace(query.Get("scope")))
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.legionMetricsProject(w, r)
	if !ok {
		return
	}
	from, to = from.UTC(), to.UTC()
	population := &metrics.Population{Project: project, From: from, To: to}
	if project != "" {
		if population, err = metrics.Read(r.Context(), s.deps.Store.Pool, s.deps.GitHub, project, from, to); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	WriteJSON(w, http.StatusOK, metrics.Compute(population, scope, time.Now().UTC()))
}

// legionMetricsProject is the project a metrics call counts: the `project` parameter where it
// names one, which must be a project key of a project that exists, and otherwise the first
// project with a Legion setting — empty where the deployment has none.
func (s *server) legionMetricsProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		settings, err := legion.ListSettings(r.Context(), s.deps.Store.Pool)
		if err != nil {
			s.writeHandlerError(w, err)
			return "", false
		}
		if len(settings) == 0 {
			return "", true
		}
		return settings[0].Project, true
	}
	if !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, projectKeyProblem("project", project))
		return "", false
	}
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select key from projects where key = $1`, project).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return "", false
	}
	return project, true
}
