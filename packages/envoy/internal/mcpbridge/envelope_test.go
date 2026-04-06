package mcpbridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadTestConfig(t *testing.T) *ServerConfig {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "whatsapp",
			"transport": "stdio",
			"command": ["echo"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.{jid}.message",
			"uri_pattern": "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)"
		}]
	}`), 0644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return &cfg.Servers[0]
}

func TestBuildEnvelope_WithContent(t *testing.T) {
	cfg := loadTestConfig(t)
	uri := "whatsapp://messages/15551234567/5551234567@s.whatsapp.net"
	contents := []resourceContent{
		{URI: uri, Text: "Hello from WhatsApp"},
	}

	env, err := BuildEnvelope(cfg, uri, contents)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}

	if env.Source != "whatsapp" {
		t.Fatalf("source = %s, want whatsapp", env.Source)
	}
	if env.Topic != "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message" {
		t.Fatalf("topic = %s", env.Topic)
	}
	if env.SourceEventID != uri {
		t.Fatalf("source_event_id = %s", env.SourceEventID)
	}
	if env.PayloadRef != uri {
		t.Fatalf("payload_ref = %s", env.PayloadRef)
	}
	if env.PayloadSummary != "Hello from WhatsApp" {
		t.Fatalf("payload_summary = %s", env.PayloadSummary)
	}
	if env.EventID == "" {
		t.Fatal("event_id is empty")
	}
	if env.TraceID == "" {
		t.Fatal("trace_id is empty")
	}
	if env.IssuedAt == 0 {
		t.Fatal("issued_at is zero")
	}
	if env.DedupeKey == "" {
		t.Fatal("dedupe_key is empty")
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("envelope validation: %v", err)
	}
}

func TestBuildEnvelope_NoContent(t *testing.T) {
	cfg := loadTestConfig(t)
	uri := "whatsapp://messages/15551234567/5551234567@s.whatsapp.net"

	env, err := BuildEnvelope(cfg, uri, nil)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}

	if !strings.Contains(env.PayloadSummary, "whatsapp") {
		t.Fatalf("expected fallback summary to contain source, got: %s", env.PayloadSummary)
	}
}

func TestBuildEnvelope_URIMismatch(t *testing.T) {
	cfg := loadTestConfig(t)
	_, err := BuildEnvelope(cfg, "something://else", nil)
	if err == nil {
		t.Fatal("expected error for URI mismatch")
	}
}

func TestBuildEnvelope_SummaryTruncation(t *testing.T) {
	cfg := loadTestConfig(t)
	uri := "whatsapp://messages/15551234567/5551234567@s.whatsapp.net"
	longText := strings.Repeat("a", 500)
	contents := []resourceContent{
		{URI: uri, Text: longText},
	}

	env, err := BuildEnvelope(cfg, uri, contents)
	if err != nil {
		t.Fatalf("BuildEnvelope: %v", err)
	}

	if len([]rune(env.PayloadSummary)) != 200 {
		t.Fatalf("expected summary truncated to 200 chars, got %d", len([]rune(env.PayloadSummary)))
	}
}

func TestBuildEnvelope_UniqueIDs(t *testing.T) {
	cfg := loadTestConfig(t)
	uri := "whatsapp://messages/15551234567/5551234567@s.whatsapp.net"

	env1, _ := BuildEnvelope(cfg, uri, nil)
	env2, _ := BuildEnvelope(cfg, uri, nil)

	if env1.EventID == env2.EventID {
		t.Fatal("event_id should be unique")
	}
	if env1.TraceID == env2.TraceID {
		t.Fatal("trace_id should be unique")
	}
}
