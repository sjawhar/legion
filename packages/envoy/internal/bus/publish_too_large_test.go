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
// makes. Both are one refusal, ErrTooLarge, which a caller answers as a refusal rather than a
// failure to retry, and neither may close the connection every subscription and watcher of the
// client runs on.
func TestPublishRefusesAnEnvelopeNATSCannotTakeWhole(t *testing.T) {
	client, err := Connect([]string{testnats.URL(t)})
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
	}{
		{"a subject past the server's protocol line", envelope(push+strings.Repeat("r", 5000), "{}")},
		{"a payload past the server's max payload", envelope(push+"main", strings.Repeat("p", 1<<20))},
		{"a core subject past the server's protocol line", envelope(contracts.RoleTopicPrefix+strings.Repeat("r", 5000), "{}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := client.Publish(tc.envelope)
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("publish error = %v, want ErrTooLarge", err)
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
