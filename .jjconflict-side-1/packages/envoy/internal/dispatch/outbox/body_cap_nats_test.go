package outbox

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// capBody is a body of exactly contracts.DispatchBodyMax UTF-16 units at its worst case in bytes:
// a character outside ASCII but inside the Basic Multilingual Plane is one unit and three bytes,
// so the cap buys three times as many bytes as units, more than any other shape of text can. The
// markers at its ends make a truncation at either end visible in a failure.
func capBody(t *testing.T) string {
	t.Helper()
	// ASCII, so each marker's bytes are its UTF-16 units.
	const head, tail = "HEAD:", ":TAIL"
	body := head + strings.Repeat("あ", contracts.DispatchBodyMax-len(head)-len(tail)) + tail
	if units := len(utf16.Encode([]rune(body))); units != contracts.DispatchBodyMax {
		t.Fatalf("the body is %d UTF-16 units, want the cap of %d", units, contracts.DispatchBodyMax)
	}
	return body
}

// A comment body of exactly the cap Dispatch accepts reaches a subscriber through a real NATS
// server, byte for byte. Dispatch stores a body the API accepted whether or not the bus can carry
// it, and the outbox marks an event published once NATS took its envelope: a body too large for
// the server's maximum payload would be recorded as delivered and reach no agent session at all,
// silently. The cap is counted in UTF-16 units and the payload in bytes, so this holds the widest
// body the cap allows (capBody) against the server's 1 MiB default.
func TestRunDeliversACommentBodyAtTheCapThroughNATS(t *testing.T) {
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
	select {
	case message := <-received:
		t.Logf("a body of %d UTF-16 units arrived in an envelope of %d bytes, against a maximum payload of %d",
			contracts.DispatchBodyMax, len(message.Data), subscriber.MaxPayload())
		var envelope contracts.Envelope
		if err := json.Unmarshal(message.Data, &envelope); err != nil {
			t.Fatalf("decode the issue envelope: %v", err)
		}
		var delivered struct {
			Payload struct {
				Body string `json:"body"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(envelope.Payload), &delivered); err != nil {
			t.Fatalf("decode the event the envelope carries: %v", err)
		}
		if delivered.Payload.Body != body {
			t.Fatalf("the delivered body is %d bytes, want the %d bytes published unchanged",
				len(delivered.Payload.Body), len(body))
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("nothing arrived on %s, though the event is marked published: a body of %d UTF-16 units was recorded delivered and reached no one",
			issueTopic, contracts.DispatchBodyMax)
	}
}
