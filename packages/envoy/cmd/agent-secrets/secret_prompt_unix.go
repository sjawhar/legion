// packages/envoy/cmd/agent-secrets/secret_prompt_unix.go
//go:build linux || darwin

// The value prompt's reader on Linux and macOS: one line typed at the terminal with echo off,
// read byte by byte (canonical mode would cut a line past the terminal's line limit, 4095 bytes on
// Linux and 1024 on macOS), and nothing typed at the prompt left for the shell.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// pasteGapDeciseconds is how long, in tenths of a second, the terminal must stay quiet after the
// line before the reader takes it as the whole value: the rest of a paste can arrive in a later
// write than its first line, as over ssh.
const pasteGapDeciseconds = 2

// maxPasteDrain bounds how long the reader goes on reading, and so discarding, input that keeps
// arriving after the line.
const maxPasteDrain = 10 * time.Second

// readHiddenAtTerminal reads one line typed at the terminal fd with echo off. Enter ends the line,
// and so does the terminal's end-of-file character (Ctrl-D), with what was typed before it; the
// erase and kill characters edit it. It then reads what follows until the terminal has been quiet
// for pasteGapDeciseconds, and answers errMoreThanOneLine when that holds anything but line
// endings: a paste of a value of more than one line, which the prompt cannot take whole. However it
// ends, a signal included (restoreOnSignal), it puts the terminal back as it was and discards every
// byte of input it has not read, so nothing typed or pasted at the prompt reaches the shell.
func readHiddenAtTerminal(fd int) ([]byte, error) {
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	restore := func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermiosFlush, saved) }
	stop := restoreOnSignal(restore)
	defer stop()
	hidden := *saved
	hidden.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON
	hidden.Lflag |= unix.ISIG
	hidden.Cc[unix.VMIN], hidden.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &hidden); err != nil {
		return nil, err
	}
	defer restore()
	special := func(c byte, index int) bool {
		// A control character set to 0 (Linux) or 0xff (macOS) is disabled.
		v := saved.Cc[index]
		return v != 0 && v != 0xff && c == v
	}
	var line []byte
	buf := make([]byte, 512)
	for {
		n, err := readTerminal(fd, buf)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			// With VMIN 1 a read answers nothing only at end of input: the terminal hung up.
			return line, nil
		}
		for i, c := range buf[:n] {
			switch {
			case c == '\r' || c == '\n' || special(c, unix.VEOF):
				more, err := moreThanLineEndings(fd, hidden, buf, buf[i+1:n])
				if err != nil {
					return nil, err
				}
				if more {
					return nil, errMoreThanOneLine
				}
				return line, nil
			case special(c, unix.VERASE) || c == 0x7f || c == '\b':
				if len(line) > 0 {
					_, size := utf8.DecodeLastRune(line)
					line = line[:len(line)-size]
				}
			case special(c, unix.VKILL):
				line = line[:0]
			default:
				line = append(line, c)
			}
		}
	}
}

// moreThanLineEndings reports whether anything but line endings follows the line: in rest, the
// remainder of the read that ended it, or in what the terminal receives until it has been quiet for
// pasteGapDeciseconds (for at most maxPasteDrain), all of which it reads into buf and so discards.
func moreThanLineEndings(fd int, hidden unix.Termios, buf, rest []byte) (bool, error) {
	more := !onlyLineEndings(rest)
	hidden.Cc[unix.VMIN], hidden.Cc[unix.VTIME] = 0, pasteGapDeciseconds
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &hidden); err != nil {
		return false, err
	}
	for deadline := time.Now().Add(maxPasteDrain); time.Now().Before(deadline); {
		n, err := readTerminal(fd, buf)
		if err != nil {
			return false, err
		}
		if n == 0 {
			break
		}
		more = more || !onlyLineEndings(buf[:n])
	}
	return more, nil
}

// onlyLineEndings reports whether b holds nothing but carriage returns and newlines: what a
// terminal sends for a pasted line ending in CR LF.
func onlyLineEndings(b []byte) bool {
	for _, c := range b {
		if c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}

// readTerminal is one read of the terminal, retried when a signal interrupts it.
func readTerminal(fd int, buf []byte) (int, error) {
	for {
		n, err := unix.Read(fd, buf)
		if !errors.Is(err, unix.EINTR) {
			return n, err
		}
	}
}

// restoreOnSignal makes an interrupt (Ctrl-C) or a termination while the prompt reads put the
// terminal back with restore and exit 130 or 143, until stop is called. A signal the process
// inherited as ignored, as from a script that traps SIGINT with an empty action, stays ignored, so
// the read goes on with echo off.
func restoreOnSignal(restore func()) (stop func()) {
	var watched []os.Signal
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		if !signal.Ignored(sig) {
			watched = append(watched, sig)
		}
	}
	if len(watched) == 0 {
		return func() {}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, watched...)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			restore()
			fmt.Fprintln(os.Stderr)
			if sig == syscall.SIGTERM {
				os.Exit(exitTerminated)
			}
			os.Exit(exitInterrupted)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}
