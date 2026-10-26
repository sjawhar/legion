package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A stored-message read returns the first message at or after its sequence on a subject its filter
// matches, wildcards included, with the concrete subject, body and stored time; past the last one
// it is nats.ErrMsgNotFound, and with no stream nats.ErrStreamNotFound.
func TestNextStoredMessageReadsByFilterAfterASequence(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js := client.JS()
	publish := func(subject, body string) uint64 {
		t.Helper()
		ack, err := js.Publish(subject, []byte(body))
		if err != nil {
			t.Fatalf("publish on %s: %v", subject, err)
		}
		return ack.Sequence
	}
	publish("notifications.dispatch.issue.KEY-1.ask.opened", `{"n":1}`)
	publish("notifications.agent.ses_a", `{"n":2}`)
	third := publish("notifications.dispatch.issue.KEY-1.ask.answered", `{"n":3}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stored, err := client.NextStoredMessage(ctx, "notifications.dispatch.issue.KEY-1.>", 2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if stored.Sequence != third || stored.Subject != "notifications.dispatch.issue.KEY-1.ask.answered" || string(stored.Data) != `{"n":3}` {
		t.Fatalf("stored = %+v, want seq %d on the answered subject with its body", stored, third)
	}
	if time.Since(stored.Time) > time.Minute || stored.Time.After(time.Now()) {
		t.Fatalf("stored time %s is not when it was stored", stored.Time)
	}

	if _, err := client.NextStoredMessage(ctx, "notifications.dispatch.issue.KEY-1.>", third+1); !errors.Is(err, nats.ErrMsgNotFound) {
		t.Fatalf("read past the last message = %v, want nats.ErrMsgNotFound", err)
	}

	if err := js.DeleteStream(Stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	if _, err := client.NextStoredMessage(ctx, "notifications.agent.ses_a", 1); !errors.Is(err, nats.ErrStreamNotFound) {
		t.Fatalf("read without the stream = %v, want nats.ErrStreamNotFound", err)
	}
}

func TestSubjectContainsFollowsNATSWildcards(t *testing.T) {
	for _, tc := range []struct {
		pattern, subject string
		want             bool
	}{
		{"notifications.agent.>", "notifications.agent.ses_a", true},
		{"notifications.agent.>", "notifications.agent.ses_a.more", true},
		{"notifications.agent.>", "notifications.agent.*", true},
		{"notifications.agent.>", "notifications.agent.>", true},
		{"notifications.agent.>", "notifications.agent", false},
		{"notifications.agent.>", "notifications.>", false},
		{"notifications.dispatch.>", "notifications.dispatch.issue.KEY-1.>", true},
		{"notifications.dispatch.>", "notifications.team.x.y", false},
		{"notifications.*.x", "notifications.team.x", true},
		{"notifications.*.x", "notifications.*.x", true},
		{"notifications.*.x", "notifications.>", false},
		{"notifications.*.x", "notifications.team.x.y", false},
		{"notifications.team", "notifications.*", false},
		{"notifications.team", "notifications.team", true},
		{"notifications.team.x", "notifications.team", false},
		{">", "anything.at.all", true},
	} {
		if got := SubjectContains(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("SubjectContains(%q, %q) = %v, want %v", tc.pattern, tc.subject, got, tc.want)
		}
	}
}

func TestCheckFilterSubjectRefusesWhatNATSCannotFilterOn(t *testing.T) {
	for subject, refused := range map[string]bool{
		"notifications.dispatch.issue.KEY-1.>": false,
		"notifications.*.ses_a":                false,
		"":                                     true,
		"notifications..agent":                 true,
		"notifications.agent.":                 true,
		"notifications.>.agent":                true,
		"notifications.agent.a b":              true,
	} {
		err := CheckFilterSubject(subject)
		if refused != (err != nil) {
			t.Errorf("CheckFilterSubject(%q) = %v, want refused %v", subject, err, refused)
		}
		if err != nil && !errors.Is(err, ErrInvalidSubject) {
			t.Errorf("CheckFilterSubject(%q) = %v, want ErrInvalidSubject", subject, err)
		}
	}
}
