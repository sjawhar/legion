package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	replaySession    = "ses_replay"
	replayIssueTopic = "notifications.dispatch.issue.KEY-1.>"
)

// replayRig is a listener's /v1 routes over the test's own notification stream, with interests
// registered for replaySession.
type replayRig struct {
	deps *listenerDeps
	mux  *http.ServeMux
	js   nats.JetStreamContext
}

func newReplayRig(t *testing.T, topics ...string) *replayRig {
	t.Helper()
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, map[string][]string{replaySession: topics}, nil)
	deps := &listenerDeps{client: client, registry: registry, sessions: sessions}
	return &replayRig{deps: deps, mux: replayMux(deps), js: client.JS()}
}

func replayMux(deps *listenerDeps) *http.ServeMux {
	mux := http.NewServeMux()
	registerV1Routes(mux, deps, "test-machine", logging.New("test"))
	return mux
}

// storeOn publishes an envelope-shaped body on subject through JetStream and returns the body and the
// sequence the stream stored it at.
func storeOn(t *testing.T, js nats.JetStreamContext, subject string) (string, uint64) {
	t.Helper()
	body := fmt.Sprintf(`{"event_id":"evt-%d","topic":%q,"payload":"hello"}`, time.Now().UnixNano(), subject)
	ack, err := js.Publish(subject, []byte(body))
	if err != nil {
		t.Fatalf("store a message on %s: %v", subject, err)
	}
	return body, ack.Sequence
}

func (rig *replayRig) store(t *testing.T, subject string) (string, uint64) {
	t.Helper()
	return storeOn(t, rig.js, subject)
}

func postReplay(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/replay", strings.NewReader(body)))
	return recorder
}

func replayOK(t *testing.T, mux *http.ServeMux, body string) replayResponse {
	t.Helper()
	recorder := postReplay(t, mux, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var response replayResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode replay answer %s: %v", recorder.Body.String(), err)
	}
	return response
}

func replayBody(t *testing.T, subjects []replaySubjectRequest, maxMessages int) string {
	t.Helper()
	request := replayRequest{SessionID: replaySession, Subjects: subjects}
	if maxMessages > 0 {
		request.MaxMessages = new(maxMessages)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal replay request: %v", err)
	}
	return string(body)
}

func seqs(messages []replayMessage) []uint64 {
	out := make([]uint64, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Seq)
	}
	return out
}

func equalSeqs(got []replayMessage, want ...uint64) bool {
	return slices.Equal(seqs(got), want)
}

// The route returns what the stream stored after each cursor, on the session's own agent subject
// and on a wildcard issue topic it registered, and nothing stored on another session's subject or
// another issue. Each message carries its concrete subject, its stored time and its body as stored.
func TestReplayReturnsStoredMessagesAfterACursorForTheAgentSubjectAndAnIssueTopic(t *testing.T) {
	rig := newReplayRig(t, replayIssueTopic)
	agent := contracts.AgentSubject(replaySession)
	_, seenSeq := rig.store(t, agent)
	answerBody, answerSeq := rig.store(t, "notifications.dispatch.issue.KEY-1.ask.answered")
	rig.store(t, contracts.AgentSubject("ses_other"))
	rig.store(t, "notifications.dispatch.issue.KEY-2.ask.answered")
	missedBody, missedSeq := rig.store(t, agent)
	commentBody, commentSeq := rig.store(t, "notifications.dispatch.issue.KEY-1.comment.added")
	before := time.Now()

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{
		{Subject: agent, After: new(seenSeq)},
		{Subject: replayIssueTopic, After: new(uint64(0))},
	}, 0))

	if response.Stream.FirstSeq != 1 || response.Stream.LastSeq != commentSeq {
		t.Fatalf("stream = %+v, want first_seq 1 and last_seq %d", response.Stream, commentSeq)
	}
	if _, err := time.Parse(time.RFC3339, response.Stream.Created); err != nil {
		t.Fatalf("stream created %q is not RFC 3339: %v", response.Stream.Created, err)
	}
	if len(response.Subjects) != 2 {
		t.Fatalf("subjects = %+v, want two", response.Subjects)
	}
	agentResult, issueResult := response.Subjects[0], response.Subjects[1]
	if agentResult.Subject != agent || !equalSeqs(agentResult.Messages, missedSeq) {
		t.Fatalf("agent subject result = %+v, want only seq %d", agentResult, missedSeq)
	}
	if issueResult.Subject != replayIssueTopic || !equalSeqs(issueResult.Messages, answerSeq, commentSeq) {
		t.Fatalf("issue topic result = %+v, want seqs %d and %d", issueResult, answerSeq, commentSeq)
	}
	for _, result := range response.Subjects {
		if result.More || result.Refused != "" || result.NextAfter != commentSeq {
			t.Fatalf("%s: more %v, refused %q, next_after %d; want an exhausted subject at the watermark %d",
				result.Subject, result.More, result.Refused, result.NextAfter, commentSeq)
		}
	}
	for _, check := range []struct {
		message replayMessage
		subject string
		data    string
	}{
		{agentResult.Messages[0], agent, missedBody},
		{issueResult.Messages[0], "notifications.dispatch.issue.KEY-1.ask.answered", answerBody},
		{issueResult.Messages[1], "notifications.dispatch.issue.KEY-1.comment.added", commentBody},
	} {
		if check.message.Subject != check.subject || check.message.Data != check.data {
			t.Fatalf("message %d = %+v, want subject %s and data %s", check.message.Seq, check.message, check.subject, check.data)
		}
		stored, err := time.Parse(time.RFC3339, check.message.Time)
		if err != nil {
			t.Fatalf("message %d time %q is not RFC 3339: %v", check.message.Seq, check.message.Time, err)
		}
		if stored.After(before) || before.Sub(stored) > time.Minute {
			t.Fatalf("message %d time %s is not when it was stored (just before %s)", check.message.Seq, stored, before)
		}
	}
}

// A subject sent with no cursor gets no messages, only the watermark, which is where its cursor
// starts: following a topic never brings up its history.
func TestReplayWithoutAfterReturnsNoMessagesAndTheWatermark(t *testing.T) {
	rig := newReplayRig(t, replayIssueTopic)
	agent := contracts.AgentSubject(replaySession)
	rig.store(t, agent)
	rig.store(t, "notifications.dispatch.issue.KEY-1.ask.opened")
	_, lastSeq := rig.store(t, agent)

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{{Subject: agent}, {Subject: replayIssueTopic}}, 0))

	for _, result := range response.Subjects {
		if len(result.Messages) != 0 || result.More || result.NextAfter != lastSeq {
			t.Fatalf("%s: %+v, want no messages and next_after = watermark %d", result.Subject, result, lastSeq)
		}
	}
}

// publishingStreamInfo reads the real stream info, then stores one more message on subject before
// handing it back: a message that lands after the request read its watermark.
type publishingStreamInfo struct {
	t       *testing.T
	js      nats.JetStreamContext
	subject string
	stored  uint64
}

func (p *publishingStreamInfo) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	info, err := p.js.StreamInfo(stream, opts...)
	if err == nil && p.stored == 0 {
		_, p.stored = storeOn(p.t, p.js, p.subject)
	}
	return info, err
}

// The watermark is the stream's last sequence when the request began: a message stored after it
// is not in the answer, next_after is that watermark, and the next replay from it returns the
// message.
func TestReplayHonoursTheWatermarkReadAtTheStartOfTheRequest(t *testing.T) {
	rig := newReplayRig(t)
	agent := contracts.AgentSubject(replaySession)
	_, firstSeq := rig.store(t, agent)
	_, watermark := rig.store(t, agent)
	late := &publishingStreamInfo{t: t, js: rig.js, subject: agent}
	rig.deps.streamInfo = late

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{{Subject: agent, After: new(firstSeq)}}, 0))

	if late.stored != watermark+1 {
		t.Fatalf("the late message was stored at %d, want %d", late.stored, watermark+1)
	}
	if response.Stream.LastSeq != watermark {
		t.Fatalf("stream last_seq = %d, want the watermark %d", response.Stream.LastSeq, watermark)
	}
	result := response.Subjects[0]
	if !equalSeqs(result.Messages, watermark) || result.NextAfter != watermark || result.More {
		t.Fatalf("result = %+v, want only seq %d, next_after %d and no more", result, watermark, watermark)
	}

	next := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{{Subject: agent, After: new(result.NextAfter)}}, 0))
	if !equalSeqs(next.Subjects[0].Messages, late.stored) || next.Subjects[0].NextAfter != late.stored {
		t.Fatalf("next replay = %+v, want the late message %d", next.Subjects[0], late.stored)
	}
}

// max_messages bounds the whole answer: the subject it cuts short says more with next_after at its
// last returned message, a later subject it reaches no message of says more with next_after at its
// own cursor, and paging from next_after returns the rest, ending exhausted at the watermark.
func TestReplayPagesWithMaxMessagesAndResumesFromNextAfter(t *testing.T) {
	rig := newReplayRig(t, replayIssueTopic)
	agent := contracts.AgentSubject(replaySession)
	var agentSeqs []uint64
	for range 5 {
		_, seq := rig.store(t, agent)
		agentSeqs = append(agentSeqs, seq)
	}
	_, issueSeq := rig.store(t, "notifications.dispatch.issue.KEY-1.ask.opened")
	watermark := issueSeq

	first := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{
		{Subject: agent, After: new(uint64(0))},
		{Subject: replayIssueTopic, After: new(uint64(0))},
	}, 2))
	agentResult, issueResult := first.Subjects[0], first.Subjects[1]
	if !equalSeqs(agentResult.Messages, agentSeqs[0], agentSeqs[1]) || !agentResult.More || agentResult.NextAfter != agentSeqs[1] {
		t.Fatalf("first page agent = %+v, want seqs %v, more, next_after %d", agentResult, agentSeqs[:2], agentSeqs[1])
	}
	if len(issueResult.Messages) != 0 || !issueResult.More || issueResult.NextAfter != 0 {
		t.Fatalf("first page issue = %+v, want no messages, more, next_after at its cursor 0", issueResult)
	}

	cursors := map[string]uint64{agent: agentResult.NextAfter, replayIssueTopic: issueResult.NextAfter}
	got := map[string][]uint64{agent: seqs(agentResult.Messages)}
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatalf("paging did not end: %v", got)
		}
		response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{
			{Subject: agent, After: new(cursors[agent])},
			{Subject: replayIssueTopic, After: new(cursors[replayIssueTopic])},
		}, 2))
		more := false
		for _, result := range response.Subjects {
			if len(result.Messages) > 2 {
				t.Fatalf("a page of max_messages 2 returned %d messages for %s", len(result.Messages), result.Subject)
			}
			got[result.Subject] = append(got[result.Subject], seqs(result.Messages)...)
			cursors[result.Subject] = result.NextAfter
			more = more || result.More
		}
		if !more {
			break
		}
	}
	if fmt.Sprint(got[agent]) != fmt.Sprint(agentSeqs) || fmt.Sprint(got[replayIssueTopic]) != fmt.Sprint([]uint64{issueSeq}) {
		t.Fatalf("pages returned %v, want agent %v and issue [%d] once each", got, agentSeqs, issueSeq)
	}
	if cursors[agent] != watermark || cursors[replayIssueTopic] != watermark {
		t.Fatalf("final cursors = %v, want both at the watermark %d", cursors, watermark)
	}
}

// A request names at most 32 subjects.
func TestReplayRefusesMoreThan32Subjects(t *testing.T) {
	mux := replayMux(&listenerDeps{})
	subjects := make([]replaySubjectRequest, 33)
	for index := range subjects {
		subjects[index] = replaySubjectRequest{Subject: fmt.Sprintf("notifications.dispatch.issue.KEY-%d.>", index)}
	}
	recorder := postReplay(t, mux, replayBody(t, subjects, 0))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
}

// A malformed body, a missing session id, a subject NATS would not filter on and a max_messages
// below one are refused before anything is read.
func TestReplayRefusesAMalformedRequest(t *testing.T) {
	mux := replayMux(&listenerDeps{})
	for name, body := range map[string]string{
		"not json":           `{"session_id":`,
		"no session":         `{"subjects":[{"subject":"notifications.agent.ses_replay"}]}`,
		"blank session":      `{"session_id":"  ","subjects":[]}`,
		"empty subject":      `{"session_id":"ses_replay","subjects":[{"subject":""}]}`,
		"empty token":        `{"session_id":"ses_replay","subjects":[{"subject":"notifications..agent"}]}`,
		"inner full wild":    `{"session_id":"ses_replay","subjects":[{"subject":"notifications.>.agent"}]}`,
		"negative after":     `{"session_id":"ses_replay","subjects":[{"subject":"notifications.agent.ses_replay","after":-1}]}`,
		"zero max messages":  `{"session_id":"ses_replay","subjects":[],"max_messages":0}`,
		"whitespace subject": `{"session_id":"ses_replay","subjects":[{"subject":"notifications.agent.a b"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := postReplay(t, mux, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// A session_id that is not one literal NATS token is refused 400 naming session_id, before anything
// is read: as a wildcard it would pass notifications.agent.> as its own agent subject and read every
// session's inbox. A plain session id is still answered.
func TestReplayRefusesASessionIDThatIsNotOneNATSToken(t *testing.T) {
	rig := newReplayRig(t)
	rig.store(t, contracts.AgentSubject("ses_other"))
	_, ownSeq := rig.store(t, contracts.AgentSubject(replaySession))

	for _, sessionID := range []string{">", "*", "a.b", "a b", "a\tb", "a\u0001b", "ses_replay.>"} {
		t.Run(sessionID, func(t *testing.T) {
			body, err := json.Marshal(replayRequest{SessionID: sessionID, Subjects: []replaySubjectRequest{
				{Subject: "notifications.agent.>", After: new(uint64(0))},
				{Subject: contracts.AgentSubject(sessionID), After: new(uint64(0))},
			}})
			if err != nil {
				t.Fatalf("marshal replay request: %v", err)
			}
			recorder := postReplay(t, rig.mux, string(body))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode refusal %s: %v", recorder.Body.String(), err)
			}
			if !slices.Equal(refusal.Expected, []string{"session_id"}) {
				t.Fatalf("refusal = %+v, want expected [session_id]", refusal)
			}
		})
	}

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{
		{Subject: contracts.AgentSubject(replaySession), After: new(uint64(0))},
	}, 0))
	if own := response.Subjects[0]; own.Refused != "" || !equalSeqs(own.Messages, ownSeq) {
		t.Fatalf("own agent subject = %+v, want seq %d", own, ownSeq)
	}
}

// A session's replay never returns another session's inbox, also when its registered interests name
// one: /v1/interests/subscribe takes any topic for any session id, so an interest in
// notifications.agent.> or in another session's agent subject, registered with one more call, must
// not turn /v1/replay into a read of every session's stored messages. Its own agent subject is still
// answered.
func TestReplayNeverReturnsAnotherSessionsInboxThroughARegisteredInterest(t *testing.T) {
	other := contracts.AgentSubject("ses_other")
	rig := newReplayRig(t, "notifications.agent.>", other, "notifications.agent.*")
	rig.store(t, other)
	_, ownSeq := rig.store(t, contracts.AgentSubject(replaySession))

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{
		{Subject: "notifications.agent.>", After: new(uint64(0))},
		{Subject: other, After: new(uint64(0))},
		{Subject: "notifications.agent.*", After: new(uint64(0))},
		{Subject: contracts.AgentSubject(replaySession), After: new(uint64(0))},
	}, 0))

	for _, result := range response.Subjects[:3] {
		for _, message := range result.Messages {
			if message.Subject != contracts.AgentSubject(replaySession) {
				t.Fatalf("%s returned %s's stored message (seq %d): want another session's inbox never answered", result.Subject, message.Subject, message.Seq)
			}
		}
		if result.Refused == "" {
			t.Fatalf("%s = %+v, want it refused: it reads other sessions' inboxes", result.Subject, result)
		}
	}
	if own := response.Subjects[3]; own.Refused != "" || !equalSeqs(own.Messages, ownSeq) {
		t.Fatalf("own agent subject = %+v, want seq %d", own, ownSeq)
	}
}

// The exception lane of the agent subjects carries another session's inbox too: a delivery that fails
// to a session is reported on notifications.envoy.exceptions.<its agent subject> with the original
// message's payload, and the stream keeps that lane. A session's replay never returns another
// session's exception lane, through a wildcard over it or through that session's own lane by name,
// also when its registered interests name them.
func TestReplayNeverReturnsAnotherSessionsExceptionLaneThroughARegisteredInterest(t *testing.T) {
	lane := bus.ExceptionTopicPrefix + contracts.AgentTopicPrefix + ">"
	otherLane := bus.ExceptionTopicPrefix + contracts.AgentSubject("ses_other")
	rig := newReplayRig(t, lane, otherLane)
	rig.store(t, otherLane)
	_, ownSeq := rig.store(t, contracts.AgentSubject(replaySession))

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{
		{Subject: lane, After: new(uint64(0))},
		{Subject: otherLane, After: new(uint64(0))},
		{Subject: contracts.AgentSubject(replaySession), After: new(uint64(0))},
	}, 0))

	for _, result := range response.Subjects[:2] {
		if len(result.Messages) != 0 || result.Refused == "" {
			t.Fatalf("%s = %+v, want it refused with no messages: it reads another session's undelivered messages", result.Subject, result)
		}
	}
	if own := response.Subjects[2]; own.Refused != "" || !equalSeqs(own.Messages, ownSeq) {
		t.Fatalf("own agent subject = %+v, want seq %d", own, ownSeq)
	}
}

// A registration naming topics that reach another session's inbox stores the rest of it and names
// each topic it left out: a wildcard over the agent subjects, another session's agent subject, and
// notifications.> are not stored, while the session's own agent subject and an issue topic are, so
// a replay of them answers the own subject and the issue and refuses the rest. The registration is
// not refused whole, since a released pi-envoy registers a fork parent's agent subject with the
// rest of what it follows.
func TestInterestSubscribeStoresOnlyTopicsThatReplayCannotTurnIntoAnotherSessionsInbox(t *testing.T) {
	rig := newReplayRig(t)
	own, other := contracts.AgentSubject(replaySession), contracts.AgentSubject("ses_other")
	refused := []string{"notifications.agent.>", "notifications.agent.*", other, "notifications.>"}
	answer := subscribeInterest(t, rig.mux, append([]string{own, replayIssueTopic}, refused...))
	if want := []string{own, replayIssueTopic}; !slices.Equal(answer.Topics, want) {
		t.Fatalf("answered topics = %v, want %v", answer.Topics, want)
	}
	if len(answer.Warnings) != len(refused) {
		t.Fatalf("warnings = %q, want one naming each of %v", answer.Warnings, refused)
	}
	for index, topic := range refused {
		if !strings.HasPrefix(answer.Warnings[index], topic+" was not registered") {
			t.Fatalf("warning %d = %q, want it to name %s as not registered", index, answer.Warnings[index], topic)
		}
	}
	stored, err := rig.deps.registry.Get(replaySession)
	if err != nil {
		t.Fatalf("read the stored interest: %v", err)
	}
	if want := []string{own, replayIssueTopic}; !slices.Equal(stored.Topics, want) {
		t.Fatalf("stored topics = %v, want %v", stored.Topics, want)
	}

	rig.store(t, other)
	_, ownSeq := rig.store(t, own)
	issueBody, issueSeq := rig.store(t, "notifications.dispatch.issue.KEY-1.ask.opened")
	subjects := []replaySubjectRequest{{Subject: own, After: new(uint64(0))}, {Subject: replayIssueTopic, After: new(uint64(0))}}
	for _, topic := range refused {
		subjects = append(subjects, replaySubjectRequest{Subject: topic, After: new(uint64(0))})
	}
	response := replayOK(t, rig.mux, replayBody(t, subjects, 0))
	if got := response.Subjects[0]; got.Refused != "" || !equalSeqs(got.Messages, ownSeq) {
		t.Fatalf("own agent subject = %+v, want seq %d", got, ownSeq)
	}
	if got := response.Subjects[1]; got.Refused != "" || !equalSeqs(got.Messages, issueSeq) || got.Messages[0].Data != issueBody {
		t.Fatalf("issue topic = %+v, want seq %d", got, issueSeq)
	}
	for _, result := range response.Subjects[2:] {
		if result.Refused != "not_followed" || len(result.Messages) != 0 {
			t.Fatalf("%s = %+v, want refused not_followed with no messages", result.Subject, result)
		}
	}
}

// Every private stream subject is left out of a registration, not only the agent subjects: a
// failed-delivery lane carries the undelivered message's payload and the session it was for (the
// session's own lane included), and the Legion controller lane is heard through the controller
// role and never registered, so neither is stored, nor is a wildcard reaching them
// (notifications.envoy.>). A GitHub topic, a team topic and the session's own agent subject are
// stored, and the answer stays 200.
func TestInterestSubscribeLeavesOutEveryPrivateStreamSubject(t *testing.T) {
	rig := newReplayRig(t)
	own := contracts.AgentSubject(replaySession)
	githubTopic, teamTopic := "notifications.github.acme.widgets.pr.7.>", "notifications.team.x.>"
	refused := []string{
		bus.ExceptionTopicPrefix + contracts.AgentTopicPrefix + ">",
		bus.ExceptionTopicPrefix + own,
		"notifications.legion.ACME.controller",
		"notifications.envoy.>",
	}
	answer := subscribeInterest(t, rig.mux, append([]string{githubTopic, teamTopic, own}, refused...))

	// Stored topics are kept sorted.
	want := []string{own, githubTopic, teamTopic}
	if !slices.Equal(answer.Topics, want) {
		t.Fatalf("answered topics = %v, want %v", answer.Topics, want)
	}
	stored, err := rig.deps.registry.Get(replaySession)
	if err != nil {
		t.Fatalf("read the stored interest: %v", err)
	}
	if !slices.Equal(stored.Topics, want) {
		t.Fatalf("stored topics = %v, want %v", stored.Topics, want)
	}
	// The GitHub topic may draw its own unwired-repository warning; only the ones left out are
	// named as not registered.
	notRegistered := make([]string, 0, len(refused))
	for _, warning := range answer.Warnings {
		if topic, _, found := strings.Cut(warning, " was not registered"); found {
			notRegistered = append(notRegistered, topic)
		}
	}
	if !slices.Equal(notRegistered, refused) {
		t.Fatalf("warnings = %q, want exactly %v named as not registered", answer.Warnings, refused)
	}
}

// A row an earlier build stored with topics reaching a private stream subject loses them at the
// session's next registration, not only the topics that registration names: after one heartbeat
// the row holds the session's own agent subject, the issue topic it followed and the topic it
// registers now, and the listener's fan-out (Registry.Match) stops handing it another session's
// failed-delivery lane and the Legion controller's notices.
func TestInterestSubscribeCleansAStoredRowOfTopicsReachingPrivateStreamSubjects(t *testing.T) {
	const githubTopic = "notifications.github.acme.widgets.pr.7.>"
	own, other := contracts.AgentSubject(replaySession), contracts.AgentSubject("ses_other")
	lane := bus.ExceptionTopicPrefix + other
	controller := "notifications.legion.ACME.controller"
	rig := newReplayRig(t, bus.ExceptionTopicPrefix+">", controller, other, replayIssueTopic)
	for _, subject := range []string{lane, controller, other} {
		if !slices.ContainsFunc(rig.deps.registry.Match("test-machine", subject), func(item store.Interest) bool { return item.SessionID == replaySession }) {
			t.Fatalf("Match(%s) before the heartbeat misses %s: the seeded row must reach it", subject, replaySession)
		}
	}

	answer := subscribeInterest(t, rig.mux, []string{own, githubTopic})

	want := []string{own, replayIssueTopic, githubTopic}
	if !slices.Equal(answer.Topics, want) {
		t.Fatalf("answered topics = %v, want %v", answer.Topics, want)
	}
	stored, err := rig.deps.registry.Get(replaySession)
	if err != nil {
		t.Fatalf("read the stored interest: %v", err)
	}
	if !slices.Equal(stored.Topics, want) {
		t.Fatalf("stored topics = %v, want %v", stored.Topics, want)
	}
	for _, subject := range []string{lane, controller, other} {
		if matched := rig.deps.registry.Match("test-machine", subject); len(matched) != 0 {
			t.Fatalf("Match(%s) = %v, want no session after the heartbeat", subject, matched)
		}
	}
}

// interestSubscribeAnswer is the part of a /v1/interests/subscribe answer these tests read.
type interestSubscribeAnswer struct {
	Topics   []string `json:"topics"`
	Warnings []string `json:"warnings"`
}

// subscribeInterest registers topics for replaySession through /v1/interests/subscribe, requires a
// 200, and returns the answer.
func subscribeInterest(t *testing.T, mux *http.ServeMux, topics []string) interestSubscribeAnswer {
	t.Helper()
	body, err := json.Marshal(subscribeBody{SessionID: replaySession, Topics: topics, SelfSubscribed: true})
	if err != nil {
		t.Fatalf("marshal subscribe: %v", err)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(string(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("subscribe status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var answer interestSubscribeAnswer
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode subscribe answer %s: %v", recorder.Body.String(), err)
	}
	return answer
}

// Only POST is served.
func TestReplayAnswers405ForGet(t *testing.T) {
	mux := replayMux(&listenerDeps{})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/replay", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body = %s", recorder.Code, recorder.Body.String())
	}
}

// A subject that is neither the session's agent subject nor one of its registered interests is
// refused not_followed, with no messages and next_after its own cursor, even when the stream holds
// messages on it; the subjects it does follow are still answered.
func TestReplayRefusesASubjectTheSessionDoesNotFollow(t *testing.T) {
	rig := newReplayRig(t, replayIssueTopic)
	other := contracts.AgentSubject("ses_other")
	rig.store(t, other)
	rig.store(t, "notifications.dispatch.issue.KEY-9.ask.opened")
	_, ownSeq := rig.store(t, contracts.AgentSubject(replaySession))

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{
		{Subject: other, After: new(uint64(0))},
		{Subject: "notifications.dispatch.issue.KEY-9.>"},
		{Subject: "notifications.dispatch.issue.KEY-1.ask.opened", After: new(uint64(1))},
		{Subject: contracts.AgentSubject(replaySession), After: new(uint64(0))},
	}, 0))

	for index, wantAfter := range []uint64{0, 0, 1} {
		result := response.Subjects[index]
		if result.Refused != "not_followed" || len(result.Messages) != 0 || result.More || result.NextAfter != wantAfter {
			t.Fatalf("%s = %+v, want refused not_followed with next_after %d", result.Subject, result, wantAfter)
		}
	}
	if own := response.Subjects[3]; own.Refused != "" || !equalSeqs(own.Messages, ownSeq) {
		t.Fatalf("own agent subject = %+v, want seq %d", own, ownSeq)
	}

	// A session the interest store holds no record of follows only its own agent subject.
	stranger := replayOK(t, rig.mux, `{"session_id":"ses_stranger","subjects":[`+
		`{"subject":"notifications.agent.ses_stranger","after":0},{"subject":"notifications.dispatch.issue.KEY-1.>","after":0}]}`)
	if stranger.Subjects[0].Refused != "" || stranger.Subjects[1].Refused != "not_followed" {
		t.Fatalf("unregistered session = %+v, want its agent subject answered and the issue topic not_followed", stranger.Subjects)
	}
}

// A registered topic is answered only under a shared stream subject: a wildcard over every
// notification, a wildcard over every exception lane, the session's own exception lane, the Legion
// controller's notices, a wildcard wider than one shared subject and an ad-hoc team topic are
// refused not_followed with no messages and next_after their own cursor, even when the stream
// holds messages on them, while a registered issue topic and a GitHub pull request topic are
// still answered.
func TestReplayAnswersARegisteredTopicOnlyUnderASharedStreamSubject(t *testing.T) {
	const pullRequest = "notifications.github.acme.widgets.pr.7.>"
	ownLane := bus.ExceptionTopicPrefix + contracts.AgentSubject(replaySession)
	controller := "notifications.legion.ACME.controller"
	refused := []string{
		"notifications.>",
		bus.ExceptionTopicPrefix + ">",
		ownLane,
		controller,
		"notifications.*",
		"notifications.team.x.y",
	}
	rig := newReplayRig(t, append([]string{replayIssueTopic, pullRequest}, refused...)...)
	rig.store(t, ownLane)
	rig.store(t, controller)
	issueBody, issueSeq := rig.store(t, "notifications.dispatch.issue.KEY-1.ask.opened")
	prBody, prSeq := rig.store(t, "notifications.github.acme.widgets.pr.7.checks")

	subjects := []replaySubjectRequest{
		{Subject: replayIssueTopic, After: new(uint64(0))},
		{Subject: pullRequest, After: new(uint64(0))},
	}
	for _, topic := range refused {
		subjects = append(subjects, replaySubjectRequest{Subject: topic, After: new(uint64(0))})
	}
	response := replayOK(t, rig.mux, replayBody(t, subjects, 0))

	for index, want := range []struct {
		seq  uint64
		data string
	}{{issueSeq, issueBody}, {prSeq, prBody}} {
		if got := response.Subjects[index]; got.Refused != "" || !equalSeqs(got.Messages, want.seq) || got.Messages[0].Data != want.data {
			t.Fatalf("%s = %+v, want seq %d answered", got.Subject, got, want.seq)
		}
	}
	for _, result := range response.Subjects[2:] {
		if result.Refused != "not_followed" || len(result.Messages) != 0 || result.More || result.NextAfter != 0 {
			t.Fatalf("%s = %+v, want refused not_followed with no messages and next_after 0", result.Subject, result)
		}
	}
}

// A registered topic under a shared stream subject that no subject of the deployed stream's
// configuration contains is refused not_retained, with no messages and next_after its own cursor.
// The deployed configuration is what counts, not this binary's list: once the stream carries the
// subject again, the topic is answered.
func TestReplayRefusesAFollowedSubjectTheStreamDoesNotRetain(t *testing.T) {
	const chat = "notifications.whatsapp.15551234567.>"
	rig := newReplayRig(t, chat)
	info, err := rig.js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read stream info: %v", err)
	}
	config := info.Config
	config.Subjects = slices.DeleteFunc(slices.Clone(info.Config.Subjects), func(subject string) bool {
		return subject == "notifications.whatsapp.>"
	})
	if _, err := rig.js.UpdateStream(&config); err != nil {
		t.Fatalf("deploy a stream without the WhatsApp subject: %v", err)
	}

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{{Subject: chat, After: new(uint64(3))}}, 0))
	if result := response.Subjects[0]; result.Refused != "not_retained" || len(result.Messages) != 0 || result.NextAfter != 3 {
		t.Fatalf("%s = %+v, want refused not_retained with next_after 3", result.Subject, result)
	}

	if _, err := rig.js.UpdateStream(&info.Config); err != nil {
		t.Fatalf("deploy the stream with the WhatsApp subject again: %v", err)
	}
	_, chatSeq := rig.store(t, "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message")

	retained := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{{Subject: chat, After: new(uint64(0))}}, 0))
	if result := retained.Subjects[0]; result.Refused != "" || !equalSeqs(result.Messages, chatSeq) {
		t.Fatalf("WhatsApp topic under the deployed subject = %+v, want seq %d", result, chatSeq)
	}
}

// A stream deleted and created again answers with a different created time, which is how a session
// tells its cursors belong to the old stream.
func TestReplayReportsARecreatedStream(t *testing.T) {
	rig := newReplayRig(t)
	agent := contracts.AgentSubject(replaySession)
	rig.store(t, agent)
	rig.store(t, agent)
	body := replayBody(t, []replaySubjectRequest{{Subject: agent}}, 0)
	before := replayOK(t, rig.mux, body)

	info, err := rig.js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read stream info: %v", err)
	}
	if err := rig.js.DeleteStream(bus.Stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	if _, err := rig.js.AddStream(&info.Config); err != nil {
		t.Fatalf("create the stream again: %v", err)
	}
	_, seq := rig.store(t, agent)

	recreated := replayOK(t, rig.mux, body)
	if recreated.Stream.Created == before.Stream.Created {
		t.Fatalf("created = %q both before and after the stream was recreated", before.Stream.Created)
	}
	if recreated.Stream.LastSeq != seq || recreated.Subjects[0].NextAfter != seq {
		t.Fatalf("recreated stream = %+v, subject %+v; want last_seq and next_after %d", recreated.Stream, recreated.Subjects[0], seq)
	}
}

// The answer names the stream's id, its bus.StreamIDMetadataKey, which a backup and restore and a
// leader election keep where they change created; a stream without one answers no id at all.
func TestReplayAnswersTheStreamsID(t *testing.T) {
	rig := newReplayRig(t)
	agent := contracts.AgentSubject(replaySession)
	rig.store(t, agent)
	body := replayBody(t, []replaySubjectRequest{{Subject: agent}}, 0)
	info, err := rig.js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read stream info: %v", err)
	}
	want := info.Config.Metadata[bus.StreamIDMetadataKey]
	if want == "" {
		t.Fatalf("stream metadata = %v, want the id the listener's start sets", info.Config.Metadata)
	}

	if got := replayOK(t, rig.mux, body).Stream.ID; got != want {
		t.Fatalf("stream id = %q, want %q", got, want)
	}

	unidentified := info.Config
	unidentified.Metadata = nil
	if _, err := rig.js.UpdateStream(&unidentified); err != nil {
		t.Fatalf("remove the stream's metadata: %v", err)
	}
	recorder := postReplay(t, rig.mux, body)
	var answer struct {
		Stream map[string]any `json:"stream"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode replay answer %s: %v", recorder.Body.String(), err)
	}
	if id, present := answer.Stream["id"]; present {
		t.Fatalf("stream id = %v for a stream without one, want no id", id)
	}
}

// A cursor below the stream's first sequence, after older messages were removed, still reads from
// the first message the stream holds, and the answer names that first sequence.
func TestReplayReadsFromTheFirstRetainedMessageWhenTheCursorIsBelowIt(t *testing.T) {
	rig := newReplayRig(t)
	agent := contracts.AgentSubject(replaySession)
	var stored []uint64
	for range 5 {
		_, seq := rig.store(t, agent)
		stored = append(stored, seq)
	}
	if err := rig.js.PurgeStream(bus.Stream, &nats.StreamPurgeRequest{Sequence: stored[3]}); err != nil {
		t.Fatalf("purge the stream below %d: %v", stored[3], err)
	}

	response := replayOK(t, rig.mux, replayBody(t, []replaySubjectRequest{{Subject: agent, After: new(stored[0])}}, 0))

	if response.Stream.FirstSeq != stored[3] {
		t.Fatalf("first_seq = %d, want %d", response.Stream.FirstSeq, stored[3])
	}
	if result := response.Subjects[0]; !equalSeqs(result.Messages, stored[3], stored[4]) || result.NextAfter != stored[4] {
		t.Fatalf("result = %+v, want seqs %d and %d", result, stored[3], stored[4])
	}
}

// grantedReplayRig is a listener whose NATS user may make every call the replay route needs but
// the stored-message read; the test grants that read later.
func grantedReplayRig(t *testing.T) (*testnats.NkeyGrant, *replayRig) {
	t.Helper()
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, replayGrantWithoutRead()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{grant.URL}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	deps := &listenerDeps{client: client}
	return grant, &replayRig{deps: deps, mux: replayMux(deps), js: client.JS()}
}

func replayGrantWithoutRead() []string {
	return []string{
		"$JS.API.STREAM.INFO." + bus.Stream,
		"$JS.API.STREAM.CREATE." + bus.Stream,
		"$JS.API.STREAM.DELETE." + bus.Stream,
		"notifications.>",
	}
}

func replayUnavailableAnswer(t *testing.T, recorder *httptest.ResponseRecorder) apiError {
	t.Helper()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	var answer apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode 503 answer %s: %v", recorder.Body.String(), err)
	}
	if answer.Code != "REPLAY_UNAVAILABLE" {
		t.Fatalf("503 answer = %+v, want code REPLAY_UNAVAILABLE", answer)
	}
	return answer
}

// A listener whose NATS user lacks $JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS answers 503
// REPLAY_UNAVAILABLE naming the server's permissions violation, at once rather than after its read
// budget; the same listener answers once the grant includes the read.
func TestReplayAnswersReplayUnavailableWhenNATSDeniesTheRead(t *testing.T) {
	grant, rig := grantedReplayRig(t)
	agent := contracts.AgentSubject(replaySession)
	body, seq := rig.store(t, agent)
	request := replayBody(t, []replaySubjectRequest{{Subject: agent, After: new(uint64(0))}}, 0)

	started := time.Now()
	answer := replayUnavailableAnswer(t, postReplay(t, rig.mux, request))
	if !strings.Contains(answer.Error, "Permissions Violation") || !strings.Contains(answer.Error, "$JS.API.STREAM.MSG.GET."+bus.Stream) {
		t.Fatalf("503 error = %q, want the server's permissions violation for the read", answer.Error)
	}
	if elapsed := time.Since(started); elapsed >= replayReadBudget {
		t.Fatalf("the denied read took %s, the whole read budget", elapsed)
	}

	grant.Grant(t, append(replayGrantWithoutRead(), "$JS.API.STREAM.MSG.GET."+bus.Stream)...)
	response := replayOK(t, rig.mux, request)
	if result := response.Subjects[0]; !equalSeqs(result.Messages, seq) || result.Messages[0].Data != body {
		t.Fatalf("granted replay = %+v, want seq %d with its body", result, seq)
	}
}

// With no notification stream, the route answers 503 REPLAY_UNAVAILABLE naming NATS's error.
func TestReplayAnswersReplayUnavailableWithoutTheStream(t *testing.T) {
	rig := newReplayRig(t)
	if err := rig.js.DeleteStream(bus.Stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	answer := replayUnavailableAnswer(t, postReplay(t, rig.mux,
		replayBody(t, []replaySubjectRequest{{Subject: contracts.AgentSubject(replaySession)}}, 0)))
	if !strings.Contains(answer.Error, "stream not found") {
		t.Fatalf("503 error = %q, want NATS's stream not found", answer.Error)
	}
}
