package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

func (g *firstReplaceGate) ApplyReplace(ctx context.Context, artifactID string, anchor model.Anchor, replacement string, actor model.Actor) error {
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
	return g.API.ApplyReplace(ctx, artifactID, anchor, replacement, actor)
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
		"custom":   false,
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
	if ask.State != "open" || ask.Anchor.From != 10 || ask.Anchor.To != 15 || ask.Anchor.Version != 1 {
		t.Fatalf("anchored ask = %#v; want open with [10,15) at version 1", ask)
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
	if notFound.Code != http.StatusUnprocessableEntity || !strings.Contains(notFound.Body.String(), `"code":"TARGET_NOT_FOUND"`) {
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
	if ask.Anchor.From != 10 || ask.Anchor.To != 13 {
		t.Fatalf("disambiguated anchor = %#v; want [10,13)", ask.Anchor)
	}
	from, to := 10, 13
	selection := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Selection ask", "anchor": map[string]any{"artifact": "spec", "from": from, "to": to}, "actor": sessionActor(),
	})
	if selection.Code != http.StatusCreated || !strings.Contains(selection.Body.String(), `"quote":"the"`) {
		t.Fatalf("selection anchor: status=%d body=%s", selection.Code, selection.Body.String())
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
	missingRequiredOptions := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Options required", "custom": false, "actor": sessionActor(),
	})
	if missingRequiredOptions.Code != http.StatusBadRequest || !strings.Contains(missingRequiredOptions.Body.String(), `"code":"INVALID_ASK"`) {
		t.Fatalf("missing required options: status=%d body=%s", missingRequiredOptions.Code, missingRequiredOptions.Body.String())
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
		"question": "Pick", "options": []map[string]string{{"label": "yes"}}, "custom": false, "actor": sessionActor(),
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

	customSingle := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Choose only one", "custom": true, "actor": sessionActor(),
	})
	customAsk := decodeBody[struct {
		ID string `json:"id"`
	}](t, customSingle)
	multipleSelected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+customAsk.ID+"/answer", map[string]any{
		"selected": []string{"first", "second"},
	}, "alice")
	if multipleSelected.Code != http.StatusBadRequest || !strings.Contains(multipleSelected.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("multiple answers for single-select custom ask: status=%d body=%s", multipleSelected.Code, multipleSelected.Body.String())
	}
	emptyCustomAnswer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+customAsk.ID+"/answer", map[string]any{"selected": []string{}}, "alice")
	if emptyCustomAnswer.Code != http.StatusBadRequest || !strings.Contains(emptyCustomAnswer.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("empty custom answer: status=%d body=%s", emptyCustomAnswer.Code, emptyCustomAnswer.Body.String())
	}

	blankCustomTextAnswer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+customAsk.ID+"/answer", map[string]any{"selected": []string{}, "text": "   "}, "alice")
	if blankCustomTextAnswer.Code != http.StatusBadRequest || !strings.Contains(blankCustomTextAnswer.Body.String(), `"code":"INVALID_ANSWER"`) {
		t.Fatalf("blank custom text answer: status=%d body=%s", blankCustomTextAnswer.Code, blankCustomTextAnswer.Body.String())
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
	if err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "The clever brown fox", model.Actor{Kind: "user", ID: "alice"}); err != nil {
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
	if ask.Anchor.Version != 2 || ask.Anchor.From != 11 || ask.Anchor.To != 16 {
		t.Fatalf("anchor against changed document = %#v; want version 2 [11,16)", ask.Anchor)
	}
	version := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions/2", nil, "alice")
	if version.Code != http.StatusOK || !strings.Contains(version.Body.String(), `"markdown":"The clever brown fox"`) || !strings.Contains(version.Body.String(), `"named":false`) {
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
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"same same"`) {
		t.Fatalf("failed edit changed document: status=%d body=%s", text.Code, text.Body.String())
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
	handler := newTestHandler(t)
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
	suggested := decodeBody[struct {
		ID string `json:"id"`
	}](t, suggestion)
	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+suggested.ID+"/reject", map[string]any{}, "alice")
	if rejected.Code != http.StatusOK || !strings.Contains(rejected.Body.String(), `"resolved":true`) || !strings.Contains(rejected.Body.String(), `"accepted":false`) {
		t.Fatalf("reject suggestion: status=%d body=%s", rejected.Code, rejected.Body.String())
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
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
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
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, suggestion)
	forbidden := sessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{"actor": sessionActor()})
	if forbidden.Code != http.StatusForbidden || !strings.Contains(forbidden.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("session accepts suggestion: status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), `"resolved":true`) || !strings.Contains(accepted.Body.String(), `"accepted":true`) {
		t.Fatalf("accept suggestion: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read accepted document: %v", err)
	}
	if markdown != "The quick red fox" {
		t.Fatalf("accepted document = %q; want replacement applied", markdown)
	}
	repeated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if repeated.Code != http.StatusConflict || !strings.Contains(repeated.Body.String(), `"code":"ALREADY_ACTIONED"`) {
		t.Fatalf("repeat accept: status=%d body=%s", repeated.Code, repeated.Body.String())
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
	if markdown != "foo2" {
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
	if markdown != "foo" {
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
	document := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md",
	}, "notes.md", "text/markdown", []byte("notes"), "alice")
	documentID := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, document).Artifact.ID

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
	assertClosed("primary selection", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+documentID+"/primary", map[string]any{}, "alice"))
	assertClosed("document edit", sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}}, "actor": sessionActor(),
	}))
	assertClosed("named version", sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]any{
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

func TestEditArtifactWithoutSummaryReturnsNoVersion(t *testing.T) {
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
	if result.Applied != 1 || result.Version != nil {
		t.Fatalf("edit result = %#v, want one applied operation and no named version", result)
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
