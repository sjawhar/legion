package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// sharedAgentTokens is a DISPATCH_AGENT_TOKEN setting parsed as the server parses it at boot.
func sharedAgentTokens(t testing.TB, setting string) *auth.SharedAgentTokens {
	t.Helper()
	tokens, err := auth.ParseSharedAgentTokens(setting)
	if err != nil {
		t.Fatalf("ParseSharedAgentTokens(%q): %v", setting, err)
	}
	return tokens
}

// testServerOptions configure the server an API test drives.
type testServerOptions struct {
	defaultProject string
	testHooks      bool
	// app and githubAPIBase wire the GitHub App access-check client at a fake
	// GitHub; both nil/empty leaves the client in its "no app credentials"
	// state (every check answers ErrNoAppKey).
	app           *auth.AppConfig
	githubAPIBase string
	// settle is the document closer delay. The default keeps settlement-dependent
	// tests fast; a test that holds an issue row lock across the seeded spec's
	// settlement passes a delay that cannot fire before it finishes, because that
	// settlement locks the same row and would otherwise queue behind the test.
	settle time.Duration
	// oidc is the service-account token verifier; nil is the unconfigured
	// deployment, whose bearer handling is untouched by that feature.
	oidc *oidc.Verifier
	// envoyURL points the Envoy listener client at a fake listener; empty leaves the client
	// unconfigured, which is every test that never asks whether a session is live.
	envoyURL string
	// envoyTimeout shortens that client's window, so a test can exercise a receipt timeout
	// without holding a stand-in listener for the production five seconds.
	envoyTimeout time.Duration
	// persistence is an optional document-store seam for API handlers that need the document
	// service to observe a persistence boundary condition.
	persistence func(*store.Store) docs.VersionedStore
	// agentStream is the live agent conversation relay; nil is the deployment with no NATS,
	// where the viewer route answers 503.
	agentStream agentstream.Source
	// embedder wires meaning search (LEGION-549); nil (every test but the meaning search tests'
	// own) is the deployment with no embedder configured, where search answers keyword-only.
	embedder embed.Embedder
	// files is the uploaded-file store; nil keeps every upload's bytes in Postgres, as a
	// deployment with no bucket does.
	files files.Store
	// lifetime is the server's Deps.Lifetime, on which Register builds the delivery hub GET
	// /ws/delivery serves and the event stream relay every SSE connection reads. Nil is the
	// test's own context (t.Context()), which ends before the cleanups that wait those
	// goroutines out; a test that stops the server before it ends (the socket's) passes one it
	// can cancel itself. It must end by the test's end, so derive it from t.Context().
	lifetime context.Context
	// streamHeartbeat replaces the event streams' and the sockets' fifteen-second heartbeat.
	streamHeartbeat time.Duration
}

// testSigningKey is the one-key set the package's session cookies are signed with.
var testSigningKey = auth.SingleSigningKey("signing-key")

// headerIdentity is the test header identity that records each named person in the database.
func headerIdentity(database *store.Store) identity.HeaderIdentity {
	return identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(database.Pool, testSigningKey, nil)}
}

// seedPeople records emails as people who have signed in: the assignee picker's options.
func seedPeople(t testing.TB, database *store.Store, emails ...string) {
	t.Helper()
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	for _, email := range emails {
		if err := people.Record(context.Background(), email); err != nil {
			t.Fatalf("seed person %q: %v", email, err)
		}
	}
}
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, _, _ := newTestServer(t, testServerOptions{})
	return handler
}

func newTestHandlerWithStore(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{})
	return handler, database
}

func newTestHandlerWithDefaultProject(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{defaultProject: "DEFAULT"})
	return handler, database
}

func newTestHandlerWithTestHooks(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{testHooks: true})
	return handler, database
}

// newTestServer builds the mux an API test drives and hands back the store behind it and the
// dependencies it runs on, so a test that also calls a handler or a delivery step directly
// (directServer) shares this server's broker and document service instead of standing up a
// second pair.
func newTestServer(t testing.TB, options testServerOptions) (http.Handler, *store.Store, Deps) {
	t.Helper()
	handler, database, deps, _ := newRegisteredTestServer(t, options)
	return handler, database, deps
}

// newRegisteredTestServer is newTestServer, answering also the server Register mounted, whose
// delivery hub and open sockets a socket test reads.
func newRegisteredTestServer(t testing.TB, options testServerOptions) (http.Handler, *store.Store, Deps, *server) {
	t.Helper()
	settle := options.settle
	if settle == 0 {
		settle = 20 * time.Millisecond
	}
	lifetime := options.lifetime
	if lifetime == nil {
		lifetime = t.Context()
	}
	database := storetest.Open(t)
	broker := events.NewBroker()
	var documentPersistence docs.VersionedStore
	if options.persistence != nil {
		documentPersistence = options.persistence(database)
	}
	documentService := docs.New(docs.Deps{Store: database, Persistence: documentPersistence, Events: broker, ServerURL: "https://dispatch.example", Settle: settle})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedPeople(t, database, "alice", "bob")
	deps, err := NewDeps(DepsInput{
		Store:            database,
		Identity:         headerIdentity(database),
		AgentTokens:      sharedAgentTokens(t, "agent-token"),
		DefaultProject:   options.defaultProject,
		ServerURL:        "https://dispatch.example",
		Docs:             documentService,
		Events:           broker,
		App:              options.app,
		GitHubAPIBase:    options.githubAPIBase,
		TestHooksEnabled: options.testHooks,
		Lifetime:         lifetime,
		StreamHeartbeat:  options.streamHeartbeat,
		EnvoyURL:         options.envoyURL,
		EnvoyTimeout:     options.envoyTimeout,
		OIDC:             options.oidc,
		AgentStream:      options.agentStream,
		Embedder:         options.embedder,
		Files:            options.files,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	served := register(mux, deps)
	t.Cleanup(served.eventRelay.wait)
	if served.deliveryHub != nil {
		t.Cleanup(served.deliveryHub.wait)
		t.Cleanup(served.deliverySockets.wait)
	}
	return mux, database, deps, served
}

// directServer is a server a test calls a handler or a delivery step on directly, without the
// mux: it runs on the dependencies of the server under test - the same store, listener, broker
// and document service - so what it writes and what it reads are the rows that handler left.
func directServer(deps Deps) *server {
	return &server{deps: deps}
}

// writeSessionRecord answers GET /v1/registry/{session_id} from the rows a stub listener also
// serves as its live list, so one literal gives both answers and the two agree the way a real
// listener's do. Dispatch resolves a session through this route rather than through the list,
// so a stub that served only the list would answer every lookup "no live session"; an id no row
// names is the listener's own 404, the one absence a caller may act on.
func writeSessionRecord(t *testing.T, w http.ResponseWriter, sessionsJSON, sessionID string) {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(sessionsJSON), &rows); err != nil {
		t.Errorf("decode the stub listener's sessions: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	for _, row := range rows {
		if row["session_id"] == sessionID {
			_ = json.NewEncoder(w).Encode(row)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"no live session ` + sessionID + `"}`))
}

// waitForDatabaseLocks waits up to 2 s for want backends to queue behind a lock that holder's
// transaction owns (storetest.WaitForLockWaiters), and fails the test when they never do.
func waitForDatabaseLocks(t *testing.T, holder pgx.Tx, want int) {
	t.Helper()
	if queued := storetest.WaitForLockWaiters(t, holder, want, 2*time.Second); queued < want {
		t.Fatalf("waiting database locks: wanted at least %d operations queued behind the held lock, saw %d", want, queued)
	}
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
	if strings.HasSuffix(target, "/answer") {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode answer request body: %v", err)
		}
		input := map[string]any{}
		if string(data) != "null" {
			if err := json.Unmarshal(data, &input); err != nil {
				t.Fatalf("decode answer request body: %v", err)
			}
		}
		if _, provided := input["expected_edited_at"]; !provided {
			input["expected_edited_at"] = nil
		}
		body = input
	}
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
	body, bodyType, err := apitest.MultipartUpload(fields, filename, contentType, content)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, body)
	request.Header.Set("Content-Type", bodyType)
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// databaseFingerprint hashes every row of every table, so a test can tell a request wrote nothing
// anywhere: no row inserted, updated or deleted.
func databaseFingerprint(t *testing.T, database *store.Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `select tablename from pg_tables where schemaname = 'public'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read table names: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("the schema has no tables; a fingerprint of it proves nothing")
	}
	fingerprint := make(map[string]string, len(tables))
	for _, table := range tables {
		var digest string
		if err := database.Pool.QueryRow(ctx, fmt.Sprintf(
			`select count(*) || ':' || coalesce(md5(string_agg(t::text, ',' order by t::text)), '') from %s t`,
			pgx.Identifier{table}.Sanitize(),
		)).Scan(&digest); err != nil {
			t.Fatalf("fingerprint %s: %v", table, err)
		}
		fingerprint[table] = digest
	}
	return fingerprint
}

// assertUnchanged fails for each table whose rows differ between two fingerprints of one database.
func assertUnchanged(t *testing.T, request string, before, after map[string]string) {
	t.Helper()
	for _, table := range slices.Sorted(maps.Keys(after)) {
		if before[table] != after[table] {
			t.Errorf("%s changed table %s", request, table)
		}
	}
}

// assertRefusal checks an answer is 400 with code and an error naming field first.
func assertRefusal(t *testing.T, request string, status int, body []byte, code, field string) {
	t.Helper()
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &refusal)
	if status != http.StatusBadRequest || refusal.Code != code || !strings.HasPrefix(refusal.Error, field+" holds a ") {
		t.Errorf("%s = %d %s %q, want 400 %s naming %s", request, status, refusal.Code, refusal.Error, code, field)
	}
}

func decodeBody[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	return value
}

// overCapCases are texts just past a cap of limit UTF-16 units, the one-character-over case and
// one counted in UTF-16 units, each with the length the refusal names.
func overCapCases(limit int) map[string]struct {
	text   string
	length int
} {
	return map[string]struct {
		text   string
		length int
	}{
		"one character over": {strings.Repeat("a", limit+1), limit + 1},
		// 501 characters outside the Basic Multilingual Plane are 1,002 UTF-16 units.
		"counted in UTF-16 units": {strings.Repeat("😀", limit/2+1), limit + 2},
	}
}

// assertCapExceeded requires response to be the 400 CAP_EXCEEDED capExceededError writes for a
// field of length units past a cap of limit, followed by "; hint" when hint is not empty.
func assertCapExceeded(t *testing.T, request string, response *httptest.ResponseRecorder, field string, length, limit int, hint string) {
	t.Helper()
	want := fmt.Sprintf("%s is %d characters over the %d-character limit (%d/%d)", field, length-limit, limit, length, limit)
	if hint != "" {
		want += "; " + hint
	}
	body := response.Body.String()
	refusal := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusBadRequest || refusal.Code != "CAP_EXCEEDED" || refusal.Error != want {
		t.Fatalf("%s: status=%d body=%.300s, want 400 CAP_EXCEEDED %q", request, response.Code, body, want)
	}
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
	if text.Markdown != "# Hello\n" || text.Version == nil || *text.Version != 1 {
		t.Fatalf("primary document: got %#v, want markdown # Hello at version 1", text)
	}
}

func TestCreateIssueWithMissingOrBlankSpecGetsAnEmptyPrimaryDocument(t *testing.T) {
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
			input := map[string]any{"project": "TEST", "title": "Seeded issue", "force": true}
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
				Version  *int   `json:"version"`
			}](t, textResponse)
			if text.Markdown != "" || text.Version == nil || *text.Version != 1 {
				t.Fatalf("primary document = %#v, want empty markdown at version 1", text)
			}
		})
	}
}

func TestCreateIssueRejectsSpecOutsideProofSchema(t *testing.T) {
	handler := newTestHandler(t)
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Invalid spec", "spec": "<details>x</details>",
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_MARKDOWN"`) {
		t.Fatalf("create invalid issue spec: status=%d body=%s", response.Code, response.Body.String())
	}
}
func TestDocumentTextReportsUnavailableService(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store:       database,
		Persistence: failingLoadStore{VersionedStore: docs.NewPgVersioned(database)},
		Events:      broker,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store:    database,
		Identity: headerIdentity(database),
		Docs:     documentService,
		Events:   broker,
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

	blocks := dispatchRequest(t, mux, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice")
	if blocks.Code != http.StatusServiceUnavailable || !strings.Contains(blocks.Body.String(), `"code":"DOC_SERVICE_UNAVAILABLE"`) {
		t.Fatalf("unavailable document blocks: status=%d body=%s", blocks.Code, blocks.Body.String())
	}
}

// failingLoadStore is a store that cannot serve any read of a document: a room's load, a cold
// read's stamp, or its load.
type failingLoadStore struct {
	docs.VersionedStore
}

func (failingLoadStore) Load(context.Context, string) (persistence.LoadResult, error) {
	return persistence.LoadResult{}, errors.New("persistence unavailable")
}

func (failingLoadStore) DocumentStamp(context.Context, string) (docs.DocumentStamp, error) {
	return docs.DocumentStamp{}, errors.New("persistence unavailable")
}

func (failingLoadStore) LoadDocument(context.Context, string) (docs.LoadedDocument, error) {
	return docs.LoadedDocument{}, errors.New("persistence unavailable")
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
		{name: "child added", event: model.Event{Type: "child.added", Actor: session}, want: true},
		{name: "child removed", event: model.Event{Type: "child.removed", Actor: session}, want: true},
		{name: "project creation", event: model.Event{Type: "project.created", Actor: user}, want: false},
		{name: "repository project mapping", event: model.Event{Type: "settings.repo_project.updated", Actor: user}, want: false},
		{name: "architecture source", event: model.Event{Type: "settings.architecture_source.updated", Actor: user}, want: false},
		{name: "project default assignee", event: model.Event{Type: "settings.project_default_assignee.updated", Actor: user}, want: false},
		{name: "user state", event: model.Event{Type: "user_state.updated", Actor: user}, want: false},
		{name: "user thread state", event: model.Event{Type: "user_thread_state.updated", Actor: user}, want: false},
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
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": "ordinary issue", "force": true,
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

func TestExternalIssueReferenceResolvesBothGitHubURLShapes(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string) model.Issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": title, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	link := func(key, url string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"external_links": []map[string]string{{"url": url}},
		}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("link %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	issueLink := create("GitHub issue link")
	pullLink := create("GitHub pull request link")
	link(issueLink.Key, "https://github.com/owner/repo/issues/42")

	resolvedIssue := dispatchRequest(
		t,
		handler,
		http.MethodGet,
		"/api/v1/issues/resolve?ref=owner/repo%2342",
		nil,
		"alice",
	)
	if resolvedIssue.Code != http.StatusOK {
		t.Fatalf("resolve GitHub issue link: status=%d body=%s", resolvedIssue.Code, resolvedIssue.Body.String())
	}
	if got := decodeBody[struct {
		Key string `json:"key"`
	}](t, resolvedIssue).Key; got != issueLink.Key {
		t.Fatalf("resolved issue key = %q, want %q", got, issueLink.Key)
	}

	link(pullLink.Key, "https://github.com/owner/repo/pull/42")
	resolvedPull := dispatchRequest(
		t,
		handler,
		http.MethodGet,
		"/api/v1/issues/resolve?ref=owner/repo%2342",
		nil,
		"alice",
	)
	if resolvedPull.Code != http.StatusBadRequest {
		t.Fatalf("resolve ambiguous GitHub links: status=%d body=%s", resolvedPull.Code, resolvedPull.Body.String())
	}
	body := resolvedPull.Body.String()
	if !strings.Contains(body, `"code":"AMBIGUOUS_ISSUE_REF"`) ||
		!strings.Contains(body, issueLink.Key) ||
		!strings.Contains(body, pullLink.Key) {
		t.Fatalf("ambiguous response = %s, want AMBIGUOUS_ISSUE_REF naming %s and %s", body, issueLink.Key, pullLink.Key)
	}
}

func TestExternalIssueCreationReusesPullRequestLink(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map repository: status=%d body=%s", response.Code, response.Body.String())
	}
	existing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Linked pull request",
	}, "alice")
	if existing.Code != http.StatusCreated {
		t.Fatalf("create existing issue: status=%d body=%s", existing.Code, existing.Body.String())
	}
	issue := decodeBody[model.Issue](t, existing)
	if linked := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"external_links": []map[string]string{{
			"url": "https://github.com/owner/repo/pull/42",
		}},
	}, "alice"); linked.Code != http.StatusOK {
		t.Fatalf("link pull request: status=%d body=%s", linked.Code, linked.Body.String())
	}

	reused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#42",
	}, "alice")
	if reused.Code != http.StatusOK {
		t.Fatalf("reuse pull-request link: status=%d body=%s", reused.Code, reused.Body.String())
	}
	if got := decodeBody[model.Issue](t, reused).Key; got != issue.Key {
		t.Fatalf("reused issue key = %q, want %q", got, issue.Key)
	}
	var issueCount int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from issues`).Scan(&issueCount); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if issueCount != 1 {
		t.Fatalf("issue count = %d, want 1", issueCount)
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
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Revised\n"`) {
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
			IssueKey: new(issue.Key),
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
	projectReplay := readSSEFrame(t, scanner)
	if projectReplay[1] != "event: project.created" {
		t.Fatalf("project replayed SSE frame: %#v", projectReplay)
	}
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
	resumeRequest.Header.Set("Last-Event-ID", "2")
	resumed, err := http.DefaultClient.Do(resumeRequest)
	if err != nil {
		t.Fatalf("resume SSE: %v", err)
	}
	defer resumed.Body.Close()
	resumedFrame := readSSEFrame(t, bufio.NewScanner(resumed.Body))
	if resumedFrame[0] != "id: 3" || resumedFrame[1] != "event: issue.updated" {
		t.Fatalf("Last-Event-ID replay: %#v", resumedFrame)
	}
	staleQueryRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/events?since=0", nil)
	if err != nil {
		t.Fatalf("construct stale-query SSE request: %v", err)
	}
	staleQueryRequest.Header.Set("Authorization", "Bearer agent-token")
	staleQueryRequest.Header.Set("Last-Event-ID", "2")
	staleQuery, err := http.DefaultClient.Do(staleQueryRequest)
	if err != nil {
		t.Fatalf("resume stale-query SSE: %v", err)
	}
	defer staleQuery.Body.Close()
	staleQueryFrame := readSSEFrame(t, bufio.NewScanner(staleQuery.Body))
	if staleQueryFrame[0] != "id: 3" || staleQueryFrame[1] != "event: issue.updated" {
		t.Fatalf("Last-Event-ID with stale since query replay: %#v", staleQueryFrame)
	}
}

// readSSEFrame reads the next frame that carries an event, the way an SSE client reads the
// stream: a comment line (one starting with ":") is ignored, so a frame made only of comments --
// the ": open" every stream starts with, and its heartbeats -- dispatches nothing and is skipped.
func readSSEFrame(t *testing.T, scanner *bufio.Scanner) []string {
	t.Helper()
	frame := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(frame) > 0 {
				return frame
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
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
	if !strings.Contains(alice.Body.String(), `"TEST-1":{"pinned":true,"last_read_seq":1,"dismissed":["ask-1"],"seq":0}`) {
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

func TestUserIssueStateSequenceRejectsStaleWrites(t *testing.T) {
	handler := newTestHandler(t)
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

	legacy := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"dismissed": []string{"legacy"},
	}, "alice")
	if legacy.Code != http.StatusOK {
		t.Fatalf("legacy state write: status=%d body=%s", legacy.Code, legacy.Body.String())
	}

	sequenced := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"dismissed": []string{"first"}, "seq": 1,
	}, "alice")
	if sequenced.Code != http.StatusOK {
		t.Fatalf("first sequenced state write: status=%d body=%s", sequenced.Code, sequenced.Body.String())
	}

	stale := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"dismissed": []string{"stale"}, "seq": 1,
	}, "alice")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale state write: status=%d body=%s", stale.Code, stale.Body.String())
	}
	conflict := decodeBody[struct {
		Code  string `json:"code"`
		State struct {
			Dismissed []string `json:"dismissed"`
			Seq       int64    `json:"seq"`
		} `json:"state"`
	}](t, stale)
	if conflict.Code != "STATE_STALE" || len(conflict.State.Dismissed) != 1 || conflict.State.Dismissed[0] != "first" || conflict.State.Seq != 1 {
		t.Fatalf("stale state conflict = %#v", conflict)
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
	// The seeded specs' settlement would lock the child row this test holds; keep it
	// out of the lock queue so only the two status patches are counted as waiters.
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
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
	waitForDatabaseLocks(t, lock, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
			"status": "done",
		}, "alice")
	}()
	waitForDatabaseLocks(t, lock, 2)
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
	waitForDatabaseLocks(t, lock, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
			"last_read_seq": 7,
		}, "alice")
	}()
	waitForDatabaseLocks(t, lock, 2)
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

// cookieRequest sends one request to a newCookieServer handler as alice, with body as its JSON.
type cookieRequest func(method, target string, body any) *httptest.ResponseRecorder

// newCookieServer is a server whose people sign in with a session cookie, beside the shared agent
// bearer: the handler, the store's session table, which a test revokes, alice@d.example's cookie,
// and a request sent with it.
func newCookieServer(t *testing.T) (http.Handler, *store.PgSessionStore, *http.Cookie, cookieRequest) {
	t.Helper()
	database := storetest.Open(t)
	sessions := store.NewPgSessionStore(database.Pool)
	cookieIdentity := identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions}
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, Identity: cookieIdentity, Settle: time.Hour,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: cookieIdentity, AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs: documentService, Events: broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	handler := http.NewServeMux()
	Register(handler, deps)
	generation, err := sessions.EnsureSession(context.Background(), "alice@d.example")
	if err != nil {
		t.Fatalf("establish alice's session: %v", err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("alice@d.example", generation, testSigningKey, true))
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
	return handler, sessions, cookie, request
}

func TestRevokedCookieIsRejectedAcrossDispatchSurfaces(t *testing.T) {
	handler, sessions, cookie, request := newCookieServer(t)
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

	if err := sessions.RevokeSessions(context.Background(), "alice@d.example"); err != nil {
		t.Fatalf("revoke alice's sessions: %v", err)
	}
	for _, target := range []string{"/api/v1/issues/" + issue.Key, "/api/v1/events"} {
		response := request(http.MethodGet, target, nil)
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"NO_IDENTITY"`) {
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
	scanner := bufio.NewScanner(stream.Body)
	// The 1005 seeded events plus the issue creation exceed one capped page
	// (maxSSEReplay=1000); a single connection pages through all of them without
	// ever disconnecting: a capped page that ended the stream and forced a client
	// reconnect would leave a gap where a low id committing between "read this page"
	// and "a new connection subscribes" could be lost forever.
	for wantID := 2; wantID <= 1007; wantID++ {
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

func TestQuiesceDocumentsHookIsGatedByTestHooksEnabled(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/_test/quiesce", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Fatalf("quiesce hook without TestHooksEnabled: status=%d, want %d", response.Code, http.StatusNotFound)
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
	projectReplay := readSSEFrame(t, scanner)
	if projectReplay[1] != "event: project.created" {
		t.Fatalf("project replayed SSE frame: %#v", projectReplay)
	}
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
	resumeRequest.Header.Set("Last-Event-ID", "2")
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
	if resumedFrame[0] != "id: 3" || resumedFrame[1] != "event: issue.updated" {
		t.Fatalf("resumed replay after disconnect-all: %#v", resumedFrame)
	}
}

func newTestHandlerWithBroker(t *testing.T) (http.Handler, *store.Store, *events.Broker) {
	t.Helper()
	database := storetest.Open(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: database, Events: broker, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedPeople(t, database, "alice", "bob")
	deps, err := NewDeps(DepsInput{
		Store:       database,
		Identity:    headerIdentity(database),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL:   "https://dispatch.example",
		Docs:        documentService,
		Events:      broker,
		// The event stream relay runs on this, and the wait below is what keeps its listening
		// connection from outliving the database the test drops.
		Lifetime: t.Context(),
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, deps))

	return mux, database, broker
}
func TestSSEResumeCursorOrdersConcurrentEventCommits(t *testing.T) {
	handler, database, broker := newTestHandlerWithBroker(t)
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
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "OTHER", "name": "Other project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create other project: status=%d body=%s", response.Code, response.Body.String())
	}
	otherCreated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "OTHER", "title": "Other issue",
	}, "alice")
	other := decodeBody[struct {
		Key string `json:"key"`
	}](t, otherCreated)
	if otherCreated.Code != http.StatusCreated {
		t.Fatalf("create other issue: status=%d body=%s", otherCreated.Code, otherCreated.Body.String())
	}

	ctx := context.Background()
	first, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first transaction: %v", err)
	}
	defer first.Rollback(ctx)
	low, err := broker.Append(ctx, first, model.Event{
		IssueKey: new(issue.Key),
		Type:     "test.concurrent",
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		Payload:  map[string]any{},
	})
	if err != nil {
		t.Fatalf("append first event: %v", err)
	}

	type appendResult struct {
		event model.Event
		err   error
	}
	started := make(chan struct{})
	secondResult := make(chan appendResult, 1)
	go func() {
		second, err := database.Pool.Begin(ctx)
		if err != nil {
			secondResult <- appendResult{err: err}
			return
		}
		defer second.Rollback(ctx)
		close(started)
		event, err := broker.Append(ctx, second, model.Event{
			IssueKey: new(other.Key),
			Type:     "test.concurrent",
			Actor:    model.Actor{Kind: "session", ID: "seed"},
			Payload:  map[string]any{},
		})
		if err == nil {
			err = second.Commit(ctx)
		}
		secondResult <- appendResult{event: event, err: err}
	}()
	<-started
	waitForDatabaseLocks(t, first, 1)

	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit first event: %v", err)
	}
	high := <-secondResult
	if high.err != nil {
		t.Fatalf("append second event: %v", high.err)
	}
	if high.event.ID <= low.ID {
		t.Fatalf("event ids did not follow commit order: first=%d second=%d", low.ID, high.event.ID)
	}

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	request, err := http.NewRequest(
		http.MethodGet,
		httpServer.URL+"/api/v1/events?since="+fmt.Sprintf("%d", low.ID),
		nil,
	)
	if err != nil {
		t.Fatalf("construct resumed SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open resumed SSE stream: %v", err)
	}
	defer stream.Body.Close()
	frame := readSSEFrame(t, bufio.NewScanner(stream.Body))
	if frame[0] != fmt.Sprintf("id: %d", high.event.ID) {
		t.Fatalf("resume replay = %#v, want committed second event %d", frame, high.event.ID)
	}
}

// eventHead is the newest committed event id: what a connection resuming now would pass as since.
func eventHead(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var head int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&head); err != nil {
		t.Fatalf("read the event log's head: %v", err)
	}
	return head
}

// sseStreamDeadline bounds a stream a test opens: a frame that never arrives fails the read with
// the context's error rather than holding the test until the package's own timeout, which would
// take every other test's result with it.
const sseStreamDeadline = 30 * time.Second

// openSSEStream opens one event stream as an agent bearer, closed when the test ends, and answers
// a scanner over its frames. It returns once the handler has answered 200, which it does only
// after it has subscribed and resolved its since. Its caller closes the httptest server with
// t.Cleanup rather than defer: Close waits for the handlers still running, and this body closes
// with the cleanups, after every defer.
func openSSEStream(t *testing.T, url string) *bufio.Scanner {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sseStreamDeadline)
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("construct SSE request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer agent-token")
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	t.Cleanup(func() { stream.Body.Close() })
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", stream.StatusCode)
	}
	return bufio.NewScanner(stream.Body)
}

// sseFrameID is the event id of one frame readSSEFrame read.
func sseFrameID(t *testing.T, frame []string) int64 {
	t.Helper()
	if len(frame) == 0 || !strings.HasPrefix(frame[0], "id: ") {
		t.Fatalf("SSE frame = %#v, want an id line", frame)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(frame[0], "id: "), 10, 64)
	if err != nil {
		t.Fatalf("SSE frame id %q: %v", frame[0], err)
	}
	return id
}

// TestSSEDoesNotResendAnEventAtOrBelowItsSince covers the one id comparison the live loop makes.
// The relay is one per process and can stand behind a connection opened after it: it then
// publishes ids that connection's catch-up has already covered, which sentDuringCatchup cannot be
// asked about, since it holds only the ids above since that catch-up sent. Forwarding them would
// show a client an event it has already seen.
//
// The comparison is safe because since is fixed for the connection, and the commit-order lock
// events.Broker.Append holds until its transaction commits leaves no uncommitted id at or below a
// committed max(id). A late-committing lower id -- what the live loop used to carry a rule
// against -- is now the lock's to prevent and TestOnlyTheBrokerInsertsEvents's to keep.
func TestSSEDoesNotResendAnEventAtOrBelowItsSince(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	// Long enough that the second connection below opens between a commit and the read that
	// commit's notification wakes, so the relay really is behind that connection's since.
	served.eventRelay.coalesce = time.Second
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	// The first connection starts the relay, whose cursor stops at what has committed by now.
	openSSEStream(t, httpServer.URL+"/api/v1/events")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename issue: status=%d body=%s", response.Code, response.Body.String())
	}
	since := eventHead(t, database)

	// This connection's catch-up covers everything through since, which the relay has not read
	// up to yet: what the relay publishes next is this connection's own backlog.
	scanner := openSSEStream(t, fmt.Sprintf("%s/api/v1/events?since=%d", httpServer.URL, since))
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed again",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename issue again: status=%d body=%s", response.Code, response.Body.String())
	}
	if id := sseFrameID(t, readSSEFrame(t, scanner)); id <= since {
		t.Fatalf("live frame id = %d, want an id above the connection's since %d", id, since)
	}
}

// TestSSECappedCatchupDeliversACommitDuringPagingExactlyOnce: a catch-up that fills a page keeps
// paging on the subscription it opened with. An event committing while it pages reaches the
// client on a later page or live from the relay, whichever sees it first, and sentDuringCatchup is
// what keeps it from arriving on both. A capped page that ended the stream instead, sending the
// client to a reconnect, would leave a gap between the last page and the new subscription for that
// commit to fall into.
func TestSSECappedCatchupDeliversACommitDuringPagingExactlyOnce(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	// Exactly maxSSEReplay committed events, so the first catch-up page fills and a second runs.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		select $1, value, 'seeded', jsonb_build_object('kind', 'session', 'id', 'seed'), jsonb_build_object('seq', value), false
		from generate_series(101, 1100) as value
	`, issue.Key); err != nil {
		t.Fatalf("seed capped backlog: %v", err)
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	scanner := openSSEStream(t, httpServer.URL+"/api/v1/events?since=2")

	// Committed while that catch-up is still paging.
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename issue: status=%d body=%s", response.Code, response.Body.String())
	}
	live := eventHead(t, database)
	seen := map[int64]bool{}
	for id := int64(0); id != live; {
		id = sseFrameID(t, readSSEFrame(t, scanner))
		if seen[id] {
			t.Fatalf("event %d was delivered twice", id)
		}
		seen[id] = true
		if len(seen) > maxSSEReplay+100 {
			t.Fatalf("the commit %d made during the catch-up was not delivered in %d frames", live, len(seen))
		}
	}
}

// TestSSEColdStartSendsNoBacklogAndDeliversTheNextCommit: a cold connection (no since= and no
// Last-Event-ID) resolves its own head inside this one handler, after it has subscribed. It is
// sent nothing already committed, which is the backlog it asked not to get, and everything
// committing after it, which lands in its subscription however its catch-up went. A client that
// read its head through a separate request first would lose an event committing between that
// request and this one: invisible to the catch-up (id <= since) and to a subscription registered
// only afterwards.
func TestSSEColdStartSendsNoBacklogAndDeliversTheNextCommit(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	backlog := eventHead(t, database)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	scanner := openSSEStream(t, httpServer.URL+"/api/v1/events")

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename issue: status=%d body=%s", response.Code, response.Body.String())
	}
	live := eventHead(t, database)
	for id := int64(0); id != live; {
		id = sseFrameID(t, readSSEFrame(t, scanner))
		if id <= backlog {
			t.Fatalf("cold connection was sent event %d, backlog at or below the head %d it opened on", id, backlog)
		}
	}
}

// TestSSEResumesAfterTheEventLogIsResetBeneathTheReader: the browser suite empties the event log
// between its scenarios (`truncate ... restart identity cascade`, e2e/seed.ts), so the next row's
// id is 1 again. The relay notices, ends every open subscription and sends the browser through
// its reconnect -- and a browser only ever raises its lastEventId, never lowers it
// (web/src/api/sse.ts), so it comes back asking for a since above the new log's head. Without the
// handler's clamp, catch-up (`id > since`) matches nothing and the live loop drops every event of
// the new log for ever, because they all sit at or below that stale since: a reader silent for
// good, on a connection that stays open and heartbeats, with nothing logged and nothing failed.
//
// The assertion is the frame of an event committed after the reset, whose id is below the
// pre-reset head this connection resumed from. It can reach the reader only through a since the
// handler clamped to the head it resolved for this connection; an unclamped since drops it, and
// the only frame this reader then ever sees is a heartbeat.
func TestSSEResumesAfterTheEventLogIsResetBeneathTheReader(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed before the reset",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename issue before the reset: status=%d body=%s", response.Code, response.Body.String())
	}
	head := eventHead(t, database)
	if _, err := database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	// What the browser reconnects with after the relay ended its subscription: the last id it saw,
	// which the reset has left above the whole new log.
	scanner := openSSEStream(t, fmt.Sprintf("%s/api/v1/events?since=%d", httpServer.URL, head))

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"title": "Renamed after the reset",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("rename issue after the reset: status=%d body=%s", response.Code, response.Body.String())
	}
	live := eventHead(t, database)
	if live >= head {
		t.Fatalf("event %d committed after the reset is not below the pre-reset head %d: the reset reused no id, so this test proves nothing", live, head)
	}
	for id := int64(0); id != live; {
		id = sseFrameID(t, readSSEFrame(t, scanner))
		if id > live {
			t.Fatalf("frame id = %d, above the %d committed after the reset", id, live)
		}
	}
}

// TestSSEStreamOpensAtOnceForABrowserWaitingOnItsFirstByte: a browser can hold its fetch of the
// event stream unresolved until a body byte arrives (Firefox does), and so holds its reconnect
// unopened, with the refresh it owes every query on open not yet run. A connection with no
// backlog to replay and nothing new committing would send it nothing until the first heartbeat,
// so the stream writes ": open" with its headers: the first body bytes arrive at once, cold or
// resuming at the head, however long the heartbeat.
func TestSSEStreamOpensAtOnceForABrowserWaitingOnItsFirstByte(t *testing.T) {
	const heartbeat = time.Minute
	const opened = ": open\n\n"
	handler, database, _ := newTestServer(t, testServerOptions{streamHeartbeat: heartbeat})
	createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	head := eventHead(t, database)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	for _, connection := range []struct {
		name   string
		resume func(*http.Request)
	}{
		{name: "cold", resume: func(*http.Request) {}},
		{name: "resuming at the head by since", resume: func(request *http.Request) {
			request.URL.RawQuery = fmt.Sprintf("since=%d", head)
		}},
		{name: "resuming at the head by Last-Event-ID", resume: func(request *http.Request) {
			request.Header.Set("Last-Event-ID", strconv.FormatInt(head, 10))
		}},
	} {
		t.Run(connection.name, func(t *testing.T) {
			// The deadline covers the request and the read of its first body bytes alike.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/events", nil)
			if err != nil {
				t.Fatalf("construct SSE request: %v", err)
			}
			request.Header.Set("Authorization", "Bearer agent-token")
			connection.resume(request)
			started := time.Now()
			stream, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("open SSE stream: %v", err)
			}
			defer stream.Body.Close()
			if stream.StatusCode != http.StatusOK {
				t.Fatalf("SSE status = %d, want 200", stream.StatusCode)
			}
			first := make([]byte, len(opened))
			read, err := io.ReadFull(stream.Body, first)
			if err != nil || string(first) != opened {
				t.Fatalf("first body bytes = %q after %s (%v), want %q long before the %s heartbeat",
					first[:read], time.Since(started).Round(time.Millisecond), err, opened, heartbeat)
			}
		})
	}
}

func TestJSONMutationsRequireApplicationJSON(t *testing.T) {
	handler := newTestHandler(t)
	body := `{"key":"TEST","name":"Test project"}`

	plain := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	plain.Header.Set("X-Dispatch-User", "alice")
	plain.Header.Set("Content-Type", "text/plain")
	plainResponse := httptest.NewRecorder()
	handler.ServeHTTP(plainResponse, plain)
	if plainResponse.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain mutation status = %d body=%s, want %d", plainResponse.Code, plainResponse.Body.String(), http.StatusUnsupportedMediaType)
	}

	jsonRequest := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	jsonRequest.Header.Set("X-Dispatch-User", "alice")
	jsonRequest.Header.Set("Content-Type", "application/json")
	jsonResponse := httptest.NewRecorder()
	handler.ServeHTTP(jsonResponse, jsonRequest)
	if jsonResponse.Code != http.StatusCreated {
		t.Fatalf("application/json mutation status = %d body=%s, want %d", jsonResponse.Code, jsonResponse.Body.String(), http.StatusCreated)
	}
}

func TestUnauthenticatedLargeIssueBodyIsRejectedBeforeDecoding(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues", strings.NewReader(strings.Repeat(" ", 8<<20)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated large body status = %d body=%s, want %d", response.Code, response.Body.String(), http.StatusUnauthorized)
	}
}
