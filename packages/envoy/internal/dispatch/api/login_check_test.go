package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// pressedAs sends body to target as caller - a person by X-Dispatch-User, or the shared agent
// token for "agent-token", or nobody for "" - naming pressed in Dispatch-Login when it is not nil,
// under key when it is not empty.
func pressedAs(t *testing.T, handler http.Handler, method, target string, body any, caller string, pressed *string, key string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	switch caller {
	case "":
	case "agent-token":
		request.Header.Set("Authorization", "Bearer agent-token")
	default:
		request.Header.Set("X-Dispatch-User", caller)
	}
	if pressed != nil {
		request.Header.Set(contracts.DispatchLoginHeader, *pressed)
	}
	if key != "" {
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// INFRA-258. A write the page sends names the person who pressed it (Dispatch-Login), and runs only
// as that person: another tab that signed someone else in in place makes this tab's retry go out on
// the new cookie, and Dispatch answers it 409 LOGIN_CHANGED before anything runs, its key untouched,
// on every write method. The person who pressed it runs it, however their login is cased; a bearer
// naming a login is refused the same way; a caller that resolves to nobody gets the route's own 401;
// a write without the header, and every read, is not checked.
func TestAWriteRunsOnlyAsThePersonWhoPressedIt(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createTestIssue(t, handler, "LOGIN", "Pressed by whom")
	comments := "/api/v1/issues/" + issue + "/comments"
	alice, bob, empty := "alice", "bob", " "
	count := func(table string) int {
		t.Helper()
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from `+table).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	commentsBefore := count("comments")

	for name, refused := range map[string]*httptest.ResponseRecorder{
		"a POST pressed by bob, sent as alice": pressedAs(t, handler, http.MethodPost, comments,
			map[string]any{"body": "Pressed by bob."}, "alice", &bob, "pressed-by-bob"),
		"a PATCH pressed by bob": pressedAs(t, handler, http.MethodPatch, "/api/v1/issues/"+issue,
			map[string]any{"title": "Renamed under bob"}, "alice", &bob, "patch-by-bob"),
		"a PUT pressed by bob": pressedAs(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue+"/state",
			map[string]any{}, "alice", &bob, ""),
		"a DELETE pressed by bob": pressedAs(t, handler, http.MethodDelete, "/api/v1/issues/"+issue+"/subscribers/s1",
			nil, "alice", &bob, ""),
		"a bearer naming alice": pressedAs(t, handler, http.MethodPost, comments,
			map[string]any{"body": "From a token.", "actor": map[string]string{"kind": "session", "id": "s1"}}, "agent-token", &alice, "token-press"),
		"a header naming nobody": pressedAs(t, handler, http.MethodPost, comments,
			map[string]any{"body": "Pressed by nobody."}, "alice", &empty, "pressed-by-nobody"),
	} {
		if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != contracts.DispatchLoginChanged {
			t.Errorf("%s: status=%d body=%s, want 409 %s", name, refused.Code, refused.Body.String(), contracts.DispatchLoginChanged)
		}
		if refused.Header().Get(contracts.IdempotencyKeyHeader) != "" {
			t.Errorf("%s: the refusal echoes a key, want it answered before the key is read", name)
		}
	}
	if keys, made := count("request_keys"), count("comments")-commentsBefore; keys != 0 || made != 0 {
		t.Fatalf("the refused writes left %d keys and %d comments, want none: nothing runs", keys, made)
	}

	upperAlice := "Alice"
	for name, ran := range map[string]*httptest.ResponseRecorder{
		"pressed by alice, cased otherwise": pressedAs(t, handler, http.MethodPost, comments,
			map[string]any{"body": "Pressed by alice."}, "alice", &upperAlice, "pressed-by-alice"),
		"no header": pressedAs(t, handler, http.MethodPost, comments,
			map[string]any{"body": "No header."}, "alice", nil, "no-header"),
	} {
		if ran.Code != http.StatusCreated {
			t.Errorf("%s: status=%d body=%s, want the comment made", name, ran.Code, ran.Body.String())
		}
	}
	if unresolved := pressedAs(t, handler, http.MethodPost, comments,
		map[string]any{"body": "Signed out."}, "", &alice, "signed-out"); unresolved.Code != http.StatusUnauthorized {
		t.Errorf("a caller that resolves to nobody: status=%d body=%s, want the route's own 401", unresolved.Code, unresolved.Body.String())
	}
	if read := pressedAs(t, handler, http.MethodGet, comments, nil, "alice", &bob, ""); read.Code != http.StatusOK {
		t.Errorf("a read naming another login: status=%d, want it answered as ever", read.Code)
	}
}

// INFRA-258. The Conversation's read mark is a PUT, and runs only as the person who
// pressed it, as every write does: a mark pressed by bob that goes out on alice's session - another
// tab signed her in in place - is refused 409 LOGIN_CHANGED and writes nothing into her state, so an
// issue never reads as read for someone who never opened it. Pressed by alice, it is written.
func TestAReadMarkRunsOnlyAsThePersonWhoPressedIt(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createTestIssue(t, handler, "MARK", "Read by whom")
	state := "/api/v1/me/issues/" + issue + "/state"
	alice, bob := "alice", "bob"
	lastRead := func() *int {
		var seq *int
		if err := database.Pool.QueryRow(context.Background(), `
			select max(last_read_seq) from user_issue_state where login = 'alice' and issue_key = $1
		`, issue).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	refused := pressedAs(t, handler, http.MethodPut, state, map[string]any{"last_read_seq": 3}, "alice", &bob, "")
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusConflict || code != contracts.DispatchLoginChanged {
		t.Fatalf("a read mark pressed by bob on alice's session: status=%d body=%s, want 409 %s",
			refused.Code, refused.Body.String(), contracts.DispatchLoginChanged)
	}
	if seq := lastRead(); seq != nil {
		t.Fatalf("the refused mark left alice's last read at %d, want her state untouched", *seq)
	}
	if marked := pressedAs(t, handler, http.MethodPut, state, map[string]any{"last_read_seq": 3}, "alice", &alice, ""); marked.Code != http.StatusOK {
		t.Fatalf("alice's own read mark: status=%d body=%s, want 200", marked.Code, marked.Body.String())
	}
	if seq := lastRead(); seq == nil || *seq != 3 {
		t.Fatalf("alice's own mark left her last read at %v, want 3", seq)
	}
}

// Every write route of the table - each POST, PATCH, PUT and DELETE - checks Dispatch-Login before
// its handler runs: register wraps every route but a read in the check, so no write route can be
// added that a retry on another person's session runs.
func TestEveryWriteRouteChecksWhoPressedIt(t *testing.T) {
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	bob := "bob"
	writes := 0
	for _, route := range served.routes() {
		if route.Method == http.MethodGet {
			continue
		}
		writes++
		var path []string
		for segment := range strings.SplitSeq(route.Pattern, "/") {
			switch {
			case segment == "{$}":
				segment = ""
			case strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}"):
				segment = "x"
			}
			path = append(path, segment)
		}
		refused := pressedAs(t, handler, route.Method, strings.Join(path, "/"), map[string]any{}, "alice", &bob, "")
		// A route the check let through answers whatever its handler does, which need not be JSON.
		var refusal struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(refused.Body.Bytes(), &refusal)
		if refused.Code != http.StatusConflict || refusal.Code != contracts.DispatchLoginChanged {
			t.Errorf("%s %s pressed by bob on alice's session: status=%d body=%.200s, want 409 %s",
				route.Method, route.Pattern, refused.Code, refused.Body.String(), contracts.DispatchLoginChanged)
		}
	}
	if writes == 0 {
		t.Fatal("the routes table holds no write route")
	}
}
