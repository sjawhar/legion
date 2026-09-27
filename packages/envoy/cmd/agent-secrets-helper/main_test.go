// packages/envoy/cmd/agent-secrets-helper/main_test.go
//go:build linux

package main

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigDerivesDefaultsAndServeRequiresURL(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/7"}))
	if err != nil {
		t.Fatalf("sessions must work without AGENT_SECRETS_URL: %v", err)
	}
	if err := serve(cfg); err == nil || !strings.Contains(err.Error(), "AGENT_SECRETS_URL") {
		t.Fatalf("serve must refuse without AGENT_SECRETS_URL, got %v", err)
	}
	cfg, err = loadConfig(env(map[string]string{"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/7", "AGENT_SECRETS_URL": "https://secrets.internal.trajectorylabs.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != "/run/user/7/agent-secrets/helper.sock" || cfg.StatePath != "/run/user/7/agent-secrets/sessions.json" ||
		cfg.OperatorFile != "/home/u/.config/agent-secrets/operator" {
		t.Fatalf("defaults: %+v", cfg)
	}
	cfg, _ = loadConfig(env(map[string]string{"HOME": "/home/u", "AGENT_SECRETS_URL": "https://s", "AGENT_SECRETS_HELPER_SOCK": "/tmp/x/h.sock"}))
	if cfg.StatePath != "/tmp/x/sessions.json" {
		t.Fatalf("state sits beside the socket: %q", cfg.StatePath)
	}
}

// TestServeStartsWithoutCredentialAndAnswersSessions pins the brief's replacement for the old
// startup refusal: the unit always serves now, full stop — no TokenFile, and a missing
// OperatorFile does not stop it from listening and answering `sessions` either.
func TestServeStartsWithoutCredentialAndAnswersSessions(t *testing.T) {
	dir := t.TempDir()
	cfg := config{URL: "https://s", Socket: dir + "/h.sock", OperatorFile: dir + "/none", StatePath: dir + "/s.json"}
	done := make(chan error, 1)
	go func() { done <- serve(cfg) }()
	t.Cleanup(func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(cfg.Socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := helper.Call(cfg.Socket, helper.Request{Op: "sessions"}, 2*time.Second)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if !resp.OK {
		t.Fatalf("sessions must succeed even with no machine credential yet: %+v", resp)
	}
}
