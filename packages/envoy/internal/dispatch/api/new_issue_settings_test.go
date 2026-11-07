package api

import (
	"net/http"
	"strings"
	"testing"
)

// A deployment that configured neither setting answers both empty, so the bar files an issue
// with no label and starts no conversation on it; one that configured both answers both.
func TestTheNewIssueSettingsReadAnswersWhatTheDeploymentConfigured(t *testing.T) {
	bare := newTestHandler(t)
	if read := decodeBody[newIssueSettings](t, dispatchRequest(t, bare, http.MethodGet, "/api/v1/settings/new-issue", nil, "alice")); read.Label != "" || read.Session != "" {
		t.Fatalf("a deployment that configured neither = %+v, want both empty", read)
	}

	handler, _, _ := newTestServer(t, testServerOptions{newIssueLabel: "from-the-bar", newIssueSession: "sess-1"})
	read := decodeBody[newIssueSettings](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/new-issue", nil, "alice"))
	if read.Label != "from-the-bar" || read.Session != "sess-1" {
		t.Fatalf("configured settings read = %+v, want the label and the session", read)
	}
}

// The test hook replaces both on the process it reaches, so a browser test can read the bar with
// a session configured and then with it cleared; a session that is no session id is refused and
// changes nothing. Without the hook mounted the route is not there at all.
func TestTheNewIssueSettingsTestHookReplacesThem(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{testHooks: true, newIssueLabel: "from-the-bar", newIssueSession: "sess-1"})

	set := decodeBody[newIssueSettings](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/settings/new-issue/_test",
		map[string]string{"label": "typed", "session": "sess-2"}, "alice"))
	if set.Label != "typed" || set.Session != "sess-2" {
		t.Fatalf("the hook answered %+v, want what it was given", set)
	}
	if read := decodeBody[newIssueSettings](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/new-issue", nil, "alice")); read != set {
		t.Fatalf("the read after the hook = %+v, want %+v", read, set)
	}

	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/settings/new-issue/_test",
		map[string]string{"label": "typed", "session": "not a session id"}, "alice")
	if code, _ := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != "INVALID_BODY" {
		t.Fatalf("a session that is no session id: status=%d body=%s", refused.Code, refused.Body.String())
	}
	if read := decodeBody[newIssueSettings](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/new-issue", nil, "alice")); read != set {
		t.Fatalf("the read after the refusal = %+v, want it unchanged at %+v", read, set)
	}

	// The hook clears the session, which is the state the bar says no conversation was started in.
	cleared := decodeBody[newIssueSettings](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/settings/new-issue/_test",
		map[string]string{"label": "typed", "session": ""}, "alice"))
	if cleared.Session != "" || cleared.Label != "typed" {
		t.Fatalf("the cleared session = %+v, want the label alone", cleared)
	}

	without := newTestHandler(t)
	missing := dispatchRequest(t, without, http.MethodPost, "/api/v1/settings/new-issue/_test",
		map[string]string{"label": "typed"}, "alice")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("the hook on a server without test hooks: status=%d body=%s", missing.Code, missing.Body.String())
	}
}

// A session id that is no session id, and a label longer than one an issue may carry, refuse
// startup rather than being carried as configuration no issue can ever take.
func TestNewDepsRefusesNewIssueSettingsNoIssueCouldTake(t *testing.T) {
	if _, err := NewDeps(DepsInput{NewIssueSession: "two words"}); err == nil ||
		!strings.Contains(err.Error(), "DISPATCH_NEW_ISSUE_SESSION") {
		t.Fatalf("a session that is no session id = %v, want it named", err)
	}
	if _, err := NewDeps(DepsInput{NewIssueLabel: strings.Repeat("x", maxIssueLabel16+1)}); err == nil ||
		!strings.Contains(err.Error(), "DISPATCH_NEW_ISSUE_LABEL") {
		t.Fatalf("a label no issue may carry = %v, want it named", err)
	}
	deps, err := NewDeps(DepsInput{NewIssueLabel: " from-the-bar ", NewIssueSession: " sess-1 "})
	if err != nil || deps.NewIssueLabel != "from-the-bar" || deps.NewIssueSession != "sess-1" {
		t.Fatalf("settings with blanks around them = %q, %q, %v; want them trimmed", deps.NewIssueLabel, deps.NewIssueSession, err)
	}
}
