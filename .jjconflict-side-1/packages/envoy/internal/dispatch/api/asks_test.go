package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestCreateAskRejectsRemovedActionKind(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Removed action kind", "A spec")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Confirm the release is deployed.",
		"kind":     "action",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), `"code":"ASK_KIND_INPUT"`) || !strings.Contains(created.Body.String(), "action") {
		t.Fatalf("create action ask: status=%d body=%s, want 400 ASK_KIND_INPUT naming the removed kind", created.Code, created.Body.String())
	}
}

// A to-do handed to a human is an ordinary question whose asker chose Done / Can't; no option
// label carries a server rule of its own.
func TestDoneCantQuestionAnswersLikeAnyQuestion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Human to-do", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Confirm the release is deployed.",
		"options":  []map[string]string{{"label": "Done"}, {"label": "Can't"}},
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create Done / Can't question: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)
	if ask.Kind != "question" {
		t.Fatalf("ask kind = %q, want question", ask.Kind)
	}

	edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+ask.ID, map[string]any{
		"options": []map[string]string{{"label": "Done"}, {"label": "Can't"}, {"label": "Later"}},
		"actor":   sessionActor(),
	})
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"label":"Later"`) {
		t.Fatalf("edit Done / Can't options: status=%d body=%s", edited.Code, edited.Body.String())
	}
	current := decodeBody[struct {
		EditedAt *string `json:"edited_at"`
	}](t, edited)

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected":           []string{"Can't"},
		"expected_edited_at": current.EditedAt,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer Can't without text: status=%d body=%s", answered.Code, answered.Body.String())
	}
	result := decodeBody[model.Ask](t, answered)
	if result.State != "answered" || result.Answer == nil || len(result.Answer.Selected) != 1 || result.Answer.Selected[0] != "Can't" || result.Answer.Text != nil {
		t.Fatalf("answered Done / Can't question = %#v", result)
	}
}

func TestSingleSelectAnswerKeepsOptionalText(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Annotated answer", "A spec")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"actor":    sessionActor(),
		"options":  []map[string]string{{"label": "Ship"}, {"label": "Hold"}},
		"question": "Should we ship?",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create single-select ask: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[model.Ask](t, created)

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"Ship"},
		"text":     "After the release note is published.",
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer single-select ask with note: status=%d body=%s", answered.Code, answered.Body.String())
	}
	result := decodeBody[model.Ask](t, answered)
	if result.Answer == nil || len(result.Answer.Selected) != 1 || result.Answer.Selected[0] != "Ship" || result.Answer.Text == nil || *result.Answer.Text != "After the release note is published." {
		t.Fatalf("single-select answer = %#v", result)
	}
}

func TestCreateAskRejectsApprovalKind(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Server-created approval", "A spec")

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Approve this?",
		"kind":     "approval",
		"actor":    sessionActor(),
	})
	if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), `"code":"ASK_KIND_INPUT"`) {
		t.Fatalf("create client approval ask: status=%d body=%s", created.Code, created.Body.String())
	}
}

// issueAskListThread is an issue ask row's thread as a client reads it, and the same eight keys
// the single-ask read returns at its top level, so the two can be compared.
type issueAskListThread struct {
	Replies     []model.Comment     `json:"replies"`
	Edits       []model.AskEdit     `json:"edits"`
	Answers     []model.AskAnswer   `json:"answers"`
	Followers   []model.AskFollower `json:"followers"`
	Claimant    *string             `json:"claimant"`
	Route       *string             `json:"route"`
	RouteStatus *string             `json:"route_status"`
	RouteHolder *string             `json:"route_holder"`
}

// issueAskListRow names the ask fields a reader had before the row carried a thread, so
// decoding proves they are still at the row's own top level, beside the two new keys. Thread
// is a pointer because `?thread=` decides whether the key is there at all.
type issueAskListRow struct {
	ID            string              `json:"id"`
	State         string              `json:"state"`
	Question      string              `json:"question"`
	OpenedEventID *int64              `json:"opened_event_id"`
	LastReply     *model.AskLastReply `json:"last_reply"`
	Thread        *issueAskListThread `json:"thread"`
}

func listIssueAskRows(t *testing.T, handler http.Handler, issueKey, query string) []issueAskListRow {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/asks"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list issue asks %q: status=%d body=%s", query, response.Code, response.Body.String())
	}
	return decodeBody[[]issueAskListRow](t, response)
}

// An issue's ask list always carries the newest reply a client orders by; `?thread=1` adds what
// a client would otherwise fetch once per ask, the thread to render. Every state is listed,
// each row is still the ask it was before, and each thread - its answers among them - is the
// one the single-ask read hands back.
func TestIssueAskListCarriesLastReplyAndThreadOnRequest(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Issue ask rows", "A spec")

	open := createEditableAsk(t, handler, issue.Key)
	replyToAskAs(t, handler, issue.Key, open.ID, "session-open-replier", "Still deciding.")
	if edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+open.ID, map[string]any{
		"question": "Which transport should we implement?",
		"actor":    sessionActor(),
	}); edited.Code != http.StatusOK {
		t.Fatalf("reword the open ask: status=%d body=%s", edited.Code, edited.Body.String())
	}

	answeredID := openAskAs(t, handler, issue.Key, "session-answered", "Ship it?")
	replyToAskAs(t, handler, issue.Key, answeredID, "session-answer-replier", "The release note is ready.")
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answeredID+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	retractedID := openAskAs(t, handler, issue.Key, "session-retracted", "Rename the component?")
	if retracted := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+retractedID+"/resolve", map[string]any{
		"kind": "retracted", "reason": "No longer needed",
		"actor": map[string]any{"kind": "session", "id": "session-retracted"},
	}); retracted.Code != http.StatusOK {
		t.Fatalf("retract ask: status=%d body=%s", retracted.Code, retracted.Body.String())
	}

	rowsByID := func(query string) map[string]issueAskListRow {
		rows := listIssueAskRows(t, handler, issue.Key, query)
		byID := make(map[string]issueAskListRow, len(rows))
		for _, row := range rows {
			byID[row.ID] = row
		}
		if len(rows) != 3 || len(byID) != 3 {
			t.Fatalf("issue ask list %q = %#v, want the open, answered and retracted asks", query, rows)
		}
		return byID
	}

	byID := rowsByID("")
	openRow, answeredRow, retractedRow := byID[open.ID], byID[answeredID], byID[retractedID]
	// Every field a reader already had is still the row's own, not nested under the ask.
	if openRow.State != "open" || openRow.Question != "Which transport should we implement?" || openRow.OpenedEventID == nil {
		t.Fatalf("open ask row = %#v, want the ask's own fields at the row's top level", openRow)
	}
	if answeredRow.State != "answered" || retractedRow.State != "resolved" {
		t.Fatalf("listed states = %q and %q, want answered and resolved", answeredRow.State, retractedRow.State)
	}

	// The newest reply is the one a client sorts by; an ask nobody replied to has none.
	if openRow.LastReply == nil || openRow.LastReply.Author.ID != "session-open-replier" || openRow.LastReply.CreatedAt == "" {
		t.Fatalf("open ask last_reply = %#v, want the replying session's reply", openRow.LastReply)
	}
	if answeredRow.LastReply == nil || answeredRow.LastReply.Author.ID != "session-answer-replier" {
		t.Fatalf("answered ask last_reply = %#v, want the replying session's reply", answeredRow.LastReply)
	}
	if retractedRow.LastReply != nil {
		t.Fatalf("retracted ask last_reply = %#v, want null on an ask nobody replied to", retractedRow.LastReply)
	}

	// Unasked, the thread is not there at all: the readers that render the ask alone pay for no
	// reply body, follower or answer on this unpaged list.
	for _, row := range []issueAskListRow{openRow, answeredRow, retractedRow} {
		if row.Thread != nil {
			t.Fatalf("thread on %q without ?thread= = %#v, want no thread key at all", row.ID, row.Thread)
		}
	}

	threaded := rowsByID("?thread=1")
	openThread, answeredThread := threaded[open.ID].Thread, threaded[answeredID].Thread
	retractedThread := threaded[retractedID].Thread
	if openThread == nil || answeredThread == nil || retractedThread == nil {
		t.Fatalf("?thread=1 threads = %#v, want every row to carry one", threaded)
	}
	if len(openThread.Replies) != 1 || openThread.Replies[0].Body != "Still deciding." {
		t.Fatalf("open ask replies = %#v, want the one reply", openThread.Replies)
	}
	if len(openThread.Edits) != 1 || openThread.Edits[0].Previous.Question != "Which implementation?" {
		t.Fatalf("open ask edits = %#v, want the rewording it went through", openThread.Edits)
	}
	if got := followerSessions(openThread.Followers); !strings.Contains(strings.Join(got, ","), "session-open-replier") {
		t.Fatalf("open ask followers = %v, want the replying session among them", got)
	}
	if len(answeredThread.Answers) != 1 || answeredThread.Answers[0].User != "alice" ||
		len(answeredThread.Answers[0].Selected) != 1 || answeredThread.Answers[0].Selected[0] != "Yes" {
		t.Fatalf("answered ask answers = %#v, want Alice's one answer", answeredThread.Answers)
	}
	if openThread.Answers == nil || retractedThread.Replies == nil ||
		retractedThread.Edits == nil || retractedThread.Answers == nil || retractedThread.Followers == nil {
		t.Fatalf("empty thread parts = %#v and %#v, want arrays rather than null", openThread, retractedThread)
	}

	// The thread the list carries is the one the per-ask fetch it replaces would have returned.
	for _, askID := range []string{open.ID, answeredID, retractedID} {
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read ask %q: status=%d body=%s", askID, read.Code, read.Body.String())
		}
		if expected := decodeBody[issueAskListThread](t, read); !reflect.DeepEqual(*threaded[askID].Thread, expected) {
			t.Fatalf("listed thread for %q = %#v, want the ask read's %#v", askID, *threaded[askID].Thread, expected)
		}
	}

	// ?state= still narrows the same list.
	if filtered := listIssueAskRows(t, handler, issue.Key, "?state=open"); len(filtered) != 1 || filtered[0].ID != open.ID {
		t.Fatalf("?state=open = %#v, want the open ask alone", filtered)
	}
	if filtered := listIssueAskRows(t, handler, issue.Key, "?state=answered"); len(filtered) != 1 || filtered[0].ID != answeredID {
		t.Fatalf("?state=answered = %#v, want the answered ask alone", filtered)
	}

	// A ?thread= the server cannot read is refused, not answered without the thread.
	invalid := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?thread=yes", nil, "alice")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_ASK_THREAD"`) {
		t.Fatalf("list asks with invalid thread: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestIssueAskListWithoutAsksIsAnEmptyArray(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "No asks", "A spec")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("list an issue with no asks: status=%d body=%s, want []", response.Code, response.Body.String())
	}
}
