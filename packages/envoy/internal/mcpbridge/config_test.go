package mcpbridge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "whatsapp",
			"transport": "stdio",
			"command": ["echo", "hello"],
			"resources": ["whatsapp://messages/new"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.{jid}.message",
			"uri_pattern": "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)"
		}]
	}`), 0644)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected valid config: %v", err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(cfg.Servers))
	}
	if cfg.Servers[0].Name != "whatsapp" {
		t.Fatalf("unexpected name: %s", cfg.Servers[0].Name)
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json {{{"), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadConfig_NoServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	os.WriteFile(path, []byte(`{"servers": []}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for no servers")
	}
}

func TestLoadConfig_MissingName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"transport": "stdio",
			"command": ["echo"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestLoadConfig_UnsupportedTransport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "http",
			"command": ["echo"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for unsupported transport")
	}
}

func TestLoadConfig_BadRegex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "stdio",
			"command": ["echo"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "(?P<phone>[invalid"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for bad regex")
	}
}

func TestLoadConfig_MissingCaptureGroup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "stdio",
			"command": ["echo"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.{jid}.message",
			"uri_pattern": "whatsapp://(?P<phone>[^/]+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing capture group 'jid'")
	}
}

func TestLoadConfig_NonexistentCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "test",
			"transport": "stdio",
			"command": ["definitely_not_a_real_command_xyz_999"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.message",
			"uri_pattern": "whatsapp://(?P<phone>.+)"
		}]
	}`), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for nonexistent command")
	}
}

func TestRenderTopic(t *testing.T) {
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

	cases := []struct {
		uri  string
		want string
		ok   bool
	}{
		{
			uri:  "whatsapp://messages/15551234567/5551234567@s.whatsapp.net",
			want: "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message",
			ok:   true,
		},
		{
			uri:  "whatsapp://messages/15559876543/group-abc@g.us",
			want: "notifications.whatsapp.15559876543.group-abc@g.us.message",
			ok:   true,
		},
		{
			uri: "something://else/entirely",
			ok:  false,
		},
	}

	for _, tc := range cases {
		got, err := cfg.Servers[0].RenderTopic(tc.uri)
		if tc.ok {
			if err != nil {
				t.Fatalf("RenderTopic(%s): unexpected error: %v", tc.uri, err)
			}
			if got != tc.want {
				t.Fatalf("RenderTopic(%s) = %s, want %s", tc.uri, got, tc.want)
			}
		} else {
			if err == nil {
				t.Fatalf("RenderTopic(%s): expected error, got %s", tc.uri, got)
			}
		}
	}
}

func TestExtractPlaceholders(t *testing.T) {
	cases := []struct {
		tmpl string
		want []string
	}{
		{tmpl: "notifications.whatsapp.{phone}.{jid}.message", want: []string{"phone", "jid"}},
		{tmpl: "no.placeholders.here", want: nil},
		{tmpl: "{single}", want: []string{"single"}},
	}
	for _, tc := range cases {
		got := extractPlaceholders(tc.tmpl)
		if len(got) != len(tc.want) {
			t.Fatalf("extractPlaceholders(%q) = %v, want %v", tc.tmpl, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("extractPlaceholders(%q)[%d] = %s, want %s", tc.tmpl, i, got[i], tc.want[i])
			}
		}
	}
}

func TestLoadConfig_WithEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{
		"servers": [{
			"name": "whatsapp",
			"transport": "stdio",
			"command": ["echo", "hello"],
			"env": {"WHATSAPP_DATA_DIR": "/data/whatsapp"},
			"resources": ["whatsapp://messages/new"],
			"source": "whatsapp",
			"topic_template": "notifications.whatsapp.{phone}.{jid}.message",
			"uri_pattern": "whatsapp://messages/(?P<phone>[^/]+)/(?P<jid>.+)"
		}]
	}`), 0644)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("expected valid config with env: %v", err)
	}
	if cfg.Servers[0].Env["WHATSAPP_DATA_DIR"] != "/data/whatsapp" {
		t.Fatalf("env not parsed: %v", cfg.Servers[0].Env)
	}
}
