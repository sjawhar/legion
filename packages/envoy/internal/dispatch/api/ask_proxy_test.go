package api

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// The table names the one person it is there for, with the skill that holds their advice. A test
// that read the table for both halves would pass with either half replaced, so this pins them.
func TestAskProxySkillsNamesTheOnePersonItIsFor(t *testing.T) {
	if skill := askProxySkills["sami@theorem.dev"]; skill != "skill://sami-proxy" {
		t.Fatalf("askProxySkills[\"sami@theorem.dev\"] = %q, want skill://sami-proxy", skill)
	}
	if len(askProxySkills) != 1 {
		t.Fatalf("askProxySkills = %v, want the one person; add a new one to this row too", askProxySkills)
	}
}

// Two answers of the same second, one with a fraction and one without, are ordered as instants
// and not as the text that spells them: Go writes a whole second with no fraction, and
// "12:00:00Z" reads above "12:00:00.1Z" as text, since "Z" is above ".", and below it as a
// time. A line longer than one screen is cut with an ellipsis, so five answers cannot make this
// response larger than the most a keyed write's answer may be kept at.
func TestAskProxyAdviceOrdersAnswersAsInstantsAndCutsALongLine(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "PXT")
	person, _ := proxyPerson(t)
	issue := createProxyIssue(t, handler, "PXT", "Hold the export's ordering", "", person)

	older, _ := openProxyAsk(t, handler, issue, "Does the export keep its own ordering?", "alice")
	answerProxyAsk(t, handler, older, strings.Repeat("word ", 400), person)
	newer, _ := openProxyAsk(t, handler, issue, "Does the export keep its own retries?", "alice")
	answerProxyAsk(t, handler, newer, "It keeps them.", person)

	// The newer answer carries the fraction: compared as text it would sink below the older one.
	setAnswerTime(t, database, newer, "2026-11-05T12:00:00.1Z")
	setAnswerTime(t, database, older, "2026-11-05T12:00:00Z")

	advice := directServer(deps).askProxyAdvice(t.Context(), issue)
	if advice == nil || len(advice.Answers) != 2 {
		t.Fatalf("advice = %+v, want both answers", advice)
	}
	if advice.Answers[0].Ref != "dispatch://"+issue+"/ask/"+newer {
		t.Fatalf("first answer = %+v, want the newer ask %s", advice.Answers[0], newer)
	}
	cut := advice.Answers[1].Line
	if len([]rune(cut)) != askProxyLineMax+1 || !strings.HasSuffix(cut, "…") {
		t.Fatalf("the long answer's line is %d runes (%q), want %d and an ellipsis", len([]rune(cut)), cut, askProxyLineMax+1)
	}
}

// setAnswerTime rewrites when one answered ask was answered, which no route lets a test choose.
func setAnswerTime(t *testing.T, database *store.Store, askID, at string) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(),
		`update asks set answer = jsonb_set(answer, '{at}', to_jsonb($2::text)) where id = $1::uuid`, askID, at); err != nil {
		t.Fatalf("set the answer time of %s: %v", askID, err)
	}
}

// An approval Dispatch carried forward in the target's name is not one of their answers. The
// carry answers the open approval request as the person: their user on the answer, their actor
// on the event, and a sentence of Dispatch's as the text, so nothing in the answer itself tells
// it from words they wrote. The four steps that make one: the person approves version 1, the
// agent changes the document and asks for approval, the agent restores the approved text, which
// answers the request, and an ask then lists the person's answers. Left in, a run of carries
// would push their real answers out of the few rows the advisory lists.
//
// The row the carry stores is the row every carry has ever stored - the ask's kind is what the
// advisory reads, and the answer carries no field of its own - so this is the upgrade case too:
// a carry written before this rule is left out exactly as one written after it.
func TestAskProxyAdviceLeavesOutAnApprovalDispatchCarried(t *testing.T) {
	handler, database, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	person, _ := proxyPerson(t)
	issue := createInteractionIssue(t, handler, "TEST", "Carry the approval under a proxy", restoreSpec)

	// The person's own answer to a question, the one row the advisory should list.
	answered, _ := openProxyAsk(t, handler, issue.Key, "Does the service ship behind a flag?", "alice")
	answerProxyAsk(t, handler, answered, "It ships behind a flag.", person)

	if review := reviewDocument(t, handler, issue.PrimaryArtifactID, person, "approved", ""); review.Version != 1 {
		t.Fatalf("review = %#v, want an approval of version 1", review)
	}
	agentEdit(t, handler, issue.PrimaryArtifactID, 2, replaceOp("behind a flag.", "behind a flag and a canary."))
	request := requestApproval(t, handler, issue.PrimaryArtifactID)
	agentEdit(t, handler, issue.PrimaryArtifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	carried := readBlockAsk(t, handler, request.ID)
	if carried.State != "answered" || carried.Answer == nil || model.CanonicalLogin(carried.Answer.User) != person ||
		carried.Answer.Text == nil || *carried.Answer.Text != restoreSentence(1) {
		t.Fatalf("the request after the restore = state %q answer %#v, want the carry in %s's name", carried.State, carried.Answer, person)
	}
	requireStoredAnswerFields(t, database, request.ID, []string{"at", "selected", "text", "user"})
	setIssueAssignee(t, handler, issue.Key, person)

	_, suggestions := openProxyAsk(t, handler, issue.Key, "Which region takes the first wave?", "alice")
	proxy := suggestions.Proxy
	if proxy == nil {
		t.Fatalf("advice.suggestions = %+v, want the proxy advisory for %s", suggestions, person)
	}
	want := []model.AskProxyAnswer{{
		Ref: "dispatch://" + issue.Key + "/ask/" + answered, Line: "It ships behind a flag.",
	}}
	if !reflect.DeepEqual(proxy.Answers, want) {
		t.Fatalf("proxy answers = %+v, want only the answer %s wrote, never the carried approval %s", proxy.Answers, person, request.ID)
	}
}

// setIssueAssignee gives the issue the person whose asks it reaches.
func setIssueAssignee(t *testing.T, handler http.Handler, issueKey, login string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issueKey,
		map[string]any{"assignee": login}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("assign %s to %s: status=%d body=%s", issueKey, login, response.Code, response.Body.String())
	}
}

// The carry's sentence is a sentence a person may write, so the wording is no mark of a carry:
// an answer the person typed themselves that opens with it is theirs and stays listed. Before
// the mark, the advisory filtered on that opening and this answer vanished from it.
func TestAskProxyAdviceKeepsAnAnswerThatOpensWithTheCarrysWording(t *testing.T) {
	handler := newTestHandler(t)
	createSuggestionProject(t, handler, "PXW")
	person, _ := proxyPerson(t)
	issue := createProxyIssue(t, handler, "PXW", "Decide the canary", "", person)

	worded := "Carried forward from the approval of version 1 is not my decision; keep the canary enabled."
	answered, _ := openProxyAsk(t, handler, issue, "Does the canary stay on for the next wave?", "alice")
	answerProxyAsk(t, handler, answered, worded, person)

	_, suggestions := openProxyAsk(t, handler, issue, "Which region takes the first wave?", "alice")
	proxy := suggestions.Proxy
	if proxy == nil {
		t.Fatalf("advice.suggestions = %+v, want the proxy advisory for %s", suggestions, person)
	}
	want := []model.AskProxyAnswer{{Ref: "dispatch://" + issue + "/ask/" + answered, Line: worded}}
	if !reflect.DeepEqual(proxy.Answers, want) {
		t.Fatalf("proxy answers = %+v, want the answer %s wrote, whatever it opens with", proxy.Answers, person)
	}
}

// requireStoredAnswerFields reads the ask's answer as the row holds it and requires exactly the
// given keys, so a test that calls a stored shape the pre-change shape has said so against the
// bytes rather than against the struct that decoded them.
func requireStoredAnswerFields(t *testing.T, database *store.Store, askID string, want []string) {
	t.Helper()
	var keys []string
	if err := database.Pool.QueryRow(t.Context(),
		`select coalesce(array(select jsonb_object_keys(answer) order by 1), '{}') from asks where id = $1::uuid`,
		askID).Scan(&keys); err != nil {
		t.Fatalf("read the stored answer of %s: %v", askID, err)
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("the stored answer of %s holds %v, want %v", askID, keys, want)
	}
}

// A person's own approval is not listed either: it is a sign-off on a document version, which
// the document's approval state shows, and the advisory lists answers to questions.
func TestAskProxyAdviceLeavesOutThePersonsOwnApproval(t *testing.T) {
	handler, _, _ := settlingCarryDocuments(t, 20*time.Millisecond)
	person, _ := proxyPerson(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approve under a proxy", restoreSpec)

	answered, _ := openProxyAsk(t, handler, issue.Key, "Does the service ship behind a flag?", "alice")
	answerProxyAsk(t, handler, answered, "It ships behind a flag.", person)
	request := requestApproval(t, handler, issue.PrimaryArtifactID)
	approved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+request.ID+"/answer",
		map[string]any{"selected": []string{"Approve"}, "text": "Ship it."}, person)
	if approved.Code != http.StatusOK {
		t.Fatalf("approve the request as %s: status=%d body=%s", person, approved.Code, approved.Body.String())
	}
	setIssueAssignee(t, handler, issue.Key, person)

	_, suggestions := openProxyAsk(t, handler, issue.Key, "Which region takes the first wave?", "alice")
	proxy := suggestions.Proxy
	if proxy == nil {
		t.Fatalf("advice.suggestions = %+v, want the proxy advisory for %s", suggestions, person)
	}
	want := []model.AskProxyAnswer{{
		Ref: "dispatch://" + issue.Key + "/ask/" + answered, Line: "It ships behind a flag.",
	}}
	if !reflect.DeepEqual(proxy.Answers, want) {
		t.Fatalf("proxy answers = %+v, want the answer to the question alone, never the approval %s", proxy.Answers, request.ID)
	}
}
