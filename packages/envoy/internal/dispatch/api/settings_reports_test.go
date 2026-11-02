package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type reportSettingsBody struct {
	Project    *string      `json:"project"`
	AgentRoute *string      `json:"agent_route"`
	UpdatedBy  *model.Actor `json:"updated_by"`
	UpdatedAt  *string      `json:"updated_at"`
}

func putReportSettings(t *testing.T, handler http.Handler, body map[string]any) reportSettingsBody {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/reports", body, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("put report settings %v: status=%d body=%s", body, response.Code, response.Body.String())
	}
	return decodeBody[reportSettingsBody](t, response)
}

func readReportSettings(t *testing.T, handler http.Handler) reportSettingsBody {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/reports", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read report settings: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[reportSettingsBody](t, response)
}

// Before anyone sets them, both settings read null: reports are not set up, and the report
// dialog says so.
func TestReportSettingsReadNullsBeforeAnyWrite(t *testing.T) {
	handler := newTestHandler(t)
	settings := readReportSettings(t, handler)
	if settings.Project != nil || settings.AgentRoute != nil || settings.UpdatedBy != nil {
		t.Fatalf("settings = %#v, want every field null", settings)
	}
}

// A person names the project reports land in and the role of the agent that fixes them; both
// read back, with who set them.
func TestReportSettingsPutStoresProjectAndRole(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")

	stored := putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": " role:dispatch-fixer "})
	if stored.Project == nil || *stored.Project != "CORE" || stored.AgentRoute == nil || *stored.AgentRoute != "role:dispatch-fixer" {
		t.Fatalf("stored = %#v, want CORE and role:dispatch-fixer", stored)
	}
	read := readReportSettings(t, handler)
	if read.Project == nil || *read.Project != "CORE" || read.AgentRoute == nil || *read.AgentRoute != "role:dispatch-fixer" {
		t.Fatalf("read = %#v, want CORE and role:dispatch-fixer", read)
	}
	if read.UpdatedBy == nil || read.UpdatedBy.Kind != "user" || read.UpdatedBy.ID != "alice" || read.UpdatedAt == nil {
		t.Fatalf("read = %#v, want updated by alice with a time", read)
	}
	agentRead := agentRequest(t, handler, http.MethodGet, "/api/v1/settings/reports", nil, "agent-token")
	if agentRead.Code != http.StatusOK || !strings.Contains(agentRead.Body.String(), `"agent_route":"role:dispatch-fixer"`) {
		t.Fatalf("an agent reads the settings: status=%d body=%s", agentRead.Code, agentRead.Body.String())
	}
}

// A project that does not exist is refused, and nothing is stored.
func TestReportSettingsPutRefusesAnUnknownProject(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/reports",
		map[string]any{"project": "NOPE", "agent_route": nil}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"REPORT_SETTINGS_INPUT"`) ||
		!strings.Contains(response.Body.String(), "NOPE") {
		t.Fatalf("unknown project: status=%d body=%s", response.Code, response.Body.String())
	}
	if settings := readReportSettings(t, handler); settings.Project != nil {
		t.Fatalf("a refused put stored %#v", settings)
	}
}

// The agent is named by a role: a session id changes when the agent restarts, so a session
// route is refused with that reason, and so is anything that is not a route.
func TestReportSettingsPutRefusesASessionRoute(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	for _, route := range []string{"session:0123456789abcdef", "role:Bad Name", "dispatch-fixer"} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/reports",
			map[string]any{"project": "CORE", "agent_route": route}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"REPORT_SETTINGS_INPUT"`) ||
			!strings.Contains(response.Body.String(), "role:") {
			t.Fatalf("route %q: status=%d body=%s", route, response.Code, response.Body.String())
		}
	}
	session := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/reports",
		map[string]any{"project": "CORE", "agent_route": "session:0123456789abcdef"}, "alice")
	if !strings.Contains(session.Body.String(), "restart") {
		t.Fatalf("a session route's refusal does not say why: %s", session.Body.String())
	}
	if settings := readReportSettings(t, handler); settings.Project != nil || settings.AgentRoute != nil {
		t.Fatalf("a refused put stored %#v", settings)
	}
}

// An explicit null clears a setting; a body that leaves a field out is refused, so a PUT of `{}`
// cannot wipe where reports go.
func TestReportSettingsPutClearsWithNull(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": "role:dispatch-fixer"})

	for _, body := range []map[string]any{{}, {"project": "CORE"}, {"agent_route": nil}} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/reports", body, "alice")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %v: status=%d body=%s, want 400", body, response.Code, response.Body.String())
		}
	}
	cleared := putReportSettings(t, handler, map[string]any{"project": "CORE", "agent_route": nil})
	if cleared.Project == nil || cleared.AgentRoute != nil {
		t.Fatalf("cleared = %#v, want CORE and no agent", cleared)
	}
	blank := putReportSettings(t, handler, map[string]any{"project": nil, "agent_route": "  "})
	if blank.Project != nil || blank.AgentRoute != nil {
		t.Fatalf("blank = %#v, want both null", blank)
	}
}

// Only a person changes where reports go: an agent session may not redirect them.
func TestReportSettingsPutIsHumanOnly(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	response := agentRequest(t, handler, http.MethodPut, "/api/v1/settings/reports",
		map[string]any{"project": "CORE", "agent_route": nil, "actor": sessionActor()}, "agent-token")
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("agent sets report settings: status=%d body=%s", response.Code, response.Body.String())
	}
}
