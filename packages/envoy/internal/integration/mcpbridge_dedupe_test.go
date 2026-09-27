package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/mcpbridge"
)

// The MCP bridge is a producer too, and its dedupe key is a content hash rather than an upstream
// delivery id: `<source>.` plus sha256 of the resource URI and the summary, where the summary is a
// constant sentence whenever the resource read returns no text (`internal/mcpbridge/envelope.go`).
// Its source is whatever the operator's config names, and `github` is a legal value. Two distinct
// bridge events on one URI therefore carry one dedupe key, and both must still be published: the
// bridge is reporting that an event happened, not redelivering the one before it. Keying the
// publish on the source alone drops the second.
func TestMCPBridge_TwoEventsSharingADedupeKeyBothLand(t *testing.T) {
	env := setupTestEnv(t)
	path := filepath.Join(t.TempDir(), "mcp.json")
	config, err := json.Marshal(map[string]any{"servers": []map[string]any{{
		"name":           "alerts",
		"command":        []string{"/bin/true"},
		"source":         "github",
		"topic_template": "notifications.github.acme.bridge.{kind}",
		"uri_pattern":    `^mcp://acme/(?P<kind>[a-z]+)$`,
	}}})
	if err != nil {
		t.Fatalf("encode the bridge config: %v", err)
	}
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatalf("write the bridge config: %v", err)
	}
	loaded, err := mcpbridge.LoadConfig(path)
	if err != nil {
		t.Fatalf("load the bridge config: %v", err)
	}
	server := &loaded.Servers[0]

	const uri = "mcp://acme/alerts"
	envelopes := make([]contracts.Envelope, 2)
	for i := range envelopes {
		envelope, err := mcpbridge.BuildEnvelope(server, uri, nil)
		if err != nil {
			t.Fatalf("build envelope %d: %v", i+1, err)
		}
		envelopes[i] = envelope
	}
	if envelopes[0].DedupeKey != envelopes[1].DedupeKey {
		t.Fatalf("the two events carry different dedupe keys (%s, %s); this reproduction needs the shared key the bridge mints",
			envelopes[0].DedupeKey, envelopes[1].DedupeKey)
	}
	if envelopes[0].EventID == envelopes[1].EventID {
		t.Fatalf("both events carry event id %s; they are meant to be distinct events", envelopes[0].EventID)
	}

	for i, envelope := range envelopes {
		duplicate, err := env.client.PublishReportingDuplicate(envelope)
		if err != nil {
			t.Fatalf("publish event %d: %v", i+1, err)
		}
		if duplicate {
			t.Fatalf("bridge event %d was dropped as a duplicate of event %d; its key names no upstream delivery", i+1, i)
		}
	}
	got := streamDedupeKeys(t, env, envelopes[0].Topic)
	if want := []string{envelopes[0].DedupeKey, envelopes[1].DedupeKey}; !slices.Equal(got, want) {
		t.Fatalf("stream holds %d messages on %s, want both events", len(got), envelopes[0].Topic)
	}
}
