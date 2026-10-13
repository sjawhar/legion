package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAcceptsDispatchToken(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, userFile(home), `{
		"dispatch": {"token": "agent-token"}
	}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: t.TempDir(), HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.Token != "agent-token" {
		t.Errorf("dispatch token: got %+v", cfg.Dispatch)
	}
}

func TestLoadMergesDispatchTokenFromCheckoutConfig(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	mustWrite(t, userFile(home), `{
		"dispatch": {"token": "user-token"}
	}`)
	mustWrite(t, checkoutFile(cwd), `{
		"dispatch": {"token": "checkout-token"}
	}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.Token != "checkout-token" {
		t.Errorf("dispatch token: got %+v", cfg.Dispatch)
	}
}

// The server reads no token: a value starting with `!` is a command only the clients run, so Load
// keeps it as written and runs nothing, from the user file or the checkout's.
func TestLoadRunsNoTokenCommand(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	command := "!touch " + marker
	mustWrite(t, userFile(home), `{"envoy": {"token": "`+command+`", "natsSeed": "`+command+`"}}`)
	mustWrite(t, checkoutFile(cwd), `{"dispatch": {"token": "`+command+`"}}`)
	cfg, err := Load(environment(nil), LoadOptions{CWD: cwd, HomeDir: home})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Dispatch == nil || cfg.Dispatch.Token != command {
		t.Errorf("dispatch token: got %+v, want %q as written", cfg.Dispatch, command)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s: %v; Load ran a token command", marker, err)
	}
}
