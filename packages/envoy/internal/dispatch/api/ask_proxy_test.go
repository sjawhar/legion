package api

import (
	"net/http"
	"testing"
)

// proxyPerson is the one person askProxySkills names, read from the table so the test follows it
// rather than repeating it.
func proxyPerson(t *testing.T) (login, skill string) {
	t.Helper()
	if len(askProxySkills) != 1 {
		t.Fatalf("askProxySkills = %v, want one person; give this test the row it should use", askProxySkills)
	}
	for person, address := range askProxySkills {
		return person, address
	}
	return "", ""
}

// createProxyIssue files one issue for the proxy test and returns its key. parent is "" for a
// root issue. An issue a person creates with no assignee goes to that person, and a child with
// none goes to its parent's assignee, so who creates it decides whom its asks reach.
func createProxyIssue(t *testing.T, handler http.Handler, project, title, parent, login string) string {
	t.Helper()
	body := map[string]any{"project": project, "title": title}
	if parent != "" {
		body["parent"] = parent
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	return decodeBody[testIssueWithSuggestions](t, response).Key
}

// openProxyAsk opens one ask and returns its id with the suggestions the write answered.
func openProxyAsk(t *testing.T, handler http.Handler, issueKey, question, login string) (string, *testSuggestions) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question,
	}, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("open ask on %s: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
	created := decodeBody[testAskWithSuggestions](t, response)
	if created.Advice == nil || created.Advice.Suggestions == nil {
		t.Fatalf("advice.suggestions missing from %+v", created)
	}
	return created.ID, created.Advice.Suggestions
}

func answerProxyAsk(t *testing.T, handler http.Handler, askID, text, login string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"text": text,
	}, login)
	if response.Code != http.StatusOK {
		t.Fatalf("answer ask %s: status=%d body=%s", askID, response.Code, response.Body.String())
	}
}

// An ask on an issue whose assignee has a proxy skill carries that skill's address and what the
// person already answered on the issue and on its parent, each as its ask reference and the first
// line of the answer, newest first. Nothing is refused: the write is a 201 with its related-item
// check as any other ask's.
func TestCreateAskAdvisesTheProxySkillAndTheTargetsEarlierAnswers(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "PXY")
	person, skill := proxyPerson(t)

	parent := createProxyIssue(t, handler, "PXY", "Retire the nightly export", "", person)
	child := createProxyIssue(t, handler, "PXY", "Move the export's schedule into the scheduler", parent, person)

	parentAsk, _ := openProxyAsk(t, handler, parent, "Does the nightly export keep its own retry budget?", "alice")
	answerProxyAsk(t, handler, parentAsk, "\nKeep the budget where it is.\nSplitting it costs more than it saves.", person)
	childAsk, _ := openProxyAsk(t, handler, child, "Which scheduler runs the export?", "alice")
	answerProxyAsk(t, handler, childAsk, "The platform scheduler, as everything else uses it.", person)

	_, suggestions := openProxyAsk(t, handler, child, "Should the export run hourly instead of nightly?", "alice")
	proxy := suggestions.Proxy
	if proxy == nil {
		t.Fatalf("advice.suggestions = %+v, want the proxy advisory for %s", suggestions, person)
	}
	if proxy.Skill != skill || proxy.Target != person {
		t.Fatalf("proxy = %+v, want skill %s for %s", proxy, skill, person)
	}
	refs := []string{"dispatch://" + child + "/ask/" + childAsk, "dispatch://" + parent + "/ask/" + parentAsk}
	lines := []string{"The platform scheduler, as everything else uses it.", "Keep the budget where it is."}
	if len(proxy.Answers) != len(refs) {
		t.Fatalf("proxy answers = %+v, want the issue's and its parent's answered asks", proxy.Answers)
	}
	for index, answer := range proxy.Answers {
		if answer.Ref != refs[index] || answer.Line != lines[index] {
			t.Fatalf("proxy answer %d = %+v, want %s with first line %q", index, answer, refs[index], lines[index])
		}
	}
}

// An ask on an issue whose assignee has no proxy skill carries no advisory, and its write is
// unchanged: a 201 whose related-item check still answers.
func TestCreateAskLeavesTheProxyAdvisoryOffForEveryoneElse(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "PXO")
	person, _ := proxyPerson(t)

	theirs := createProxyIssue(t, handler, "PXO", "Rotate the export's credentials", "", person)
	answered, _ := openProxyAsk(t, handler, theirs, "Does the export keep one credential per region?", "alice")
	answerProxyAsk(t, handler, answered, "One per region.", person)

	mine := createProxyIssue(t, handler, "PXO", "Write the export's runbook", "", "alice")
	_, suggestions := openProxyAsk(t, handler, mine, "Which runbook section covers a failed export?", "alice")
	if suggestions.Proxy != nil {
		t.Fatalf("proxy = %+v on an issue assigned to alice, want none", suggestions.Proxy)
	}
	if suggestions.Outcome == "" {
		t.Fatalf("advice.suggestions = %+v, want the related-item check's own result", suggestions)
	}
}
