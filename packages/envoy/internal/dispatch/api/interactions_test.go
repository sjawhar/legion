package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "closed"}, "alice")
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
}

func TestInboxOrdersOpenAsksAcrossIssues(t *testing.T) {
	handler := newTestHandler(t)
	first := createInteractionIssue(t, handler, "TEST", "First", "first")
	firstAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+first.Key+"/asks", map[string]any{"question": "First question", "actor": sessionActor()})
	if firstAsk.Code != http.StatusCreated {
		t.Fatalf("create first ask: status=%d body=%s", firstAsk.Code, firstAsk.Body.String())
	}
	time.Sleep(2 * time.Millisecond)
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
