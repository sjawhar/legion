package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// collaboratorsAnswer is GET /api/v1/issues/{key}/collaborators as a reader decodes it.
type collaboratorsAnswer struct {
	IssueKey string `json:"issue_key"`
	People   []struct {
		Kind    string   `json:"kind"`
		Login   string   `json:"login"`
		Reasons []string `json:"reasons"`
		LastAt  *string  `json:"last_at"`
	} `json:"people"`
	Agents []struct {
		Kind      string   `json:"kind"`
		SessionID string   `json:"session_id"`
		Title     *string  `json:"title"`
		Reasons   []string `json:"reasons"`
		LastAt    *string  `json:"last_at"`
		Legion    *struct {
			Role       string  `json:"role"`
			RoleToken  string  `json:"role_token"`
			StartedAt  string  `json:"started_at"`
			ReplacedBy *string `json:"replaced_by"`
			ReplacedAt *string `json:"replaced_at"`
		} `json:"legion"`
	} `json:"agents"`
	LegionRegistry string `json:"legion_registry"`
}

// person is the row for login, failing the test when there is not exactly one.
func (a collaboratorsAnswer) person(t *testing.T, login string) (reasons []string, lastAt *string) {
	t.Helper()
	found := 0
	for _, row := range a.People {
		if row.Login == login {
			found++
			reasons, lastAt = row.Reasons, row.LastAt
		}
	}
	if found != 1 {
		t.Fatalf("people hold %d rows for %s, want 1: %+v", found, login, a.People)
	}
	return reasons, lastAt
}

func (a collaboratorsAnswer) logins() []string {
	logins := make([]string, len(a.People))
	for index, row := range a.People {
		logins[index] = row.Login
	}
	return logins
}

func (a collaboratorsAnswer) sessionIDs() []string {
	ids := make([]string, len(a.Agents))
	for index, row := range a.Agents {
		ids[index] = row.SessionID
	}
	return ids
}

// legionRegistryListener is the Envoy listener a collaborators test runs against: it serves the
// Legion agent registry's issue read from entries (any other issue has none), answers every
// session lookup "no live session" (a claim asks about the holder it would replace), and refuses
// everything else.
type legionRegistryListener struct {
	mu      sync.Mutex
	issue   string
	entries []envoy.LegionAgent
	status  int
	reads   int
}

func (l *legionRegistryListener) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/legion/agents":
			l.reads++
			if l.status != 0 {
				w.WriteHeader(l.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "nats: no responders available for request"})
				return
			}
			entries := []envoy.LegionAgent{}
			if r.URL.Query().Get("issue") == l.issue {
				entries = l.entries
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
		case strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no live session " + strings.TrimPrefix(r.URL.Path, "/v1/registry/")})
		default:
			t.Errorf("the listener was asked %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (l *legionRegistryListener) set(issue string, entries ...envoy.LegionAgent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.issue, l.entries = issue, entries
}

func (l *legionRegistryListener) readCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads
}

// legionAgentEntry is a registry entry as the listener answers it, under the Legion project token
// sjawharlegion, which is not the Dispatch project.
func legionAgentEntry(id, issue, role, started string, replacedBy, replacedAt *string) envoy.LegionAgent {
	return envoy.LegionAgent{
		SessionID:     id,
		LegionProject: "sjawharlegion",
		Issue:         issue,
		Role:          role,
		RoleToken:     "legion-sjawharlegion-" + strings.ToLower(issue) + "-" + role,
		Title:         "Legion " + role + " · " + issue,
		Harness:       "omp",
		StartedAt:     started,
		RecordedAt:    started,
		ReplacedBy:    replacedBy,
		ReplacedAt:    replacedAt,
	}
}

func readCollaborators(t *testing.T, handler http.Handler, key string) collaboratorsAnswer {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/collaborators", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read collaborators: status=%d body=%s", response.Code, response.Body.String())
	}
	answer := decodeBody[collaboratorsAnswer](t, response)
	if answer.IssueKey != key {
		t.Fatalf("collaborators issue_key = %q, want %q", answer.IssueKey, key)
	}
	for _, row := range answer.People {
		if row.Kind != "user" {
			t.Fatalf("person %s has kind %q, want user", row.Login, row.Kind)
		}
	}
	for _, row := range answer.Agents {
		if row.Kind != "session" {
			t.Fatalf("agent %s has kind %q, want session", row.SessionID, row.Kind)
		}
	}
	return answer
}

// insertIssueEvent appends one event row to key's log as an older server stored it, taking the
// issue's next sequence so a later write through the broker still takes the one after.
func insertIssueEvent(t *testing.T, database *store.Store, key, eventType, actor string, at time.Time) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		with next as (update issues set last_seq = last_seq + 1 where key = $1 returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select $1, last_seq, $2, $3::jsonb, '{}', false, $4 from next
	`, key, eventType, actor, at); err != nil {
		t.Fatalf("insert %s event: %v", eventType, err)
	}
}

func wantStatus(t *testing.T, what string, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("%s: status=%d body=%s, want %d", what, response.Code, response.Body.String(), status)
	}
}

// Every source names its collaborator through the real write routes: the assignee, a claim, an
// anchored comment on the issue's spec, a message, an ask and its answer, a document edit, and
// the registry; a session the registry records and that also posted is one agent carrying both;
// the daemon's lifecycle writer is nobody.
func TestIssueCollaboratorsListsEverySourceOncePerPersonAndSession(t *testing.T) {
	listener := &legionRegistryListener{}
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.serve(t).URL})
	seedPeople(t, database, "carol")
	issue := createInteractionIssue(t, handler, "TEST", "Collaborators", "The quick brown fox")
	key := issue.Key
	replacedBy, replacedAt := "ses-new-reviewer", "2026-10-26T02:00:00Z"
	listener.set(key,
		legionAgentEntry("ses-tester", key, "tester", "2026-10-26T00:30:00Z", nil, nil),
		legionAgentEntry("ses-new-reviewer", key, "reviewer", "2026-10-26T02:00:00Z", nil, nil),
		legionAgentEntry("ses-old-reviewer", key, "reviewer", "2026-10-26T01:00:00Z", &replacedBy, &replacedAt),
	)

	wantStatus(t, "claim", bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("ses-claimer", "Implementer worker"),
	}), http.StatusOK)
	wantStatus(t, "bob's anchored comment", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": "Why brown?", "anchor": map[string]any{"artifact": "spec", "quote": "brown"},
	}, "bob"), http.StatusCreated)
	wantStatus(t, "carol's message", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/messages", map[string]any{
		"body": "A note from carol",
	}, "carol"), http.StatusCreated)
	wantStatus(t, "the tester's message", bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/messages", map[string]any{
		"body": "Tests pass", "actor": claimActorBody("ses-tester", "stamped tester title"),
	}), http.StatusCreated)
	wantStatus(t, "the daemon's message", bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/messages", map[string]any{
		"body": "Legion moved the issue", "actor": map[string]any{"kind": "session", "id": "legion-daemon:TEST"},
	}), http.StatusCreated)
	asked := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/asks", map[string]any{
		"question": "Ship it?", "actor": map[string]any{"kind": "session", "id": "ses-asker"},
	})
	wantStatus(t, "ask", asked, http.StatusCreated)
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, asked).ID
	wantStatus(t, "alice's answer", dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"text": "Yes",
	}, "alice"), http.StatusOK)
	wantStatus(t, "document edit", bearerRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "quick", "with": "slow"}}, "actor": map[string]any{"kind": "session", "id": "ses-editor"},
	}), http.StatusOK)

	answer := readCollaborators(t, handler, key)
	if answer.LegionRegistry != "ok" {
		t.Fatalf("legion_registry = %q, want ok", answer.LegionRegistry)
	}
	if got, want := answer.logins(), []string{"alice", "carol", "bob"}; !slices.Equal(got, want) {
		t.Fatalf("people = %v, want %v (newest last_at first)", got, want)
	}
	for login, want := range map[string][]string{
		"alice": {"assignee", "answered"},
		"bob":   {"posted"},
		"carol": {"posted"},
	} {
		reasons, lastAt := answer.person(t, login)
		if !slices.Equal(reasons, want) {
			t.Fatalf("%s reasons = %v, want %v", login, reasons, want)
		}
		if lastAt == nil {
			t.Fatalf("%s has no last_at", login)
		}
	}

	if got, want := answer.sessionIDs(), []string{
		"ses-tester", "ses-new-reviewer", "ses-old-reviewer", "ses-editor", "ses-asker", "ses-claimer",
	}; !slices.Equal(got, want) {
		t.Fatalf("agents = %v, want %v: the registry's in its order, then the others newest first, no daemon", got, want)
	}
	tester, newReviewer, oldReviewer, editor, asker, claimer := answer.Agents[0], answer.Agents[1], answer.Agents[2], answer.Agents[3], answer.Agents[4], answer.Agents[5]
	if tester.Legion == nil || tester.Legion.Role != "tester" || tester.Legion.RoleToken != "legion-sjawharlegion-"+strings.ToLower(key)+"-tester" ||
		tester.Legion.StartedAt != "2026-10-26T00:30:00Z" || tester.Legion.ReplacedBy != nil || tester.Legion.ReplacedAt != nil {
		t.Fatalf("tester legion = %+v", tester.Legion)
	}
	if tester.Title == nil || *tester.Title != "Legion tester · "+key {
		t.Fatalf("tester title = %v, want the registry's", tester.Title)
	}
	if !slices.Equal(tester.Reasons, []string{"posted"}) || tester.LastAt == nil {
		t.Fatalf("tester, who posted and is in the registry: reasons=%v last_at=%v", tester.Reasons, tester.LastAt)
	}
	if newReviewer.Legion == nil || newReviewer.Legion.ReplacedBy != nil || len(newReviewer.Reasons) != 0 || newReviewer.LastAt != nil {
		t.Fatalf("the current reviewer = %+v legion %+v, want a registry entry with no events", newReviewer, newReviewer.Legion)
	}
	if oldReviewer.Legion == nil || oldReviewer.Legion.ReplacedBy == nil || *oldReviewer.Legion.ReplacedBy != "ses-new-reviewer" ||
		oldReviewer.Legion.ReplacedAt == nil || *oldReviewer.Legion.ReplacedAt != "2026-10-26T02:00:00Z" {
		t.Fatalf("the replaced reviewer's legion = %+v, want replaced by ses-new-reviewer at its start", oldReviewer.Legion)
	}
	if !slices.Equal(editor.Reasons, []string{"posted"}) || editor.Legion != nil {
		t.Fatalf("editor = %+v", editor)
	}
	if !slices.Equal(asker.Reasons, []string{"posted"}) || asker.Legion != nil {
		t.Fatalf("asker = %+v", asker)
	}
	if !slices.Equal(claimer.Reasons, []string{"claimed"}) || claimer.Legion != nil {
		t.Fatalf("claimer = %+v", claimer)
	}
	if claimer.Title == nil || *claimer.Title != "Implementer worker" {
		t.Fatalf("claimer title = %v, want the newest stamped title", claimer.Title)
	}

	// An agent token reads it as a person does.
	read := sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/collaborators", nil)
	if read.Code != http.StatusOK || decodeBody[collaboratorsAnswer](t, read).LegionRegistry != "ok" {
		t.Fatalf("agent read: status=%d body=%s", read.Code, read.Body.String())
	}
}

// Each event type that makes its actor a collaborator gives the reason it means, a person's and a
// session's alike; an event of any other type makes nobody one.
func TestIssueCollaboratorsTakesEachEventTypesReason(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	key := createInteractionIssue(t, handler, "TEST", "Event types", "spec").Key
	wantStatus(t, "unassign", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"assignee": nil}, "alice"), http.StatusOK)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	want := map[string]string{
		"issue.claimed":    "claimed",
		"comment.created":  "posted",
		"message.created":  "posted",
		"ask.opened":       "posted",
		"artifact.version": "posted",
		"comment.answered": "posted",
		"message.answered": "posted",
		"ask.answered":     "answered",
	}
	for eventType := range want {
		id := strings.ReplaceAll(eventType, ".", "-")
		insertIssueEvent(t, database, key, eventType, `{"kind":"user","id":"`+id+`@example.com"}`, at)
		insertIssueEvent(t, database, key, eventType, `{"kind":"session","id":"ses-`+id+`"}`, at)
	}
	for _, eventType := range []string{"issue.created", "issue.updated", "issue.released", "comment.resolved", "ask.resolved", "ask.edited", "artifact.created", "message.delivery"} {
		insertIssueEvent(t, database, key, eventType, `{"kind":"user","id":"bystander@example.com"}`, at)
		insertIssueEvent(t, database, key, eventType, `{"kind":"session","id":"ses-bystander"}`, at)
	}
	insertIssueEvent(t, database, key, "comment.created", `{"kind":"system","id":"architecture-importer"}`, at)

	answer := readCollaborators(t, handler, key)
	if len(answer.People) != len(want) || len(answer.Agents) != len(want) {
		t.Fatalf("people %v and agents %v, want one of each per collaborator event type (%d)", answer.logins(), answer.sessionIDs(), len(want))
	}
	for eventType, reason := range want {
		id := strings.ReplaceAll(eventType, ".", "-")
		reasons, lastAt := answer.person(t, id+"@example.com")
		if !slices.Equal(reasons, []string{reason}) || lastAt == nil || *lastAt != "2026-10-26T12:00:00Z" {
			t.Fatalf("the %s person: reasons=%v last_at=%v, want [%s] at 2026-10-26T12:00:00Z", eventType, reasons, lastAt, reason)
		}
		index := slices.Index(answer.sessionIDs(), "ses-"+id)
		if index < 0 || !slices.Equal(answer.Agents[index].Reasons, []string{reason}) {
			t.Fatalf("the %s session: %v, want reasons [%s]", eventType, answer.sessionIDs(), reason)
		}
	}
}

// One login is one person whatever case and spacing its rows were stored in, its reasons and its
// newest time gathered from all of them; a session acting for a person's token is an agent, not
// that person; the daemon's lifecycle writer is left out under any project.
func TestIssueCollaboratorsFoldsLoginsAndSortsSessions(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	key := createInteractionIssue(t, handler, "TEST", "Login case", "spec").Key
	wantStatus(t, "assign bob", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"assignee": "bob"}, "alice"), http.StatusOK)
	early := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	insertIssueEvent(t, database, key, "comment.created", `{"kind":"user","id":" Bob "}`, early)
	insertIssueEvent(t, database, key, "ask.answered", `{"kind":"user","id":"BOB"}`, late)
	insertIssueEvent(t, database, key, "issue.claimed", `{"kind":"user","id":"bob"}`, early)
	insertIssueEvent(t, database, key, "message.created", `{"kind":"session","id":"ses-owned","owner":"carol@example.com"}`, early)
	insertIssueEvent(t, database, key, "message.created", `{"kind":"session","id":"legion-daemon:TEST"}`, late)
	insertIssueEvent(t, database, key, "issue.claimed", `{"kind":"session","id":"legion-daemon:OTHER"}`, late)

	answer := readCollaborators(t, handler, key)
	if got := answer.logins(); !slices.Equal(got, []string{"bob"}) {
		t.Fatalf("people = %v, want bob alone: the assignee and his rows in any case, and no token owner", got)
	}
	reasons, lastAt := answer.person(t, "bob")
	if !slices.Equal(reasons, []string{"assignee", "claimed", "posted", "answered"}) {
		t.Fatalf("bob reasons = %v, want every reason in order", reasons)
	}
	if lastAt == nil || *lastAt != "2026-10-26T10:00:00Z" {
		t.Fatalf("bob last_at = %v, want his newest row's", lastAt)
	}
	if got := answer.sessionIDs(); !slices.Equal(got, []string{"ses-owned"}) {
		t.Fatalf("agents = %v, want the owned session and no daemon writer", got)
	}
	if answer.LegionRegistry != "unavailable" {
		t.Fatalf("legion_registry = %q with no listener configured, want unavailable", answer.LegionRegistry)
	}
}

// A listener that errors or does not answer in the client's window fails nothing: people and the
// sessions the events name still answer, without the registry's entries, as unavailable.
func TestIssueCollaboratorsAnswersWithoutTheRegistry(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T) testServerOptions{
		"listener errors": func(t *testing.T) testServerOptions {
			listener := &legionRegistryListener{status: http.StatusServiceUnavailable}
			return testServerOptions{envoyURL: listener.serve(t).URL}
		},
		"listener times out": func(t *testing.T) testServerOptions {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(release) })
			return testServerOptions{envoyURL: server.URL, envoyTimeout: 100 * time.Millisecond}
		},
		"no listener configured": func(t *testing.T) testServerOptions {
			return testServerOptions{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			handler, _, _ := newTestServer(t, setup(t))
			key := createInteractionIssue(t, handler, "TEST", "Registry down", "spec").Key
			wantStatus(t, "bob's comment", dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
				"body": "Still here",
			}, "bob"), http.StatusCreated)
			wantStatus(t, "the tester's comment", bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
				"body": "Tests pass", "actor": claimActorBody("ses-tester", "Legion tester"),
			}), http.StatusCreated)

			answer := readCollaborators(t, handler, key)
			if answer.LegionRegistry != "unavailable" {
				t.Fatalf("legion_registry = %q, want unavailable", answer.LegionRegistry)
			}
			if got := answer.logins(); !slices.Equal(got, []string{"bob", "alice"}) {
				t.Fatalf("people = %v, want bob (posted) then alice (assignee)", got)
			}
			if got := answer.sessionIDs(); !slices.Equal(got, []string{"ses-tester"}) || answer.Agents[0].Legion != nil ||
				answer.Agents[0].Title == nil || *answer.Agents[0].Title != "Legion tester" {
				t.Fatalf("agents = %+v, want the tester by its stamped title, no legion entry", answer.Agents)
			}
		})
	}
}

// An issue that does not exist is the ordinary 404, before the listener is asked.
func TestIssueCollaboratorsOfAnUnknownIssueIs404(t *testing.T) {
	listener := &legionRegistryListener{}
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.serve(t).URL})
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/TEST-404/collaborators", nil, "alice")
	refusal := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusNotFound || refusal.Code != "NOT_FOUND" || refusal.Error == "" {
		t.Fatalf("collaborators of an unknown issue = %d %s, want 404 NOT_FOUND", response.Code, response.Body.String())
	}
	if reads := listener.readCount(); reads != 0 {
		t.Fatalf("the listener was asked %d times for an unknown issue, want 0", reads)
	}
}
