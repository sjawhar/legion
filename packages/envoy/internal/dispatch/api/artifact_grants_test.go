package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The Go half of dispatch://INFRA-682's A10: the data grants of a page file, which decide what
// Dispatch gives a view block's page. Only a person writes one, every token is refused and
// changes nothing, a reference a grant cannot cover is refused by its cause, and each change
// announces itself on the page file's own event log.

// grantsFixture is the issue, the page file and the data file a grants test works on, with the
// handler that serves them.
type grantsFixture struct {
	handler http.Handler
	project string
	issue   string
	page    uploaded
	pageRef string
	data    uploaded
	dataRef string
}

// seedGrantsFixture builds a project, an issue in it, a page file on that issue and one data
// file beside it. project is the project's key, so a test that needs a page file in the project
// a broker token is scoped to can name it.
func seedGrantsFixture(t *testing.T, handler http.Handler, project string) grantsFixture {
	t.Helper()
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": project, "name": "Grants",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": "A page and its data", "spec": "# Grants",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	route := "/api/v1/issues/" + issue + "/artifacts"
	return grantsFixture{
		handler: handler,
		project: project,
		issue:   issue,
		page:    uploadTestFile(t, handler, route, "chart.html", "text/html", []byte("<!doctype html><title>Chart</title>")),
		pageRef: "dispatch://" + issue + "/artifact/chart-html",
		data:    uploadTestFile(t, handler, route, "attempts.json", "application/json", []byte(`{"attempts":3}`)),
		dataRef: "dispatch://" + issue + "/artifact/attempts-json",
	}
}

// grantsRoute is the grants list of one page file.
func grantsRoute(pageID string) string { return "/api/v1/artifacts/" + pageID + "/grants" }

// listedGrant is one row of the grants list as the panel reads it.
type listedGrant struct {
	ID         string `json:"id"`
	ArtifactID string `json:"artifact_id"`
	Ref        string `json:"ref"`
	Name       string `json:"name"`
	Version    *int   `json:"version"`
	GrantedBy  string `json:"granted_by"`
}

// listGrants reads a page file's grants as a signed-in person.
func listGrants(t *testing.T, handler http.Handler, pageID string) []listedGrant {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, grantsRoute(pageID), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list grants: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Grants []listedGrant `json:"grants"`
	}](t, response).Grants
}

// grantFile adds one grant as a person and answers the grant that was made.
func grantFile(t *testing.T, handler http.Handler, pageID, ref string) listedGrant {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, grantsRoute(pageID), map[string]string{"ref": ref}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("grant %s: status=%d body=%s", ref, response.Code, response.Body.String())
	}
	return decodeBody[listedGrant](t, response)
}

// grantsChangedEvent is what a reader of an event log sees of a grant change.
type grantsChangedEvent struct {
	IssueKey *string     `json:"issue_key"`
	Project  string      `json:"project"`
	Type     string      `json:"type"`
	Actor    model.Actor `json:"actor"`
	Payload  struct {
		ArtifactID string `json:"artifact_id"`
	} `json:"payload"`
}

// grantsChangedEvents reads one owner's event log and keeps its artifact.grants_changed rows,
// oldest first. log is the events route of the owner the page file's events hang from: its
// issue for an issue page file, the file itself for a project one.
func grantsChangedEvents(t *testing.T, handler http.Handler, log string) []grantsChangedEvent {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, log, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", log, response.Code, response.Body.String())
	}
	events := decodeBody[[]grantsChangedEvent](t, response)
	changes := []grantsChangedEvent{}
	for _, event := range events {
		if event.Type == "artifact.grants_changed" {
			changes = append(changes, event)
		}
	}
	return changes
}

// TestArtifactGrantsRefuseEveryTokenAndChangeNothing: the four credentials an agent can hold -
// the shared token, a personal token, a Kubernetes service-account token and an access token
// the secrets broker scoped to the page file's own project - are each answered 403 HUMAN_ONLY
// by both writes, and the list and the page file's event log are what they were.
func TestArtifactGrantsRefuseEveryTokenAndChangeNothing(t *testing.T) {
	issuer := newServiceTokenIssuer(t)
	stub := brokertokentest.NewStubBroker(t)
	handler, _, _ := newTestServer(t, testServerOptions{
		oidc: issuer.verifier(t),
		brokerTokens: func(database *store.Store) *brokertoken.Verifier {
			verifier, err := brokertoken.New(brokertoken.Options{
				BrokerURL: stub.Server.URL,
				Account:   brokertoken.NewAccountToken(accountTokenFile(t, credentialTestAccountToken)),
				Origin:    testServerURL,
				Replay:    store.NewDPoPProofs(database.Pool),
				Address:   auth.ForwardedAddress,
			})
			if err != nil {
				t.Fatalf("build the broker token verifier: %v", err)
			}
			return verifier
		},
	})
	// The page file sits in the project the broker token is scoped to, so the project check
	// admits that caller and it is requireHuman, not the scope, that refuses it.
	fixture := seedGrantsFixture(t, handler, brokerTestProject)
	granted := grantFile(t, handler, fixture.page.Artifact.ID, fixture.dataRef)

	personal := decodeBody[agentTokenResponse](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens",
		map[string]string{"name": "a page's agent"}, "alice"))
	if personal.Token == "" {
		t.Fatal("minting a personal token answered no token")
	}
	caller := newBrokerCaller(t)
	const brokerToken = "a-token-the-broker-issued-for-the-pages-project"
	stub.Says(brokerToken, liveNamedToken(caller.jkt))

	add := grantsRoute(fixture.page.Artifact.ID)
	remove := add + "/" + granted.ID
	for _, credential := range []struct {
		name string
		send func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder
	}{
		{"the shared token", func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
			return agentRequest(t, handler, method, target, body, "agent-token")
		}},
		{"a personal token", func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
			return agentRequest(t, handler, method, target, body, personal.Token)
		}},
		{"a service account token", func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
			return agentRequest(t, handler, method, target, body, issuer.mint(t, serviceTokenAudience))
		}},
		{"a broker token scoped to the page file's project", func(t *testing.T, method, target string, body any) *httptest.ResponseRecorder {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, caller.presenting(t, method, target, brokerToken, body))
			return response
		}},
	} {
		t.Run(credential.name, func(t *testing.T) {
			for _, write := range []struct {
				method string
				target string
				body   any
			}{
				{http.MethodPost, add, map[string]string{"ref": fixture.pageRef}},
				{http.MethodDelete, remove, nil},
			} {
				response := credential.send(t, write.method, write.target, write.body)
				if response.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, response).Code != "HUMAN_ONLY" {
					t.Errorf("%s %s as %s = %d %s, want 403 HUMAN_ONLY", write.method, write.target, credential.name, response.Code, response.Body.String())
				}
				listed := listGrants(t, handler, fixture.page.Artifact.ID)
				if len(listed) != 1 || listed[0].ID != granted.ID || listed[0].ArtifactID != fixture.data.Artifact.ID {
					t.Fatalf("after %s %s as %s the grants are %+v, want the one grant %s", write.method, write.target, credential.name, listed, granted.ID)
				}
			}
		})
	}
	if changes := grantsChangedEvents(t, handler, "/api/v1/issues/"+fixture.issue+"/events"); len(changes) != 1 {
		t.Fatalf("%d artifact.grants_changed events after the refusals, want only the person's own", len(changes))
	}
}

// TestArtifactGrantsRefuseAReferenceThatCannotBeCovered: each refusal a person can correct,
// under the code that names its cause, and nothing granted by any of them. The page file of
// {id} is refused the same way when it is no page file at all.
func TestArtifactGrantsRefuseAReferenceThatCannotBeCovered(t *testing.T) {
	handler := newTestHandler(t)
	fixture := seedGrantsFixture(t, handler, "GRANT")
	document := uploadTestFile(t, handler, "/api/v1/issues/"+fixture.issue+"/artifacts", "notes.md", "text/markdown", []byte("# Notes\n\nA paragraph.\n"))
	if document.Artifact.ID == "" {
		t.Fatal("the uploaded document has no id")
	}
	const session = "01a1058e-f14f-7684-87eb-3dc885955552"
	conversation := uploadTestFile(t, handler, "/api/v1/agents/"+session+"/artifacts", "session.html", "text/html", []byte("<!doctype html><title>Session</title>"))

	for _, refusal := range []struct {
		what   string
		ref    string
		status int
		code   string
	}{
		{"an issue, which is no file", "dispatch://" + fixture.issue, http.StatusBadRequest, "NOT_A_FILE_REFERENCE"},
		{"no reference at all", "", http.StatusBadRequest, "NOT_A_FILE_REFERENCE"},
		{"a file that is not there", "dispatch://" + fixture.issue + "/artifact/absent-json", http.StatusBadRequest, "DATA_FILE_NOT_FOUND"},
		{"a document", "dispatch://" + fixture.issue + "/artifact/notes-md", http.StatusBadRequest, "DATA_FILE_IS_A_DOCUMENT"},
		{"an agent conversation's file", "dispatch://agent/" + session + "/artifact/session-html", http.StatusBadRequest, "AGENT_CONVERSATION_FILE"},
		{"a version the file does not have", fixture.dataRef + "@v2", http.StatusBadRequest, "VERSION_NOT_FOUND"},
	} {
		t.Run(refusal.what, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, grantsRoute(fixture.page.Artifact.ID),
				map[string]string{"ref": refusal.ref}, "alice")
			if response.Code != refusal.status || decodeBody[refusalAnswer](t, response).Code != refusal.code {
				t.Errorf("granting %s = %d %s, want %d %s", refusal.what, response.Code, response.Body.String(), refusal.status, refusal.code)
			}
			if listed := listGrants(t, handler, fixture.page.Artifact.ID); len(listed) != 0 {
				t.Fatalf("granting %s left %+v, want no grant", refusal.what, listed)
			}
		})
	}

	// An agent conversation's file is no page file either, so it holds no grant list to add to.
	t.Run("an agent conversation's file as the page file", func(t *testing.T) {
		response := dispatchRequest(t, handler, http.MethodPost, grantsRoute(conversation.Artifact.ID),
			map[string]string{"ref": fixture.dataRef}, "alice")
		if response.Code != http.StatusUnsupportedMediaType || decodeBody[refusalAnswer](t, response).Code != "NOT_A_PAGE" {
			t.Fatalf("granting to an agent conversation's file = %d %s, want 415 NOT_A_PAGE", response.Code, response.Body.String())
		}
	})
}

// TestArtifactGrantsChangesAreAnnouncedOnTheIssuesLog: a person's add and a person's removal
// each write artifact.grants_changed for the page file on the issue that holds it, so every
// block showing that page re-reads the list.
func TestArtifactGrantsChangesAreAnnouncedOnTheIssuesLog(t *testing.T) {
	handler := newTestHandler(t)
	fixture := seedGrantsFixture(t, handler, "GRANT")
	log := "/api/v1/issues/" + fixture.issue + "/events"

	granted := grantFile(t, handler, fixture.page.Artifact.ID, fixture.dataRef)
	added := grantsChangedEvents(t, handler, log)
	if len(added) != 1 {
		t.Fatalf("%d artifact.grants_changed events after the add, want 1", len(added))
	}
	if added[0].Payload.ArtifactID != fixture.page.Artifact.ID {
		t.Errorf("the add's payload names %q, want the page file %s", added[0].Payload.ArtifactID, fixture.page.Artifact.ID)
	}
	if added[0].IssueKey == nil || *added[0].IssueKey != fixture.issue || added[0].Project != fixture.project {
		t.Errorf("the add's event = %+v, want it on issue %s of project %s", added[0], fixture.issue, fixture.project)
	}
	if added[0].Actor.Kind != "user" || added[0].Actor.ID != "alice" {
		t.Errorf("the add's actor = %+v, want the person who granted", added[0].Actor)
	}

	removed := dispatchRequest(t, handler, http.MethodDelete, grantsRoute(fixture.page.Artifact.ID)+"/"+granted.ID, nil, "alice")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("remove the grant: status=%d body=%s", removed.Code, removed.Body.String())
	}
	changes := grantsChangedEvents(t, handler, log)
	if len(changes) != 2 {
		t.Fatalf("%d artifact.grants_changed events after the removal, want 2", len(changes))
	}
	if changes[1].Payload.ArtifactID != fixture.page.Artifact.ID {
		t.Errorf("the removal's payload names %q, want the page file %s", changes[1].Payload.ArtifactID, fixture.page.Artifact.ID)
	}
	if listed := listGrants(t, handler, fixture.page.Artifact.ID); len(listed) != 0 {
		t.Fatalf("grants after the removal = %+v, want none", listed)
	}
}

// TestArtifactGrantsOnAProjectPageFileCarryTheProject: a project's page file belongs to no
// issue, so its change hangs from the file itself and the event names the project with no issue
// key - what a reader following a project needs to refresh the panel.
func TestArtifactGrantsOnAProjectPageFileCarryTheProject(t *testing.T) {
	handler := newTestHandler(t)
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "GRANT", "name": "Grants",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", created.Code, created.Body.String())
	}
	route := "/api/v1/projects/GRANT/artifacts"
	page := uploadTestFile(t, handler, route, "board.html", "text/html", []byte("<!doctype html><title>Board</title>"))
	data := uploadTestFile(t, handler, route, "board.json", "application/json", []byte(`{"rows":[]}`))

	granted := grantFile(t, handler, page.Artifact.ID, "dispatch://GRANT/artifact/board-json")
	if granted.ArtifactID != data.Artifact.ID || granted.Ref != "dispatch://GRANT/artifact/board-json" {
		t.Fatalf("the grant = %+v, want the project's board.json", granted)
	}
	log := "/api/v1/artifacts/" + page.Artifact.ID + "/events"
	added := grantsChangedEvents(t, handler, log)
	if len(added) != 1 {
		t.Fatalf("%d artifact.grants_changed events on the project page file, want 1", len(added))
	}
	if added[0].IssueKey != nil {
		t.Errorf("the event's issue_key = %q, want null for a project page file", *added[0].IssueKey)
	}
	if added[0].Project != "GRANT" || added[0].Payload.ArtifactID != page.Artifact.ID {
		t.Errorf("the event = %+v, want project GRANT and the page file %s", added[0], page.Artifact.ID)
	}

	if removed := dispatchRequest(t, handler, http.MethodDelete, grantsRoute(page.Artifact.ID)+"/"+granted.ID, nil, "alice"); removed.Code != http.StatusNoContent {
		t.Fatalf("remove the project grant: status=%d body=%s", removed.Code, removed.Body.String())
	}
	changes := grantsChangedEvents(t, handler, log)
	if len(changes) != 2 || changes[1].IssueKey != nil || changes[1].Project != "GRANT" {
		t.Fatalf("the removal's events = %+v, want a second one on the project with no issue key", changes)
	}
}

// TestArtifactGrantsMayCoverThePageFileItself: a page that draws data it also holds as a file is
// nothing to refuse, so the page file may be granted to itself and is listed like any other.
func TestArtifactGrantsMayCoverThePageFileItself(t *testing.T) {
	handler := newTestHandler(t)
	fixture := seedGrantsFixture(t, handler, "GRANT")

	granted := grantFile(t, handler, fixture.page.Artifact.ID, fixture.pageRef)
	if granted.ArtifactID != fixture.page.Artifact.ID || granted.Ref != fixture.pageRef || granted.Name != "chart.html" {
		t.Fatalf("the grant = %+v, want the page file itself", granted)
	}
	listed := listGrants(t, handler, fixture.page.Artifact.ID)
	if len(listed) != 1 || listed[0].ID != granted.ID || listed[0].ArtifactID != fixture.page.Artifact.ID || listed[0].Version != nil {
		t.Fatalf("the grants = %+v, want the page file covered at every version", listed)
	}
}

// TestArtifactGrantsListWhatEachOneCovers: a grant pinned to a version covers that version
// alone and a grant without one covers every version, and the list says which each is, with the
// file's address and name and who granted it.
func TestArtifactGrantsListWhatEachOneCovers(t *testing.T) {
	handler := newTestHandler(t)
	fixture := seedGrantsFixture(t, handler, "GRANT")
	route := "/api/v1/issues/" + fixture.issue + "/artifacts"
	pinnedFile := uploadTestFile(t, handler, route, "snapshot.json", "application/json", []byte(`{"at":"v1"}`))
	if again := multipartRequest(t, handler, route, map[string]string{"name": "snapshot.json"}, "snapshot.json", "application/json", []byte(`{"at":"v2"}`), "alice"); again.Code != http.StatusCreated {
		t.Fatalf("second version of snapshot.json: status=%d body=%s", again.Code, again.Body.String())
	}

	pinned := grantFile(t, handler, fixture.page.Artifact.ID, "dispatch://"+fixture.issue+"/artifact/snapshot-json@v1")
	live := grantFile(t, handler, fixture.page.Artifact.ID, fixture.dataRef)

	listed := listGrants(t, handler, fixture.page.Artifact.ID)
	if len(listed) != 2 {
		t.Fatalf("the grants = %+v, want the pinned one and the live one", listed)
	}
	byID := map[string]listedGrant{}
	for _, grant := range listed {
		byID[grant.ID] = grant
	}
	got, ok := byID[pinned.ID]
	if !ok || got.Version == nil || *got.Version != 1 || got.ArtifactID != pinnedFile.Artifact.ID ||
		got.Ref != "dispatch://"+fixture.issue+"/artifact/snapshot-json" || got.Name != "snapshot.json" || got.GrantedBy != "alice" {
		t.Errorf("the pinned grant = %+v, want version 1 of snapshot.json granted by alice", got)
	}
	got, ok = byID[live.ID]
	if !ok || got.Version != nil || got.ArtifactID != fixture.data.Artifact.ID || got.Ref != fixture.dataRef || got.Name != "attempts.json" {
		t.Errorf("the unpinned grant = %+v, want every version of attempts.json", got)
	}
	// Oldest first, so the panel lists them in the order a person added them.
	if !slices.Equal([]string{listed[0].ID, listed[1].ID}, []string{pinned.ID, live.ID}) {
		t.Errorf("the grants are listed %q then %q, want the order they were added", listed[0].ID, listed[1].ID)
	}
}

// TestArtifactGrantsChangeOnAClosedIssue: a grant is access, not content. Blocks elsewhere still
// show a page whose issue is closed, so a person may add and remove its grants after the close.
func TestArtifactGrantsChangeOnAClosedIssue(t *testing.T) {
	handler := newTestHandler(t)
	fixture := seedGrantsFixture(t, handler, "GRANT")
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+fixture.issue,
		map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close the issue: status=%d body=%s", closed.Code, closed.Body.String())
	}

	granted := grantFile(t, handler, fixture.page.Artifact.ID, fixture.dataRef)
	if listed := listGrants(t, handler, fixture.page.Artifact.ID); len(listed) != 1 || listed[0].ID != granted.ID {
		t.Fatalf("grants on a closed issue = %+v, want the one just added", listed)
	}
	if removed := dispatchRequest(t, handler, http.MethodDelete, grantsRoute(fixture.page.Artifact.ID)+"/"+granted.ID, nil, "alice"); removed.Code != http.StatusNoContent {
		t.Fatalf("remove a grant on a closed issue: status=%d body=%s", removed.Code, removed.Body.String())
	}
	if listed := listGrants(t, handler, fixture.page.Artifact.ID); len(listed) != 0 {
		t.Fatalf("grants after the removal = %+v, want none", listed)
	}
}
