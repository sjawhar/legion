package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// requireAnswers fails unless the comment's answers is want (nil: it answers no comment).
func requireAnswers(t *testing.T, what string, got *string, want *string) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("%s answers = %s, want null", what, *got)
	case want != nil && got == nil:
		t.Fatalf("%s answers = null, want %s", what, *want)
	case want != nil && *got != *want:
		t.Fatalf("%s answers = %s, want %s", what, *got, *want)
	}
}

// requireReplyTo fails unless the comment's reply_to is exactly want.
func requireReplyTo(t *testing.T, what string, got *string, want string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s reply_to = %v, want %s", what, got, want)
	}
}

// A reply keeps the comment it answers. The write climbs the reply chain to the head of the
// thread before it stores, so reply_to holds the head and the comment the writer answered was
// lost; comments.answers holds the comment the request named. In a three-deep thread the leaf's
// reply_to is still the root, exactly as before, and its answers is the middle comment; a reply
// to the root records the root in both, so a reader never has to guess which replies carry it.
func TestAReplyKeepsTheCommentItAnswers(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Nested replies", "A spec")

	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The root."}, "alice")
	if root.ReplyTo != nil {
		t.Fatalf("the root's reply_to = %s, want null", *root.ReplyTo)
	}
	requireAnswers(t, "the root", root.Answers, nil)

	middle := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the root.", "reply_to": root.ID,
	}, "bob")
	requireReplyTo(t, "a reply to the root", middle.ReplyTo, root.ID)
	requireAnswers(t, "a reply to the root", middle.Answers, &root.ID)

	leaf := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the reply.", "reply_to": middle.ID,
	}, "alice")
	requireReplyTo(t, "a reply to a reply", leaf.ReplyTo, root.ID)
	requireAnswers(t, "a reply to a reply", leaf.Answers, &middle.ID)

	// The rows themselves, not only the write's answer.
	for _, row := range []struct {
		what string
		id   string
		want *string
	}{
		{"the root's row", root.ID, nil},
		{"the middle comment's row", middle.ID, &root.ID},
		{"the leaf's row", leaf.ID, &middle.ID},
	} {
		var stored *string
		if err := database.Pool.QueryRow(context.Background(),
			`select answers::text from comments where id = $1`, row.id).Scan(&stored); err != nil {
			t.Fatalf("read answers of %s: %v", row.what, err)
		}
		requireAnswers(t, row.what, stored, row.want)
	}

	// Every read carries it beside reply_to.
	read := readCommentThread(t, handler, leaf.ID)
	requireReplyTo(t, "GET the leaf", read.Comment.ReplyTo, root.ID)
	requireAnswers(t, "GET the leaf", read.Comment.Answers, &middle.ID)
	thread := readCommentThread(t, handler, root.ID)
	if len(thread.Replies) != 2 {
		t.Fatalf("the root's replies = %#v, want the two replies", thread.Replies)
	}
	for _, reply := range thread.Replies {
		requireReplyTo(t, "a reply in the root's thread", reply.ReplyTo, root.ID)
		switch reply.ID {
		case middle.ID:
			requireAnswers(t, "the middle comment in the thread read", reply.Answers, &root.ID)
		case leaf.ID:
			requireAnswers(t, "the leaf in the thread read", reply.Answers, &middle.ID)
		default:
			t.Fatalf("the root's thread holds an unexpected reply %s", reply.ID)
		}
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the issue's comments: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, comment := range decodeBody[[]model.Comment](t, listed) {
		switch comment.ID {
		case root.ID:
			requireAnswers(t, "the root in the issue's comment list", comment.Answers, nil)
		case middle.ID:
			requireAnswers(t, "the middle comment in the issue's comment list", comment.Answers, &root.ID)
		case leaf.ID:
			requireAnswers(t, "the leaf in the issue's comment list", comment.Answers, &middle.ID)
		}
	}
}

// A reply that names an ask and no comment answers no comment: its thread hangs under the ask,
// which ask_id records, and answers stays null.
func TestAReplyNamingAnAskAnswersNoComment(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Replies to an ask", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Which day do we ship?")

	onAsk := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Monday works.", "ask_id": ask.ID,
	}, "alice")
	if onAsk.AskID == nil || *onAsk.AskID != ask.ID {
		t.Fatalf("the reply's ask_id = %v, want %s", onAsk.AskID, ask.ID)
	}
	requireAnswers(t, "a reply naming the ask", onAsk.Answers, nil)
	if onAsk.ReplyTo != nil {
		t.Fatalf("a reply naming the ask has reply_to = %s, want null", *onAsk.ReplyTo)
	}
	var stored *string
	if err := database.Pool.QueryRow(context.Background(),
		`select answers::text from comments where id = $1`, onAsk.ID).Scan(&stored); err != nil {
		t.Fatalf("read answers of the ask reply: %v", err)
	}
	requireAnswers(t, "the ask reply's row", stored, nil)
	requireAnswers(t, "GET the ask reply", readCommentThread(t, handler, onAsk.ID).Comment.Answers, nil)

	// A reply to that reply still answers it, and its ask_id is the ask the climb reached.
	under := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Monday then.", "reply_to": onAsk.ID,
	}, "bob")
	if under.AskID == nil || *under.AskID != ask.ID {
		t.Fatalf("the nested reply's ask_id = %v, want the thread's ask %s", under.AskID, ask.ID)
	}
	requireAnswers(t, "a reply under the ask's thread", under.Answers, &onAsk.ID)
}

// A reply that names a block of a comment keeps both pointers: replies_to_block for the block
// and answers for the comment it is a block of, while reply_to stays the thread's head.
func TestAReplyNamingABlockKeepsBothPointers(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A block and its comment", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The root."}, "alice")
	middle := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "Alpha.\n\nBeta.", "reply_to": root.ID,
	}, "bob")
	beta := &model.ReplyBlock{
		Node:    "comment/" + middle.ID + "#b2",
		Ref:     "dispatch://" + issue.Key + "/comment/" + middle.ID + "#b-b2",
		BlockID: "b2",
		Text:    "Beta.",
	}

	onBlock := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On beta.", "reply_to": middle.ID, "reply_to_block": "b2",
	}, "alice")
	requireReplyTo(t, "a reply naming a block", onBlock.ReplyTo, root.ID)
	requireReplyBlock(t, "a reply naming a block", onBlock.ReplyBlock, beta)
	requireAnswers(t, "a reply naming a block", onBlock.Answers, &middle.ID)

	var block, answers *string
	if err := database.Pool.QueryRow(context.Background(),
		`select replies_to_block, answers::text from comments where id = $1`, onBlock.ID).Scan(&block, &answers); err != nil {
		t.Fatalf("read the reply's pointers: %v", err)
	}
	if block == nil || *block != beta.Node {
		t.Fatalf("replies_to_block = %v, want %s", block, beta.Node)
	}
	requireAnswers(t, "the row of a reply naming a block", answers, &middle.ID)

	read := readCommentThread(t, handler, onBlock.ID)
	requireReplyBlock(t, "GET a reply naming a block", read.Comment.ReplyBlock, beta)
	requireAnswers(t, "GET a reply naming a block", read.Comment.Answers, &middle.ID)
	parentRef := "dispatch://" + issue.Key + "/comment/" + middle.ID
	if edges := readGraph(t, handler, url.Values{"to": {parentRef}}).Edges; len(edges) != 1 ||
		edges[0].Kind != "replies_to" || edges[0].Node.ID != onBlock.ID || edges[0].Block == nil || edges[0].Block.Ref != beta.Ref {
		t.Errorf("a nested comment's block reply is listed %#v, want once under its block", edges)
	}
	if edges := readGraph(t, handler, url.Values{"to": {parentRef}, "kind": {"answers"}}).Edges; len(edges) != 1 ||
		edges[0].Kind != "answers" || edges[0].Node.ID != onBlock.ID {
		t.Fatalf("explicit answers to the nested comment = %#v, want its block reply", edges)
	}
	replyRef := "dispatch://" + issue.Key + "/comment/" + onBlock.ID
	links := readGraph(t, handler, url.Values{"from": {replyRef}}).Edges
	if len(links) != 2 {
		t.Errorf("a block reply's default links = %#v, want its thread and block parent", links)
	}
	for _, edge := range links {
		if edge.Kind != "replies_to" || (edge.Node.ID != root.ID && edge.Node.ID != beta.Node) {
			t.Errorf("a block reply's default links duplicate its parent answer: %#v", edge)
		}
	}
	if edges := readGraph(t, handler, url.Values{"from": {replyRef}, "kind": {"answers"}}).Edges; len(edges) != 1 ||
		edges[0].Kind != "answers" || edges[0].Node.ID != middle.ID {
		t.Fatalf("the block reply's explicit answers = %#v, want its nested parent", edges)
	}
	requireEdit(t, "remove the nested parent's block", dispatchRequest(t, handler, http.MethodPatch,
		"/api/v1/comments/"+middle.ID, map[string]any{"body": "Alpha."}, "bob"))
	if edges := readGraph(t, handler, url.Values{"to": {parentRef}}).Edges; len(edges) != 1 ||
		edges[0].Kind != "answers" || edges[0].Node.ID != onBlock.ID {
		t.Fatalf("a reply to a removed nested block = %#v, want its whole-comment answer", edges)
	}
}

// The graph nests a reply under the comment it answers: ?to=<the middle comment>&kind=answers is
// the leaf's edge to it, and the replies_to edges the view listed before are unchanged - both
// replies still point at the thread's head and at nothing else.
func TestTheGraphListsTheCommentAReplyAnswers(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "The answers arm", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The root."}, "alice")
	middle := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the root.", "reply_to": root.ID,
	}, "bob")
	leaf := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the reply.", "reply_to": middle.ID,
	}, "alice")
	rootRef := "dispatch://" + issue.Key + "/comment/" + root.ID
	middleRef := "dispatch://" + issue.Key + "/comment/" + middle.ID

	edges := readGraph(t, handler, url.Values{"to": {middleRef}, "kind": {"answers"}}).Edges
	if len(edges) != 1 || edges[0].Node.ID != leaf.ID || edges[0].Kind != "answers" {
		t.Fatalf("?to=<the middle comment>&kind=answers lists %#v, want the leaf's answers edge", edges)
	}
	if edges[0].Direction != "in" {
		t.Fatalf("the answers edge's direction = %q, want in", edges[0].Direction)
	}
	if onRoot := readGraph(t, handler, url.Values{"to": {rootRef}, "kind": {"answers"}}).Edges; len(onRoot) != 1 ||
		onRoot[0].Node.ID != middle.ID {
		t.Fatalf("?to=<the root>&kind=answers lists %#v, want the middle comment alone", onRoot)
	}
	if out := readGraph(t, handler, url.Values{"from": {middleRef}, "kind": {"answers"}}).Edges; len(out) != 1 ||
		out[0].Node.ID != root.ID || out[0].Direction != "out" {
		t.Fatalf("?from=<the middle comment>&kind=answers lists %#v, want its edge to the root", out)
	}

	// The replies_to arm is what every earlier reader sees, and it is untouched: both replies
	// point at the thread's head, and the middle comment carries no inbound replies_to edge.
	repliesToRoot := readGraph(t, handler, url.Values{"to": {rootRef}, "kind": {"replies_to"}}).Edges
	if len(repliesToRoot) != 2 {
		t.Fatalf("?to=<the root>&kind=replies_to lists %#v, want both replies", repliesToRoot)
	}
	for _, edge := range repliesToRoot {
		if edge.Node.ID != middle.ID && edge.Node.ID != leaf.ID {
			t.Fatalf("?to=<the root>&kind=replies_to lists %s, want the two replies", edge.Node.ID)
		}
	}
	if onMiddle := readGraph(t, handler, url.Values{"to": {middleRef}, "kind": {"replies_to"}}).Edges; len(onMiddle) != 0 {
		t.Fatalf("?to=<the middle comment>&kind=replies_to lists %#v, want none: the climb flattens the thread", onMiddle)
	}
}

// The thread reads are unchanged by the new column. Every one of them selects on reply_to, which
// keeps exactly the value it held before, so a three-deep thread reads flat: the root's thread is
// both replies, each with reply_to the root, and a reply read by id carries the root as its
// reply_to and its own thread beside it.
func TestTheThreadReadsAreUnchangedByTheAnswersColumn(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Thread reads", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The root."}, "alice")
	middle := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the root.", "reply_to": root.ID,
	}, "bob")
	leaf := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On the reply.", "reply_to": middle.ID,
	}, "alice")

	thread := readCommentThread(t, handler, root.ID)
	if thread.Comment.ID != root.ID || thread.Comment.ReplyTo != nil {
		t.Fatalf("GET the root reads %s with reply_to %v, want %s with none", thread.Comment.ID, thread.Comment.ReplyTo, root.ID)
	}
	if len(thread.Replies) != 2 || thread.Replies[0].ID != middle.ID || thread.Replies[1].ID != leaf.ID {
		t.Fatalf("the root's thread = %#v, want the middle comment then the leaf, both flat under it", thread.Replies)
	}
	for _, reply := range thread.Replies {
		requireReplyTo(t, "a reply in the root's thread", reply.ReplyTo, root.ID)
	}

	// A reply read by id reads the thread it joined, as it did before: its own row, the head in
	// reply_to, and the thread's replies beside it.
	nested := readCommentThread(t, handler, leaf.ID)
	requireReplyTo(t, "GET the leaf", nested.Comment.ReplyTo, root.ID)
	if len(nested.Replies) != 0 {
		t.Fatalf("the leaf's replies = %#v, want none", nested.Replies)
	}

	// The issue's comment list is the same flat thread.
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the issue's comments: status=%d body=%s", listed.Code, listed.Body.String())
	}
	comments := decodeBody[[]model.Comment](t, listed)
	if len(comments) != 3 {
		t.Fatalf("the issue's comments = %#v, want the three", comments)
	}
	for _, comment := range comments {
		switch comment.ID {
		case root.ID:
			if comment.ReplyTo != nil {
				t.Fatalf("the root's reply_to = %s, want null", *comment.ReplyTo)
			}
		case middle.ID, leaf.ID:
			requireReplyTo(t, "a reply in the issue's comment list", comment.ReplyTo, root.ID)
		default:
			t.Fatalf("the issue's comments hold an unexpected comment %s", comment.ID)
		}
	}
}

// A delivery callback's reply answers the comment it was handed, not the head of that comment's
// thread: its reply_to climbs to the head exactly as before, and answers is comment {id}.
func TestADeliveryCallbackReplyAnswersTheCommentItWasHanded(t *testing.T) {
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"mention-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "A callback's reply", "before")

	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "The root."}, "alice")
	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship, @s1?", "reply_to": root.ID,
		"mentions": []map[string]any{{"target": "session:s1"}},
	}))
	requireReplyTo(t, "the mentioned reply", mentioned.ReplyTo, root.ID)

	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+mentioned.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "Yes, it can ship.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("answer the mention: status=%d body=%s", answered.Code, answered.Body.String())
	}
	reply := decodeBody[mentionedCommentRead](t, answered)
	requireReplyTo(t, "the callback's reply", reply.ReplyTo, root.ID)
	read := readCommentThread(t, handler, reply.ID).Comment
	requireReplyTo(t, "GET the callback's reply", read.ReplyTo, root.ID)
	requireAnswers(t, "GET the callback's reply", read.Answers, &mentioned.ID)
}

func TestARootsDirectReplyIsReferencedOnce(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "One direct reply", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Root."}, "alice")
	direct := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Direct.", "reply_to": root.ID}, "bob")
	rootRef := "dispatch://" + issue.Key + "/comment/" + root.ID
	directRef := "dispatch://" + issue.Key + "/comment/" + direct.ID
	for _, read := range []struct {
		query url.Values
		want  string
	}{
		{url.Values{"to": {rootRef}}, direct.ID},
		{url.Values{"from": {directRef}}, root.ID},
	} {
		edges := readGraph(t, handler, read.query).Edges
		if len(edges) != 1 || edges[0].Node.ID != read.want || edges[0].Kind != "replies_to" {
			t.Fatalf("%s lists %#v, want one replies_to edge to %s", read.query.Encode(), edges, read.want)
		}
	}
}

func TestANestedReplyIsReferencedByTheCommentItAnswers(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "The direct parent", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Root."}, "alice")
	middle := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Middle.", "reply_to": root.ID}, "bob")
	leaf := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Leaf.", "reply_to": middle.ID}, "alice")
	middleRef := "dispatch://" + issue.Key + "/comment/" + middle.ID
	leafRef := "dispatch://" + issue.Key + "/comment/" + leaf.ID
	edges := readGraph(t, handler, url.Values{"to": {middleRef}}).Edges
	if len(edges) != 1 || edges[0].Kind != "answers" || edges[0].Node.ID != leaf.ID {
		t.Fatalf("middle's references = %#v, want leaf's answers edge", edges)
	}
	out := readGraph(t, handler, url.Values{"from": {leafRef}}).Edges
	if len(out) != 2 {
		t.Fatalf("leaf's links = %#v, want answers and replies_to", out)
	}
	want := map[string]string{"answers": middle.ID, "replies_to": root.ID}
	for _, edge := range out {
		if target, ok := want[edge.Kind]; !ok || edge.Node.ID != target {
			t.Fatalf("leaf's links = %#v, want answers to middle and replies_to to root", out)
		}
		delete(want, edge.Kind)
	}
}

func TestAskingForAnswersListsEveryAnswer(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Explicit answers", "A spec")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Root."}, "alice")
	direct := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Direct.", "reply_to": root.ID}, "bob")
	rootRef := "dispatch://" + issue.Key + "/comment/" + root.ID
	directRef := "dispatch://" + issue.Key + "/comment/" + direct.ID
	for _, read := range []struct {
		query url.Values
		want  string
	}{
		{url.Values{"to": {rootRef}, "kind": {"answers"}}, direct.ID},
		{url.Values{"from": {directRef}, "kind": {"answers"}}, root.ID},
	} {
		edges := readGraph(t, handler, read.query).Edges
		if len(edges) != 1 || edges[0].Kind != "answers" || edges[0].Node.ID != read.want {
			t.Fatalf("%s lists %#v, want one answers edge to %s", read.query.Encode(), edges, read.want)
		}
	}
}
