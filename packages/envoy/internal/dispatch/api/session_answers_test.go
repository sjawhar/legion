package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The session whose list each test reads, and another session beside it.
const (
	answersSession = "session-answers"
	answersOther   = "session-other"
)

// answersFixture is a server with one issue, an Envoy listener that holds both sessions live (so
// targeted messages, broadcasts and mentions are delivered and recorded sent), and the store and
// document service the outbox runs on. Every S1 case writes through the API and then runs the real
// outbox (publishOutbox), so published_destinations is what the publisher recorded.
type answersFixture struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	issue    string
	title    string
}

func newAnswersFixture(t *testing.T) *answersFixture {
	t.Helper()
	listener, _ := newBroadcastListener(t, `[
		{"session_id":"`+answersSession+`","title":"Answers","capabilities":["aside","btw","steer"],"last_seen":200},
		{"session_id":"`+answersOther+`","title":"Other","capabilities":["aside","btw","steer"],"last_seen":100}
	]`)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	const title = "Session answers"
	issue := createInteractionIssue(t, handler, "ANS", title, "The issue's spec.")
	return &answersFixture{handler: handler, database: database, deps: deps, issue: issue.Key, title: title}
}

func answersActor(session string) map[string]any {
	return map[string]any{"kind": "session", "id": session}
}

// write posts body to target as actor, a session id or a person's login ("alice", "bob"), and
// returns the created row's id.
func (f *answersFixture) write(t *testing.T, method, target string, body map[string]any, actor string) string {
	t.Helper()
	var response *httptest.ResponseRecorder
	if strings.HasPrefix(actor, "session-") {
		body["actor"] = answersActor(actor)
		response = sessionRequest(t, f.handler, method, target, body)
	} else {
		response = dispatchRequest(t, f.handler, method, target, body, actor)
	}
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("%s %s as %s: status=%d body=%s", method, target, actor, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID
}

// ask opens a question on the fixture's issue as session and returns its id.
func (f *answersFixture) ask(t *testing.T, session, question string) string {
	t.Helper()
	return f.write(t, http.MethodPost, "/api/v1/issues/"+f.issue+"/asks", map[string]any{
		"question": question,
		"options":  []map[string]string{{"label": "Postgres"}, {"label": "SQLite"}},
	}, session)
}

// answer answers ask as alice, replacing the answer at replaces when it is set.
func (f *answersFixture) answer(t *testing.T, askID string, selected []string, text string, replaces *time.Time) model.Ask {
	t.Helper()
	body := map[string]any{"selected": selected, "text": text}
	if replaces != nil {
		body["expected_answer_at"] = replaces.UTC().Format(time.RFC3339Nano)
	}
	response := dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", body, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("answer ask %s: status=%d body=%s", askID, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

// comment posts a comment on the fixture's issue as actor and returns its id.
func (f *answersFixture) comment(t *testing.T, body map[string]any, actor string) string {
	t.Helper()
	return f.write(t, http.MethodPost, "/api/v1/issues/"+f.issue+"/comments", body, actor)
}

// message posts a message on the fixture's issue as actor and returns its id.
func (f *answersFixture) message(t *testing.T, body map[string]any, actor string) string {
	t.Helper()
	return f.write(t, http.MethodPost, "/api/v1/issues/"+f.issue+"/messages", body, actor)
}

// route sets the fixture issue's route.
func (f *answersFixture) route(t *testing.T, route string) {
	t.Helper()
	if response := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{"route": route}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("route %s to %s: status=%d body=%s", f.issue, route, response.Code, response.Body.String())
	}
}

// publishOutbox runs the real outbox until every event that can carry a person's words to a session
// is published, so each one's published_destinations holds every topic the publisher reached.
func (f *answersFixture) publishOutbox(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.Run(ctx, outbox.Deps{Store: f.database, Publisher: acceptingPublisher{}, Broker: events.NewBroker(), Docs: f.deps.Docs})
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var unpublished int
		if err := f.database.Pool.QueryRow(context.Background(), `
			select count(*) from events
			where published_at is null
			  and type in ('ask.answered', 'comment.created', 'message.created', 'message.answered')
		`).Scan(&unpublished); err != nil {
			t.Fatalf("count unpublished events: %v", err)
		}
		if unpublished == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox left %d events unpublished", unpublished)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// destinations is the published_destinations of the one eventType event about id.
func (f *answersFixture) destinations(t *testing.T, eventType, id string) []string {
	t.Helper()
	var topics []string
	if err := f.database.Pool.QueryRow(context.Background(), `
		select published_destinations from events where type = $1 and payload->>'id' = $2
	`, eventType, id).Scan(&topics); err != nil {
		t.Fatalf("read %s destinations for %s: %v", eventType, id, err)
	}
	return topics
}

// sentCommentDelivery says whether a mention of comment was delivered to session.
func (f *answersFixture) sentCommentDelivery(t *testing.T, commentID, session string) bool {
	t.Helper()
	var sent bool
	if err := f.database.Pool.QueryRow(context.Background(), `
		select exists (select 1 from comment_deliveries where comment_id = $1 and session_id = $2 and state = 'sent')
	`, commentID, session).Scan(&sent); err != nil {
		t.Fatalf("read comment %s deliveries: %v", commentID, err)
	}
	return sent
}

// list reads session's list with query (a leading "?..." or "") as alice.
func (f *answersFixture) list(t *testing.T, session, query string) model.SessionAnswersResponse {
	t.Helper()
	response := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/agents/"+session+"/answers"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list %s answers%s: status=%d body=%s", session, query, response.Code, response.Body.String())
	}
	return decodeBody[model.SessionAnswersResponse](t, response)
}

// only is the one item of session's list, failing unless the list holds exactly one.
func (f *answersFixture) only(t *testing.T, session string) model.SessionAnswer {
	t.Helper()
	listed := f.list(t, session, "")
	if listed.Total != 1 || len(listed.Items) != 1 {
		t.Fatalf("%s answers = %+v, want exactly one item", session, listed)
	}
	return listed.Items[0]
}

// empty fails unless session's list holds nothing.
func (f *answersFixture) empty(t *testing.T, session string) {
	t.Helper()
	if listed := f.list(t, session, ""); listed.Total != 0 || len(listed.Items) != 0 {
		t.Fatalf("%s answers = %+v, want none", session, listed)
	}
}

// issuePlace checks an item lives on the fixture's issue at its kind's issue address.
func (f *answersFixture) issuePlace(t *testing.T, item model.SessionAnswer, segment string) {
	t.Helper()
	if item.Owner == nil || item.Owner.Issue == nil || item.Owner.Issue.Key != f.issue || item.Owner.Issue.Title != f.title || item.Owner.Document != nil {
		t.Fatalf("item owner = %+v, want issue %s %q", item.Owner, f.issue, f.title)
	}
	if want := "dispatch://" + f.issue + "/" + segment + "/" + item.ID; item.Ref == nil || *item.Ref != want {
		t.Fatalf("item ref = %v, want %s", item.Ref, want)
	}
	if want := "/issues/" + f.issue + "/" + segment + "s/" + item.ID; item.Href != want {
		t.Fatalf("item href = %q, want %s", item.Href, want)
	}
}

// answersAlice is alice, the person every test writes as, as an item's author.
var answersAlice = model.Actor{Kind: "user", ID: "alice"}

func TestSessionAnswersListsAPersonsAnswerToTheSessionsAsk(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	answered := f.answer(t, askID, []string{"Postgres"}, "Postgres, for the pool.", nil)
	f.publishOutbox(t)

	before := time.Now().UTC()
	listed := f.list(t, answersSession, "")
	if listed.SessionID != answersSession || listed.Total != 1 || len(listed.Items) != 1 {
		t.Fatalf("answers = %+v, want one item for %s", listed, answersSession)
	}
	if window := before.Sub(listed.Since); window < 72*time.Hour-time.Minute || window > 72*time.Hour+time.Minute {
		t.Fatalf("since = %s, want 72 hours before the read (%s)", listed.Since, before)
	}
	item := listed.Items[0]
	if item.Kind != model.SessionAnswerKindAnswer || item.Relation != model.SessionAnswerRelationAnsweredYourAsk || item.ID != askID {
		t.Fatalf("item = %+v, want the answer to ask %s", item, askID)
	}
	if item.Author != answersAlice || item.Text != "Postgres, for the pool." || !slices.Equal(item.Selected, []string{"Postgres"}) || item.Replaced {
		t.Fatalf("item words = %+v, want alice's answer word for word", item)
	}
	if !item.At.Equal(answered.Answer.At) || item.EditedAt != nil {
		t.Fatalf("item at = %s edited %v, want the answer's time %s", item.At, item.EditedAt, answered.Answer.At)
	}
	if item.Ask == nil || item.Ask.ID != askID || item.Ask.Question != "Which database backs the queue?" {
		t.Fatalf("item ask = %+v, want the session's whole question", item.Ask)
	}
	f.issuePlace(t, item, "ask")

	// auth any: the session reads its own list with a bearer too.
	if bearer := sessionRequest(t, f.handler, http.MethodGet, "/api/v1/agents/"+answersSession+"/answers", nil); bearer.Code != http.StatusOK {
		t.Fatalf("bearer read: status=%d body=%s", bearer.Code, bearer.Body.String())
	}
}

func TestSessionAnswersListsAReplacedAnswerOnceAtItsLatestWords(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	first := f.answer(t, askID, []string{"SQLite"}, "SQLite for now.", nil)
	second := f.answer(t, askID, []string{"Postgres"}, "Postgres after all.", &first.Answer.At)
	f.publishOutbox(t)

	item := f.only(t, answersSession)
	if item.ID != askID || item.Text != "Postgres after all." || !slices.Equal(item.Selected, []string{"Postgres"}) || !item.Replaced {
		t.Fatalf("item = %+v, want the latest answer marked replaced", item)
	}
	if !item.At.Equal(second.Answer.At) {
		t.Fatalf("item at = %s, want the latest answer's time %s", item.At, second.Answer.At)
	}
}

func TestSessionAnswersListsAPersonsReplyOnTheSessionsAsk(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	reply := f.comment(t, map[string]any{"body": "Which pool size do you need?", "ask_id": askID}, "alice")
	f.publishOutbox(t)

	item := f.only(t, answersSession)
	if item.Kind != model.SessionAnswerKindComment || item.Relation != model.SessionAnswerRelationRepliedOnYourAsk || item.ID != reply {
		t.Fatalf("item = %+v, want the reply %s on the session's ask", item, reply)
	}
	if item.Author != answersAlice || item.Text != "Which pool size do you need?" {
		t.Fatalf("item words = %+v", item)
	}
	if item.Ask == nil || item.Ask.ID != askID || item.Ask.Question != "Which database backs the queue?" {
		t.Fatalf("item ask = %+v, want the ask the reply is in", item.Ask)
	}
	f.issuePlace(t, item, "comment")
}

func TestSessionAnswersListsAReplyInTheSessionsCommentThread(t *testing.T) {
	f := newAnswersFixture(t)
	root := f.comment(t, map[string]any{"body": "I will cut the release on Friday."}, answersSession)
	reply := f.comment(t, map[string]any{"body": "Friday works.", "reply_to": root}, "alice")
	f.publishOutbox(t)

	item := f.only(t, answersSession)
	if item.ID != reply || item.Relation != model.SessionAnswerRelationRepliedInCommentThread || item.Text != "Friday works." || item.Ask != nil {
		t.Fatalf("item = %+v, want alice's reply %s in the thread", item, reply)
	}
	f.issuePlace(t, item, "comment")
}

func TestSessionAnswersListsACommentOnADocumentTheSessionWrote(t *testing.T) {
	f := newAnswersFixture(t)
	uploaded := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/projects/ANS/artifacts", map[string]any{
		"name": "Rollout plan", "content": "# Rollout plan\n\nShip on Friday.\n", "actor": answersActor(answersSession),
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	document := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	comment := f.write(t, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{"body": "Friday is a freeze day."}, "alice")
	f.publishOutbox(t)

	item := f.only(t, answersSession)
	if item.ID != comment || item.Relation != model.SessionAnswerRelationCommentedOnDocument || item.Text != "Friday is a freeze day." {
		t.Fatalf("item = %+v, want alice's comment %s on the document", item, comment)
	}
	if item.Owner == nil || item.Owner.Document == nil || item.Owner.Issue != nil ||
		*item.Owner.Document != (model.OpenAskDocument{Project: "ANS", Slug: document.Slug, Name: document.Name}) {
		t.Fatalf("item owner = %+v, want the project document %s", item.Owner, document.Slug)
	}
	if want := "dispatch://ANS/artifact/" + document.Slug + "/comment/" + comment; item.Ref == nil || *item.Ref != want {
		t.Fatalf("item ref = %v, want %s", item.Ref, want)
	}
	if want := "/projects/ANS/documents/" + document.Slug + "?comment=" + comment; item.Href != want {
		t.Fatalf("item href = %q, want %s", item.Href, want)
	}
}

func TestSessionAnswersListsACommentOnAnIssueRoutedToTheSession(t *testing.T) {
	f := newAnswersFixture(t)
	f.route(t, "session:"+answersSession)
	comment := f.comment(t, map[string]any{"body": "Please take the flaky test next."}, "alice")
	f.publishOutbox(t)

	item := f.only(t, answersSession)
	if item.ID != comment || item.Relation != model.SessionAnswerRelationCommentedOnIssue || item.Text != "Please take the flaky test next." {
		t.Fatalf("item = %+v, want alice's comment %s on the routed issue", item, comment)
	}
	f.issuePlace(t, item, "comment")
}

func TestSessionAnswersListsAMention(t *testing.T) {
	f := newAnswersFixture(t)
	comment := f.comment(t, map[string]any{
		"body": "@answers, can you look at the queue?", "mentions": []map[string]any{{"target": "session:" + answersSession}},
	}, "alice")
	f.publishOutbox(t)
	if !f.sentCommentDelivery(t, comment, answersSession) {
		t.Fatal("the mention was not delivered to the session")
	}
	if slices.Contains(f.destinations(t, "comment.created", comment), contracts.AgentSubject(answersSession)) {
		t.Fatal("the comment also reached the session's own topic; this case is the mention alone (S3)")
	}

	item := f.only(t, answersSession)
	if item.ID != comment || item.Relation != model.SessionAnswerRelationMentionedYou || item.Text != "@answers, can you look at the queue?" {
		t.Fatalf("item = %+v, want the mention %s", item, comment)
	}
	f.issuePlace(t, item, "comment")
	f.empty(t, answersOther)
}

func TestSessionAnswersListsATargetedMessage(t *testing.T) {
	f := newAnswersFixture(t)
	direct := f.write(t, http.MethodPost, "/api/v1/agents/"+answersSession+"/messages", map[string]any{"body": "Status, please.", "delivery": "btw"}, "alice")
	onIssue := f.message(t, map[string]any{"body": "Pick this up.", "target": "session:" + answersSession, "delivery": "btw"}, "alice")

	listed := f.list(t, answersSession, "")
	if listed.Total != 2 || len(listed.Items) != 2 || listed.Items[0].ID != onIssue || listed.Items[1].ID != direct {
		t.Fatalf("answers = %+v, want the issue message then the direct one", listed)
	}
	for _, item := range listed.Items {
		if item.Kind != model.SessionAnswerKindMessage || item.Relation != model.SessionAnswerRelationSentYouMessage || item.Author != answersAlice {
			t.Fatalf("item = %+v, want a message alice sent the session", item)
		}
	}
	f.issuePlace(t, listed.Items[0], "message")
	if item := listed.Items[1]; item.Text != "Status, please." || item.Owner != nil || item.Ref != nil || item.Href != "/agents/"+answersSession+"/live" {
		t.Fatalf("direct message item = %+v, want no owner, no ref and the session's conversation link", item)
	}
	f.empty(t, answersOther)
}

func TestSessionAnswersListsABroadcastCopy(t *testing.T) {
	f := newAnswersFixture(t)
	response := postBroadcast(t, f.handler, map[string]any{
		"body": "Stand down and report status.", "delivery": "btw", "session_ids": []string{answersSession, answersOther},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	// A broadcast delivers behind its create response; a copy is listed once its delivery is sent.
	copies := map[string]string{}
	for _, recipient := range awaitBroadcastDeliveries(t, f.handler, decodeBody[broadcastResponse](t, response).ID).Recipients {
		if len(recipient.Message.Deliveries) != 1 || recipient.Message.Deliveries[0].State != "sent" {
			t.Fatalf("recipient %s deliveries = %+v, want one sent attempt", recipient.SessionID, recipient.Message.Deliveries)
		}
		copies[recipient.SessionID] = recipient.Message.ID
	}

	item := f.only(t, answersSession)
	if item.ID != copies[answersSession] || item.Relation != model.SessionAnswerRelationSentYouMessage || item.Text != "Stand down and report status." {
		t.Fatalf("item = %+v, want the session's own copy %s", item, copies[answersSession])
	}
	if item.Owner != nil || item.Ref != nil || item.Href != "/agents/"+answersSession+"/live" {
		t.Fatalf("broadcast copy place = %+v", item)
	}
	if other := f.only(t, answersOther); other.ID != copies[answersOther] {
		t.Fatalf("the other session's item = %+v, want its own copy %s", other, copies[answersOther])
	}
}

func TestSessionAnswersListsAPersonsReplyToTheSessionsMessage(t *testing.T) {
	f := newAnswersFixture(t)
	root := f.message(t, map[string]any{"body": "The deploy is blocked on review."}, answersSession)
	reply := f.message(t, map[string]any{"body": "I will review it now.", "in_reply_to": root}, "alice")
	f.publishOutbox(t)
	if topics := f.destinations(t, "message.answered", reply); !slices.Contains(topics, contracts.AgentSubject(answersSession)) {
		t.Fatalf("the reply reached %v, want the session's own topic", topics)
	}

	item := f.only(t, answersSession)
	if item.ID != reply || item.Kind != model.SessionAnswerKindMessage || item.Relation != model.SessionAnswerRelationRepliedToYourMessage || item.Text != "I will review it now." {
		t.Fatalf("item = %+v, want alice's reply %s", item, reply)
	}
	f.issuePlace(t, item, "message")
}

func TestSessionAnswersLeavesOutAnAgentsCommentToTheSession(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	reply := f.comment(t, map[string]any{"body": "Postgres, I would say.", "ask_id": askID}, answersOther)
	f.publishOutbox(t)
	// The ask's follower route ignores Notify, so the agent's reply did reach the session's topic.
	if topics := f.destinations(t, "comment.created", reply); !slices.Contains(topics, contracts.AgentSubject(answersSession)) {
		t.Fatalf("the agent's reply reached %v, want the session's own topic", topics)
	}
	f.empty(t, answersSession)
}

func TestSessionAnswersLeavesOutAnEventDeliveredToAnotherSession(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersOther, "Which database backs the queue?")
	f.answer(t, askID, []string{"Postgres"}, "Postgres.", nil)
	f.publishOutbox(t)

	f.empty(t, answersSession)
	if item := f.only(t, answersOther); item.ID != askID {
		t.Fatalf("the other session's item = %+v, want the answer to its ask %s", item, askID)
	}
}

func TestSessionAnswersLeavesOutADeliveryToARoleRoute(t *testing.T) {
	f := newAnswersFixture(t)
	f.route(t, "role:reviewer")
	comment := f.comment(t, map[string]any{"body": "Reviewers, please look."}, "alice")
	f.publishOutbox(t)
	topics := f.destinations(t, "comment.created", comment)
	if !slices.Contains(topics, contracts.RoleTopicPrefix+"reviewer") {
		t.Fatalf("the comment reached %v, want the role's topic", topics)
	}
	for _, topic := range topics {
		if strings.HasPrefix(topic, contracts.AgentTopicPrefix) {
			t.Fatalf("the comment reached a session's own topic %s", topic)
		}
	}
	f.empty(t, answersSession)
	f.empty(t, answersOther)
}

func TestSessionAnswersLeavesOutAnAnswer73HoursOld(t *testing.T) {
	f := newAnswersFixture(t)
	old := f.ask(t, answersSession, "Old question?")
	recent := f.ask(t, answersSession, "Recent question?")
	f.answer(t, old, nil, "Old answer.", nil)
	f.answer(t, recent, nil, "Recent answer.", nil)
	f.publishOutbox(t)
	// Move each answer, and its event, back in time: 73 hours is outside the default window, and
	// 71 hours, the negative control, inside it.
	for askID, age := range map[string]time.Duration{old: 73 * time.Hour, recent: 71 * time.Hour} {
		at := time.Now().UTC().Add(-age)
		stamp := at.Format(time.RFC3339Nano)
		if _, err := f.database.Pool.Exec(context.Background(), `
			update asks set answer = jsonb_set(answer, '{at}', to_jsonb($2::text)) where id = $1
		`, askID, stamp); err != nil {
			t.Fatalf("age ask %s: %v", askID, err)
		}
		if _, err := f.database.Pool.Exec(context.Background(), `
			update events set created_at = $2, payload = jsonb_set(payload, '{answer,at}', to_jsonb($3::text))
			where type = 'ask.answered' and payload->>'id' = $1
		`, askID, at, stamp); err != nil {
			t.Fatalf("age ask %s's answer event: %v", askID, err)
		}
	}

	item := f.only(t, answersSession)
	if item.ID != recent || item.Text != "Recent answer." {
		t.Fatalf("item = %+v, want the 71-hour-old answer alone", item)
	}
}

func TestSessionAnswersListsAnEditedCommentAtItsCurrentBody(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	reply := f.comment(t, map[string]any{"body": "Use SQLite.", "ask_id": askID}, "alice")
	f.publishOutbox(t)
	if edited := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/comments/"+reply, map[string]any{"body": "Use Postgres."}, "alice"); edited.Code != http.StatusOK {
		t.Fatalf("edit comment: status=%d body=%s", edited.Code, edited.Body.String())
	}

	item := f.only(t, answersSession)
	if item.ID != reply || item.Text != "Use Postgres." {
		t.Fatalf("item = %+v, want the comment's current body", item)
	}
	if item.EditedAt == nil || item.EditedAt.Before(item.At) {
		t.Fatalf("item edited_at = %v, want the edit's time, after %s", item.EditedAt, item.At)
	}
}

// The author routes skip a session a comment's mention reaches (suppressed_authors), so a comment
// reaches one session both ways only where the ask's follower route does, which ignores mentions:
// a person's reply on the session's ask that also mentions it.
func TestSessionAnswersListsACommentReachedByAMentionAndARouteOnce(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	reply := f.comment(t, map[string]any{
		"body": "@answers, Postgres.", "ask_id": askID, "mentions": []map[string]any{{"target": "session:" + answersSession}},
	}, "alice")
	f.publishOutbox(t)
	if !f.sentCommentDelivery(t, reply, answersSession) {
		t.Fatal("the mention was not delivered to the session")
	}
	if topics := f.destinations(t, "comment.created", reply); !slices.Contains(topics, contracts.AgentSubject(answersSession)) {
		t.Fatalf("the reply reached %v, want the session's own topic too", topics)
	}

	item := f.only(t, answersSession)
	if item.ID != reply || item.Relation != model.SessionAnswerRelationMentionedYou || item.Ask == nil || item.Ask.ID != askID {
		t.Fatalf("item = %+v, want the reply once, as a mention in the ask's thread", item)
	}
}

func TestSessionAnswersListsNewestFirst(t *testing.T) {
	f := newAnswersFixture(t)
	askID := f.ask(t, answersSession, "Which database backs the queue?")
	f.answer(t, askID, []string{"Postgres"}, "", nil)
	reply := f.comment(t, map[string]any{"body": "And the pool size is 4.", "ask_id": askID}, "alice")
	message := f.write(t, http.MethodPost, "/api/v1/agents/"+answersSession+"/messages", map[string]any{"body": "Ship it.", "delivery": "btw"}, "alice")
	f.publishOutbox(t)

	listed := f.list(t, answersSession, "")
	ids := []string{}
	for _, item := range listed.Items {
		ids = append(ids, item.ID)
	}
	if want := []string{message, reply, askID}; !slices.Equal(ids, want) {
		t.Fatalf("order = %v, want %v (newest first)", ids, want)
	}
	for index := 1; index < len(listed.Items); index++ {
		if !listed.Items[index-1].At.After(listed.Items[index].At) {
			t.Fatalf("items %d and %d are not newest first: %s, %s", index-1, index, listed.Items[index-1].At, listed.Items[index].At)
		}
	}
	if answer := listed.Items[2]; answer.Text != "" || !slices.Equal(answer.Selected, []string{"Postgres"}) {
		t.Fatalf("an answer that chose options only = %+v, want empty text and its option", answer)
	}
}

func TestSessionAnswersMaxCharsStopsAfterTheCrossingItem(t *testing.T) {
	f := newAnswersFixture(t)
	bodies := []string{"0123456789", "abcdefghij", "😀😀😀😀😀"} // 10 UTF-16 units each, oldest first
	ids := make([]string, len(bodies))
	for index, body := range bodies {
		ids[index] = f.write(t, http.MethodPost, "/api/v1/agents/"+answersSession+"/messages", map[string]any{"body": body, "delivery": "btw"}, "alice")
	}
	newest := []string{ids[2], ids[1], ids[0]}
	for _, test := range []struct {
		maxChars string
		want     []string
	}{
		{"5", newest[:1]},  // the newest item alone passes 5
		{"10", newest[:2]}, // 10 is not past 10; the second item takes the sum to 20
		{"15", newest[:2]}, // the second item takes the sum past 15
		{"20", newest},     // 20 is not past 20; the third takes it to 30
		{"100000", newest}, // never passed
	} {
		listed := f.list(t, answersSession, "?max_chars="+test.maxChars)
		ids := []string{}
		for _, item := range listed.Items {
			ids = append(ids, item.ID)
		}
		if listed.Total != 3 || !slices.Equal(ids, test.want) {
			t.Fatalf("max_chars=%s: total=%d items=%v, want total 3 and %v", test.maxChars, listed.Total, ids, test.want)
		}
	}
	for _, query := range []string{"?max_chars=0", "?max_chars=100001", "?max_chars=ten", "?max_chars=5&max_chars=6"} {
		response := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/agents/"+answersSession+"/answers"+query, nil, "alice")
		if response.Code != http.StatusBadRequest || responseCode(t, response) != "INVALID_QUERY" {
			t.Fatalf("%s: status=%d body=%s, want 400 INVALID_QUERY", query, response.Code, response.Body.String())
		}
	}
}

func TestSessionAnswersRefusesASinceOlderThanSevenDays(t *testing.T) {
	f := newAnswersFixture(t)
	first := f.write(t, http.MethodPost, "/api/v1/agents/"+answersSession+"/messages", map[string]any{"body": "First.", "delivery": "btw"}, "alice")
	between := time.Now().UTC()
	second := f.write(t, http.MethodPost, "/api/v1/agents/"+answersSession+"/messages", map[string]any{"body": "Second.", "delivery": "btw"}, "alice")

	now := time.Now().UTC()
	for _, since := range []string{
		now.Add(-7*24*time.Hour - time.Minute).Format(time.RFC3339), // older than 7 days
		now.Add(time.Hour).Format(time.RFC3339),                     // in the future
		"yesterday",
	} {
		response := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/agents/"+answersSession+"/answers?since="+since, nil, "alice")
		if response.Code != http.StatusBadRequest || responseCode(t, response) != "INVALID_QUERY" {
			t.Fatalf("since=%s: status=%d body=%s, want 400 INVALID_QUERY", since, response.Code, response.Body.String())
		}
	}
	if all := f.list(t, answersSession, "?since="+now.Add(-7*24*time.Hour+time.Minute).Format(time.RFC3339)); all.Total != 2 {
		t.Fatalf("a since just inside 7 days = %+v, want both messages", all)
	}
	stamp := between.Format(time.RFC3339Nano)
	narrowed := f.list(t, answersSession, "?since="+stamp)
	if !narrowed.Since.Equal(between) || narrowed.Total != 1 || narrowed.Items[0].ID != second {
		t.Fatalf("since=%s = %+v, want the second message alone (not %s)", stamp, narrowed, first)
	}
}

func TestSessionAnswersRefusesABadSessionID(t *testing.T) {
	f := newAnswersFixture(t)
	for _, session := range []string{"bad%20id", "%3Cx%3E", "a%7Cb"} {
		response := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/agents/"+session+"/answers", nil, "alice")
		if response.Code != http.StatusBadRequest || responseCode(t, response) != "INVALID_SESSION_ID" {
			t.Fatalf("session %s: status=%d body=%s, want 400 INVALID_SESSION_ID", session, response.Code, response.Body.String())
		}
	}
}
