package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func openEmptyTestStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL, err := url.Parse(os.Getenv("DISPATCH_TEST_DATABASE_URL"))
	if err != nil || baseURL.String() == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres API tests")
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("open test database admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	databaseName := "dispatch_api_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(context.Background(), "create database "+databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+databaseName+" with (force)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})

	testURL := *baseURL
	testURL.Path = "/" + databaseName
	database, err := store.Open(context.Background(), testURL.String())
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate isolated database: %v", err)
	}
	return database
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, _ := newTestHandlerWithStore(t)
	return handler
}

func newTestHandlerWithStore(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	database := openEmptyTestStore(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}, "bob": {}},
		},
		AgentToken:      "agent-token",
		RepoProjectsRaw: "owner/repo=TEST",
		ServerURL:       "https://dispatch.example",
		Docs:            documentService,
		Events:          broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux, database
}

func newTestHandlerWithDefaultProject(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	database := openEmptyTestStore(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}, "bob": {}},
		},
		AgentToken:      "agent-token",
		RepoProjectsRaw: "owner/repo=TEST",
		DefaultProject:  "DEFAULT",
		ServerURL:       "https://dispatch.example",
		Docs:            documentService,
		Events:          broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux, database
}

func newTestHandlerWithTestHooks(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	database := openEmptyTestStore(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}, "bob": {}},
		},
		AgentToken:       "agent-token",
		RepoProjectsRaw:  "owner/repo=TEST",
		ServerURL:        "https://dispatch.example",
		Docs:             documentService,
		Events:           broker,
		TestHooksEnabled: true,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux, database
}

func waitForDatabaseLocks(t *testing.T, database *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&count); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if count >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waiting database locks: wanted at least %d blocked operations", want)
}

func awaitResponse(t *testing.T, responses <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent request did not complete")
		return nil
	}
}

func dispatchRequest(t *testing.T, handler http.Handler, method, target string, body any, login string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func multipartRequest(t *testing.T, handler http.Handler, target string, fields map[string]string, filename, contentType string, content []byte, login string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write multipart field: %v", err)
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("create multipart file part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write multipart file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("finish multipart request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, target, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeBody[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	return value
}

func TestCreateProjectIssueAndReadPrimaryDocument(t *testing.T) {
	handler := newTestHandler(t)
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}

	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "First issue", "spec": "# Hello",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
		LastSeq           int    `json:"last_seq"`
	}](t, issueResponse)
	if issue.Key != "TEST-1" {
		t.Fatalf("issue key: got %q, want TEST-1", issue.Key)
	}
	if issue.PrimaryArtifactID == "" {
		t.Fatal("created issue has no primary artifact")
	}
	if issue.LastSeq != 1 {
		t.Fatalf("created issue event sequence: got %d, want 1", issue.LastSeq)
	}

	textResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if textResponse.Code != http.StatusOK {
		t.Fatalf("read primary document: status=%d body=%s", textResponse.Code, textResponse.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
		Version  *int   `json:"version"`
	}](t, textResponse)
	if text.Markdown != "# Hello" || text.Version != nil {
		t.Fatalf("primary document: got %#v, want markdown # Hello and null version", text)
	}
}

func TestCreateIssueWithMissingOrBlankSpecSeedsPrimaryDocument(t *testing.T) {
	handler := newTestHandler(t)
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}

	empty := ""
	whitespace := " \n\t "
	for _, test := range []struct {
		name string
		spec *string
	}{
		{name: "omitted"},
		{name: "empty", spec: &empty},
		{name: "whitespace", spec: &whitespace},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := map[string]string{"project": "TEST", "title": "Seeded issue"}
			if test.spec != nil {
				input["spec"] = *test.spec
			}
			issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", input, "alice")
			if issueResponse.Code != http.StatusCreated {
				t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
			}
			issue := decodeBody[struct {
				PrimaryArtifactID string `json:"primary_artifact_id"`
			}](t, issueResponse)

			textResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
			if textResponse.Code != http.StatusOK {
				t.Fatalf("read primary document: status=%d body=%s", textResponse.Code, textResponse.Body.String())
			}
			text := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, textResponse)
			if !strings.HasPrefix(text.Markdown, "## Decisions needed") {
				t.Fatalf("primary document = %q, want it to start with Decisions needed", text.Markdown)
			}
		})
	}
}

func TestSeededSpecHeadingsMatchDispatchWritingGuidance(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read primary document: status=%d body=%s", response.Code, response.Body.String())
	}
	document := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, response)

	skill, err := os.ReadFile("../../../../../skills/dispatch/SKILL.md")
	if err != nil {
		t.Fatalf("read Dispatch skill: %v", err)
	}
	const tableHeader = "| Section | Required content | Form |\n"
	_, table, found := strings.Cut(string(skill), tableHeader)
	if !found {
		t.Fatal("Dispatch skill has no Writing a spec section table")
	}
	var want []string
	for _, line := range strings.Split(table, "\n") {
		if line == "" {
			break
		}
		if !strings.HasPrefix(line, "| **") {
			continue
		}
		section, _, found := strings.Cut(strings.TrimPrefix(line, "| **"), "** |")
		if !found {
			t.Fatalf("malformed Dispatch skill section row %q", line)
		}
		want = append(want, section)
	}

	var got []string
	for _, line := range strings.Split(document.Markdown, "\n") {
		if section, found := strings.CutPrefix(line, "## "); found {
			got = append(got, section)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("seeded spec headings = %q, want %q from Dispatch guidance", got, want)
	}
}

func TestDocumentTextReportsUnavailableService(t *testing.T) {
	database := openEmptyTestStore(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store:       database,
		Persistence: failingLoadStore{VersionedStore: docs.NewPgVersioned(database)},
		Events:      broker,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}},
		},
		Docs:   documentService,
		Events: broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)

	if response := dispatchRequest(t, mux, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, mux, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue", "spec": "before",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, issueResponse)

	response := dispatchRequest(t, mux, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"DOC_SERVICE_UNAVAILABLE"`) {
		t.Fatalf("unavailable document response: status=%d body=%s", response.Code, response.Body.String())
	}
}

type failingLoadStore struct {
	docs.VersionedStore
}

func (failingLoadStore) Load(context.Context, string) (persistence.LoadResult, error) {
	return persistence.LoadResult{}, errors.New("persistence unavailable")
}

func TestDocumentEditRejectsInvalidOperationField(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue", "spec": "before",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, issueResponse)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "with": "after"}},
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_OP"`) || !strings.Contains(response.Body.String(), "find") {
		t.Fatalf("invalid edit response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEventNotifyRules(t *testing.T) {
	broker := events.NewBroker()
	user := model.Actor{Kind: "user", ID: "alice"}
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for _, test := range []struct {
		name  string
		event model.Event
		want  bool
	}{
		{name: "user message", event: model.Event{Type: "message.created", Actor: user}, want: true},
		{name: "session message", event: model.Event{Type: "message.created", Actor: session}, want: false},
		{name: "user title change", event: model.Event{Type: "issue.updated", Actor: user}, want: true},
		{name: "session ask", event: model.Event{Type: "ask.opened", Actor: session}, want: false},
		{name: "session resolve", event: model.Event{Type: "ask.resolved", Actor: session}, want: false},
		{name: "user resolve", event: model.Event{Type: "ask.resolved", Actor: user}, want: true},
		{name: "user named version", event: model.Event{Type: "artifact.version", Actor: user, Payload: map[string]any{"version": model.Version{Named: true}}}, want: true},
		{name: "session named version", event: model.Event{Type: "artifact.version", Actor: session, Payload: map[string]any{"version": model.Version{Named: true}}}, want: false},
		{name: "child status", event: model.Event{Type: "child.status", Actor: session}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := broker.Notify(test.event); got != test.want {
				t.Fatalf("Notify(%+v) = %t, want %t", test.event, got, test.want)
			}
		})
	}
}

func TestExternalIssueResolutionAndAutoCreation(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", response.Code, response.Body.String())
	}
	for number := 1; number <= 2; number++ {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "TEST", "title": "ordinary issue",
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue %d: status=%d body=%s", number, response.Code, response.Body.String())
		}
	}
	unlinked := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/resolve?ref=owner/repo%237", nil, "alice")
	if unlinked.Code != http.StatusNotFound {
		t.Fatalf("resolve unlinked external issue: status=%d body=%s", unlinked.Code, unlinked.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#7",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("auto-create external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key           string `json:"key"`
		ExternalLinks []struct {
			URL string `json:"url"`
		} `json:"external_links"`
	}](t, created)
	if issue.Key != "TEST-3" {
		t.Fatalf("external issue key: got %q, want TEST-3", issue.Key)
	}
	if len(issue.ExternalLinks) != 1 || issue.ExternalLinks[0].URL != "https://github.com/owner/repo/issues/7" {
		t.Fatalf("external links: got %#v", issue.ExternalLinks)
	}
}

func TestExternalIssueRejectsUnmappedProject(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "unmapped/repository#1",
	}, "alice")
	body := response.Body.String()
	if response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"PROJECT_UNMAPPED"`) ||
		!strings.Contains(body, "repository settings") || !strings.Contains(body, "DISPATCH_DEFAULT_PROJECT") {
		t.Fatalf("create unmapped external issue: status=%d body=%s", response.Code, body)
	}
}

func TestExternalIssueUsesDefaultProjectForUnmappedRepository(t *testing.T) {
	handler, _ := newTestHandlerWithDefaultProject(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "DEFAULT", "name": "Default project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create default project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "another/repo#5",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create default-project external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key           string   `json:"key"`
		Labels        []string `json:"labels"`
		ExternalLinks []struct {
			URL string `json:"url"`
		} `json:"external_links"`
	}](t, created)
	if issue.Key != "DEFAULT-1" {
		t.Fatalf("default-project issue key: got %q, want DEFAULT-1", issue.Key)
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != repoLabelPrefix+"another/repo" {
		t.Fatalf("default-project issue labels: got %#v, want [%sanother/repo]", issue.Labels, repoLabelPrefix)
	}
	if len(issue.ExternalLinks) != 1 || issue.ExternalLinks[0].URL != "https://github.com/another/repo/issues/5" {
		t.Fatalf("default-project issue external links: got %#v", issue.ExternalLinks)
	}
}

func TestExternalIssueExplicitMappingTakesPrecedenceOverDefaultProject(t *testing.T) {
	handler, _ := newTestHandlerWithDefaultProject(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create mapped project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "DEFAULT", "name": "Default project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create default project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#9",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create mapped external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key    string   `json:"key"`
		Labels []string `json:"labels"`
	}](t, created)
	if issue.Key != "TEST-1" {
		t.Fatalf("explicit-mapping issue key: got %q, want TEST-1", issue.Key)
	}
	if len(issue.Labels) != 0 {
		t.Fatalf("explicit-mapping issue labels: got %#v, want none", issue.Labels)
	}
}

func TestArtifactVersions(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue", "spec": "# Initial",
	}, "alice")
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, issueResponse)
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}

	first := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "a.png"}, "a.png", "image/png", []byte("first image"), "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload first image: status=%d body=%s", first.Code, first.Body.String())
	}
	image := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, first)
	blob := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+image.Artifact.ID+"/versions/1", nil, "alice")
	checksum := sha256.Sum256([]byte("first image"))
	if blob.Code != http.StatusOK || blob.Body.String() != "first image" || blob.Header().Get("Content-Type") != "image/png" || blob.Header().Get("ETag") != hex.EncodeToString(checksum[:]) || blob.Header().Get("Content-Disposition") != "attachment" || blob.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("stream first blob: status=%d headers=%v body=%s", blob.Code, blob.Header(), blob.Body.String())
	}

	second := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "a.png"}, "a.png", "image/png", []byte("second image"), "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("upload second image: status=%d body=%s", second.Code, second.Body.String())
	}
	version := decodeBody[struct {
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}](t, second)
	if version.Version.Number != 2 {
		t.Fatalf("second image version: got %d, want 2", version.Version.Number)
	}

	notes := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("# Notes"), "alice")
	if notes.Code != http.StatusCreated {
		t.Fatalf("upload document: status=%d body=%s", notes.Code, notes.Body.String())
	}
	uploaded := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, notes)
	replacement := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("# Revised"), "alice")
	if replacement.Code != http.StatusCreated {
		t.Fatalf("replace document: status=%d body=%s", replacement.Code, replacement.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+uploaded.Artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Revised"`) {
		t.Fatalf("read replaced document: status=%d body=%s", text.Code, text.Body.String())
	}

}

func agentRequest(t *testing.T, handler http.Handler, method, target string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode agent request body: %v", err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestIssueRouteVersionEventsAndChildStatus(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	parentResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Parent",
	}, "alice")
	parent := decodeBody[struct {
		Key string `json:"key"`
	}](t, parentResponse)
	childResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Child", "parent": parent.Key,
	}, "alice")
	child := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, childResponse)
	if childResponse.Code != http.StatusCreated {
		t.Fatalf("create child: status=%d body=%s", childResponse.Code, childResponse.Body.String())
	}
	updated := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
		"route": "role:legion-controller-x",
	}, "alice")
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"route":"role:legion-controller-x"`) {
		t.Fatalf("set valid route: status=%d body=%s", updated.Code, updated.Body.String())
	}
	invalid := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
		"route": "bogus",
	}, "alice")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"ROUTE_INVALID"`) {
		t.Fatalf("reject invalid route: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	status := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
		"status": "in_progress",
	}, "alice")
	if status.Code != http.StatusOK {
		t.Fatalf("change child status: status=%d body=%s", status.Code, status.Body.String())
	}

	named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+child.PrimaryArtifactID+"/versions", map[string]string{
		"summary": "checkpoint",
	}, "alice")
	if named.Code != http.StatusCreated || !strings.Contains(named.Body.String(), `"named":true`) {
		t.Fatalf("create user named version: status=%d body=%s", named.Code, named.Body.String())
	}
	blankSummary := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+child.PrimaryArtifactID+"/versions", map[string]string{
		"summary": "   ",
	}, "alice")
	if blankSummary.Code != http.StatusBadRequest || !strings.Contains(blankSummary.Body.String(), `"code":"INVALID_VERSION"`) {
		t.Fatalf("blank-summary named version: status=%d body=%s", blankSummary.Code, blankSummary.Body.String())
	}
	sessionVersion := agentRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+child.PrimaryArtifactID+"/versions", map[string]any{
		"summary": "agent checkpoint",
		"actor":   map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "agent-token")
	if sessionVersion.Code != http.StatusCreated {
		t.Fatalf("create session named version: status=%d body=%s", sessionVersion.Code, sessionVersion.Body.String())
	}

	childEvents := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child.Key+"/events", nil, "alice")
	var childLog []struct {
		Seq    int    `json:"seq"`
		Type   string `json:"type"`
		Notify bool   `json:"notify"`
	}
	if err := json.NewDecoder(childEvents.Body).Decode(&childLog); err != nil {
		t.Fatalf("decode child events: %v", err)
	}
	if len(childLog) != 5 {
		t.Fatalf("child events: got %#v, want five events", childLog)
	}
	for index, event := range childLog {
		if event.Seq != index+1 {
			t.Fatalf("child event %d sequence: got %d, want %d", index, event.Seq, index+1)
		}
	}
	if !childLog[3].Notify || childLog[4].Notify {
		t.Fatalf("named version notifications: got user=%t session=%t", childLog[3].Notify, childLog[4].Notify)
	}

	parentEvents := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+parent.Key+"/events", nil, "alice")
	var parentLog []struct {
		Type   string `json:"type"`
		Notify bool   `json:"notify"`
	}
	if err := json.NewDecoder(parentEvents.Body).Decode(&parentLog); err != nil {
		t.Fatalf("decode parent events: %v", err)
	}
	if len(parentLog) != 2 || parentLog[1].Type != "child.status" || !parentLog[1].Notify {
		t.Fatalf("parent events: got %#v, want child.status notification", parentLog)
	}
}

func TestListIssueEventsSupportsNewestCursorAndIDLookup(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Event queries",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, issueResponse)

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event seed transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	broker := events.NewBroker()
	eventIDs := make(map[int]int64, 249)
	for wantSeq := 2; wantSeq <= 250; wantSeq++ {
		event, err := broker.Append(ctx, tx, model.Event{
			IssueKey: issue.Key,
			Type:     "test.seeded",
			Actor:    model.Actor{Kind: "session", ID: "seed-session-0123456789"},
			Payload:  map[string]any{"seq": wantSeq},
		})
		if err != nil {
			t.Fatalf("append event %d: %v", wantSeq, err)
		}
		if event.Seq != wantSeq {
			t.Fatalf("seed event sequence: got %d, want %d", event.Seq, wantSeq)
		}
		eventIDs[wantSeq] = event.ID
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit event seed transaction: %v", err)
	}

	type listedEvent struct {
		ID  int64 `json:"id"`
		Seq int   `json:"seq"`
	}
	newest := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events?order=desc", nil, "alice")
	if newest.Code != http.StatusOK {
		t.Fatalf("list newest events: status=%d body=%s", newest.Code, newest.Body.String())
	}
	newestLog := decodeBody[[]listedEvent](t, newest)
	if len(newestLog) != 200 {
		t.Fatalf("newest events: got %d, want 200", len(newestLog))
	}
	for index, event := range newestLog {
		if want := 250 - index; event.Seq != want {
			t.Fatalf("newest event %d: got sequence %d, want %d", index, event.Seq, want)
		}
	}

	before := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events?before=51", nil, "alice")
	if before.Code != http.StatusOK {
		t.Fatalf("list events before cursor: status=%d body=%s", before.Code, before.Body.String())
	}
	beforeLog := decodeBody[[]listedEvent](t, before)
	if len(beforeLog) != 50 {
		t.Fatalf("events before cursor: got %d, want 50", len(beforeLog))
	}
	for index, event := range beforeLog {
		if want := 50 - index; event.Seq != want {
			t.Fatalf("event before cursor %d: got sequence %d, want %d", index, event.Seq, want)
		}
	}

	ids := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/issues/"+issue.Key+"/events?ids="+fmt.Sprintf("%d,%d,%d", eventIDs[250], eventIDs[2], eventIDs[100]),
		nil, "alice")
	if ids.Code != http.StatusOK {
		t.Fatalf("list event IDs: status=%d body=%s", ids.Code, ids.Body.String())
	}
	idLog := decodeBody[[]listedEvent](t, ids)
	for index, want := range []int{2, 100, 250} {
		if len(idLog) != 3 || idLog[index].Seq != want {
			t.Fatalf("events by ID: got %#v, want sequences [2 100 250]", idLog)
		}
	}

	for _, target := range []string{
		"/api/v1/issues/" + issue.Key + "/events?ids=" + strings.Repeat("1,", 50) + "1",
		"/api/v1/issues/" + issue.Key + "/events?after=0&before=10",
		"/api/v1/issues/" + issue.Key + "/events?after=0&order=desc",
		"/api/v1/issues/" + issue.Key + "/events?after=not-a-number",
		"/api/v1/issues/" + issue.Key + "/events?before=not-a-number",
		"/api/v1/issues/" + issue.Key + "/events?before=",
		"/api/v1/issues/" + issue.Key + "/events?before=%20",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_QUERY"`) {
			t.Fatalf("reject invalid event query %q: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestActorAuthenticationRules(t *testing.T) {
	handler := newTestHandler(t)
	unauthenticated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "")
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("header-less project creation: status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	noActor := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "agent-token")
	if noActor.Code != http.StatusBadRequest || !strings.Contains(noActor.Body.String(), `"code":"ACTOR_KIND"`) {
		t.Fatalf("bearer without actor: status=%d body=%s", noActor.Code, noActor.Body.String())
	}
	userActor := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "TEST", "name": "Test project", "actor": map[string]string{"kind": "user", "id": "alice"},
	}, "agent-token")
	if userActor.Code != http.StatusBadRequest || !strings.Contains(userActor.Body.String(), `"code":"ACTOR_KIND"`) {
		t.Fatalf("bearer user actor: status=%d body=%s", userActor.Code, userActor.Body.String())
	}
	wrongToken := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "TEST", "name": "Test project", "actor": map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "wrong-token")
	if wrongToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: status=%d body=%s", wrongToken.Code, wrongToken.Body.String())
	}
}

func TestBearerReadRoutesDoNotRequireActor(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST",
		"title":   "Bearer reads",
		"spec":    "# Read me",
		"actor":   map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "agent-token")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	askResponse := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Is this readable?",
		"actor":    map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "agent-token")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, askResponse)
	comment := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":  "A readable comment",
		"actor": map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "agent-token")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}

	for _, target := range []string{
		"/api/v1/projects",
		"/api/v1/issues",
		"/api/v1/issues/resolve?ref=" + issue.Key,
		"/api/v1/issues/" + issue.Key,
		"/api/v1/issues/" + issue.Key + "/events",
		"/api/v1/issues/" + issue.Key + "/artifacts",
		"/api/v1/asks/" + ask.ID,
		"/api/v1/issues/" + issue.Key + "/comments",
		"/api/v1/artifacts/" + issue.PrimaryArtifactID,
		"/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text",
		"/api/v1/artifacts/" + issue.PrimaryArtifactID + "/versions/1",
	} {
		t.Run(target, func(t *testing.T) {
			response := agentRequest(t, handler, http.MethodGet, target, nil, "agent-token")
			if response.Code != http.StatusOK {
				t.Fatalf("bearer read: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	inbox := agentRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "agent-token")
	if inbox.Code != http.StatusForbidden || !strings.Contains(inbox.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("bearer inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	anonymous := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "")
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous issue read: status=%d body=%s", anonymous.Code, anonymous.Body.String())
	}
}

func TestSSEReplaysThenStreamsCommittedEvent(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events?since=0", nil)
	if err != nil {
		t.Fatalf("construct SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	responseChannel := make(chan *http.Response, 1)
	errorChannel := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			errorChannel <- err
			return
		}
		responseChannel <- response
	}()
	var stream *http.Response
	select {
	case err := <-errorChannel:
		t.Fatalf("connect SSE: %v", err)
	case stream = <-responseChannel:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE endpoint did not establish a stream")
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: got %d, want 200", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)
	replayed := readSSEFrame(t, scanner)
	if replayed[1] != "event: issue.created" || !strings.Contains(replayed[2], `"issue_key":"TEST-1"`) {
		t.Fatalf("replayed SSE frame: %#v", replayed)
	}

	updated := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed",
	}, "alice")
	if updated.Code != http.StatusOK {
		t.Fatalf("update issue during SSE: status=%d body=%s", updated.Code, updated.Body.String())
	}
	live := readSSEFrame(t, scanner)
	if live[1] != "event: issue.updated" || !strings.Contains(live[2], `"title":"Renamed"`) {
		t.Fatalf("live SSE frame: %#v", live)
	}
	stream.Body.Close()
	resumeRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatalf("construct resumed SSE request: %v", err)
	}
	resumeRequest.Header.Set("Authorization", "Bearer agent-token")
	resumeRequest.Header.Set("Last-Event-ID", "1")
	resumed, err := http.DefaultClient.Do(resumeRequest)
	if err != nil {
		t.Fatalf("resume SSE: %v", err)
	}
	defer resumed.Body.Close()
	resumedFrame := readSSEFrame(t, bufio.NewScanner(resumed.Body))
	if resumedFrame[0] != "id: 2" || resumedFrame[1] != "event: issue.updated" {
		t.Fatalf("Last-Event-ID replay: %#v", resumedFrame)
	}
	staleQueryRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events?since=0", nil)
	if err != nil {
		t.Fatalf("construct stale-query SSE request: %v", err)
	}
	staleQueryRequest.Header.Set("Authorization", "Bearer agent-token")
	staleQueryRequest.Header.Set("Last-Event-ID", "1")
	staleQuery, err := http.DefaultClient.Do(staleQueryRequest)
	if err != nil {
		t.Fatalf("resume stale-query SSE: %v", err)
	}
	defer staleQuery.Body.Close()
	staleQueryFrame := readSSEFrame(t, bufio.NewScanner(staleQuery.Body))
	if staleQueryFrame[0] != "id: 2" || staleQueryFrame[1] != "event: issue.updated" {
		t.Fatalf("Last-Event-ID with stale since query replay: %#v", staleQueryFrame)
	}
}

func readSSEFrame(t *testing.T, scanner *bufio.Scanner) []string {
	t.Helper()
	frame := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return frame
		}
		frame = append(frame, line)
	}
	t.Fatalf("read SSE frame: %v", scanner.Err())
	return nil
}

func TestPerUserIssueStateIsIsolated(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"pinned": true, "last_read_seq": 1, "dismissed": []string{"ask-1"},
	}, "alice")
	if saved.Code != http.StatusOK {
		t.Fatalf("save Alice state: status=%d body=%s", saved.Code, saved.Body.String())
	}
	alice := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "alice")
	if !strings.Contains(alice.Body.String(), `"TEST-1":{"pinned":true,"last_read_seq":1,"dismissed":["ask-1"]}`) {
		t.Fatalf("Alice state: status=%d body=%s", alice.Code, alice.Body.String())
	}
	bob := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "bob")
	if bob.Code != http.StatusOK || strings.Contains(bob.Body.String(), issue.Key) {
		t.Fatalf("Bob state leaked Alice pin: status=%d body=%s", bob.Code, bob.Body.String())
	}
	session := agentRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "agent-token")
	if session.Code != http.StatusForbidden || !strings.Contains(session.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("session state: status=%d body=%s", session.Code, session.Body.String())
	}
}

func TestMessageMutationAppendsAndPublishesEvent(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]string{
		"body": "please review",
	}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", message.Code, message.Body.String())
	}
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if !strings.Contains(log.Body.String(), `"type":"message.created"`) || !strings.Contains(log.Body.String(), `"notify":true`) {
		t.Fatalf("message event: status=%d body=%s", log.Code, log.Body.String())
	}
}

func TestResolveIssueRejectsMalformedNativeKey(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/resolve?ref=TEST-not-a-number", nil, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_ISSUE_REF"`) {
		t.Fatalf("resolve malformed key: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestArtifactUploadRecognizesParameterizedMarkdown(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md",
	}, "notes.md", "text/markdown; charset=utf-8", []byte("# Notes"), "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload parameterized Markdown: status=%d body=%s", response.Code, response.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact struct {
			Kind string `json:"kind"`
		} `json:"artifact"`
	}](t, response)
	if artifact.Artifact.Kind != "doc" {
		t.Fatalf("parameterized Markdown kind: got %q, want doc", artifact.Artifact.Kind)
	}
}

func TestArtifactSlugsIncludeExtensions(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	markdown := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "report.md",
	}, "report.md", "text/markdown", []byte("# Report"), "alice")
	if markdown.Code != http.StatusCreated {
		t.Fatalf("upload report.md: status=%d body=%s", markdown.Code, markdown.Body.String())
	}
	pdf := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "report.pdf",
	}, "report.pdf", "application/pdf", []byte("PDF"), "alice")
	if pdf.Code != http.StatusCreated {
		t.Fatalf("upload report.pdf: status=%d body=%s", pdf.Code, pdf.Body.String())
	}
	markdownArtifact := decodeBody[struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}](t, markdown)
	pdfArtifact := decodeBody[struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}](t, pdf)
	if markdownArtifact.Artifact.Slug != "report-md" || pdfArtifact.Artifact.Slug != "report-pdf" {
		t.Fatalf("artifact slugs: got Markdown %q and PDF %q, want report-md and report-pdf", markdownArtifact.Artifact.Slug, pdfArtifact.Artifact.Slug)
	}
}

func TestArtifactSlugsDisambiguateNormalizedNameCollisions(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	markdown := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "report.md",
	}, "report.md", "text/markdown", []byte("# Report"), "alice")
	if markdown.Code != http.StatusCreated {
		t.Fatalf("upload report.md: status=%d body=%s", markdown.Code, markdown.Body.String())
	}
	dashed := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "report-md",
	}, "report-md", "application/octet-stream", []byte("report"), "alice")
	if dashed.Code != http.StatusCreated {
		t.Fatalf("upload report-md: status=%d body=%s", dashed.Code, dashed.Body.String())
	}
	versioned := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "report.md",
	}, "report.md", "text/markdown", []byte("# Revision"), "alice")
	if versioned.Code != http.StatusCreated {
		t.Fatalf("upload report.md version 2: status=%d body=%s", versioned.Code, versioned.Body.String())
	}
	first := decodeBody[struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}](t, markdown)
	second := decodeBody[struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}](t, dashed)
	version := decodeBody[struct {
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}](t, versioned)
	if first.Artifact.Slug != "report-md" || second.Artifact.Slug != "report-md-2" || version.Version.Number != 2 {
		t.Fatalf("artifact collision result: got slugs %q, %q and version %d; want report-md, report-md-2, 2", first.Artifact.Slug, second.Artifact.Slug, version.Version.Number)
	}
}

func TestConcurrentStatusPatchesUseCommittedPreimage(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	parentResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Parent",
	}, "alice")
	childResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Child", "parent": "TEST-1",
	}, "alice")
	parent := decodeBody[struct {
		Key string `json:"key"`
	}](t, parentResponse)
	child := decodeBody[struct {
		Key string `json:"key"`
	}](t, childResponse)
	if parentResponse.Code != http.StatusCreated || childResponse.Code != http.StatusCreated {
		t.Fatalf("create parent and child: parent=%d child=%d", parentResponse.Code, childResponse.Code)
	}

	lock, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin status lock: %v", err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	var lockedKey string
	if err := lock.QueryRow(context.Background(), `select key from issues where key = $1 for update`, child.Key).Scan(&lockedKey); err != nil {
		t.Fatalf("lock child: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
			"status": "in_progress",
		}, "alice")
	}()
	waitForDatabaseLocks(t, database, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
			"status": "done",
		}, "alice")
	}()
	waitForDatabaseLocks(t, database, 2)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatalf("release child lock: %v", err)
	}
	for range 2 {
		response := awaitResponse(t, responses)
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent status update: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	eventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+parent.Key+"/events", nil, "alice")
	var eventLog []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&eventLog); err != nil {
		t.Fatalf("decode parent events: %v", err)
	}
	var transitions []struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	for _, event := range eventLog {
		if event.Type != "child.status" {
			continue
		}
		var transition struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := json.Unmarshal(event.Payload, &transition); err != nil {
			t.Fatalf("decode child status payload: %v", err)
		}
		transitions = append(transitions, transition)
	}
	if len(transitions) != 2 || transitions[0].From != "triage" || transitions[0].To != "in_progress" || transitions[1].From != "in_progress" || transitions[1].To != "done" {
		t.Fatalf("child status transition chain: got %#v, want triage→in_progress→done", transitions)
	}
}

func TestConcurrentPartialUserStateUpdatesPreserveFields(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	initial := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"pinned": false,
	}, "alice")
	if initial.Code != http.StatusOK {
		t.Fatalf("create state: status=%d body=%s", initial.Code, initial.Body.String())
	}

	lock, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin user state lock: %v", err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	var login string
	if err := lock.QueryRow(context.Background(), `select login from user_issue_state where login = 'alice' and issue_key = $1 for update`, issue.Key).Scan(&login); err != nil {
		t.Fatalf("lock user state: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
			"pinned": true,
		}, "alice")
	}()
	waitForDatabaseLocks(t, database, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
			"last_read_seq": 7,
		}, "alice")
	}()
	waitForDatabaseLocks(t, database, 2)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatalf("release user state lock: %v", err)
	}
	for range 2 {
		response := awaitResponse(t, responses)
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent user state update: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	stateResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "alice")
	state := decodeBody[map[string]userIssueState](t, stateResponse)
	if !state[issue.Key].Pinned || state[issue.Key].LastReadSeq != 7 {
		t.Fatalf("user state: got %#v, want pinned with last_read_seq 7", state[issue.Key])
	}
}

func TestIssueExternalLinksRequireAbsoluteHTTPURLs(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "External links",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	for _, address := range []string{"javascript:alert(1)", "file:///etc/passwd", "/relative"} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
			"external_links": []map[string]string{{"url": address}},
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_URL"`) {
			t.Fatalf("reject unsafe link %q: status=%d body=%s", address, response.Code, response.Body.String())
		}
	}
	valid := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"external_links": []map[string]string{{"url": "http://100.64.0.1:8766/issues/TEST-1"}},
	}, "alice")
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), `"external_links":[{"url":"http://100.64.0.1:8766/issues/TEST-1","kind":"url"}]`) {
		t.Fatalf("accept HTTP external link: status=%d body=%s", valid.Code, valid.Body.String())
	}
}

func TestDocumentUploadIndexesDispatchReferences(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Document references",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	uploaded := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md",
	}, "notes.md", "text/markdown", []byte("See dispatch://TEST-1/artifact/spec."), "alice")
	artifact := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, uploaded)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	var references int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = 'TEST-1/spec'
	`, artifact.Artifact.ID).Scan(&references); err != nil {
		t.Fatalf("count uploaded document references: %v", err)
	}
	if references != 1 {
		t.Fatalf("uploaded document references = %d, want 1", references)
	}
}

func TestIssueDocumentCreationIndexesDispatchReferences(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Created references", "spec": "See dispatch://TEST-1/artifact/spec.",
	}, "alice")
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	var references int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = 'TEST-1/spec'
	`, issue.PrimaryArtifactID).Scan(&references); err != nil {
		t.Fatalf("count created document references: %v", err)
	}
	if references != 1 {
		t.Fatalf("created document references = %d, want 1", references)
	}
}

func TestRevokedCookieIsRejectedAcrossDispatchSurfaces(t *testing.T) {
	database := openEmptyTestStore(t)
	allowed := map[string]struct{}{"alice": {}}
	cookieIdentity := identity.CookieIdentity{SigningKey: "signing-key", AllowedLogins: allowed}
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, Identity: cookieIdentity, Settle: time.Hour,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: cookieIdentity, AgentToken: "agent-token",
		RepoProjectsRaw: "owner/repo=TEST", Docs: documentService, Events: broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	handler := http.NewServeMux()
	Register(handler, deps)
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("alice", "signing-key"))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	request := func(method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload *bytes.Reader
		if body == nil {
			payload = bytes.NewReader(nil)
		} else {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("encode request: %v", err)
			}
			payload = bytes.NewReader(encoded)
		}
		req := httptest.NewRequest(method, target, payload)
		req.AddCookie(cookie)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := request(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "Test"}); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := request(http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Cookie issue"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)

	delete(allowed, "alice")
	for _, target := range []string{"/api/v1/issues/" + issue.Key, "/api/v1/events"} {
		response := request(http.MethodGet, target, nil)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"LOGIN_NOT_ALLOWED"`) {
			t.Fatalf("revoked cookie %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + issue.PrimaryArtifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, http.Header{"Cookie": []string{cookie.Name + "=" + cookie.Value}})
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || (response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden) {
		t.Fatalf("revoked cookie websocket: response=%#v err=%v, want rejected handshake", response, err)
	}
}

func TestArtifactUploadSummaryCreatesNamedVersions(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Named uploads", "before")
	for _, test := range []struct {
		name        string
		artifact    string
		contentType string
		content     []byte
	}{
		{name: "document", artifact: "notes.md", contentType: "text/markdown", content: []byte("# Notes")},
		{name: "blob", artifact: "data.pdf", contentType: "application/pdf", content: []byte("%PDF")},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
				"name": test.artifact, "summary": "Initial upload",
			}, test.artifact, test.contentType, test.content, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("upload artifact: status=%d body=%s", response.Code, response.Body.String())
			}
			upload := decodeBody[struct {
				Version model.Version `json:"version"`
			}](t, response)
			if !upload.Version.Named || upload.Version.Summary == nil || *upload.Version.Summary != "Initial upload" {
				t.Fatalf("upload version = %#v, want named version with summary", upload.Version)
			}
		})
	}
}

func TestSSEPagesThroughCappedBacklogWithoutDisconnecting(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, value, 'seeded', jsonb_build_object('kind', 'session', 'id', 'seed'), jsonb_build_object('seq', value), false
		from generate_series(2, 1006) as value
	`, issue.Key); err != nil {
		t.Fatalf("seed replay events: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `update issues set last_seq = 1006 where key = $1`, issue.Key); err != nil {
		t.Fatalf("sync issue last_seq after seeding: %v", err)
	}

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events?since=1", nil)
	if err != nil {
		t.Fatalf("construct capped SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open capped SSE stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("capped SSE status: got %d, want %d", stream.StatusCode, http.StatusOK)
	}
	scanner := bufio.NewScanner(stream.Body)
	// The backlog (1005 events) exceeds one capped page (maxSSEReplay=1000); a
	// single connection pages through all of it without ever disconnecting — a
	// capped page used to end the stream and force a client reconnect, which left
	// a gap where a low id committing between "read this page" and "a new
	// connection subscribes" could be lost forever.
	for wantID := 2; wantID <= 1006; wantID++ {
		frame := readSSEFrame(t, scanner)
		if frame[0] != fmt.Sprintf("id: %d", wantID) {
			t.Fatalf("paged replay frame = %#v, want id %d", frame, wantID)
		}
	}
	// The same connection is still open afterward: a live event delivers with no
	// reconnect required.
	updated := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed after paging",
	}, "alice")
	if updated.Code != http.StatusOK {
		t.Fatalf("update issue after paged backlog: status=%d body=%s", updated.Code, updated.Body.String())
	}
	live := readSSEFrame(t, scanner)
	if live[1] != "event: issue.updated" || !strings.Contains(live[2], `"title":"Renamed after paging"`) {
		t.Fatalf("live frame after paged backlog: %#v", live)
	}
}

func TestDisconnectAllStreamsHookIsGatedByTestHooksEnabled(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/events/_test/disconnect", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Fatalf("disconnect hook without TestHooksEnabled: status=%d, want %d", response.Code, http.StatusNotFound)
	}
}

// TestDisconnectAllStreamsClosesOpenConnections proves the test-only hook drives a
// client through its reconnect-from-lastId path without seeding thousands of events
// to trip the replay cap: the same recovery path useEventStream exercises after any
// real disconnect (dropped connection, server restart, tab sleep).
func TestDisconnectAllStreamsClosesOpenConnections(t *testing.T) {
	handler, _ := newTestHandlerWithTestHooks(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events?since=0", nil)
	if err != nil {
		t.Fatalf("construct SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: got %d, want 200", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)
	replayed := readSSEFrame(t, scanner)
	if replayed[1] != "event: issue.created" {
		t.Fatalf("replayed SSE frame: %#v", replayed)
	}

	disconnect := agentRequest(t, handler, http.MethodPost, "/api/v1/events/_test/disconnect", nil, "agent-token")
	if disconnect.Code != http.StatusOK {
		t.Fatalf("disconnect-all hook: status=%d body=%s", disconnect.Code, disconnect.Body.String())
	}

	streamEnded := make(chan bool, 1)
	go func() {
		more := scanner.Scan()
		streamEnded <- !more
	}()
	select {
	case ended := <-streamEnded:
		if !ended {
			t.Fatalf("stream emitted an unexpected line after disconnect-all: %q", scanner.Text())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect-all did not close the open SSE stream")
	}

	// A client reconnecting with Last-Event-ID after this forced close (exactly what
	// useEventStream's watchdog/error path does) resumes from lastId, not from 0.
	resumeRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatalf("construct resumed SSE request: %v", err)
	}
	resumeRequest.Header.Set("Authorization", "Bearer agent-token")
	resumeRequest.Header.Set("Last-Event-ID", "1")
	updated := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed after disconnect",
	}, "alice")
	if updated.Code != http.StatusOK {
		t.Fatalf("update issue after disconnect: status=%d body=%s", updated.Code, updated.Body.String())
	}
	resumed, err := http.DefaultClient.Do(resumeRequest)
	if err != nil {
		t.Fatalf("resume SSE: %v", err)
	}
	defer resumed.Body.Close()
	resumedFrame := readSSEFrame(t, bufio.NewScanner(resumed.Body))
	if resumedFrame[0] != "id: 2" || resumedFrame[1] != "event: issue.updated" {
		t.Fatalf("resumed replay after disconnect-all: %#v", resumedFrame)
	}
}

func newTestHandlerWithBroker(t *testing.T) (http.Handler, *store.Store, *events.Broker) {
	t.Helper()
	database := openEmptyTestStore(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}, "bob": {}},
		},
		AgentToken:      "agent-token",
		RepoProjectsRaw: "owner/repo=TEST",
		ServerURL:       "https://dispatch.example",
		Docs:            documentService,
		Events:          broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux, database, broker
}

// TestSSELiveEventBelowSinceIsNotDropped proves the server never uses an id
// comparison to decide whether to forward a live event. Event ids are assigned by
// nextval() when an insert runs, before commit, so a transaction that grabbed a
// lower id can commit after a higher id is already visible — exactly the ordering
// this test forces. The old `event.ID <= lastID` filter would have silently
// dropped the late-committing lower id forever.
func TestSSELiveEventBelowSinceIsNotDropped(t *testing.T) {
	handler, database, broker := newTestHandlerWithBroker(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)

	ctx := context.Background()
	// Holds a low id open, uncommitted — simulating a request that started first
	// but is slower to finish than one that starts later.
	stalled, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin stalled transaction: %v", err)
	}
	defer stalled.Rollback(ctx)
	var lowID int64
	if err := stalled.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, 100, 'seeded', '{"kind":"session","id":"seed"}'::jsonb, '{}'::jsonb, false)
		returning id
	`, issue.Key).Scan(&lowID); err != nil {
		t.Fatalf("insert stalled event: %v", err)
	}

	// An independent insert grabs a higher id and commits immediately — this is
	// what makes it visible as the current head before the lower id ever is.
	var highID int64
	if err := database.Pool.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, 101, 'seeded', '{"kind":"session","id":"seed"}'::jsonb, '{}'::jsonb, false)
		returning id
	`, issue.Key).Scan(&highID); err != nil {
		t.Fatalf("insert committed event: %v", err)
	}
	if highID <= lowID {
		t.Fatalf("expected the committed insert to take a higher id than the stalled one: low=%d high=%d", lowID, highID)
	}
	broker.Publish(model.Event{
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		ID:       highID,
		IssueKey: issue.Key,
		Payload:  map[string]any{},
		Seq:      101,
		Type:     "seeded",
	})

	// A client opens the stream at since=highID: it has seen everything the server
	// can currently prove exists, but not the still-uncommitted lowID.
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events?since="+fmt.Sprintf("%d", highID), nil)
	if err != nil {
		t.Fatalf("construct SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: got %d, want 200", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)

	// The stalled transaction finally commits and is published: a lower id than
	// the client's since, arriving live after the stream is already open.
	if err := stalled.Commit(ctx); err != nil {
		t.Fatalf("commit stalled transaction: %v", err)
	}
	broker.Publish(model.Event{
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		ID:       lowID,
		IssueKey: issue.Key,
		Payload:  map[string]any{},
		Seq:      100,
		Type:     "seeded",
	})

	frame := readSSEFrame(t, scanner)
	if frame[0] != fmt.Sprintf("id: %d", lowID) {
		t.Fatalf("expected the late-committing lower id %d to be delivered, got %#v", lowID, frame)
	}
}

// TestSSECappedCatchupStillDeliversLowerIDCommittedDuringPaging proves the fix for
// the recurrence: a capped catch-up page used to end the stream (forcing a client
// reconnect with a higher Last-Event-ID), which meant a still-uncommitted low id —
// invisible to every catch-up page, since each page's cursor only moves forward —
// could never be recovered once it finally committed. Keeping one subscription
// attached across every page closes that gap.
func TestSSECappedCatchupStillDeliversLowerIDCommittedDuringPaging(t *testing.T) {
	handler, database, broker := newTestHandlerWithBroker(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)

	ctx := context.Background()
	// Holds a low id open, uncommitted, before the 1000-row backlog below is
	// seeded — so it keeps a lower id than all of them for the entire test.
	stalled, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin stalled transaction: %v", err)
	}
	defer stalled.Rollback(ctx)
	var lowID int64
	if err := stalled.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, 100, 'seeded', '{"kind":"session","id":"seed"}'::jsonb, '{}'::jsonb, false)
		returning id
	`, issue.Key).Scan(&lowID); err != nil {
		t.Fatalf("insert stalled event: %v", err)
	}

	// Exactly maxSSEReplay committed events, all with higher ids than lowID, so
	// the first catch-up page hits the cap and a second page runs.
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, value, 'seeded', jsonb_build_object('kind', 'session', 'id', 'seed'), jsonb_build_object('seq', value), false
		from generate_series(101, 1100) as value
	`, issue.Key); err != nil {
		t.Fatalf("seed capped backlog: %v", err)
	}

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events?since=1", nil)
	if err != nil {
		t.Fatalf("construct SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: got %d, want 200", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)
	for range 1000 {
		readSSEFrame(t, scanner)
	}

	// The stalled transaction finally commits, past the point where any catch-up
	// page could still see it (every page's cursor has already moved beyond it).
	if err := stalled.Commit(ctx); err != nil {
		t.Fatalf("commit stalled transaction: %v", err)
	}
	broker.Publish(model.Event{
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		ID:       lowID,
		IssueKey: issue.Key,
		Payload:  map[string]any{},
		Seq:      100,
		Type:     "seeded",
	})

	frame := readSSEFrame(t, scanner)
	if frame[0] != fmt.Sprintf("id: %d", lowID) {
		t.Fatalf("expected the late-committing lower id %d to be delivered, got %#v", lowID, frame)
	}
}

// TestSSEColdStartSubscribesBeforeReadingHeadSoLateCommitIsNotLost proves the fix
// for the cold-start event-loss recurrence: the client used to make two separate
// requests (GET /events/head, then GET /events?since=<head>), so a transaction
// that grabbed a lower id before the head was read could commit in the gap
// between those two requests and never be delivered — invisible to a catch-up
// query (its id is <= since) and to the subscription (registered only by the
// second request, after the gap). A cold request (no since=, no Last-Event-ID)
// now subscribes to the broker before computing its own head inside this same
// handler, so a commit landing after Subscribe is still forwarded live
// regardless of its id — exactly like TestSSELiveEventBelowSinceIsNotDropped,
// but for the head the server resolves for itself instead of one the client
// supplies.
func TestSSEColdStartSubscribesBeforeReadingHeadSoLateCommitIsNotLost(t *testing.T) {
	handler, database, broker := newTestHandlerWithBroker(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)

	ctx := context.Background()
	// Holds a low id open, uncommitted — allocated before the cold request below
	// ever runs, so it stays lower than whatever head that request resolves.
	stalled, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin stalled transaction: %v", err)
	}
	defer stalled.Rollback(ctx)
	var lowID int64
	if err := stalled.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, 100, 'seeded', '{"kind":"session","id":"seed"}'::jsonb, '{}'::jsonb, false)
		returning id
	`, issue.Key).Scan(&lowID); err != nil {
		t.Fatalf("insert stalled event: %v", err)
	}

	// An independent insert grabs a higher id and commits immediately — this is
	// exactly the current head the cold request below will resolve for itself.
	var highID int64
	if err := database.Pool.QueryRow(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, 101, 'seeded', '{"kind":"session","id":"seed"}'::jsonb, '{}'::jsonb, false)
		returning id
	`, issue.Key).Scan(&highID); err != nil {
		t.Fatalf("insert committed event: %v", err)
	}
	if highID <= lowID {
		t.Fatalf("expected the committed insert to take a higher id than the stalled one: low=%d high=%d", lowID, highID)
	}
	broker.Publish(model.Event{
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		ID:       highID,
		IssueKey: issue.Key,
		Payload:  map[string]any{},
		Seq:      101,
		Type:     "seeded",
	})

	// A cold client opens the stream with no since= and no Last-Event-ID: it must
	// subscribe to the broker before resolving highID as its own head, or the
	// still-stalled lowID would land in the gap between those two steps. Under
	// the old two-request design this since=0 catch-up would have replayed highID
	// immediately as its first frame; the fixed cold path resolves its head as
	// highID internally and sends nothing until the live lowID commit below.
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatalf("construct SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("SSE status: got %d, want 200", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)

	// The stalled transaction finally commits and is published: a lower id than
	// the head this cold connection resolved for itself, arriving live after the
	// stream is already open. The old design lost this forever.
	if err := stalled.Commit(ctx); err != nil {
		t.Fatalf("commit stalled transaction: %v", err)
	}
	broker.Publish(model.Event{
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		ID:       lowID,
		IssueKey: issue.Key,
		Payload:  map[string]any{},
		Seq:      100,
		Type:     "seeded",
	})

	frame := readSSEFrame(t, scanner)
	if frame[0] != fmt.Sprintf("id: %d", lowID) {
		t.Fatalf("expected the late-committing lower id %d to be delivered as the cold connection's first frame, got %#v", lowID, frame)
	}
}
