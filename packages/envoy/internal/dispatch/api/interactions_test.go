package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func newInteractionHandler(t *testing.T, makeDocs func(*store.Store) docs.API) (http.Handler, *store.Store) {
	t.Helper()
	database := openEmptyTestStore(t)
	var docsAPI docs.API
	if makeDocs != nil {
		docsAPI = makeDocs(database)
	}
	deps, err := NewDeps(DepsInput{
		Store: database,
		Identity: identity.HeaderIdentity{
			Header:        "X-Dispatch-User",
			AllowedLogins: map[string]struct{}{"alice": {}, "bob": {}},
		},
		AgentToken:      "agent-token",
		RepoProjectsRaw: "owner/repo=TEST",
		Docs:            docsAPI,
		ServerURL:       "https://dispatch.example",
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return mux, database
}

func sessionRequest(t *testing.T, handler http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode session request: %v", err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func createInteractionIssue(t *testing.T, handler http.Handler, project, title, spec string) struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
} {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": project, "name": project + " project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": title, "spec": spec,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, response)
}

func sessionActor() map[string]any {
	return map[string]any{"kind": "session", "id": "session-0123456789abcdef"}
}

type firstReplaceGate struct {
	docs.API
	firstEntered  chan struct{}
	releaseFirst  chan struct{}
	secondEntered chan struct{}
	first         sync.Once
	second        sync.Once
}

func (g *firstReplaceGate) AcceptSuggestion(ctx context.Context, artifactID, markID, replacement string, actor model.Actor) error {
	first := false
	g.first.Do(func() {
		first = true
		close(g.firstEntered)
	})
	if first {
		<-g.releaseFirst
	} else {
		g.second.Do(func() { close(g.secondEntered) })
	}
	return g.API.AcceptSuggestion(ctx, artifactID, markID, replacement, actor)
}

func waitForSecondReplacementOrCommentLock(t *testing.T, database *store.Store, secondReplacement <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-secondReplacement:
			return
		default:
		}
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("second accept did not reach document service or wait for the comment lock")
}

func TestAskWithoutOptionsEmitsEmptyOptionsArray(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Options", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "No options provided", "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"options":[]`) {
		t.Fatalf("create ask without options: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"options":[]`) {
		t.Fatalf("read ask without options: status=%d body=%s", read.Code, read.Body.String())
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), `"options":[]`) {
		t.Fatalf("inbox ask without options: status=%d body=%s", inbox.Code, inbox.Body.String())
	}

	if _, err := database.Pool.Exec(context.Background(), `update asks set options = 'null'::jsonb where id = $1`, ask.ID); err != nil {
		t.Fatalf("restore legacy null ask options: %v", err)
	}
	legacyRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if legacyRead.Code != http.StatusOK || !strings.Contains(legacyRead.Body.String(), `"options":[]`) {
		t.Fatalf("read legacy ask with null options: status=%d body=%s", legacyRead.Code, legacyRead.Body.String())
	}
	legacyInbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if legacyInbox.Code != http.StatusOK || !strings.Contains(legacyInbox.Body.String(), `"options":[]`) {
		t.Fatalf("inbox legacy ask with null options: status=%d body=%s", legacyInbox.Code, legacyInbox.Body.String())
	}
}

func TestAnchoredAskAnswerAndInbox(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored ask", "The quick brown fox")
	input := map[string]any{
		"question": "Which colour?",
		"options":  []map[string]string{{"label": "brown"}, {"label": "red"}},
		"anchor":   map[string]any{"artifact": "spec", "quote": "brown"},
		"actor":    sessionActor(),
	}
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", input)
	if created.Code != http.StatusCreated {
		t.Fatalf("create anchored ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID     string       `json:"id"`
		State  string       `json:"state"`
		Anchor model.Anchor `json:"anchor"`
	}](t, created)
	if ask.State != "open" || ask.Anchor.MarkID != ask.ID || ask.Anchor.Quote != "brown" || ask.Anchor.Version != 1 {
		t.Fatalf("anchored ask = %#v; want open brown mark at version 1", ask)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"question":"Which colour?"`) {
		t.Fatalf("read ask: status=%d body=%s", read.Code, read.Body.String())
	}

	second := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "A second open ask", "actor": sessionActor(),
	})
	if second.Code != http.StatusCreated {
		t.Fatalf("create second ask: status=%d body=%s", second.Code, second.Body.String())
	}
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK || strings.Count(inbox.Body.String(), `"state":"open"`) != 2 {
		t.Fatalf("open asks were superseded or missing from inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}

	forbidden := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"brown"}, "actor": sessionActor(),
	})
	if forbidden.Code != http.StatusForbidden || !strings.Contains(forbidden.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("session answer: status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"brown"}, "text": "Use the existing text.",
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[struct {
		State  string `json:"state"`
		Answer struct {
			User     string   `json:"user"`
			Selected []string `json:"selected"`
		} `json:"answer"`
	}](t, answered)
	if answer.State != "answered" || answer.Answer.User != "alice" || len(answer.Answer.Selected) != 1 || answer.Answer.Selected[0] != "brown" {
		t.Fatalf("answered ask = %#v", answer)
	}
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if log.Code != http.StatusOK || !strings.Contains(log.Body.String(), `"type":"ask.answered"`) || !strings.Contains(log.Body.String(), `"notify":true`) {
		t.Fatalf("answered ask event: status=%d body=%s", log.Code, log.Body.String())
	}

	closed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"brown"},
	}, "alice")
	if closed.Code != http.StatusConflict || !strings.Contains(closed.Body.String(), `"code":"ASK_CLOSED"`) {
		t.Fatalf("answer closed ask: status=%d body=%s", closed.Code, closed.Body.String())
	}
}

func TestListIssueAsksFiltersByState(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask states", "The quick brown fox")
	otherIssue := createInteractionIssue(t, handler, "OTHER", "Unrelated issue", "A spec")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "quote": "quick"},
		"question": "Keep this answer?",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create answered ask: status=%d body=%s", created.Code, created.Body.String())
	}
	answeredAsk := decodeBody[model.Ask](t, created)

	open := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]any{"artifact": "spec", "quote": "fox"},
		"question": "Keep this open?",
		"actor":    sessionActor(),
	})
	if open.Code != http.StatusCreated {
		t.Fatalf("create open ask: status=%d body=%s", open.Code, open.Body.String())
	}
	openAsk := decodeBody[model.Ask](t, open)

	resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Still worth asking?", "actor": sessionActor(),
	})
	if resolved.Code != http.StatusCreated {
		t.Fatalf("create resolved ask: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	resolvedAsk := decodeBody[model.Ask](t, resolved)

	otherOpen := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+otherIssue.Key+"/asks", map[string]any{
		"question": "Unrelated to the first issue?", "actor": sessionActor(),
	})
	if otherOpen.Code != http.StatusCreated {
		t.Fatalf("create ask on other issue: status=%d body=%s", otherOpen.Code, otherOpen.Body.String())
	}

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answeredAsk.ID+"/answer", map[string]any{
		"text": "Yes.",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolvedAsk.ID+"/resolve", map[string]any{
		"kind": "retracted", "reason": "No longer relevant.", "actor": sessionActor(),
	}); response.Code != http.StatusOK {
		t.Fatalf("resolve ask: status=%d body=%s", response.Code, response.Body.String())
	}

	assertAsks := func(target, login string, want ...string) []model.Ask {
		t.Helper()
		var response *httptest.ResponseRecorder
		if login == "" {
			response = sessionRequest(t, handler, http.MethodGet, target, nil)
		} else {
			response = dispatchRequest(t, handler, http.MethodGet, target, nil, login)
		}
		if response.Code != http.StatusOK {
			t.Fatalf("list asks %s: status=%d body=%s", target, response.Code, response.Body.String())
		}
		got := decodeBody[[]model.Ask](t, response)
		if len(got) != len(want) {
			t.Fatalf("list asks %s = %#v, want %d asks", target, got, len(want))
		}
		byID := make(map[string]model.Ask, len(got))
		for _, ask := range got {
			byID[ask.ID] = ask
		}
		for _, id := range want {
			if _, ok := byID[id]; !ok {
				t.Fatalf("list asks %s = %#v, want to contain %q", target, got, id)
			}
		}
		return got
	}

	all := assertAsks("/api/v1/issues/"+issue.Key+"/asks?state=all", "bob", answeredAsk.ID, openAsk.ID, resolvedAsk.ID)
	for _, ask := range all {
		if ask.ID == answeredAsk.ID && (ask.State != "answered" || ask.Answer == nil || ask.Anchor == nil) {
			t.Fatalf("answered anchored ask = %#v, want persisted answer and anchor", ask)
		}
		if ask.ID == resolvedAsk.ID && (ask.State != "resolved" || ask.Resolution == nil) {
			t.Fatalf("resolved ask = %#v, want persisted resolution", ask)
		}
	}
	// state=open and state=answered match only that exact state - a resolved ask is neither.
	assertAsks("/api/v1/issues/"+issue.Key+"/asks?state=open", "bob", openAsk.ID)
	assertAsks("/api/v1/issues/"+issue.Key+"/asks?state=answered", "bob", answeredAsk.ID)
	// A bearer session (no human login) can read the list too.
	assertAsks("/api/v1/issues/"+issue.Key+"/asks?state=all", "", answeredAsk.ID, openAsk.ID, resolvedAsk.ID)

	invalid := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=bogus", nil, "bob")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_ASK_STATE"`) {
		t.Fatalf("list asks with invalid state: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestAnswerAskLocksIssueBeforeAskRow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Answer lock order", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Can this be answered?", "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)

	edit, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	defer func() { _ = edit.Rollback(context.Background()) }()
	var lockedKey string
	if err := edit.QueryRow(context.Background(), `select key from issues where key = $1 for update`, issue.Key).Scan(&lockedKey); err != nil {
		t.Fatalf("lock issue for edit: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
			"text": "Yes.",
		}, "alice")
	}()
	waitForDatabaseLocks(t, database, 1)
	if _, err := edit.Exec(context.Background(), `update asks set anchor = anchor where id = $1`, ask.ID); err != nil {
		t.Fatalf("edit anchors while answer awaits issue lock: %v", err)
	}
	if err := edit.Commit(context.Background()); err != nil {
		t.Fatalf("commit edit transaction: %v", err)
	}
	response := awaitResponse(t, responses)
	if response.Code != http.StatusOK {
		t.Fatalf("answer after edit: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClosedIssueAsksCannotBeAnsweredOrShown(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Closed ask", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Will this close?", "actor": sessionActor(),
	})
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	answer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{}, "alice")
	if answer.Code != http.StatusConflict || !strings.Contains(answer.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("answer ask on closed issue: status=%d body=%s", answer.Code, answer.Body.String())
	}
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=TEST", nil, "alice")
	if inbox.Code != http.StatusOK || strings.Contains(inbox.Body.String(), ask.ID) {
		t.Fatalf("closed issue ask appeared in inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
}

func TestAskAnchorAmbiguityAndOccurrence(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ambiguous ask", "the quick the fox")

	notFound := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Missing text", "anchor": map[string]any{"artifact": "spec", "quote": "missing"}, "actor": sessionActor(),
	})
	if notFound.Code != http.StatusNotFound || !strings.Contains(notFound.Body.String(), `"code":"TARGET_NOT_FOUND"`) {
		t.Fatalf("missing anchor target: status=%d body=%s", notFound.Code, notFound.Body.String())
	}
	ambiguous := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which the?", "anchor": map[string]any{"artifact": "spec", "quote": "the"}, "actor": sessionActor(),
	})
	if ambiguous.Code != http.StatusConflict || !strings.Contains(ambiguous.Body.String(), `"code":"TARGET_AMBIGUOUS"`) || strings.Count(ambiguous.Body.String(), `"from"`) != 2 {
		t.Fatalf("ambiguous ask: status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}

	disambiguated := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "The second one.", "anchor": map[string]any{"artifact": "spec", "quote": "the", "occurrence": 1}, "actor": sessionActor(),
	})
	if disambiguated.Code != http.StatusCreated {
		t.Fatalf("disambiguated ask: status=%d body=%s", disambiguated.Code, disambiguated.Body.String())
	}
	ask := decodeBody[struct {
		Anchor model.Anchor `json:"anchor"`
	}](t, disambiguated)
	if ask.Anchor.MarkID == "" || ask.Anchor.Quote != "the" {
		t.Fatalf("disambiguated anchor = %#v; want the on a document mark", ask.Anchor)
	}
}

func TestInteractionCapsAndAnswerValidation(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Caps", "A spec")
	tooLong := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": strings.Repeat("a", 801), "actor": sessionActor(),
	})
	if tooLong.Code != http.StatusBadRequest || !strings.Contains(tooLong.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(tooLong.Body.String(), "question") || !strings.Contains(tooLong.Body.String(), "801") || !strings.Contains(tooLong.Body.String(), "800") {
		t.Fatalf("question cap: status=%d body=%s", tooLong.Code, tooLong.Body.String())
	}
	tooManyOptions := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Options", "options": []map[string]string{{"label": "1"}, {"label": "2"}, {"label": "3"}, {"label": "4"}, {"label": "5"}, {"label": "6"}, {"label": "7"}, {"label": "8"}, {"label": "9"}}, "actor": sessionActor(),
	})
	if tooManyOptions.Code != http.StatusBadRequest || !strings.Contains(tooManyOptions.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("options cap: status=%d body=%s", tooManyOptions.Code, tooManyOptions.Body.String())
	}
	blankOption := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Blank option", "options": []map[string]string{{"label": " "}}, "actor": sessionActor(),
	})
	if blankOption.Code != http.StatusBadRequest || !strings.Contains(blankOption.Body.String(), `"code":"INVALID_ASK"`) {
		t.Fatalf("blank option label: status=%d body=%s", blankOption.Code, blankOption.Body.String())
	}
	duplicateOption := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Duplicate option", "options": []map[string]string{{"label": "yes"}, {"label": "yes"}}, "actor": sessionActor(),
	})
	if duplicateOption.Code != http.StatusBadRequest || !strings.Contains(duplicateOption.Body.String(), `"code":"INVALID_ASK"`) {
		t.Fatalf("duplicate option label: status=%d body=%s", duplicateOption.Code, duplicateOption.Body.String())
	}
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Pick", "options": []map[string]string{{"label": "yes"}}, "actor": sessionActor(),
	})
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	invalidAnswer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{"selected": []string{"no"}}, "alice")
	if invalidAnswer.Code != http.StatusBadRequest {
		t.Fatalf("invalid selected answer: status=%d body=%s", invalidAnswer.Code, invalidAnswer.Body.String())
	}
	emptyRequiredOptionAnswer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{"selected": []string{}}, "alice")
	if emptyRequiredOptionAnswer.Code != http.StatusBadRequest || !strings.Contains(emptyRequiredOptionAnswer.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("empty required-option answer: status=%d body=%s", emptyRequiredOptionAnswer.Code, emptyRequiredOptionAnswer.Body.String())
	}

	blankRequiredOptionTextAnswer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{"selected": []string{}, "text": "   "}, "alice")
	if blankRequiredOptionTextAnswer.Code != http.StatusBadRequest || !strings.Contains(blankRequiredOptionTextAnswer.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("blank required-option text answer: status=%d body=%s", blankRequiredOptionTextAnswer.Code, blankRequiredOptionTextAnswer.Body.String())
	}

	noOptionSingle := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Choose only one", "actor": sessionActor(),
	})
	noOptionAsk := decodeBody[struct {
		ID string `json:"id"`
	}](t, noOptionSingle)
	multipleSelected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+noOptionAsk.ID+"/answer", map[string]any{
		"selected": []string{"first", "second"},
	}, "alice")
	if multipleSelected.Code != http.StatusBadRequest || !strings.Contains(multipleSelected.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("multiple answers for single-select ask: status=%d body=%s", multipleSelected.Code, multipleSelected.Body.String())
	}
	emptyAnswer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+noOptionAsk.ID+"/answer", map[string]any{"selected": []string{}}, "alice")
	if emptyAnswer.Code != http.StatusBadRequest || !strings.Contains(emptyAnswer.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("empty answer: status=%d body=%s", emptyAnswer.Code, emptyAnswer.Body.String())
	}

	blankTextAnswer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+noOptionAsk.ID+"/answer", map[string]any{"selected": []string{}, "text": "   "}, "alice")
	if blankTextAnswer.Code != http.StatusBadRequest || !strings.Contains(blankTextAnswer.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("blank text answer: status=%d body=%s", blankTextAnswer.Code, blankTextAnswer.Body.String())
	}

	tooLongComment := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": strings.Repeat("a", 2001), "actor": sessionActor(),
	})
	if tooLongComment.Code != http.StatusBadRequest || !strings.Contains(tooLongComment.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(tooLongComment.Body.String(), "body") {
		t.Fatalf("comment cap: status=%d body=%s", tooLongComment.Code, tooLongComment.Body.String())
	}
	tooLongMessage := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": strings.Repeat("a", 2001), "actor": sessionActor(),
	})
	if tooLongMessage.Code != http.StatusBadRequest || !strings.Contains(tooLongMessage.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(tooLongMessage.Body.String(), "body") {
		t.Fatalf("message cap: status=%d body=%s", tooLongMessage.Code, tooLongMessage.Body.String())
	}
}

func TestAskAnswerAcceptsFreeTextAloneOrWithSelection(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Free text answers", "A spec")

	newAsk := func(question string) string {
		created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": question, "options": []map[string]string{{"label": "yes"}, {"label": "no"}}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create ask %q: status=%d body=%s", question, created.Code, created.Body.String())
		}
		ask := decodeBody[struct {
			ID string `json:"id"`
		}](t, created)
		return ask.ID
	}
	answerOf := func(response *httptest.ResponseRecorder) struct {
		Selected []string `json:"selected"`
		Text     *string  `json:"text"`
	} {
		return decodeBody[struct {
			Answer struct {
				Selected []string `json:"selected"`
				Text     *string  `json:"text"`
			} `json:"answer"`
		}](t, response).Answer
	}

	textOnlyID := newAsk("Text only")
	textOnly := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+textOnlyID+"/answer", map[string]any{
		"selected": []string{}, "text": "Neither; let's do a third thing.",
	}, "alice")
	if textOnly.Code != http.StatusOK {
		t.Fatalf("text-only answer: status=%d body=%s", textOnly.Code, textOnly.Body.String())
	}
	if answer := answerOf(textOnly); len(answer.Selected) != 0 || answer.Text == nil || *answer.Text != "Neither; let's do a third thing." {
		t.Fatalf("text-only answer = %#v", answer)
	}

	selectionOnlyID := newAsk("Selection only")
	selectionOnly := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+selectionOnlyID+"/answer", map[string]any{
		"selected": []string{"yes"},
	}, "alice")
	if selectionOnly.Code != http.StatusOK {
		t.Fatalf("selection-only answer: status=%d body=%s", selectionOnly.Code, selectionOnly.Body.String())
	}
	if answer := answerOf(selectionOnly); len(answer.Selected) != 1 || answer.Selected[0] != "yes" || answer.Text != nil {
		t.Fatalf("selection-only answer = %#v", answer)
	}

	bothID := newAsk("Selection and text")
	both := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+bothID+"/answer", map[string]any{
		"selected": []string{"no"}, "text": "Going with no, and here's why.",
	}, "alice")
	if both.Code != http.StatusOK {
		t.Fatalf("selection+text answer: status=%d body=%s", both.Code, both.Body.String())
	}
	if answer := answerOf(both); len(answer.Selected) != 1 || answer.Selected[0] != "no" || answer.Text == nil || *answer.Text != "Going with no, and here's why." {
		t.Fatalf("selection+text answer = %#v", answer)
	}

	invalidLabelID := newAsk("Invalid label stays rejected")
	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+invalidLabelID+"/answer", map[string]any{
		"selected": []string{"maybe"},
	}, "alice")
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("invalid selected label still rejected: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}

func TestAnchorsCaptureAnUnnamedVersionWhenLiveTextChanged(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() {
			if err := documentService.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Dirty document", "The quick brown fox")
	if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "The clever brown fox", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("change live document: %v", err)
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Why brown?", "anchor": map[string]any{"artifact": "spec", "quote": "brown"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask against changed document: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		Anchor model.Anchor `json:"anchor"`
	}](t, created)
	if ask.Anchor.Version != 2 || ask.Anchor.MarkID == "" || ask.Anchor.Quote != "brown" {
		t.Fatalf("anchor against changed document = %#v; want brown mark at version 2", ask.Anchor)
	}
	version := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions/2", nil, "alice")
	if version.Code != http.StatusOK || !strings.Contains(version.Body.String(), `"markdown":"The clever brown fox\n"`) || !strings.Contains(version.Body.String(), `"named":false`) {
		t.Fatalf("unnamed anchor version: status=%d body=%s", version.Code, version.Body.String())
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	var log []struct {
		Type   string `json:"type"`
		Notify bool   `json:"notify"`
	}

	if err := json.NewDecoder(events.Body).Decode(&log); err != nil {
		t.Fatalf("decode dirty document events: %v", err)
	}
	if len(log) != 3 || log[1].Type != "artifact.version" || log[1].Notify || log[2].Type != "ask.opened" || !log[2].Notify {
		t.Fatalf("dirty document events = %#v; want non-notifying unnamed artifact.version before notifying ask.opened", log)
	}
}

func TestDocumentEditMapsMissingAndAmbiguousTargets(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Document edits", "same same")
	ambiguous := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "same", "with": "changed"}},
	}, "alice")
	if ambiguous.Code != http.StatusConflict || !strings.Contains(ambiguous.Body.String(), `"code":"TARGET_AMBIGUOUS"`) {
		t.Fatalf("ambiguous edit: status=%d body=%s", ambiguous.Code, ambiguous.Body.String())
	}
	missing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "missing", "with": "changed"}},
	}, "alice")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"TARGET_NOT_FOUND"`) {
		t.Fatalf("missing edit: status=%d body=%s", missing.Code, missing.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"same same\n"`) {
		t.Fatalf("failed edit changed document: status=%d body=%s", text.Code, text.Body.String())
	}
}

func TestDocumentEditWithoutSummaryWritesUnnamedVersionAndEvent(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Unnamed document edit", "before")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}}, "actor": sessionActor(),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("edit document without summary: status=%d body=%s", response.Code, response.Body.String())
	}
	var versions int
	var unnamed bool
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), coalesce(bool_and(not named), false)
		from artifact_versions where artifact_id = $1 and number = 2
	`, issue.PrimaryArtifactID).Scan(&versions, &unnamed); err != nil {
		t.Fatalf("count unnamed versions: %v", err)
	}
	if versions != 1 || !unnamed {
		t.Fatalf("summary-less edit versions = %d unnamed=%t, want one unnamed version", versions, unnamed)
	}
	var events int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where issue_key = $1 and type = 'artifact.version'
	`, issue.Key).Scan(&events); err != nil {
		t.Fatalf("count unnamed version events: %v", err)
	}
	if events != 1 {
		t.Fatalf("summary-less edit artifact.version events = %d, want 1", events)
	}
}

func TestAnchorsKeepCleanDocumentAtExistingVersion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Clean document", "The quick brown fox")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Why brown?", "anchor": map[string]any{"artifact": "spec", "quote": "brown"}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask against unchanged document: status=%d body=%s", created.Code, created.Body.String())
	}
	version := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions/2", nil, "alice")
	if version.Code != http.StatusNotFound {
		t.Fatalf("unexpected snapshot for unchanged document: status=%d body=%s", version.Code, version.Body.String())
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	var cleanLog []struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(events.Body).Decode(&cleanLog); err != nil {
		t.Fatalf("decode clean document events: %v", err)
	}
	if len(cleanLog) != 2 || cleanLog[0].Type != "issue.created" || cleanLog[1].Type != "ask.opened" {
		t.Fatalf("clean document events = %#v; want issue.created then ask.opened", cleanLog)
	}
}
func TestCommentsSuggestionsAndArtifactFilter(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Comments", "The quick brown fox")
	first := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Why brown?", "anchor": map[string]any{"artifact": "spec", "quote": "brown"}, "actor": sessionActor(),
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("create anchored comment: status=%d body=%s", first.Code, first.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, first)
	reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Because that is the selected word.", "reply_to": comment.ID,
	}, "alice")
	if reply.Code != http.StatusCreated || !strings.Contains(reply.Body.String(), `"reply_to":"`+comment.ID+`"`) {
		t.Fatalf("create reply: status=%d body=%s", reply.Code, reply.Body.String())
	}
	floating := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]string{"body": "Floating comment"}, "alice")

	resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", map[string]any{"actor": sessionActor()})
	if resolved.Code != http.StatusOK || !strings.Contains(resolved.Body.String(), `"resolved":true`) {
		t.Fatalf("session resolves comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	if projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID); projection["resolved"] != true || len(projection["replies"].([]any)) != 1 {
		t.Fatalf("resolved comment projection = %#v, want resolved with one reply", projection)
	}
	if floating.Code != http.StatusCreated {
		t.Fatalf("create floating comment: status=%d body=%s", floating.Code, floating.Body.String())
	}

	filtered := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments?artifact="+issue.PrimaryArtifactID, nil, "alice")
	if filtered.Code != http.StatusOK {
		t.Fatalf("filter anchored comments: status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	var anchored []model.Comment
	if err := json.NewDecoder(filtered.Body).Decode(&anchored); err != nil {
		t.Fatalf("decode filtered comments: %v", err)
	}
	if len(anchored) != 1 || anchored[0].ID != comment.ID {
		t.Fatalf("filtered comments = %#v; want only %s", anchored, comment.ID)
	}

	suggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Use red.", "anchor": map[string]any{"artifact": "spec", "quote": "brown"}, "suggestion": map[string]string{"replace_with": "red"}, "actor": sessionActor(),
	})
	if suggestion.Code != http.StatusCreated {
		t.Fatalf("create suggestion: status=%d body=%s", suggestion.Code, suggestion.Body.String())
	}
	suggested := decodeBody[model.Comment](t, suggestion)
	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+suggested.ID+"/reject", map[string]any{}, "alice")
	if rejected.Code != http.StatusOK || !strings.Contains(rejected.Body.String(), `"resolved":true`) || !strings.Contains(rejected.Body.String(), `"accepted":false`) {
		t.Fatalf("reject suggestion: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	rejectedStored := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+suggested.ID, nil, "alice")
	if rejectedStored.Code != http.StatusOK {
		t.Fatalf("read rejected suggestion: status=%d body=%s", rejectedStored.Code, rejectedStored.Body.String())
	}
	if stored := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, rejectedStored).Comment; stored.Anchor == nil || stored.Anchor.Orphaned {
		t.Fatalf("rejected suggestion anchor = %#v, want retained non-orphaned anchor", stored.Anchor)
	}
	if markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || markdown != "The quick brown fox\n" {
		t.Fatalf("rejected suggestion text = %q (%v), want unchanged document", markdown, err)
	}
	if projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, suggested.ID); projection["status"] != "rejected" {
		t.Fatalf("rejected suggestion projection = %#v", projection)
	}

	floatingSuggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "An unanchored suggestion can be rejected.", "suggestion": map[string]string{"replace_with": "ignored"}, "actor": sessionActor(),
	})
	if floatingSuggestion.Code != http.StatusCreated {
		t.Fatalf("create floating suggestion: status=%d body=%s", floatingSuggestion.Code, floatingSuggestion.Body.String())
	}
	floatingValue := decodeBody[struct {
		ID string `json:"id"`
	}](t, floatingSuggestion)
	floatingRejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+floatingValue.ID+"/reject", map[string]any{}, "alice")
	if floatingRejected.Code != http.StatusOK || !strings.Contains(floatingRejected.Body.String(), `"accepted":false`) {
		t.Fatalf("reject floating suggestion: status=%d body=%s", floatingRejected.Code, floatingRejected.Body.String())
	}
}

func TestSuggestionAcceptAppliesLiveDocument(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() {
			if err := documentService.Shutdown(context.Background()); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Accept suggestion", "The quick brown fox")

	suggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Use red.", "anchor": map[string]any{"artifact": "spec", "quote": "brown"}, "suggestion": map[string]string{"replace_with": "red"}, "actor": sessionActor(),
	})
	comment := decodeBody[model.Comment](t, suggestion)
	forbidden := sessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{"actor": sessionActor()})
	if forbidden.Code != http.StatusForbidden || !strings.Contains(forbidden.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("session accepts suggestion: status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), `"resolved":true`) || !strings.Contains(accepted.Body.String(), `"accepted":true`) {
		t.Fatalf("accept suggestion: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	acceptedStored := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if acceptedStored.Code != http.StatusOK {
		t.Fatalf("read accepted suggestion: status=%d body=%s", acceptedStored.Code, acceptedStored.Body.String())
	}
	if stored := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, acceptedStored).Comment; stored.Anchor == nil || stored.Anchor.Orphaned {
		t.Fatalf("accepted suggestion anchor = %#v, want retained non-orphaned anchor", stored.Anchor)
	}
	markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read accepted document: %v", err)
	}
	if markdown != "The quick red fox\n" {
		t.Fatalf("accepted document = %q; want replacement applied", markdown)
	}
	if _, err := documentService.VerifyMark(context.Background(), issue.PrimaryArtifactID, docs.MarkSuggestion, comment.Anchor.MarkID); !errors.Is(err, docs.ErrAnchorMissing) {
		t.Fatalf("accepted suggestion mark = %v, want missing", err)
	}
	if projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID); projection["status"] != "accepted" {
		t.Fatalf("accepted suggestion projection = %#v", projection)
	}
	repeated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if repeated.Code != http.StatusConflict || !strings.Contains(repeated.Body.String(), `"code":"ALREADY_ACTIONED"`) {
		t.Fatalf("repeat accept: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
}

func TestAcceptOrphanedSuggestionIs409(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Orphaned suggestion", "The quick brown fox")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Use red.", "anchor": map[string]any{"artifact": "spec", "quote": "brown"}, "suggestion": map[string]string{"replace_with": "red"}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "The quick fox", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("delete suggestion mark text: %v", err)
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)

	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusConflict || !strings.Contains(accepted.Body.String(), `"code":"ANCHOR_ORPHANED"`) {
		t.Fatalf("accept orphaned suggestion: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read orphaned suggestion: status=%d body=%s", read.Code, read.Body.String())
	}
	loaded := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, read)
	if loaded.Comment.Anchor == nil || !loaded.Comment.Anchor.Orphaned {
		t.Fatalf("orphaned suggestion = %#v, want orphaned anchor", loaded.Comment)
	}
}

func waitForArtifactVersion(t *testing.T, handler http.Handler, artifactID string, number int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		version := dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", artifactID, number), nil, "alice")
		if version.Code == http.StatusOK {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("artifact version %d did not settle", number)
}

func TestSuggestionAcceptClearsPendingAuthorBeforeNextVersion(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Accepted author", "before")
	suggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Replace before.", "anchor": map[string]any{"artifact": "spec", "quote": "before"},
		"suggestion": map[string]string{"replace_with": "after"}, "actor": sessionActor(),
	})
	if suggestion.Code != http.StatusCreated {
		t.Fatalf("create suggestion: status=%d body=%s", suggestion.Code, suggestion.Body.String())
	}
	comment := decodeBody[model.Comment](t, suggestion)
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept suggestion: status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	next := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "after", "with": "next"}},
		"summary": "Next version",
		"actor":   sessionActor(),
	})
	if next.Code != http.StatusOK {
		t.Fatalf("create next version: status=%d body=%s", next.Code, next.Body.String())
	}
	result := decodeBody[struct {
		Version *model.Version `json:"version"`
	}](t, next)
	if result.Version == nil || len(result.Version.Authors) != 1 || result.Version.Authors[0] != (model.Actor{Kind: "session", ID: "session-0123456789abcdef"}) {
		t.Fatalf("next version authors = %#v, want only the next session editor", result.Version)
	}
}

func TestConcurrentSuggestionAcceptAppliesReplacementExactlyOnce(t *testing.T) {
	var documentService *docs.Service
	var gate *firstReplaceGate
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		gate = &firstReplaceGate{
			API: documentService, firstEntered: make(chan struct{}), releaseFirst: make(chan struct{}), secondEntered: make(chan struct{}),
		}
		return gate
	})
	issue := createInteractionIssue(t, handler, "TEST", "Concurrent accepts", "foo")
	suggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Replace foo.", "anchor": map[string]any{"artifact": "spec", "quote": "foo"},
		"suggestion": map[string]string{"replace_with": "foo2"}, "actor": sessionActor(),
	})
	commentID := decodeBody[struct {
		ID string `json:"id"`
	}](t, suggestion).ID

	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/accept", map[string]any{}, "alice")
	}()
	select {
	case <-gate.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first acceptance did not reach the document replacement")
	}
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/accept", map[string]any{}, "alice")
	}()
	waitForSecondReplacementOrCommentLock(t, database, gate.secondEntered)
	close(gate.releaseFirst)

	var success, conflict int
	for range 2 {
		select {
		case response := <-responses:
			switch response.Code {
			case http.StatusOK:
				success++
			case http.StatusConflict:
				if strings.Contains(response.Body.String(), `"code":"ALREADY_ACTIONED"`) {
					conflict++
				}
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent acceptance did not complete")
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("acceptance statuses = success:%d conflict:%d, want one each", success, conflict)
	}
	markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read document after concurrent acceptance: %v", err)
	}
	if markdown != "foo2\n" {
		t.Fatalf("document after concurrent acceptance = %q, want foo2", markdown)
	}
}

func TestSuggestionAcceptChecksClosureBeforeApplyingReplacement(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Closed suggestion", "foo")
	suggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Replace foo.", "anchor": map[string]any{"artifact": "spec", "quote": "foo"},
		"suggestion": map[string]string{"replace_with": "foo2"}, "actor": sessionActor(),
	})
	commentID := decodeBody[struct {
		ID string `json:"id"`
	}](t, suggestion).ID

	blocker, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin issue blocker: %v", err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(context.Background(), `select 1 from issues where key = $1 for update`, issue.Key); err != nil {
		t.Fatalf("lock issue: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/accept", map[string]any{}, "alice")
	}()
	waitForSecondReplacementOrCommentLock(t, database, make(chan struct{}))
	if _, err := blocker.Exec(context.Background(), `update issues set status = 'done', closed_at = now() where key = $1`, issue.Key); err != nil {
		t.Fatalf("close locked issue: %v", err)
	}
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatalf("commit closed issue: %v", err)
	}
	response := <-responses
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("accept suggestion after close: status=%d body=%s", response.Code, response.Body.String())
	}
	markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read closed document: %v", err)
	}
	if markdown != "foo\n" {
		t.Fatalf("closed suggestion changed document to %q, want foo", markdown)
	}
}

func TestInboxOrdersOpenAsksAcrossIssues(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	first := createInteractionIssue(t, handler, "TEST", "First", "first")
	firstAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+first.Key+"/asks", map[string]any{"question": "First question", "actor": sessionActor()})
	if firstAsk.Code != http.StatusCreated {
		t.Fatalf("create first ask: status=%d body=%s", firstAsk.Code, firstAsk.Body.String())
	}
	firstAskBody := decodeBody[struct {
		ID string `json:"id"`
	}](t, firstAsk)
	if _, err := database.Pool.Exec(context.Background(), `update asks set created_at = created_at - interval '1 minute' where id = $1`, firstAskBody.ID); err != nil {
		t.Fatalf("move first ask earlier: %v", err)
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Second", "spec": "second"}, "alice")
	second := decodeBody[struct {
		Key string `json:"key"`
	}](t, response)
	secondAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+second.Key+"/asks", map[string]any{"question": "Second question", "actor": sessionActor()})
	if secondAsk.Code != http.StatusCreated {
		t.Fatalf("create second ask: status=%d body=%s", secondAsk.Code, secondAsk.Body.String())
	}
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=TEST", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	var asks []struct {
		Question string `json:"question"`
		Issue    struct {
			Key   string `json:"key"`
			Title string `json:"title"`
		} `json:"issue"`
	}
	if err := json.NewDecoder(inbox.Body).Decode(&asks); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if len(asks) != 2 || asks[0].Question != "Second question" || asks[0].Issue.Key != second.Key || asks[0].Issue.Title != "Second" {
		t.Fatalf("inbox ordering and embedded issue = %#v", asks)
	}
}

func TestReferencesAndSessionMessageNotify(t *testing.T) {
	handler, database := newInteractionHandler(t, nil)
	issue := createInteractionIssue(t, handler, "TEST", "References", "The quick brown fox")
	ask := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{"question": "Ask target dispatch://" + issue.Key + "/spec", "actor": sessionActor()})
	askValue := decodeBody[struct {
		ID string `json:"id"`
	}](t, ask)
	body := "see dispatch://" + issue.Key + "/spec and https://dispatch.example/issues/" + issue.Key + "/asks/" + askValue.ID
	comment := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": body, "actor": sessionActor()})
	commentValue := decodeBody[struct {
		ID string `json:"id"`
	}](t, comment)
	rows, err := database.Pool.Query(context.Background(), `select to_kind, to_id from refs where from_kind = 'comment' and from_id = $1 order by to_kind`, commentValue.ID)
	if err != nil {
		t.Fatalf("read comment refs: %v", err)
	}
	defer rows.Close()
	targets := map[string]string{}
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			t.Fatalf("scan comment ref: %v", err)
		}
		targets[kind] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate comment refs: %v", err)
	}
	if len(targets) != 2 || targets["artifact"] != issue.Key+"/spec" || targets["ask"] != askValue.ID {
		t.Fatalf("comment reference targets = %#v", targets)
	}
	artifact := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID, nil, "alice")
	if artifact.Code != http.StatusOK || !strings.Contains(artifact.Body.String(), `"referenced_by"`) || !strings.Contains(artifact.Body.String(), `"kind":"comment"`) || !strings.Contains(artifact.Body.String(), commentValue.ID) {
		t.Fatalf("artifact references: status=%d body=%s", artifact.Code, artifact.Body.String())
	}
	message := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "A session message referencing dispatch://" + issue.Key + "/spec", "actor": sessionActor(),
	})
	if message.Code != http.StatusCreated {
		t.Fatalf("session message: status=%d body=%s", message.Code, message.Body.String())
	}
	if !strings.Contains(message.Body.String(), `"body":"A session message`) {
		t.Fatalf("session message response: %s", message.Body.String())
	}

	var artifactRefSources int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from refs where to_kind = 'artifact' and to_id = $1`, issue.Key+"/spec").Scan(&artifactRefSources); err != nil {
		t.Fatalf("count artifact reference sources: %v", err)
	}
	if artifactRefSources != 3 {
		t.Fatalf("artifact reference sources = %d; want ask, comment, and message", artifactRefSources)
	}
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if log.Code != http.StatusOK || !strings.Contains(log.Body.String(), `"type":"message.created"`) || !strings.Contains(log.Body.String(), `"notify":false`) {
		t.Fatalf("session message event: status=%d body=%s", log.Code, log.Body.String())
	}
}

func TestCommentReplyMustBelongToItsIssue(t *testing.T) {
	handler := newTestHandler(t)
	first := createInteractionIssue(t, handler, "TEST", "First", "first")
	comment := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+first.Key+"/comments", map[string]any{"body": "First", "actor": sessionActor()})
	firstComment := decodeBody[struct {
		ID string `json:"id"`
	}](t, comment)
	secondResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Second", "spec": "second"}, "alice")
	second := decodeBody[struct {
		Key string `json:"key"`
	}](t, secondResponse)
	invalid := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+second.Key+"/comments", map[string]string{"body": "Wrong issue", "reply_to": firstComment.ID}, "alice")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("cross-issue reply: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestAskThreadRepliesStoredAndReturnedInOrder(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask thread", "before")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which approach?", "actor": sessionActor(),
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, askResponse)

	reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "I think option A.", "ask_id": ask.ID,
	}, "alice")
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply to ask: status=%d body=%s", reply.Code, reply.Body.String())
	}
	firstReply := decodeBody[model.Comment](t, reply)
	if firstReply.AskID == nil || *firstReply.AskID != ask.ID {
		t.Fatalf("reply ask_id = %#v, want %q", firstReply.AskID, ask.ID)
	}

	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if !strings.Contains(log.Body.String(), `"ask_question":"Which approach?"`) {
		t.Fatalf("comment.created event log = %s, want ask_question in the payload", log.Body.String())
	}

	nestedReply := model.Comment{
		IssueKey: issue.Key,
		Author:   model.Actor{Kind: "session", ID: "session-0123456789abcdef"},
		Body:     "Why option A?",
		ReplyTo:  &firstReply.ID,
	}
	author, err := json.Marshal(nestedReply.Author)
	if err != nil {
		t.Fatalf("encode nested ask reply author: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, reply_to)
		values ($1, $2, $3, $4)
		returning id::text, created_at
	`, nestedReply.IssueKey, author, nestedReply.Body, nestedReply.ReplyTo).Scan(&nestedReply.ID, &nestedReply.CreatedAt); err != nil {
		t.Fatalf("seed nested ask reply: %v", err)
	}

	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Because it's simpler.", "ask_id": ask.ID,
	}, "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("second reply to ask: status=%d body=%s", second.Code, second.Body.String())
	}
	secondReply := decodeBody[model.Comment](t, second)

	base := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	for index, comment := range []model.Comment{firstReply, nestedReply, secondReply} {
		if _, err := database.Pool.Exec(context.Background(), `
			update comments set created_at = $2 where id = $1
		`, comment.ID, base.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatalf("set reply timestamp: %v", err)
		}
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask thread: status=%d body=%s", read.Code, read.Body.String())
	}
	thread := decodeBody[struct {
		Ask     model.Ask       `json:"ask"`
		Replies []model.Comment `json:"replies"`
	}](t, read)
	if thread.Ask.ID != ask.ID {
		t.Fatalf("thread ask = %q, want %q", thread.Ask.ID, ask.ID)
	}
	if len(thread.Replies) != 3 {
		t.Fatalf("thread replies = %#v, want three replies", thread.Replies)
	}
	for index, want := range []string{firstReply.ID, nestedReply.ID, secondReply.ID} {
		if thread.Replies[index].ID != want {
			t.Fatalf("thread reply %d = %q, want %q", index, thread.Replies[index].ID, want)
		}
	}
}

func TestAskReplyMustBelongToItsIssue(t *testing.T) {
	handler := newTestHandler(t)
	first := createInteractionIssue(t, handler, "TEST", "First", "first")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+first.Key+"/asks", map[string]any{
		"question": "Ship it?", "actor": sessionActor(),
	})
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, askResponse)
	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Second", "spec": "second",
	}, "alice")
	secondIssue := decodeBody[struct {
		Key string `json:"key"`
	}](t, second)

	invalid := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+secondIssue.Key+"/comments", map[string]string{
		"body": "Wrong issue", "ask_id": ask.ID,
	}, "alice")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_COMMENT"`) {
		t.Fatalf("cross-issue ask reply: status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	both := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+first.Key+"/comments", map[string]any{
		"body": "Ambiguous parent", "ask_id": ask.ID, "reply_to": ask.ID,
	}, "alice")
	if both.Code != http.StatusBadRequest || !strings.Contains(both.Body.String(), `"code":"INVALID_COMMENT"`) {
		t.Fatalf("reply_to and ask_id together: status=%d body=%s", both.Code, both.Body.String())
	}
}

func TestAskReplyOnClosedIssueIsRefused(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Closing soon", "before")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Ship it?", "actor": sessionActor(),
	})
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, askResponse)
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Too late?", "ask_id": ask.ID,
	}, "alice")
	if reply.Code != http.StatusConflict || !strings.Contains(reply.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("reply on closed issue: status=%d body=%s", reply.Code, reply.Body.String())
	}
}

func TestDoneIssueRejectsEveryMutation(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Done issue", "before")
	ask := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Open ask", "actor": sessionActor(),
	})
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, ask).ID
	comment := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Open comment", "actor": sessionActor(),
	})
	commentID := decodeBody[struct {
		ID string `json:"id"`
	}](t, comment).ID
	suggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Replace before.", "anchor": map[string]any{"artifact": "spec", "quote": "before"},
		"suggestion": map[string]string{"replace_with": "after"}, "actor": sessionActor(),
	})
	suggestionID := decodeBody[struct {
		ID string `json:"id"`
	}](t, suggestion).ID
	secondSuggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Reject this.", "suggestion": map[string]string{"replace_with": "ignored"}, "actor": sessionActor(),
	})
	secondSuggestionID := decodeBody[struct {
		ID string `json:"id"`
	}](t, secondSuggestion).ID
	issueDetail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	artifactList := decodeBody[struct {
		Artifacts []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"artifacts"`
	}](t, issueDetail)
	var primarySlug string
	for _, artifact := range artifactList.Artifacts {
		if artifact.ID == issue.PrimaryArtifactID {
			primarySlug = artifact.Slug
			break
		}
	}
	if primarySlug == "" {
		t.Fatal("resolve primary artifact slug")
	}

	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	var closedIssue struct {
		Status   string     `json:"status"`
		ClosedAt *time.Time `json:"closed_at"`
	}
	if closed.Code != http.StatusOK {
		t.Fatalf("set done: status=%d body=%s", closed.Code, closed.Body.String())
	}
	closedIssue = decodeBody[struct {
		Status   string     `json:"status"`
		ClosedAt *time.Time `json:"closed_at"`
	}](t, closed)
	if closedIssue.Status != "done" || closedIssue.ClosedAt == nil {
		t.Fatalf("done issue = %#v, want status done with closed_at", closedIssue)
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if !strings.Contains(events.Body.String(), `"type":"issue.closed"`) {
		t.Fatalf("done event log = %s, want issue.closed", events.Body.String())
	}

	assertClosed := func(name string, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ISSUE_CLOSED"`) {
			t.Fatalf("%s after done: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	assertClosed("issue title", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Nope"}, "alice"))
	assertClosed("ask", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Later ask", "actor": sessionActor(),
	}))
	assertClosed("ask answer", dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{}, "alice"))
	assertClosed("comment", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Later comment", "actor": sessionActor(),
	}))
	assertClosed("comment resolution", sessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/resolve", map[string]any{
		"actor": sessionActor(),
	}))
	assertClosed("suggestion acceptance", dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+suggestionID+"/accept", map[string]any{}, "alice"))
	assertClosed("suggestion rejection", dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+secondSuggestionID+"/reject", map[string]any{}, "alice"))
	assertClosed("message", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Later message", "actor": sessionActor(),
	}))
	assertClosed("upload", multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "later.md",
	}, "later.md", "text/markdown", []byte("later"), "alice"))
	assertClosed("document edit", sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}}, "actor": sessionActor(),
	}))
	assertClosed("named version", sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]any{
		"summary": "checkpoint", "actor": sessionActor(),
	}))
	assertClosed("document edit (slug)", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/"+primarySlug+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}}, "actor": sessionActor(),
	}))
	assertClosed("named version (slug)", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts/"+primarySlug+"/versions", map[string]any{
		"summary": "checkpoint", "actor": sessionActor(),
	}))

	reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice")
	if reopened.Code != http.StatusOK || strings.Contains(reopened.Body.String(), `"closed_at":`) && !strings.Contains(reopened.Body.String(), `"closed_at":null`) {
		t.Fatalf("reopen done issue: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Reopened ask", "actor": sessionActor(),
	}); created.Code != http.StatusCreated {
		t.Fatalf("ask after reopen: status=%d body=%s", created.Code, created.Body.String())
	}
	if invalid := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "closed"}, "alice"); invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_STATUS"`) {
		t.Fatalf("closed is not a valid lifecycle status: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestEditArtifactCreatesNamedVersion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Edit version", "before")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "before", "with": "after"}},
		"summary": "Change opening",
		"actor":   sessionActor(),
	})
	var result struct {
		Applied int           `json:"applied"`
		Version model.Version `json:"version"`
	}
	if edited.Code != http.StatusOK {
		t.Fatalf("edit document: status=%d body=%s", edited.Code, edited.Body.String())
	}
	result = decodeBody[struct {
		Applied int           `json:"applied"`
		Version model.Version `json:"version"`
	}](t, edited)
	if result.Applied != 1 || !result.Version.Named || result.Version.Summary == nil || *result.Version.Summary != "Change opening" {
		t.Fatalf("edit result = %#v, want named version with summary", result)
	}
	unchanged := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{}, "actor": sessionActor(),
	})
	if unchanged.Code != http.StatusOK || !strings.Contains(unchanged.Body.String(), `"applied":0`) || !strings.Contains(unchanged.Body.String(), `"version":null`) {
		t.Fatalf("empty edit result: status=%d body=%s", unchanged.Code, unchanged.Body.String())
	}
}

func TestEditArtifactWithoutSummaryReturnsUnnamedVersion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Unversioned edit", "before")
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": "before", "with": "after"}},
		"actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit document: status=%d body=%s", edited.Code, edited.Body.String())
	}
	result := decodeBody[struct {
		Applied int            `json:"applied"`
		Version *model.Version `json:"version"`
	}](t, edited)
	if result.Applied != 1 || result.Version == nil || result.Version.Named || result.Version.Summary != nil {
		t.Fatalf("edit result = %#v, want one applied operation and an unnamed version", result)
	}
}

func TestDoneIssueAllowsOnlyAStatusReopen(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Strict reopen", "before")
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("set done: status=%d body=%s", closed.Code, closed.Body.String())
	}
	combined := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"status": "todo", "title": "Reopened with a change",
	}, "alice")
	if combined.Code != http.StatusConflict || !strings.Contains(combined.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("combined reopen and mutation: status=%d body=%s, want ISSUE_CLOSED", combined.Code, combined.Body.String())
	}
}

func TestNamedArtifactVersionEventCarriesUnifiedDiff(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Version diff", "before")
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "before", "with": "after"}},
		"summary": "Change opening",
		"actor":   sessionActor(),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("write named version: status=%d body=%s", response.Code, response.Body.String())
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list version events: status=%d body=%s", events.Code, events.Body.String())
	}
	var log []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(events.Body).Decode(&log); err != nil {
		t.Fatalf("decode version events: %v", err)
	}
	for _, event := range log {
		if event.Type != "artifact.version" {
			continue
		}
		var payload struct {
			Diff *string `json:"diff"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode version event payload: %v", err)
		}
		want := "--- previous\n+++ current\n@@ -1 +1 @@\n-before\n+after\n"
		if payload.Diff == nil || *payload.Diff != want {
			t.Fatalf("version diff = %#v, want %q", payload.Diff, want)
		}
		return
	}
	t.Fatalf("artifact.version event not found in %#v", log)
}

func TestSuggestionAllowsEmptyBodyWithoutRelaxingCommentValidation(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Empty suggestion", "before")
	suggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"suggestion": map[string]string{"replace_with": "after"},
		"actor":      sessionActor(),
	})
	if suggestion.Code != http.StatusCreated {
		t.Fatalf("create empty-body suggestion: status=%d body=%s", suggestion.Code, suggestion.Body.String())
	}
	comment := decodeBody[model.Comment](t, suggestion)
	if comment.Body != "" {
		t.Fatalf("empty-body suggestion body = %q, want empty", comment.Body)
	}
	ordinary := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": " ", "actor": sessionActor(),
	})
	if ordinary.Code != http.StatusBadRequest || !strings.Contains(ordinary.Body.String(), `"code":"INVALID_COMMENT"`) {
		t.Fatalf("empty ordinary comment: status=%d body=%s", ordinary.Code, ordinary.Body.String())
	}
}

func TestGetCommentReturnsReplyChain(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Comment thread", "before")
	createComment := func(body string, replyTo *string) model.Comment {
		t.Helper()
		input := map[string]any{"body": body, "actor": sessionActor()}
		if replyTo != nil {
			input["reply_to"] = *replyTo
		}
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", input)
		if response.Code != http.StatusCreated {
			t.Fatalf("create comment %q: status=%d body=%s", body, response.Code, response.Body.String())
		}
		return decodeBody[model.Comment](t, response)
	}
	root := createComment("root", nil)
	first := createComment("first reply", &root.ID)
	nested := model.Comment{
		IssueKey: issue.Key,
		Author:   model.Actor{Kind: "session", ID: "session-0123456789abcdef"},
		Body:     "nested reply",
		ReplyTo:  &first.ID,
	}
	author, err := json.Marshal(nested.Author)
	if err != nil {
		t.Fatalf("encode nested reply author: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, reply_to)
		values ($1, $2, $3, $4)
		returning id::text, created_at
	`, nested.IssueKey, author, nested.Body, nested.ReplyTo).Scan(&nested.ID, &nested.CreatedAt); err != nil {
		t.Fatalf("seed nested reply: %v", err)
	}
	second := createComment("second reply", &root.ID)
	base := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	for index, comment := range []model.Comment{root, first, nested, second} {
		if _, err := database.Pool.Exec(context.Background(), `
			update comments set created_at = $2 where id = $1
		`, comment.ID, base.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatalf("set comment timestamp: %v", err)
		}
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+root.ID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read comment thread: status=%d body=%s", response.Code, response.Body.String())
	}
	thread := decodeBody[struct {
		Comment model.Comment   `json:"comment"`
		Replies []model.Comment `json:"replies"`
	}](t, response)
	if thread.Comment.ID != root.ID {
		t.Fatalf("thread root = %q, want %q", thread.Comment.ID, root.ID)
	}
	if len(thread.Replies) != 3 {
		t.Fatalf("thread replies = %#v, want three replies", thread.Replies)
	}
	for index, want := range []string{first.ID, nested.ID, second.ID} {
		if thread.Replies[index].ID != want {
			t.Fatalf("thread reply %d = %q, want %q", index, thread.Replies[index].ID, want)
		}
	}
}

func TestGetCommentReturnsNotFound(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/00000000-0000-0000-0000-000000000000", nil, "alice")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"NOT_FOUND"`) {
		t.Fatalf("read absent comment: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEditArtifactRollbackEvictsLiveDocument(t *testing.T) {
	const settleInterval = 50 * time.Millisecond
	var documentService *docs.Service
	var failure *postApplyFailureDocs
	var persistenceStore *recordingVersionedStore
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		persistenceStore = &recordingVersionedStore{
			VersionedStore: docs.NewPgVersioned(database),
			updates:        make(chan struct{}, 2),
		}
		documentService = docs.New(docs.Deps{
			Store:       database,
			Persistence: persistenceStore,
			Identity: identity.HeaderIdentity{
				Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}},
			},
			Settle: settleInterval,
		})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		failure = &postApplyFailureDocs{
			API:                documentService,
			beforeApply:        make(chan struct{}),
			releaseBeforeApply: make(chan struct{}),
			beforeEvict:        make(chan struct{}),
			releaseEvict:       make(chan struct{}),
			applied:            make(chan struct{}),
			release:            make(chan struct{}),
		}
		return failure
	})
	issue := createInteractionIssue(t, handler, "TEST", "Transactional edit", "before")
	documentServer := httptest.NewServer(http.HandlerFunc(documentService.ServeHTTP))
	t.Cleanup(documentServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(documentServer.URL, "http") + "/ws/doc/" + issue.PrimaryArtifactID
	connection, wsResponse, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", wsResponse, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	drainDocumentUpdates(persistenceStore)
	responses := make(chan *httptest.ResponseRecorder, 1)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		responses <- sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}}, "actor": sessionActor(),
		})
	}()
	// Release every test gate before connection, server, and service cleanup. Otherwise a
	// failed assertion can leave this request holding its transaction open indefinitely.
	t.Cleanup(func() {
		closeTestGate(failure.releaseBeforeApply)
		closeTestGate(failure.release)
		closeTestGate(failure.releaseEvict)
		select {
		case <-handlerDone:
		case <-time.After(5 * time.Second):
			t.Error("transactional edit did not exit after releasing test gates")
		}
	})
	waitForBeforeApply(t, failure)
	if _, err := documentService.ApplyOps(context.Background(), issue.PrimaryArtifactID, []model.EditOp{{Op: "replace", Find: "before", With: "before"}}, model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("apply live update before transactional edit: %v", err)
	}
	waitForDocumentUpdate(t, persistenceStore)
	waitForSecondReplacementOrCommentLock(t, database, make(chan struct{}))
	assertArtifactKeyShareLockAvailable(t, database, issue.PrimaryArtifactID)
	closeTestGate(failure.releaseBeforeApply)
	waitForPostApply(t, failure)
	waitForDocumentUpdate(t, persistenceStore)
	assertNoSettledDocumentVersion(t, database, issue.PrimaryArtifactID, settleInterval)
	closeTestGate(failure.release)
	waitForBeforeEvict(t, failure)
	assertNoSettledDocumentVersion(t, database, issue.PrimaryArtifactID, settleInterval)
	closeTestGate(failure.releaseEvict)
	handlerResponse := awaitResponse(t, responses)
	if handlerResponse.Code != http.StatusInternalServerError {
		t.Fatalf("edit with forced post-apply failure: status=%d body=%s", handlerResponse.Code, handlerResponse.Body.String())
	}
	assertHandlerDocumentText(t, handler, issue.PrimaryArtifactID, "before\n")
	assertNoSettledDocumentVersion(t, database, issue.PrimaryArtifactID, settleInterval)
	waitForDocumentConnectionClose(t, connection)
	reconnected, wsResponse, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("reconnect evicted document: response=%#v err=%v", wsResponse, err)
	}
	t.Cleanup(func() { _ = reconnected.Close() })
	assertHandlerDocumentText(t, handler, issue.PrimaryArtifactID, "before\n")
}

func TestSuggestionAcceptRollbackEvictsLiveDocument(t *testing.T) {
	const settleInterval = 50 * time.Millisecond
	var documentService *docs.Service
	var failure *postApplyFailureDocs
	var persistenceStore *recordingVersionedStore
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		persistenceStore = &recordingVersionedStore{
			VersionedStore: docs.NewPgVersioned(database),
			updates:        make(chan struct{}, 2),
		}
		documentService = docs.New(docs.Deps{
			Store:       database,
			Persistence: persistenceStore,
			Identity: identity.HeaderIdentity{
				Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}},
			},
			Settle: settleInterval,
		})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		failure = &postApplyFailureDocs{
			API: documentService, applied: make(chan struct{}), release: make(chan struct{}),
		}
		return failure
	})
	issue := createInteractionIssue(t, handler, "TEST", "Transactional suggestion", "before")
	documentServer := httptest.NewServer(http.HandlerFunc(documentService.ServeHTTP))
	t.Cleanup(documentServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(documentServer.URL, "http") + "/ws/doc/" + issue.PrimaryArtifactID
	connection, wsResponse, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", wsResponse, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	drainDocumentUpdates(persistenceStore)
	suggestion := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":       "replace it",
		"anchor":     map[string]any{"artifact": "spec", "quote": "before"},
		"suggestion": map[string]string{"replace_with": "after"},
		"actor":      sessionActor(),
	})
	if suggestion.Code != http.StatusCreated {
		t.Fatalf("create suggestion: status=%d body=%s", suggestion.Code, suggestion.Body.String())
	}
	comment := decodeBody[model.Comment](t, suggestion)
	drainDocumentUpdates(persistenceStore)
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	}()
	waitForPostApply(t, failure)
	waitForDocumentUpdate(t, persistenceStore)
	close(failure.release)
	handlerResponse := awaitResponse(t, responses)
	if handlerResponse.Code != http.StatusInternalServerError {
		t.Fatalf("accept with forced post-apply failure: status=%d body=%s", handlerResponse.Code, handlerResponse.Body.String())
	}
	assertHandlerDocumentText(t, handler, issue.PrimaryArtifactID, "before\n")
	assertNoSettledDocumentVersion(t, database, issue.PrimaryArtifactID, settleInterval)
	waitForDocumentConnectionClose(t, connection)
	reconnected, wsResponse, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("reconnect evicted document: response=%#v err=%v", wsResponse, err)
	}
	t.Cleanup(func() { _ = reconnected.Close() })
	assertHandlerDocumentText(t, handler, issue.PrimaryArtifactID, "before\n")
}

func TestCommentProjectionFailureEvictsLiveDocument(t *testing.T) {
	for _, action := range []string{"reply", "resolve"} {
		t.Run(action, func(t *testing.T) {
			var documentService *docs.Service
			var failure *postApplyFailureDocs
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{
					Store:    database,
					Settle:   time.Hour,
					Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
				})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				failure = &postApplyFailureDocs{
					API:               documentService,
					applied:           make(chan struct{}),
					release:           make(chan struct{}),
					failProjectMark:   true,
					projectMarkPasses: 1,
				}
				return failure
			})
			issue := createInteractionIssue(t, handler, "TEST", "Projection rollback", "before")
			documentServer := httptest.NewServer(http.HandlerFunc(documentService.ServeHTTP))
			t.Cleanup(documentServer.Close)
			connection, wsResponse, err := gws.DefaultDialer.Dial(
				"ws"+strings.TrimPrefix(documentServer.URL, "http")+"/ws/doc/"+issue.PrimaryArtifactID,
				http.Header{"X-Dispatch-User": []string{"alice"}},
			)
			if err != nil {
				t.Fatalf("connect live document: response=%#v err=%v", wsResponse, err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			rootResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "root", "anchor": map[string]any{"artifact": "spec", "quote": "before"}, "actor": sessionActor(),
			})
			if rootResponse.Code != http.StatusCreated {
				t.Fatalf("create anchored root: status=%d body=%s", rootResponse.Code, rootResponse.Body.String())
			}
			root := decodeBody[model.Comment](t, rootResponse)

			responses := make(chan *httptest.ResponseRecorder, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if action == "reply" {
					responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
						"body": "reply", "reply_to": root.ID,
					}, "alice")
					return
				}
				responses <- sessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+root.ID+"/resolve", map[string]any{"actor": sessionActor()})
			}()
			t.Cleanup(func() {
				closeTestGate(failure.release)
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("projection failure request did not finish after its gate opened")
				}
			})
			waitForPostApply(t, failure)
			closeTestGate(failure.release)
			handlerResponse := awaitResponse(t, responses)
			if handlerResponse.Code != http.StatusInternalServerError {
				t.Fatalf("%s projection failure: status=%d body=%s", action, handlerResponse.Code, handlerResponse.Body.String())
			}
			assertHandlerDocumentText(t, handler, issue.PrimaryArtifactID, "before\n")
			waitForDocumentConnectionClose(t, connection)
		})
	}
}

func TestDocumentUploadRollbackEvictsLiveDocument(t *testing.T) {
	var documentService *docs.Service
	var failure *postApplyFailureDocs
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		failure = &postApplyFailureDocs{
			API: documentService, applied: make(chan struct{}), release: make(chan struct{}),
		}
		return failure
	})
	issue := createInteractionIssue(t, handler, "TEST", "Transactional upload", "before")
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
			"name": "spec.md",
		}, "spec.md", "text/markdown", []byte("after"), "alice")
	}()
	waitForPostApply(t, failure)
	close(failure.release)
	response := awaitResponse(t, responses)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("upload with forced post-replace failure: status=%d body=%s", response.Code, response.Body.String())
	}
	assertHandlerDocumentText(t, handler, issue.PrimaryArtifactID, "before\n")
}

type recordingVersionedStore struct {
	docs.VersionedStore
	updates chan struct{}
}

func (s *recordingVersionedStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	version, err := s.VersionedStore.AppendUpdate(ctx, room, update)
	if err == nil {
		s.updates <- struct{}{}
	}
	return version, err
}

func (s *recordingVersionedStore) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte) (persistence.Version, error) {
	version, err := s.VersionedStore.AppendUpdateTx(ctx, tx, room, update)
	if err == nil {
		s.updates <- struct{}{}
	}
	return version, err
}

type postApplyFailureDocs struct {
	docs.API
	beforeApply        chan struct{}
	releaseBeforeApply chan struct{}
	beforeEvict        chan struct{}
	releaseEvict       chan struct{}
	applied            chan struct{}
	release            chan struct{}
	failProjectMark    bool
	projectMarkPasses  int
}

func (d *postApplyFailureDocs) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor) (int, error) {
	d.waitBeforeApply()
	applied, err := d.API.ApplyOps(ctx, artifactID, ops, actor)
	if err != nil {
		return 0, err
	}
	close(d.applied)
	<-d.release
	return applied, errors.New("forced post-apply failure")
}

func (d *postApplyFailureDocs) AcceptSuggestion(ctx context.Context, artifactID, markID, replacement string, actor model.Actor) error {
	d.waitBeforeApply()
	if err := d.API.AcceptSuggestion(ctx, artifactID, markID, replacement, actor); err != nil {
		return err
	}
	close(d.applied)
	<-d.release
	return errors.New("forced post-apply failure")
}

func (d *postApplyFailureDocs) ProjectMark(ctx context.Context, artifactID, markID string, record docs.MarkRecord) error {
	if !d.failProjectMark {
		return d.API.ProjectMark(ctx, artifactID, markID, record)
	}
	if d.projectMarkPasses > 0 {
		d.projectMarkPasses--
		return d.API.ProjectMark(ctx, artifactID, markID, record)
	}
	d.waitBeforeApply()
	if err := d.API.ProjectMark(ctx, artifactID, markID, record); err != nil {
		return err
	}
	close(d.applied)
	<-d.release
	return errors.New("forced post-projection failure")
}

func (d *postApplyFailureDocs) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	d.waitBeforeApply()
	_, err := d.API.ReplaceText(ctx, artifactID, markdown, actor)
	if err != nil {
		return "", err
	}
	close(d.applied)
	<-d.release
	return "", errors.New("forced post-apply failure")
}

func (d *postApplyFailureDocs) Evict(ctx context.Context, artifactID string) error {
	if d.beforeEvict != nil {
		close(d.beforeEvict)
		<-d.releaseEvict
	}
	return d.API.Evict(ctx, artifactID)
}

func (d *postApplyFailureDocs) waitBeforeApply() {
	if d.beforeApply == nil {
		return
	}
	close(d.beforeApply)
	<-d.releaseBeforeApply
}

func loadMarkProjection(t *testing.T, database *store.Store, artifactID, markID string) map[string]any {
	t.Helper()
	projection, found := findMarkProjection(t, database, artifactID, markID)
	if !found {
		t.Fatalf("mark projection %q is missing", markID)
	}
	return projection
}

func findMarkProjection(t *testing.T, database *store.Store, artifactID, markID string) (map[string]any, bool) {
	t.Helper()
	loaded, err := docs.NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load persisted document: %v", err)
	}
	document := crdt.New()
	if err := crdt.ApplyUpdateV1(document, loaded.Update, nil); err != nil {
		t.Fatalf("decode persisted document: %v", err)
	}
	value, found := document.GetMap("marks").Get(markID)
	if !found {
		return nil, false
	}
	projection, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("mark projection = %#v, want object", value)
	}
	return projection, true
}

func drainDocumentUpdates(store *recordingVersionedStore) {
	for {
		select {
		case <-store.updates:
		default:
			return
		}
	}
}

func closeTestGate(gate chan struct{}) {
	if gate == nil {
		return
	}
	select {
	case <-gate:
		return
	default:
		close(gate)
	}
}

func assertArtifactKeyShareLockAvailable(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin artifact lock probe: %v", err)
	}
	defer tx.Rollback(context.Background())
	var lockedID string
	if err := tx.QueryRow(context.Background(), `
		select id::text from artifacts where id = $1 for key share nowait
	`, artifactID).Scan(&lockedID); err != nil {
		t.Fatalf("settlement holds artifact lock while waiting for issue lock: %v", err)
	}
}

func waitForBeforeApply(t *testing.T, docs *postApplyFailureDocs) {
	t.Helper()
	select {
	case <-docs.beforeApply:
	case <-time.After(time.Second):
		t.Fatal("document mutation did not reach pre-apply gate")
	}
}

func waitForBeforeEvict(t *testing.T, docs *postApplyFailureDocs) {
	t.Helper()
	select {
	case <-docs.beforeEvict:
	case <-time.After(time.Second):
		t.Fatal("document mutation did not reach eviction gate")
	}
}

func waitForPostApply(t *testing.T, docs *postApplyFailureDocs) {
	t.Helper()
	select {
	case <-docs.applied:
	case <-time.After(time.Second):
		t.Fatal("document mutation did not complete")
	}
}

func waitForDocumentUpdate(t *testing.T, store *recordingVersionedStore) {
	t.Helper()
	select {
	case <-store.updates:
	case <-time.After(time.Second):
		t.Fatal("document mutation did not reach persistence")
	}
}

func assertHandlerDocumentText(t *testing.T, handler http.Handler, artifactID, want string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read document after rollback: status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, response)
	if result.Markdown != want {
		t.Fatalf("document after rollback = %q, want %q", result.Markdown, want)
	}
}

func assertNoSettledDocumentVersion(t *testing.T, database *store.Store, artifactID string, settle time.Duration) {
	t.Helper()
	time.Sleep(3 * settle)
	var versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count document versions after rollback: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after rollback = %d, want 1", versions)
	}
}

func waitForDocumentConnectionClose(t *testing.T, connection *gws.Conn) {
	t.Helper()
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				t.Fatal("document connection remained open after rollback")
			}
			return
		}
	}
}

func TestResolveAskRemovesItFromInboxAndKeepsTheThread(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Retracted decision", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Should this remain open?", "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)

	resolver := map[string]any{"kind": "session", "id": "session-handoff"}
	resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/resolve", map[string]any{
		"kind": "retracted", "reason": "A newer question supersedes this one.", "actor": resolver,
	})
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve ask: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	resolution := decodeBody[struct {
		State      string `json:"state"`
		Resolution struct {
			Kind   string      `json:"kind"`
			Reason string      `json:"reason"`
			Actor  model.Actor `json:"actor"`
		} `json:"resolution"`
	}](t, resolved)
	if resolution.State != "resolved" || resolution.Resolution.Kind != "retracted" || resolution.Resolution.Reason != "A newer question supersedes this one." || resolution.Resolution.Actor.Kind != "session" || resolution.Resolution.Actor.ID != "session-handoff" {
		t.Fatalf("resolved ask = %#v", resolution)
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK || strings.Contains(inbox.Body.String(), ask.ID) {
		t.Fatalf("resolved ask remained in inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues", nil, "alice")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"open_asks":0`) {
		t.Fatalf("resolved ask remained in issue count: status=%d body=%s", listed.Code, listed.Body.String())
	}
	thread := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if thread.Code != http.StatusOK || !strings.Contains(thread.Body.String(), `"state":"resolved"`) || !strings.Contains(thread.Body.String(), `"reason":"A newer question supersedes this one."`) {
		t.Fatalf("resolved ask thread = status=%d body=%s", thread.Code, thread.Body.String())
	}
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if log.Code != http.StatusOK || !strings.Contains(log.Body.String(), `"type":"ask.resolved"`) || !strings.Contains(log.Body.String(), `"notify":false`) {
		t.Fatalf("resolved ask event = status=%d body=%s", log.Code, log.Body.String())
	}
}

func TestResolveAskRejectsAnsweredAsksAndAnswerRejectsResolvedAsks(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Closed decisions", "A spec")
	answered := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Should this be answered?", "actor": sessionActor(),
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("create answered ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answeredAsk := decodeBody[struct {
		ID string `json:"id"`
	}](t, answered)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answeredAsk.ID+"/answer", map[string]any{"text": "Already decided."}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answeredAsk.ID+"/resolve", map[string]any{
		"kind": "resolved", "reason": "No longer relevant.", "actor": sessionActor(),
	}); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ASK_ANSWERED"`) {
		t.Fatalf("resolve answered ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+answeredAsk.ID, nil, "alice"); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"answered"`) || !strings.Contains(response.Body.String(), `"text":"Already decided."`) {
		t.Fatalf("answered ask after failed resolve: status=%d body=%s", response.Code, response.Body.String())
	}

	open := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Should this be retracted?", "actor": sessionActor(),
	})
	if open.Code != http.StatusCreated {
		t.Fatalf("create resolvable ask: status=%d body=%s", open.Code, open.Body.String())
	}
	resolvedAsk := decodeBody[struct {
		ID string `json:"id"`
	}](t, open)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolvedAsk.ID+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Found the answer.", "actor": sessionActor(),
	}); response.Code != http.StatusOK {
		t.Fatalf("resolve ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolvedAsk.ID+"/resolve", map[string]any{
		"kind": "retracted", "reason": "Superseded after all.", "actor": sessionActor(),
	}); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ASK_RESOLVED"`) {
		t.Fatalf("resolve resolved ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+resolvedAsk.ID+"/answer", map[string]any{"text": "Too late."}, "alice"); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ASK_RESOLVED"`) {
		t.Fatalf("answer resolved ask: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestResolveAskRejectsInvalidInputAndClosedIssues(t *testing.T) {
	handler := newTestHandler(t)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/00000000-0000-0000-0000-000000000000/resolve", map[string]any{
		"kind": "resolved", "reason": "Found the answer.", "actor": sessionActor(),
	}); response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"NOT_FOUND"`) {
		t.Fatalf("resolve missing ask: status=%d body=%s", response.Code, response.Body.String())
	}
	issue := createInteractionIssue(t, handler, "TEST", "Resolution validation", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Can this be resolved?", "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, created)
	for _, input := range []map[string]any{
		{"kind": "closed", "reason": "No."},
		{"kind": "resolved", "reason": "   "},
	} {
		input["actor"] = sessionActor()
		if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/resolve", input); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_RESOLUTION"`) {
			t.Fatalf("invalid resolution %#v: status=%d body=%s", input, response.Code, response.Body.String())
		}
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Found the answer.", "actor": sessionActor(),
	}); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("resolve ask on closed issue: status=%d body=%s", response.Code, response.Body.String())
	}
}
