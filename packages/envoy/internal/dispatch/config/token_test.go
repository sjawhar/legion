package config

import (
	"path/filepath"
	"testing"
)

func TestLoadAcceptsDispatchToken(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"token": "agent-token"}
	}`)
	cfg, err := Load(LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.Token != "agent-token" {
		t.Errorf("dispatch token: got %+v", cfg.Dispatch)
	}
}

func TestLoadMergesDispatchTokenFromRepoConfig(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, filepath.Join(home, ".config", "opencode", "envoy.json"), `{
		"dispatch": {"token": "user-token"}
	}`)
	mustWrite(t, filepath.Join(cwd, ".opencode", "envoy.json"), `{
		"dispatch": {"token": "repo-token"}
	}`)
	cfg, err := Load(LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.Token != "repo-token" {
		t.Errorf("dispatch token: got %+v", cfg.Dispatch)
	}
}
