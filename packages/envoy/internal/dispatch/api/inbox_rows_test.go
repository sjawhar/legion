package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// assignIssue puts an issue on one person, or on nobody when login is empty.
func assignIssue(t *testing.T, handler http.Handler, key, login string) {
	t.Helper()
	assignee := any(nil)
	if login != "" {
		assignee = login
	}
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"assignee": assignee}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("assign %s to %q: status=%d body=%s", key, login, response.Code, response.Body.String())
	}
}

// setIssuePriority gives an issue a coarse priority, which its rows carry.
func setIssuePriority(t *testing.T, handler http.Handler, key string, priority int) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"priority": priority}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("set %s's priority: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

// agentThread is one of the caller's one-off messages to s1, with one reply from s1 under it.
func agentThread(t *testing.T, handler http.Handler, delivery, body, reply string) (root, answer model.Message) {
	t.Helper()
	created := postAgentMessage(t, handler, "s1", map[string]any{"body": body, "delivery": delivery}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("send %s %q: status=%d body=%s", delivery, body, created.Code, created.Body.String())
	}
	root = decodeBody[model.Message](t, created)
	replied := replyTo(t, handler, root.ID, reply)
	if replied.Code != http.StatusCreated {
		t.Fatalf("reply to %q: status=%d body=%s", body, replied.Code, replied.Body.String())
	}
	return root, decodeBody[model.Message](t, replied)
}

// A Reply row carries everything the Inbox draws beside the reply itself: the issue's priority,
// the thread's project, the caller's read mark, how many writes they have not read, the document
// mark an anchored root carries, and that the row is unread.
func TestInboxRowsCarryThePriorityProjectReadMarkCountAndAnchor(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Fields", "The quick brown fox.")
	setIssuePriority(t, handler, issue.Key, 1)
	root := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Which fox?", "anchor": map[string]any{"artifact": "spec", "quote": "brown fox"},
	}, "alice")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	sessionComment(t, handler, comments, map[string]any{"body": "The quick one.", "reply_to": root.ID})
	second := sessionComment(t, handler, comments, map[string]any{"body": "And the lazy dog.", "reply_to": root.ID})

	row := replyRow(t, getMyReplies(t, handler, "alice", ""), root.ID)
	if row.Priority == nil || *row.Priority != 1 {
		t.Fatalf("priority = %v, want 1", row.Priority)
	}
	if row.Project == nil || *row.Project != "TEST" {
		t.Fatalf("project = %v, want TEST", row.Project)
	}
	if row.ReadThrough == nil || !row.ReadThrough.Equal(root.CreatedAt) {
		t.Fatalf("read_through = %v, want alice's own write at %s", row.ReadThrough, root.CreatedAt)
	}
	if !row.Unread || row.UnreadCount != 2 {
		t.Fatalf("unread = %v, unread_count = %d, want true and the two session replies", row.Unread, row.UnreadCount)
	}
	if row.Anchor == nil || row.Anchor.Quote != "brown fox" || row.Anchor.ArtifactID == "" || row.Anchor.MarkID == "" || row.Anchor.Orphaned {
		t.Fatalf("anchor = %#v, want the root's mark on the issue's spec", row.Anchor)
	}
	if row.Session != nil || len(row.UnreadReplyIDs) != 0 {
		t.Fatalf("row = %#v, want no session and no reply ids on a Reply row", row)
	}

	// Reading through the first reply leaves one unread write, and the count follows the mark.
	if marked := markThreadRead(t, handler, "alice", root.ID, second.CreatedAt.Add(-time.Microsecond)); marked.Code != http.StatusNoContent {
		t.Fatalf("mark: status=%d body=%s", marked.Code, marked.Body.String())
	}
	if row := replyRow(t, getMyReplies(t, handler, "alice", ""), root.ID); row.UnreadCount != 1 || !row.Unread {
		t.Fatalf("after reading the first reply: unread_count = %d, unread = %v, want 1 and true", row.UnreadCount, row.Unread)
	}
}

// A message thread's unread count reaches every reply, however deep: the walk follows in_reply_to
// down, as the user_threads trigger walks it up.
func TestInboxRowsCountEveryUnreadMessageInAThread(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Messages", "A spec")
	root := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Status?"}, "alice")
	first := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Green.", "in_reply_to": root.ID, "actor": sessionActor(),
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("session reply: status=%d body=%s", first.Code, first.Body.String())
	}
	deeper := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "And deploying.", "in_reply_to": decodeBody[model.Message](t, first).ID, "actor": sessionActor(),
	})
	if deeper.Code != http.StatusCreated {
		t.Fatalf("deeper session reply: status=%d body=%s", deeper.Code, deeper.Body.String())
	}
	if row := replyRow(t, getMyReplies(t, handler, "alice", ""), root.ID); row.UnreadCount != 2 {
		t.Fatalf("unread_count = %d, want the reply and the reply under it", row.UnreadCount)
	}
}

// kind=agent lists the caller's one-off messages to a session that hold a reply: a BTW, a
// broadcast's copy and a Send the session never took as a turn. A Send whose attempt the session
// accepted as a user turn is a turn of its live conversation, so it is in no row - while its reply
// still counts in the Agents page's New replies badge, which this change does not touch.
func TestInboxRowsListOneOffAgentThreadsAndNotAnAcceptedSend(t *testing.T) {
	handler, _, accepted, acceptedReply, _ := directConversationFrom(t, "alice")
	if response := acceptDelivery(t, handler, accepted.ID, 1, "s1"); response.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", response.Code, response.Body.String())
	}
	if replied := acceptedReply("A card is open."); replied.Code != http.StatusCreated {
		t.Fatalf("reply to the accepted send: status=%d body=%s", replied.Code, replied.Body.String())
	}

	btw, btwReply := agentThread(t, handler, "btw", "Deploy is tonight.", "Noted.")
	send, _ := agentThread(t, handler, "aside", "Where is the runbook?", "In the spec.")

	broadcast := postBroadcast(t, handler, map[string]any{
		"body": "Stand down.", "delivery": "btw", "session_ids": []string{"s1"},
	}, "alice")
	if broadcast.Code != http.StatusCreated {
		t.Fatalf("broadcast: status=%d body=%s", broadcast.Code, broadcast.Body.String())
	}
	recipients := decodeBody[broadcastResponse](t, broadcast).Recipients
	if len(recipients) != 1 {
		t.Fatalf("broadcast recipients = %#v, want s1 alone", recipients)
	}
	copied := recipients[0].Message.ID
	if replied := replyTo(t, handler, copied, "Standing down."); replied.Code != http.StatusCreated {
		t.Fatalf("reply to the broadcast's copy: status=%d body=%s", replied.Code, replied.Body.String())
	}

	page := getMyReplies(t, handler, "alice", "?kind=agent")
	if page.Total != 3 || len(page.Rows) != 3 {
		t.Fatalf("page = %#v, want the BTW, the broadcast's copy and the unaccepted send", page)
	}
	if listsThread(page, accepted.ID) {
		t.Fatalf("the accepted send %s is an Agent row: %#v", accepted.ID, page.Rows)
	}
	for _, want := range []string{btw.ID, send.ID, copied} {
		if !listsThread(page, want) {
			t.Fatalf("thread %s is not listed: %#v", want, page.Rows)
		}
	}
	row := replyRow(t, page, btw.ID)
	if row.Kind != "agent" || row.Ref != "/agents/s1/live" || row.Subject != "Deploy is tonight." {
		t.Fatalf("agent row = %#v, want the BTW named by its body, linking the live view", row)
	}
	if row.Session == nil || row.Session.ID != "s1" || row.Session.Name == "" {
		t.Fatalf("agent row's session = %#v, want s1 by name", row.Session)
	}
	if row.Issue != nil || row.Document != nil || row.Priority != nil || row.Project != nil || row.ReadThrough != nil {
		t.Fatalf("agent row = %#v, want no owner, no priority, no project and no read mark", row)
	}
	if !row.Unread || row.UnreadCount != 1 || len(row.UnreadReplyIDs) != 1 || row.UnreadReplyIDs[0] != btwReply.ID {
		t.Fatalf("agent row = %#v, want its one unread reply %s", row, btwReply.ID)
	}
	if row.Reply.ID != btwReply.ID || row.Reply.Author.Kind != "session" || row.Reply.FirstLine != "Noted." {
		t.Fatalf("agent row's reply = %#v, want the session's reply", row.Reply)
	}

	// An Agent row has no issue and no document, so an owner-narrowed read holds none.
	issue := createInteractionIssue(t, handler, "TEST", "Narrowed", "A spec")
	if narrowed := getMyReplies(t, handler, "alice", "?kind=agent&issue="+issue.Key); narrowed.Total != 0 || len(narrowed.Rows) != 0 {
		t.Fatalf("?issue= page = %#v, want no Agent row", narrowed)
	}
}

// The Agents page's New replies N badge counts an accepted Send's unread reply exactly as before:
// the Inbox's narrowing is in its own query, not in unreadDirectRepliesCTE, which the badge and
// the conversation window share.
func TestAgentStateStillCountsAnAcceptedSendsUnreadReply(t *testing.T) {
	handler, _, root, reply, _ := directConversationFrom(t, "alice")
	if response := acceptDelivery(t, handler, root.ID, 1, "s1"); response.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", response.Code, response.Body.String())
	}
	if replied := reply("Taken as a turn."); replied.Code != http.StatusCreated {
		t.Fatalf("reply: status=%d body=%s", replied.Code, replied.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 1 {
		t.Fatalf("unread_replies = %d, want 1: the badge still counts an accepted Send's reply", got.UnreadReplies)
	}
	if page := getMyReplies(t, handler, "alice", "?kind=agent"); page.Total != 0 || len(page.Rows) != 0 {
		t.Fatalf("agent page = %#v, want no row for a Send the session took as a turn", page)
	}
}

// kind=reply,agent is one list in one order, newest reply first, and total counts both kinds.
func TestInboxRowsMergeReplyAndAgentRowsNewestFirst(t *testing.T) {
	handler, _, _, _, _ := directConversationFrom(t, "alice")
	issue := createInteractionIssue(t, handler, "TEST", "Merged", "A spec")
	older := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ready?"}, "alice")
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Yes.", "reply_to": older.ID})
	newer, _ := agentThread(t, handler, "btw", "One more thing.", "On it.")

	page := getMyReplies(t, handler, "alice", "?kind=reply,agent")
	if page.Total != 2 || len(page.Rows) != 2 {
		t.Fatalf("page = %#v, want the Reply row and the Agent row", page)
	}
	if page.Rows[0].ThreadID != newer.ID || page.Rows[0].Kind != "agent" || page.Rows[1].ThreadID != older.ID {
		t.Fatalf("rows = %#v, want the newest reply first", page.Rows)
	}
	// Each kind on its own answers its own half, and a page of one still counts both.
	if only := getMyReplies(t, handler, "alice", ""); only.Total != 1 || len(only.Rows) != 1 || only.Rows[0].ThreadID != older.ID {
		t.Fatalf("default page = %#v, want the Reply row alone", only)
	}
	one := getMyReplies(t, handler, "alice", "?kind=reply,agent&limit=1")
	if one.Total != 2 || len(one.Rows) != 1 || one.Rows[0].ThreadID != newer.ID || one.Limit != 1 {
		t.Fatalf("limit=1 page = %#v, want the newest row and both kinds in total", one)
	}
	if second := getMyReplies(t, handler, "alice", "?kind=reply,agent&limit=1&offset=1"); len(second.Rows) != 1 || second.Rows[0].ThreadID != older.ID {
		t.Fatalf("offset=1 page = %#v, want the Reply row", second)
	}
}

// state=all lists a thread the caller has read, drawn as read, of either kind; state=unread lists
// neither.
func TestInboxRowsUnderStateAllListReadThreads(t *testing.T) {
	handler, _, _, _, _ := directConversationFrom(t, "alice")
	issue := createInteractionIssue(t, handler, "TEST", "Read", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ready?"}, "alice")
	reply := sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Yes.", "reply_to": root.ID})
	if marked := markThreadRead(t, handler, "alice", root.ID, reply.CreatedAt); marked.Code != http.StatusNoContent {
		t.Fatalf("mark the thread: status=%d body=%s", marked.Code, marked.Body.String())
	}
	agentRoot, agentReply := agentThread(t, handler, "btw", "Deploy is tonight.", "Noted.")
	readReplies := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_replies": []string{agentReply.ID},
	}, "alice")
	if readReplies.Code != http.StatusOK {
		t.Fatalf("mark the agent reply read: status=%d body=%s", readReplies.Code, readReplies.Body.String())
	}

	if unread := getMyReplies(t, handler, "alice", "?kind=reply,agent"); unread.Total != 0 || len(unread.Rows) != 0 {
		t.Fatalf("unread page = %#v, want nothing once both are read", unread)
	}
	all := getMyReplies(t, handler, "alice", "?kind=reply,agent&state=all")
	if all.Total != 2 || len(all.Rows) != 2 {
		t.Fatalf("state=all page = %#v, want both read threads", all)
	}
	if row := replyRow(t, all, root.ID); row.Unread || row.UnreadCount != 0 {
		t.Fatalf("read Reply row = %#v, want it drawn as read", row)
	}
	if row := replyRow(t, all, agentRoot.ID); row.Unread || row.UnreadCount != 0 || len(row.UnreadReplyIDs) != 0 || row.Reply.ID != agentReply.ID {
		t.Fatalf("read Agent row = %#v, want it drawn as read with its newest reply", row)
	}
}

// The de-dupe is exactly the row the Inbox already shows in Waiting on you: an open ask on an open
// issue assigned to the caller. An open ask on another person's issue, on an unassigned issue or
// on a project document is nobody else's row of theirs, so each is a Reply row.
func TestInboxRowsKeepAnOpenAskNobodyElsesInboxRowCovers(t *testing.T) {
	handler := newTestHandler(t)
	createInteractionIssue(t, handler, "TEST", "Asks", "A spec")
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n\nStep one.\n")
	// Bob signs in, so he is a person an issue can be assigned to.
	getMyReplies(t, handler, "bob", "")

	mine := createAnswerListIssue(t, handler, "Mine")
	assignIssue(t, handler, mine.Key, "alice")
	bobs := createAnswerListIssue(t, handler, "Bob's")
	assignIssue(t, handler, bobs.Key, "bob")
	nobodys := createAnswerListIssue(t, handler, "Nobody's")
	assignIssue(t, handler, nobodys.Key, "")

	asks := map[string]string{}
	for key, issueKey := range map[string]string{"mine": mine.Key, "bobs": bobs.Key, "nobodys": nobodys.Key} {
		ask := createAnswerListAsk(t, handler, issueKey, "Which way on "+key+"?")
		createThreadComment(t, handler, issueKey, map[string]any{"body": "Why ask?", "ask_id": ask.ID}, "alice")
		sessionComment(t, handler, "/api/v1/issues/"+issueKey+"/comments", map[string]any{"body": "Because.", "ask_id": ask.ID})
		asks[key] = ask.ID
	}
	onDocument := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Is step one current?", "actor": sessionActor(),
	})
	if onDocument.Code != http.StatusCreated {
		t.Fatalf("document ask: status=%d body=%s", onDocument.Code, onDocument.Body.String())
	}
	documentAsk := decodeBody[model.Ask](t, onDocument)
	alicesReply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
		"body": "Why?", "ask_id": documentAsk.ID,
	}, "alice")
	if alicesReply.Code != http.StatusCreated {
		t.Fatalf("alice's reply on the document ask: status=%d body=%s", alicesReply.Code, alicesReply.Body.String())
	}
	sessionComment(t, handler, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
		"body": "Because.", "ask_id": documentAsk.ID,
	})

	page := getMyReplies(t, handler, "alice", "")
	if listsThread(page, asks["mine"]) {
		t.Fatalf("an open ask on alice's own open issue is listed: %#v", page.Rows)
	}
	for name, id := range map[string]string{"bob's issue": asks["bobs"], "an unassigned issue": asks["nobodys"], "a project document": documentAsk.ID} {
		if !listsThread(page, id) {
			t.Fatalf("the open ask on %s is not listed: %#v", name, page.Rows)
		}
	}
	if page.Total != 3 {
		t.Fatalf("total = %d, want the three asks no Inbox row of alice's covers", page.Total)
	}
}

// as_of is the database time of the snapshot the rows were read in, so it is at or after every
// row's newest reply and no row the answer holds was written after it.
func TestInboxRowsAnswerWithTheSnapshotsOwnTime(t *testing.T) {
	handler, _, _, _, _ := directConversationFrom(t, "alice")
	issue := createInteractionIssue(t, handler, "TEST", "As of", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Ready?"}, "alice")
	sessionComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "Yes.", "reply_to": root.ID})
	agentThread(t, handler, "btw", "Deploy is tonight.", "Noted.")

	before := time.Now()
	page := getMyReplies(t, handler, "alice", "?kind=reply,agent")
	if page.AsOf.IsZero() || len(page.Rows) != 2 {
		t.Fatalf("page = %#v, want both rows and an as_of", page)
	}
	for _, row := range page.Rows {
		if page.AsOf.Before(row.Reply.CreatedAt) {
			t.Fatalf("as_of %s is before %s's reply at %s", page.AsOf, row.ThreadID, row.Reply.CreatedAt)
		}
	}
	if page.AsOf.Before(before.Add(-time.Minute)) || page.AsOf.After(time.Now().Add(time.Minute)) {
		t.Fatalf("as_of = %s, want the database's own clock around %s", page.AsOf, before)
	}
}

// An absent limit answers every row, where the page read 50 before, and names that in total; a
// limit the caller gives still pages.
func TestInboxRowsWithNoLimitAnswerEveryThread(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Every row", "A spec")
	const threads = 51
	for index := range threads {
		var root string
		if err := database.Pool.QueryRow(context.Background(), `
			insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2)
			returning id::text
		`, issue.Key, fmt.Sprintf("Thread %d", index)).Scan(&root); err != nil {
			t.Fatalf("seed thread %d: %v", index, err)
		}
		if _, err := database.Pool.Exec(context.Background(), `
			insert into comments (issue_key, author, body, reply_to)
			values ($1, '{"kind":"session","id":"s1"}', $2, $3::uuid)
		`, issue.Key, fmt.Sprintf("Reply %d", index), root); err != nil {
			t.Fatalf("seed reply %d: %v", index, err)
		}
	}

	page := getMyReplies(t, handler, "alice", "")
	if page.Total != threads || len(page.Rows) != page.Total || page.Limit != 0 {
		t.Fatalf("unpaged page = %d rows of %d (limit %d), want every one of %d", len(page.Rows), page.Total, page.Limit, threads)
	}
	paged := getMyReplies(t, handler, "alice", "?limit=50")
	if paged.Total != threads || len(paged.Rows) != 50 || paged.Limit != 50 {
		t.Fatalf("limit=50 page = %d rows of %d, want 50", len(paged.Rows), paged.Total)
	}
	last := getMyReplies(t, handler, "alice", "?limit=50&offset=50")
	if len(last.Rows) != 1 || last.Rows[0].ThreadID != page.Rows[50].ThreadID {
		t.Fatalf("offset=50 page = %#v, want the row the first page left", last.Rows)
	}
}

// What the read refuses, each before it opens its transaction.
func TestInboxRowsRefuseAKindStateOrPageTheyCannotRead(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Refusals", "A spec")
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n")
	for _, query := range []string{
		"?kind=asks",
		"?kind=reply,agents",
		"?state=read",
		"?limit=0",
		"?limit=201",
		"?issue=" + issue.Key + "&artifact=" + document.ID,
		"?artifact=not-a-uuid",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/replies"+query, nil, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_QUERY"`) {
			t.Fatalf("%s: status=%d body=%s, want 400 INVALID_QUERY", query, response.Code, response.Body.String())
		}
	}
	// An empty kind or state is the default, not a refusal.
	for _, query := range []string{"?kind=", "?state=", "?kind=reply&state=unread"} {
		if page := getMyReplies(t, handler, "alice", query); page.Total != 0 {
			t.Fatalf("%s: page = %#v, want the default read", query, page)
		}
	}
}
