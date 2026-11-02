package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Where a report of a problem with Dispatch itself goes (POST /api/v1/reports, reports.go): the
// project its issue is filed in, and the route of the agent that fixes one when the reporter asks.
// Both are the deployment's, one row of report_settings, beside delivery_settings. Like
// putDeliverySettings, the write appends no event: the row has no project, issue or artifact to
// own one (events/broker.go, eventOwnerKey).
//
// The agent is named by a role and never a session: a role outlives the session that holds it, so
// the agent's restart, which gives it a new session id, leaves the setting pointing at it once it
// claims the role again.

// maxReportAgentRoute bounds the agent setting, `role:` and the role's name.
const maxReportAgentRoute = 128

type reportSettings struct {
	Project    *string      `json:"project"`
	AgentRoute *string      `json:"agent_route"`
	UpdatedBy  *model.Actor `json:"updated_by"`
	UpdatedAt  *time.Time   `json:"updated_at"`
}

// loadReportSettings reads the one report_settings row; with none written yet every field is
// null, which reads as reports not set up.
func loadReportSettings(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
},
) (reportSettings, error) {
	var settings reportSettings
	var updatedBy []byte
	var updatedAt time.Time
	err := q.QueryRow(ctx, `
		select project_key, agent_route, updated_by, updated_at from report_settings where singleton
	`).Scan(&settings.Project, &settings.AgentRoute, &updatedBy, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return reportSettings{}, nil
	}
	if err != nil {
		return reportSettings{}, err
	}
	var actor model.Actor
	if err := json.Unmarshal(updatedBy, &actor); err != nil {
		return reportSettings{}, fmt.Errorf("decode report settings author: %w", err)
	}
	settings.UpdatedBy = &actor
	settings.UpdatedAt = &updatedAt
	return settings, nil
}

// getReportSettings answers GET /api/v1/settings/reports: the report form reads it to know whether
// reports are set up and whether to offer the agent.
func (s *server) getReportSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	settings, err := loadReportSettings(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, settings)
}

// putReportSettings answers PUT /api/v1/settings/reports {project, agent_route}. Human-only, as a
// project's default assignee is: which agent a person's report wakes, and where it lands, is a
// person's call. Each field is required and only an explicit null (or a blank string) clears it,
// so a PUT of `{}` cannot wipe where reports go.
func (s *server) putReportSettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		Project    json.RawMessage `json:"project"`
		AgentRoute json.RawMessage `json:"agent_route"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	project, err := optionalSettingText("project", input.Project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	route, err := optionalSettingText("agent_route", input.AgentRoute)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if route != nil {
		if err := checkReportAgentRoute(*route); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if project != nil {
		if !projectKeyPattern.MatchString(*project) {
			writeError(w, "REPORT_SETTINGS_INPUT", http.StatusBadRequest, projectKeyProblem("project", *project))
			return
		}
		err := s.deps.Store.Pool.QueryRow(r.Context(), `select key from projects where key = $1`, *project).Scan(new(string))
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "REPORT_SETTINGS_INPUT", http.StatusBadRequest, fmt.Sprintf("project %s does not exist", *project))
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := s.deps.Store.Pool.Exec(r.Context(), `
		insert into report_settings (singleton, project_key, agent_route, updated_by)
		values (true, $1, $2, $3)
		on conflict (singleton) do update set
			project_key = excluded.project_key,
			agent_route = excluded.agent_route,
			updated_by = excluded.updated_by,
			updated_at = now()
	`, project, route, actorJSON); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	settings, err := loadReportSettings(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, settings)
}

// optionalSettingText reads one required field of a settings write: absent is refused, null or a
// blank string clears it, and any other string is its trimmed value.
func optionalSettingText(name string, raw json.RawMessage) (*string, error) {
	if len(raw) == 0 {
		return nil, errorf(http.StatusBadRequest, "REPORT_SETTINGS_INPUT", "%s is required: a value, or null to clear it", name)
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, errorf(http.StatusBadRequest, "REPORT_SETTINGS_INPUT", "%s must be a string or null", name)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	return &value, nil
}

// checkReportAgentRoute holds the agent setting to a role route (the head of this file says why).
func checkReportAgentRoute(route string) error {
	// A role names a key of Envoy's role bucket, so it is held to a length a key may take.
	if len(route) > maxReportAgentRoute {
		return errorf(http.StatusBadRequest, "REPORT_SETTINGS_INPUT",
			"agent_route is %d bytes, past the %d a role route may hold", len(route), maxReportAgentRoute)
	}
	parsed, err := model.ParseRoute(route)
	if err == nil && parsed.Kind == "role" {
		return nil
	}
	if err == nil && parsed.Kind == "session" {
		return errorf(http.StatusBadRequest, "REPORT_SETTINGS_INPUT",
			"agent_route %q names a session, whose id changes when the agent restarts: name the role the agent holds, as role:<name>", route)
	}
	return errorf(http.StatusBadRequest, "REPORT_SETTINGS_INPUT",
		"agent_route %q is not a role route: name the role the agent holds, as role:<name> (lowercase letters, digits and hyphens)", route)
}
