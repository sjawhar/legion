// packages/envoy/cmd/agent-secrets/secret_tty_linux_test.go
//go:build linux

// The value prompt's reader (readHidden) at a real pseudo-terminal: what a person types or pastes
// at it, Ctrl-D, and what it leaves for the shell once it returns. Linux-only: the pseudo-terminal
// pair is opened with Linux's /dev/ptmx ioctls.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal: the controller, the side a terminal emulator writes what a
// person types or pastes to, and the terminal a program reads, as a blocking descriptor.
func openPTY(t *testing.T) (controller *os.File, terminal int) {
	t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { ptmx.Close() })
	raw, err := ptmx.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var number uint32
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		if ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioctlErr == nil {
			number, ioctlErr = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN)
		}
	}); err != nil || ioctlErr != nil {
		t.Fatalf("unlock the pseudo-terminal: %v %v", err, ioctlErr)
	}
	fd, err := unix.Open(fmt.Sprintf("/dev/pts/%d", number), unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/pts/%d: %v", number, err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	return ptmx, fd
}

// lflag is the terminal's local mode flags.
func lflag(t *testing.T, fd int) uint32 {
	t.Helper()
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag
}

// promptRead is what readHidden returned.
type promptRead struct {
	line []byte
	err  error
}

// typeAtPrompt starts readHidden at a fresh terminal and, once it has turned echo off, writes typed
// to the controller as a terminal emulator does for a person's keys or a paste. It answers the
// pseudo-terminal and the reader's answer, failing t if the reader has not answered within two
// seconds.
func typeAtPrompt(t *testing.T, typed string) (*os.File, int, promptRead) {
	t.Helper()
	return typeAtPromptInWrites(t, 0, typed)
}

// typeAtPromptInWrites is typeAtPrompt with what the person types or pastes reaching the terminal
// in several writes, gap apart, as a paste over a slow link does.
func typeAtPromptInWrites(t *testing.T, gap time.Duration, writes ...string) (*os.File, int, promptRead) {
	t.Helper()
	controller, terminal := openPTY(t)
	answer := make(chan promptRead, 1)
	go func() {
		line, err := readHidden(terminal)
		answer <- promptRead{line, err}
	}()
	awaitEchoOff(t, terminal)
	for i, w := range writes {
		if i > 0 {
			time.Sleep(gap)
		}
		if _, err := controller.Write([]byte(w)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-answer:
		return controller, terminal, got
	case <-time.After(2 * time.Second):
		t.Fatalf("the reader did not return within 2s of %q being typed", writes)
		return nil, 0, promptRead{}
	}
}

// awaitEchoOff waits until the prompt has turned the terminal's echo off, failing t after ten
// seconds: a prompt in a process of its own may take a while to start on a loaded machine.
func awaitEchoOff(t *testing.T, terminal int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); lflag(t, terminal)&unix.ECHO != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the reader never turned echo off")
		}
	}
}

// unread is how many bytes the terminal holds unread, which the shell would read once the form
// exits: with canonical mode turned off, so a last line without a newline counts too.
func unread(t *testing.T, fd int) int {
	t.Helper()
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	tio.Lflag &^= unix.ICANON
	tio.Cc[unix.VMIN], tio.Cc[unix.VTIME] = 0, 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, tio); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// shown is what the terminal wrote back to the controller, which the person would see: an echo.
func shown(t *testing.T, controller *os.File) []byte {
	t.Helper()
	if err := controller.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := controller.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			return out.Bytes()
		}
	}
}

// awaitOutput drains pty output until it contains want, failing after ten seconds.
func awaitOutput(t *testing.T, controller *os.File, want string) []byte {
	t.Helper()
	var out bytes.Buffer
	for deadline := time.Now().Add(10 * time.Second); ; {
		out.Write(shown(t, controller))
		if bytes.Contains(out.Bytes(), []byte(want)) {
			return out.Bytes()
		}
		if time.Now().After(deadline) {
			t.Fatalf("the terminal did not show %q; got %q", want, out.String())
		}
	}
}

// TestPromptReadsOneTypedLineWithEchoOff: Enter ends the value, which is never echoed; the reader
// asks the terminal to bracket pastes while it reads and to stop once it is done, and leaves it as
// it was, echo on.
func TestPromptReadsOneTypedLineWithEchoOff(t *testing.T) {
	controller, terminal, got := typeAtPrompt(t, "typed value\r")
	if got.err != nil || string(got.line) != "typed value" {
		t.Fatalf("readHidden = %q, %v; want %q", got.line, got.err, "typed value")
	}
	echoed := shown(t, controller)
	if bytes.Contains(echoed, []byte("typed")) {
		t.Fatalf("the terminal showed %q: the value was echoed", echoed)
	}
	on, off := bytes.Index(echoed, []byte("\x1b[?2004h")), bytes.LastIndex(echoed, []byte("\x1b[?2004l"))
	if on < 0 || off < on {
		t.Fatalf("the terminal was sent %q, want bracketed paste turned on and then off", echoed)
	}
	if flags := lflag(t, terminal); flags&unix.ECHO == 0 || flags&unix.ICANON == 0 {
		t.Fatalf("lflag after the read = %#x, want echo and canonical mode back on", flags)
	}
}

// TestPromptCtrlDEndsTheValue: Ctrl-D ends the read, with nothing typed (an empty value) and after
// typing (the value typed).
func TestPromptCtrlDEndsTheValue(t *testing.T) {
	for _, tc := range []struct{ typed, want string }{{"\x04", ""}, {"typed\x04", "typed"}} {
		_, terminal, got := typeAtPrompt(t, tc.typed)
		if got.err != nil || string(got.line) != tc.want {
			t.Fatalf("after %q: readHidden = %q, %v; want %q", tc.typed, got.line, got.err, tc.want)
		}
		if flags := lflag(t, terminal); flags&unix.ECHO == 0 {
			t.Fatalf("after %q: lflag = %#x, want echo back on", tc.typed, flags)
		}
	}
}

// TestPromptRefusesAPasteOfMoreThanOneLineAndLeavesNothingForTheShell: a paste of a value of more
// than one line, as a terminal sends it (each line ended by a carriage return, the last one with
// or without), is refused rather than cut at its first line, and none of it is left for the shell
// to read once the form exits, nor echoed.
func TestPromptRefusesAPasteOfMoreThanOneLineAndLeavesNothingForTheShell(t *testing.T) {
	for _, paste := range []string{
		"-----BEGIN KEY-----\rline two\recho line three",
		"-----BEGIN KEY-----\nline two\nline three\n",
	} {
		controller, terminal, got := typeAtPrompt(t, paste)
		if !errors.Is(got.err, errMoreThanOneLine) {
			t.Fatalf("paste %q: readHidden = %q, %v; want errMoreThanOneLine", paste, got.line, got.err)
		}
		if n := unread(t, terminal); n != 0 {
			t.Fatalf("paste %q: the terminal holds %d bytes unread for the shell, want none", paste, n)
		}
		if echoed := shown(t, controller); bytes.Contains(echoed, []byte("line")) {
			t.Fatalf("paste %q: the terminal showed %q", paste, echoed)
		}
	}
}

// TestPromptReadsABracketedPasteThroughItsEnd: a bracketed paste of more than one line is read
// through its closing mark however long a gap splits it, as one lost packet over ssh can, so it is
// refused whole and nothing of it is left for the shell or echoed.
func TestPromptReadsABracketedPasteThroughItsEnd(t *testing.T) {
	controller, terminal, got := typeAtPromptInWrites(t, 300*time.Millisecond, "\x1b[200~line1\n", "line2\x1b[201~")
	if !errors.Is(got.err, errMoreThanOneLine) {
		t.Fatalf("readHidden = %q, %v; want errMoreThanOneLine", got.line, got.err)
	}
	if n := unread(t, terminal); n != 0 {
		t.Fatalf("the terminal holds %d bytes unread for the shell, want none", n)
	}
	if echoed := shown(t, controller); bytes.Contains(echoed, []byte("line")) {
		t.Fatalf("the terminal showed %q", echoed)
	}
}

// TestPromptAcceptsABracketedPasteOfOneLine: a bracketed paste of one line is the value, never its
// marks: ending in a line ending, which is dropped, or followed by Enter, with its marks whole or
// split across reads.
func TestPromptAcceptsABracketedPasteOfOneLine(t *testing.T) {
	for _, writes := range [][]string{
		{"\x1b[200~value\n\x1b[201~"},
		{"\x1b[200~value\r\n\x1b[201~"},
		{"\x1b[200~value\x1b[201~", "\r"},
		{"\x1b[20", "0~value\n\x1b[2", "01~"},
	} {
		_, terminal, got := typeAtPromptInWrites(t, 50*time.Millisecond, writes...)
		if got.err != nil || string(got.line) != "value" {
			t.Fatalf("%q: readHidden = %q, %v; want %q", writes, got.line, got.err, "value")
		}
		if n := unread(t, terminal); n != 0 {
			t.Fatalf("%q: the terminal holds %d bytes unread, want none", writes, n)
		}
	}
}

// TestPromptWordEraseAndControlBytes: the terminal's configured word erase removes the word it
// would in canonical mode, and another control byte cannot become an invisible byte in a secret.
func TestPromptWordEraseAndControlBytes(t *testing.T) {
	_, terminal, got := typeAtPrompt(t, "abc def\x17ghi\r")
	if got.err != nil || string(got.line) != "abc ghi" {
		t.Fatalf("word erase: readHidden = %q, %v; want %q", got.line, got.err, "abc ghi")
	}
	if n := unread(t, terminal); n != 0 {
		t.Fatalf("word erase: the terminal holds %d bytes unread, want none", n)
	}
	_, _, got = typeAtPrompt(t, "value\x01\r")
	if got.err == nil || !strings.Contains(got.err.Error(), "the control byte 0x01 cannot be typed at the prompt") {
		t.Fatalf("control byte: readHidden = %q, %v; want refusal naming 0x01", got.line, got.err)
	}
}

// TestPromptRefusesAPasteCutShortByAHangUp: when the terminal hangs up inside a bracketed paste, as
// when its window or ssh session closes mid-paste, the reader answers an error rather than what it
// has read, so nothing is stored. The hang-up lands while the reader is between reads, where its
// next read of the hung-up terminal answers end of input.
func TestPromptRefusesAPasteCutShortByAHangUp(t *testing.T) {
	for _, tc := range []struct {
		paste string
		want  error
	}{
		{"\x1b[200~-----BEGIN KEY-----\nline2", errMoreThanOneLine},
		{"\x1b[200~value\n", errPasteCutShort},
		{"\x1b[200~val", errPasteCutShort},
		{"partial-secr", errValueCutShort},
	} {
		t.Run(fmt.Sprintf("%q", tc.paste), func(t *testing.T) {
			controller, terminal := openPTY(t)
			real := readTerminal
			t.Cleanup(func() { readTerminal = real })
			consumed, hungUp := make(chan struct{}), make(chan struct{})
			var once sync.Once
			read := 0
			readTerminal = func(fd int, buf []byte) (int, error) {
				if read >= len(tc.paste) {
					once.Do(func() { close(consumed) })
					<-hungUp
				}
				n, err := real(fd, buf)
				read += n
				return n, err
			}
			answer := make(chan promptRead, 1)
			go func() {
				line, err := readHidden(terminal)
				answer <- promptRead{line, err}
			}()
			awaitEchoOff(t, terminal)
			if _, err := controller.Write([]byte(tc.paste)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-consumed:
			case <-time.After(2 * time.Second):
				t.Fatal("the reader did not read the paste within 2s")
			}
			if err := controller.Close(); err != nil {
				t.Fatal(err)
			}
			close(hungUp)
			select {
			case got := <-answer:
				if !errors.Is(got.err, tc.want) || got.line != nil {
					t.Fatalf("readHidden = %q, %v; want no value and %v", got.line, got.err, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the reader did not return within 2s of the hang-up")
			}
		})
	}
}

// TestPromptSignalHelper is the process the signal tests start: the value prompt's reader on its
// standard input, its controlling terminal. It names only the reader's error, never its value.
func TestPromptSignalHelper(t *testing.T) {
	if os.Getenv("AGENT_SECRETS_PROMPT_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	_, err := readHidden(0)
	fmt.Printf("RETURNED %v\n", err)
}

// TestPromptCtrlCEndsTheProcessBySIGINT: Ctrl-C at the prompt ends the process by SIGINT, echo back
// on, so a calling shell stops its list rather than running the next command. The prompt runs in a
// process of its own, the session leader whose controlling terminal sends the Ctrl-C, started by
// `env --default-signal`, so it does not inherit a SIGINT this test process ignores (as a job run in
// the background of a script does), at which the prompt rightly goes on reading.
func TestPromptCtrlCEndsTheProcessBySIGINT(t *testing.T) {
	reset := []string{"env", "--default-signal=INT,TERM"}
	helper := shellWord(os.Args[0]) + " -test.run=^TestPromptSignalHelper$"
	for _, tc := range []struct {
		name    string
		command *exec.Cmd
	}{
		{"the prompt alone", exec.Command(reset[0], reset[1], os.Args[0], "-test.run=^TestPromptSignalHelper$")},
		{"a bash list", exec.Command(reset[0], reset[1], "bash", "-c", helper+"; echo SECOND")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, terminal := openPTY(t)
			fd, err := unix.Dup(terminal)
			if err != nil {
				t.Fatal(err)
			}
			tty := os.NewFile(uintptr(fd), "terminal")
			cmd := tc.command
			cmd.Env = append(os.Environ(), "AGENT_SECRETS_PROMPT_HELPER=1")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			err = cmd.Start()
			tty.Close()
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			awaitEchoOff(t, terminal)
			if _, err := controller.Write([]byte{0x03}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("the process did not end within 10s of Ctrl-C; the terminal showed %q", shown(t, controller))
			}
			status := cmd.ProcessState.Sys().(syscall.WaitStatus)
			out := shown(t, controller)
			if tc.name == "the prompt alone" && (!status.Signaled() || status.Signal() != syscall.SIGINT) {
				t.Fatalf("wait status %v (%s); want ended by SIGINT. The terminal showed %q", status, cmd.ProcessState, out)
			}
			if bytes.Contains(out, []byte("SECOND")) || bytes.Contains(out, []byte("RETURNED")) {
				t.Fatalf("the terminal showed %q (%s): the list went on past the Ctrl-C", out, cmd.ProcessState)
			}
			if lflag(t, terminal)&unix.ECHO == 0 {
				t.Fatal("echo is off after the process ended")
			}
		})
	}
}

// TestPromptQuitRestoresTerminal: Ctrl-\ at the prompt restores echo before the process quits.
func TestPromptQuitRestoresTerminal(t *testing.T) {
	controller, terminal := openPTY(t)
	fd, err := unix.Dup(terminal)
	if err != nil {
		t.Fatal(err)
	}
	tty := os.NewFile(uintptr(fd), "terminal")
	cmd := exec.Command("env", "--default-signal=QUIT", os.Args[0], "-test.run=^TestPromptSignalHelper$")
	cmd.Env = append(os.Environ(), "AGENT_SECRETS_PROMPT_HELPER=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		tty.Close()
		t.Fatal(err)
	}
	tty.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	awaitEchoOff(t, terminal)
	if _, err := controller.Write([]byte{0x1c}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the process did not quit within 10s of Ctrl-\\")
	}
	if lflag(t, terminal)&unix.ECHO == 0 {
		t.Fatalf("echo is off after Ctrl-\\; the terminal showed %q", shown(t, controller))
	}
}
