package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// legionListener is a listener's /v1 mux over a session registry and the Legion agent registry on
// one NATS account: the stores the /v1/legion/agents routes read.
type legionListener struct {
	deps *listenerDeps
	mux  http.Handler
}

func newLegionListener(t *testing.T, client *bus.Client) *legionListener {
	t.Helper()
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("warm the session cache: %v", err)
	}
	agents, err := legionagents.Open(client.Conn, legionagents.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the legion agent registry: %v", err)
	}
	deps := &listenerDeps{client: client, sessions: sessions, legionAgents: agents}
	mux := http.NewServeMux()
	registerV1Routes(mux, deps, "test-machine", logging.New("test"))
	return &legionListener{deps: deps, mux: mux}
}

func (l *legionListener) do(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	l.mux.ServeHTTP(recorder, httptest.NewRequest(method, target, strings.NewReader(body)))
	return recorder
}

// want asserts a request's status and returns its body.
func (l *legionListener) want(t *testing.T, status int, method, target, body string) string {
	t.Helper()
	recorder := l.do(t, method, target, body)
	if recorder.Code != status {
		t.Fatalf("%s %s = %d %s, want %d", method, target, recorder.Code, recorder.Body.String(), status)
	}
	return recorder.Body.String()
}

// legionEntry is a valid registry entry's JSON: session id on issue's role under the Legion
// project token infra, started at started.
func legionEntry(id, issue, role, started string) string {
	return fmt.Sprintf(`{"session_id":%q,"legion_project":"infra","issue":%q,"role":%q,"role_token":%q,"title":%q,"harness":"omp","started_at":%q}`,
		id, issue, role, "legion-infra-"+strings.ToLower(issue)+"-"+role, "Legion "+role+" · "+issue, started)
}

// legionAnswer is one entry as a read answers it.
type legionAnswer struct {
	SessionID     string  `json:"session_id"`
	LegionProject string  `json:"legion_project"`
	Issue         string  `json:"issue"`
	Role          string  `json:"role"`
	RoleToken     string  `json:"role_token"`
	Title         string  `json:"title"`
	Harness       string  `json:"harness"`
	StartedAt     string  `json:"started_at"`
	RecordedAt    string  `json:"recorded_at"`
	Live          bool    `json:"live"`
	ReplacedBy    *string `json:"replaced_by"`
	ReplacedAt    *string `json:"replaced_at"`
}

func decodeLegion[T any](t *testing.T, body string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return value
}

func legionIDs(t *testing.T, body string) []string {
	t.Helper()
	list := decodeLegion[struct{ Entries []legionAnswer }](t, body)
	ids := make([]string, len(list.Entries))
	for i, entry := range list.Entries {
		ids[i] = entry.SessionID
	}
	return ids
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// One entry written through PUT /v1/legion/agents/sessions/<id> is answered whole by each read,
// with live following the session registry as it stands at the read.
func TestLegionAgentRoutesRoundTripAnEntry(t *testing.T) {
	l := newLegionListener(t, setupPublishTestClient(t))
	stored := decodeLegion[legionAnswer](t, l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/ses-tester",
		legionEntry("ses-tester", "INFRA-448", "tester", "2026-10-26T03:15:00Z")))
	if stored.RoleToken != "legion-infra-infra-448-tester" || stored.RecordedAt == "" || stored.Title != "Legion tester · INFRA-448" {
		t.Fatalf("stored = %#v", stored)
	}

	got := decodeLegion[legionAnswer](t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents/sessions/ses-tester", ""))
	if got.Live || got.ReplacedBy != nil || got.ReplacedAt != nil || got.RecordedAt != stored.RecordedAt || got.Harness != "omp" {
		t.Fatalf("an unregistered session answered %#v, want not live and not replaced", got)
	}
	if err := l.deps.sessions.Put("ses-tester", session.SessionEntry{Port: 1, MachineID: "test-machine"}); err != nil {
		t.Fatalf("register ses-tester: %v", err)
	}
	if got := decodeLegion[legionAnswer](t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents/sessions/ses-tester", "")); !got.Live {
		t.Fatalf("a registered session answered %#v, want live", got)
	}
	byRole := decodeLegion[struct{ Entries []legionAnswer }](t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents/roles/legion-infra-infra-448-tester", ""))
	if len(byRole.Entries) != 1 || !byRole.Entries[0].Live || byRole.Entries[0].SessionID != "ses-tester" {
		t.Fatalf("by role token = %#v", byRole)
	}
	byIssue := decodeLegion[struct{ Entries []legionAnswer }](t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents?issue=INFRA-448", ""))
	if len(byIssue.Entries) != 1 || byIssue.Entries[0] != byRole.Entries[0] {
		t.Fatalf("by issue = %#v, want %#v", byIssue, byRole)
	}
}

// A batch is recorded in order, and each listing filters its issue or role token, orders an issue
// by role then newest first, and derives who replaced whom within a role token.
func TestLegionAgentBatchRouteRecordsEveryEntry(t *testing.T) {
	l := newLegionListener(t, setupPublishTestClient(t))
	body := `{"entries":[` + strings.Join([]string{
		legionEntry("ses-old-reviewer", "INFRA-448", "reviewer", "2026-10-26T01:00:00Z"),
		legionEntry("ses-new-reviewer", "INFRA-448", "reviewer", "2026-10-26T02:00:00Z"),
		legionEntry("ses-impl", "INFRA-448", "implementer", "2026-10-26T00:30:00Z"),
		legionEntry("ses-other", "INFRA-449", "reviewer", "2026-10-26T03:00:00Z"),
	}, ",") + `]}`
	stored := legionIDs(t, l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents", body))
	if !slices.Equal(stored, []string{"ses-old-reviewer", "ses-new-reviewer", "ses-impl", "ses-other"}) {
		t.Fatalf("stored %v, want the batch in order", stored)
	}
	if got := legionIDs(t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents?issue=INFRA-448", "")); !slices.Equal(got, []string{"ses-impl", "ses-new-reviewer", "ses-old-reviewer"}) {
		t.Fatalf("by issue = %v, want the implementer, then the reviewers newest first", got)
	}
	byRole := decodeLegion[struct{ Entries []legionAnswer }](t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents/roles/legion-infra-infra-448-reviewer", ""))
	if len(byRole.Entries) != 2 || byRole.Entries[0].SessionID != "ses-new-reviewer" || byRole.Entries[0].ReplacedBy != nil {
		t.Fatalf("by role token = %#v, want the current reviewer first, not replaced", byRole)
	}
	if old := byRole.Entries[1]; old.SessionID != "ses-old-reviewer" || deref(old.ReplacedBy) != "ses-new-reviewer" || deref(old.ReplacedAt) != "2026-10-26T02:00:00Z" {
		t.Fatalf("the old reviewer answered %#v, want replaced by ses-new-reviewer at its start", old)
	}
	old := decodeLegion[legionAnswer](t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents/sessions/ses-old-reviewer", ""))
	if deref(old.ReplacedBy) != "ses-new-reviewer" || deref(old.ReplacedAt) != "2026-10-26T02:00:00Z" {
		t.Fatalf("GET the old reviewer = %#v, want it replaced", old)
	}
	if got := legionIDs(t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents/roles/legion-infra-infra-449-reviewer", "")); !slices.Equal(got, []string{"ses-other"}) {
		t.Fatalf("INFRA-449's reviewers = %v", got)
	}
}

// An absence is answered as one: an unknown session is a 404 naming it, and a listing with nothing
// in it is an empty list.
func TestLegionAgentRoutesAnswerAnAbsence(t *testing.T) {
	l := newLegionListener(t, setupPublishTestClient(t))
	l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/ses-tester", legionEntry("ses-tester", "INFRA-448", "tester", "2026-10-26T03:15:00Z"))
	if got := l.want(t, http.StatusNotFound, http.MethodGet, "/v1/legion/agents/sessions/ses-unknown", ""); strings.TrimSpace(got) != `{"error":"unknown legion agent ses-unknown"}` {
		t.Fatalf("404 body = %s", got)
	}
	for _, target := range []string{"/v1/legion/agents/roles/legion-infra-infra-448-reviewer", "/v1/legion/agents?issue=INFRA-1"} {
		if got := l.want(t, http.StatusOK, http.MethodGet, target, ""); strings.TrimSpace(got) != `{"entries":[]}` {
			t.Fatalf("GET %s = %s, want an empty list", target, got)
		}
	}
	if got := l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents", `{"entries":[]}`); strings.TrimSpace(got) != `{"entries":[]}` {
		t.Fatalf("an empty batch answered %s", got)
	}
}

// Each request the registry refuses is a 400 (a 413 past a bound, a 405 for a method no route
// takes) and writes nothing.
func TestLegionAgentRoutesRefuseWhatTheRegistryDoesNotTake(t *testing.T) {
	l := newLegionListener(t, setupPublishTestClient(t))
	good := legionEntry("ses-good", "INFRA-448", "tester", "2026-10-26T03:15:00Z")
	badRole := strings.Replace(legionEntry("ses-bad", "INFRA-448", "tester", "2026-10-26T03:15:00Z"), `"role":"tester"`, `"role":"controller"`, 1)
	wrongProject := strings.Replace(good, `"legion_project":"infra"`, `"legion_project":"sjawharlegion"`, 1)
	many := make([]string, legionagents.MaxBatch+1)
	for i := range many {
		many[i] = legionEntry("ses-"+strconv.Itoa(i), "INFRA-448", "tester", "2026-10-26T03:15:00Z")
	}
	for _, tc := range []struct {
		name, method, target, body string
		status                     int
		says                       string
	}{
		{"a session id with a dot", http.MethodGet, "/v1/legion/agents/sessions/ses.bad", "", http.StatusBadRequest, "session_id"},
		{"a session id outside the key alphabet", http.MethodGet, "/v1/legion/agents/sessions/ses:bad", "", http.StatusBadRequest, "session_id"},
		{"no session id", http.MethodGet, "/v1/legion/agents/sessions/", "", http.StatusBadRequest, "session_id is required"},
		{"a token that is not a role's", http.MethodGet, "/v1/legion/agents/roles/legion-infra-controller", "", http.StatusBadRequest, "role_token"},
		{"no token", http.MethodGet, "/v1/legion/agents/roles/", "", http.StatusBadRequest, "role_token is required"},
		{"no issue", http.MethodGet, "/v1/legion/agents", "", http.StatusBadRequest, "issue is required"},
		{"a lower-case issue", http.MethodGet, "/v1/legion/agents?issue=infra-448", "", http.StatusBadRequest, "issue"},
		{"an issue with a wildcard", http.MethodGet, "/v1/legion/agents?issue=INFRA-448.%3E", "", http.StatusBadRequest, "issue"},
		{"a path naming another session", http.MethodPut, "/v1/legion/agents/sessions/ses-other", good, http.StatusBadRequest, `is not the path's`},
		{"a path id the registry refuses", http.MethodPut, "/v1/legion/agents/sessions/ses.bad", good, http.StatusBadRequest, "session_id"},
		{"a role outside the six", http.MethodPut, "/v1/legion/agents/sessions/ses-bad", badRole, http.StatusBadRequest, "role"},
		{"a token built from another project", http.MethodPut, "/v1/legion/agents/sessions/ses-good", wrongProject, http.StatusBadRequest, "role_token"},
		{"a body that is not JSON", http.MethodPut, "/v1/legion/agents/sessions/ses-good", "{", http.StatusBadRequest, "invalid json"},
		{"a batch with a bad member", http.MethodPut, "/v1/legion/agents", `{"entries":[` + good + `,` + badRole + `]}`, http.StatusBadRequest, "entries[1]"},
		{"a batch without entries", http.MethodPut, "/v1/legion/agents", `{}`, http.StatusBadRequest, "entries is required"},
		{"a batch past its bound", http.MethodPut, "/v1/legion/agents", `{"entries":[` + strings.Join(many, ",") + `]}`, http.StatusRequestEntityTooLarge, "at most 256"},
		{"a body past 1 MiB", http.MethodPut, "/v1/legion/agents", `{"entries":[],"pad":"` + strings.Repeat("x", 1<<20) + `"}`, http.StatusRequestEntityTooLarge, "past 1048576 bytes"},
		{"a role token read by PUT", http.MethodPut, "/v1/legion/agents/roles/legion-infra-infra-448-tester", good, http.StatusMethodNotAllowed, "method not allowed"},
		{"a delete", http.MethodDelete, "/v1/legion/agents/sessions/ses-good", "", http.StatusMethodNotAllowed, "method not allowed"},
	} {
		got := l.want(t, tc.status, tc.method, tc.target, tc.body)
		if !strings.Contains(got, tc.says) {
			t.Fatalf("%s: body %.300s, want it to say %s", tc.name, got, tc.says)
		}
	}
	if got := l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents?issue=INFRA-448", ""); strings.TrimSpace(got) != `{"entries":[]}` {
		t.Fatalf("after every refusal INFRA-448 lists %s, want nothing written", got)
	}
}

// A session belongs to one issue and role for good: a write moving it, alone or in a batch, is a
// 409 naming what it is recorded for, and nothing changes.
func TestLegionAgentRoutesRefuseMovingASession(t *testing.T) {
	l := newLegionListener(t, setupPublishTestClient(t))
	l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/ses-tester", legionEntry("ses-tester", "INFRA-448", "tester", "2026-10-26T03:15:00Z"))
	got := l.want(t, http.StatusConflict, http.MethodPut, "/v1/legion/agents/sessions/ses-tester", legionEntry("ses-tester", "INFRA-449", "tester", "2026-10-26T03:15:00Z"))
	if strings.TrimSpace(got) != `{"error":"legion agent ses-tester is recorded for INFRA-448 tester"}` {
		t.Fatalf("409 body = %s", got)
	}
	got = l.want(t, http.StatusConflict, http.MethodPut, "/v1/legion/agents", `{"entries":[`+
		legionEntry("ses-reviewer", "INFRA-448", "reviewer", "2026-10-26T04:00:00Z")+`,`+
		legionEntry("ses-tester", "INFRA-448", "reviewer", "2026-10-26T03:15:00Z")+`]}`)
	if !strings.Contains(got, "entries[1]: legion agent ses-tester is recorded for INFRA-448 tester") {
		t.Fatalf("batch 409 body = %s", got)
	}
	if ids := legionIDs(t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents?issue=INFRA-448", "")); !slices.Equal(ids, []string{"ses-tester"}) {
		t.Fatalf("after the refusals INFRA-448 lists %v, want the tester alone", ids)
	}
	if ids := legionIDs(t, l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents?issue=INFRA-449", "")); len(ids) != 0 {
		t.Fatalf("the refused move listed %v under INFRA-449", ids)
	}
}

// The project.<legion_project> key is the daemon's own (INFRA-204), and no route reads it.
func TestLegionAgentRoutesIgnoreAProjectKey(t *testing.T) {
	client := setupPublishTestClient(t)
	l := newLegionListener(t, client)
	l.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/ses-tester", legionEntry("ses-tester", "INFRA-448", "tester", "2026-10-26T03:15:00Z"))
	kv, err := client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	if _, err := kv.Put("project.infra", []byte(`{"answering":true,"updated_at":"2026-10-26T04:00:00Z"}`)); err != nil {
		t.Fatalf("put the project key: %v", err)
	}
	for _, target := range []string{"/v1/legion/agents?issue=INFRA-448", "/v1/legion/agents/roles/legion-infra-infra-448-tester"} {
		if ids := legionIDs(t, l.want(t, http.StatusOK, http.MethodGet, target, "")); !slices.Equal(ids, []string{"ses-tester"}) {
			t.Fatalf("GET %s = %v, want the tester alone", target, ids)
		}
	}
	l.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents/sessions/ses-tester", "")
}

// The registry is the bucket's: a listener started again over a restarted NATS answers what the
// one before it recorded, and a read the bucket cannot answer is a 503, never an absence.
func TestLegionAgentsSurviveAListenerRestart(t *testing.T) {
	server, uri := testnats.Start(t)
	connect := func() *bus.Client {
		client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		return client
	}
	before := newLegionListener(t, connect())
	stored := before.want(t, http.StatusOK, http.MethodPut, "/v1/legion/agents/sessions/ses-tester", legionEntry("ses-tester", "INFRA-448", "tester", "2026-10-26T03:15:00Z"))

	server.Stop(t)
	recorder := before.do(t, http.MethodGet, "/v1/legion/agents/sessions/ses-tester", "")
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
		t.Fatalf("a read with NATS down = %d %s, want a 503 to retry", recorder.Code, recorder.Body.String())
	}
	recorder = before.do(t, http.MethodPut, "/v1/legion/agents", `{"entries":[`+legionEntry("ses-other", "INFRA-448", "reviewer", "2026-10-26T04:00:00Z")+`]}`)
	if failure := decodeLegion[legionAgentBatchFailure](t, recorder.Body.String()); recorder.Code != http.StatusServiceUnavailable || failure.Written != 0 || !strings.Contains(failure.Error, "wrote 0 of 1") {
		t.Fatalf("a batch with NATS down = %d %s, want a 503 naming none written", recorder.Code, recorder.Body.String())
	}
	server.Start(t)

	after := newLegionListener(t, connect())
	got := decodeLegion[legionAnswer](t, after.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents/sessions/ses-tester", ""))
	if want := decodeLegion[legionAnswer](t, stored); got != want {
		t.Fatalf("after the restart = %#v, want %#v", got, want)
	}
	if ids := legionIDs(t, after.want(t, http.StatusOK, http.MethodGet, "/v1/legion/agents?issue=INFRA-448", "")); !slices.Equal(ids, []string{"ses-tester"}) {
		t.Fatalf("after the restart INFRA-448 lists %v", ids)
	}
}
