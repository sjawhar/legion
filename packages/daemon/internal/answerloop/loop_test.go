package answerloop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/answer"
	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/conversation/omp"
	"github.com/sjawhar/legion/daemon/internal/ghtool"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The same question handed off twice, and again after a restart, is one question, answered once,
// and the second and third hand-offs are counted on it.
func TestAQuestionHandedOffTwiceAndAcrossARestartIsAnsweredOnce(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	r.receive(messageHandoff("ses_tester", "m-1", 2, "Why plum?"))
	r.drain()
	r.restart()
	r.receive(messageHandoff("ses_tester", "m-1", 3, "Why plum?"))
	r.drain()
	q := r.only()
	if q.State != store.QuestionPosted || q.Deliveries != 3 || r.answerCount() != 1 {
		t.Fatalf("question = %s with %d deliveries after %d answers; want posted once from three hand-offs", q.State, q.Deliveries, r.answerCount())
	}
	if posts := r.dispatch.posts(); len(posts) != 1 {
		t.Fatalf("Dispatch posts = %s; want one", describe(posts))
	}
}

// A hand-off for a session the daemon's history does not hold is dropped, nothing recorded, and
// acknowledged.
func TestAHandoffForAnUnknownSessionIsDropped(t *testing.T) {
	r := newRig(t)
	r.receive(messageHandoff("ses_stranger", "m-1", 1, "Who are you?"))
	if all := r.questions(); len(all) != 0 {
		t.Fatalf("questions = %+v; want none", all)
	}
}

// A message to a session answers from that session's conversation, an older one a newer session
// of the role replaced included; a message to the role answers from the role's newest.
func TestASessionAnswersFromItsOwnConversationAndARoleFromItsNewest(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_old", testerToken, 0)
	r.conversation("ses_new", testerToken, 10)
	r.receive(envoyHandoff("notifications.agent.ses_old", "ses_old", "evt-1", "agent.ses_old.k1", "What did you test first?"))
	r.receive(envoyHandoff("notifications.role."+string(testerToken), "ses_new", "evt-2", "publish.k2", "What did you test last?"))
	bySession := map[string]string{}
	for _, q := range r.questions() {
		bySession[q.Identity] = q.Session
	}
	if bySession["agent.ses_old.k1"] != "ses_old" || bySession["publish.k2"] != "ses_new" {
		t.Fatalf("conversations chosen = %v; want ses_old for its own session, ses_new for the role", bySession)
	}
}

// Two questions to one conversation are answered one after the other: the second is generated from
// the conversation holding the first exchange, and the session ends with both, in order, once each
// (A9).
func TestTwoQuestionsToOneConversationAreAnsweredInOrder(t *testing.T) {
	r := newRig(t)
	path := r.conversation("ses_tester", testerToken, 0)
	r.receive(messageHandoff("ses_tester", "m-1", 1, "First?"))
	r.receive(messageHandoff("ses_tester", "m-2", 1, "Second?"))
	r.drain()
	if r.answerCount() != 2 {
		t.Fatalf("answers = %d; want 2", r.answerCount())
	}
	second := r.requests[1].Conversation.Messages
	if !strings.Contains(second[len(second)-1].Content[0].Text, "Plum, from First?") {
		t.Fatalf("the second answer was generated from %+v; want the first exchange in it", second[len(second)-1])
	}
	s, err := omp.ReadFile(path, omp.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range s.Messages {
		texts = append(texts, m.Content[0].Text)
	}
	want := []string{"Test the fruit.", "Tested: plum.", "First?", "Plum, from First?", "Second?", "Plum, from Second?"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Fatalf("conversation = %q; want %q", texts, want)
	}
}

// The questions to one conversation never overlap, and no more conversations are answered at once
// than bounds.concurrent.
func TestNoMoreConversationsAreAnsweredAtOnceThanTheBound(t *testing.T) {
	for _, bound := range []int{1, 2} {
		r := newRig(t)
		for _, session := range []string{"ses_a", "ses_b", "ses_c"} {
			r.conversation(session, testerToken, 0)
			r.receive(messageHandoff(session, "m-"+session, 1, "Q?"))
		}
		r.receive(messageHandoff("ses_a", "m-a2", 1, "Q2?"))
		r.cfg.Answers.Bounds.Concurrent = bound
		var inFlight, most atomic.Int32
		var mu sync.Mutex
		bySession := map[string]bool{}
		r.answer = func(req answer.Request) (answer.Reply, error) {
			mu.Lock()
			if bySession[req.Agent.Session] {
				t.Errorf("two answers of %s at once", req.Agent.Session)
			}
			bySession[req.Agent.Session] = true
			mu.Unlock()
			now := inFlight.Add(1)
			for {
				seen := most.Load()
				if now <= seen || most.CompareAndSwap(seen, now) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			inFlight.Add(-1)
			mu.Lock()
			bySession[req.Agent.Session] = false
			mu.Unlock()
			return answered(req, "A."), nil
		}
		r.restart()
		ctx, cancel := context.WithCancel(r.ctx)
		done := make(chan struct{})
		go func() { r.loop.Run(ctx); close(done) }()
		testwait.Eventually(t, "every question answered", func() bool { return r.answerCount() == 4 })
		cancel()
		<-done
		if got := most.Load(); got != int32(bound) {
			t.Fatalf("bound %d: at most %d answers ran at once; want %d", bound, got, bound)
		}
	}
}

// Each kind of question is answered where it was asked, in the agent's name: a message and a
// conversation-page message through their delivery attempt, a mention through its own, an ask
// reply on the ask's thread with the question's marker, an Envoy message back to its sender in
// reply to its envelope.
func TestEachAnswerIsPostedWhereItWasAskedAsTheAgent(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	pageMessage := dispatchHandoff("ses_tester", frame("message.created", person, nil,
		map[string]any{"id": "m-page", "issue_key": nil, "body": "Hi?", "target": "session:ses_tester"}, map[string]any{"attempt": 3, "mode": "btw"}))
	mention := dispatchHandoff("ses_tester", frame("comment.created", person, new("LEGION-9"),
		map[string]any{"id": "c-1", "issue_key": "LEGION-9", "body": "@tester why?"},
		map[string]any{"attempt": 2, "mode": "btw", "comment_id": "c-1", "target": "role:" + string(testerToken)}))
	askReply := dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
		map[string]any{"id": "c-2", "issue_key": "LEGION-9", "body": "Why A?", "ask_id": askID}))
	for _, h := range []intake.Handoff{messageHandoff("ses_tester", "m-1", 1, "Why?"), pageMessage, mention, askReply,
		envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-9", "agent.ses_tester.k9", "Which?")} {
		r.receive(h)
	}
	r.drain()
	ids := map[string]string{}
	for _, q := range r.questions() {
		if q.State != store.QuestionPosted {
			t.Errorf("question %s is %s; want posted", q.Identity, q.State)
		}
		ids[q.Identity] = q.ID
	}
	marker := func(identity string) string { return "\n\n" + AnswerMarker + ids[identity] + " -->" }
	agent := map[string]any{"kind": "session", "id": "ses_tester"}
	want := []request{
		{"POST", "/api/v1/messages/m-1/reply", map[string]any{"actor": agent, "attempt": float64(1), "body": "Plum, from Why?" + marker("dispatch:m-1")}},
		{"POST", "/api/v1/messages/m-page/reply", map[string]any{"actor": agent, "attempt": float64(3), "body": "Plum, from Hi?" + marker("dispatch:m-page")}},
		{"POST", "/api/v1/comments/c-1/reply", map[string]any{"actor": agent, "attempt": float64(2), "target": "role:" + string(testerToken),
			"body": "Plum, from @tester why?" + marker("dispatch:c-1")}},
		{"POST", "/api/v1/issues/LEGION-9/comments", map[string]any{"actor": agent, "ask_id": askID, "body": "Plum, from Why A?" + marker("dispatch:c-2")}},
	}
	if got := r.dispatch.posts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Dispatch posts:\n%s\nwant:\n%s", describe(got), describe(want))
	}
	sends := r.listener.posts()
	wantSend := request{"POST", "/v1/messages/send", map[string]any{"target_session": "ses_asker", "source": "agent", "source_session": "ses_tester",
		"message": "Plum, from Which?" + marker("agent.ses_tester.k9"), "in_reply_to": "evt-9", "idempotency_key": ids["agent.ses_tester.k9"]}}
	if len(sends) != 1 || !reflect.DeepEqual(sends[0], wantSend) {
		t.Fatalf("listener posts = %s; want %s", describe(sends), describe([]request{wantSend}))
	}
	if len(r.rec.decisions) != 1 || r.rec.decisions[0].ID != "dispatch:c-2" {
		t.Fatalf("decisions = %+v; want the person's reply on the agent's ask", r.rec.decisions)
	}
	for _, req := range r.requests {
		if req.MayAskNothing != (req.Question.Text == "Why A?" || req.Question.Text == "Which?") {
			t.Errorf("question %q may ask nothing = %t", req.Question.Text, req.MayAskNothing)
		}
		if !strings.Contains(req.ChangeNote, "pull request #7 is merged") {
			t.Errorf("change note = %q; want the pull request's state", req.ChangeNote)
		}
	}
}

// An ask's thread is read before the loop posts there: a reply of the agent's carrying the
// question's marker, one a post the daemon stopped after left, is not posted again.
func TestAnAskThreadHoldingTheAnswerIsNotPostedAgain(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.receive(dispatchHandoff("ses_tester", bare("comment.created", person, new("LEGION-9"),
		map[string]any{"id": "c-2", "issue_key": "LEGION-9", "body": "Why A?", "ask_id": askID})))
	q := r.only()
	q.State, q.ReplyText, q.UpdatedAt = store.QuestionAppended, "Because B.", testNow
	if err := r.st.UpdateQuestion(r.ctx, q, store.QuestionReceived); err != nil {
		t.Fatal(err)
	}
	r.ask["replies"] = []any{map[string]any{"id": "c-3", "author": map[string]any{"kind": "session", "id": "ses_tester"},
		"body": "Because B.\n\n" + AnswerMarker + q.ID + " -->"}}
	r.restart()
	r.drain()
	if posts := r.dispatch.posts(); len(posts) != 0 {
		t.Fatalf("Dispatch posts = %s; want none, the thread holds the answer", describe(posts))
	}
	if got := r.only(); got.State != store.QuestionPosted || r.answerCount() != 0 {
		t.Fatalf("question = %s after %d answers; want posted, never answered again", got.State, r.answerCount())
	}
}

// A person's answer to the agent's ask that also asks a question is passed on as a decision with
// the same id at every hand-off (Decisions passes it once by that id), and its question is
// answered once, the model told it may reply the message asks nothing, on the ask's thread in the
// agent's name. An agent's reply on the ask, and the loop's own answer there, are no decision.
func TestAnAnswerThatAlsoAsksIsADecisionAndItsQuestionIsAnswered(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	answer := dispatchHandoff("ses_tester", answeredEvent(34, person, askID, "question", []string{"B"}, new("Use B. Why did you test A?")))
	r.receive(answer)
	r.receive(answer)
	r.drain()
	r.restart()
	r.receive(answer)
	r.drain()
	q := r.only()
	if q.State != store.QuestionPosted || q.Identity != "dispatch:34" || r.answerCount() != 1 {
		t.Fatalf("question %s is %s after %d answers; want dispatch:34 posted after one", q.Identity, q.State, r.answerCount())
	}
	if req := r.requests[0]; !req.MayAskNothing || req.Question.Text != "Selected: B\n\nUse B. Why did you test A?" {
		t.Fatalf("model asked %q, may ask nothing %t; want the answer, allowed to ask nothing", req.Question.Text, req.MayAskNothing)
	}
	agent := map[string]any{"kind": "session", "id": "ses_tester"}
	want := []request{{"POST", "/api/v1/issues/LEGION-9/comments", map[string]any{"actor": agent, "ask_id": askID,
		"body": "Plum, from Selected: B\n\nUse B. Why did you test A?\n\n" + AnswerMarker + q.ID + " -->"}}}
	if got := r.dispatch.posts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Dispatch posts:\n%s\nwant:\n%s", describe(got), describe(want))
	}
	if len(r.rec.decisions) != 3 {
		t.Fatalf("decisions = %+v; want the answer at each of its three hand-offs", r.rec.decisions)
	}
	for _, d := range r.rec.decisions {
		if d.ID != "dispatch:34" || d.Asker != "user:person@example.com" || d.Ref != "dispatch://LEGION-9/ask/"+askID || d.Conversation.Session != "ses_tester" {
			t.Fatalf("decision = %+v; want dispatch:34 by the person on the agent's ask", d)
		}
	}
	r.receive(dispatchHandoff("ses_tester", bare("comment.created", intake.DispatchActor{Kind: "session", ID: "ses_tester"}, new("LEGION-9"),
		map[string]any{"id": "c-own", "issue_key": "LEGION-9", "body": "Plum.\n\n" + AnswerMarker + q.ID + " -->", "ask_id": askID, "ask_waiting_on": "human"})))
	r.receive(dispatchHandoff("ses_tester", bare("comment.created", intake.DispatchActor{Kind: "session", ID: "ses_asker"}, new("LEGION-9"),
		map[string]any{"id": "c-agent", "issue_key": "LEGION-9", "body": "B is green.", "ask_id": askID, "ask_waiting_on": "agent"})))
	if len(r.rec.decisions) != 3 {
		t.Fatalf("decisions = %+v; want no more for the loop's own answer and another agent's reply", r.rec.decisions)
	}
}

// A claim started while the answer is generated takes the conversation: nothing is appended, nothing
// posted, and the question goes to the running agent on its role topic, with the gh calls the loop
// already made and their results (A7, A18).
func TestAStartDuringTheAnswerForwardsTheQuestionAndAppendsNothing(t *testing.T) {
	r := newRig(t)
	path := r.conversation("ses_tester", testerToken, 0)
	before := fileContent(t, path)
	r.answer = func(req answer.Request) (answer.Reply, error) {
		exit := 0
		if err := r.st.StartAnswerToolCall(r.ctx, store.AnswerToolCall{Question: req.Question.ID, Seq: 1, Args: []string{"pr", "comment", "7", "--body", "Done."},
			App: "review", Owner: "o", StartedAt: testNow}); err != nil {
			t.Error(err)
		}
		if err := r.st.FinishAnswerToolCall(r.ctx, req.Question.ID, 1, "https://github.com/o/r/pull/7#issuecomment-1", &exit, testNow); err != nil {
			t.Error(err)
		}
		r.gate.set(r.gate.started, "ses_tester", true)
		return answered(req, "Commented."), nil
	}
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Please comment."))
	r.drain()
	if after := fileContent(t, path); after != before {
		t.Fatalf("the conversation changed:\n%s", after[len(before):])
	}
	if q := r.only(); q.State != store.QuestionForwarded || !q.ForwardDelivered {
		t.Fatalf("question = %s, delivered %t; want forwarded and delivered", q.State, q.ForwardDelivered)
	}
	if posts := r.dispatch.posts(); len(posts) != 0 {
		t.Fatalf("Dispatch posts = %s; want none", describe(posts))
	}
	publishes := r.listener.posts()
	if len(publishes) != 1 || publishes[0].Path != "/v1/messages/publish" || publishes[0].Body["topic"] != "notifications.role."+string(testerToken) ||
		publishes[0].Body["dedupe_key"] != "legion-answer:"+r.only().ID || publishes[0].Body["handoff"] != false {
		t.Fatalf("listener posts = %s; want one publish to the tester's role topic", describe(publishes))
	}
	var payload forwardPayload
	if err := json.Unmarshal([]byte(publishes[0].Body["payload"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Question != "Please comment." || len(payload.Calls) != 1 || payload.Calls[0].Result == nil ||
		!strings.Contains(*payload.Calls[0].Result, "issuecomment-1") || !strings.Contains(payload.Text, "do not repeat a write") {
		t.Fatalf("forward payload = %+v; want the question and its gh call with its result", payload)
	}
}

// A conversation that moved while the answer was generated (its agent wrote to it) is not appended
// to: the answer is dropped and the question forwarded to the agent now running it.
func TestAMovedConversationForwardsTheQuestion(t *testing.T) {
	r := newRig(t)
	path := r.conversation("ses_tester", testerToken, 0)
	r.answer = func(req answer.Request) (answer.Reply, error) {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.WriteString(`{"id":"e0000004","parentId":"e0000003","timestamp":"2026-10-26T00:00:00.000Z","type":"message","message":{"role":"user","content":[{"type":"text","text":"resumed"}],"attribution":"user","timestamp":3}}` + "\n")
		_ = file.Close()
		return answered(req, "Plum."), nil
	}
	r.gate.onAppend = func(err error) {
		if errors.Is(err, conversation.ErrMoved) {
			r.gate.set(r.gate.running, "ses_tester", true)
		}
	}
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	r.drain()
	if strings.Contains(fileContent(t, path), "Why plum?") {
		t.Fatal("the answer was appended to a conversation that moved")
	}
	if q := r.only(); q.State != store.QuestionForwarded || !q.ForwardDelivered {
		t.Fatalf("question = %s, delivered %t; want forwarded", q.State, q.ForwardDelivered)
	}
}

// A question the daemon stopped while generating is answered again on restart, the model handed the
// gh calls the first run made: a finished one with its result, a comment call of unknown outcome
// with what GitHub shows of it (recorded as its result), and a read of unknown outcome as unknown.
func TestARestartAnswersAGeneratingQuestionAgainWithItsRecordedCalls(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Comment on the PR."))
	q := r.only()
	q.State, q.UpdatedAt = store.QuestionGenerating, testNow
	if err := r.st.UpdateQuestion(r.ctx, q, store.QuestionReceived); err != nil {
		t.Fatal(err)
	}
	exit := 0
	for seq, args := range map[int][]string{1: {"pr", "view", "7"}, 2: {"pr", "comment", "7", "--body", "Done."}, 3: {"api", "repos/o/r"}} {
		if err := r.st.StartAnswerToolCall(r.ctx, store.AnswerToolCall{Question: q.ID, Seq: seq, Args: args, App: "review", Owner: "o", StartedAt: testNow}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.st.FinishAnswerToolCall(r.ctx, q.ID, 1, "open", &exit, testNow); err != nil {
		t.Fatal(err)
	}
	r.tool.found[2] = ghtool.Found{Exists: true, URL: "https://github.com/o/r/pull/7#issuecomment-5", ID: 5}
	r.tool.err[3] = ghtool.ErrNotAWrite
	r.restart()
	r.drain()
	if r.answerCount() != 1 {
		t.Fatalf("answers = %d; want the question answered again once", r.answerCount())
	}
	earlier := r.requests[0].Earlier
	if len(earlier) != 3 || *earlier[0].Result != "open" || earlier[1].Result == nil || !strings.Contains(*earlier[1].Result, "issuecomment-5") || earlier[2].Result != nil {
		t.Fatalf("earlier calls = %+v; want the finished read, the comment found on GitHub, the read of unknown outcome", earlier)
	}
	if call, _, _ := r.st.AnswerToolCall(r.ctx, q.ID, 2); call.Result == nil || !strings.Contains(*call.Result, "issuecomment-5") {
		t.Fatalf("call 2 = %+v; want what the lookup found recorded as its result", call)
	}
	if got := r.only(); got.State != store.QuestionPosted {
		t.Fatalf("question = %s; want posted", got.State)
	}
}

// A question whose answer was appended before the daemon stopped is posted on restart, as decided,
// and never answered again (spec departure 3).
func TestARestartPostsAnAppendedAnswerWithoutAnsweringAgain(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	q := r.only()
	q.State, q.ReplyText, q.ReplyKind, q.UpdatedAt = store.QuestionAppended, "Because it ripened.", "answered", testNow
	if err := r.st.UpdateQuestion(r.ctx, q, store.QuestionReceived); err != nil {
		t.Fatal(err)
	}
	r.restart()
	r.drain()
	posts := r.dispatch.posts()
	if r.answerCount() != 0 || len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string), "Because it ripened.") {
		t.Fatalf("after %d answers, posts = %s; want the appended answer posted, nothing answered", r.answerCount(), describe(posts))
	}
	if got := r.only(); got.State != store.QuestionPosted {
		t.Fatalf("question = %s; want posted", got.State)
	}
}

// A question to a running agent goes to it alone. While no session holds its role the forward is
// held and offered again; once the role is held it is delivered. A forward whose claim stops first
// goes back to the loop, which answers it.
func TestAForwardIsHeldUntilTheRoleIsHeldAndReturnsWhenTheClaimStops(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.gate.set(r.gate.running, "ses_tester", true)
	r.held = true
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	r.drain()
	if q := r.only(); q.State != store.QuestionForwarded || q.ForwardDelivered || r.answerCount() != 0 {
		t.Fatalf("question = %s, delivered %t; want a held forward, nothing answered", q.State, q.ForwardDelivered)
	}
	r.held = false
	r.drain()
	if q := r.only(); !q.ForwardDelivered || len(r.dispatch.posts()) != 0 {
		t.Fatalf("question = %s, delivered %t; want delivered to the agent alone", q.State, q.ForwardDelivered)
	}

	r.held = true
	r.receive(messageHandoff("ses_tester", "m-2", 1, "And pear?"))
	r.drain()
	r.gate.set(r.gate.running, "ses_tester", false)
	r.drain()
	for _, q := range r.questions() {
		if q.Identity == "dispatch:m-2" && q.State != store.QuestionPosted {
			t.Fatalf("the held forward of a claim that stopped is %s; want answered by the loop and posted", q.State)
		}
	}
}

// Over the day's limit no answer is generated: the daemon replies in the agent's name that it is
// over its limit, and the question is passed on.
func TestOverTheDailyLimitTheDaemonRepliesAndPassesTheQuestionOn(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	if err := r.st.AddAnswerSpend(r.ctx, project, testNow, 50); err != nil {
		t.Fatal(err)
	}
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	r.drain()
	posts := r.dispatch.posts()
	if r.answerCount() != 0 || len(posts) != 1 || !strings.HasPrefix(posts[0].Body["body"].(string),
		"I am over the project's daily limit of $50 for answers. I passed your question to the Legion controller.") {
		t.Fatalf("after %d answers, posts = %s; want the daemon's over-limit reply", r.answerCount(), describe(posts))
	}
	q := r.only()
	if q.State != store.QuestionPassedOn || len(r.rec.passed) != 1 || r.rec.passed[0] != (passed{q.ID, "over_limit", "user:person@example.com", "dispatch://LEGION-9/message/m-1"}) {
		t.Fatalf("question = %s, passed on %+v; want passed on over_limit with the asker and where", q.State, r.rec.passed)
	}
}

// The answer's cost is added to the day's spend, which the next answer is checked against.
func TestAnAnswersCostIsAddedToTheDaysSpend(t *testing.T) {
	r := newRig(t)
	r.conversation("ses_tester", testerToken, 0)
	r.receive(messageHandoff("ses_tester", "m-1", 1, "Why plum?"))
	r.drain()
	if spent, err := r.st.AnswerSpend(r.ctx, project, testNow); err != nil || spent != 0.25 {
		t.Fatalf("spend = %v, %v; want the answer's 0.25", spent, err)
	}
}

// What the model gives decides what is posted and whether the question is passed on: a fault is the
// daemon's reply naming it, nothing appended; an answer the agent could not give, or a request for
// work, is its own text, appended, and passed on; a message that asks nothing is appended and gets
// no reply.
func TestTheReplyKindDecidesWhatIsPostedAndPassedOn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reply    func(req answer.Request) answer.Reply
		post     string
		reason   string
		state    store.QuestionState
		appended bool
	}{
		{name: "past the time bound", reply: func(answer.Request) answer.Reply {
			return answer.Reply{Kind: answer.NotAnswered, Fault: answer.AnswerSeconds, CostUSD: 0.1}
		}, post: "I could not answer within the 180-second bound. I passed your question to the Legion controller.", reason: "answer_seconds", state: store.QuestionPassedOn},
		{name: "not in the form", reply: func(answer.Request) answer.Reply {
			return answer.Reply{Kind: answer.NotAnswered, Fault: answer.Malformed}
		}, post: "My reply was not in the form the daemon checks, so it is not posted. I passed your question to the Legion controller.", reason: "malformed", state: store.QuestionPassedOn},
		{name: "not answered", reply: func(req answer.Request) answer.Reply {
			reply := answered(req, "I would need my working copy.")
			reply.Kind = answer.NotAnswered
			return reply
		}, post: "I would need my working copy.", reason: "not_answered", state: store.QuestionPassedOn, appended: true},
		{name: "needs work", reply: func(req answer.Request) answer.Reply {
			reply := answered(req, "That needs a run of my phase; nothing starts.")
			reply.Kind = answer.NeedsWork
			return reply
		}, post: "That needs a run of my phase; nothing starts.", reason: "needs_work", state: store.QuestionPassedOn, appended: true},
		{name: "asks nothing", reply: func(req answer.Request) answer.Reply {
			reply := answered(req, "Noted.")
			reply.Kind = answer.None
			return reply
		}, state: store.QuestionDropped, appended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			path := r.conversation("ses_tester", testerToken, 0)
			r.answer = func(req answer.Request) (answer.Reply, error) { return tc.reply(req), nil }
			r.receive(envoyHandoff("notifications.agent.ses_tester", "ses_tester", "evt-1", "agent.ses_tester.k1", "Run the suite?"))
			r.drain()
			q := r.only()
			sends := r.listener.posts()
			switch {
			case q.State != tc.state:
				t.Fatalf("question = %s; want %s", q.State, tc.state)
			case tc.post == "" && len(sends) != 0:
				t.Fatalf("sends = %s; want none", describe(sends))
			case tc.post != "" && (len(sends) != 1 || !strings.HasPrefix(sends[0].Body["message"].(string), tc.post+"\n\n"+AnswerMarker)):
				t.Fatalf("sends = %s; want %q", describe(sends), tc.post)
			case strings.Contains(fileContent(t, path), "Run the suite?") != tc.appended:
				t.Fatalf("appended = %t; want %t", !tc.appended, tc.appended)
			}
			if tc.reason == "" && len(r.rec.passed) != 0 || tc.reason != "" && (len(r.rec.passed) != 1 || r.rec.passed[0].Reason != tc.reason) {
				t.Fatalf("passed on = %+v; want %q", r.rec.passed, tc.reason)
			}
		})
	}
}
