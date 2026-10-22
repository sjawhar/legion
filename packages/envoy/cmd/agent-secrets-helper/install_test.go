//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// installRig is a temp XDG config dir, a fake systemctl on PATH that appends its argv to a log,
// and the helper socket path install checks for a running helper.
type installRig struct {
	env    map[string]string
	unit   string
	calls  string
	sock   string
	exe    string
	stdout bytes.Buffer
}

func newInstallRig(t *testing.T) *installRig {
	t.Helper()
	// TMPDIR may sit behind a symlink; install writes the resolved path, so the rig's binary
	// lives at one. The socket keeps the short unresolved path (unix socket paths are capped).
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &installRig{calls: filepath.Join(dir, "systemctl.log"), sock: filepath.Join(dir, "h.sock"), exe: filepath.Join(real, "agent-secrets-helper")}
	script := "#!/bin/sh\necho \"$*\" >> " + r.calls + "\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	r.env = map[string]string{"HOME": dir, "XDG_CONFIG_HOME": filepath.Join(dir, "cfg"), "AGENT_SECRETS_HELPER_SOCK": r.sock}
	r.unit = filepath.Join(dir, "cfg", "systemd", "user", "agent-secrets-helper.service")
	return r
}

func (r *installRig) run(args ...string) error {
	r.stdout.Reset()
	return install(args, env(r.env), r.exe, &r.stdout)
}

func (r *installRig) systemctlCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(r.calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// fakeHelper answers `sessions` on the rig's socket with the given sessions.
func (r *installRig) fakeHelper(t *testing.T, sessions []helper.SessionInfo) {
	t.Helper()
	ln, err := net.Listen("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			data, _ := json.Marshal(helper.Response{OK: true, Sessions: sessions})
			_, _ = conn.Write(append(data, '\n'))
			_ = conn.Close()
		}
	}()
}

func TestInstallWritesTheUnitForThisBinary(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.unit)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	for _, want := range []string{
		"\nExecStart=" + r.exe + " serve\n",
		"\nEnvironment=AGENT_SECRETS_URL=https://secrets.example\n",
		"\nRestart=always\n", "\nRestartSec=10\n", "\nWantedBy=default.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if fi, _ := os.Stat(r.unit); fi.Mode().Perm() != 0o644 {
		t.Errorf("unit mode %v, want 0644", fi.Mode().Perm())
	}
	calls := r.systemctlCalls(t)
	if strings.Join(calls, ";") != "--user daemon-reload;--user enable --now agent-secrets-helper.service" {
		t.Fatalf("systemctl calls: %q", calls)
	}
}

func TestInstallResolvesSymlinksAndTakesTheURLFromTheEnvironment(t *testing.T) {
	r := newInstallRig(t)
	link := filepath.Join(filepath.Dir(r.exe), "link")
	if err := os.Symlink(r.exe, link); err != nil {
		t.Fatal(err)
	}
	r.env["AGENT_SECRETS_URL"] = "https://env.example"
	if err := install(nil, env(r.env), link, &r.stdout); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(r.unit)
	if !strings.Contains(string(data), "ExecStart="+r.exe+" serve\n") || !strings.Contains(string(data), "AGENT_SECRETS_URL=https://env.example\n") {
		t.Fatalf("unit:\n%s", data)
	}
}

func TestInstallIsANoOpWhenNothingChanged(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(r.unit)
	_ = os.Remove(r.calls)
	r.fakeHelper(t, nil)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(r.unit)
	if !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
		t.Error("the unchanged unit was rewritten")
	}
	if calls := r.systemctlCalls(t); calls != nil {
		t.Fatalf("systemctl ran on an unchanged unit: %q", calls)
	}
}

func TestInstallRestartsAHelperWithNoSessions(t *testing.T) {
	r := newInstallRig(t)
	r.fakeHelper(t, nil)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	calls := r.systemctlCalls(t)
	if len(calls) != 3 || calls[2] != "--user restart agent-secrets-helper.service" {
		t.Fatalf("systemctl calls: %q", calls)
	}
}

func TestInstallNeverRestartsAHelperWithSessions(t *testing.T) {
	r := newInstallRig(t)
	r.fakeHelper(t, []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}, {PID: 42, RuntimeID: "rt-b"}})
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.systemctlCalls(t) {
		if strings.Contains(c, "restart") {
			t.Fatalf("restarted a helper with sessions: %q", c)
		}
	}
	out := r.stdout.String()
	if !strings.Contains(out, "41\trt-a") || !strings.Contains(out, "42\trt-b") ||
		!strings.Contains(out, "not restarted: 2 sessions are registered; a restart ends their grants. Re-run with --restart-with-sessions.") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestInstallRestartsWithSessionsWhenTold(t *testing.T) {
	r := newInstallRig(t)
	r.fakeHelper(t, []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}})
	if err := r.run("--url", "https://secrets.example", "--restart-with-sessions"); err != nil {
		t.Fatal(err)
	}
	calls := r.systemctlCalls(t)
	if len(calls) != 3 || calls[2] != "--user restart agent-secrets-helper.service" {
		t.Fatalf("systemctl calls: %q", calls)
	}
}

func TestInstallRefusesWithoutAURL(t *testing.T) {
	r := newInstallRig(t)
	err := r.run()
	if err == nil || !strings.Contains(err.Error(), "--url") || !strings.Contains(err.Error(), "AGENT_SECRETS_URL") {
		t.Fatalf("install without a URL: %v", err)
	}
	if _, statErr := os.Stat(r.unit); statErr == nil {
		t.Fatal("wrote a unit without a URL")
	}
	if err := r.run("--url", "https://a b"); err == nil {
		t.Fatal("accepted a URL with whitespace")
	}
}

// TestInstallRefusesAnExecutablePathSystemdWouldSplit: ExecStart splits on whitespace and expands
// specifiers, so install refuses such a path rather than escaping it.
func TestInstallRefusesAnExecutablePathSystemdWouldSplit(t *testing.T) {
	r := newInstallRig(t)
	spaced := filepath.Join(filepath.Dir(r.exe), "my helper")
	if err := os.WriteFile(spaced, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := install([]string{"--url", "https://s"}, env(r.env), spaced, &r.stdout); err == nil || !strings.Contains(err.Error(), spaced) {
		t.Fatalf("install from %q: %v", spaced, err)
	}
}
