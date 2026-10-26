package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func referenceQuestionsRequest(t *testing.T, handler http.Handler, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/references/questions?"+query.Encode(), nil, "alice")
}

func referenceQuestionsOf(t *testing.T, handler http.Handler, kind, id string) referenceQuestions {
	t.Helper()
	query := url.Values{"kind": {kind}, "id": {id}}
	response := referenceQuestionsRequest(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/references/questions?%s: status=%d body=%s", query.Encode(), response.Code, response.Body.String())
	}
	return decodeBody[referenceQuestions](t, response)
}

// The dashboard resolves a `#id` by the server's rule: the route names the document a text belongs
// to and that document's question blocks, each to its ask. The spec's own text, a question block
// of it, a comment anchored to it and an issue message, which belongs to no document and so to the
// spec, all answer the spec's q1 and q2; a comment anchored to a second document answers that
// document's own q2; a direct message belongs to no document and answers none. A kind the route
// does not read or an id that is not a uuid is 400, a text that does not exist 404, as
// /api/v1/references answers a node that does not, and a caller with no identity 401.
func TestReferenceQuestionsNameTheQuestionsOfTheTextsDocument(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	issue := createInteractionIssue(t, handler, "ACME", "Questions",
		"Context\n\n"+questionBlock("q1", "Ship on Monday?")+"\n"+questionBlock("q2", "Ship the docs too?"))
	spec := issue.PrimaryArtifactID
	q1 := blockQuestion(t, database, spec, "q1")
	q2 := blockQuestion(t, database, spec, "q2")
	specRef := "dispatch://" + issue.Key + "/spec"
	specQuestions := referenceQuestions{Document: &specRef, Questions: []referenceQuestion{
		{BlockID: "q1", Ask: "dispatch://" + issue.Key + "/ask/" + q1},
		{BlockID: "q2", Ask: "dispatch://" + issue.Key + "/ask/" + q2},
	}}

	anchored := postIssueComment(t, handler, issue.Key, map[string]any{
		"body": "This hangs on #q2.", "anchor": map[string]any{"artifact": "spec", "quote": "Context"},
	})
	messaged := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Per #q2, go ahead.", "actor": sessionActor(),
	})
	if messaged.Code != http.StatusCreated {
		t.Fatalf("message the issue: status=%d body=%s", messaged.Code, messaged.Body.String())
	}
	message := decodeBody[model.Message](t, messaged)
	for name, source := range map[string][2]string{
		"the spec's own text":            {"artifact", spec},
		"a question block of the spec":   {"ask", q1},
		"a comment anchored to the spec": {"comment", anchored.ID},
		"an issue message":               {"message", message.ID},
	} {
		if got := referenceQuestionsOf(t, handler, source[0], source[1]); !reflect.DeepEqual(got, specQuestions) {
			t.Errorf("%s answers %+v; want %+v", name, got, specQuestions)
		}
	}

	other := uploadIssueDocument(t, handler, issue.Key, "rollout.md", "Rollout.\n\n"+questionBlock("q2", "Roll out by region?"))
	otherQ2 := blockQuestion(t, database, other.ID, "q2")
	otherComment := postIssueComment(t, handler, issue.Key, map[string]any{
		"body": "Here #q2 is the rollout's.", "anchor": map[string]any{"artifact": other.ID, "quote": "Rollout"},
	})
	otherRef := "dispatch://" + issue.Key + "/artifact/" + other.Slug
	want := referenceQuestions{Document: &otherRef, Questions: []referenceQuestion{
		{BlockID: "q2", Ask: "dispatch://" + issue.Key + "/ask/" + otherQ2},
	}}
	if got := referenceQuestionsOf(t, handler, "comment", otherComment.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("a comment anchored to the rollout answers %+v; want %+v", got, want)
	}

	var direct string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'See #q2.', 'session:s1')
		returning id::text
	`).Scan(&direct); err != nil {
		t.Fatalf("write a direct message: %v", err)
	}
	directResponse := referenceQuestionsRequest(t, handler, url.Values{"kind": {"message"}, "id": {direct}})
	if body := strings.TrimSpace(directResponse.Body.String()); directResponse.Code != http.StatusOK || body != `{"document":null,"questions":[]}` {
		t.Errorf("a direct message: status=%d body=%s; want 200 with no document and no questions", directResponse.Code, body)
	}

	for name, tc := range map[string]struct {
		query  url.Values
		status int
		code   string
	}{
		"no kind":                   {url.Values{"id": {spec}}, http.StatusBadRequest, "INVALID_KIND"},
		"an unread kind":            {url.Values{"kind": {"issue"}, "id": {spec}}, http.StatusBadRequest, "INVALID_KIND"},
		"an id not a uuid":          {url.Values{"kind": {"comment"}, "id": {"c1"}}, http.StatusBadRequest, "INVALID_ID"},
		"no id":                     {url.Values{"kind": {"comment"}}, http.StatusBadRequest, "INVALID_ID"},
		"a missing comment":         {url.Values{"kind": {"comment"}, "id": {uuid.NewString()}}, http.StatusNotFound, "NOT_FOUND"},
		"a missing message":         {url.Values{"kind": {"message"}, "id": {uuid.NewString()}}, http.StatusNotFound, "NOT_FOUND"},
		"an ask's id as a document": {url.Values{"kind": {"artifact"}, "id": {q1}}, http.StatusNotFound, "NOT_FOUND"},
	} {
		response := referenceQuestionsRequest(t, handler, tc.query)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%s: status=%d body=%s; want %d %s", name, response.Code, response.Body.String(), tc.status, tc.code)
		}
	}

	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/v1/references/questions?kind=artifact&id="+spec, nil))
	if anonymous.Code != http.StatusUnauthorized || !strings.Contains(anonymous.Body.String(), `"code":"NO_IDENTITY"`) {
		t.Errorf("no identity: status=%d body=%s; want 401 NO_IDENTITY", anonymous.Code, anonymous.Body.String())
	}
}
