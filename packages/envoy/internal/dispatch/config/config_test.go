package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMergesUserAndRepo(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"natsUrls": ["nats://user:4222"],
		"dispatch": {"enabled": true, "serverUrl": "https://user.example"}
	}`)
	mustWrite(t, filepath.Join(cwd, ".opencode", "envoy.json"), `{
		"dispatch": {"serverUrl": "https://repo.example"}
	}`)
	cfg, err := Load(LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil {
		t.Fatalf("missing dispatch block")
	}
	if cfg.Dispatch.ServerURL != "https://repo.example" {
		t.Errorf("serverUrl: got %q", cfg.Dispatch.ServerURL)
	}
	if !cfg.Dispatch.Enabled {
		t.Errorf("enabled should remain true (fell through from user config)")
	}
	if len(cfg.NatsURLs) != 1 || cfg.NatsURLs[0] != "nats://user:4222" {
		t.Errorf("natsUrls: %+v", cfg.NatsURLs)
	}
}

func TestLoadRejectsRemovedDefaultRepoKey(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"defaultRepo": "user/repo"}
	}`)
	_, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err == nil {
		t.Fatalf("expected an error for the removed dispatch.defaultRepo key")
	}
	if !strings.Contains(err.Error(), "dispatch.defaultRepo") {
		t.Errorf("error should name the key, got %q", err.Error())
	}
}

func TestLoadRejectsRemovedAppClientIDKey(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"appClientId": "user-id"}
	}`)
	_, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err == nil {
		t.Fatalf("expected an error for the removed dispatch.appClientId key")
	}
	if !strings.Contains(err.Error(), "dispatch.appClientId") {
		t.Errorf("error should name the key, got %q", err.Error())
	}
}

func TestLoadRejectsInvalidServerURL(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"serverUrl": ""}
	}`)
	_, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err == nil {
		t.Fatalf("expected an error for an invalid serverUrl")
	}
}

func TestLoadMissingFilesReturnEmpty(t *testing.T) {
	cfg, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg == nil {
		t.Fatalf("expected non-nil config")
	}
	if cfg.Dispatch != nil {
		t.Errorf("expected nil dispatch, got %+v", cfg.Dispatch)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
