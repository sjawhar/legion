// acting_agent_test.go proves what Dispatch records when an agent acts on a person's own routes
// with that person's credential: the act is the person's, and the agent that made it is named
// beside them in the actor's act. The routes a session has always called are unchanged - a
// comment is still the session's own write - and the two places an act cannot simply ride along
// on a stored actor have their own records: a credential decision's access-log line, and the
// accept, which refuses a message an agent wrote for its person rather than making it the words
// the person typed.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// actingSession is the session an agent names when it acts for its person, and actingTokenName
// the name of the personal token it presents.
const (
	actingSession   = "session-acting-for-alice"
	actingTokenName = "architect"
)

// mintPersonalToken mints one of login's own personal agent tokens through the route a person
// mints one on, named name and holding scopes (nil for the set the minting person holds, which
// is both). The answer carries the plaintext once, which is what the agent then presents.
func mintPersonalToken(t *testing.T, handler http.Handler, login, name string, scopes []string) agentTokenResponse {
	t.Helper()
	body := map[string]any{"name": name}
	if scopes != nil {
		body["scopes"] = scopes
	}
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", body, login)
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint %s's %q token: status=%d body=%s", login, name, minted.Code, minted.Body.String())
	}
	return decodeBody[agentTokenResponse](t, minted)
}

// answerAsToken answers askID under token, naming session in ?session= when it is given, with the
// revision a question nobody reworded carries.
func answerAsToken(t *testing.T, handler http.Handler, askID, token, session string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/asks/" + askID + "/answer"
	if session != "" {
		target += "?session=" + session
	}
	if _, named := body["expected_edited_at"]; !named {
		body["expected_edited_at"] = nil
	}
	return agentRequest(t, handler, http.MethodPost, target, body, token)
}

// storedAskAnswer is the answer the ask's row holds, as every reader of it decodes it.
func storedAskAnswer(t *testing.T, database *store.Store, askID string) model.AskAnswer {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `select answer from asks where id = $1`, askID).Scan(&raw); err != nil {
		t.Fatalf("read ask %s's answer: %v", askID, err)
	}
	var answer model.AskAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("decode ask %s's answer %s: %v", askID, raw, err)
	}
	return answer
}

// requireActingToken fails t unless act names session (empty for an act that names none), the
// token row given and exactly scopes.
func requireActingToken(t *testing.T, what string, act *model.ActorAct, session string, token agentTokenResponse, scopes ...string) {
	t.Helper()
	if act == nil {
		t.Fatalf("%s carries no act, want one naming the token %q", what, token.Name)
	}
	if act.Session != session || act.TokenID != token.ID || act.TokenName != token.Name || !slices.Equal(act.Scopes, scopes) {
		t.Fatalf("%s act = %+v, want session %q, token %s %q and scopes %v", what, *act, session, token.ID, token.Name, scopes)
	}
}

// requirePersonActor fails t unless actor is the person themselves, whoever acted for them.
func requirePersonActor(t *testing.T, what string, actor model.Actor, login string) {
	t.Helper()
	if actor.Kind != "user" || actor.ID != login {
		t.Fatalf("%s actor = %s:%s, want the person %s", what, actor.Kind, actor.ID, login)
	}
}

// An answer, a review and a snooze an agent makes with its person's own token are each recorded
// as the person - the actor every reader of them matches on - with an act naming the agent: the
// session the caller named through ?session=, the token's id and name, and the scopes that token
// held at the act. A caller that names no session leaves the act naming the token alone, and a
// broker-issued token's act names the session the broker itself proved, which no caller chooses.
func TestAnAgentsAnswerReviewAndSnoozeAreRecordedAsThePersonWithItsActBesideThem(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "ACT", "Recording who acted", "A spec")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)

	named := openAskAs(t, handler, issue.Key, "session-asking", "Which option, with a session named?")
	if answered := answerAsToken(t, handler, named, token.Token, actingSession, map[string]any{
		"selected": []string{"Yes"},
	}); answered.Code != http.StatusOK {
		t.Fatalf("answer with a personal token: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := storedAskAnswer(t, database, named)
	if answer.User != "alice" {
		t.Fatalf("the stored answer's user = %q, want the person alice", answer.User)
	}
	requireActingToken(t, "the stored answer's", answer.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)
	event := latestAskEventActor(t, database, named, "ask.answered")
	requirePersonActor(t, "the ask.answered event's", event, "alice")
	requireActingToken(t, "the ask.answered event's", event.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)

	unnamed := openAskAs(t, handler, issue.Key, "session-asking", "Which option, with no session named?")
	if answered := answerAsToken(t, handler, unnamed, token.Token, "", map[string]any{
		"selected": []string{"No"},
	}); answered.Code != http.StatusOK {
		t.Fatalf("answer naming no session: status=%d body=%s", answered.Code, answered.Body.String())
	}
	requireActingToken(t, "the answer of a caller that named no session", storedAskAnswer(t, database, unnamed).Act,
		"", token, model.ScopePerson, model.ScopeApprove)

	reviewed := agentRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews?session="+actingSession,
		map[string]any{"state": "approved"}, token.Token)
	if reviewed.Code != http.StatusCreated {
		t.Fatalf("review with a personal token: status=%d body=%s", reviewed.Code, reviewed.Body.String())
	}
	review := decodeBody[model.ArtifactReview](t, reviewed)
	requirePersonActor(t, "the review's", review.Actor, "alice")
	requireActingToken(t, "the review's", review.Actor.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)

	snoozed := agentRequest(t, handler, http.MethodPut,
		"/api/v1/me/asks/"+named+"/snooze?session="+actingSession,
		map[string]any{"snoozed_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, token.Token)
	if snoozed.Code != http.StatusOK {
		t.Fatalf("snooze with a personal token: status=%d body=%s", snoozed.Code, snoozed.Body.String())
	}
	// The snooze's event is owned by the ask's project and carries no issue, so it is read by
	// its type alone.
	snooze := scanEventActor(t, database, `select actor from events where type = 'user_state.updated' order by id desc limit 1`)
	requirePersonActor(t, "the snooze's", snooze, "alice")
	requireActingToken(t, "the snooze's", snooze.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)
}

// A broker-issued token proves its own session, so the act names that session and no token row;
// a ?session= naming another session is the disagreement actingPerson refuses.
func TestABrokerTokensAnswerActsAsTheSessionTheBrokerProved(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-dev-box-token-that-answers"
	says := liveNamedToken(caller.jkt)
	says.ProjectScope, says.Project = brokertoken.ProjectScopePerson, ""
	rig.broker.Says(token, says)

	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "BRK", "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "BRK", "title": "Answered under a broker token", "spec": "# Spec",
	})
	if issue.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", issue.Code, issue.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key
	askID := openAskAs(t, rig.handler, key, "session-asking", "Which option?")

	answered := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/asks/"+askID+"/answer", token, map[string]any{
		"selected": []string{"Yes"}, "expected_edited_at": nil,
	}))
	if answered.Code != http.StatusOK {
		t.Fatalf("answer under a broker token: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := storedAskAnswer(t, rig.database, askID)
	if answer.User != brokerTestPerson {
		t.Fatalf("the stored answer's user = %q, want the person %q", answer.User, brokerTestPerson)
	}
	if answer.Act == nil || answer.Act.Session != brokerTestSession || answer.Act.TokenID != "" {
		t.Fatalf("the stored answer's act = %+v, want the session the broker proved and no token row", answer.Act)
	}
	if !slices.Equal(answer.Act.Scopes, []string{model.ScopePerson, model.ScopeApprove}) {
		t.Fatalf("the stored answer's act scopes = %v, want both", answer.Act.Scopes)
	}
}

// The two handlers that already decode a body actor - the answer and a person's issue state -
// take the session it names for the act, where the caller's credential proved none of its own:
// a personal token names its agent nowhere else on a route whose body is read anyway. A
// credential that did prove a session decides it, and a body naming another is refused the way
// every other disagreement about the writer is.
func TestTheBodysActorNamesTheActingSessionWhereTheCredentialProvedNone(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "ACT", "The body names the acting session", "A spec")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)
	askID := openAskAs(t, handler, issue.Key, "session-asking", "Which option?")

	answered := agentRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"Yes"}, "expected_edited_at": nil,
		"actor": map[string]any{"kind": "session", "id": actingSession},
	}, token.Token)
	if answered.Code != http.StatusOK {
		t.Fatalf("answer with the session in the body: status=%d body=%s", answered.Code, answered.Body.String())
	}
	requireActingToken(t, "the answer of a caller that named its session in the body",
		storedAskAnswer(t, database, askID).Act, actingSession, token, model.ScopePerson, model.ScopeApprove)

	state := agentRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"pinned": true, "actor": map[string]any{"kind": "session", "id": actingSession},
	}, token.Token)
	if state.Code != http.StatusOK {
		t.Fatalf("pin the issue with the session in the body: status=%d body=%s", state.Code, state.Body.String())
	}
	pinned := scanEventActor(t, database, `select actor from events where type = 'user_state.updated' order by id desc limit 1`)
	requirePersonActor(t, "the pin's", pinned, "alice")
	requireActingToken(t, "the pin's", pinned.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)
}

// A broker-issued token proves the session it acts for, so a body naming another session is 403
// ACTOR_SESSION_MISMATCH, and one naming that same session is taken.
func TestABodyActorNamingAnotherSessionThanTheCredentialProvedIsRefused(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-dev-box-token-whose-body-lies"
	says := liveNamedToken(caller.jkt)
	says.ProjectScope, says.Project = brokertoken.ProjectScopePerson, ""
	rig.broker.Says(token, says)
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": brokerTestProject, "title": "A body that names another session", "spec": "# Spec",
	})
	if issue.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", issue.Code, issue.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key

	refused := rig.serve(caller.presenting(t, http.MethodPost,
		"/api/v1/asks/"+openAskAs(t, rig.handler, key, "session-asking", "Whose session?")+"/answer", token, map[string]any{
			"selected": []string{"Yes"}, "expected_edited_at": nil,
			"actor": map[string]any{"kind": "session", "id": "session-someone-else"},
		}))
	if refused.Code != http.StatusForbidden || decodeBody[refusalAnswer](t, refused).Code != "ACTOR_SESSION_MISMATCH" {
		t.Fatalf("answer naming another session: status=%d body=%s, want 403 ACTOR_SESSION_MISMATCH", refused.Code, refused.Body.String())
	}

	agreed := openAskAs(t, rig.handler, key, "session-asking", "The same session?")
	answered := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/asks/"+agreed+"/answer", token, map[string]any{
		"selected": []string{"Yes"}, "expected_edited_at": nil,
		"actor": map[string]any{"kind": "session", "id": brokerTestSession},
	}))
	if answered.Code != http.StatusOK {
		t.Fatalf("answer naming the session the broker proved: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if act := storedAskAnswer(t, rig.database, agreed).Act; act == nil || act.Session != brokerTestSession {
		t.Fatalf("the stored answer's act = %+v, want the session the broker proved", act)
	}
}

// The answer an agent made is the person's own, so the person changes it from their browser
// rather than being refused as somebody else (NOT_ANSWERER), and the three reads that match an
// answer against the actor who wrote it - the ask's answer history, the person's own answers
// list and the event the broker notifies on - read it exactly as they read an answer typed in a
// browser, the act riding along on each.
func TestThePersonChangesTheAnswerTheirAgentMadeAndEveryReaderStillSeesThem(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "ACT", "Changing an agent's answer", "A spec")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)
	askID := openAskAs(t, handler, issue.Key, "session-asking", "Which option?")

	if answered := answerAsToken(t, handler, askID, token.Token, actingSession, map[string]any{
		"selected": []string{"Yes"}, "text": "The agent's answer.",
	}); answered.Code != http.StatusOK {
		t.Fatalf("the agent answers: status=%d body=%s", answered.Code, answered.Body.String())
	}
	agentAnswer := storedAskAnswer(t, database, askID)

	changed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"No"}, "text": "The person's own answer.",
		"expected_answer_at": agentAnswer.At.UTC().Format(time.RFC3339Nano),
	}, "alice")
	if changed.Code != http.StatusOK {
		t.Fatalf("alice changes the answer her agent made: status=%d body=%s", changed.Code, changed.Body.String())
	}
	if current := storedAskAnswer(t, database, askID); current.Act != nil || current.User != "alice" {
		t.Fatalf("the person's own answer = %+v, want alice's with no act", current)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the ask: status=%d body=%s", read.Code, read.Body.String())
	}
	history := decodeBody[struct {
		Answers []model.AskAnswer `json:"answers"`
	}](t, read).Answers
	if len(history) != 2 {
		t.Fatalf("the ask's answer history = %+v, want the agent's answer and the person's", history)
	}
	requireActingToken(t, "the first answer in the history", history[0].Act, actingSession, token, model.ScopePerson, model.ScopeApprove)
	if history[1].Act != nil {
		t.Fatalf("the person's own answer in the history carries act %+v, want none", history[1].Act)
	}

	mine := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/answers", nil, "alice")
	if mine.Code != http.StatusOK {
		t.Fatalf("read my answers: status=%d body=%s", mine.Code, mine.Body.String())
	}
	rows := decodeBody[model.MyAnswersResponse](t, mine).Rows
	if len(rows) != 2 || rows[0].Answer == nil || rows[1].Answer == nil {
		t.Fatalf("my answers = %+v, want both answers", rows)
	}
	// Newest first: the person's own answer, then the one their agent made.
	if rows[0].Answer.Act != nil {
		t.Fatalf("my answers' newest row carries act %+v, want none", rows[0].Answer.Act)
	}
	requireActingToken(t, "my answers' older row", rows[1].Answer.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)

	var notified []bool
	rowsOfEvents, err := database.Pool.Query(context.Background(), `
		select notify from events where type = 'ask.answered' and payload->>'id' = $1 order by id
	`, askID)
	if err != nil {
		t.Fatalf("read the ask.answered events: %v", err)
	}
	defer rowsOfEvents.Close()
	for rowsOfEvents.Next() {
		var notify bool
		if err := rowsOfEvents.Scan(&notify); err != nil {
			t.Fatalf("scan an ask.answered event: %v", err)
		}
		notified = append(notified, notify)
	}
	if !slices.Equal(notified, []bool{true, true}) {
		t.Fatalf("the ask.answered events notify = %v, want both to notify as a browser's answer does", notified)
	}
}

// The list of what people wrote to a session carries the act of an answer an agent made for its
// person, so a session reading its own answers sees which agent answered as well as who for.
func TestTheSessionsAnswersListCarriesTheActOfAnAnswerMadeForThePerson(t *testing.T) {
	f := newAnswersFixture(t)
	token := mintPersonalToken(t, f.handler, "alice", actingTokenName, nil)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	if answered := answerAsToken(t, f.handler, askID, token.Token, actingSession, map[string]any{
		"selected": []string{"Postgres"},
	}); answered.Code != http.StatusOK {
		t.Fatalf("the agent answers: status=%d body=%s", answered.Code, answered.Body.String())
	}
	f.publishOutbox(t)

	item := f.only(t, answersSession)
	requirePersonActor(t, "the listed answer's author", item.Author, "alice")
	requireActingToken(t, "the listed answer's author", item.Author.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)
}

// A message an agent sent for its person is recorded as the person with its act, and the accept
// refuses it: it is never taken as the session's own user turn, because the person did not type
// it. The same message the person sends in a browser is accepted, so the act alone is what
// refuses.
func TestAMessageAnAgentSentForItsPersonIsRecordedWithItsActAndRefusedByTheAccept(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)

	created := agentRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages?session="+actingSession, map[string]any{
		"body": "Where is the dashboard?", "delivery": "aside",
	}, token.Token)
	if created.Code != http.StatusCreated {
		t.Fatalf("send a direct message with a personal token: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	requirePersonActor(t, "the message's author", message.Author, "alice")
	requireActingToken(t, "the message's author", message.Author.Act, actingSession, token, model.ScopePerson, model.ScopeApprove)

	refused := acceptDelivery(t, handler, message.ID, 1, "s1")
	if refused.Code != http.StatusConflict || responseCode(t, refused) != "ACCEPT_WRITTEN_BY_AGENT" {
		t.Fatalf("accept a message an agent wrote: status=%d body=%s, want 409 ACCEPT_WRITTEN_BY_AGENT", refused.Code, refused.Body.String())
	}
	if len(acceptedEvents(t, database, message.ID)) != 0 {
		t.Fatal("the refused accept appended a message.accepted event")
	}

	own := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "And where are the logs?", "delivery": "aside",
	}, "alice")
	if own.Code != http.StatusCreated {
		t.Fatalf("send the same message in a browser: status=%d body=%s", own.Code, own.Body.String())
	}
	typed := decodeBody[model.Message](t, own)
	if typed.Author.Act != nil {
		t.Fatalf("a browser's message carries act %+v, want none", typed.Author.Act)
	}
	if accepted := acceptDelivery(t, handler, typed.ID, 1, "s1"); accepted.Code != http.StatusOK {
		t.Fatalf("accept the message the person typed: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
}

// A comment is recorded as it always was: the route was never one only a browser could call, so
// an agent presenting its person's token writes as the session it names, with that person as the
// owner behind it and no act at all.
func TestACommentUnderAPersonalTokenIsStillTheSessionsOwnWrite(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "ACT", "Comments are unchanged", "A spec")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)

	commented := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "The session's own comment.", "actor": map[string]any{"kind": "session", "id": actingSession},
	}, token.Token)
	if commented.Code != http.StatusCreated {
		t.Fatalf("comment with a personal token: status=%d body=%s", commented.Code, commented.Body.String())
	}
	commentID := decodeBody[struct {
		ID string `json:"id"`
	}](t, commented).ID

	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `select author from comments where id = $1`, commentID).Scan(&raw); err != nil {
		t.Fatalf("read the comment's author: %v", err)
	}
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		t.Fatalf("decode the comment's author %s: %v", raw, err)
	}
	if author.Kind != "session" || author.ID != actingSession {
		t.Fatalf("the comment's author = %s:%s, want the session it named", author.Kind, author.ID)
	}
	if author.Owner == nil || *author.Owner != "alice" {
		t.Fatalf("the comment's owner = %v, want the token's owner alice", author.Owner)
	}
	if author.Act != nil {
		t.Fatalf("the comment's author carries act %+v, want none: a comment is the session's own write", author.Act)
	}
}

// A machine login's decision writes no Dispatch event - the broker records it - so the agent that
// decided for its person is named on Dispatch's own access-log line: the session the caller named,
// and otherwise the token, by its owner, with the credential beside it either way.
func TestAMachineLoginDecisionNamesTheActingAgentOnItsAccessLogLine(t *testing.T) {
	for _, test := range []struct {
		name    string
		session string
		want    []string
	}{
		{name: "a caller that names its session", session: actingSession, want: []string{
			"caller_kind=session", "caller=" + actingSession, "credential=token:alice",
		}},
		{name: "a caller that names none", session: "", want: []string{
			"caller_kind=token", "caller=alice",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			broker := newFakeBrokerRig(t)
			handler := newCredentialTestHandler(t, broker.server.URL)
			token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)
			logged := captureLog(t)

			target := "/api/v1/credential-requests/rec1/approve"
			if test.session != "" {
				target += "?session=" + test.session
			}
			approved := agentRequest(t, handler, http.MethodPost, target, map[string]any{"code": "123456"}, token.Token)
			if approved.Code != http.StatusOK {
				t.Fatalf("approve a machine login with a personal token: status=%d body=%s", approved.Code, approved.Body.String())
			}
			lines := accessLines(t, logged)
			if len(lines) != 1 {
				t.Fatalf("the approve wrote %d access-log lines:\n%s", len(lines), strings.Join(lines, "\n"))
			}
			for _, want := range test.want {
				if !strings.Contains(lines[0], want) {
					t.Fatalf("access line %q does not carry %q", lines[0], want)
				}
			}
		})
	}
}

// Answering an approval request approves the document it names, so it needs the approve scope
// beside the person scope the route's own row declares, and a token narrowed to person alone is
// refused exactly as the gate refuses it - the code, the scope named and the challenge header.
// That token answers any other kind of ask, so it is the ask's kind alone that refuses.
func TestAnsweringAnApprovalAskNeedsTheApproveScope(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "ACT", "Approving needs approve", "A spec")
	narrow := mintPersonalToken(t, handler, "alice", "planner", []string{model.ScopePerson})

	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	approvalAsk := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID

	refused := answerAsToken(t, handler, approvalAsk, narrow.Token, actingSession, map[string]any{
		"selected": []string{docs.ApprovalOptionApprove},
	})
	if refused.Code != http.StatusForbidden {
		t.Fatalf("answer an approval ask without approve: status=%d body=%s, want 403", refused.Code, refused.Body.String())
	}
	body := decodeBody[refusalAnswer](t, refused)
	if body.Code != codeScopeRequired || !strings.Contains(body.Error, model.ScopeApprove) {
		t.Fatalf("the refusal = %+v, want %s naming the approve scope", body, codeScopeRequired)
	}
	if got, want := refused.Header().Get("WWW-Authenticate"), insufficientScopeChallenge(model.ScopeApprove); got != want {
		t.Fatalf("the refusal's challenge = %q, want %q", got, want)
	}
	var state string
	if err := database.Pool.QueryRow(context.Background(), `select state from asks where id = $1`, approvalAsk).Scan(&state); err != nil {
		t.Fatalf("read the approval ask's state: %v", err)
	}
	if state != "open" {
		t.Fatalf("the approval ask is %q after the refusal, want it still open", state)
	}

	question := openAskAs(t, handler, issue.Key, "session-asking", "Which option?")
	if answered := answerAsToken(t, handler, question, narrow.Token, actingSession, map[string]any{
		"selected": []string{"Yes"},
	}); answered.Code != http.StatusOK {
		t.Fatalf("answer an ordinary question with the same token: status=%d body=%s", answered.Code, answered.Body.String())
	}
}

// newestEventActor is the actor of the newest event of type on issueKey, read from the issue's
// own event log as any reader of it would.
func newestEventActor(t *testing.T, handler http.Handler, issueKey, eventType string) model.Actor {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s's events: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
	logged := decodeBody[[]struct {
		Type  string      `json:"type"`
		Actor model.Actor `json:"actor"`
	}](t, response)
	for index := len(logged) - 1; index >= 0; index-- {
		if logged[index].Type == eventType {
			return logged[index].Actor
		}
	}
	t.Fatalf("%s holds no %s event", issueKey, eventType)
	return model.Actor{}
}

// An agent acting on its own ask and its own following is doing its own work, not its person's,
// so it is recorded as the session acting for its owner, exactly as it was before the person
// scope existed. That matters beyond the record: events.Broker.Notify reads an actor of kind
// user as a person's act and sends it on, so a session recorded as its person would be handed
// back its own reword and its own follow as deliveries. The person's scope opens the other
// half - another session's ask, another session's following - which the two tests after this
// one hold.
func TestAnAgentsOwnRewordAndFollowAreStillTheSessionsOwnAct(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "OWN", "An agent's own acts", "A spec")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)
	askID := openAskAs(t, handler, issue.Key, actingSession, "Which option?")

	reworded := agentRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID,
		map[string]any{
			"question": "Which option, as the agent rewords its own?",
			"actor":    map[string]any{"kind": "session", "id": actingSession},
		}, token.Token)
	if reworded.Code != http.StatusOK {
		t.Fatalf("the agent rewords its own ask: status=%d body=%s", reworded.Code, reworded.Body.String())
	}
	if actor := newestEventActor(t, handler, issue.Key, "ask.edited"); actor.Kind != "session" || actor.ID != actingSession || actor.Act != nil {
		t.Fatalf("ask.edited actor = %#v, want the session %s acting for its owner with no act", actor, actingSession)
	}

	// Another session's ask, since a session already follows every ask it opened and a repeated
	// PUT is a no-op with no event: the agent joining that ask is still its own act.
	otherAsk := openAskAs(t, handler, issue.Key, "session-somebody-else", "Which other option?")
	followed := agentRequest(t, handler, http.MethodPut, "/api/v1/asks/"+otherAsk+"/followers/"+actingSession,
		map[string]any{"actor": map[string]any{"kind": "session", "id": actingSession}}, token.Token)
	if followed.Code != http.StatusNoContent {
		t.Fatalf("the agent follows an ask for itself: status=%d body=%s", followed.Code, followed.Body.String())
	}
	if actor := newestEventActor(t, handler, issue.Key, "ask.follower_added"); actor.Kind != "session" || actor.ID != actingSession || actor.Act != nil {
		t.Fatalf("ask.follower_added actor = %#v, want the session %s acting for its owner with no act", actor, actingSession)
	}
}

// The other half: acting on another session's ask, or putting another session on one, is the
// person's act, so it is recorded as the person with the agent named beside them.
func TestAnAgentsActOnAnotherSessionIsRecordedAsThePerson(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "OTH", "An act for the person", "A spec")
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)
	askID := openAskAs(t, handler, issue.Key, "session-somebody-else", "Which option?")

	reworded := agentRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID,
		map[string]any{
			"question": "Which option, as her agent words it for her?",
			"actor":    map[string]any{"kind": "session", "id": actingSession},
		}, token.Token)
	if reworded.Code != http.StatusOK {
		t.Fatalf("the agent rewords another session's ask: status=%d body=%s", reworded.Code, reworded.Body.String())
	}
	actor := newestEventActor(t, handler, issue.Key, "ask.edited")
	if actor.Kind != "user" || actor.ID != "alice" || actor.Act == nil || actor.Act.Session != actingSession {
		t.Fatalf("ask.edited actor = %#v, want alice acting through session %s", actor, actingSession)
	}

	followed := agentRequest(t, handler, http.MethodPut, "/api/v1/asks/"+askID+"/followers/session-watching",
		map[string]any{"actor": map[string]any{"kind": "session", "id": actingSession}}, token.Token)
	if followed.Code != http.StatusNoContent {
		t.Fatalf("the agent follows the ask for another session: status=%d body=%s", followed.Code, followed.Body.String())
	}
	added := newestEventActor(t, handler, issue.Key, "ask.follower_added")
	if added.Kind != "user" || added.ID != "alice" || added.Act == nil || added.Act.Session != actingSession {
		t.Fatalf("ask.follower_added actor = %#v, want alice acting through session %s", added, actingSession)
	}
}
