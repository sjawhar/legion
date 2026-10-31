package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The command's refusals come before anything starts, each naming what is missing, as a usage error.
func TestClaudeBridgeRefusesWhatItCannotRun(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("LEGION_ROLE", "implementer")
	t.Setenv("LEGION_ISSUE", "ACME-7")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "no claude command to run after --"},
		{[]string{"--", "claude", "-p"}, "exactly one --session-id or --resume"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"legion", "claude-bridge"}, tc.args...), &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), tc.want) || !strings.Contains(stderr.String(), claudeBridgeUsage) {
			t.Errorf("claude-bridge %q = %d %q, want 2 naming %q and the usage", tc.args, code, stderr.String(), tc.want)
		}
	}
}

// A claude that cannot be started is the bridge's failure, exit 1, naming it.
func TestClaudeBridgeNamesAClaudeItCannotStart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"legion", "claude-bridge", "--", "/nonexistent/claude", "--resume", "s"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "start /nonexistent/claude") {
		t.Errorf("claude-bridge with no claude = %d %q", code, stderr.String())
	}
}

// A tmux pane's processes get SIGHUP when the pane or its server goes (kill-pane, kill-server):
// the bridge ends then as it ends on SIGTERM, stopping Claude Code and removing its inbox's
// directory, so nothing it made stays in TMPDIR.
func TestClaudeBridgeRemovesItsInboxWhenHungUp(t *testing.T) {
	stdout, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	p := startClaudeBridge(t, "touch \"$CLAUDE_STANDIN_READY\"\nexec sleep 300\n", stdout)
	state, left := p.hangUp()
	// Claude Code's status is the bridge's: the stand-in ended by the bridge's SIGTERM.
	if code := state.ExitCode(); code != 128+int(syscall.SIGTERM) {
		t.Errorf("the bridge given SIGHUP ended %v, want exit %d, Claude Code's status once SIGTERM ended it:\n%s",
			state, 128+int(syscall.SIGTERM), p.logged())
	}
	if len(left) > 0 {
		t.Errorf("the bridge given SIGHUP left %q in TMPDIR", left)
	}
}

// The shim dies with its pane, so the bridge's stdout has no reader through its shutdown, while
// Claude Code's last lines are still read and answered: a frame the bridge writes then fails, is
// logged, and ends nothing, rather than SIGPIPE ending the bridge before it removes its inbox.
func TestClaudeBridgeOutlivesTheShimItsPaneTookWithIt(t *testing.T) {
	// The stand-in answers the bridge's SIGTERM with a tool call, which the bridge shows on the pane
	// with a frame to the shim.
	toolCall := filepath.Join(t.TempDir(), "tool-call.json")
	if err := os.WriteFile(toolCall, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startClaudeBridge(t, "trap 'kill $!; cat "+toolCall+"; exit 0' TERM\n"+
		"sleep 300 >/dev/null &\ntouch \"$CLAUDE_STANDIN_READY\"\nwait\n", goneShim(t))
	state, left := p.hangUp()
	if code := state.ExitCode(); code != 0 || !strings.Contains(p.logged(), "could not write to the shim") {
		t.Errorf("the bridge given SIGHUP with no shim ended %v, want exit 0, Claude Code's status, after logging the frame it could not write:\n%s", state, p.logged())
	}
	if len(left) > 0 {
		t.Errorf("the bridge given SIGHUP with no shim left %q in TMPDIR", left)
	}
}

// Claude Code ends a turn and exits while the shim reads slowly: the bridge waits for the shim to
// read every frame, the turn's agent_end among them, whole, and exits as Claude Code did.
func TestClaudeBridgeExitsOnceTheShimHasReadEveryFrame(t *testing.T) {
	turn := filepath.Join(t.TempDir(), "turn")
	content := strings.Repeat("x", 256<<10)
	if err := os.WriteFile(turn, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"WAITING on CI"},`+
		`{"type":"tool_use","id":"t1","name":"Write","input":{"content":"`+content+`"}}]}}`+"\n"+
		`{"type":"result","subtype":"success","is_error":false}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shim, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer shim.Close()
	// The stand-in takes the turn's prompt, answers it, and exits 0.
	p := startClaudeBridge(t, "touch \"$CLAUDE_STANDIN_READY\"\nread -r prompt\ncat "+turn+"\n", stdout)
	// The bridge holds its own copy: the shim's end reads to its end once the bridge exits.
	_ = stdout.Close()
	p.send(shimwire.Prompt{ID: "p1", DeliveryID: "d-p1", Message: "go"})
	// A shim that reads, slowly: 4 KiB every 25 ms, so the 256 KiB frame takes it over 1.5 s.
	var read bytes.Buffer
	chunk := make([]byte, 4<<10)
	for {
		n, err := shim.Read(chunk)
		read.Write(chunk[:n])
		if err != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	state, left := p.exit("Claude Code's exit")
	if code := state.ExitCode(); code != 0 {
		t.Errorf("the bridge ended %v, want exit 0, Claude Code's:\n%s", state, p.logged())
	}
	var types []string
	for i, line := range strings.Split(strings.TrimSuffix(read.String(), "\n"), "\n") {
		var frame struct {
			Type string `json:"type"`
			Args struct {
				Content string `json:"content"`
			} `json:"args"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("frame %d (%d bytes) is not whole: %v\n%s", i, len(line), err, p.logged())
		}
		if frame.Type == "tool_execution_start" && frame.Args.Content != content {
			t.Errorf("the tool call's frame carries %d bytes of content, want %d", len(frame.Args.Content), len(content))
		}
		types = append(types, frame.Type)
	}
	if want := []string{"response", "agent_start", "tool_execution_start", "agent_end"}; !slices.Equal(types, want) {
		t.Errorf("the shim read frames %q, want %q:\n%s", types, want, p.logged())
	}
	if len(left) > 0 {
		t.Errorf("the bridge left %q in TMPDIR", left)
	}
}

// goneShim is a pipe whose reader is already closed: the bridge's stdout once its pane's shim died.
func goneShim(t *testing.T) *os.File {
	t.Helper()
	shim, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = shim.Close()
	t.Cleanup(func() { _ = stdout.Close() })
	return stdout
}

// claudeBridgeProcess is `legion claude-bridge` run as a process of its own (this package's test
// binary as the real `legion`) on a stand-in Claude Code: a shell script that ignores its arguments
// and touches $CLAUDE_STANDIN_READY once it is ready for the bridge's signals.
type claudeBridgeProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser // the shim's end of the bridge's stdin
	exited chan struct{}
	tmp    string // the bridge's TMPDIR, where its inbox is
	stderr string // the file holding what the bridge logged
}

// startClaudeBridge starts the bridge on the stand-in script with stdout as its stdout, and returns
// once its inbox is open and Claude Code is ready.
func startClaudeBridge(t *testing.T, script string, stdout *os.File) *claudeBridgeProcess {
	t.Helper()
	dir := t.TempDir()
	p := &claudeBridgeProcess{t: t, tmp: socketDir(t), stderr: filepath.Join(dir, "stderr"), exited: make(chan struct{})}
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(p.stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	ready := filepath.Join(dir, "ready")
	p.cmd = exec.Command(os.Args[0], "claude-bridge", "--", claude, "--resume", "s")
	p.cmd.Env = append(os.Environ(), testMainEnv+"=1", "TMPDIR="+p.tmp, "CLAUDE_CONFIG_DIR="+t.TempDir(),
		"LEGION_ROLE=implementer", "LEGION_ISSUE=ACME-7", "CLAUDE_STANDIN_READY="+ready)
	p.cmd.Stdout, p.cmd.Stderr = stdout, stderr
	// Its own process group, so the cleanup kills Claude Code with it whatever the bridge did.
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if p.stdin, err = p.cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start claude-bridge: %v", err)
	}
	go func() {
		_ = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.exited
		_ = p.stdin.Close()
	})
	p.await("its inbox open and Claude Code ready", func() bool {
		sockets, _ := filepath.Glob(filepath.Join(p.tmp, "legion-claude-*", "inbox.sock"))
		_, err := os.Stat(ready)
		return err == nil && len(sockets) > 0
	})
	return p
}

func (p *claudeBridgeProcess) logged() string {
	raw, _ := os.ReadFile(p.stderr)
	return string(raw)
}

// await waits, at most 30s and failing the test if the bridge exits first, until ok holds.
func (p *claudeBridgeProcess) await(what string, ok func() bool) {
	p.t.Helper()
	deadline := time.After(30 * time.Second)
	for !ok() {
		select {
		case <-p.exited:
			p.t.Fatalf("the bridge exited (%v) before %s:\n%s", p.cmd.ProcessState, what, p.logged())
		case <-deadline:
			p.t.Fatalf("not %s within 30s:\n%s", what, p.logged())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// send writes f to the bridge's stdin, as the shim forwards a daemon frame.
func (p *claudeBridgeProcess) send(f shimwire.Frame) {
	p.t.Helper()
	line, err := json.Marshal(f)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.stdin.Write(append(line, '\n')); err != nil {
		p.t.Fatal(err)
	}
}

// hangUp gives the bridge SIGHUP and waits for it to exit (exit).
func (p *claudeBridgeProcess) hangUp() (*os.ProcessState, []string) {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		p.t.Fatal(err)
	}
	return p.exit("SIGHUP")
}

// exit waits, at most 30s after what ended the bridge, for it to exit; it answers how the bridge
// exited and what it left of its inbox in TMPDIR.
func (p *claudeBridgeProcess) exit(after string) (*os.ProcessState, []string) {
	p.t.Helper()
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		p.t.Fatalf("the bridge did not exit within 30s of %s:\n%s", after, p.logged())
	}
	left, _ := filepath.Glob(filepath.Join(p.tmp, "legion-claude-*"))
	return p.cmd.ProcessState, left
}
