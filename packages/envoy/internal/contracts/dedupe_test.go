package contracts

import "testing"

// The same table as dedupeKeyNamesItsEvent's test in packages/contracts/src/envelope.test.ts: the
// stream's MsgId and every host's dedupe answer one question, so a key either side misjudges is a
// repeat one of them drops and the other does not, or a distinct event both lose.
func TestDedupeKeyNamesTheUpstreamEvent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		item  Envelope
		names bool
	}{
		{"a Dispatch key", Envelope{Source: "dispatch", DedupeKey: "agent.ses_a.m1:aside"}, true},
		{"a webhook key of its delivery id", Envelope{Source: "github", SourceEventID: "delivery-7", DedupeKey: "github.delivery-7"}, true},
		{"a publish key the listener minted", Envelope{Source: "agent", DedupeKey: "publish.0123456789abcdef0123456789abcdef"}, true},
		{"a send key the listener minted", Envelope{Source: "agent", DedupeKey: "agent.ses_a.0123456789abcdef0123456789abcdef"}, true},
		{"a key around the transport's idempotency key", Envelope{Source: "agent", DedupeKey: "agent.ses_a.3f2a9c1e-5b7d-4e8f-9a0b-1c2d3e4f5a6b"}, true},
		{"a minted key the role arbiter forwarded", Envelope{Source: "agent", DedupeKey: "envoy.role.forward.publish.0123456789abcdef0123456789abcdef"}, true},
		{"the MCP bridge's content hash", Envelope{Source: "github", SourceEventID: "mcp://acme/alerts", DedupeKey: "github.3f2a"}, false},
		{"the Go daemon's outbox row", Envelope{Source: "agent", DedupeKey: "legion-outbox:1"}, false},
		{"a caller's own idempotency key", Envelope{Source: "agent", DedupeKey: "agent.ses_a.retry-abc"}, false},
		{"a publish key of another shape", Envelope{Source: "agent", DedupeKey: "publish.abc"}, false},
	} {
		if got := DedupeKeyNamesTheUpstreamEvent(tc.item); got != tc.names {
			t.Errorf("%s: DedupeKeyNamesTheUpstreamEvent = %v, want %v", tc.name, got, tc.names)
		}
	}
}
