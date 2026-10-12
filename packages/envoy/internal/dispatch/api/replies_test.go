package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func getMyReplies(t *testing.T, handler http.Handler, login, query string) model.MyRepliesResponse {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/replies"+query, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("list my replies%s: status=%d body=%s", query, response.Code, response.Body.String())
	}
	return decodeBody[model.MyRepliesResponse](t, response)
}

// replyRow is the row for threadID, failing the test when the page does not list it.
func replyRow(t *testing.T, page model.MyRepliesResponse, threadID string) model.MyReplyRow {
	t.Helper()
	for _, row := range page.Rows {
		if row.ThreadID == threadID {
			return row
		}
	}
	t.Fatalf("thread %s is not on the page: %#v", threadID, page.Rows)
	return model.MyReplyRow{}
}

func listsThread(page model.MyRepliesResponse, threadID string) bool {
	for _, row := range page.Rows {
		if row.ThreadID == threadID {
			return true
		}
	}
	return false
}

// sessionComment posts a comment as the shared token's session.
func sessionComment(t *testing.T, handler http.Handler, path string, input map[string]any) model.Comment {
	t.Helper()
	input["actor"] = sessionActor()
	response := sessionRequest(t, handler, http.MethodPost, path, input)
	if response.Code != http.StatusCreated {
		t.Fatalf("session comment on %s: status=%d body=%s", path, response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

func markThreadRead(t *testing.T, handler http.Handler, login, threadID string, through time.Time) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/threads/"+threadID+"/read", map[string]string{
		"read_through": through.UTC().Format(time.RFC3339Nano),
	}, login)
}

func countEvents(t *testing.T, database *store.Store, eventType string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from events where type = $1`, eventType).Scan(&count); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return count
}

// A comment thread Alice wrote in reaches her Replies page once a session replies there: one row
// naming the thread's root, its subject and the reply, and a total of one. Bob, who wrote nothing
// there, sees none of it.
func TestMyRepliesListsACommentThreadASessionRepliedIn(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Replies", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ship it on Friday?\nOr wait."}, "alice")
	if page := getMyReplies(t, handler, "alice", ""); page.Total != 0 || len(page.Rows) != 0 {
		t.Fatalf("before any reply: %#v, want an empty page", page)
	}
	reply := sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Friday works.\nDetails below.", "reply_to": root.ID,
	})

	page := getMyReplies(t, handler, "alice", "")
	if page.Total != 1 || len(page.Rows) != 1 || page.Limit != 50 || page.Offset != 0 {
		t.Fatalf("page = %#v, want one row of the default page", page)
	}
	row := page.Rows[0]
	if row.ThreadID != root.ID || row.Kind != "comment" || row.Ref != "/issues/"+issue.Key+"/comments/"+root.ID {
		t.Fatalf("row = %#v, want the comment thread's root and its item route", row)
	}
	if row.Issue == nil || row.Issue.Key != issue.Key || row.Issue.Title != "Replies" || row.Document != nil {
		t.Fatalf("row owner = %#v, want the issue", row)
	}
	if row.Subject != "Ship it on Friday?" {
		t.Fatalf("subject = %q, want the root's first line", row.Subject)
	}
	if row.Reply.ID != reply.ID || row.Reply.Author.Kind != "session" || row.Reply.FirstLine != "Friday works." || !row.Reply.CreatedAt.Equal(reply.CreatedAt) {
		t.Fatalf("reply = %#v, want the session's reply %s", row.Reply, reply.ID)
	}
	if bob := getMyReplies(t, handler, "bob", ""); bob.Total != 0 || len(bob.Rows) != 0 {
		t.Fatalf("bob's page = %#v, want none of alice's rows", bob)
	}
}

// What the page leaves out and what it keeps: an open ask the Inbox lists is left out, since its row
// in Waiting on you already shows the reply; an open ask on a closed issue, which the Inbox hides,
// stays; so do an answered ask's later reply and a resolved comment thread, until read.
func TestMyRepliesKeepsWhatTheInboxDoesNotShow(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Open ask", "A spec")
	closing := createAnswerListIssue(t, handler, "Closing")
	answered := createAnswerListIssue(t, handler, "Answered")

	open := createAnswerListAsk(t, handler, issue.Key, "Which way?")
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "Why ask?", "ask_id": open.ID}, "alice")
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Because.", "ask_id": open.ID})

	onClosed := createAnswerListAsk(t, handler, closing.Key, "Still need this?")
	createThreadComment(t, handler, closing.Key, map[string]any{"body": "Maybe.", "ask_id": onClosed.ID}, "alice")
	sessionComment(t, handler, "/api/v1/issues/"+closing.Key+"/comments", map[string]any{"body": "Noted.", "ask_id": onClosed.ID})

	decided := createAnswerListAsk(t, handler, answered.Key, "Ship?")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+decided.ID+"/answer", map[string]any{"text": "Yes."}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer: status=%d body=%s", response.Code, response.Body.String())
	}
	after := sessionComment(t, handler, "/api/v1/issues/"+answered.Key+"/comments", map[string]any{"body": "Shipped, thanks.", "ask_id": decided.ID})

	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Resolve me"}, "alice")
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Done.", "reply_to": root.ID})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+root.ID+"/resolve", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("resolve: status=%d body=%s", response.Code, response.Body.String())
	}

	page := getMyReplies(t, handler, "alice", "")
	if listsThread(page, open.ID) || listsThread(page, onClosed.ID) {
		t.Fatalf("page = %#v, want neither open ask while both issues are open", page.Rows)
	}
	if row := replyRow(t, page, decided.ID); row.Kind != "ask" || row.Ref != "/issues/"+answered.Key+"/asks/"+decided.ID || row.Subject != "Ship?" || row.Reply.ID != after.ID {
		t.Fatalf("answered ask row = %#v", row)
	}
	replyRow(t, page, root.ID)
	if page.Total != 2 {
		t.Fatalf("total = %d, want 2", page.Total)
	}

	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closing.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	page = getMyReplies(t, handler, "alice", "")
	if listsThread(page, open.ID) {
		t.Fatalf("the open ask on an open issue is listed: %#v", page.Rows)
	}
	if row := replyRow(t, page, onClosed.ID); row.Kind != "ask" {
		t.Fatalf("open ask on a closed issue = %#v", row)
	}
	if page.Total != 3 {
		t.Fatalf("total = %d, want 3", page.Total)
	}
}

// An answer is a write like a reply: when Bob answers the ask Alice asked, her row reads as his
// answer (who answered, the chosen option and the answer's text), naming the ask. A mark through
// the answer's own time, which carries nanoseconds, clears it, as the ask's card sends it.
func TestMyRepliesListsAnAnswerSomeoneElseGave(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Regions", "A spec")
	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which region?",
		"options":  []map[string]string{{"label": "eu-west-1"}, {"label": "us-east-1"}},
	}, "alice")
	if asked.Code != http.StatusCreated {
		t.Fatalf("alice's ask: status=%d body=%s", asked.Code, asked.Body.String())
	}
	ask := decodeBody[model.Ask](t, asked)
	if page := getMyReplies(t, handler, "alice", ""); page.Total != 0 {
		t.Fatalf("before the answer: %#v, want nothing (an open ask the Inbox lists)", page)
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"selected": []string{"eu-west-1"}, "text": "Closest to the users.",
	}, "bob")
	if answered.Code != http.StatusOK {
		t.Fatalf("bob's answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[model.Ask](t, answered).Answer

	row := replyRow(t, getMyReplies(t, handler, "alice", ""), ask.ID)
	if row.Kind != "ask" || row.Subject != "Which region?" || row.Reply.Kind != "answer" || row.Reply.ID != ask.ID {
		t.Fatalf("row = %#v, want the ask with its answer as the reply", row)
	}
	if row.Reply.Author != (model.Actor{Kind: "user", ID: "bob"}) || row.Reply.FirstLine != "eu-west-1 - Closest to the users." {
		t.Fatalf("reply = %#v, want bob's answer with its option and text", row.Reply)
	}
	if bob := getMyReplies(t, handler, "bob", ""); bob.Total != 0 {
		t.Fatalf("bob's page = %#v, want his own answer read", bob)
	}

	if marked := markThreadRead(t, handler, "alice", ask.ID, answer.At); marked.Code != http.StatusNoContent {
		t.Fatalf("mark: status=%d body=%s", marked.Code, marked.Body.String())
	}
	if page := getMyReplies(t, handler, "alice", ""); page.Total != 0 {
		t.Fatalf("after a mark through the answer's own time: %#v, want it read", page)
	}
}

// A comment on a passage of an issue's document, a comment on a project document and an issue
// message thread each reach the page, each linking to its root; ?issue= and ?artifact= narrow the
// rows to one owner.
func TestMyRepliesListsDocumentAndMessageThreads(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Documents", "The quick brown fox.")
	other := createAnswerListIssue(t, handler, "Other")
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n\nStep one.\n")

	anchored := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Which fox?", "anchor": map[string]any{"artifact": "spec", "quote": "brown fox"},
	}, "alice")
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "The quick one.", "reply_to": anchored.ID})

	onDocument := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{"body": "Is step one current?"}, "alice")
	if onDocument.Code != http.StatusCreated {
		t.Fatalf("document comment: status=%d body=%s", onDocument.Code, onDocument.Body.String())
	}
	documentRoot := decodeBody[model.Comment](t, onDocument)
	sessionComment(t, handler, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{"body": "Yes.", "reply_to": documentRoot.ID})

	message := createIssueMessage(t, handler, other.Key, map[string]any{"body": "Status?"}, "alice")
	answer := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+other.Key+"/messages", map[string]any{
		"body": "Green.", "in_reply_to": message.ID, "actor": sessionActor(),
	})
	if answer.Code != http.StatusCreated {
		t.Fatalf("session message reply: status=%d body=%s", answer.Code, answer.Body.String())
	}
	messageReply := decodeBody[model.Message](t, answer)

	page := getMyReplies(t, handler, "alice", "")
	if page.Total != 3 {
		t.Fatalf("total = %d, want 3: %#v", page.Total, page.Rows)
	}
	if row := replyRow(t, page, anchored.ID); row.Kind != "comment" || row.Ref != "/issues/"+issue.Key+"/comments/"+anchored.ID {
		t.Fatalf("anchored comment row = %#v", row)
	}
	row := replyRow(t, page, documentRoot.ID)
	if row.Document == nil || row.Document.Project != "TEST" || row.Document.Slug != document.Slug || row.Issue != nil ||
		row.Ref != "/projects/TEST/documents/"+document.Slug+"?comment="+documentRoot.ID {
		t.Fatalf("project document row = %#v", row)
	}
	if row := replyRow(t, page, message.ID); row.Kind != "message" || row.Ref != "/issues/"+other.Key+"/messages/"+message.ID ||
		row.Subject != "Status?" || row.Reply.ID != messageReply.ID || row.Reply.FirstLine != "Green." {
		t.Fatalf("message row = %#v", row)
	}

	byIssue := getMyReplies(t, handler, "alice", "?issue="+issue.Key)
	if byIssue.Total != 1 || len(byIssue.Rows) != 1 || byIssue.Rows[0].ThreadID != anchored.ID {
		t.Fatalf("?issue= page = %#v", byIssue)
	}
	byDocument := getMyReplies(t, handler, "alice", "?artifact="+document.ID)
	if byDocument.Total != 1 || len(byDocument.Rows) != 1 || byDocument.Rows[0].ThreadID != documentRoot.ID {
		t.Fatalf("?artifact= page = %#v", byDocument)
	}
	for _, query := range []string{"?issue=" + issue.Key + "&artifact=" + document.ID, "?limit=0", "?artifact=not-a-uuid"} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/replies"+query, nil, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_QUERY"`) {
			t.Fatalf("%s: status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
	if bearer := sessionRequest(t, handler, http.MethodGet, "/api/v1/me/replies", nil); bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("bearer: status=%d body=%s", bearer.Code, bearer.Body.String())
	}
}

// Authorship is the author's kind: a session running under Alice's own personal token is still a
// session, so its reply reaches her page. And one person is one login however it is cased.
func TestMyRepliesCountsASessionUnderThePersonsOwnToken(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Own token", "A spec")
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "mine"}, "alice")
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	token := decodeBody[agentTokenResponse](t, minted).Token

	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Can you check?"}, "alice")
	reply := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Checked.", "reply_to": root.ID, "actor": map[string]any{"kind": "session", "id": "alices-session"},
	}, token)
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply under alice's token: status=%d body=%s", reply.Code, reply.Body.String())
	}
	if row := replyRow(t, getMyReplies(t, handler, "alice", ""), root.ID); row.Reply.Author.ID != "alices-session" {
		t.Fatalf("row = %#v, want the session's reply", row)
	}

	var cased string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"Alice"}', 'Cased.')
		returning id::text
	`, issue.Key).Scan(&cased); err != nil {
		t.Fatalf("insert a cased comment: %v", err)
	}
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Seen.", "reply_to": cased})
	replyRow(t, getMyReplies(t, handler, "ALICE", ""), cased)
}

// Marking a thread read clears its row and announces itself once, on a never-notifying event the
// viewer's other tabs hear; a repeat writes nothing. A later reply brings the row back, and a thread
// on a closed issue can still be marked.
func TestMarkThreadReadClearsTheRowUntilTheNextReply(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Mark", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ready?"}, "alice")
	first := sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Yes.", "reply_to": root.ID})

	if response := markThreadRead(t, handler, "alice", root.ID, first.CreatedAt); response.Code != http.StatusNoContent {
		t.Fatalf("mark: status=%d body=%s", response.Code, response.Body.String())
	}
	if listsThread(getMyReplies(t, handler, "alice", ""), root.ID) {
		t.Fatalf("a read thread is still listed")
	}
	if got := countEvents(t, database, "user_thread_state.updated"); got != 1 {
		t.Fatalf("%d user_thread_state.updated events, want 1", got)
	}
	var login, thread, project string
	var notify bool
	if err := database.Pool.QueryRow(context.Background(), `
		select payload->>'login', payload->>'thread_id', project_key, notify from events where type = 'user_thread_state.updated'
	`).Scan(&login, &thread, &project, &notify); err != nil {
		t.Fatalf("read the event: %v", err)
	}
	if login != "alice" || thread != root.ID || project != "TEST" || notify {
		t.Fatalf("event = %s %s %s notify=%v, want alice's mark on %s owned by TEST, not notifying", login, thread, project, notify, root.ID)
	}
	if response := markThreadRead(t, handler, "alice", root.ID, first.CreatedAt); response.Code != http.StatusNoContent {
		t.Fatalf("repeat mark: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := countEvents(t, database, "user_thread_state.updated"); got != 1 {
		t.Fatalf("a repeat mark appended an event: %d, want 1", got)
	}

	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "One more thing.", "reply_to": root.ID})
	replyRow(t, getMyReplies(t, handler, "alice", ""), root.ID)

	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	if response := markThreadRead(t, handler, "alice", root.ID, time.Now()); response.Code != http.StatusNoContent {
		t.Fatalf("mark on a closed issue: status=%d body=%s", response.Code, response.Body.String())
	}
	if listsThread(getMyReplies(t, handler, "alice", ""), root.ID) {
		t.Fatalf("a thread marked on a closed issue is still listed")
	}

	// A thread the caller never wrote in writes nothing.
	if response := markThreadRead(t, handler, "bob", root.ID, time.Now()); response.Code != http.StatusNoContent {
		t.Fatalf("bob's mark: status=%d body=%s", response.Code, response.Body.String())
	}
	var bobRows int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from user_threads where login = 'bob'`).Scan(&bobRows); err != nil || bobRows != 0 {
		t.Fatalf("bob's rows = %d (%v), want none", bobRows, err)
	}
}

func TestMarkThreadReadRefusesWhatItCannotStore(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Refusals", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Root"}, "alice")
	if response := markThreadRead(t, handler, "alice", root.ID, time.Now().Add(2*time.Minute)); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_STATE"`) {
		t.Fatalf("future mark: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := markThreadRead(t, handler, "alice", "not-a-uuid", time.Now()); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"THREAD_ID_INPUT"`) {
		t.Fatalf("non-uuid thread: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/threads/"+root.ID+"/read", map[string]string{}, "alice"); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_STATE"`) {
		t.Fatalf("missing read_through: status=%d body=%s", response.Code, response.Body.String())
	}
	if bearer := sessionRequest(t, handler, http.MethodPut, "/api/v1/me/threads/"+root.ID+"/read", map[string]string{"read_through": time.Now().UTC().Format(time.RFC3339)}); bearer.Code != http.StatusForbidden {
		t.Fatalf("bearer mark: status=%d body=%s", bearer.Code, bearer.Body.String())
	}
}

// raceRounds is how many times each race below runs its two writes at once.
const raceRounds = 50

// race starts both writes together and waits for both, failing the test when either does not
// finish within the window a deadlock would outlast.
func race(t *testing.T, round int, first, second func() *httptest.ResponseRecorder) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var a, b *httptest.ResponseRecorder
	wg.Add(2)
	go func() { defer wg.Done(); <-start; a = first() }()
	go func() { defer wg.Done(); <-start; b = second() }()
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("round %d: the two writes did not both finish within 10 s", round)
	}
	return a, b
}

// readThrough is Alice's read mark on a thread.
func readThrough(t *testing.T, database *store.Store, threadID string) (time.Time, *time.Time) {
	t.Helper()
	var through time.Time
	var replied *time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select read_through, replied_at from user_threads where login = 'alice' and thread_id = $1
	`, threadID).Scan(&through, &replied); err != nil {
		t.Fatalf("read alice's row: %v", err)
	}
	return through, replied
}

// A mark and a session's reply in the same thread, at once: neither waits on the other forever, the
// mark is never lost, and the reply's time is never lost either. One variant is a comment thread,
// the other an issue message thread answered through the session's message reply route
// (POST /api/v1/messages/{id}/reply), which locks the issue before it inserts.
func TestMarkThreadReadRacingASessionReply(t *testing.T) {
	t.Run("comment thread", func(t *testing.T) {
		handler, database := newTestHandlerWithStore(t)
		issue := createInteractionIssue(t, handler, "TEST", "Race", "A spec")
		root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Racing"}, "alice")
		for round := range raceRounds {
			mark := time.Now()
			marked, replied := race(t, round,
				func() *httptest.ResponseRecorder { return markThreadRead(t, handler, "alice", root.ID, mark) },
				func() *httptest.ResponseRecorder {
					return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
						"body": "Reply", "reply_to": root.ID, "actor": sessionActor(),
					})
				})
			if marked.Code != http.StatusNoContent || replied.Code != http.StatusCreated {
				t.Fatalf("round %d: mark %d %s, reply %d %s", round, marked.Code, marked.Body.String(), replied.Code, replied.Body.String())
			}
			reply := decodeBody[model.Comment](t, replied)
			through, repliedAt := readThrough(t, database, root.ID)
			if through.Before(mark.Truncate(time.Microsecond)) {
				t.Fatalf("round %d: read mark %s lost the mark %s", round, through, mark)
			}
			if repliedAt == nil || repliedAt.Before(reply.CreatedAt) {
				t.Fatalf("round %d: replied_at %v lost the reply at %s", round, repliedAt, reply.CreatedAt)
			}
		}
	})
	t.Run("message thread", func(t *testing.T) {
		live := true
		sent := []map[string]any{}
		listener := sessionListener(t, &live, &sent)
		defer listener.Close()
		handler, database := newTargetedMessageHandler(t, listener.URL)
		issue := createInteractionIssue(t, handler, "TEST", "Message race", "A spec")
		root := createIssueMessage(t, handler, issue.Key, map[string]any{
			"body": "Racing", "target": "session:s1", "delivery": "btw",
		}, "alice")
		for round := range raceRounds {
			mark := time.Now()
			body := "Reply " + time.Now().Format(time.RFC3339Nano)
			marked, replied := race(t, round,
				func() *httptest.ResponseRecorder { return markThreadRead(t, handler, "alice", root.ID, mark) },
				func() *httptest.ResponseRecorder {
					return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply?follow_up=true", map[string]any{
						"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": body,
					})
				})
			if marked.Code != http.StatusNoContent || replied.Code != http.StatusCreated {
				t.Fatalf("round %d: mark %d %s, reply %d %s", round, marked.Code, marked.Body.String(), replied.Code, replied.Body.String())
			}
			reply := decodeBody[model.Message](t, replied)
			through, repliedAt := readThrough(t, database, root.ID)
			if through.Before(mark.Truncate(time.Microsecond)) {
				t.Fatalf("round %d: read mark %s lost the mark %s", round, through, mark)
			}
			if repliedAt == nil || repliedAt.Before(reply.CreatedAt) {
				t.Fatalf("round %d: replied_at %v lost the reply at %s", round, repliedAt, reply.CreatedAt)
			}
		}
	})
}

// A mark racing an answer to a block ask, whose attribute write refreshes a comment's anchor and so
// appends that event before the answer's update moves Alice's row (docs/marks.go's refreshAnchors,
// then ask_transitions.go's update asks): the mark takes the issue before Alice's row, so it never
// holds the row the answer's trigger waits on while waiting for the event lock the answer holds.
func TestMarkThreadReadRacingABlockAskAnswerThatRefreshesAnAnchor(t *testing.T) {
	handler, database := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Block race", "decision", transportAsk, "Which transport?")
	anchored := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Say which.", "anchor": map[string]any{"artifact": "spec", "quote": "Which transport"},
	}, "bob")
	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{"selected": []string{"REST"}}, "alice")
	if first.Code != http.StatusOK {
		t.Fatalf("first answer: status=%d body=%s", first.Code, first.Body.String())
	}
	answerAt := decodeBody[model.Ask](t, first).Answer.At
	refreshes := countEvents(t, database, "comment.anchor_refreshed")
	for round := range raceRounds {
		// A stale quote makes the answer's attribute write refresh the anchor, appending its event
		// ahead of the answer's own update; a session reply leaves Alice's row unread to mark.
		if _, err := database.Pool.Exec(context.Background(), `
			update comments set anchor = jsonb_set(anchor, '{quote}', to_jsonb('stale'::text)) where id = $1
		`, anchored.ID); err != nil {
			t.Fatalf("round %d: stale the anchor: %v", round, err)
		}
		sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Reply", "ask_id": askID})
		selected := []string{"REST", "gRPC"}[round%2]
		mark := time.Now()
		marked, answered := race(t, round,
			func() *httptest.ResponseRecorder { return markThreadRead(t, handler, "alice", askID, mark) },
			func() *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
					"selected": []string{selected}, "expected_answer_at": timestampValue(answerAt),
				}, "alice")
			})
		if marked.Code != http.StatusNoContent || answered.Code != http.StatusOK {
			t.Fatalf("round %d: mark %d %s, answer %d %s", round, marked.Code, marked.Body.String(), answered.Code, answered.Body.String())
		}
		answerAt = decodeBody[model.Ask](t, answered).Answer.At
		through, _ := readThrough(t, database, askID)
		if through.Before(mark.Truncate(time.Microsecond)) {
			t.Fatalf("round %d: read mark %s lost the mark %s", round, through, mark)
		}
	}
	if got := countEvents(t, database, "comment.anchor_refreshed"); got < refreshes+raceRounds {
		t.Fatalf("%d anchor refreshes over %d rounds, want one a round: the race never took the refresh path", got-refreshes, raceRounds)
	}
}
