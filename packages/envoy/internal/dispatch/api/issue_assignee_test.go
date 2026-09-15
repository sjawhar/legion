package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type assigneeIssue struct {
	Key      string  `json:"key"`
	Parent   *string `json:"parent"`
	Assignee *string `json:"assignee"`
}

func createAssigneeProject(t *testing.T, handler http.Handler) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
}

func expectAssignee(t *testing.T, got *string, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Fatalf("assignee = %q, want null", *got)
		}
		return
	}
	if got == nil || *got != want {
		t.Fatalf("assignee = %v, want %q", got, want)
	}
}

// A human who creates an issue is its assignee, stored as the lowercase login even when the
// identity layer echoes GitHub's display casing.
func TestCreateIssueByHumanAssignsCreatorLowercased(t *testing.T) {
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Mine",
	}, "Alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, response).Assignee, "alice")
}

// A personal token acts for its owner: an issue it creates is assigned to that human.
func TestCreateIssueByPersonalTokenAssignsOwner(t *testing.T) {
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "planner"}, "Bob")
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	token := decodeBody[agentTokenResponse](t, minted).Token
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "For Bob", "actor": sessionActor(),
	}, token)
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue with personal token: status=%d body=%s", response.Code, response.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, response).Assignee, "bob")
}

// The shared token has no owner: a child inherits its parent's assignee (null included) and a
// root issue is unassigned; an explicit assignee always wins.
func TestCreateIssueBySharedTokenInheritsParentOrStaysUnassigned(t *testing.T) {
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	root := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Root", "actor": sessionActor(),
	}, "agent-token")
	if root.Code != http.StatusCreated {
		t.Fatalf("create root: status=%d body=%s", root.Code, root.Body.String())
	}
	rootIssue := decodeBody[assigneeIssue](t, root)
	expectAssignee(t, rootIssue.Assignee, "")

	orphanChild := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Child of unassigned", "parent": rootIssue.Key, "actor": sessionActor(),
	}, "agent-token")
	if orphanChild.Code != http.StatusCreated {
		t.Fatalf("create child: status=%d body=%s", orphanChild.Code, orphanChild.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, orphanChild).Assignee, "")

	explicit := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Explicit", "assignee": " Bob ", "actor": sessionActor(),
	}, "agent-token")
	if explicit.Code != http.StatusCreated {
		t.Fatalf("create explicit: status=%d body=%s", explicit.Code, explicit.Body.String())
	}
	explicitIssue := decodeBody[assigneeIssue](t, explicit)
	expectAssignee(t, explicitIssue.Assignee, "bob")

	child := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Child of Bob", "parent": explicitIssue.Key, "actor": sessionActor(),
	}, "agent-token")
	if child.Code != http.StatusCreated {
		t.Fatalf("create child of bob: status=%d body=%s", child.Code, child.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, child).Assignee, "bob")

	humanOverride := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Alice files for Bob", "assignee": "bob",
	}, "alice")
	if humanOverride.Code != http.StatusCreated {
		t.Fatalf("create with explicit assignee as human: status=%d body=%s", humanOverride.Code, humanOverride.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, humanOverride).Assignee, "bob")
}

func TestIssueAssigneeRejectsUnlistedAndMalformedLogins(t *testing.T) {
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Target",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[assigneeIssue](t, created).Key

	for _, test := range []struct {
		name   string
		method string
		target string
		body   map[string]any
		code   string
	}{
		{name: "create unlisted", method: http.MethodPost, target: "/api/v1/issues", body: map[string]any{"project": "CORE", "title": "X", "assignee": "mallory"}, code: "ASSIGNEE_NOT_ALLOWED"},
		{name: "patch unlisted", method: http.MethodPatch, target: "/api/v1/issues/" + key, body: map[string]any{"assignee": "mallory"}, code: "ASSIGNEE_NOT_ALLOWED"},
		{name: "patch blank", method: http.MethodPatch, target: "/api/v1/issues/" + key, body: map[string]any{"assignee": "  "}, code: "ASSIGNEE_NOT_ALLOWED"},
		{name: "patch number", method: http.MethodPatch, target: "/api/v1/issues/" + key, body: map[string]any{"assignee": 7}, code: "INVALID_ISSUE"},
		{name: "create object", method: http.MethodPost, target: "/api/v1/issues", body: map[string]any{"project": "CORE", "title": "X", "assignee": map[string]any{"login": "bob"}}, code: "INVALID_ISSUE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, test.method, test.target, test.body, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("status=%d body=%s, want 400 %s", response.Code, response.Body.String(), test.code)
			}
			if test.code == "ASSIGNEE_NOT_ALLOWED" && test.name != "patch blank" && !strings.Contains(response.Body.String(), "mallory") {
				t.Fatalf("refusal does not name the login: %s", response.Body.String())
			}
		})
	}
	unchanged := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	expectAssignee(t, decodeBody[assigneeIssue](t, unchanged).Assignee, "alice")
}

// Anyone on the allowlist may reassign; the update event carries the new assignee and PATCH
// null clears it. A PATCH that does not mention assignee (the daemon's status writes) leaves it.
func TestPatchIssueReassignsClearsAndLeavesAssigneeAlone(t *testing.T) {
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Handoff",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[assigneeIssue](t, created).Key

	reassigned := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"assignee": "Bob",
	}, "bob")
	if reassigned.Code != http.StatusOK {
		t.Fatalf("reassign: status=%d body=%s", reassigned.Code, reassigned.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, reassigned).Assignee, "bob")

	statusOnly := agentRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"status": "in_progress", "actor": sessionActor(),
	}, "agent-token")
	if statusOnly.Code != http.StatusOK {
		t.Fatalf("status patch: status=%d body=%s", statusOnly.Code, statusOnly.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, statusOnly).Assignee, "bob")

	eventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
	if eventsResponse.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", eventsResponse.Code, eventsResponse.Body.String())
	}
	var events []struct {
		Type    string `json:"type"`
		Actor   model.Actor
		Payload struct {
			Assignee *string `json:"assignee"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	sawReassign := false
	for _, event := range events {
		if event.Type == "issue.updated" && event.Actor.Kind == "user" && event.Actor.ID == "bob" {
			sawReassign = true
			expectAssignee(t, event.Payload.Assignee, "bob")
		}
		if event.Type == "issue.created" {
			expectAssignee(t, event.Payload.Assignee, "alice")
		}
	}
	if !sawReassign {
		t.Fatalf("reassignment did not emit issue.updated by bob: %#v", events)
	}

	cleared := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"assignee": nil,
	}, "alice")
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, cleared).Assignee, "")
}

func TestListIssuesSummariesCarryAssignee(t *testing.T) {
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Listed",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Nobody's", "actor": sessionActor(),
	}, "agent-token"); response.Code != http.StatusCreated {
		t.Fatalf("create unassigned issue: status=%d body=%s", response.Code, response.Body.String())
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	summaries := decodeBody[[]struct {
		Title    string  `json:"title"`
		Assignee *string `json:"assignee"`
	}](t, listed)
	byTitle := map[string]*string{}
	for _, summary := range summaries {
		byTitle[summary.Title] = summary.Assignee
	}
	expectAssignee(t, byTitle["Listed"], "alice")
	if _, present := byTitle["Nobody's"]; !present {
		t.Fatalf("unassigned issue missing from listing: %#v", summaries)
	}
	expectAssignee(t, byTitle["Nobody's"], "")
}

// GET /users is the picker's option list: the allowlist, sorted, humans only.
func TestListUsersReturnsSortedAllowlistForHumansOnly(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/users", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list users: status=%d body=%s", response.Code, response.Body.String())
	}
	users := decodeBody[struct {
		Users []struct {
			Login string `json:"login"`
		} `json:"users"`
	}](t, response)
	if len(users.Users) != 2 || users.Users[0].Login != "alice" || users.Users[1].Login != "bob" {
		t.Fatalf("users = %#v, want alice then bob", users)
	}
	denied := agentRequest(t, handler, http.MethodGet, "/api/v1/users", nil, "agent-token")
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("bearer lists users: status=%d body=%s", denied.Code, denied.Body.String())
	}
}

// GET /whoami tells a caller who the server takes it for: a human by (display-cased) login, a
// personal token by its owner's lowercase login, the shared token by a null owner.
func TestWhoamiNamesHumanPersonalTokenOwnerAndSharedToken(t *testing.T) {
	handler := newTestHandler(t)
	type whoami struct {
		Kind  string  `json:"kind"`
		Login string  `json:"login"`
		Owner *string `json:"owner"`
	}
	human := dispatchRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "Alice")
	if human.Code != http.StatusOK {
		t.Fatalf("human whoami: status=%d body=%s", human.Code, human.Body.String())
	}
	if got := decodeBody[whoami](t, human); got.Kind != "user" || got.Login != "Alice" {
		t.Fatalf("human whoami = %#v", got)
	}
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "planner"}, "Bob")
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, decodeBody[agentTokenResponse](t, minted).Token)
	if personal.Code != http.StatusOK {
		t.Fatalf("personal whoami: status=%d body=%s", personal.Code, personal.Body.String())
	}
	if got := decodeBody[whoami](t, personal); got.Kind != "agent" || got.Owner == nil || *got.Owner != "bob" {
		t.Fatalf("personal-token whoami = %#v, want agent owned by bob", got)
	}
	shared := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "agent-token")
	if shared.Code != http.StatusOK {
		t.Fatalf("shared whoami: status=%d body=%s", shared.Code, shared.Body.String())
	}
	if got := decodeBody[whoami](t, shared); got.Kind != "agent" || got.Owner != nil {
		t.Fatalf("shared-token whoami = %#v, want agent with null owner", got)
	}
	if anonymous := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "wrong"); anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("unknown bearer whoami: status=%d body=%s", anonymous.Code, anonymous.Body.String())
	}
}
