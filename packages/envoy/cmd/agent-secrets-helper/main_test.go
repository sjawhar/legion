// packages/envoy/cmd/agent-secrets-helper/main_test.go
//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
	cfg, err = loadConfig(env(map[string]string{"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/7", "AGENT_SECRETS_URL": "https://secrets.internal.example"}))
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

// TestServeNamesTheSignalItStopsOn: a SIGTERM from anything but systemd (a test rig's
// `pkill -f "agent-secrets-helper serve"` matches the unit's helper too) stops the helper as
// systemd's own stop does, with exit 0, since a non-zero exit would leave every stop failed. The
// unit's journal then says only that it ended, so the helper's last line names the signal, the
// sessions it had and whether it held a launcher credential.
func TestServeNamesTheSignalItStopsOn(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "agent-secrets-helper")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	logged := func() string {
		b, err := os.ReadFile(stderr.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	sock := filepath.Join(dir, "run", "helper.sock")
	cmd := exec.Command(binary, "serve")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "AGENT_SECRETS_URL=http://127.0.0.1:1",
		"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_OPERATOR_FILE=" + filepath.Join(dir, "operator")}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := helper.Call(sock, helper.Request{Op: "sessions"}, time.Second); err == nil && resp.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the helper never answered on %s; stderr:\n%s", sock, logged())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		stopped = true
		if err != nil {
			t.Fatalf("serve after SIGTERM: %v; want exit 0, as a requested stop. stderr:\n%s", err, logged())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not stop on SIGTERM; stderr:\n%s", logged())
	}
	const want = `level=WARN msg="agent-secrets-helper stopping on a signal" signal=terminated sessions=0 launcher_credential=false`
	if !strings.Contains(logged(), want) {
		t.Fatalf("stderr after SIGTERM:\n%s\nwant a line carrying %s", logged(), want)
	}
}
