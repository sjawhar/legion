package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// recordSessionTitles gives each session its title the way Dispatch learns one: an event the
// session wrote, carrying its origin's session_title (delivery/agents.go ResolveSessionTitles reads
// the newest). at stamps the events, so a later call can give a session a newer title.
func recordSessionTitles(t *testing.T, database *store.Store, at time.Time, titles map[string]string) {
	t.Helper()
	ctx := t.Context()
	pool := database.Pool
	for _, statement := range []string{
		`insert into projects (key, name) values ('ACME', 'Acme') on conflict do nothing`,
		`insert into issues (key, project_key, number, title, status, created_by, rank)
			values ('ACME-900', 'ACME', 900, 'Titles', 'todo', '{"kind":"system","id":"test"}', 'Z')
			on conflict do nothing`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed titles issue: %v", err)
		}
	}
	sessions := make([]string, 0, len(titles))
	for session := range titles {
		sessions = append(sessions, session)
	}
	slices.Sort(sessions)
	for _, session := range sessions {
		if _, err := pool.Exec(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			select 'ACME-900', coalesce(max(seq), 0) + 1, 'message.created',
				jsonb_build_object('kind', 'session', 'id', $1::text, 'origin', jsonb_build_object('session_title', $2::text)),
				'{}', false, $3
			from events where issue_key = 'ACME-900'
		`, session, titles[session], at); err != nil {
			t.Fatalf("seed title of %s: %v", session, err)
		}
	}
}

// seedSessionTitles records each session's title and a population pull request naming it, so the
// title rule, which reads the sessions population pull requests name, sees it.
func seedSessionTitles(t *testing.T, database *store.Store, at time.Time, titles map[string]string) {
	t.Helper()
	recordSessionTitles(t, database, at, titles)
	for session := range titles {
		var number int
		if err := database.Pool.QueryRow(t.Context(), `select coalesce(max(number), 0) + 1 from delivery_pull_requests where repo = 'acme/titles'`).Scan(&number); err != nil {
			t.Fatalf("next titles PR number: %v", err)
		}
		merged := at
		if err := delivery.UpsertPullRequest(t.Context(), database.Pool, delivery.DeliveryPullRequest{
			Repo: "acme/titles", Number: number, Title: "work by " + session, URL: "https://github.com/acme/titles/pull/" + strconv.Itoa(number),
			Author: "octocat", MergedAt: &merged, CreatedAt: &merged, Sessions: []string{session},
		}); err != nil {
			t.Fatalf("seed PR naming %s: %v", session, err)
		}
	}
}

type sessionGroupBody struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Sessions  []string `json:"sessions"`
	CreatedBy struct {
		ID string `json:"id"`
	} `json:"created_by"`
	UpdatedBy struct {
		ID string `json:"id"`
	} `json:"updated_by"`
}

type apiErrorBody struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func createSessionGroup(t *testing.T, handler http.Handler, name string, sessions ...string) sessionGroupBody {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": name, "sessions": sessions,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create group %q %v: status = %d, body = %s", name, sessions, response.Code, response.Body.String())
	}
	var group sessionGroupBody
	if err := json.Unmarshal(response.Body.Bytes(), &group); err != nil {
		t.Fatalf("decode group: %v", err)
	}
	return group
}

// expectRefusal asserts response is status with code, and that its message names every one of
// names.
func expectRefusal(t *testing.T, label string, response interface {
	Result() *http.Response
}, status int, code string, names ...string) {
	t.Helper()
	result := response.Result()
	defer result.Body.Close()
	var body apiErrorBody
	if err := json.NewDecoder(result.Body).Decode(&body); err != nil {
		t.Fatalf("%s: decode refusal (status %d): %v", label, result.StatusCode, err)
	}
	if result.StatusCode != status || body.Code != code {
		t.Fatalf("%s: status = %d code = %q (%s), want %d %s", label, result.StatusCode, body.Code, body.Error, status, code)
	}
	for _, name := range names {
		if !strings.Contains(body.Error, name) {
			t.Errorf("%s: message %q does not name %q", label, body.Error, name)
		}
	}
}

func TestCreateSessionGroupRecordsItsActor(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)

	person := createSessionGroup(t, handler, "Release crew", "s1", "s2")
	if person.ID == "" || person.Name != "Release crew" || !slices.Equal(person.Sessions, []string{"s1", "s2"}) {
		t.Fatalf("group = %+v, want an id, its name and both sessions", person)
	}
	if person.CreatedBy.ID != "alice" || person.UpdatedBy.ID != "alice" {
		t.Errorf("person's group created_by = %q updated_by = %q, want alice", person.CreatedBy.ID, person.UpdatedBy.ID)
	}

	response := agentRequest(t, handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "Agents' crew", "sessions": []string{"s3"},
		"actor": map[string]string{"kind": "session", "id": "agent-groups"},
	}, "agent-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("agent create: status = %d, body = %s", response.Code, response.Body.String())
	}
	var agent sessionGroupBody
	if err := json.Unmarshal(response.Body.Bytes(), &agent); err != nil {
		t.Fatalf("decode agent group: %v", err)
	}
	if agent.CreatedBy.ID != "agent-groups" {
		t.Errorf("agent's group created_by = %q, want agent-groups", agent.CreatedBy.ID)
	}

	anonymous := dispatchRequest(t, handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "Nobody's", "sessions": []string{"s4"},
	}, "")
	if anonymous.Code != http.StatusUnauthorized {
		t.Errorf("anonymous create: status = %d, body = %s, want 401", anonymous.Code, anonymous.Body.String())
	}
}

func TestCreateSessionGroupRefusesBadInput(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	for label, body := range map[string]map[string]any{
		"a blank name":       {"name": "  ", "sessions": []string{"s1"}},
		"no sessions":        {"name": "Empty", "sessions": []string{}},
		"a blank session id": {"name": "Blank", "sessions": []string{" "}},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/delivery/session-groups", body, "alice")
		expectRefusal(t, label, response, http.StatusBadRequest, "SESSION_GROUP_INPUT")
	}
	unknown := dispatchRequest(t, handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "Extra", "sessions": []string{"s1"}, "colour": "red",
	}, "alice")
	expectRefusal(t, "an unknown field", unknown, http.StatusBadRequest, "INVALID_JSON")
}

func TestASessionIsInOneGroup(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	first := createSessionGroup(t, handler, "First", "s1")

	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "Second", "sessions": []string{"s1"},
	}, "alice")
	expectRefusal(t, "a create naming s1", second, http.StatusConflict, "SESSION_IN_GROUP", "s1", first.Name)

	other := createSessionGroup(t, handler, "Other", "s2")
	added := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/delivery/session-groups/"+other.ID, map[string]any{
		"add": []string{"s1"},
	}, "alice")
	expectRefusal(t, "adding s1 to another group", added, http.StatusConflict, "SESSION_IN_GROUP", "s1", first.Name)
}

func TestGroupNamesAreUniqueIgnoringCase(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	createSessionGroup(t, handler, "Alpha", "s1")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "alpha", "sessions": []string{"s2"},
	}, "alice")
	expectRefusal(t, "a second Alpha", response, http.StatusConflict, "SESSION_GROUP_NAME_TAKEN", "Alpha")
}

// TestAGroupMayTakeATitleItHoldsWhole: a group may carry a session title as its name when it holds
// every session with that title, the usual case for an agent that restarted under the same title.
func TestAGroupMayTakeATitleItHoldsWhole(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedSessionTitles(t, database, time.Now().Add(-time.Hour), map[string]string{"s1": "Planner", "s2": "Planner"})
	group := createSessionGroup(t, handler, "planner", "s1", "s2")
	if group.Name != "planner" {
		t.Fatalf("group name = %q, want planner", group.Name)
	}
}

// TestAGroupNameRefusesATitleOutsideIt: a name another session holds as its title, outside the
// group, is refused naming that session, on a create, a rename and a removal alike.
func TestAGroupNameRefusesATitleOutsideIt(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedSessionTitles(t, database, time.Now().Add(-time.Hour), map[string]string{
		"s1": "Planner", "s2": "Planner", "s3": "Planner", "s4": "Reviewer",
	})

	partial := dispatchRequest(t, handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "Planner", "sessions": []string{"s1", "s2"},
	}, "alice")
	expectRefusal(t, "a Planner group leaving s3 out", partial, http.StatusConflict, "SESSION_GROUP_NAME_IS_SESSION_TITLE", "s3")

	planner := createSessionGroup(t, handler, "Planner", "s1", "s2", "s3")
	removed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/delivery/session-groups/"+planner.ID, map[string]any{
		"remove": []string{"s3"},
	}, "alice")
	expectRefusal(t, "removing s3 from Planner", removed, http.StatusConflict, "SESSION_GROUP_NAME_IS_SESSION_TITLE", "s3")

	other := createSessionGroup(t, handler, "Night shift", "s9")
	taken := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/delivery/session-groups/"+other.ID, map[string]any{
		"name": "PLANNER",
	}, "alice")
	expectRefusal(t, "renaming to another group's name", taken, http.StatusConflict, "SESSION_GROUP_NAME_TAKEN", "Planner")

	titled := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/delivery/session-groups/"+other.ID, map[string]any{
		"name": "REVIEWER",
	}, "alice")
	expectRefusal(t, "renaming to an ungrouped session's title", titled, http.StatusConflict, "SESSION_GROUP_NAME_IS_SESSION_TITLE", "s4")
}

func TestPatchRenamesAddsRemoves(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	group := createSessionGroup(t, handler, "Crew", "s1", "s2")
	path := "/api/v1/delivery/session-groups/" + group.ID

	response := dispatchRequest(t, handler, http.MethodPatch, path, map[string]any{
		"name": "Release crew", "add": []string{"s4"}, "remove": []string{"s1"},
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("patch: status = %d, body = %s", response.Code, response.Body.String())
	}
	var edited sessionGroupBody
	if err := json.Unmarshal(response.Body.Bytes(), &edited); err != nil {
		t.Fatalf("decode patched group: %v", err)
	}
	if edited.ID != group.ID || edited.Name != "Release crew" || !slices.Equal(edited.Sessions, []string{"s2", "s4"}) {
		t.Fatalf("patched group = %+v, want Release crew holding s2 and s4", edited)
	}

	emptied := dispatchRequest(t, handler, http.MethodPatch, path, map[string]any{
		"remove": []string{"s2", "s4"},
	}, "alice")
	expectRefusal(t, "removing every session", emptied, http.StatusBadRequest, "SESSION_GROUP_INPUT")
}

func TestDeleteSessionGroup(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	group := createSessionGroup(t, handler, "Crew", "s1", "s2")
	path := "/api/v1/delivery/session-groups/" + group.ID

	if response := dispatchRequest(t, handler, http.MethodDelete, path, nil, "alice"); response.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, body = %s, want 204", response.Code, response.Body.String())
	}
	var members int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from delivery_session_group_members`).Scan(&members); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if members != 0 {
		t.Errorf("members after delete = %d, want 0", members)
	}
	expectRefusal(t, "a second delete", dispatchRequest(t, handler, http.MethodDelete, path, nil, "alice"), http.StatusNotFound, "NOT_FOUND")
	expectRefusal(t, "a patch of a deleted group", dispatchRequest(t, handler, http.MethodPatch, path, map[string]any{"name": "Gone"}, "alice"), http.StatusNotFound, "NOT_FOUND")
	expectRefusal(t, "a non-uuid id", dispatchRequest(t, handler, http.MethodDelete, "/api/v1/delivery/session-groups/nope", nil, "alice"), http.StatusBadRequest, "SESSION_GROUP_ID_INPUT")

	// The sessions are free again.
	createSessionGroup(t, handler, "Crew again", "s1", "s2")
}
