// packages/envoy/cmd/agent-secrets/secret_prompt_unix.go
//go:build linux || darwin

// The value prompt's reader on Linux and macOS. The reader alone changes the terminal;
// the signal watcher reports a resume or a terminating signal without touching it.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
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

// Core protection stays in place for the process lifetime, including after the prompt
// returns a value to the AWS client.
var disableCoreDumps = sync.OnceValue(preventCoreDumps)

// readHiddenAtTerminal reads one hidden line, drains refused input, and restores the
// terminal only after joining its signal watcher. Bracketed pastes are read through
// their closing mark; unbracketed input is drained through a 200 ms quiet window.
func readHiddenAtTerminal(fd int, onStop func()) (line []byte, err error) {
	if err := disableCoreDumps(); err != nil {
		return nil, err
	}
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	wake, notify, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer wake.Close()
	defer notify.Close()
	events, stop, ignoredControls, err := watchPromptSignals(notify)
	if err != nil {
		return nil, err
	}
	tty := promptTerminal{fd: fd, current: *saved, wake: int(wake.Fd()), events: events, wait: -1, onStop: onStop}
	// Keep the kernel's signal flush: NOFLSH would leave unread secret bytes for
	// bash when a stop gives it the foreground. Every caught stop invalidates the entry.
	tty.current.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.NOFLSH
	tty.current.Lflag |= unix.ISIG
	var ignoredKeys [3]byte
	for i, cc := range ignoredControls {
		if key := saved.Cc[cc]; key != 0 && key != 0xff {
			ignoredKeys[i] = key
			// Even an ignored tty signal flushes input. Disable its tty character
			// and let the reader consume it without flushing the value.
			tty.current.Cc[cc] = disabledControlByte
		}
	}
	// Poll owns both waits. Reads themselves never block, including after resume.
	tty.current.Cc[unix.VMIN], tty.current.Cc[unix.VTIME] = 0, 0
	defer func() {
		pending := stop()
		if pending.sig != 0 && !promptStopSignal(pending.sig) {
			tty.death = pending.sig
		}
		if pending.err != nil && err == nil {
			line, err = nil, pending.err
		}
		if tty.stopped || promptStopSignal(pending.sig) {
			line = nil
			if err == nil {
				err = errPromptStopped
			}
		}
		// A shell owns the terminal while this job is in the background. Restoring
		// it there could stop a terminating job on SIGTTOU or overwrite the shell.
		foreground, fgErr := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		if fgErr != nil || foreground == unix.Getpgrp() {
			_, _ = unix.Write(fd, bracketedPasteOff)
			if restoreErr := unix.IoctlSetTermios(fd, ioctlSetTermiosFlush, saved); restoreErr != nil && err == nil {
				line, err = nil, restoreErr
			}
		}
		if tty.death != 0 {
			endByPromptSignal(tty.death)
		}
	}()
	if err := tty.apply(); err != nil {
		return nil, err
	}
	r := promptReader{ignored: ignoredKeys, special: func(c byte, index int) bool {
		v := saved.Cc[index]
		return v != 0 && v != 0xff && c == v
	}}
	buf := make([]byte, 512)
	for {
		n, err := tty.read(buf)
		if err != nil {
			return nil, err
		}
		if tty.stopped {
			// The kernel may have flushed bytes or paste marks, with no count of
			// what was lost. Drain a fresh line instead of keeping a partial value.
			r = promptReader{special: r.special, ignored: r.ignored, err: errPromptStopped}
			tty.stopped = false
		}
		if n == 0 {
			switch {
			case r.err != nil:
				return nil, r.err
			case r.more:
				return nil, errMoreThanOneLine
			case r.inPaste:
				return nil, errPasteCutShort
			}
			return nil, errValueCutShort
		}
		if r.feed(buf[:n]) {
			break
		}
	}
	if !r.pasted || r.err != nil {
		more, err := moreAfterTheLine(&tty, buf)
		if err != nil {
			return nil, err
		}
		r.more = r.more || more
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.more {
		return nil, errMoreThanOneLine
	}
	return r.line, nil
}

type promptTerminal struct {
	fd      int
	current unix.Termios
	wake    int
	events  <-chan promptSignal
	death   syscall.Signal
	wait    int // poll timeout in milliseconds; -1 until the line ends
	stopped bool
	onStop  func()
}

func (t *promptTerminal) apply() error {
	for {
		err := unix.IoctlSetTermios(t.fd, ioctlSetTermios, &t.current)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	// A terminal opened read-only cannot enable bracketed paste.
	_, _ = unix.Write(t.fd, bracketedPasteOn)
	return nil
}

func (t *promptTerminal) read(buf []byte) (int, error) {
	for {
		select {
		case event := <-t.events:
			if event.err != nil {
				return 0, event.err
			}
			if !promptStopSignal(event.sig) {
				t.death = event.sig
				return 0, fmt.Errorf("value prompt ended by %s", event.sig)
			}
			if err := t.apply(); err != nil {
				return 0, err
			}
			t.stopped = true
			if t.onStop != nil {
				t.onStop()
			}
		default:
		}
		n, err := readTerminal(t.fd, t.wake, buf, t.wait)
		if errors.Is(err, unix.EINTR) {
			if err := t.apply(); err != nil {
				return 0, err
			}
			continue
		}
		if n < 0 && err == nil {
			continue
		}
		return n, err
	}
}

// promptReader is the state of one read at the prompt, fed each read's bytes in turn.
type promptReader struct {
	special func(c byte, index int) bool // whether c is the terminal's control character index
	ignored [3]byte                      // disabled tty characters of inherited ignored signals
	line    []byte                       // the value read so far
	pending []byte                       // bytes that may begin a paste mark split across reads
	inPaste bool                         // inside a bracketed paste
	ended   bool                         // the line has ended; only line endings may follow
	more    bool                         // something but line endings followed the line
	pasted  bool                         // the line was ended inside a bracketed paste
	err     error                        // a refusal whose remaining input must be drained
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
		case !r.inPaste && c != 0 && bytes.IndexByte(r.ignored[:], c) >= 0:
			// Preserve inherited SIG_IGN without the tty driver's input flush.
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
		case c < 0x20 || c == 0x7f:
			if r.err == nil {
				r.err = promptControlByteError(c)
				r.line = nil
			}
		default:
			if r.err == nil {
				r.line = append(r.line, c)
			}
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
func moreAfterTheLine(tty *promptTerminal, buf []byte) (bool, error) {
	tty.wait = pasteGapDeciseconds * 100
	more := false
	for deadline := time.Now().Add(maxPasteDrain); time.Now().Before(deadline); {
		n, err := tty.read(buf)
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

// readTerminal waits for input, a watcher event or the quiet window's end. A
// negative count means retry (a watcher event, or input gone before read).
// Tests wrap it to place signals and hang-ups at these boundaries.
var readTerminal = func(fd, wake int, buf []byte, timeout int) (int, error) {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(wake), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, timeout)
	if err != nil || n == 0 {
		return 0, err
	}
	if fds[1].Revents != 0 {
		var token [1]byte
		_, err := unix.Read(wake, token[:])
		return -1, err
	}
	n, err = unix.Read(fd, buf)
	if errors.Is(err, unix.EAGAIN) || (n == 0 && err == nil && fds[0].Revents&unix.POLLHUP == 0) {
		return -1, nil
	}
	return n, err
}

type promptSignal struct {
	sig syscall.Signal
	err error
}

// watchPromptSignals never changes the terminal. The pipe wakes the reader when
// the channel carries a resume or death signal. Cancellation joins the watcher,
// so no late resume can race the reader's final restore.
func watchPromptSignals(wake *os.File) (<-chan promptSignal, func() promptSignal, []int, error) {
	var watched []os.Signal
	var ignoredControls []int
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTSTP} {
		ignored, err := promptSignalIgnored(sig)
		if err != nil {
			return nil, nil, nil, err
		}
		if !ignored {
			watched = append(watched, sig)
		} else {
			switch sig {
			case syscall.SIGINT:
				ignoredControls = append(ignoredControls, unix.VINTR)
			case syscall.SIGQUIT:
				ignoredControls = append(ignoredControls, unix.VQUIT)
			case syscall.SIGTSTP:
				ignoredControls = append(ignoredControls, unix.VSUSP)
			}
		}
	}
	signals := make(chan os.Signal, 8)
	events := make(chan promptSignal, 8)
	done, joined := make(chan struct{}), make(chan struct{})
	var pending promptSignal // written by the watcher, read only after joined
	// Notify with an empty set would subscribe to every signal.
	if len(watched) != 0 {
		signal.Notify(signals, watched...)
	}
	go func() {
		defer close(joined)
		for {
			select {
			case sig := <-signals:
				event := promptSignal{sig: sig.(syscall.Signal)}
				if promptStopSignal(sig) {
					event.err = stopBy(event.sig)
					// On Darwin stopBy must clear os/signal's handler bookkeeping
					// before Notify can reinstall it. On Linux it is still installed.
					signal.Notify(signals, sig)
				}
				select {
				case events <- event:
					_, _ = wake.Write([]byte{1})
				case <-done:
					pending = event
					return
				}
			case <-done:
				return
			}
		}
	}()
	return events, func() promptSignal {
		close(done)
		<-joined
		signal.Stop(signals)
		// A death arriving at the read's end still takes effect after restoration,
		// including one Notify queued but the watcher did not receive.
		for len(events) != 0 {
			event := <-events
			if event.err != nil {
				pending.err = event.err
			}
			if pending.sig == 0 || !promptStopSignal(event.sig) {
				pending.sig = event.sig
			}
		}
		for len(signals) != 0 {
			sig := (<-signals).(syscall.Signal)
			if pending.sig == 0 || !promptStopSignal(sig) {
				pending.sig = sig
			}
		}
		return pending
	}, ignoredControls, nil
}

func promptStopSignal(sig os.Signal) bool {
	return sig == syscall.SIGTSTP
}

// endByPromptSignal runs on the reader, after the watcher has stopped and the
// terminal has been restored. The fallback exit is only for a failed re-raise.
func endByPromptSignal(sig syscall.Signal) {
	signal.Reset(sig)
	if sig == syscall.SIGQUIT {
		if err := quitWithoutCore(); err != nil {
			fmt.Fprintf(os.Stderr, "agent-secrets: %v\n", err)
			os.Exit(1)
		}
	}
	_ = syscall.Kill(os.Getpid(), sig)
	time.Sleep(time.Second)
	switch sig {
	case syscall.SIGHUP:
		os.Exit(exitHangup)
	case syscall.SIGTERM:
		os.Exit(exitTerminated)
	case syscall.SIGQUIT:
		os.Exit(exitQuit)
	default:
		os.Exit(exitInterrupted)
	}
}
