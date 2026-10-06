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
	controller, terminal := openPTY(t)
	answer := make(chan promptRead, 1)
	go func() {
		line, err := readHidden(terminal)
		answer <- promptRead{line, err}
	}()
	for deadline := time.Now().Add(2 * time.Second); lflag(t, terminal)&unix.ECHO != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the reader never turned echo off")
		}
	}
	if _, err := controller.Write([]byte(typed)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answer:
		return controller, terminal, got
	case <-time.After(2 * time.Second):
		t.Fatalf("the reader did not return within 2s of %q being typed", typed)
		return nil, 0, promptRead{}
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

// TestPromptReadsOneTypedLineWithEchoOff: Enter ends the value, which is never echoed, and the
// terminal is left as it was, echo on.
func TestPromptReadsOneTypedLineWithEchoOff(t *testing.T) {
	controller, terminal, got := typeAtPrompt(t, "typed value\r")
	if got.err != nil || string(got.line) != "typed value" {
		t.Fatalf("readHidden = %q, %v; want %q", got.line, got.err, "typed value")
	}
	if echoed := shown(t, controller); bytes.Contains(echoed, []byte("typed")) {
		t.Fatalf("the terminal showed %q: the value was echoed", echoed)
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
