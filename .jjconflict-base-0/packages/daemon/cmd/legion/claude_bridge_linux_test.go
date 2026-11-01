package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// These tests hold the bridge to what it does when a reader of one of its pipes takes none of a
// line larger than the pipe holds, or none at all for a while. Each waits, through /proc and the
// pipe's byte count (TIOCINQ, which is FIONREAD), until that pipe is full, so the writer is blocked
// when the test acts.

// The bridge blocked writing Claude Code a prompt it never reads, with no shim reading its frames:
// SIGHUP still ends Claude Code, and the bridge exits and removes its inbox, whether its input has
// ended first or is still open.
func TestClaudeBridgeEndsAClaudeCodeItIsBlockedWritingTo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inputEnds bool
	}{{"input ended", true}, {"input open", false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := startClaudeBridge(t, "touch \"$CLAUDE_STANDIN_READY\"\nexec sleep 300\n", goneShim(t))
			p.blockOnAPrompt()
			if tc.inputEnds {
				_ = p.stdin.Close()
			}
			state, left := p.hangUp()
			if code := state.ExitCode(); code != 128+int(syscall.SIGTERM) {
				t.Errorf("the bridge ended %v, want exit %d, Claude Code's status once SIGTERM ended it:\n%s",
					state, 128+int(syscall.SIGTERM), p.logged())
			}
			if len(left) > 0 {
				t.Errorf("the bridge left %q in TMPDIR", left)
			}
		})
	}
}

// The shim goes, both its pipes closed, while the bridge is blocked writing Claude Code a prompt it
// never reads and a daemon frame follows the prompt: the end of the bridge's input alone, with no
// signal, ends Claude Code, and the bridge exits and removes its inbox.
func TestClaudeBridgeEndsWithItsInputWhileBlockedWritingToClaudeCode(t *testing.T) {
	p := startClaudeBridge(t, "touch \"$CLAUDE_STANDIN_READY\"\nexec sleep 300\n", goneShim(t))
	p.blockOnAPrompt()
	p.send(shimwire.GetState{ID: "s1"})
	_ = p.stdin.Close()
	state, left := p.exit("its input ending")
	if code := state.ExitCode(); code != 128+int(syscall.SIGTERM) {
		t.Errorf("the bridge ended %v, want exit %d, Claude Code's status once SIGTERM ended it:\n%s",
			state, 128+int(syscall.SIGTERM), p.logged())
	}
	if len(left) > 0 {
		t.Errorf("the bridge left %q in TMPDIR", left)
	}
}

// The shim alive but no longer reading, a frame larger than a pipe holds unread: SIGHUP still ends
// Claude Code, and the bridge exits and removes its inbox, saying it leaves that frame unwritten.
func TestClaudeBridgeEndsWithItsFramesUnread(t *testing.T) {
	shim, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer shim.Close()
	defer stdout.Close()
	// A tool call whose input a pipe cannot hold: the bridge shows it on the pane with a frame of
	// its size.
	toolCall := filepath.Join(t.TempDir(), "tool-call.json")
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Write","input":{"content":"` +
		strings.Repeat("x", 256<<10) + `"}}]}}` + "\n"
	if err := os.WriteFile(toolCall, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startClaudeBridge(t, "cat "+toolCall+"\ntouch \"$CLAUDE_STANDIN_READY\"\nexec sleep 300\n", stdout)
	p.awaitFull("the bridge's stdout full, the shim reading none of it", shim)
	state, left := p.hangUp()
	if code := state.ExitCode(); code != 128+int(syscall.SIGTERM) {
		t.Errorf("the bridge ended %v, want exit %d, Claude Code's status once SIGTERM ended it:\n%s",
			state, 128+int(syscall.SIGTERM), p.logged())
	}
	if !strings.Contains(p.logged(), "leaving them") {
		t.Errorf("the bridge did not say it leaves the frames the shim has not read:\n%s", p.logged())
	}
	if len(left) > 0 {
		t.Errorf("the bridge left %q in TMPDIR", left)
	}
}

// The shim stops reading, as it does while its write to the daemon waits, for longer than the
// bridge holds frames, and Claude Code writes on: the bridge stops taking Claude Code's output, so
// Claude Code waits on its own stdout and is never signalled, and once the shim reads again every
// frame arrives, in order, and the session ends as Claude Code ends it.
func TestClaudeBridgeHoldsClaudeCodeWhileTheShimPauses(t *testing.T) {
	const frames = 2000
	dir := t.TempDir()
	stream, signals := filepath.Join(dir, "frames"), filepath.Join(dir, "signals")
	var lines strings.Builder
	pad := strings.Repeat("x", 4000)
	for i := range frames {
		fmt.Fprintf(&lines, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{"i":%d,"pad":"%s"}}]}}`+"\n", i, i, pad)
	}
	if err := os.WriteFile(stream, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	shim, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer shim.Close()
	p := startClaudeBridge(t, "trap 'echo TERM >>"+signals+"' TERM\ntrap 'echo HUP >>"+signals+"' HUP\n"+
		"touch \"$CLAUDE_STANDIN_READY\"\ncat "+stream+"\n", stdout)
	// The bridge holds its own copy: the shim's end reads to its end once the bridge exits.
	_ = stdout.Close()
	p.await("the bridge holding Claude Code's output for the shim", func() bool {
		return strings.Contains(p.logged(), "Claude Code's output waits")
	})
	claudeOut, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/1", p.claudePID()), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open Claude Code's stdout: %v", err)
	}
	p.awaitFull("Claude Code's stdout full, the bridge taking none of it", claudeOut)
	_ = claudeOut.Close()

	// The shim reads again.
	next, reader := 0, bufio.NewReader(shim)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break
		}
		var frame struct {
			Type string `json:"type"`
			Args struct {
				I int `json:"i"`
			} `json:"args"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatalf("frame %d is not whole: %v", next, err)
		}
		if frame.Type == "tool_execution_start" {
			if frame.Args.I != next {
				t.Fatalf("frame %d is tool call %d, want them in order", next, frame.Args.I)
			}
			next++
		}
	}
	state, left := p.exit("Claude Code's exit")
	if next != frames {
		t.Errorf("the shim read %d of the %d tool-call frames", next, frames)
	}
	if code := state.ExitCode(); code != 0 {
		t.Errorf("the bridge ended %v, want exit 0, Claude Code's:\n%s", state, p.logged())
	}
	if got, err := os.ReadFile(signals); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Claude Code was signalled: %q (%v)\n%s", got, err, p.logged())
	}
	if len(left) > 0 {
		t.Errorf("the bridge left %q in TMPDIR", left)
	}
}

// blockOnAPrompt sends the bridge a prompt larger than a pipe holds, for a Claude Code that reads
// none of its stdin, and waits until the bridge's write of it into Claude Code's stdin is blocked.
func (p *claudeBridgeProcess) blockOnAPrompt() {
	p.t.Helper()
	p.send(shimwire.Prompt{ID: "p1", DeliveryID: "d-p1", Message: strings.Repeat("x", 256<<10)})
	stdin, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/0", p.claudePID()), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		p.t.Fatalf("open Claude Code's stdin: %v", err)
	}
	defer stdin.Close()
	p.awaitFull("Claude Code's stdin full, Claude Code reading none of it", stdin)
}

var startedPID = regexp.MustCompile(`\(pid (\d+)\)`)

// claudePID is Claude Code's pid, as the bridge logs it once it has started it.
func (p *claudeBridgeProcess) claudePID() int {
	p.t.Helper()
	p.await("the bridge logging Claude Code's pid", func() bool { return startedPID.MatchString(p.logged()) })
	match := startedPID.FindStringSubmatch(p.logged())
	pid, err := strconv.Atoi(match[1])
	if err != nil {
		p.t.Fatal(err)
	}
	return pid
}

// awaitFull waits until the pipe whose read end is pipe holds all it can, so a write into it that
// has more to write is blocked. A pipe is a ring of page-sized slots and only the slot at its
// reader's head can be part read, so a full pipe holds more than its size less a page.
func (p *claudeBridgeProcess) awaitFull(what string, pipe *os.File) {
	p.t.Helper()
	fd := int(pipe.Fd())
	size, err := unix.FcntlInt(uintptr(fd), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		p.t.Fatalf("the pipe's size: %v", err)
	}
	p.await(what, func() bool {
		held, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
		return err == nil && held > size-os.Getpagesize()
	})
}
