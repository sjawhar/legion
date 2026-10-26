package intake

import (
	"context"
	"encoding/json"
	"errors"
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
