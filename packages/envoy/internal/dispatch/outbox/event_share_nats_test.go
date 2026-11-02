package outbox

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// What one event carries of an item is bounded in UTF-16 units and sent in bytes, so every
// bound here is proved against the server's own maximum payload rather than counted on paper.
// These tests run a real NATS server.

// widestText is text of exactly units UTF-16 units at its worst case in bytes: a character
// outside ASCII but inside the Basic Multilingual Plane is one unit and three bytes, so a bound
// in units buys three times as many bytes as units, more than any other shape of text can. The
// markers at its ends make a truncation at either end visible in a failure.
func widestText(t *testing.T, units int) string {
	t.Helper()
	// ASCII, so each marker's bytes are its UTF-16 units.
	const head, tail = "HEAD:", ":TAIL"
	text := head + strings.Repeat("あ", units-len(head)-len(tail)) + tail
	if got := len(utf16.Encode([]rune(text))); got != units {
		t.Fatalf("the text is %d UTF-16 units, want %d", got, units)
	}
	return text
}

// capBody is the longest body an event carries whole, at its worst case in bytes.
func capBody(t *testing.T) string {
	t.Helper()
	return widestText(t, contracts.DispatchEventBodyMax)
}

// eventPayload is the payload of the one event that arrived on topic, decoded out of its
// envelope. It fails the test when nothing arrives within ten seconds.
func eventPayload(t *testing.T, received <-chan *natsgo.Msg, topic string) (contracts.Envelope, map[string]any) {
	t.Helper()
	select {
	case message := <-received:
		var envelope contracts.Envelope
		if err := json.Unmarshal(message.Data, &envelope); err != nil {
			t.Fatalf("decode the envelope on %s: %v", topic, err)
		}
		var delivered struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal([]byte(envelope.Payload), &delivered); err != nil {
			t.Fatalf("decode the event the envelope on %s carries: %v", topic, err)
		}
		return envelope, delivered.Payload
	case <-time.After(10 * time.Second):
		t.Fatalf("nothing arrived on %s", topic)
		return contracts.Envelope{}, nil
	}
}

// A comment body of exactly the share an event carries reaches a subscriber through a real NATS
// server, byte for byte. Dispatch stores a body the API accepted whether or not the bus can
// carry it, and the outbox marks an event published once NATS took its envelope: a body too
// large for the server's maximum payload would be recorded as delivered and reach no agent
// session at all, silently. The share is counted in UTF-16 units and the payload in bytes, so
// this holds the widest body the share allows (capBody) against the server's 1 MiB default.
func TestRunDeliversACommentBodyAtTheEventShareThroughNATS(t *testing.T) {
	database := storetest.Open(t)
	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", "_INBOX.>", "$JS.API.>")
	issueTopic := "notifications.dispatch.issue.T-1.comment.created"
	received := subscribe(t, subscriber, issueTopic)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	body := capBody(t)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: body,
		}},
	})
	stop := run(t, database, client, broker)
	defer stop()

	waitFor(t, 10*time.Second, "the event marked published", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	_, payload := eventPayload(t, received, issueTopic)
	t.Logf("a body of %d UTF-16 units arrived against a maximum payload of %d",
		contracts.DispatchEventBodyMax, subscriber.MaxPayload())
	if payload["body"] != body {
		t.Fatalf("the delivered body is %d bytes, want the %d bytes published unchanged",
			len(payload["body"].(string)), len(body))
	}
	if _, cut := payload["body_truncated"]; cut {
		t.Fatalf("a body at the event share says it was cut: %v", payload["body_truncated"])
	}
}

// The largest event Dispatch publishes is an ask.answered: it carries the ask's whole wording,
// the labels the human chose and the answer's text at once, each bounded in UTF-16 units
// (asks.go's validateAskWording, ask_transitions.go's text). At their worst case in bytes they
// still reach a subscriber whole.
func TestRunDeliversTheWidestAnsweredAskThroughNATS(t *testing.T) {
	database := storetest.Open(t)
	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", "_INBOX.>", "$JS.API.>")
	issueTopic := "notifications.dispatch.issue.T-1.ask.answered"
	received := subscribe(t, subscriber, issueTopic)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	// The question and the option labels share one bound, and the answer's text has its own.
	label := widestText(t, 1_000)
	question := widestText(t, contracts.DispatchEventBodyMax-1_000)
	answer := capBody(t)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "ask.answered", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Ask{
			ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"),
			Question: question, State: "answered",
			Options: []model.AskOption{{Label: label}},
			Answer:  &model.AskAnswer{User: "alice", Selected: []string{label}, Text: &answer},
		},
	})
	stop := run(t, database, client, broker)
	defer stop()

	waitFor(t, 10*time.Second, "the event marked published", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	_, payload := eventPayload(t, received, issueTopic)
	t.Logf("an answered ask of %d question units, a %d-unit label and a %d-unit answer arrived against a maximum payload of %d",
		contracts.DispatchEventBodyMax-1_000, 1_000, contracts.DispatchEventBodyMax, subscriber.MaxPayload())
	if payload["question"] != question {
		t.Fatalf("the delivered question is %d bytes, want the %d bytes published unchanged",
			len(payload["question"].(string)), len(question))
	}
	delivered, _ := payload["answer"].(map[string]any)
	if delivered["text"] != answer {
		t.Fatalf("the delivered answer text is not the %d bytes published", len(answer))
	}
	if chosen, _ := delivered["selected"].([]any); len(chosen) != 1 || chosen[0] != label {
		t.Fatalf("the delivered answer chose %v, want the one %d-byte label published", chosen, len(label))
	}
}

// An event NATS refuses - here a comment whose body is 1,200,000 characters, past the server's
// maximum payload - used to be logged, counted and marked published, so it reached nobody. Now
// a stand-in goes out in its place: the same topic, the same event id and the same
// payload_summary, with the payload's short top-level fields kept, its long one gone, and
// payload_omitted saying so. A reader then knows which comment to read.
func TestRunPublishesAStandInForAnEventNATSRefuses(t *testing.T) {
	database := storetest.Open(t)
	client, subscriber, _ := grantedNATS(t, "notifications.dispatch.>", "_INBOX.>", "$JS.API.>")
	issueTopic := "notifications.dispatch.issue.T-1.comment.created"
	received := subscribe(t, subscriber, issueTopic)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	const commentID = "5a660655-04ad-4ce0-8a9b-93dd03c412b7"
	body := strings.Repeat("a", 1_200_000)
	if int64(len(body)) <= subscriber.MaxPayload() {
		t.Fatalf("a body of %d bytes fits the server's maximum payload of %d, so it is not refused",
			len(body), subscriber.MaxPayload())
	}
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: commentID, IssueKey: new("T-1"), Body: body,
			Author: model.Actor{Kind: "user", ID: "alice"},
		}},
	})
	stop := run(t, database, client, broker)
	defer stop()

	waitFor(t, 10*time.Second, "the event marked published", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	envelope, payload := eventPayload(t, received, issueTopic)
	if envelope.Topic != issueTopic {
		t.Fatalf("the stand-in arrived on %s, want %s", envelope.Topic, issueTopic)
	}
	if want := "dispatch-" + strconv.FormatInt(event.ID, 10); envelope.EventID != want {
		t.Fatalf("the stand-in's event id is %q, want %q", envelope.EventID, want)
	}
	want := payloadSummary(model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Payload: map[string]any{"body": body},
	}, "")
	if envelope.PayloadSummary != want {
		t.Fatalf("the stand-in's payload_summary is %q, want the refused event's %q", envelope.PayloadSummary, want)
	}
	if payload["payload_omitted"] != true {
		t.Fatalf("the stand-in does not say its payload was left out: %v", payload)
	}
	if payload["id"] != commentID {
		t.Fatalf("the stand-in dropped the comment's id, so no reader can read the comment: %v", payload)
	}
	if _, kept := payload["body"]; kept {
		t.Fatalf("the stand-in kept the body NATS refused, so it is refused too")
	}
	if payload["issue_key"] != "T-1" {
		t.Fatalf("the stand-in dropped the issue key, a short field: %v", payload)
	}
}
