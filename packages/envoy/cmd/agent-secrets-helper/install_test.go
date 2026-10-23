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

// installRig is a temp XDG config dir, a fake systemctl on PATH that appends its argv to a log
// (all but is-active, which prints the unit's state: "active" unless a test writes another to
// the active file, and logs nothing), and the helper socket path install checks for a running
// helper.
type installRig struct {
	env    map[string]string
	unit   string
	calls  string
	active string
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
	r := &installRig{calls: filepath.Join(dir, "systemctl.log"), active: filepath.Join(dir, "active"), sock: filepath.Join(dir, "h.sock"), exe: filepath.Join(real, "agent-secrets-helper")}
	if err := os.WriteFile(r.active, []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$2\" = is-active ]; then read -r state < " + r.active + "; echo \"$state\"; exit 0; fi\n" +
		"echo \"$*\" >> " + r.calls + "\n" +
		"if [ \"$2\" = restart ] && [ -e " + r.calls + ".fail-restart ]; then exit 1; fi\n"
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

// fakeHelper answers on the rig's socket with the given sessions and returns the ops it was
// asked, in order. With knowsDrain false it answers `drain` as an older helper does.
func (r *installRig) fakeHelper(t *testing.T, sessions []helper.SessionInfo, knowsDrain ...bool) *[]string {
	t.Helper()
	return r.scriptedHelper(t, func(op string) helper.Response {
		if op == "drain" && len(knowsDrain) > 0 && !knowsDrain[0] {
			return helper.Response{Code: helper.CodeBadRequest, Error: "unknown op drain"}
		}
		return helper.Response{OK: true, Sessions: sessions}
	})
}

// scriptedHelper answers each op on the rig's socket with answer(op) and returns the ops it was
// asked, in order.
func (r *installRig) scriptedHelper(t *testing.T, answer func(op string) helper.Response) *[]string {
	t.Helper()
	ln, err := net.Listen("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ops := &[]string{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadBytes('\n')
			var req helper.Request
			_ = json.Unmarshal(line, &req)
			// install reads each reply before its next call, so this append happens-before the test reads ops.
			*ops = append(*ops, req.Op)
			data, _ := json.Marshal(answer(req.Op))
			_, _ = conn.Write(append(data, '\n'))
			_ = conn.Close()
		}
	}()
	return ops
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

func TestInstallDrainsAndRestartsAHelperWithNoSessions(t *testing.T) {
	r := newInstallRig(t)
	ops := r.fakeHelper(t, nil)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	calls := r.systemctlCalls(t)
	if len(calls) != 3 || calls[2] != "--user restart agent-secrets-helper.service" {
		t.Fatalf("systemctl calls: %q", calls)
	}
	if strings.Join(*ops, ",") != "sessions,drain" {
		t.Fatalf("ops asked: %q, want the running check then drain", *ops)
	}
}

// TestInstallNeverRestartsAHelperThatCannotDrain: a helper without the drain op cannot refuse a
// register between install's check and the restart, so install lists its sessions (none here, or
// two) and says to restart at a quiet moment, never restarting it itself.
func TestInstallNeverRestartsAHelperThatCannotDrain(t *testing.T) {
	const notice = "not restarted: the running agent-secrets-helper predates drain, so a session could start between the check and the restart. Re-run with --restart-with-sessions at a quiet moment.\n"
	for _, tc := range []struct {
		name     string
		sessions []helper.SessionInfo
		want     string
	}{
		{name: "no sessions", want: notice},
		{name: "sessions", sessions: []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}, {PID: 42, RuntimeID: "rt-b"}}, want: "41\trt-a\t\t\n42\trt-b\t\t\n" + notice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newInstallRig(t)
			ops := r.fakeHelper(t, tc.sessions, false)
			if err := r.run("--url", "https://secrets.example"); err != nil {
				t.Fatal(err)
			}
			for _, c := range r.systemctlCalls(t) {
				if strings.Contains(c, "restart") {
					t.Fatalf("restarted a helper that cannot drain: %q", c)
				}
			}
			if strings.Join(*ops, ",") != "sessions,drain,sessions" {
				t.Fatalf("ops asked: %q", *ops)
			}
			if out := r.stdout.String(); out != tc.want {
				t.Fatalf("output:\n%s\nwant:\n%s", out, tc.want)
			}
		})
	}
}

// TestInstallDoesNotRestartAHelperThatWasNotRunning: enable --now starts the new unit, so no
// restart follows.
func TestInstallDoesNotRestartAHelperThatWasNotRunning(t *testing.T) {
	r := newInstallRig(t)
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.systemctlCalls(t) {
		if strings.Contains(c, "restart") {
			t.Fatalf("restarted a helper that was not running: %q", c)
		}
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

// TestInstallRefusesAHelperItsUnitDoesNotRun: a helper answers on the socket while the unit is
// not active, so something else started it; enabling the unit would start a second helper on
// its socket. install lists that helper's sessions, says to stop it, and writes and starts
// nothing.
func TestInstallRefusesAHelperItsUnitDoesNotRun(t *testing.T) {
	r := newInstallRig(t)
	if err := os.WriteFile(r.active, []byte("inactive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := r.fakeHelper(t, []helper.SessionInfo{{PID: 41, RuntimeID: "rt-a"}})
	err := r.run("--url", "https://secrets.example")
	if err == nil || !strings.Contains(err.Error(), "not run by agent-secrets-helper.service") || !strings.Contains(err.Error(), r.sock) ||
		!strings.Contains(err.Error(), "stop it, then re-run install") {
		t.Fatalf("install beside an unmanaged helper: %v", err)
	}
	if !strings.Contains(r.stdout.String(), "41\trt-a") {
		t.Fatalf("output %q; want the running helper's sessions", r.stdout.String())
	}
	if _, statErr := os.Stat(r.unit); statErr == nil {
		t.Fatal("wrote the unit beside an unmanaged helper")
	}
	if calls := r.systemctlCalls(t); calls != nil {
		t.Fatalf("systemctl ran beside an unmanaged helper: %q", calls)
	}
	if strings.Join(*ops, ",") != "sessions" {
		t.Fatalf("ops asked: %q; want only the running check", *ops)
	}
}

// TestInstallUndrainsWhenTheRestartFails: a drain found no sessions, so the helper refuses new
// ones, and then the restart failed: install sends undrain, so the running helper takes sessions
// again, and returns the restart's error.
func TestInstallUndrainsWhenTheRestartFails(t *testing.T) {
	r := newInstallRig(t)
	if err := os.WriteFile(r.calls+".fail-restart", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ops := r.fakeHelper(t, nil)
	err := r.run("--url", "https://secrets.example")
	if err == nil || !strings.Contains(err.Error(), "systemctl --user restart agent-secrets-helper.service") {
		t.Fatalf("install with a failing restart: %v; want the restart's error", err)
	}
	if strings.Join(*ops, ",") != "sessions,drain,undrain" {
		t.Fatalf("ops asked: %q; want the running check, drain, then undrain", *ops)
	}
}

// TestInstallNeverRestartsWhenASessionArrivesBeforeTheDrain: the running check finds no sessions,
// but one registers before the drain, which finds it: install does not restart, and prints the
// session and why.
func TestInstallNeverRestartsWhenASessionArrivesBeforeTheDrain(t *testing.T) {
	r := newInstallRig(t)
	r.scriptedHelper(t, func(op string) helper.Response {
		if op == "drain" {
			return helper.Response{OK: true, Sessions: []helper.SessionInfo{{PID: 43, RuntimeID: "rt-late"}}}
		}
		return helper.Response{OK: true}
	})
	if err := r.run("--url", "https://secrets.example"); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.systemctlCalls(t) {
		if strings.Contains(c, "restart") {
			t.Fatalf("restarted a helper whose drain found a session: %q", c)
		}
	}
	out := r.stdout.String()
	if !strings.Contains(out, "43\trt-late") || !strings.Contains(out, "not restarted: 1 sessions are registered") {
		t.Fatalf("output:\n%s", out)
	}
}
