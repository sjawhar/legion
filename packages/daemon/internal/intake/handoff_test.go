package intake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// fakeDelivery is one JetStream delivery as ConsumeHandoff settles it, recording how.
type fakeDelivery struct {
	subject string
	data    []byte
	settled string
	delay   time.Duration
}

func (d *fakeDelivery) Subject() string { return d.subject }
func (d *fakeDelivery) Data() []byte    { return d.data }
func (d *fakeDelivery) Ack() error      { d.settled = "ack"; return nil }
func (d *fakeDelivery) Term() error     { d.settled = "term"; return nil }
func (d *fakeDelivery) NakWithDelay(delay time.Duration) error {
	d.settled, d.delay = "nak", delay
	return nil
}

func handoffJSON(t *testing.T, project, session string) []byte {
	t.Helper()
	data, err := json.Marshal(Handoff{
		OriginalTopic: "notifications.agent." + session, TargetSession: session, RoleToken: "legion-legion-legion-9-tester", Project: project,
		Envelope: HandoffEnvelope{EventID: "evt-1", Source: "agent", SourceEventID: "src-1", SourceSession: "ses_asker",
			Topic: "notifications.agent." + session, DedupeKey: "agent." + session + ".0123456789abcdef0123456789abcdef",
			PayloadSummary: "Which fruit?", TraceID: "trace-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A hand-off is acknowledged only once the loop has recorded it: a handler that fails has it
// delivered again after the nak delay, one that succeeds acknowledges it, and the handler is handed
// the hand-off it decoded with the bytes as published.
func TestAHandoffIsAcknowledgedOnlyOnceTheLoopRecordedIt(t *testing.T) {
	spec := HandoffSpec{Project: "legion", NakDelay: 7 * time.Second}
	data := handoffJSON(t, "legion", "ses_tester")

	failing := &fakeDelivery{subject: HandoffSubject("legion", "ses_tester"), data: data}
	ConsumeHandoff(context.Background(), failing, spec, func(context.Context, Handoff, []byte) error {
		return errors.New("the store is down")
	})
	if failing.settled != "nak" || failing.delay != 7*time.Second {
		t.Fatalf("a hand-off the loop could not record settled %q after %s, want nak after 7s", failing.settled, failing.delay)
	}

	var got Handoff
	var raw []byte
	recorded := &fakeDelivery{subject: HandoffSubject("legion", "ses_tester"), data: data}
	ConsumeHandoff(context.Background(), recorded, spec, func(_ context.Context, h Handoff, published []byte) error {
		got, raw = h, published
		return nil
	})
	if recorded.settled != "ack" {
		t.Fatalf("a recorded hand-off settled %q, want ack", recorded.settled)
	}
	if got.TargetSession != "ses_tester" || got.Envelope.SourceSession != "ses_asker" || string(raw) != string(data) {
		t.Fatalf("the handler was handed %+v (%s), want the decoded hand-off and its bytes", got, raw)
	}
}

// A hand-off that cannot be read, one for another project, and one whose session is not its
// subject's are poison: terminated, and never handed to the loop.
func TestAHandoffTheDaemonCannotTrustIsTerminated(t *testing.T) {
	spec := HandoffSpec{Project: "legion"}
	for name, delivery := range map[string]*fakeDelivery{
		"not JSON":        {subject: HandoffSubject("legion", "ses_tester"), data: []byte("{")},
		"another project": {subject: HandoffSubject("legion", "ses_tester"), data: handoffJSON(t, "other", "ses_tester")},
		"another session": {subject: HandoffSubject("legion", "ses_other"), data: handoffJSON(t, "legion", "ses_tester")},
		"no envelope id": {subject: HandoffSubject("legion", "ses_tester"),
			data: []byte(`{"original_topic":"t","target_session":"ses_tester","role_token":"r","project":"legion","envelope":{"source":"agent"}}`)},
	} {
		ConsumeHandoff(context.Background(), delivery, spec, func(context.Context, Handoff, []byte) error {
			t.Fatalf("%s: the poison hand-off reached the loop", name)
			return nil
		})
		if delivery.settled != "term" {
			t.Errorf("%s: settled %q, want term", name, delivery.settled)
		}
	}
}

// A Dispatch envelope's payload reads as the targeted send's frame, with its delivery attempt, or
// as the bare event a follower route carries.
func TestADispatchEnvelopeReadsAsItsFrameOrItsBareEvent(t *testing.T) {
	frame := HandoffEnvelope{EventID: "e1", Source: "dispatch", Payload: `{"event":{"type":"comment.created","actor":{"kind":"user","id":"p@example.com"},` +
		`"issue_key":"LEGION-9","payload":{"id":"c1","body":"@tester why?","ask_id":null}},"delivery":{"attempt":2,"mode":"btw","comment_id":"c1","target":"role:tester"}}`}
	event, err := frame.Dispatch()
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != "comment.created" || event.Delivery == nil || event.Delivery.Attempt != 2 || event.Delivery.Target != "role:tester" || event.Payload.Body != "@tester why?" {
		t.Fatalf("frame = %+v, want the comment with its attempt", event)
	}
	bare := HandoffEnvelope{EventID: "e2", Source: "dispatch", Payload: `{"type":"comment.created","actor":{"kind":"user","id":"p@example.com"},` +
		`"issue_key":"LEGION-9","payload":{"id":"c2","body":"and?","ask_id":"a1"}}`}
	event, err = bare.Dispatch()
	if err != nil {
		t.Fatal(err)
	}
	if event.Delivery != nil || event.Payload.AskID == nil || *event.Payload.AskID != "a1" {
		t.Fatalf("bare event = %+v, want the ask reply with no delivery", event)
	}
	if _, err := (HandoffEnvelope{EventID: "e3", Source: "agent", Payload: "{}"}).Dispatch(); err == nil {
		t.Fatal("an agent's envelope decoded as Dispatch's")
	}
}

// readHandoffFixture decodes the hand-off testdata/handoff/<name>.json holds and its Dispatch event.
func readHandoffFixture(t *testing.T, name string) DispatchEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "handoff", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := DecodeHandoff(data)
	if err != nil {
		t.Fatal(err)
	}
	event, err := h.Envelope.Dispatch()
	if err != nil {
		t.Fatal(err)
	}
	return event
}

// A person's answer to an ask reads with its Dispatch event id, the person, the ask's id and kind,
// and the options they selected with their text. A reply on an ask reads with its comment id, its
// ask and thread, and whether it hands the ask back to the agent: a person's reply that leaves the
// next move with the agent does, an agent's reply that hands the ask to the person does not
// (sjawhar/legion#2163's testdata shapes, as the listener hands them off).
func TestADispatchAskAnswerAndReplyReadAsTheirHandOffCarriesThem(t *testing.T) {
	const ask = "6c83cb7f-8f4e-4820-9e05-7c94fa725007"
	answered := readHandoffFixture(t, "ask-answered")
	answer := answered.Payload.Answer
	if answered.ID != 34 || answered.Type != "ask.answered" || !answered.Actor.Person() || answered.Actor.ID != "human" ||
		answered.Payload.ID != ask || answered.Payload.Kind != "question" || answer == nil || !slices.Equal(answer.Selected, []string{"B"}) ||
		answer.Text == nil || *answer.Text != "Use B. Why did you test A first?" || answered.HandsTurnBack() {
		t.Fatalf("ask.answered = %+v (answer %+v); want event 34, the person's answer B with its text, no hand-back", answered, answer)
	}
	reply := readHandoffFixture(t, "comment-ask-agent")
	if reply.ID != 32 || reply.Payload.ID != "4c4b2a5f-93df-4aeb-a2ca-b5c74187c8bf" || reply.Payload.AskID == nil || *reply.Payload.AskID != ask ||
		reply.Payload.ReplyTo != nil || reply.Payload.AskWaitingOn != "agent" || reply.Payload.Body != "Use B, and tell me why A failed." || !reply.HandsTurnBack() {
		t.Fatalf("person's reply = %+v; want the comment on the ask, handing it back to the agent", reply)
	}
	handed := readHandoffFixture(t, "comment-ask-human")
	if handed.Actor.Person() || handed.Payload.AskWaitingOn != "human" || handed.HandsTurnBack() {
		t.Fatalf("agent's reply = %+v; want the agent's, handing the ask to the person, not back to the agent", handed)
	}
	// A person's reply under a closed ask records no turn; it is the only answer they can still give
	// there, and #2163 relays it as a decision too.
	closed := reply
	closed.Payload.AskWaitingOn = ""
	if !closed.HandsTurnBack() {
		t.Fatal("a person's reply under a closed ask does not hand it back; want it to")
	}
}
