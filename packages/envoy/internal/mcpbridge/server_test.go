package mcpbridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func buildMockBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	srcPath := writeMockServer(dir)
	binPath := filepath.Join(dir, "mock_mcp_server")
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to build mock server: %v", err)
	}
	return binPath
}

func TestManagedServer_StartAndStop(t *testing.T) {
	bin := buildMockBinary(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(mockServerConfig(bin)), 0644)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	var notifiedURI string
	s := NewManagedServer(cfg.Servers[0], func(uri string) {
		notifiedURI = uri
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	if s.State() != StateReady {
		t.Fatalf("expected ready state, got %d", s.State())
	}

	// The mock server sends a notification after subscribe.
	// Give it a moment to arrive.
	time.Sleep(500 * time.Millisecond)

	if notifiedURI == "" {
		t.Fatal("expected notification URI to be set")
	}
	if notifiedURI != "whatsapp://messages/15551234567/5551234567@s.whatsapp.net" {
		t.Fatalf("unexpected notified URI: %s", notifiedURI)
	}
}

func TestManagedServer_ReadResource(t *testing.T) {
	bin := buildMockBinary(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(mockServerConfig(bin)), 0644)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	s := NewManagedServer(cfg.Servers[0], nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	contents, err := s.ReadResource("whatsapp://messages/15551234567/5551234567@s.whatsapp.net")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(contents))
	}
	if contents[0].Text != "Hello from mock" {
		t.Fatalf("unexpected text: %s", contents[0].Text)
	}
}

func TestManagedServer_StopSetsStateDead(t *testing.T) {
	bin := buildMockBinary(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(mockServerConfig(bin)), 0644)

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	s := NewManagedServer(cfg.Servers[0], nil)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	s.Stop()

	if s.State() != StateDead {
		t.Fatalf("expected dead state after stop, got %d", s.State())
	}
}
