package bus

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// NATS takes a message only whole, and two limits decide what whole can be: nats.go refuses a
// message past the server's max payload before sending it, and the server closes the connection
// over a protocol line past its max control line (4 KiB by default), which a long enough subject
// makes. Both are ErrTooLarge, which names the size. A subject NATS does not accept, one holding
// whitespace or an empty token (which no stream's subjects match, so a JetStream publish waits out
// its deadline for an answer that never comes), is ErrInvalidSubject. Each is an ErrRefused, which
// a caller answers as a refusal rather than a failure to retry, and none may close the connection
// every subscription and watcher of the client runs on.
func TestPublishRefusesAnEnvelopeNATSCannotTakeWhole(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	envelope := func(topic, payload string) contracts.Envelope {
		return contracts.Envelope{
			EventID:        "event",
			Source:         "github",
			SourceEventID:  "delivery",
			Topic:          topic,
			DedupeKey:      "github.delivery",
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "push to a branch",
			Payload:        payload,
			TraceID:        "trace",
		}
	}
	push := "notifications.github.acme.widgets.push.branch."
	for _, tc := range []struct {
		name     string
		envelope contracts.Envelope
		want     error
		says     string
	}{
		{"a subject past the server's protocol line", envelope(push+strings.Repeat("r", 5000), "{}"), ErrTooLarge, "a subject of 5046 bytes"},
		{"a payload past the server's max payload", envelope(push+"main", strings.Repeat("p", 1<<20)), ErrTooLarge, "max payload of 1048576 bytes"},
		{"a core subject past the server's protocol line", envelope(contracts.RoleTopicPrefix+strings.Repeat("r", 5000), "{}"), ErrTooLarge, "a subject of 5019 bytes"},
		{"a subject holding a space", envelope(push+"ci yml", "{}"), ErrInvalidSubject, `"notifications.github.acme.widgets.push.branch.ci yml"`},
		{"a subject holding an empty token", envelope(contracts.AgentSubject("sess..x"), "{}"), ErrInvalidSubject, `"notifications.agent.sess..x"`},
		{"a subject ending in an empty token", envelope(push+"main.", "{}"), ErrInvalidSubject, `"notifications.github.acme.widgets.push.branch.main."`},
		{"a core subject holding an empty token", envelope(contracts.RoleTopicPrefix+"a..b", "{}"), ErrInvalidSubject, `"notifications.role.a..b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := client.Publish(tc.envelope)
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrRefused) {
				t.Fatalf("publish error = %v, want %v, an ErrRefused", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("publish error = %q, want it to say %q", err, tc.says)
			}
			if !client.Conn.IsConnected() {
				t.Fatalf("the refusal left the connection %v, want it connected", client.Conn.Status())
			}
			// The control: the same connection still publishes an envelope that fits.
			if err := client.Publish(envelope(push+"main", "{}")); err != nil {
				t.Fatalf("publish after the refusal: %v", err)
			}
		})
	}
}
