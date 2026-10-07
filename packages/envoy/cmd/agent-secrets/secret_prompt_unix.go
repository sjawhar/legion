// packages/envoy/cmd/agent-secrets/secret_prompt_unix.go
//go:build linux || darwin

// The value prompt's reader on Linux and macOS: one line typed at the terminal with echo off,
// read byte by byte (canonical mode would cut a line past the terminal's line limit, 4095 bytes on
// Linux and 1024 on macOS), and nothing typed or pasted at the prompt left for the shell.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// pasteGapDeciseconds is how long, in tenths of a second, the terminal must stay quiet after the
// line before the reader takes it as the whole value, on a terminal that does not bracket pastes:
// the rest of a paste can arrive in a later write than its first line.
const pasteGapDeciseconds = 2

// maxPasteDrain bounds how long the reader goes on reading, and so discarding, input that keeps
// arriving after the line.
const maxPasteDrain = 10 * time.Second

// The bracketed-paste sequences (xterm's mode 2004): the reader turns the mode on while it reads,
// and a terminal that supports it then sends each paste between pasteStart and pasteEnd.
var (
	bracketedPasteOn  = []byte("\x1b[?2004h")
	bracketedPasteOff = []byte("\x1b[?2004l")
	pasteStart        = []byte("\x1b[200~")
	pasteEnd          = []byte("\x1b[201~")
)

// readHiddenAtTerminal reads one line typed at the terminal fd with echo off. Enter ends the line,
// and so does the terminal's end-of-file character (Ctrl-D), with what was typed before it; the
// erase and kill characters edit it. A paste the terminal brackets is read through its end however
// long it takes to arrive, and is one line when nothing but line endings follows its first line
// ending. On a terminal that does not bracket pastes the reader reads, after the line, what follows
// until the terminal has been quiet for pasteGapDeciseconds. Anything but line endings after the
// line, in either, answers errMoreThanOneLine: a paste of a value of more than one line, which the
// prompt cannot take whole. A hang-up inside a bracketed paste answers errMoreThanOneLine when
// more than line endings followed the line, else errPasteCutShort, never what was read. However
// the read ends, a signal included (restoreOnSignal), it puts the terminal back as it was,
// bracketed paste off, and discards every byte of input it has not read, so nothing typed or
// pasted at the prompt reaches the shell.
func readHiddenAtTerminal(fd int) ([]byte, error) {
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	hidden := *saved
	hidden.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON
	hidden.Lflag |= unix.ISIG
	hidden.Cc[unix.VMIN], hidden.Cc[unix.VTIME] = 1, 0
	rehide := func() error {
		if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &hidden); err != nil {
			return err
		}
		// A terminal opened for reading alone (`< /dev/tty`) takes no write, and then brackets no
		// paste, which the quiet window after the line covers.
		_, _ = unix.Write(fd, bracketedPasteOn)
		return nil
	}
	restore := func() error {
		_, _ = unix.Write(fd, bracketedPasteOff)
		return unix.IoctlSetTermios(fd, ioctlSetTermiosFlush, saved)
	}
	stop := restoreOnSignal(restore, rehide)
	defer stop()
	if err := rehide(); err != nil {
		return nil, err
	}
	defer func() { _ = restore() }()
	r := promptReader{special: func(c byte, index int) bool {
		// A control character set to 0 (Linux) or 0xff (macOS) is disabled.
		v := saved.Cc[index]
		return v != 0 && v != 0xff && c == v
	}}
	buf := make([]byte, 512)
	for {
		n, err := readTerminal(fd, buf)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			// With VMIN 1 a read answers nothing only at end of input: the terminal hung up.
			switch {
			case r.more:
				return nil, errMoreThanOneLine
			case r.inPaste:
				return nil, errPasteCutShort
			}
			return nil, errValueCutShort
		}
		if r.feed(buf[:n]) {
			if r.err != nil {
				return nil, r.err
			}
			break
		}
	}
	if !r.pasted {
		more, err := moreAfterTheLine(fd, hidden, buf)
		if err != nil {
			return nil, err
		}
		r.more = r.more || more
	}
	if r.more {
		return nil, errMoreThanOneLine
	}
	return r.line, nil
}

// promptReader is the state of one read at the prompt, fed each read's bytes in turn.
type promptReader struct {
	special func(c byte, index int) bool // whether c is the terminal's control character index
	line    []byte                       // the value read so far
	pending []byte                       // bytes that may begin a paste mark split across reads
	inPaste bool                         // inside a bracketed paste
	ended   bool                         // the line has ended; only line endings may follow
	more    bool                         // something but line endings followed the line
	pasted  bool                         // the line was ended inside a bracketed paste
	err     error                        // a control byte the prompt cannot safely accept
}

// feed takes one read's bytes and reports whether the read is over: the line ended outside a paste,
// or a paste in which it ended has closed.
func (r *promptReader) feed(b []byte) bool {
	b = append(r.pending, b...)
	r.pending = nil
	for i := 0; i < len(b); i++ {
		if b[i] == 0x1b {
			rest := b[i:]
			if !r.inPaste && bytes.HasPrefix(rest, pasteStart) {
				r.inPaste = true
				i += len(pasteStart) - 1
				continue
			}
			if r.inPaste && bytes.HasPrefix(rest, pasteEnd) {
				r.inPaste = false
				i += len(pasteEnd) - 1
				if r.ended {
					return true
				}
				continue
			}
			if bytes.HasPrefix(pasteStart, rest) || bytes.HasPrefix(pasteEnd, rest) {
				r.pending = append(r.pending, rest...)
				return false
			}
		}
		c := b[i]
		if r.ended {
			// The line ended inside a paste: outside one, feed returned as it ended.
			if c != '\r' && c != '\n' {
				r.more = true
			}
			continue
		}
		switch {
		case c == '\r' || c == '\n' || (!r.inPaste && r.special(c, unix.VEOF)):
			r.ended = true
			if r.inPaste {
				r.pasted = true
				continue
			}
			r.more = r.more || !onlyLineEndings(b[i+1:])
			return true
		case !r.inPaste && (r.special(c, unix.VERASE) || c == 0x7f || c == '\b'):
			if len(r.line) > 0 {
				_, size := utf8.DecodeLastRune(r.line)
				r.line = r.line[:len(r.line)-size]
			}
		case !r.inPaste && r.special(c, unix.VWERASE):
			r.line = eraseWord(r.line)
		case !r.inPaste && r.special(c, unix.VKILL):
			r.line = r.line[:0]
		case !r.inPaste && c < 0x20:
			r.err = promptControlByteError(c)
			return true
		default:
			r.line = append(r.line, c)
		}
	}
	return false
}

// eraseWord removes trailing blanks, then the preceding word, by rune as canonical terminal
// VWERASE does. The separating blank stays for the word that follows.
func eraseWord(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if !unicode.IsSpace(r) {
			break
		}
		b = b[:len(b)-size]
	}
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if unicode.IsSpace(r) {
			break
		}
		b = b[:len(b)-size]
	}
	return b
}

// moreAfterTheLine reports whether anything but line endings follows the line on a terminal that
// does not bracket pastes: what it receives until it has been quiet for pasteGapDeciseconds (for at
// most maxPasteDrain), all of which it reads into buf and so discards.
func moreAfterTheLine(fd int, hidden unix.Termios, buf []byte) (bool, error) {
	hidden.Cc[unix.VMIN], hidden.Cc[unix.VTIME] = 0, pasteGapDeciseconds
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &hidden); err != nil {
		return false, err
	}
	more := false
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

// readTerminal is one read of the terminal, retried when a signal interrupts it; tests replace it.
var readTerminal = func(fd int, buf []byte) (int, error) {
	for {
		n, err := unix.Read(fd, buf)
		if !errors.Is(err, unix.EINTR) {
			return n, err
		}
	}
}

// restoreOnSignal restores the terminal and re-raises a signal that interrupts, quits or stops the
// prompt. A stop resumes at the call to Kill: then the watcher re-arms that signal, reapplies
// hidden and turns bracketed paste on before the reader keeps going. A signal inherited as ignored,
// as from a script that traps it with an empty action, stays ignored.
func restoreOnSignal(restore func() error, rehide func() error) (stop func()) {
	var watched []os.Signal
	for _, sig := range []os.Signal{
		os.Interrupt,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGTSTP,
		syscall.SIGTTIN,
		syscall.SIGTTOU,
	} {
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
		for {
			select {
			case sig := <-signals:
				if err := restore(); err != nil {
					fmt.Fprintf(os.Stderr, "agent-secrets: restore the value prompt: %v\n", err)
					os.Exit(1)
				}
				fmt.Fprintln(os.Stderr)
				signal.Reset(sig)
				if promptStopSignal(sig) {
					if err := resetJobControlSignalDefault(sig.(syscall.Signal)); err != nil {
						fmt.Fprintf(os.Stderr, "agent-secrets: reset job-control signal: %v\n", err)
						os.Exit(1)
					}
				}
				_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
				if promptStopSignal(sig) {
					// SIGCONT from fg resumes at the next statement.
					signal.Notify(signals, sig)
					if err := rehide(); err != nil {
						_ = restore()
						fmt.Fprintf(os.Stderr, "agent-secrets: re-hide the value prompt: %v\n", err)
						os.Exit(1)
					}
					continue
				}
				time.Sleep(time.Second)
				switch sig {
				case syscall.SIGTERM:
					os.Exit(exitTerminated)
				case syscall.SIGQUIT:
					os.Exit(exitQuit)
				default:
					os.Exit(exitInterrupted)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

// promptStopSignal is a job-control signal that resumes at the statement after it is re-raised.
func promptStopSignal(sig os.Signal) bool {
	switch sig {
	case syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU:
		return true
	default:
		return false
	}
}
