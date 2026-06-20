package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMergesUserAndRepo(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"natsUrls": ["nats://user:4222"],
		"dispatch": {"enabled": true, "defaultRepo": "user/repo", "appClientId": "user-id"}
	}`)
	mustWrite(t, filepath.Join(cwd, ".opencode", "envoy.json"), `{
		"dispatch": {"defaultRepo": "repo/override"}
	}`)
	cfg, err := Load(LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil {
		t.Fatalf("missing dispatch block")
	}
	if cfg.Dispatch.DefaultRepo != "repo/override" {
		t.Errorf("defaultRepo: got %q", cfg.Dispatch.DefaultRepo)
	}
	if cfg.Dispatch.AppClientID != "user-id" {
		t.Errorf("appClientId should fall through from user: got %q", cfg.Dispatch.AppClientID)
	}
	if !cfg.Dispatch.Enabled {
		t.Errorf("enabled should remain true")
	}
	if len(cfg.NatsURLs) != 1 || cfg.NatsURLs[0] != "nats://user:4222" {
		t.Errorf("natsUrls: %+v", cfg.NatsURLs)
	}
}

func TestLoadInvalidRepoSlugIsSkipped(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"defaultRepo": "bogus"}
	}`)
	cfg, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch != nil {
		t.Errorf("invalid config should have been skipped; got dispatch=%+v", cfg.Dispatch)
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
