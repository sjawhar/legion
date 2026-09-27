package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	priorQuestion = "Should candidate trial runs reach the model gateway with the candidate's own hawk login instead of the launcher's machine identity?"
	priorAnswer   = "Use human hawk tokens for candidate runs; the candidate cannot reach hawk directly, only through the platform."
	// The same question reworded, the way an agent that forgot the answer asks it again.
	repeatQuestion = "Can a candidate's trial run use the candidate's own hawk login to reach the model gateway, retiring the launcher machine identity?"
	novelQuestion  = "Which colour should the dashboard's warning banner use?"
)

type priorAnswerRefusalBody struct {
	Error      string                       `json:"error"`
	Code       string                       `json:"code"`
	Candidates []model.PriorAnswerCandidate `json:"candidates"`
}

func agentAsk(t *testing.T, handler http.Handler, issueKey, question string, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"question": question, "actor": sessionActor()}
	for key, value := range extra {
		body[key] = value
	}
	return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", body)
}

func requireAskCreated(t *testing.T, response *httptest.ResponseRecorder) model.Ask {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

func requirePriorAnswerRefusal(t *testing.T, response *httptest.ResponseRecorder) priorAnswerRefusalBody {
	t.Helper()
	if response.Code != http.StatusConflict {
		t.Fatalf("ask with a prior answer: status=%d body=%s, want 409", response.Code, response.Body.String())
	}
	refusal := decodeBody[priorAnswerRefusalBody](t, response)
	if refusal.Code != "POSSIBLE_PRIOR_ANSWER" || len(refusal.Candidates) == 0 || len(refusal.Candidates) > 5 {
		t.Fatalf("refusal = %#v, want POSSIBLE_PRIOR_ANSWER with one to five candidates", refusal)
	}
	return refusal
}

// answeredPriorAsk opens a question as an agent and has a human answer it.
func answeredPriorAsk(t *testing.T, handler http.Handler, issueKey string) model.Ask {
	t.Helper()
	ask := requireAskCreated(t, agentAsk(t, handler, issueKey, priorQuestion, nil))
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"text": priorAnswer,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer prior ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	return ask
}

func TestCreateAskRefusesAQuestionAHumanAlreadyAnswered(t *testing.T) {
	handler := newTestHandler(t)
	first := createInteractionIssue(t, handler, "TEST", "Staging launcher checks", "A spec")
	second := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Production deploy freeze",
	})).Key
	prior := answeredPriorAsk(t, handler, first.Key)

	before := decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+second+"/asks", nil, "alice"))
	refusal := requirePriorAnswerRefusal(t, agentAsk(t, handler, second, repeatQuestion, nil))

	candidate := refusal.Candidates[0]
	if candidate.Kind != "ask" || candidate.Ref != "dispatch://"+first.Key+"/ask/"+prior.ID ||
		candidate.Author.Kind != "user" || candidate.Author.ID != "alice" || candidate.At.IsZero() {
		t.Fatalf("top candidate = %#v, want alice's answer at dispatch://%s/ask/%s", candidate, first.Key, prior.ID)
	}
	if !strings.Contains(candidate.Snippet, "<mark>") || !strings.Contains(strings.NewReplacer("<mark>", "", "</mark>", "").Replace(candidate.Snippet), "human hawk tokens for candidate runs") {
		t.Fatalf("snippet = %q, want the answer with the matched terms marked", candidate.Snippet)
	}
	if !strings.Contains(refusal.Error, candidate.Ref) || !strings.Contains(refusal.Error, "force: true") {
		t.Fatalf("refusal message = %q, want it to name the candidate and the force escape", refusal.Error)
	}
	after := decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+second+"/asks", nil, "alice"))
	if len(after) != len(before) {
		t.Fatalf("a refused ask was written: asks before=%d after=%d", len(before), len(after))
	}
}

func TestCreateAskWithForceSkipsThePriorAnswerCheck(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Staging launcher checks", "A spec")
	answeredPriorAsk(t, handler, issue.Key)

	forced := requireAskCreated(t, agentAsk(t, handler, issue.Key, repeatQuestion, map[string]any{"force": true}))
	if forced.Question != repeatQuestion || forced.State != "open" {
		t.Fatalf("forced ask = %#v", forced)
	}
}

func TestCreateAskWithNoPriorAnswerIsCreated(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Staging launcher checks", "A spec")
	answeredPriorAsk(t, handler, issue.Key)

	requireAskCreated(t, agentAsk(t, handler, issue.Key, novelQuestion, nil))
}

func TestPriorAnswersAreScopedToTheAsksProject(t *testing.T) {
	handler := newTestHandler(t)
	elsewhere := createInteractionIssue(t, handler, "OTHER", "Staging launcher checks", "A spec")
	answeredPriorAsk(t, handler, elsewhere.Key)
	here := createInteractionIssue(t, handler, "TEST", "Production deploy freeze", "A spec")

	requireAskCreated(t, agentAsk(t, handler, here.Key, repeatQuestion, nil))
}

func TestAgentAuthoredContentIsNeverAPriorAnswer(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Staging launcher checks", "A spec")
	// The same words, three ways an agent writes them: a comment, a message, and a question of
	// its own that it resolved without a human answering.
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": priorQuestion + " " + priorAnswer, "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("agent comment: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": priorQuestion + " " + priorAnswer, "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("agent message: status=%d body=%s", response.Code, response.Body.String())
	}
	own := requireAskCreated(t, agentAsk(t, handler, issue.Key, priorQuestion, nil))
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+own.ID+"/resolve", map[string]any{
		"kind": "resolved", "reason": priorAnswer, "actor": sessionActor(),
	}); response.Code != http.StatusOK {
		t.Fatalf("resolve own ask: status=%d body=%s", response.Code, response.Body.String())
	}

	requireAskCreated(t, agentAsk(t, handler, issue.Key, repeatQuestion, nil))
}

// A human comment answers as well as an answered ask does, and an issue filed with the decision
// in its title and spec is offered beside it: the shape of AGENTC-1010, where the answer was
// recorded as a new issue hours before the same question was asked again.
func TestCreateAskOffersHumanCommentsAndRecentIssues(t *testing.T) {
	handler := newTestHandler(t)
	thread := createInteractionIssue(t, handler, "TEST", "Staging launcher checks", "A spec")
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+thread.Key+"/comments", map[string]any{
		"body": "Candidate trial runs should reach the model gateway with the candidate's own hawk login. " + priorAnswer,
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("human comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID
	decision := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST",
		"title":   "A candidate's own hawk login carries their trial run, retiring the launcher machine identity",
		"spec":    "## Summary\n\nThe candidate's own hawk login reaches the model gateway for their trial run; the launcher's machine identity is retired.\n",
		"actor":   sessionActor(),
	})
	if decision.Code != http.StatusCreated {
		t.Fatalf("decision issue: status=%d body=%s", decision.Code, decision.Body.String())
	}
	decisionKey := decodeBody[model.Issue](t, decision).Key

	refusal := requirePriorAnswerRefusal(t, agentAsk(t, handler, thread.Key, repeatQuestion, nil))
	refs := map[string]model.PriorAnswerCandidate{}
	for _, candidate := range refusal.Candidates {
		refs[candidate.Ref] = candidate
	}
	if got, ok := refs["dispatch://"+thread.Key+"/comment/"+commentID]; !ok || got.Kind != "comment" || got.Author.ID != "alice" {
		t.Fatalf("candidates = %#v, want alice's comment", refusal.Candidates)
	}
	if got, ok := refs["dispatch://"+decisionKey]; !ok || got.Kind != "issue" {
		t.Fatalf("candidates = %#v, want the decision issue %s", refusal.Candidates, decisionKey)
	}
}
