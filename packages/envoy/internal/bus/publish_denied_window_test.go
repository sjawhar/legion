package bus_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The two stream subjects below are the shape a notifying event has: an issue topic and an agent
// session's copy, each a subject streamSubjects holds, published together on one window.
const (
	deniedIssueTopic   = "notifications.dispatch.issue.ACME-7.message.created"
	deniedSessionTopic = "notifications.agent.session-7.mention.created"
)

func streamEnvelope(topic string) contracts.Envelope {
	return contracts.Envelope{
		EventID:       "evt-" + topic,
		Source:        "dispatch",
		SourceEventID: "source-" + topic,
		Topic:         topic,
		DedupeKey:     "dedupe-" + topic,
		IssuedAt:      contracts.NowMillis(),
	}
}

// windowAnswer is the outcome of a window's send, or a failure of the test when the send is never
// answered. A denied JetStream send waits out the acknowledgement timeout, so the wait is well past
// it.
func windowAnswer(t *testing.T, topic string, outcome <-chan error) error {
	t.Helper()
	select {
	case err := <-outcome:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("the send to %s was never answered", topic)
		return nil
	}
}

// Every denied JetStream send of one publish window is answered the denial of the subject it sent.
// A window sends every destination of one event on one connection, so a grant that withholds those
// subjects has the server report a violation of each; nats.go keeps one last error per connection,
// which holds only the violation that landed last, so reading it named one send denied and left
// every other to the acknowledgement timeout, answered as an unreachable NATS. The client records
// each violation under its own subject instead (recordPublishDenial), and each send reads the one
// recorded for its subject since it was sent (deniedPublish). Both sends go out before either
// answer is read, which is how the outbox publishes an event's destinations. Under a grant that
// covers those same subjects, both sends are accepted.
func TestAWindowAnswersEachDeniedJetStreamSendItsOwnDenial(t *testing.T) {
	window := jetStreamGrantedClient(t).Window()
	issue := window.PublishAsync(streamEnvelope(deniedIssueTopic))
	session := window.PublishAsync(streamEnvelope(deniedSessionTopic))

	for _, send := range []struct {
		topic   string
		other   string
		outcome <-chan error
	}{
		{topic: deniedIssueTopic, other: deniedSessionTopic, outcome: issue},
		{topic: deniedSessionTopic, other: deniedIssueTopic, outcome: session},
	} {
		err := windowAnswer(t, send.topic, send.outcome)
		if !errors.Is(err, bus.ErrPublishDenied) || !errors.Is(err, natsgo.ErrPermissionViolation) {
			t.Fatalf("the denied send to %s was answered %v, want ErrPublishDenied wrapping the permissions violation",
				send.topic, err)
		}
		if errors.Is(err, bus.ErrUnavailable) {
			t.Fatalf("the denied send to %s was answered %v, which reads as an unreachable NATS", send.topic, err)
		}
		if !strings.Contains(err.Error(), `Permissions Violation for Publish to "`+send.topic+`"`) {
			t.Fatalf("the denied send to %s was answered %q, which does not carry the server's violation of that subject",
				send.topic, err)
		}
		if strings.Contains(err.Error(), send.other) {
			t.Fatalf("the denied send to %s was answered %q, which names the other send's subject", send.topic, err)
		}
	}

	granted := jetStreamGrantedClient(t, "notifications.dispatch.>", "notifications.agent.>").Window()
	issue = granted.PublishAsync(streamEnvelope(deniedIssueTopic))
	session = granted.PublishAsync(streamEnvelope(deniedSessionTopic))
	for _, send := range []struct {
		topic   string
		outcome <-chan error
	}{
		{topic: deniedIssueTopic, outcome: issue},
		{topic: deniedSessionTopic, outcome: session},
	} {
		if err := windowAnswer(t, send.topic, send.outcome); err != nil {
			t.Fatalf("the granted send to %s was answered %v, want accepted", send.topic, err)
		}
	}
}

// batchEnvelope is one event of a batch the outbox's scan sends through one window: its own event
// and its own dedupe key, under the topic it shares with the batch's other events, as two comments
// a person writes a moment apart on one issue share their issue topic.
func batchEnvelope(topic, event string) contracts.Envelope {
	item := streamEnvelope(topic)
	item.EventID = "evt-" + event
	item.SourceEventID = "source-" + event
	item.DedupeKey = "dedupe-" + event
	return item
}

// Each denied JetStream send of one subject a publish window sent twice is answered a denial of
// its own message. nats-server refuses every message and reports a violation of each, so a record
// holding one violation per subject answered one of the two sends and left the other to the
// acknowledgement timeout, which reads as an unreachable NATS; the client queues the violations of
// a subject in arrival order (recordPublishDenial) and each send takes the oldest recorded since
// it was sent (deniedPublish). Both messages go out before either answer is read, which is how the
// outbox sends a scan's batch. Under a grant that covers the subject, both are accepted and the
// stream holds both.
func TestAWindowAnswersEachDeniedSendOfOneSubjectItsOwnDenial(t *testing.T) {
	window := jetStreamGrantedClient(t).Window()
	first := window.PublishAsync(batchEnvelope(deniedIssueTopic, "comment-1"))
	second := window.PublishAsync(batchEnvelope(deniedIssueTopic, "comment-2"))

	for _, send := range []struct {
		event   string
		outcome <-chan error
	}{
		{event: "comment-1", outcome: first},
		{event: "comment-2", outcome: second},
	} {
		err := windowAnswer(t, deniedIssueTopic, send.outcome)
		if !errors.Is(err, bus.ErrPublishDenied) || !errors.Is(err, natsgo.ErrPermissionViolation) {
			t.Fatalf("the denied send of %s was answered %v, want ErrPublishDenied wrapping the permissions violation",
				send.event, err)
		}
		if errors.Is(err, bus.ErrUnavailable) {
			t.Fatalf("the denied send of %s was answered %v, which reads as an unreachable NATS", send.event, err)
		}
		if !strings.Contains(err.Error(), `Permissions Violation for Publish to "`+deniedIssueTopic+`"`) {
			t.Fatalf("the denied send of %s was answered %q, which does not carry the server's violation of that subject",
				send.event, err)
		}
	}

	client := jetStreamGrantedClient(t, "notifications.dispatch.>")
	granted := client.Window()
	first = granted.PublishAsync(batchEnvelope(deniedIssueTopic, "comment-1"))
	second = granted.PublishAsync(batchEnvelope(deniedIssueTopic, "comment-2"))
	for _, send := range []struct {
		event   string
		outcome <-chan error
	}{
		{event: "comment-1", outcome: first},
		{event: "comment-2", outcome: second},
	} {
		if err := windowAnswer(t, deniedIssueTopic, send.outcome); err != nil {
			t.Fatalf("the granted send of %s was answered %v, want accepted", send.event, err)
		}
	}
	info, err := client.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.State.Msgs != 2 {
		t.Fatalf("the stream holds %d messages of the granted sends, want both", info.State.Msgs)
	}
}
