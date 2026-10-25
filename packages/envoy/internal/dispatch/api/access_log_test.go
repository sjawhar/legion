package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// captureLog sends the default logger's lines to a buffer for the test's lifetime, as the
// deployment's own logger writes them: a text handler, the one cmd/dispatch installs.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logged
}

// accessLines are the access-log lines a buffer holds, in order.
func accessLines(t *testing.T, logged *bytes.Buffer) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, `msg="`+accesslog.Message+`"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// A person's read writes one line naming the route, the person and the answer, and no request
// after it writes a second line for the same request.
func TestAPersonsRequestWritesOneAccessLogLineNamingThem(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedTestProject(t, database)
	logged := captureLog(t)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=ACCESS", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues = %d: %s", response.Code, response.Body.String())
	}
	lines := accessLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("one request wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, want := range []string{
		`level=INFO`,
		`method=GET`,
		`route=/api/v1/issues`,
		`path=/api/v1/issues`,
		`caller_kind=user`,
		`caller=alice`,
		`status=200`,
		`elapsed_ms=`,
		`bytes=`,
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
}

// A bearer's write is named by the session it acts as, never by the token it sent, and the line
// holds neither the token nor the body the caller wrote.
func TestABearersRequestIsNamedByItsSessionAndCarriesNoToken(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedTestProject(t, database)
	logged := captureLog(t)

	body, err := json.Marshal(map[string]any{
		"project": "ACCESS",
		"title":   "An issue a session created",
		"actor":   map[string]string{"kind": "session", "id": "session-0123456789abcdef"},
	})
	if err != nil {
		t.Fatalf("encode issue: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/issues = %d: %s", response.Code, response.Body.String())
	}
	lines := accessLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("one request wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	line := lines[0]
	for _, want := range []string{`method=POST`, `route=/api/v1/issues`, `caller_kind=session`, `caller=session-0123456789abcdef`, `status=201`} {
		if !strings.Contains(line, want) {
			t.Errorf("access-log line %q does not carry %q", line, want)
		}
	}
	for _, forbidden := range []string{"agent-token", "Bearer", "Authorization", "An issue a session created"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("access-log line %q carries %q, which no line may hold", line, forbidden)
		}
	}
}

// A request no credential authenticates is logged with its refusal and no caller, so a flood of
// refused requests is visible too.
func TestARefusedRequestIsLoggedWithNoCaller(t *testing.T) {
	handler := newTestHandler(t)
	logged := captureLog(t)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/issues", nil)
	request.Header.Set("Authorization", "Bearer not-a-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/issues with an unknown bearer = %d, want 401", response.Code)
	}
	lines := accessLines(t, logged)
	if len(lines) != 1 {
		t.Fatalf("one request wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, want := range []string{`caller_kind=anonymous`, `status=401`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("access-log line %q does not carry %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "not-a-token") {
		t.Fatalf("access-log line %q carries the refused bearer", lines[0])
	}
}

func seedTestProject(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `insert into projects (key, name) values ('ACCESS', 'Access')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}
