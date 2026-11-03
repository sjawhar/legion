package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedLegionMetricsCorpus writes one finished Legion child of one root into the last seven days:
// an implementer's event, a parked stretch, the sampler's observations and the close.
func seedLegionMetricsCorpus(t *testing.T, database *store.Store, project string, now time.Time) {
	t.Helper()
	ago := func(hours float64) time.Time { return now.Add(-time.Duration(hours * float64(time.Hour))) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	root := project + "-1001"
	child := project + "-1002"
	exec(`insert into issues (key, project_key, number, title, status, rank, created_by, created_at)
		values ($1, $2, 1001, 'The root', 'in_progress', 'm', '{"kind":"user","id":"alice"}'::jsonb, $3)`, root, project, ago(60))
	exec(`insert into issues (key, project_key, number, title, status, rank, parent_key, created_by, created_at, closed_at)
		values ($1, $2, 1002, 'The child', 'done', 'n', $3, '{"kind":"user","id":"alice"}'::jsonb, $4, $5)`,
		child, project, root, ago(50), ago(24))
	session := func(title string) string {
		return fmt.Sprintf(`{"kind":"session","id":"s-%s","origin":{"session_title":%q}}`, title, title)
	}
	event := func(issue string, seq int, kind, actor, payload string, at time.Time) {
		t.Helper()
		exec(`insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			values ($1, $2, $3, $4::jsonb, $5::jsonb, false, $6)`, issue, seq, kind, actor, payload, at)
	}
	event(root, 1, "comment.created", session("Legion architect · "+root), `{}`, ago(58))
	event(child, 1, "issue.created", session("Legion architect · "+root),
		fmt.Sprintf(`{"status":"triage","parent":%q,"ancestors":[%q]}`, root, root), ago(50))
	event(child, 2, "comment.created", session("Legion implementer · "+child), `{}`, ago(48))
	event(child, 3, "issue.updated", `{"kind":"user","id":"alice"}`, `{"status":"backlog"}`, ago(40))
	event(child, 4, "issue.updated", `{"kind":"user","id":"alice"}`, `{"status":"in_progress"}`, ago(38))
	event(child, 5, "issue.closed", session("Legion merger · "+child), `{"status":"done"}`, ago(24))
	exec(`insert into handoffs (issue_key, phase, version, body, author, created_at)
		values ($1, 'implement', 1, 'done', '{"kind":"session","id":"s-impl"}'::jsonb, $2)`, child, ago(30))
	for _, observation := range []struct {
		field, value string
		at           time.Time
	}{
		{legion.FieldPhase, `"implementing"`, ago(48)},
		{legion.ClaimFieldPrefix + "implementer", `"working"`, ago(48)},
		{legion.ClaimFieldPrefix + "implementer", `"idle"`, ago(44)},
		{legion.FieldPhase, `"done"`, ago(24)},
	} {
		exec(`insert into legion_observations (project, issue_key, field, value, observed_at)
			values ($1, $2, $3, $4::jsonb, $5)`, project, child, observation.field, observation.value, observation.at)
	}
}

// TestLegionMetricsAnswersTheLastSevenDaysWithNoWindow: a bare call, with no query string at all,
// answers 200 over the last seven days of the first project that names a Legion daemon, as
// GET /api/v1/delivery/measures answers its own default window.
func TestLegionMetricsAnswersTheLastSevenDaysWithNoWindow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	if written := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/legion/INFRA",
		map[string]any{"state_url": "http://legion-infra.internal:13370"}, "alice"); written.Code != http.StatusOK {
		t.Fatalf("name INFRA's daemon: status=%d body=%s", written.Code, written.Body.String())
	}
	now := time.Now().UTC()
	seedLegionMetricsCorpus(t, database, "INFRA", now)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/legion/metrics", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("a bare call: status=%d body=%s", response.Code, response.Body.String())
	}
	report := decodeBody[metrics.Report](t, response)
	if report.Project != "INFRA" || report.Scope != metrics.ScopeAll {
		t.Fatalf("the report names project %q at scope %q, want INFRA and all", report.Project, report.Scope)
	}
	if window := report.To.Sub(report.From); window < 7*24*time.Hour-time.Minute || window > 7*24*time.Hour+time.Minute {
		t.Fatalf("the window is %v wide, want the last 7 days", window)
	}
	if report.Finished != 1 || len(report.WorkTime.Issues) != 1 || report.WorkTime.Issues[0].Key != "INFRA-1002" {
		t.Fatalf("the report counted %d issues (%+v), want the child that closed yesterday", report.Finished, report.WorkTime.Issues)
	}
	// Twenty-four hours from the first worker event to the close, less the two parked ones.
	if row := report.WorkTime.Issues[0]; row.Hours != 22 || row.ParkedHours != 2 {
		t.Errorf("the work time = %g hours with %g parked, want 22 and 2", row.Hours, row.ParkedHours)
	}
	if len(report.PhaseTime.Bars) != 7 {
		t.Errorf("the phase time has %d bars, want the six roles and the one for no role", len(report.PhaseTime.Bars))
	}
}

// TestLegionMetricsAnswersWhatTheComputeFunctionAnswers: the route is the read and the compute the
// live hub calls, not a second implementation of the rules, so its body is what Compute answers
// over the population Read gathered for the same window.
func TestLegionMetricsAnswersWhatTheComputeFunctionAnswers(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	now := time.Now().UTC()
	seedLegionMetricsCorpus(t, database, "INFRA", now)
	from, to := now.Add(-7*24*time.Hour), now

	query := fmt.Sprintf("/api/v1/legion/metrics?project=INFRA&from=%s&to=%s&scope=children",
		from.Format(time.RFC3339), to.Format(time.RFC3339))
	response := dispatchRequest(t, handler, http.MethodGet, query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the route: status=%d body=%s", response.Code, response.Body.String())
	}
	fromRoute := decodeBody[metrics.Report](t, response)

	population, err := metrics.Read(t.Context(), database.Pool, nil, "INFRA", from, to)
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	direct := metrics.Compute(population, metrics.ScopeChildren, now)
	direct.ComputedAt, fromRoute.ComputedAt = time.Time{}, time.Time{}
	direct.From, fromRoute.From = time.Time{}, time.Time{}
	direct.To, fromRoute.To = time.Time{}, time.Time{}
	wanted, served := mustEncode(t, direct), mustEncode(t, fromRoute)
	if wanted != served {
		t.Fatalf("the route answered\n\t%s\nand the compute function answered\n\t%s", served, wanted)
	}
	if fromRoute.Scope != metrics.ScopeChildren || fromRoute.Finished != 1 {
		t.Fatalf("at scope children the report is %+v, want the one child that finished", fromRoute)
	}
}

// TestLegionMetricsRefusesAMalformedWindowScopeOrProject: the three refusals the row names.
func TestLegionMetricsRefusesAMalformedWindowScopeOrProject(t *testing.T) {
	handler := newTestHandler(t)
	seedLegionProjects(t, handler, "INFRA")
	for _, one := range []struct {
		name, query string
		status      int
	}{
		{"a malformed from", "?project=INFRA&from=yesterday", http.StatusBadRequest},
		{"an inverted window", "?project=INFRA&from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z", http.StatusBadRequest},
		{"a window over 84 days", "?project=INFRA&from=2026-01-01T00:00:00Z&to=2026-11-01T00:00:00Z", http.StatusBadRequest},
		{"an unknown scope", "?project=INFRA&scope=everything", http.StatusBadRequest},
		{"a project that is no key", "?project=not%20a%20key", http.StatusBadRequest},
		{"a project that does not exist", "?project=NOPE", http.StatusNotFound},
	} {
		t.Run(one.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/legion/metrics"+one.query, nil, "alice")
			if response.Code != one.status {
				t.Fatalf("%s: status=%d body=%s, want %d", one.name, response.Code, response.Body.String(), one.status)
			}
		})
	}
}

// TestLegionMetricsAnswersADeploymentWithNoLegionProject: a deployment where no project names a
// daemon has nothing to measure, and answers an empty report rather than a refusal, so a bare
// call answers 200 wherever it is made.
func TestLegionMetricsAnswersADeploymentWithNoLegionProject(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/legion/metrics", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	report := decodeBody[metrics.Report](t, response)
	if report.Project != "" || report.Finished != 0 || len(report.PhaseTime.Bars) != 7 {
		t.Fatalf("the report is %+v, want an empty one with its seven bars", report)
	}
}

// mustEncode is a report as JSON, which is how two of them are compared.
func mustEncode(t *testing.T, report metrics.Report) string {
	t.Helper()
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode a report: %v", err)
	}
	return string(encoded)
}
